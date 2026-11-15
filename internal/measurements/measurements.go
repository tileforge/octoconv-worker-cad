package measurements

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
)

const (
	Schema                   = "octoconv.measurements/v1"
	MaxAbsCoordinate         = 1e12
	UnitMM                   = "mm"
	UnitCM                   = "cm"
	UnitM                    = "m"
	UnitInch                 = "inch"
	UnitFoot                 = "foot"
	UnitMicrometre           = "micrometre"
	UnitUnspecified          = "unspecified"
	SourceDeclared           = "declared"
	SourceClientAssumed      = "client_assumed"
	SourceNone               = "none"
	ReasonNoSolids           = "no_solids"
	ReasonOpenShell          = "open_shell"
	ReasonInvalidSolid       = "invalid_solid"
	ReasonNegativeVolume     = "negative_volume"
	ReasonSTLNotWatertight   = "stl_not_watertight"
	ReasonSTLAmbiguousShells = "stl_ambiguous_shells"
	ReasonNotApplicable2D    = "not_applicable_2d"
	FindingSTLOpen           = "stl_open"
	FindingSTLNonManifold    = "stl_non_manifold"
	FindingSTLUnoriented     = "stl_unoriented"
	FormatSTEP               = "step"
	FormatIGES               = "iges"
	FormatSTL                = "stl"
	FormatDXF                = "dxf"
	MaxSkippedKinds          = 64
	MaxSkipKindLen           = 128
)

var ErrInvalid = errors.New("invalid measurements document")

var (
	units        = []string{UnitMM, UnitCM, UnitM, UnitInch, UnitFoot, UnitMicrometre, UnitUnspecified}
	unitSources  = []string{SourceDeclared, SourceClientAssumed, SourceNone}
	nullReasons  = []string{ReasonNoSolids, ReasonOpenShell, ReasonInvalidSolid, ReasonNegativeVolume, ReasonSTLNotWatertight, ReasonSTLAmbiguousShells, ReasonNotApplicable2D}
	formats      = []string{FormatSTEP, FormatIGES, FormatSTL, FormatDXF}
	meshFindings = []string{FindingSTLOpen, FindingSTLNonManifold, FindingSTLUnoriented}
)

var reasonsByFormat = map[string][]string{
	FormatSTEP: {ReasonNoSolids, ReasonOpenShell, ReasonInvalidSolid, ReasonNegativeVolume},
	FormatIGES: {ReasonNoSolids, ReasonOpenShell, ReasonInvalidSolid, ReasonNegativeVolume},
	FormatSTL:  {ReasonSTLNotWatertight, ReasonSTLAmbiguousShells, ReasonInvalidSolid, ReasonNegativeVolume},
	FormatDXF:  {ReasonNotApplicable2D},
}

var cadCountRules = map[string]func(solids, closed int) bool{
	ReasonNoSolids:       func(solids, closed int) bool { return solids == 0 },
	ReasonOpenShell:      func(solids, closed int) bool { return solids > 0 && closed == 0 },
	ReasonInvalidSolid:   func(solids, closed int) bool { return solids > 0 && closed == 0 },
	ReasonNegativeVolume: func(solids, closed int) bool { return closed > 0 },
}

func AllUnits() []string        { return slices.Clone(units) }
func AllUnitSources() []string  { return slices.Clone(unitSources) }
func AllNullReasons() []string  { return slices.Clone(nullReasons) }
func AllFormats() []string      { return slices.Clone(formats) }
func AllMeshFindings() []string { return slices.Clone(meshFindings) }

type BBox struct {
	Min []float64 `json:"min"`
	Max []float64 `json:"max"`
}

type Body struct {
	BBox BBox `json:"bbox"`
}

type Document struct {
	Schema           string         `json:"schema"`
	Format           string         `json:"format"`
	Unit             string         `json:"unit"`
	UnitSource       string         `json:"unit_source"`
	BBox             BBox           `json:"bbox"`
	Solids           *int           `json:"solids,omitempty"`
	ClosedSolids     *int           `json:"closed_solids,omitempty"`
	Volume           *float64       `json:"volume"`
	VolumeNullReason string         `json:"volume_null_reason,omitempty"`
	Area             *float64       `json:"area"`
	AreaNullReason   string         `json:"area_null_reason,omitempty"`
	MeshFindings     []string       `json:"mesh_findings,omitempty"`
	Bodies           []Body         `json:"bodies,omitempty"`
	Skipped          map[string]int `json:"skipped,omitempty"`
}

func invalid(field, format string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalid, field, fmt.Sprintf(format, args...))
}

