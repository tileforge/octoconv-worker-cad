package convert

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/tileforge/octoconv-worker-cad/internal/measurements"
)

//go:embed testdata/cad/synthetic/units_inch.step
var cadPreflightSTEP []byte

//go:embed testdata/cad/synthetic/legacy_cp1251.dxf
var cadPreflightDXF []byte

const cadPreflightDXFText = "Деталь"

const (
	cadPreflightTriangles   = 12
	cadPreflightTolerance   = 1e-3
	cadPreflightFrame       = 256
	cadPreflightInchExtent  = 1e-4
	cadPreflightInchMeasure = 1e-6
)

var cadPreflightExtents = [3]float64{25.4, 50.8, 76.2}

var cadPreflightInchExtents = [3]float64{1, 2, 3}

var cadMeshReadOnly = []string{
	"/opt/venv",
	"/usr/bin/python3.11",
	"/usr/lib/python3.11",
}

var cadGuardReadOnly = []string{
	"/usr/bin/python3.11",
	"/usr/lib/python3.11",
}

var cadRenderReadOnly = []string{
	"/opt/venv",
	"/usr/bin/python3.11",
	"/usr/lib/python3.11",
}

var cadMeshEngines = []string{"cad-mesh"}

var cadRenderEngines = []string{"cad-render"}

var cadGuardEngines = []string{"cad-guard"}

var cadDXFReadOnly = []string{
	"/opt/venv",
	"/usr/bin/python3.11",
	"/usr/lib/python3.11",
	"/usr/share/fonts",
}

var cadDXFEngines = []string{"cad-dxf"}

var cadRsvgReadOnly = []string{
	"/usr/share/fonts",
}

var cadRsvgEngines = []string{"rsvg-convert"}

var cadMultiarchLib = map[string]string{"arm64": "aarch64-linux-gnu", "amd64": "x86_64-linux-gnu"}

func (c CADConverter) cadReadOnly(readOnly []string) ([]string, error) {
	ro := append([]string{}, readOnly...)
	if c.Sandbox.confines() {
		triple, ok := cadMultiarchLib[runtime.GOARCH]
		if !ok {
			return nil, fmt.Errorf("cad: no multiarch library directory is known for GOARCH %s", runtime.GOARCH)
		}
		ro = append(ro, "/usr/lib/"+triple)
	}
	return ro, nil
}

func (c CADConverter) cadStage(jobDir string, readOnly, engines []string) (engineExec, error) {
	ro, err := c.cadReadOnly(readOnly)
	if err != nil {
		return engineExec{}, err
	}
	env := append(engineEnv(jobDir), "LP_NUM_THREADS=2")
	return c.Sandbox.forJob(jobDir, ro, engines).withEnv(env), nil
}

func (c CADConverter) cadDrawStage(jobDir string, readOnly, engines []string) (engineExec, error) {
	ro, err := c.cadReadOnly(readOnly)
	if err != nil {
		return engineExec{}, err
	}
	home := filepath.Join(jobDir, ".home")
	cache := filepath.Join(home, ".cache")
	config := filepath.Join(home, ".config")
	for _, dir := range []string{home, cache, config} {
		if err := cadPrivateDir(dir); err != nil {
			return engineExec{}, err
		}
	}
	var env []string
	for _, e := range engineEnv(jobDir) {
		if !strings.HasPrefix(e, "HOME=") {
			env = append(env, e)
		}
	}
	env = append(env, "HOME="+home, "XDG_CACHE_HOME="+cache, "XDG_CONFIG_HOME="+config)
	return c.Sandbox.forJob(jobDir, ro, engines).withEnv(env), nil
}

func cadPrivateDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("cad: create the stage home %s: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("cad: stat the stage home %s: %w", dir, err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("cad: the stage home %s is %v, want a 0700 directory", dir, info.Mode())
	}
	return nil
}

func (c CADConverter) WithSandbox(helper []string) CADConverter {
	c.Sandbox = newEngineSandbox(helper)
	return c
}

