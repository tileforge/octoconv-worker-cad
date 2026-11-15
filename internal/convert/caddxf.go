package convert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	cadDXFDrawName   = "draw.svg"
	cadDXFRenderName = "draw.render.svg"
	cadDXFTextsName  = "texts.json"
	cadDXFCleanName  = "clean.svg"
	cadDXFHomeName   = ".home"
	cadDXFInkName    = "ink.png"
	cadDXFPageSlack  = 1e-6
)

var cadDXFJobNames = []string{cadDXFDrawName, cadDXFRenderName, cadDXFTextsName, cadMeasureDraftName, cadDXFCleanName, cadDXFHomeName, cadDXFInkName}

type CADDXFConverter struct{ CADConverter }

func (CADDXFConverter) Pairs() []Pair {
	return []Pair{
		{From: "dxf", To: "svg"}, {From: "dxf", To: "pdf"},
		{From: "dwg", To: "svg"}, {From: "dwg", To: "pdf"}, {From: "dwg", To: "png"},
	}
}

type cadDXFDrawing struct {
	client, render CADSVGFacts
	texts          []cadDXFText
	verdict        cadVerdict
}

func (c CADDXFConverter) Convert(ctx context.Context, inPath, outPath string, opts map[string]any) error {
	return c.ConvertWithStages(ctx, inPath, outPath, opts, nil)
}

func (c CADDXFConverter) ConvertWithStages(ctx context.Context, inPath, outPath string, opts map[string]any, sink StageSink) (err error) {
	parsed, err := CADOptsFromMap(opts)
	if err != nil {
		return fmt.Errorf("cad: %w", err)
	}
	format := NormalizeFormat(filepath.Ext(inPath))
	target := NormalizeFormat(filepath.Ext(outPath))
	switch {
	case (format == "dxf" || format == "dwg") && (target == "svg" || target == "pdf"):
	case format == "dwg" && target == "png":
	default:
		return fmt.Errorf("cad: unsupported conversion %s -> %s", format, target)
	}
	if err := ValidateCADApplicability(EngineCAD, OperationConvert, format, target, parsed); err != nil {
		return fmt.Errorf("cad: %w", err)
	}
	st := newCADStager(sink, OperationConvert, format, target)
	jobDir, inPath, err := c.openDrawing(ctx, inPath, outPath, format, st)
	if err != nil {
		return err
	}
	clean := filepath.Join(jobDir, cadDXFCleanName)
	defer func() {
		if err != nil {
			os.Remove(outPath)
			os.Remove(clean)
			if format == "dwg" {
				os.Remove(inPath)
			}
		}
	}()

	if target == "svg" {
		_, err = c.dxfDrawing(ctx, jobDir, inPath, outPath, "", parsed.Layout, st)
		return err
	}
	if target == "png" {
		err = c.writeDrawingPNG(ctx, jobDir, inPath, outPath, parsed.Layout, st)
		return err
	}
	d, err := c.dxfDrawing(ctx, jobDir, inPath, "", "", parsed.Layout, st)
	if err != nil {
		return err
	}
	if err := cadDXFClear(outPath); err != nil {
		return err
	}
	st.enter(cadStageRsvg)
	if err := c.rsvgStage(ctx, jobDir, outPath, cadDXFPDFArgs(outPath, clean)...); err != nil {
		return err
	}
	st.enter(cadStageJudge)
	if err := cadDXFRegular(outPath, "the PDF", ErrCADDrawingFailed); err != nil {
		return err
	}
	if _, err := JudgeCADDXFPDF(outPath, CADMaxDXFPDFBytes, d.texts, d.render); err != nil {
		return fmt.Errorf("cad: %w", err)
	}
	return nil
}

func (c CADDXFConverter) writeDrawingPNG(ctx context.Context, jobDir, inPath, outPath, layout string, st cadStager) error {
	o := CADOpts{}.Resolved()
	size := *o.Size
	d, err := c.dxfDrawing(ctx, jobDir, inPath, "", "", layout, st)
	if err != nil {
		return err
	}
	box, err := CADDrawingLetterbox(d.render.WidthMM, d.render.HeightMM, size)
	if err != nil {
		return fmt.Errorf("cad dxf: %w: %v", ErrCADSVGRejected, err)
	}
	clean := filepath.Join(jobDir, cadDXFCleanName)
	if err := cadDXFClear(outPath); err != nil {
		return err
	}
	st.enter(cadStageRsvg)
	if err := c.rsvgStage(ctx, jobDir, outPath, cadDXFFrameArgs(o.Background, box, size, outPath, clean)...); err != nil {
		return err
	}
	st.enter(cadStageJudge)
	if err := cadDXFRegular(outPath, "the PNG", ErrCADDrawingFailed); err != nil {
		return err
	}
	f, _, err := OpenJobFile(outPath)
	if err != nil {
		return fmt.Errorf("cad: %w: no image at %s: %v", ErrCADRenderRejected, outPath, err)
	}
	_, err = JudgeCADDrawingPNG(f, size, o.Background, box)
	f.Close()
	return err
}

