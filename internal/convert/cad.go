package convert

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/tileforge/octoconv-worker-cad/internal/measurements"
)

const (
	CADMaxSTEPBytes         = 63 << 20
	CADMaxIGESBytes         = 120 << 20
	CADMaxSTEPEntities      = 754762
	CADMaxNAUO              = 1518
	CADMaxRenderTriangles   = 3835382
	CADMaxSTLBytes          = 190840832
	CADDefaultGuardTimeout  = 21 * time.Second
	CADDefaultMeshTimeout   = 324 * time.Second
	CADDefaultRenderTimeout = 521 * time.Second
	CADIOAllowance          = 20 * time.Second
	CADDefaultEngineTimeout = max(
		CADDefaultGuardTimeout+CADDefaultMeshTimeout+CADDefaultRenderTimeout+CADIOAllowance,
		CADDefaultGuardTimeout+CADDefaultDXFTimeout+2*CADDefaultRsvgTimeout+CADIOAllowance,
		CADDefaultGuardTimeout+CADDefaultDWGTimeout+CADDefaultDXFTimeout+2*CADDefaultRsvgTimeout+CADIOAllowance)

	CADMaxDXFBytes            = 118 << 20
	CADMaxDXFExpandedInserts  = 170031
	CADMaxDXFExpandedEntities = 850155
	CADMaxDXFNesting          = 16
	CADMaxDXFSVGBytes         = 134 << 20
	CADMaxDXFPDFBytes         = 19 << 20
	CADDefaultDXFTimeout      = 186 * time.Second
	CADDefaultDWGTimeout      = 186 * time.Second
	CADDefaultRsvgTimeout     = 217 * time.Second

	cadOutputLimit   = 1 << 20
	cadVerdictPrefix = "CADVERDICT "
	cadStageGuard    = "guard"
	cadStageMesh     = "mesh"
	cadStageRender   = "render"
	cadStageDXF      = "dxf"
	cadStageDWG      = "dwg"
	cadStageRsvg     = "rsvg"
	cadStageInk      = "ink"
	cadStageJudge    = "judge"
	stlHeaderBytes   = 84
	stlTriangleBytes = 50
	stlBlockTriangle = 4096
)

var (
	ErrCADInputTooLarge  = errors.New("cad: the input exceeds this deployment's size ceiling")
	ErrCADTooComplex     = errors.New("cad: the model is too complex to convert")
	ErrCADUnreadable     = errors.New("cad: the file could not be read as CAD data")
	ErrCADEmptyResult    = errors.New("cad: the conversion produced no geometry")
	ErrCADMeshTimeout    = errors.New("cad: the mesh stage did not finish within the time limit")
	ErrCADEngineKilled   = errors.New("cad: the engine was killed before it finished")
	ErrCADRenderRejected = errors.New("cad: the rendered image failed the content check")

	ErrCADRenderTimeout       = errors.New("cad: the render stage did not finish within the time limit")
	ErrCADRenderFailed        = errors.New("cad: the render stage failed")
	ErrCADMeasurementsInvalid = errors.New("cad: the measurements could not be produced")

	ErrCADDXFTimeout      = errors.New("cad: the dxf stage did not finish within the time limit")
	ErrCADDWGTimeout      = errors.New("cad: the dwg took too long to read; simplify the drawing or export fewer details")
	ErrCADRsvgTimeout     = errors.New("cad: the rsvg stage did not finish within the time limit")
	ErrCADDrawingFailed   = errors.New("cad: the drawing stage failed")
	ErrCADUnknownLayout   = errors.New("unknown layout")
	ErrCADNothingDrawable = errors.New("cad: the drawing has nothing drawable")
)

type CADConverter struct {
	Sandbox       *engineSandbox
	GuardTimeout  time.Duration
	MeshTimeout   time.Duration
	RenderTimeout time.Duration
	DXFTimeout    time.Duration
	DWGTimeout    time.Duration
	RsvgTimeout   time.Duration
}

func (CADConverter) Pairs() []Pair {
	return []Pair{{From: "step", To: "stl"}, {From: "iges", To: "stl"}}
}

func (CADConverter) Engine() string { return EngineCAD }

func cadMaxBytes(format string) int64 {
	switch format {
	case "iges":
		return CADMaxIGESBytes
	case "stl":
		return CADMaxSTLBytes
	case "dxf":
		return CADMaxDXFBytes
	}
	return CADMaxSTEPBytes
}