func ValidateCADTimeouts(guard, mesh, render, dxf, rsvg, engine time.Duration) error {
	for _, v := range []struct {
		name  string
		value time.Duration
	}{
		{"CAD_GUARD_TIMEOUT", guard}, {"CAD_MESH_TIMEOUT", mesh}, {"CAD_RENDER_TIMEOUT", render},
		{"CAD_DXF_TIMEOUT", dxf}, {"CAD_RSVG_TIMEOUT", rsvg}, {"CAD_ENGINE_TIMEOUT", engine},
	} {
		if v.value <= 0 {
			return fmt.Errorf("%s=%s must be positive", v.name, v.value)
		}
	}
	if guard+mesh+render+CADIOAllowance > engine {
		return fmt.Errorf("CAD_GUARD_TIMEOUT=%s + CAD_MESH_TIMEOUT=%s + CAD_RENDER_TIMEOUT=%s + %s of I/O allowance exceeds CAD_ENGINE_TIMEOUT=%s: "+
			"the attempt deadline would cut a stage short of its own limit", guard, mesh, render, CADIOAllowance, engine)
	}
	if guard+dxf+2*rsvg+CADIOAllowance > engine {
		return fmt.Errorf("CAD_GUARD_TIMEOUT=%s + CAD_DXF_TIMEOUT=%s + two rsvg-convert stages of CAD_RSVG_TIMEOUT=%s + %s of I/O allowance exceeds CAD_ENGINE_TIMEOUT=%s: "+
			"the attempt deadline would cut a stage short of its own limit", guard, dxf, rsvg, CADIOAllowance, engine)
	}
	return nil
}

func ConfinedCAD(helper []string, concurrency int, concurrencyEnv string, guard, mesh, render, dxf, rsvg, engine time.Duration) (CADConverter, error) {
	if concurrency != 1 {
		return CADConverter{}, fmt.Errorf("%s=%d: the cad pod limit was derived for one job at a time; set %s=1 and scale with replicas",
			concurrencyEnv, concurrency, concurrencyEnv)
	}
	if err := ValidateCADTimeouts(guard, mesh, render, dxf, rsvg, engine); err != nil {
		return CADConverter{}, err
	}
	c := CADConverter{GuardTimeout: guard, MeshTimeout: mesh, RenderTimeout: render, DXFTimeout: dxf, RsvgTimeout: rsvg}.WithSandbox(helper)
	return c, nil
}

func cadStepEntities(data []byte) (int, error) {
	idx := bytes.Index(data, []byte("DATA;"))
	if idx < 0 {
		return 0, errors.New("no DATA; section")
	}
	return max(bytes.Count(data[idx:], []byte(";"))-3, 0), nil
}

func requireCADDenied(stage string, err error) error {
	if errors.Is(err, ErrEngineSandboxUnavailable) {
		return fmt.Errorf("the %s stage could not establish its sandbox, so its denial of the outside file proves nothing: %w", stage, err)
	}
	return nil
}