type CADDXFRenderConverter struct{ CADConverter }

func (CADDXFRenderConverter) Operation() string { return OperationRender }

func (CADDXFRenderConverter) Pairs() []Pair {
	return []Pair{{From: "dxf", To: "png"}, {From: "dwg", To: "png"}}
}

func (c CADDXFRenderConverter) Convert(ctx context.Context, inPath, outPath string, opts map[string]any) error {
	return c.ConvertWithStages(ctx, inPath, outPath, opts, nil)
}

func (c CADDXFRenderConverter) ConvertWithStages(ctx context.Context, inPath, outPath string, opts map[string]any, sink StageSink) (err error) {
	parsed, err := CADOptsFromMap(opts)
	if err != nil {
		return fmt.Errorf("cad: %w", err)
	}
	format := NormalizeFormat(filepath.Ext(inPath))
	target := NormalizeFormat(filepath.Ext(outPath))
	if (format != "dxf" && format != "dwg") || target != "png" {
		return fmt.Errorf("cad: unsupported render %s -> %s", format, target)
	}
	if err := ValidateCADApplicability(EngineCAD, OperationRender, format, target, parsed); err != nil {
		return fmt.Errorf("cad: %w", err)
	}
	o := parsed.Resolved()
	size := *o.Size
	st := newCADStager(sink, OperationRender, format, "png")
	jobDir, inPath, err := c.openDrawing(ctx, inPath, outPath, format, st)
	if err != nil {
		return err
	}
	clean := filepath.Join(jobDir, cadDXFCleanName)
	sidecar := CADMeasurementsPath(outPath)
	defer func() {
		if err != nil {
			os.Remove(outPath)
			os.Remove(clean)
			os.Remove(sidecar)
			if format == "dwg" {
				os.Remove(inPath)
			}
		}
	}()

	draftPath := filepath.Join(jobDir, cadMeasureDraftName)
	d, err := c.dxfDrawing(ctx, jobDir, inPath, "", draftPath, parsed.Layout, st)
	if err != nil {
		return err
	}
	if err := cadDXFRegular(draftPath, "the measurements draft", ErrCADMeasurementsInvalid); err != nil {
		return err
	}
	draft, err := parseCADMeasureDraft(draftPath)
	if err != nil {
		return fmt.Errorf("cad: %w: %v", ErrCADMeasurementsInvalid, err)
	}
	if draft.Format != "dxf" {
		return fmt.Errorf("cad: %w: the draft of a dxf drawing reports format %q", ErrCADMeasurementsInvalid, draft.Format)
	}
	if cadZeroExtent(draft.BBox) {
		return fmt.Errorf("cad: %w: the drawing has no extent", ErrCADEmptyResult)
	}
	doc, err := buildCADMeasurements(draft, o.AssumeUnit)
	if err != nil {
		return fmt.Errorf("cad: %w: %v", ErrCADMeasurementsInvalid, err)
	}

	box, err := CADDrawingLetterbox(d.render.WidthMM, d.render.HeightMM, size)
	if err != nil {
		return fmt.Errorf("cad dxf: %w: %v", ErrCADSVGRejected, err)
	}
	args := cadDXFFrameArgs(o.Background, box, size, outPath, clean)
	for _, p := range []string{outPath, sidecar} {
		if err := cadDXFClear(p); err != nil {
			return err
		}
	}
	st.enter(cadStageRsvg)
	if err := c.rsvgStage(ctx, jobDir, outPath, args...); err != nil {
		return err
	}
	st.enter(cadStageJudge)
	if err := cadDXFRegular(outPath, "the PNG", ErrCADDrawingFailed); err != nil {
		return err
	}
	f, _, err := OpenJobFile(outPath)
	if err != nil {
		return fmt.Errorf("cad: %w: no image at %s: %v", ErrCADRenderRejected, outPath, err)
	}
	_, err = JudgeCADDrawingPNG(f, size, o.Background, box)
	f.Close()
	if err != nil {
		return err
	}

	body, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("cad: %w: %v", ErrCADMeasurementsInvalid, err)
	}
	if err := writeNewFile(sidecar, body, 0o600); err != nil {
		return fmt.Errorf("cad: write measurements: %w", err)
	}
	return nil
}