func Validate(raw []byte) (Document, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return Document{}, fmt.Errorf("%w: document: %v", ErrInvalid, err)
	}
	if keys == nil {
		return Document{}, invalid("document", "not a JSON object")
	}

	var d Document
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return Document{}, fmt.Errorf("%w: document: %v", ErrInvalid, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Document{}, invalid("document", "trailing data after the object")
	}

	for _, k := range []string{"schema", "format", "unit", "unit_source", "bbox", "volume", "area"} {
		if _, ok := keys[k]; !ok {
			return Document{}, invalid(k, "missing")
		}
	}
	for _, k := range []string{"volume_null_reason", "area_null_reason"} {
		if v, ok := keys[k]; ok && string(v) == `""` {
			return Document{}, invalid(k, "empty reason")
		}
	}
	for _, k := range []string{"mesh_findings", "bodies", "skipped"} {
		if v, ok := keys[k]; ok && string(v) == "null" {
			return Document{}, invalid(k, "null")
		}
	}
	for _, k := range []string{"solids", "closed_solids"} {
		_, present := keys[k]
		switch d.Format {
		case FormatSTEP, FormatIGES:
			if !present {
				return Document{}, invalid(k, "missing for format %s", d.Format)
			}
		case FormatSTL, FormatDXF:
			if present {
				return Document{}, invalid(k, "not allowed for format %s", d.Format)
			}
		}
	}

	if err := d.Validate(); err != nil {
		return Document{}, err
	}
	return d, nil
}

func (d Document) Validate() error {
	if d.Schema != Schema {
		return invalid("schema", "got %q, want %q", d.Schema, Schema)
	}
	if !slices.Contains(formats, d.Format) {
		return invalid("format", "%q is not in the vocabulary", d.Format)
	}
	if !slices.Contains(units, d.Unit) {
		return invalid("unit", "%q is not in the vocabulary", d.Unit)
	}
	if !slices.Contains(unitSources, d.UnitSource) {
		return invalid("unit_source", "%q is not in the vocabulary", d.UnitSource)
	}
	if (d.Unit == UnitUnspecified) != (d.UnitSource == SourceNone) {
		return invalid("unit_source", "unit %q cannot pair with unit_source %q: unspecified pairs with none and only with none", d.Unit, d.UnitSource)
	}
	if d.Format == FormatSTL && d.UnitSource == SourceDeclared {
		return invalid("unit_source", "stl carries no units, declared is not possible")
	}

	dims := 3
	if d.Format == FormatDXF {
		dims = 2
	}
	if err := d.validateBox("bbox", d.BBox, dims); err != nil {
		return err
	}

	cad := d.Format == FormatSTEP || d.Format == FormatIGES
	if cad && d.bboxAxes() == 0 {
		return invalid("bbox", "min equals max on every axis: a point is not a measured shape")
	}
	solids, closed := 0, 0
	if cad {
		if d.Solids == nil {
			return invalid("solids", "missing for format %s", d.Format)
		}
		if d.ClosedSolids == nil {
			return invalid("closed_solids", "missing for format %s", d.Format)
		}
		if *d.Solids < 0 {
			return invalid("solids", "negative: %d", *d.Solids)
		}
		if *d.ClosedSolids < 0 {
			return invalid("closed_solids", "negative: %d", *d.ClosedSolids)
		}
		if *d.ClosedSolids > *d.Solids {
			return invalid("closed_solids", "%d exceeds solids %d", *d.ClosedSolids, *d.Solids)
		}
		solids, closed = *d.Solids, *d.ClosedSolids
	} else {
		if d.Solids != nil {
			return invalid("solids", "not allowed for format %s", d.Format)
		}
		if d.ClosedSolids != nil {
			return invalid("closed_solids", "not allowed for format %s", d.Format)
		}
	}

	if err := d.validateQuantity("volume", d.Volume, d.VolumeNullReason, cad, solids, closed); err != nil {
		return err
	}
	if err := d.validateQuantity("area", d.Area, d.AreaNullReason, cad, solids, closed); err != nil {
		return err
	}
	if (d.Volume == nil) != (d.Area == nil) {
		return invalid("area", "volume and area are both null or both numbers, got volume %s and area %s", presence(d.Volume), presence(d.Area))
	}
	if d.Volume == nil && d.VolumeNullReason != d.AreaNullReason {
		return invalid("area_null_reason", "%q differs from volume_null_reason %q: both describe the same shape", d.AreaNullReason, d.VolumeNullReason)
	}
	if err := d.validateMeshFindings(); err != nil {
		return err
	}
	if err := d.validateBodies(); err != nil {
		return err
	}
	if err := d.validateSkipped(); err != nil {
		return err
	}
	return nil
}

func (d Document) validateMeshFindings() error {
	if len(d.MeshFindings) == 0 {
		if d.MeshFindings != nil {
			return invalid("mesh_findings", "empty")
		}
		return nil
	}
	if d.Format != FormatSTL {
		return invalid("mesh_findings", "not allowed for format %s", d.Format)
	}
	prev := -1
	seen := make(map[string]bool, len(d.MeshFindings))
	for _, code := range d.MeshFindings {
		i := slices.Index(meshFindings, code)
		if i < 0 {
			return invalid("mesh_findings", "%q is not in the vocabulary", code)
		}
		if seen[code] {
			return invalid("mesh_findings", "duplicate %q", code)
		}
		if i < prev {
			return invalid("mesh_findings", "not in canonical order")
		}
		seen[code] = true
		prev = i
	}
	return nil
}

