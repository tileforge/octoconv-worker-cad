package convert

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/tileforge/octoconv-worker-cad/internal/measurements"
)

const (
	cadMeasureDraftMaxBytes = 64 << 10
	cadFactorTolerance      = 1e-9
)

var ErrCADMeasure = errors.New("cad: the measurements draft cannot become a measurements document")

var cadMMPerUnit = map[string]float64{
	measurements.UnitMM:         1,
	measurements.UnitCM:         10,
	measurements.UnitM:          1000,
	measurements.UnitInch:       25.4,
	measurements.UnitFoot:       304.8,
	measurements.UnitMicrometre: 0.001,
}

var cadMeasureDraftKeys = []string{
	"format", "declared_unit", "declared_unit_raw", "applied_factor", "bbox",
	"solids", "closed_solids", "volume", "area", "null_reason", "triangles",
}

type cadMeasureDraft struct {
	Format          string            `json:"format"`
	DeclaredUnit    string            `json:"declared_unit"`
	DeclaredUnitRaw string            `json:"declared_unit_raw"`
	AppliedFactor   *float64          `json:"applied_factor"`
	BBox            measurements.BBox `json:"bbox"`
	Solids          *int              `json:"solids"`
	ClosedSolids    *int              `json:"closed_solids"`
	Volume          *float64          `json:"volume"`
	Area            *float64          `json:"area"`
	NullReason      string            `json:"null_reason"`
	Triangles       int               `json:"triangles"`
	MeshFindings    []string          `json:"mesh_findings,omitempty"`
}

func cadDraftError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCADMeasure, fmt.Sprintf(format, args...))
}

func parseCADMeasureDraft(path string) (cadMeasureDraft, error) {
	f, _, err := OpenJobFile(path)
	if err != nil {
		return cadMeasureDraft{}, fmt.Errorf("%w: %v", ErrCADMeasure, err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, cadMeasureDraftMaxBytes+1))
	if err != nil {
		return cadMeasureDraft{}, fmt.Errorf("%w: %v", ErrCADMeasure, err)
	}
	if len(raw) > cadMeasureDraftMaxBytes {
		return cadMeasureDraft{}, cadDraftError("draft is larger than %d bytes", cadMeasureDraftMaxBytes)
	}

	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return cadMeasureDraft{}, fmt.Errorf("%w: %v", ErrCADMeasure, err)
	}
	for _, k := range cadMeasureDraftKeys {
		if _, ok := keys[k]; !ok {
			return cadMeasureDraft{}, cadDraftError("key %q is missing", k)
		}
	}

	var d cadMeasureDraft
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return cadMeasureDraft{}, fmt.Errorf("%w: %v", ErrCADMeasure, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return cadMeasureDraft{}, cadDraftError("trailing data after the object")
	}
	if err := d.checkFinite(); err != nil {
		return cadMeasureDraft{}, err
	}
	return d, nil
}

func (d cadMeasureDraft) checkFinite() error {
	check := func(name string, v float64) error {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return cadDraftError("%s is not finite", name)
		}
		return nil
	}
	for _, c := range d.BBox.Min {
		if err := check("bbox.min", c); err != nil {
			return err
		}
	}
	for _, c := range d.BBox.Max {
		if err := check("bbox.max", c); err != nil {
			return err
		}
	}
	for name, p := range map[string]*float64{"applied_factor": d.AppliedFactor, "volume": d.Volume, "area": d.Area} {
		if p == nil {
			continue
		}
		if err := check(name, *p); err != nil {
			return err
		}
	}
	return nil
}

func cadFactorMatches(applied, table float64) bool {
	return math.Abs(applied-table) <= cadFactorTolerance*table
}

