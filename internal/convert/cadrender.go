package convert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"strconv"

	"github.com/tileforge/octoconv-worker-cad/internal/measurements"
)

const (
	cadMeasureDraftName = "measure.json"
	cadMeshSTLName      = "mesh.stl"
)

type CADRenderConverter struct{ CADConverter }

func (CADRenderConverter) Operation() string { return OperationRender }

func (CADRenderConverter) Pairs() []Pair {
	return []Pair{{From: "step", To: "png"}, {From: "iges", To: "png"}, {From: "stl", To: "png"}}
}

func CADMeasurementsPath(outPath string) string {
	return outPath + ".measurements.json"
}

func (c CADRenderConverter) Convert(ctx context.Context, inPath, outPath string, opts map[string]any) error {
	return c.ConvertWithStages(ctx, inPath, outPath, opts, nil)
}

func (c CADRenderConverter) ConvertWithStages(ctx context.Context, inPath, outPath string, opts map[string]any, sink StageSink) (err error) {
	parsed, err := CADOptsFromMap(opts)
	if err != nil {
		return fmt.Errorf("cad: %w", err)
	}
	format := NormalizeFormat(filepath.Ext(inPath))
	target := NormalizeFormat(filepath.Ext(outPath))
	if (format != "step" && format != "iges" && format != "stl") || target != "png" {
		return fmt.Errorf("cad: unsupported render %s -> %s", format, target)
	}
	if err := ValidateCADApplicability(EngineCAD, OperationRender, format, target, parsed); err != nil {
		return fmt.Errorf("cad: %w", err)
	}
	o := parsed.Resolved()
	st := newCADStager(sink, OperationRender, format, "png")

	info, err := os.Stat(inPath)
	if err != nil {
		return fmt.Errorf("cad: stat input: %w", err)
	}
	if ceiling := cadMaxBytes(format); info.Size() > ceiling {
		return fmt.Errorf("cad: %w: %s input is %d bytes against a %d-byte ceiling", ErrCADInputTooLarge, format, info.Size(), ceiling)
	}

	jobDir := filepath.Dir(outPath)
	if filepath.Clean(inPath) == filepath.Join(jobDir, cadMeshSTLName) {
		return fmt.Errorf("cad: the input %s is the mesh path of its own job", inPath)
	}
	draftPath := filepath.Join(jobDir, cadMeasureDraftName)
	sidecar := CADMeasurementsPath(outPath)
	defer func() {
		if err != nil {
			os.Remove(sidecar)
		}
	}()

	renderInput := filepath.Join(jobDir, cadMeshSTLName)
	var verdict cadVerdict
	if format == "stl" {
		st.enter(cadStageMesh)
		verdict, err = c.statsStage(ctx, jobDir, inPath, renderInput, draftPath)
		if err != nil {
			return err
		}
	} else {
		st.enter(cadStageGuard)
		if _, err := c.guardStage(ctx, jobDir, format, inPath); err != nil {
			return err
		}
		st.enter(cadStageMesh)
		verdict, err = c.meshStage(ctx, jobDir, format, inPath, renderInput, o.Quality, draftPath)
		if err != nil {
			return err
		}
	}
	triangles, meshBounds, err := judgeBinarySTL(renderInput)
	if err != nil {
		return fmt.Errorf("cad: %w", err)
	}
	if err := requireSTLMatchesVerdict(triangles, verdict); err != nil {
		return err
	}

	draft, err := parseCADMeasureDraft(draftPath)
	if err != nil {
		return fmt.Errorf("cad: %w: %v", ErrCADMeasurementsInvalid, err)
	}
	if draft.Triangles != triangles {
		return fmt.Errorf("cad: %w: the measurements count %d triangles, the mesh holds %d", ErrCADMeasurementsInvalid, draft.Triangles, triangles)
	}
	if triangles > CADMaxRenderTriangles {
		return fmt.Errorf("cad: %w: %d triangles against a cap of %d", ErrCADTooComplex, triangles, CADMaxRenderTriangles)
	}
	if cadZeroExtent(draft.BBox) {
		return fmt.Errorf("cad: %w: the model has no extent", ErrCADEmptyResult)
	}
	doc, err := buildCADMeasurements(draft, o.AssumeUnit)
	if err != nil {
		return fmt.Errorf("cad: %w: %v", ErrCADMeasurementsInvalid, err)
	}

	st.enter(cadStageRender)
	if err := c.renderStage(ctx, jobDir, renderInput, outPath, o); err != nil {
		return err
	}

	st.enter(cadStageJudge)
	f, _, err := OpenJobFile(outPath)
	if err != nil {
		return fmt.Errorf("cad: %w: no image at %s: %v", ErrCADRenderRejected, outPath, err)
	}
	_, err = JudgeCADPNG(f, *o.Size, o.Background)
	f.Close()
	if err != nil {
		return err
	}

	if *o.Dimensions {
		f, _, err := OpenJobFile(outPath)
		if err != nil {
			return fmt.Errorf("cad: read model for dimensions: %w", err)
		}
		var facts CADPixelFacts
		model, err := cadReadFrame(f, *o.Size, o.Background, &facts)
		f.Close()
		if err != nil {
			return err
		}
		annotated, err := cadDimensionImage(model, meshBounds, doc, o)
		if err != nil {
			return fmt.Errorf("cad: annotate dimensions: %w", err)
		}
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, annotated); err != nil {
			return fmt.Errorf("cad: encode dimensions: %w", err)
		}
		if err := writeNewFile(outPath, encoded.Bytes(), 0o600); err != nil {
			return fmt.Errorf("cad: write dimensions: %w", err)
		}
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

func cadZeroExtent(b measurements.BBox) bool {
	for i := range b.Min {
		if i < len(b.Max) && b.Max[i]-b.Min[i] > 0 {
			return false
		}
	}
	return true
}

func (c CADRenderConverter) renderStage(ctx context.Context, jobDir, inPath, outPath string, o CADOpts) error {
	x, err := c.cadStage(jobDir, cadRenderReadOnly, cadRenderEngines)
	if err != nil {
		return err
	}
	timeout := c.renderTimeout()
	stageCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, cpu, err := x.runBounded(stageCtx, cadOutputLimit, "cad-render",
		"--view", o.View,
		"--size", strconv.Itoa(*o.Size),
		"--background", o.Background,
		inPath, outPath)
	v, err := classifyCADStage(cadStageRender, ctx, stageCtx, timeout, out, cpu, err)
	if err != nil {
		return err
	}
	if v.View != o.View || v.Size != *o.Size || v.Background != o.Background {
		return fmt.Errorf("cad render: %w: the renderer reported view %q size %d background %q, the request was %q %d %q",
			ErrCADRenderRejected, v.View, v.Size, v.Background, o.View, *o.Size, o.Background)
	}
	return nil
}