func CADPreflight(ctx context.Context, c CADConverter) error {
	if !c.Sandbox.confines() {
		return nil
	}
	triple, ok := cadMultiarchLib[runtime.GOARCH]
	if !ok {
		return fmt.Errorf("cad preflight: no multiarch library directory is known for GOARCH %s", runtime.GOARCH)
	}
	wantEntities, err := cadStepEntities(cadPreflightSTEP)
	if err != nil {
		return fmt.Errorf("cad preflight: embedded fixture: %w", err)
	}

	w, cleanup, err := newPreflightWorkspace("cad")
	if err != nil {
		return err
	}
	defer cleanup()
	if err := w.requireOutsideGrants(cadGuardReadOnly, cadMeshReadOnly, cadRenderReadOnly, cadDXFReadOnly, cadRsvgReadOnly, []string{"/usr/lib/" + triple}); err != nil {
		return err
	}
	inside, outside, err := w.place("in.step", cadPreflightSTEP)
	if err != nil {
		return err
	}

	v, err := c.guardStage(ctx, w.jobDir, "step", inside)
	if err != nil {
		return fmt.Errorf("the cad guard stage cannot count a file inside the job directory under its sandbox: %w", err)
	}
	if v.Entities == nil || *v.Entities != wantEntities {
		return fmt.Errorf("the cad guard stage counted %v entities in the fixture, want %d", v.Entities, wantEntities)
	}

	outInside := filepath.Join(w.jobDir, "out.stl")
	draftPath := filepath.Join(w.jobDir, cadMeasureDraftName)
	meshVerdict, err := c.meshStage(ctx, w.jobDir, "step", inside, outInside, "standard", draftPath)
	if err != nil {
		return fmt.Errorf("the cad mesh stage cannot mesh a file inside the job directory under its sandbox: %w", err)
	}
	n, bounds, err := judgeBinarySTL(outInside)
	if err != nil {
		return fmt.Errorf("the cad mesh stage output inside the job directory is not a usable STL: %w", err)
	}
	if err := requireSTLMatchesVerdict(n, meshVerdict); err != nil {
		return fmt.Errorf("the cad mesh stage output inside the job directory disagrees with its own verdict: %w", err)
	}
	if n != cadPreflightTriangles {
		return fmt.Errorf("the cad mesh stage produced %d triangles for the fixture, want %d", n, cadPreflightTriangles)
	}
	got := bounds.Extents()
	sort.Float64s(got[:])
	for i, want := range cadPreflightExtents {
		if math.Abs(got[i]-want) > cadPreflightTolerance {
			return fmt.Errorf("units: the cad mesh stage produced sorted extents %.4f %.4f %.4f for an inch fixture, want %.1f %.1f %.1f millimetres",
				got[0], got[1], got[2], cadPreflightExtents[0], cadPreflightExtents[1], cadPreflightExtents[2])
		}
	}

	if err := requireCADInchMeasurements(draftPath); err != nil {
		return err
	}
	if err := cadPreflightRender(ctx, CADRenderConverter{c}, w, outInside); err != nil {
		return err
	}
	if err := cadPreflightDrawing(ctx, c, w); err != nil {
		return err
	}

	_, guardErr := c.guardStage(ctx, w.jobDir, "step", outside)
	if guardErr == nil {
		return errors.New("the cad guard stage returned ok for a file outside the job directory: confinement is not in effect")
	}
	if err := requireCADDenied("cad guard", guardErr); err != nil {
		return err
	}

	outOutside := filepath.Join(w.jobDir, "neg.stl")
	_, meshErr := c.meshStage(ctx, w.jobDir, "step", outside, outOutside, "standard", "")
	if err := requireCADDenied("cad mesh", meshErr); err != nil {
		return err
	}
	if err := requireDenied(meshErr, outOutside, nil); err != nil {
		return fmt.Errorf("the cad mesh stage read a file outside the job directory: %w", err)
	}
	return cadPreflightRenderDenied(ctx, CADRenderConverter{c}, w, outInside)
}

func requireCADInchMeasurements(draftPath string) error {
	draft, err := parseCADMeasureDraft(draftPath)
	if err != nil {
		return fmt.Errorf("units: the cad mesh stage left no usable measurements draft for the inch fixture: %w", err)
	}
	doc, err := buildCADMeasurements(draft, "")
	if err != nil {
		return fmt.Errorf("units: the measurements of the inch fixture cannot be built: %w", err)
	}
	if doc.Unit != measurements.UnitInch || doc.UnitSource != measurements.SourceDeclared {
		return fmt.Errorf("units: the inch fixture measured as unit %q source %q, want inch declared", doc.Unit, doc.UnitSource)
	}
	if len(doc.BBox.Min) != 3 || len(doc.BBox.Max) != 3 {
		return fmt.Errorf("units: the inch fixture bounding box has %d and %d coordinates, want 3", len(doc.BBox.Min), len(doc.BBox.Max))
	}
	ext := make([]float64, 3)
	for i := range ext {
		ext[i] = doc.BBox.Max[i] - doc.BBox.Min[i]
	}
	sort.Float64s(ext)
	for i, want := range cadPreflightInchExtents {
		if math.Abs(ext[i]-want) > cadPreflightInchExtent {
			return fmt.Errorf("units: the inch fixture measured sorted extents %.6f %.6f %.6f, want 1 2 3", ext[0], ext[1], ext[2])
		}
	}
	if doc.Volume == nil || math.Abs(*doc.Volume-6) > cadPreflightInchMeasure {
		return fmt.Errorf("units: the inch fixture volume is %v, want 6", doc.Volume)
	}
	if doc.Area == nil || math.Abs(*doc.Area-22) > cadPreflightInchMeasure {
		return fmt.Errorf("units: the inch fixture area is %v, want 22", doc.Area)
	}
	return nil
}