func presence(v *float64) string {
	if v == nil {
		return "null"
	}
	return "a number"
}

func (d Document) bboxAxes() int {
	n := 0
	for i := range d.BBox.Min {
		if d.BBox.Max[i] > d.BBox.Min[i] {
			n++
		}
	}
	return n
}

func (d Document) validateBox(field string, box BBox, dims int) error {
	if len(box.Min) != dims || len(box.Max) != dims {
		return invalid(field, "format %s needs %d coordinates per corner, got min %d and max %d", d.Format, dims, len(box.Min), len(box.Max))
	}
	for _, corner := range []struct {
		name string
		v    []float64
	}{{"min", box.Min}, {"max", box.Max}} {
		for i, c := range corner.v {
			if math.IsNaN(c) || math.IsInf(c, 0) {
				return invalid(field, "%s[%d] is not finite", corner.name, i)
			}
			if math.Abs(c) > MaxAbsCoordinate {
				return invalid(field, "%s[%d] = %g exceeds %g", corner.name, i, c, MaxAbsCoordinate)
			}
		}
	}
	for i := range box.Min {
		if box.Min[i] > box.Max[i] {
			return invalid(field, "min[%d] = %g is greater than max[%d] = %g", i, box.Min[i], i, box.Max[i])
		}
	}
	return nil
}

func (d Document) validateBodies() error {
	if len(d.Bodies) == 0 {
		if d.Bodies != nil {
			return invalid("bodies", "empty")
		}
		return nil
	}
	if d.Format != FormatSTEP && d.Format != FormatIGES {
		return invalid("bodies", "not allowed for format %s", d.Format)
	}
	if len(d.Bodies) != *d.Solids {
		return invalid("bodies", "length %d is not solids %d", len(d.Bodies), *d.Solids)
	}
	for i, body := range d.Bodies {
		if err := d.validateBox(fmt.Sprintf("bodies[%d].bbox", i), body.BBox, 3); err != nil {
			return err
		}
	}
	return nil
}

func (d Document) validateSkipped() error {
	if len(d.Skipped) == 0 {
		if d.Skipped != nil {
			return invalid("skipped", "empty")
		}
		return nil
	}
	if d.Format != FormatDXF {
		return invalid("skipped", "not allowed for format %s", d.Format)
	}
	if len(d.Skipped) > MaxSkippedKinds {
		return invalid("skipped", "%d kinds exceeds %d", len(d.Skipped), MaxSkippedKinds)
	}
	names := make([]string, 0, len(d.Skipped))
	for name := range d.Skipped {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if !skipKindOK(name) {
			return invalid("skipped", "kind %q is not a drawing skip kind", name)
		}
		if d.Skipped[name] < 1 {
			return invalid("skipped", "kind %q has count %d", name, d.Skipped[name])
		}
	}
	return nil
}

func skipKindOK(kind string) bool {
	if kind == "" || len([]rune(kind)) > MaxSkipKindLen {
		return false
	}
	rest, proxy := strings.CutPrefix(kind, "PROXY:")
	if proxy {
		if rest == "" || strings.Contains(rest, ":") {
			return false
		}
	} else if strings.Contains(kind, ":") {
		return false
	}
	for _, r := range rest {
		digit := r >= '0' && r <= '9'
		letter := r >= 'A' && r <= 'Z'
		if !letter && !digit && r != '_' {
			return false
		}
	}
	return rest != ""
}

func (d Document) validateQuantity(name string, v *float64, reason string, cad bool, solids, closed int) error {
	reasonField := name + "_null_reason"
	if (v == nil) != (reason != "") {
		if v == nil {
			return invalid(name, "null needs a %s", reasonField)
		}
		return invalid(reasonField, "a non-null %s forbids a reason, got %q", name, reason)
	}
	if v == nil {
		if !slices.Contains(nullReasons, reason) {
			return invalid(reasonField, "%q is not in the vocabulary", reason)
		}
		if !slices.Contains(reasonsByFormat[d.Format], reason) {
			return invalid(reasonField, "%q is not allowed for format %s", reason, d.Format)
		}
		if rule, ok := cadCountRules[reason]; ok && cad && !rule(solids, closed) {
			return invalid(reasonField, "%q contradicts solids %d and closed_solids %d", reason, solids, closed)
		}
		return nil
	}
	if d.Format == FormatDXF {
		return invalid(name, "a 2D document has no %s, it must be null", name)
	}
	if math.IsNaN(*v) || math.IsInf(*v, 0) {
		return invalid(name, "not finite")
	}
	if *v <= 0 {
		return invalid(name, "%g is not strictly positive; zero is never a measurement", *v)
	}
	if cad && closed < 1 {
		return invalid(name, "a positive value needs at least one closed solid")
	}
	if name == "volume" && d.bboxAxes() < len(d.BBox.Min) {
		return invalid(name, "a positive volume needs a bbox with extent on all %d axes, got %d", len(d.BBox.Min), d.bboxAxes())
	}
	return nil
}
