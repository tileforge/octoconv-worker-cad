package convert

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/tileforge/octoconv-worker-cad/internal/measurements"
)

func floatPtr(v float64) *float64 { return &v }

func intPtr(v int) *int { return &v }

func writeDraft(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "measure.json")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDraftKeepsSkipCountsAndScalesSolidBoxes(t *testing.T) {
	dxf := writeDraft(t, `{
		"format":"dxf","declared_unit":"mm","declared_unit_raw":"","applied_factor":1,
		"bbox":{"min":[0,0],"max":[4,5]},
		"solids":null,"closed_solids":null,"volume":null,"area":null,
		"null_reason":"not_applicable_2d","triangles":0,
		"skipped":{"IMAGE":2,"PROXY:ACAD_PROXY_ENTITY":1}
	}`)
	parsed, err := parseCADMeasureDraft(dxf)
	if err != nil {
		t.Fatalf("parse dxf: %v", err)
	}
	doc, err := buildCADMeasurements(parsed, "")
	if err != nil {
		t.Fatalf("build dxf: %v", err)
	}
	if doc.Skipped["IMAGE"] != 2 || doc.Skipped["PROXY:ACAD_PROXY_ENTITY"] != 1 || len(doc.Skipped) != 2 {
		t.Fatalf("skipped = %v", doc.Skipped)
	}
	parsed.Skipped["IMAGE"] = 9
	if doc.Skipped["IMAGE"] != 2 {
		t.Fatalf("document skipped shares the draft map: %v", doc.Skipped)
	}

	factor := 25.4
	step := cadMeasureDraft{
		Format: measurements.FormatSTEP, DeclaredUnit: measurements.UnitInch, AppliedFactor: &factor,
		BBox:         measurements.BBox{Min: []float64{0, 0, 0}, Max: []float64{factor, factor, factor}},
		Solids:       intPtr(1),
		ClosedSolids: intPtr(1),
		Volume:       floatPtr(factor * factor * factor),
		Area:         floatPtr(factor * factor),
		Triangles:    12,
		Bodies: []cadMeasureBody{{
			BBox: measurements.BBox{Min: []float64{0, 0, 0}, Max: []float64{factor, factor / 2, factor}},
		}},
	}
	doc, err = buildCADMeasurements(step, "")
	if err != nil {
		t.Fatalf("build step: %v", err)
	}
	if doc.Unit != measurements.UnitInch || doc.UnitSource != measurements.SourceDeclared {
		t.Fatalf("unit %s %s", doc.Unit, doc.UnitSource)
	}
	if len(doc.Bodies) != 1 || math.Abs(doc.Bodies[0].BBox.Max[1]-0.5) > 1e-9 || math.Abs(doc.BBox.Max[0]-1) > 1e-9 {
		t.Fatalf("scaled boxes document %v body %v", doc.BBox, doc.Bodies)
	}
	if doc.Volume == nil || doc.Area == nil || math.Abs(*doc.Volume-1) > 1e-9 || math.Abs(*doc.Area-1) > 1e-9 {
		t.Fatalf("scaled volume %v area %v", doc.Volume, doc.Area)
	}

	old := step
	old.Bodies = nil
	if _, err := buildCADMeasurements(old, ""); err != nil {
		t.Fatalf("step draft without bodies: %v", err)
	}
}

func TestDraftRejectsTheNewFieldsOnTheWrongFormat(t *testing.T) {
	one := 1.0
	body := []cadMeasureBody{{BBox: measurements.BBox{Min: []float64{0, 0, 0}, Max: []float64{1, 1, 1}}}}
	dxf := cadMeasureDraft{
		Format: measurements.FormatDXF, DeclaredUnit: measurements.UnitMM, AppliedFactor: &one,
		BBox:       measurements.BBox{Min: []float64{0, 0}, Max: []float64{1, 1}},
		NullReason: measurements.ReasonNotApplicable2D,
		Bodies:     body,
	}
	if _, err := buildCADMeasurements(dxf, ""); !errors.Is(err, ErrCADMeasure) {
		t.Fatalf("dxf bodies: %v", err)
	}

	step := cadMeasureDraft{
		Format: measurements.FormatSTEP, DeclaredUnit: measurements.UnitMM, AppliedFactor: &one,
		BBox:         measurements.BBox{Min: []float64{0, 0, 0}, Max: []float64{1, 1, 1}},
		Solids:       intPtr(1),
		ClosedSolids: intPtr(1),
		Volume:       floatPtr(1),
		Area:         floatPtr(1),
		Skipped:      map[string]int{"IMAGE": 1},
	}
	if _, err := buildCADMeasurements(step, ""); !errors.Is(err, ErrCADMeasure) {
		t.Fatalf("step skipped: %v", err)
	}

	stl := cadMeasureDraft{
		Format: measurements.FormatSTL, AppliedFactor: &one,
		BBox:   measurements.BBox{Min: []float64{0, 0, 0}, Max: []float64{1, 1, 1}},
		Volume: floatPtr(1),
		Area:   floatPtr(1),
		Bodies: body,
	}
	if _, err := buildCADMeasurements(stl, ""); !errors.Is(err, ErrCADMeasure) {
		t.Fatalf("stl bodies: %v", err)
	}

	empty := dxf
	empty.Bodies = nil
	empty.Skipped = map[string]int{}
	doc, err := buildCADMeasurements(empty, "")
	if err != nil || doc.Skipped != nil {
		t.Fatalf("empty skipped = %v, err %v", doc.Skipped, err)
	}

	nan := math.NaN()
	bad := step
	bad.Skipped = nil
	bad.Bodies = []cadMeasureBody{{BBox: measurements.BBox{Min: []float64{nan, 0, 0}, Max: []float64{1, 1, 1}}}}
	if err := bad.checkFinite(); err == nil || !errors.Is(err, ErrCADMeasure) {
		t.Fatalf("non-finite body: %v", err)
	}
}

func TestDraftRejectsAnUnknownField(t *testing.T) {
	path := writeDraft(t, `{
		"format":"dxf","declared_unit":"mm","declared_unit_raw":"","applied_factor":1,
		"bbox":{"min":[0,0],"max":[1,1]},
		"solids":null,"closed_solids":null,"volume":null,"area":null,
		"null_reason":"not_applicable_2d","triangles":0,"edges":[]
	}`)
	if _, err := parseCADMeasureDraft(path); err == nil || !errors.Is(err, ErrCADMeasure) {
		t.Fatalf("unknown field: %v", err)
	}
}