func cadPreflightRender(ctx context.Context, r CADRenderConverter, w preflightWorkspace, stl string) error {
	size := cadPreflightFrame
	for _, background := range []string{CADBackgroundOpq, CADBackgroundTr} {
		path := filepath.Join(w.jobDir, "preflight-"+background+".png")
		o := CADOpts{View: CADViewIso, Size: &size, Background: background}
		if err := r.renderStage(ctx, w.jobDir, stl, path, o); err != nil {
			return fmt.Errorf("the cad render stage cannot render a file inside the job directory (%s) under its sandbox: %w", background, err)
		}
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("the cad render stage left no %s image inside the job directory: %w", background, err)
		}
		_, err = JudgeCADPNG(f, size, background)
		f.Close()
		if err != nil {
			return fmt.Errorf("the pixel judge rejects the %s frame the cad render stage drew inside the job directory: %w", background, err)
		}
	}
	var blank bytes.Buffer
	if err := png.Encode(&blank, image.NewNRGBA(image.Rect(0, 0, size, size))); err != nil {
		return fmt.Errorf("cad preflight: build the blank frame: %w", err)
	}
	if _, err := JudgeCADPNG(bytes.NewReader(blank.Bytes()), size, CADBackgroundTr); !errors.Is(err, ErrCADRenderRejected) {
		return fmt.Errorf("the pixel judge did not reject a blank frame (%v): a judge that cannot fail proves nothing", err)
	}
	return nil
}

func cadPreflightRenderDenied(ctx context.Context, r CADRenderConverter, w preflightWorkspace, stl string) error {
	data, err := os.ReadFile(stl)
	if err != nil {
		return fmt.Errorf("cad preflight: read the fixture mesh: %w", err)
	}
	outsideSTL := filepath.Join(w.outsideDir, "out.stl")
	if err := os.WriteFile(outsideSTL, data, 0o600); err != nil {
		return fmt.Errorf("cad preflight fixture (outside): %w", err)
	}
	size := cadPreflightFrame
	outPNG := filepath.Join(w.jobDir, "neg.png")
	err = r.renderStage(ctx, w.jobDir, outsideSTL, outPNG, CADOpts{View: CADViewIso, Size: &size, Background: CADBackgroundOpq})
	if err := requireCADDenied("cad render", err); err != nil {
		return err
	}
	if err := requireDenied(err, outPNG, nil); err != nil {
		return fmt.Errorf("the cad render stage read a file outside the job directory: confinement is not in effect: %w", err)
	}
	return nil
}