func cadUnitFor(d cadMeasureDraft, assumeUnit string) (unit, source string, factor float64, err error) {
	factor = 1
	if d.AppliedFactor != nil {
		if *d.AppliedFactor <= 0 {
			return "", "", 0, cadDraftError("applied_factor %g is not positive", *d.AppliedFactor)
		}
		factor = *d.AppliedFactor
	}
	if assumeUnit != "" {
		if _, ok := cadMMPerUnit[assumeUnit]; !ok {
			return "", "", 0, cadDraftError("assume unit %q is not in the vocabulary", assumeUnit)
		}
	}
	unspecified := func() (string, string, float64, error) {
		return measurements.UnitUnspecified, measurements.SourceNone, factor, nil
	}
	switch d.Format {
	case measurements.FormatDXF:
		return cadUnitForDXF(d, assumeUnit)
	case measurements.FormatSTL:
		if assumeUnit != "" {
			return assumeUnit, measurements.SourceClientAssumed, 1, nil
		}
		return measurements.UnitUnspecified, measurements.SourceNone, 1, nil
	case measurements.FormatSTEP, measurements.FormatIGES:
	default:
		return "", "", 0, cadDraftError("format %q has no measurements draft", d.Format)
	}
	if d.DeclaredUnit != "" {
		table, ok := cadMMPerUnit[d.DeclaredUnit]
		if !ok || d.AppliedFactor == nil || !cadFactorMatches(*d.AppliedFactor, table) {
			return unspecified()
		}
		return d.DeclaredUnit, measurements.SourceDeclared, factor, nil
	}
	if d.DeclaredUnitRaw != "" {
		return unspecified()
	}
	if assumeUnit != "" {
		return assumeUnit, measurements.SourceClientAssumed, factor, nil
	}
	return unspecified()
}

func cadUnitForDXF(d cadMeasureDraft, assumeUnit string) (unit, source string, factor float64, err error) {
	if d.AppliedFactor == nil || *d.AppliedFactor != 1 {
		return "", "", 0, cadDraftError("a dxf draft is in drawing units, applied_factor must be exactly 1, got %s", factorWord(d.AppliedFactor))
	}
	if d.Solids != nil || d.ClosedSolids != nil {
		return "", "", 0, cadDraftError("a dxf draft carries no solid counts")
	}
	if d.DeclaredUnit != "" && d.DeclaredUnitRaw != "" {
		return "", "", 0, cadDraftError("declared_unit %q and declared_unit_raw %q are both set", d.DeclaredUnit, d.DeclaredUnitRaw)
	}
	if d.DeclaredUnit != "" {
		if _, ok := cadMMPerUnit[d.DeclaredUnit]; !ok {
			return "", "", 0, cadDraftError("declared_unit %q is not in the vocabulary", d.DeclaredUnit)
		}
		return d.DeclaredUnit, measurements.SourceDeclared, 1, nil
	}
	if d.DeclaredUnitRaw == "" && assumeUnit != "" {
		return assumeUnit, measurements.SourceClientAssumed, 1, nil
	}
	return measurements.UnitUnspecified, measurements.SourceNone, 1, nil
}

func factorWord(v *float64) string {
	if v == nil {
		return "null"
	}
	return fmt.Sprintf("%g", *v)
}

func buildCADMeasurements(d cadMeasureDraft, assumeUnit string) (measurements.Document, error) {
	unit, source, factor, err := cadUnitFor(d, assumeUnit)
	if err != nil {
		return measurements.Document{}, err
	}
	if (d.Volume == nil) != (d.NullReason != "") {
		return measurements.Document{}, cadDraftError("volume %s with null_reason %q", presenceWord(d.Volume), d.NullReason)
	}

	scale := func(v, power float64) float64 {
		if factor == 1 {
			return v
		}
		return v / math.Pow(factor, power)
	}
	lengths := func(in []float64) []float64 {
		out := make([]float64, len(in))
		for i, v := range in {
			out[i] = scale(v, 1)
		}
		return out
	}

	doc := measurements.Document{
		Schema:     measurements.Schema,
		Format:     d.Format,
		Unit:       unit,
		UnitSource: source,
		BBox:       measurements.BBox{Min: lengths(d.BBox.Min), Max: lengths(d.BBox.Max)},
	}
	if d.Format == measurements.FormatSTEP || d.Format == measurements.FormatIGES {
		if d.Solids != nil {
			v := *d.Solids
			doc.Solids = &v
		}
		if d.ClosedSolids != nil {
			v := *d.ClosedSolids
			doc.ClosedSolids = &v
		}
	}
	if d.Volume != nil {
		v := scale(*d.Volume, 3)
		doc.Volume = &v
	} else {
		doc.VolumeNullReason = d.NullReason
	}
	if d.Area != nil {
		v := scale(*d.Area, 2)
		doc.Area = &v
	} else {
		doc.AreaNullReason = d.NullReason
	}
	if len(d.MeshFindings) > 0 {
		if d.Format != measurements.FormatSTL {
			return measurements.Document{}, cadDraftError("mesh_findings are only for stl")
		}
		doc.MeshFindings = append([]string(nil), d.MeshFindings...)
	}
	if err := doc.Validate(); err != nil {
		return measurements.Document{}, fmt.Errorf("%w: %w", ErrCADMeasure, err)
	}
	return doc, nil
}

func presenceWord(v *float64) string {
	if v == nil {
		return "null"
	}
	return "a number"
}
