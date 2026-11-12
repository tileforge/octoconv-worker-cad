package convert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

const (
	CADMinRenderSize     = 256
	CADMaxRenderSize     = 2048
	CADDefaultRenderSize = 1280

	CADViewIso       = "iso"
	CADQualityStd    = "standard"
	CADBackgroundOpq = "opaque"
	CADBackgroundTr  = "transparent"
)

var (
	cadViews = map[string]bool{
		"iso": true, "front": true, "back": true, "left": true, "right": true, "top": true, "bottom": true,
	}
	cadQualities   = map[string]bool{"draft": true, "standard": true, "fine": true}
	cadBackgrounds = map[string]bool{"opaque": true, "transparent": true}
	cadUnits       = map[string]bool{"mm": true, "cm": true, "m": true, "inch": true, "foot": true, "micrometre": true}
)

type CADOpts struct {
	View       string `json:"view,omitempty"`
	Size       *int   `json:"size,omitempty"`
	Quality    string `json:"quality,omitempty"`
	Background string `json:"background,omitempty"`
	AssumeUnit string `json:"assume_unit,omitempty"`
	Dimensions *bool  `json:"dimensions,omitempty"`
}

func ParseCADOpts(raw []byte) (CADOpts, error) {
	if err := checkStrictObject(raw); err != nil {
		return CADOpts{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return CADOpts{}, fmt.Errorf("parse opts: %w", err)
	}
	var o CADOpts
	for key, value := range fields {
		switch key {
		case "view":
			s, err := cadClosedString(key, value, cadViews)
			if err != nil {
				return CADOpts{}, err
			}
			o.View = s
		case "quality":
			s, err := cadClosedString(key, value, cadQualities)
			if err != nil {
				return CADOpts{}, err
			}
			o.Quality = s
		case "background":
			s, err := cadClosedString(key, value, cadBackgrounds)
			if err != nil {
				return CADOpts{}, err
			}
			o.Background = s
		case "assume_unit":
			s, err := cadClosedString(key, value, cadUnits)
			if err != nil {
				return CADOpts{}, err
			}
			o.AssumeUnit = s
		case "size":
			n, err := cadSize(value)
			if err != nil {
				return CADOpts{}, err
			}
			o.Size = &n
		case "dimensions":
			var enabled bool
			if string(value) != "true" && string(value) != "false" {
				return CADOpts{}, fmt.Errorf("parse opts: dimensions must be a boolean")
			}
			if err := json.Unmarshal(value, &enabled); err != nil {
				return CADOpts{}, fmt.Errorf("parse opts: dimensions: %w", err)
			}
			o.Dimensions = &enabled
		default:
			return CADOpts{}, fmt.Errorf("parse opts: unknown option %q", key)
		}
	}
	return o, nil
}

func cadClosedString(key string, raw json.RawMessage, allowed map[string]bool) (string, error) {
	var s string
	if len(raw) == 0 || raw[0] != '"' {
		return "", fmt.Errorf("parse opts: %s must be a string", key)
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("parse opts: %s: %w", key, err)
	}
	if !allowed[s] {
		return "", fmt.Errorf("unsupported %s %q", key, s)
	}
	return s, nil
}

func cadSize(raw json.RawMessage) (int, error) {
	text := string(bytes.TrimSpace(raw))
	n, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("parse opts: size must be an integer")
	}
	if n < CADMinRenderSize || n > CADMaxRenderSize {
		return 0, fmt.Errorf("size %d is outside %d..%d", n, CADMinRenderSize, CADMaxRenderSize)
	}
	return n, nil
}

func CADOptsFromMap(m map[string]any) (CADOpts, error) {
	if len(m) == 0 {
		return CADOpts{}, nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return CADOpts{}, fmt.Errorf("marshal opts: %w", err)
	}
	return ParseCADOpts(raw)
}

func (o CADOpts) Resolved() CADOpts {
	if o.View == "" {
		o.View = CADViewIso
	}
	if o.Size == nil {
		n := CADDefaultRenderSize
		o.Size = &n
	}
	if o.Quality == "" {
		o.Quality = CADQualityStd
	}
	if o.Background == "" {
		o.Background = CADBackgroundOpq
	}
	if o.Dimensions == nil {
		enabled := true
		o.Dimensions = &enabled
	}
	return o
}

func (o CADOpts) isZero() bool {
	return o == CADOpts{}
}

func ValidateCADApplicability(engine, operation, source, target string, o CADOpts) error {
	if o.isZero() {
		return nil
	}
	if engine != EngineCAD {
		return fmt.Errorf("cad options are only valid for cad-engine conversions")
	}
	if NormalizeFormat(source) == "dxf" {
		switch {
		case operation == OperationConvert:
			return fmt.Errorf("no option applies to a dxf conversion")
		case operation == OperationRender && o.View != "":
			return fmt.Errorf("view does not apply to a dxf source")
		case operation == OperationRender && o.Quality != "":
			return fmt.Errorf("quality does not apply to a dxf source")
		case operation == OperationRender && o.Dimensions != nil:
			return fmt.Errorf("dimensions does not apply to a dxf source")
		}
	}
	switch operation {
	case OperationConvert:
		other := o
		other.Quality = ""
		if !other.isZero() {
			return fmt.Errorf("only quality applies to a cad conversion")
		}
	case OperationRender:
		if o.Quality != "" && NormalizeFormat(source) == "stl" {
			return fmt.Errorf("quality does not apply to an stl source")
		}
	default:
		return fmt.Errorf("cad options do not apply to operation %q", operation)
	}
	return nil
}