func (c CADConverter) guardTimeout() time.Duration {
	if c.GuardTimeout > 0 {
		return c.GuardTimeout
	}
	return CADDefaultGuardTimeout
}

func (c CADConverter) meshTimeout() time.Duration {
	if c.MeshTimeout > 0 {
		return c.MeshTimeout
	}
	return CADDefaultMeshTimeout
}

func (c CADConverter) renderTimeout() time.Duration {
	if c.RenderTimeout > 0 {
		return c.RenderTimeout
	}
	return CADDefaultRenderTimeout
}

func (c CADConverter) dxfTimeout() time.Duration {
	if c.DXFTimeout > 0 {
		return c.DXFTimeout
	}
	return CADDefaultDXFTimeout
}

func (c CADConverter) dwgTimeout() time.Duration {
	if c.DWGTimeout > 0 {
		return c.DWGTimeout
	}
	return CADDefaultDWGTimeout
}

func (c CADConverter) rsvgTimeout() time.Duration {
	if c.RsvgTimeout > 0 {
		return c.RsvgTimeout
	}
	return CADDefaultRsvgTimeout
}

func (c CADConverter) Convert(ctx context.Context, inPath, outPath string, opts map[string]any) error {
	return c.ConvertWithStages(ctx, inPath, outPath, opts, nil)
}

func (c CADConverter) ConvertWithStages(ctx context.Context, inPath, outPath string, opts map[string]any, sink StageSink) error {
	o, err := CADOptsFromMap(opts)
	if err != nil {
		return fmt.Errorf("cad: %w", err)
	}
	if err := ValidateCADApplicability(EngineCAD, OperationConvert, "", "", o); err != nil {
		return fmt.Errorf("cad: %w", err)
	}
	format := NormalizeFormat(filepath.Ext(inPath))
	if target := NormalizeFormat(filepath.Ext(outPath)); (format != "step" && format != "iges") || target != "stl" {
		return fmt.Errorf("cad: unsupported conversion %s -> %s", format, target)
	}
	info, err := os.Stat(inPath)
	if err != nil {
		return fmt.Errorf("cad: stat input: %w", err)
	}
	if ceiling := cadMaxBytes(format); info.Size() > ceiling {
		return fmt.Errorf("cad: %w: %s input is %d bytes against a %d-byte ceiling", ErrCADInputTooLarge, format, info.Size(), ceiling)
	}

	jobDir := filepath.Dir(outPath)
	st := newCADStager(sink, OperationConvert, format, "stl")
	st.enter(cadStageGuard)
	if _, err := c.guardStage(ctx, jobDir, format, inPath); err != nil {
		return err
	}
	st.enter(cadStageMesh)
	verdict, err := c.meshStage(ctx, jobDir, format, inPath, outPath, o.Resolved().Quality, "")
	if err != nil {
		return err
	}
	st.enter(cadStageJudge)
	n, _, err := judgeBinarySTL(outPath)
	if err != nil {
		return fmt.Errorf("cad: %w", err)
	}
	return requireSTLMatchesVerdict(n, verdict)
}

func requireSTLMatchesVerdict(n int, v cadVerdict) error {
	if n != v.Triangles {
		return fmt.Errorf("cad: %w: the STL holds %d triangles, the mesh stage reported %d", ErrCADEmptyResult, n, v.Triangles)
	}
	return nil
}

func (c CADConverter) guardStage(ctx context.Context, jobDir, format, inPath string) (cadVerdict, error) {
	x, err := c.cadStage(jobDir, cadGuardReadOnly, cadGuardEngines)
	if err != nil {
		return cadVerdict{}, err
	}
	timeout := c.guardTimeout()
	stageCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, cpu, err := x.runBounded(stageCtx, cadOutputLimit, "cad-guard", cadGuardArgs(format, inPath)...)
	return classifyCADStage(cadStageGuard, ctx, stageCtx, timeout, out, cpu, err)
}