func cadDXFPDFArgs(out, in string) []string {
	return []string{"-f", "pdf", "-b", "white", "-o", out, in}
}

func cadDXFFrameArgs(background string, box CADLetterbox, size int, out, in string) []string {
	var args []string
	if background == CADBackgroundOpq {
		args = append(args, "-b", "white")
	}
	return append(args,
		"-w", strconv.Itoa(box.Width), "-h", strconv.Itoa(box.Height),
		"--page-width", strconv.Itoa(size), "--page-height", strconv.Itoa(size),
		"--left", strconv.Itoa(box.Left), "--top", strconv.Itoa(box.Top),
		"-o", out, in)
}

func cadDXFNames(inPath, outPath string, extra ...string) (string, error) {
	jobDir := filepath.Dir(outPath)
	names := append(append([]string{}, cadDXFJobNames...), extra...)
	for _, name := range names {
		p := filepath.Join(jobDir, name)
		if filepath.Clean(inPath) == p {
			return "", fmt.Errorf("cad: the input %s is the %s path of its own job", inPath, name)
		}
		if filepath.Clean(outPath) == p {
			return "", fmt.Errorf("cad: the output %s is the %s path of its own job", outPath, name)
		}
	}
	return jobDir, nil
}

func cadDXFAdmit(inPath, outPath string) (string, error) {
	jobDir, err := cadDXFNames(inPath, outPath)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(inPath)
	if err != nil {
		return "", fmt.Errorf("cad: stat input: %w", err)
	}
	if ceiling := cadMaxBytes("dxf"); info.Size() > ceiling {
		return "", fmt.Errorf("cad: %w: dxf input is %d bytes against a %d-byte ceiling", ErrCADInputTooLarge, info.Size(), ceiling)
	}
	return jobDir, nil
}

func cadDXFClear(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cad: clear %s: %w", path, err)
	}
	return nil
}

func (c CADConverter) dxfDrawing(ctx context.Context, jobDir, inPath, clientOut, measure, layout string, st cadStager) (cadDXFDrawing, error) {
	var d cadDXFDrawing
	st.enter(cadStageGuard)
	if _, err := c.guardStage(ctx, jobDir, "dxf", inPath); err != nil {
		return d, err
	}
	draw := filepath.Join(jobDir, cadDXFDrawName)
	render := filepath.Join(jobDir, cadDXFRenderName)
	texts := filepath.Join(jobDir, cadDXFTextsName)
	clean := filepath.Join(jobDir, cadDXFCleanName)
	stale := []string{draw, render, texts, clean, filepath.Join(jobDir, cadDXFInkName)}
	if measure != "" {
		stale = append(stale, measure)
	}
	for _, p := range stale {
		if err := cadDXFClear(p); err != nil {
			return d, err
		}
	}
	st.enter(cadStageDXF)
	v, err := c.dxfStage(ctx, jobDir, inPath, draw, render, texts, measure, layout)
	if err != nil {
		return d, err
	}
	d.verdict = v
	for _, out := range []struct{ path, what string }{{draw, "the client SVG"}, {render, "the render-only SVG"}, {texts, "texts.json"}} {
		if err := cadDXFRegular(out.path, out.what, ErrCADSVGRejected); err != nil {
			return d, err
		}
	}
	for _, p := range []string{draw, render} {
		info, err := os.Lstat(p)
		if err != nil {
			return d, fmt.Errorf("cad dxf: %w: %v", ErrCADDrawingFailed, err)
		}
		if info.Size() > CADMaxDXFSVGBytes {
			return d, fmt.Errorf("cad dxf: %w: %s is %d bytes against a %d-byte ceiling", ErrCADTooComplex, filepath.Base(p), info.Size(), int64(CADMaxDXFSVGBytes))
		}
	}
	if d.client, err = cadDXFValidate(draw, clientOut); err != nil {
		return d, err
	}
	if d.render, err = cadDXFValidate(render, clean); err != nil {
		return d, err
	}
	d.texts, err = parseCADDXFTexts(texts, CADMaxDXFSVGBytes)
	if err != nil {
		if errors.Is(err, ErrCADPDFRejected) {
			return d, fmt.Errorf("cad dxf: %w: %w", ErrCADSVGRejected, err)
		}
		return d, fmt.Errorf("cad dxf: %w", err)
	}
	if err := cadDXFCrossCheck(d); err != nil {
		return d, err
	}
	if d.client.Visible == 0 {
		return d, fmt.Errorf("cad dxf: %w: %w: the drawing has nothing visible", ErrCADNothingDrawable, ErrCADEmptyResult)
	}
	st.enter(cadStageInk)
	if err := c.dxfInk(ctx, jobDir, clean, d.render); err != nil {
		return d, err
	}
	return d, nil
}