func cadPreflightDrawing(ctx context.Context, c CADConverter, w preflightWorkspace) error {
	inside, outside, err := w.place("in.dxf", cadPreflightDXF)
	if err != nil {
		return err
	}
	if _, err := c.guardStage(ctx, w.jobDir, "dxf", inside); err != nil {
		return fmt.Errorf("the cad guard stage in dxf mode refuses the fixture inside the job directory under its sandbox: %w", err)
	}
	clean := filepath.Join(w.jobDir, cadDXFCleanName)
	d, err := c.dxfDrawing(ctx, w.jobDir, inside, "", "", cadStager{})
	if errors.Is(err, ErrCADNothingDrawable) {
		return fmt.Errorf("the ink check finds nothing on the fixture's page drawn under the rsvg-convert ruleset: %w", err)
	}
	if err != nil {
		return fmt.Errorf("the cad dxf stage cannot draw a file inside the job directory under its sandbox: %w", err)
	}
	if len(d.texts) != 1 || d.texts[0].Text != cadPreflightDXFText || d.texts[0].Covered != cadPreflightDXFText || d.texts[0].Invisible {
		var got []string
		for _, t := range d.texts {
			got = append(got, t.Text)
		}
		return fmt.Errorf("text: the cad dxf stage drew the fixture's texts as %q, want exactly %q with every character covered by the font", got, cadPreflightDXFText)
	}

	pdf := filepath.Join(w.jobDir, "preflight-dxf.pdf")
	if err := c.rsvgStage(ctx, w.jobDir, pdf, cadDXFPDFArgs(pdf, clean)...); err != nil {
		return fmt.Errorf("the rsvg-convert stage cannot convert an SVG inside the job directory to PDF under its sandbox: %w", err)
	}
	facts, err := JudgeCADDXFPDF(pdf, CADMaxDXFPDFBytes, d.texts, d.render)
	if err != nil {
		return fmt.Errorf("fonts: the PDF judge rejects the fixture's PDF drawn under the rsvg-convert ruleset, so its text did not survive: %w", err)
	}
	if facts.BelowRenderFloor != 0 {
		return fmt.Errorf("text: the fixture's text falls below the PDF render floor, so the PDF judge would accept it without any text")
	}
	if facts.InvisibleText != 0 {
		return fmt.Errorf("text: the fixture's text is invisible, so the PDF judge would accept it without any text")
	}
	if _, err := JudgeCADPDF(pdf, CADMaxDXFPDFBytes, cadDXFExpectedText(d.texts)); err != nil {
		return fmt.Errorf("fonts: the PDF drawn under the rsvg-convert ruleset does not carry the text %q: %w", cadPreflightDXFText, err)
	}

	size := cadPreflightFrame
	box, err := CADDrawingLetterbox(d.render.WidthMM, d.render.HeightMM, size)
	if err != nil {
		return fmt.Errorf("cad preflight: letterbox of the fixture drawing: %w", err)
	}
	frame := filepath.Join(w.jobDir, "preflight-dxf.png")
	if err := c.rsvgStage(ctx, w.jobDir, frame, cadDXFFrameArgs(CADBackgroundOpq, box, size, frame, clean)...); err != nil {
		return fmt.Errorf("the rsvg-convert stage cannot draw an SVG inside the job directory to PNG under its sandbox: %w", err)
	}
	f, err := os.Open(frame)
	if err != nil {
		return fmt.Errorf("the rsvg-convert stage left no frame inside the job directory: %w", err)
	}
	_, err = JudgeCADDrawingPNG(f, size, CADBackgroundOpq, box)
	f.Close()
	if err != nil {
		return fmt.Errorf("the drawing judge rejects the frame the rsvg-convert stage drew inside the job directory: %w", err)
	}

	negSVG := filepath.Join(w.jobDir, "neg.svg")
	negRender := filepath.Join(w.jobDir, "neg.render.svg")
	negTexts := filepath.Join(w.jobDir, "neg.texts.json")
	_, dxfErr := c.dxfStage(ctx, w.jobDir, outside, negSVG, negRender, negTexts, "")
	if err := requireCADDenied("cad dxf", dxfErr); err != nil {
		return err
	}
	for _, p := range []string{negSVG, negRender, negTexts} {
		if err := requireDenied(dxfErr, p, nil); err != nil {
			return fmt.Errorf("the cad dxf stage read a file outside the job directory: confinement is not in effect: %w", err)
		}
	}

	data, err := os.ReadFile(clean)
	if err != nil {
		return fmt.Errorf("cad preflight: read the validated fixture SVG: %w", err)
	}
	outsideSVG := filepath.Join(w.outsideDir, cadDXFCleanName)
	if err := os.WriteFile(outsideSVG, data, 0o600); err != nil {
		return fmt.Errorf("cad preflight fixture (outside): %w", err)
	}
	negPDF := filepath.Join(w.jobDir, "neg.pdf")
	rsvgErr := c.rsvgStage(ctx, w.jobDir, negPDF, cadDXFPDFArgs(negPDF, outsideSVG)...)
	if err := requireCADDenied("rsvg-convert", rsvgErr); err != nil {
		return err
	}
	if err := requireDenied(rsvgErr, negPDF, nil); err != nil {
		return fmt.Errorf("the rsvg-convert stage read a file outside the job directory: confinement is not in effect: %w", err)
	}
	return nil
}