func cadGuardArgs(format, inPath string) []string {
	if format == "dxf" {
		return []string{
			"--format", format,
			"--max-expanded-inserts", strconv.Itoa(CADMaxDXFExpandedInserts),
			"--max-expanded-entities", strconv.Itoa(CADMaxDXFExpandedEntities),
			"--max-nesting", strconv.Itoa(CADMaxDXFNesting),
			"--max-abs-coordinate", strconv.FormatFloat(measurements.MaxAbsCoordinate, 'g', -1, 64),
			inPath,
		}
	}
	return []string{
		"--format", format,
		"--max-entities", strconv.Itoa(CADMaxSTEPEntities),
		"--max-nauo", strconv.Itoa(CADMaxNAUO),
		inPath,
	}
}

func (c CADConverter) meshStage(ctx context.Context, jobDir, format, inPath, outPath, quality, measure string) (cadVerdict, error) {
	x, err := c.cadStage(jobDir, cadMeshReadOnly, cadMeshEngines)
	if err != nil {
		return cadVerdict{}, err
	}
	timeout := c.meshTimeout()
	stageCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := []string{"--format", format, inPath, outPath}
	if quality != "" {
		args = append(args, "--quality", quality)
	}
	if measure != "" {
		args = append(args, "--measure", measure)
	}
	out, cpu, err := x.runBounded(stageCtx, cadOutputLimit, "cad-mesh", args...)
	return classifyCADStage(cadStageMesh, ctx, stageCtx, timeout, out, cpu, err)
}

func (c CADConverter) statsStage(ctx context.Context, jobDir, inPath, outPath, measure string) (cadVerdict, error) {
	x, err := c.cadStage(jobDir, cadMeshReadOnly, cadMeshEngines)
	if err != nil {
		return cadVerdict{}, err
	}
	timeout := c.meshTimeout()
	stageCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, cpu, err := x.runBounded(stageCtx, cadOutputLimit, "cad-mesh", "stats", "--measure", measure, inPath, outPath)
	return classifyCADStage(cadStageMesh, ctx, stageCtx, timeout, out, cpu, err)
}

type cadVerdict struct {
	Stage     string `json:"stage"`
	Verdict   string `json:"verdict"`
	Reason    string `json:"reason"`
	Triangles int    `json:"triangles"`
	Entities  *int   `json:"entities"`

	View       string  `json:"view"`
	Size       int     `json:"size"`
	Background string  `json:"background"`
	ProjAspect float64 `json:"proj_aspect"`

	Texts             int            `json:"texts"`
	Uncovered         int            `json:"uncovered"`
	RenderSubstituted int            `json:"render_substituted"`
	HatchTruncated    int            `json:"hatch_truncated"`
	EntitiesDrawn     int            `json:"entities_drawn"`
	PageWMM           float64        `json:"page_w_mm"`
	PageHMM           float64        `json:"page_h_mm"`
	AuditorFixes      int            `json:"auditor_fixes"`
	InsUnits          *int           `json:"insunits"`
	Codepage          *string        `json:"codepage"`
	Skipped           map[string]int `json:"skipped"`
}

func parseCADVerdict(stage string, out []byte) (cadVerdict, bool) {
	var last []byte
	for _, line := range bytes.Split(out, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if bytes.HasPrefix(line, []byte(cadVerdictPrefix)) {
			last = line
		}
	}
	if last == nil {
		return cadVerdict{}, false
	}
	var v cadVerdict
	if err := json.Unmarshal(last[len(cadVerdictPrefix):], &v); err != nil || v.Stage != stage {
		return cadVerdict{}, false
	}
	return v, true
}

func cadSignal(err error) (syscall.Signal, bool) {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 0, false
	}
	st, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !st.Signaled() {
		return 0, false
	}
	return st.Signal(), true
}