func (c CADConverter) dxfInk(ctx context.Context, jobDir, clean string, render CADSVGFacts) error {
	box, err := CADDrawingLetterbox(render.WidthMM, render.HeightMM, CADDXFInkSize)
	if err != nil {
		return fmt.Errorf("cad dxf: %w: %v", ErrCADSVGRejected, err)
	}
	ink := filepath.Join(jobDir, cadDXFInkName)
	if err := cadDXFClear(ink); err != nil {
		return err
	}
	defer os.Remove(ink)
	if err := c.rsvgStage(ctx, jobDir, ink, cadDXFFrameArgs(CADBackgroundOpq, box, CADDXFInkSize, ink, clean)...); err != nil {
		return err
	}
	f, _, err := OpenJobFile(ink)
	if err != nil {
		return fmt.Errorf("cad dxf: %w: open the ink check frame: %v", ErrCADDrawingFailed, err)
	}
	defer f.Close()
	if _, err := JudgeCADDrawingInk(f, CADDXFInkSize); err != nil {
		return fmt.Errorf("cad dxf: %w", err)
	}
	return nil
}

func (c CADConverter) dxfStage(ctx context.Context, jobDir, inPath, svg, render, texts, measure, layout string) (cadVerdict, error) {
	x, err := c.cadDrawStage(jobDir, cadDXFReadOnly, cadDXFEngines)
	if err != nil {
		return cadVerdict{}, err
	}
	timeout := c.dxfTimeout()
	stageCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := []string{
		"--max-drawn-entities", strconv.Itoa(CADMaxDXFExpandedEntities),
		"--texts", texts,
		"--render-svg", render,
	}
	if measure != "" {
		args = append(args, "--measure", measure)
	}
	if layout != "" {
		args = append(args, "--layout="+layout)
	}
	args = append(args, inPath, svg)
	out, cpu, err := x.runBounded(stageCtx, cadOutputLimit, "cad-dxf", args...)
	return classifyCADStage(cadStageDXF, ctx, stageCtx, timeout, out, cpu, err)
}

func (c CADConverter) rsvgStage(ctx context.Context, jobDir, outPath string, args ...string) error {
	x, err := c.cadDrawStage(jobDir, cadRsvgReadOnly, cadRsvgEngines)
	if err != nil {
		return err
	}
	timeout := c.rsvgTimeout()
	stageCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, cpu, err := x.runBounded(stageCtx, cadOutputLimit, "rsvg-convert", args...)
	return classifyCADRsvg(ctx, stageCtx, timeout, cpu, err, outPath)
}

func cadDXFRegular(path, what string, irregular error) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cad: %w: the stage reported ok without %s", ErrCADDrawingFailed, what)
	}
	if err != nil {
		return fmt.Errorf("cad: %w: stat %s: %v", ErrCADDrawingFailed, what, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("cad: %w: %s is %v, not a regular file", irregular, what, info.Mode().Type())
	}
	return nil
}

func cadDXFValidate(src, dst string) (facts CADSVGFacts, err error) {
	in, _, err := OpenJobFile(src)
	if err != nil {
		return facts, fmt.Errorf("cad dxf: %w: open %s: %v", ErrCADSVGRejected, filepath.Base(src), err)
	}
	defer in.Close()
	var w io.Writer = io.Discard
	if dst != "" {
		if err := cadDXFClear(dst); err != nil {
			return facts, err
		}
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return facts, fmt.Errorf("cad dxf: create %s: %w", dst, err)
		}
		defer func() {
			if cerr := out.Close(); cerr != nil && err == nil {
				err = fmt.Errorf("cad dxf: write %s: %w", dst, cerr)
			}
			if err != nil {
				os.Remove(dst)
			}
		}()
		w = out
	}
	facts, err = ValidateCADSVG(in, w, CADMaxDXFSVGBytes)
	if err != nil {
		return facts, fmt.Errorf("cad dxf: %s: %w", filepath.Base(src), err)
	}
	return facts, nil
}