func classifyCADStage(stage string, parent, stageCtx context.Context, timeout time.Duration, out []byte, cpu engineCPU, err error) (cadVerdict, error) {
	if errors.Is(err, ErrEngineSandboxUnavailable) {
		return cadVerdict{}, fmt.Errorf("cad %s: %w", stage, err)
	}
	render := stage == cadStageRender
	dxf := stage == cadStageDXF
	if v, ok := parseCADVerdict(stage, out); ok {
		switch v.Verdict {
		case "ok":
			if err == nil {
				return v, nil
			}
		case "empty":
			if dxf {
				return v, fmt.Errorf("cad %s: %w: %w: %s", stage, ErrCADNothingDrawable, ErrCADEmptyResult, v.Reason)
			}
			return v, fmt.Errorf("cad %s: %w: %s", stage, ErrCADEmptyResult, v.Reason)
		case "failed":
			if render {
				return v, fmt.Errorf("cad %s: %w: %s", stage, ErrCADRenderFailed, v.Reason)
			}
			if dxf && v.Reason == "unknown_layout" {
				return v, fmt.Errorf("cad %s: %w", stage, ErrCADUnknownLayout)
			}
			if dxf {
				return v, fmt.Errorf("cad %s: %w: %s", stage, ErrCADDrawingFailed, v.Reason)
			}
		case "too_complex":
			if !render {
				return v, fmt.Errorf("cad %s: %w: %s", stage, ErrCADTooComplex, v.Reason)
			}
			return v, fmt.Errorf("cad %s: %w: %s", stage, ErrCADRenderFailed, v.Reason)
		case "unreadable", "incomplete":
			if !render {
				return v, fmt.Errorf("cad %s: %w: %s", stage, ErrCADUnreadable, v.Reason)
			}
			return v, fmt.Errorf("cad %s: %w: %s", stage, ErrCADRenderFailed, v.Reason)
		}
	}
	if err != nil && parent.Err() == nil && errors.Is(stageCtx.Err(), context.DeadlineExceeded) {
		switch stage {
		case cadStageMesh:
			return cadVerdict{}, fmt.Errorf("cad mesh: %w: no result within %s, engine cpu %.1fs: %v",
				ErrCADMeshTimeout, timeout, cpu.Total().Seconds(), err)
		case cadStageRender:
			return cadVerdict{}, fmt.Errorf("cad render: %w: no result within %s, engine cpu %.1fs: %v",
				ErrCADRenderTimeout, timeout, cpu.Total().Seconds(), err)
		case cadStageDXF:
			return cadVerdict{}, fmt.Errorf("cad dxf: %w: no result within %s, engine cpu %.1fs: %v",
				ErrCADDXFTimeout, timeout, cpu.Total().Seconds(), err)
		case cadStageDWG:
			return cadVerdict{}, fmt.Errorf("cad dwg: %w: no result within %s, engine cpu %.1fs: %v",
				ErrCADDWGTimeout, timeout, cpu.Total().Seconds(), err)
		}
		return cadVerdict{}, fmt.Errorf("cad %s: no verdict within the %s stage deadline: %w", stage, timeout, err)
	}
	if parent.Err() != nil {
		cause := err
		if cause == nil {
			cause = parent.Err()
		}
		return cadVerdict{}, fmt.Errorf("cad %s: %w", stage, cause)
	}
	if sig, signalled := cadSignal(err); signalled {
		if sig == syscall.SIGKILL {
			return cadVerdict{}, fmt.Errorf("cad %s: %w: %v", stage, ErrCADEngineKilled, err)
		}
		if render {
			return cadVerdict{}, fmt.Errorf("cad %s: %w: the renderer died on signal %s", stage, ErrCADRenderFailed, sig)
		}
		if dxf {
			return cadVerdict{}, fmt.Errorf("cad %s: %w: the drawing helper died on signal %s", stage, ErrCADDrawingFailed, sig)
		}
		return cadVerdict{}, fmt.Errorf("cad %s: %w: the reader died on signal %s", stage, ErrCADUnreadable, sig)
	}
	if render {
		if err != nil {
			return cadVerdict{}, fmt.Errorf("cad %s: %w: no verdict: %v", stage, ErrCADRenderFailed, err)
		}
		return cadVerdict{}, fmt.Errorf("cad %s: %w: exited 0 without a verdict", stage, ErrCADRenderFailed)
	}
	if dxf {
		if err != nil {
			return cadVerdict{}, fmt.Errorf("cad %s: %w: no verdict: %v", stage, ErrCADDrawingFailed, err)
		}
		return cadVerdict{}, fmt.Errorf("cad %s: %w: exited 0 without a verdict", stage, ErrCADDrawingFailed)
	}
	if err != nil {
		return cadVerdict{}, fmt.Errorf("cad %s: no verdict: %w", stage, err)
	}
	return cadVerdict{}, fmt.Errorf("cad %s: exited 0 without a verdict", stage)
}