func cadXMLLineEnds(s string) string {
	if !strings.Contains(s, "\r") {
		return s
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

func cadDXFRenderText(t cadDXFText) (string, int) {
	covered := make(map[rune]bool, len(t.Covered))
	for _, r := range t.Covered {
		covered[r] = true
	}
	var b strings.Builder
	substituted := 0
	for _, r := range t.Text {
		if covered[r] {
			b.WriteRune(r)
			continue
		}
		b.WriteRune('�')
		if r != '�' {
			substituted++
		}
	}
	return b.String(), substituted
}

func cadDXFCrossCheck(d cadDXFDrawing) error {
	n := len(d.texts)
	if len(d.client.Texts) != n || len(d.render.Texts) != n || d.verdict.Texts != n {
		return fmt.Errorf("cad dxf: %w", cadSVGReject("texts.json holds %d texts, the client SVG %d, the render-only SVG %d and the verdict %d",
			n, len(d.client.Texts), len(d.render.Texts), d.verdict.Texts))
	}
	if len(d.client.TextOpacity) != n || len(d.render.TextOpacity) != n {
		return fmt.Errorf("cad dxf: %w", cadSVGReject("the client SVG carries %d text opacities and the render-only SVG %d for %d texts",
			len(d.client.TextOpacity), len(d.render.TextOpacity), n))
	}
	if len(d.client.TextFonts) != n || len(d.render.TextFonts) != n {
		return fmt.Errorf("cad dxf: %w", cadSVGReject("the client SVG carries %d text fonts and the render-only SVG %d for %d texts",
			len(d.client.TextFonts), len(d.render.TextFonts), n))
	}
	uncovered, substituted := 0, 0
	for i, t := range d.texts {
		if cadXMLLineEnds(t.Text) != cadXMLLineEnds(d.client.Texts[i]) {
			return fmt.Errorf("cad dxf: %w", cadSVGReject("text %d of texts.json differs from the client SVG", i))
		}
		if d.client.TextOpacity[i] != d.render.TextOpacity[i] {
			return fmt.Errorf("cad dxf: %w", cadSVGReject("text %d has fill opacity %v in the client SVG and %v in the render-only SVG",
				i, d.client.TextOpacity[i], d.render.TextOpacity[i]))
		}
		if d.client.TextFonts[i] != d.render.TextFonts[i] {
			return fmt.Errorf("cad dxf: %w", cadSVGReject("text %d has the font %s in the client SVG and %s in the render-only SVG",
				i, cadSVGQuote(d.client.TextFonts[i]), cadSVGQuote(d.render.TextFonts[i])))
		}
		if t.Invisible != (d.client.TextOpacity[i] == 0) {
			return fmt.Errorf("cad dxf: %w", cadSVGReject("text %d of texts.json has invisible=%t, its fill opacity in the SVG is %v",
				i, t.Invisible, d.client.TextOpacity[i]))
		}
		want, subs := cadDXFRenderText(t)
		if cadXMLLineEnds(want) != cadXMLLineEnds(d.render.Texts[i]) {
			return fmt.Errorf("cad dxf: %w", cadSVGReject("text %d of the render-only SVG is not texts.json with U+FFFD outside covered", i))
		}
		uncovered += utf8.RuneCountInString(t.Text) - utf8.RuneCountInString(t.Covered)
		substituted += subs
	}
	if d.verdict.Uncovered != uncovered || d.verdict.RenderSubstituted != substituted {
		return fmt.Errorf("cad dxf: %w", cadSVGReject("the verdict reports %d uncovered and %d substituted characters, texts.json implies %d and %d",
			d.verdict.Uncovered, d.verdict.RenderSubstituted, uncovered, substituted))
	}
	if d.client.Visible != d.render.Visible {
		return fmt.Errorf("cad dxf: %w", cadSVGReject("the client SVG has %d visible elements and the render-only SVG %d",
			d.client.Visible, d.render.Visible))
	}
	if d.client.WidthMM != d.render.WidthMM || d.client.HeightMM != d.render.HeightMM ||
		d.client.ViewBoxW != d.render.ViewBoxW || d.client.ViewBoxH != d.render.ViewBoxH {
		return fmt.Errorf("cad dxf: %w", cadSVGReject("the client SVG page %vx%v mm, viewBox %vx%v differs from the render-only SVG page %vx%v mm, viewBox %vx%v",
			d.client.WidthMM, d.client.HeightMM, d.client.ViewBoxW, d.client.ViewBoxH, d.render.WidthMM, d.render.HeightMM, d.render.ViewBoxW, d.render.ViewBoxH))
	}
	if math.Abs(d.verdict.PageWMM-d.client.WidthMM) > cadDXFPageSlack || math.Abs(d.verdict.PageHMM-d.client.HeightMM) > cadDXFPageSlack {
		return fmt.Errorf("cad dxf: %w", cadSVGReject("the verdict page %vx%v mm differs from the SVG page %vx%v mm",
			d.verdict.PageWMM, d.verdict.PageHMM, d.client.WidthMM, d.client.HeightMM))
	}
	return nil
}