func classifyCADRsvg(parent, stageCtx context.Context, timeout time.Duration, cpu engineCPU, err error, outPath string) error {
	if errors.Is(err, ErrEngineSandboxUnavailable) {
		return fmt.Errorf("cad %s: %w", cadStageRsvg, err)
	}
	if err != nil && parent.Err() == nil && errors.Is(stageCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("cad %s: %w: no result within %s, engine cpu %.1fs: %v",
			cadStageRsvg, ErrCADRsvgTimeout, timeout, cpu.Total().Seconds(), err)
	}
	if parent.Err() != nil {
		cause := err
		if cause == nil {
			cause = parent.Err()
		}
		return fmt.Errorf("cad %s: %w", cadStageRsvg, cause)
	}
	if sig, signalled := cadSignal(err); signalled {
		if sig == syscall.SIGKILL {
			return fmt.Errorf("cad %s: %w: %v", cadStageRsvg, ErrCADEngineKilled, err)
		}
		return fmt.Errorf("cad %s: %w: rsvg-convert died on signal %s", cadStageRsvg, ErrCADDrawingFailed, sig)
	}
	if err != nil {
		return fmt.Errorf("cad %s: %w: %v", cadStageRsvg, ErrCADDrawingFailed, err)
	}
	info, statErr := os.Stat(outPath)
	if statErr != nil {
		return fmt.Errorf("cad %s: %w: exited 0 without an output: %v", cadStageRsvg, ErrCADDrawingFailed, statErr)
	}
	if info.Size() == 0 {
		return fmt.Errorf("cad %s: %w: exited 0 with an empty output", cadStageRsvg, ErrCADDrawingFailed)
	}
	return nil
}

type stlBounds struct {
	Min [3]float64
	Max [3]float64
}

func (b stlBounds) Extents() [3]float64 {
	return [3]float64{b.Max[0] - b.Min[0], b.Max[1] - b.Min[1], b.Max[2] - b.Min[2]}
}

func judgeBinarySTL(path string) (int, stlBounds, error) {
	var bounds stlBounds
	f, info, err := OpenJobFile(path)
	if err != nil {
		return 0, bounds, fmt.Errorf("%w: no STL at %s: %w", ErrCADEmptyResult, path, err)
	}
	defer f.Close()
	if info.Size() < stlHeaderBytes {
		return 0, bounds, fmt.Errorf("%w: STL is %d bytes, shorter than its %d-byte header", ErrCADEmptyResult, info.Size(), stlHeaderBytes)
	}
	r := bufio.NewReaderSize(f, 1<<20)
	header := make([]byte, stlHeaderBytes)
	if _, err := io.ReadFull(r, header); err != nil {
		return 0, bounds, fmt.Errorf("%w: read STL header: %v", ErrCADEmptyResult, err)
	}
	n := int64(binary.LittleEndian.Uint32(header[80:]))
	if n == 0 {
		return 0, bounds, fmt.Errorf("%w: STL declares 0 triangles", ErrCADEmptyResult)
	}
	if want := stlHeaderBytes + stlTriangleBytes*n; info.Size() != want {
		return 0, bounds, fmt.Errorf("%w: STL is %d bytes, %d triangles need exactly %d", ErrCADEmptyResult, info.Size(), n, want)
	}
	first := true
	block := make([]byte, stlBlockTriangle*stlTriangleBytes)
	for remaining := n; remaining > 0; {
		count := min(remaining, int64(stlBlockTriangle))
		buf := block[:count*stlTriangleBytes]
		if _, err := io.ReadFull(r, buf); err != nil {
			return 0, bounds, fmt.Errorf("%w: read STL triangles: %v", ErrCADEmptyResult, err)
		}
		for t := int64(0); t < count; t++ {
			tri := buf[t*stlTriangleBytes:]
			for v := 0; v < 3; v++ {
				for axis := 0; axis < 3; axis++ {
					x := float64(math.Float32frombits(binary.LittleEndian.Uint32(tri[12+v*12+axis*4:])))
					if math.IsNaN(x) || math.IsInf(x, 0) {
						return 0, bounds, fmt.Errorf("%w: STL has a non-finite vertex coordinate", ErrCADEmptyResult)
					}
					if first || x < bounds.Min[axis] {
						bounds.Min[axis] = x
					}
					if first || x > bounds.Max[axis] {
						bounds.Max[axis] = x
					}
				}
				first = false
			}
		}
		remaining -= count
	}
	return int(n), bounds, nil
}
