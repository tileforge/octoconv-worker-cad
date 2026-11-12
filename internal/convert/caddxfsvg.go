package convert

import (
	"bufio"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

var ErrCADSVGRejected = errors.New("cad: the drawing's SVG failed the safety check")

type CADSVGFacts struct {
	WidthMM, HeightMM  float64
	ViewBoxW, ViewBoxH float64
	Texts              []string
	TextSizes          []float64
	TextOpacity        []float64
	TextFonts          []string
	Visible            int
}

const (
	cadSVGNamespace   = "http://www.w3.org/2000/svg"
	cadXMLNamespace   = "http://www.w3.org/XML/1998/namespace"
	cadSVGMaxDepth    = 16
	cadSVGQuoteRunes  = 40
	cadSVGDeclaration = `<?xml version="1.0" encoding="UTF-8"?>` + "\n"
)

const cadSVGNum = `-?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][-+]?\d+)?`

var (
	cadSVGNumberRe   = regexp.MustCompile(`^` + cadSVGNum + `$`)
	cadSVGMatrixRe   = regexp.MustCompile(`^matrix\(\s*(` + cadSVGNum + `)(?:\s*,\s*|\s+)(` + cadSVGNum + `)(?:\s*,\s*|\s+)(` + cadSVGNum + `)(?:\s*,\s*|\s+)(` + cadSVGNum + `)(?:\s*,\s*|\s+)(` + cadSVGNum + `)(?:\s*,\s*|\s+)(` + cadSVGNum + `)\s*\)$`)
	cadSVGColorRe    = regexp.MustCompile(`^(?:#[0-9a-fA-F]{6}|none)$`)
	cadSVGPathRe     = regexp.MustCompile(`^[MmLlQqCcZz0-9 .\-]+$`)
	cadSVGPointsRe   = regexp.MustCompile(`^\s*` + cadSVGNum + `(?:(?:\s*,\s*|\s+)` + cadSVGNum + `)*\s*$`)
	cadSVGPointNumRe = regexp.MustCompile(cadSVGNum)
	cadSVGMMRe       = regexp.MustCompile(`^\d{1,6}(?:\.\d{1,6})?mm$`)
	cadSVGViewBoxRe  = regexp.MustCompile(`^0 0 (\d{1,9}) (\d{1,9})$`)
	cadSVGDeclRe     = regexp.MustCompile(`^\s*version\s*=\s*(?:"1\.0"|'1\.0')(?:\s+encoding\s*=\s*(?:"([^"]*)"|'([^']*)'))?\s*$`)
)

type cadSVGAttrCheck func(string) bool

func cadSVGFinite(s string) (float64, bool) {
	if !cadSVGNumberRe.MatchString(s) {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, false
	}
	return v, true
}

func cadSVGAnyNumber(s string) bool {
	_, ok := cadSVGFinite(s)
	return ok
}

func cadSVGNonNegative(s string) bool {
	v, ok := cadSVGFinite(s)
	return ok && v >= 0
}

func cadSVGPositive(s string) bool {
	v, ok := cadSVGFinite(s)
	return ok && v > 0
}

func cadSVGUnit(s string) bool {
	v, ok := cadSVGFinite(s)
	return ok && v >= 0 && v <= 1
}

func cadSVGColor(s string) bool { return cadSVGColorRe.MatchString(s) }

func cadSVGOneOf(values ...string) cadSVGAttrCheck {
	return func(s string) bool {
		for _, v := range values {
			if s == v {
				return true
			}
		}
		return false
	}
}

func cadSVGMatrix(s string) bool {
	m := cadSVGMatrixRe.FindStringSubmatch(s)
	if m == nil {
		return false
	}
	for _, n := range m[1:] {
		if !cadSVGAnyNumber(n) {
			return false
		}
	}
	return true
}

func cadSVGMatrixScale(s string) float64 {
	m := cadSVGMatrixRe.FindStringSubmatch(s)
	if m == nil {
		return math.Inf(1)
	}
	var v [4]float64
	for i := range v {
		f, err := strconv.ParseFloat(m[i+1], 64)
		if err != nil {
			return math.Inf(1)
		}
		v[i] = f
	}
	return math.Sqrt(math.Abs(v[0]*v[3] - v[1]*v[2]))
}

func cadSVGTextSize(attrs []xml.Attr, scale float64) (float64, bool) {
	for _, a := range attrs {
		if a.Name.Space == "" && a.Name.Local == "font-size" {
			f, err := strconv.ParseFloat(a.Value, 64)
			if err != nil {
				return math.Inf(1), true
			}
			return f * scale, true
		}
	}
	return 0, false
}

var cadSVGStretches = []string{"ultra-condensed", "extra-condensed", "condensed", "semi-condensed",
	"semi-expanded", "expanded", "extra-expanded", "ultra-expanded"}

var cadSVGFontAttrs = []string{"font-family", "font-size", "font-weight", "font-style", "font-stretch"}

func cadSVGFontOf(attrs []xml.Attr) string {
	var parts []string
	for _, name := range cadSVGFontAttrs {
		for _, a := range attrs {
			if a.Name.Space == "" && a.Name.Local == name {
				parts = append(parts, name+"="+strconv.Quote(a.Value))
			}
		}
	}
	return strings.Join(parts, " ")
}

func cadSVGPathData(s string) bool { return cadSVGPathRe.MatchString(s) }

func cadSVGPoints(s string) bool {
	if !cadSVGPointsRe.MatchString(s) {
		return false
	}
	nums := cadSVGPointNumRe.FindAllString(s, -1)
	if len(nums) < 2 || len(nums)%2 != 0 {
		return false
	}
	for _, n := range nums {
		if !cadSVGAnyNumber(n) {
			return false
		}
	}
	return true
}

func cadSVGMillimetres(s string) bool {
	if !cadSVGMMRe.MatchString(s) {
		return false
	}
	v, err := strconv.ParseFloat(strings.TrimSuffix(s, "mm"), 64)
	return err == nil && v > 0
}

func cadSVGViewBox(s string) bool {
	m := cadSVGViewBoxRe.FindStringSubmatch(s)
	if m == nil {
		return false
	}
	w, errW := strconv.Atoi(m[1])
	h, errH := strconv.Atoi(m[2])
	return errW == nil && errH == nil && w > 0 && h > 0
}

type cadSVGPaint struct {
	none          bool
	opacity       float64
	strokeNone    bool
	strokeOpacity float64
	strokeWidth   float64
	hidden        bool
}

var cadSVGInitialPaint = cadSVGPaint{opacity: 1, strokeNone: true, strokeOpacity: 1, strokeWidth: 1}

func (p cadSVGPaint) visible() float64 {
	if p.none {
		return 0
	}
	return p.opacity
}

func (p cadSVGPaint) stroked() bool {
	return !p.strokeNone && p.strokeOpacity > 0 && p.strokeWidth > 0
}

func (p cadSVGPaint) inked() bool {
	return p.visible() > 0 || p.stroked()
}

func cadSVGPaintOf(parent cadSVGPaint, attrs []xml.Attr) cadSVGPaint {
	for _, a := range attrs {
		if a.Name.Space != "" {
			continue
		}
		v, finite := cadSVGFinite(a.Value)
		switch a.Name.Local {
		case "fill":
			parent.none = a.Value == "none"
		case "fill-opacity":
			if finite {
				parent.opacity = v
			}
		case "stroke":
			parent.strokeNone = a.Value == "none"
		case "stroke-opacity":
			if finite {
				parent.strokeOpacity = v
			}
		case "stroke-width":
			if finite {
				parent.strokeWidth = v
			}
		}
	}
	return parent
}

func cadSVGAttr(attrs []xml.Attr, name string) string {
	for _, a := range attrs {
		if a.Name.Space == "" && a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func cadSVGPositiveAttr(attrs []xml.Attr, names ...string) bool {
	for _, n := range names {
		v, ok := cadSVGFinite(cadSVGAttr(attrs, n))
		if !ok || v <= 0 {
			return false
		}
	}
	return true
}

func cadSVGShapeHasExtent(name string, attrs []xml.Attr) bool {
	switch name {
	case "rect":
		return cadSVGPositiveAttr(attrs, "width", "height")
	case "circle":
		return cadSVGPositiveAttr(attrs, "r")
	case "ellipse":
		return cadSVGPositiveAttr(attrs, "rx", "ry")
	case "path":
		return strings.ContainsAny(cadSVGAttr(attrs, "d"), "LlQqCcZz")
	case "polyline", "polygon":
		return len(cadSVGPointNumRe.FindAllString(cadSVGAttr(attrs, "points"), -1)) >= 4
	}
	return true
}

func cadSVGShapeVisible(name string, attrs []xml.Attr, p cadSVGPaint) bool {
	if p.hidden {
		return false
	}
	filled := name != "line" && p.visible() > 0
	return (filled || p.stroked()) && cadSVGShapeHasExtent(name, attrs)
}

type cadSVGElementRule struct {
	attrs    map[string]cadSVGAttrCheck
	required []string
	children []string
	text     bool
}

func cadSVGWith(base map[string]cadSVGAttrCheck, extra map[string]cadSVGAttrCheck) map[string]cadSVGAttrCheck {
	out := make(map[string]cadSVGAttrCheck, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

var cadSVGShapes = []string{"path", "polyline", "polygon", "line", "circle", "ellipse", "rect"}

var cadSVGRules = func() map[string]cadSVGElementRule {
	presentation := map[string]cadSVGAttrCheck{
		"fill":            cadSVGColor,
		"fill-opacity":    cadSVGUnit,
		"fill-rule":       cadSVGOneOf("nonzero", "evenodd"),
		"stroke":          cadSVGColor,
		"stroke-width":    cadSVGNonNegative,
		"stroke-opacity":  cadSVGUnit,
		"stroke-linecap":  cadSVGOneOf("butt", "round", "square"),
		"stroke-linejoin": cadSVGOneOf("miter", "round", "bevel"),
	}
	textAttrs := map[string]cadSVGAttrCheck{
		"font-family":  cadSVGOneOf("DejaVu Sans", "DejaVu Sans Mono", "DejaVu Serif"),
		"font-size":    cadSVGPositive,
		"font-weight":  cadSVGOneOf("bold", "100", "200", "300", "500", "600", "800", "900"),
		"font-style":   cadSVGOneOf("italic", "oblique"),
		"font-stretch": cadSVGOneOf(cadSVGStretches...),
		"fill":         cadSVGColor,
		"fill-opacity": cadSVGUnit,
		"xml:space":    cadSVGOneOf("preserve", "default"),
	}
	drawable := append([]string{"g", "text"}, cadSVGShapes...)
	return map[string]cadSVGElementRule{
		"svg": {
			attrs: map[string]cadSVGAttrCheck{
				"xmlns":   cadSVGOneOf(cadSVGNamespace),
				"width":   cadSVGMillimetres,
				"height":  cadSVGMillimetres,
				"viewBox": cadSVGViewBox,
			},
			required: []string{"xmlns", "width", "height", "viewBox"},
			children: append([]string{"defs"}, drawable...),
		},
		"g":        {attrs: presentation, children: drawable},
		"defs":     {attrs: map[string]cadSVGAttrCheck{}, children: []string{"clipPath"}},
		"clipPath": {attrs: map[string]cadSVGAttrCheck{}, children: cadSVGShapes},
		"path":     {attrs: cadSVGWith(presentation, map[string]cadSVGAttrCheck{"d": cadSVGPathData}), required: []string{"d"}},
		"polyline": {attrs: cadSVGWith(presentation, map[string]cadSVGAttrCheck{"points": cadSVGPoints}), required: []string{"points"}},
		"polygon":  {attrs: cadSVGWith(presentation, map[string]cadSVGAttrCheck{"points": cadSVGPoints}), required: []string{"points"}},
		"line": {attrs: cadSVGWith(presentation, map[string]cadSVGAttrCheck{
			"x1": cadSVGAnyNumber, "y1": cadSVGAnyNumber, "x2": cadSVGAnyNumber, "y2": cadSVGAnyNumber,
		})},
		"circle": {attrs: cadSVGWith(presentation, map[string]cadSVGAttrCheck{
			"cx": cadSVGAnyNumber, "cy": cadSVGAnyNumber, "r": cadSVGNonNegative,
		})},
		"ellipse": {attrs: cadSVGWith(presentation, map[string]cadSVGAttrCheck{
			"cx": cadSVGAnyNumber, "cy": cadSVGAnyNumber, "rx": cadSVGNonNegative, "ry": cadSVGNonNegative,
		})},
		"rect": {attrs: cadSVGWith(presentation, map[string]cadSVGAttrCheck{
			"x": cadSVGAnyNumber, "y": cadSVGAnyNumber, "width": cadSVGNonNegative, "height": cadSVGNonNegative,
		})},
		"text": {
			attrs:    cadSVGWith(textAttrs, map[string]cadSVGAttrCheck{"transform": cadSVGMatrix}),
			children: []string{"tspan"},
			text:     true,
		},
		"tspan": {attrs: textAttrs, text: true},
	}
}()

func cadSVGClip(s string) string {
	if utf8.RuneCountInString(s) > cadSVGQuoteRunes {
		return string([]rune(s)[:cadSVGQuoteRunes]) + "..."
	}
	return s
}

func cadSVGQuote(s string) string {
	return strconv.Quote(cadSVGClip(s))
}

func cadSVGSyntax(err error) string {
	var se *xml.SyntaxError
	if errors.As(err, &se) {
		return fmt.Sprintf("line %d: %s", se.Line, cadSVGQuote(se.Msg))
	}
	return cadSVGQuote(err.Error())
}

func cadSVGReject(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCADSVGRejected, fmt.Sprintf(format, args...))
}

type cadSVGCountingReader struct {
	r   io.Reader
	n   int64
	err error
}

func (c *cadSVGCountingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if err != nil && err != io.EOF && c.err == nil {
		c.err = err
	}
	return n, err
}

func cadSVGEscapeAttr(w *bufio.Writer, s string) {
	for _, r := range s {
		switch r {
		case '&':
			w.WriteString("&amp;")
		case '<':
			w.WriteString("&lt;")
		case '>':
			w.WriteString("&gt;")
		case '"':
			w.WriteString("&#34;")
		case '\'':
			w.WriteString("&#39;")
		case '\t':
			w.WriteString("&#x9;")
		case '\n':
			w.WriteString("&#xA;")
		case '\r':
			w.WriteString("&#xD;")
		default:
			w.WriteRune(r)
		}
	}
}

func cadSVGAttrName(a xml.Attr) (string, bool) {
	switch a.Name.Space {
	case "":
		return a.Name.Local, true
	case cadXMLNamespace:
		return "xml:" + a.Name.Local, true
	}
	return a.Name.Space + ":" + a.Name.Local, false
}

func cadSVGOnlySpace(b []byte) bool {
	for _, c := range b {
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return false
		}
	}
	return true
}

func ValidateCADSVG(r io.Reader, w io.Writer, maxBytes int64) (CADSVGFacts, error) {
	if maxBytes <= 0 {
		return CADSVGFacts{}, fmt.Errorf("cad: SVG byte limit %d is not positive", maxBytes)
	}
	cr := &cadSVGCountingReader{r: io.LimitReader(r, maxBytes+1)}
	dec := xml.NewDecoder(cr)
	dec.Strict = true
	bw := bufio.NewWriter(w)
	facts, err := cadSVGWalk(dec, cr, bw, maxBytes)
	if err != nil {
		return CADSVGFacts{}, err
	}
	if err := bw.Flush(); err != nil {
		return CADSVGFacts{}, fmt.Errorf("cad: write the validated SVG: %w", err)
	}
	return facts, nil
}

func cadSVGWalk(dec *xml.Decoder, cr *cadSVGCountingReader, bw *bufio.Writer, maxBytes int64) (CADSVGFacts, error) {
	var facts CADSVGFacts
	bw.WriteString(cadSVGDeclaration)
	var stack []string
	var paints []cadSVGPaint
	first, rootDone := true, false
	var text *strings.Builder
	var textFont []string
	textInk := false
	textScale, textSize, textOpacity := 1.0, math.Inf(1), 1.0
	for {
		tok, err := dec.Token()
		if cr.n > maxBytes {
			return facts, fmt.Errorf("%w: %w", ErrCADTooComplex, cadSVGReject("the SVG exceeds %d bytes", maxBytes))
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			if cr.err != nil {
				return facts, fmt.Errorf("cad: read the SVG: %w", cr.err)
			}
			return facts, cadSVGReject("malformed XML at %s", cadSVGSyntax(err))
		}
		isFirst := first
		first = false
		switch t := tok.(type) {
		case xml.ProcInst:
			if !isFirst || t.Target != "xml" {
				return facts, cadSVGReject("processing instruction %s", cadSVGQuote(t.Target))
			}
			m := cadSVGDeclRe.FindStringSubmatch(string(t.Inst))
			if m == nil {
				return facts, cadSVGReject("XML declaration %s", cadSVGQuote(string(t.Inst)))
			}
			if enc := m[1] + m[2]; strings.Contains(string(t.Inst), "encoding") && !strings.EqualFold(enc, "utf-8") {
				return facts, cadSVGReject("XML declaration encoding %s", cadSVGQuote(enc))
			}
		case xml.Directive:
			return facts, cadSVGReject("directive (DOCTYPE or entity declaration)")
		case xml.Comment:
			return facts, cadSVGReject("comment")
		case xml.StartElement:
			if rootDone {
				return facts, cadSVGReject("element %s after the root closed", cadSVGClip(t.Name.Local))
			}
			if t.Name.Space != cadSVGNamespace {
				return facts, cadSVGReject("element %s outside the SVG namespace (%s)", cadSVGClip(t.Name.Local), cadSVGQuote(t.Name.Space))
			}
			name := t.Name.Local
			rule, known := cadSVGRules[name]
			if len(stack) == 0 {
				if name != "svg" {
					return facts, cadSVGReject("root element is %s, want svg", cadSVGClip(name))
				}
			} else {
				parent := stack[len(stack)-1]
				allowed := false
				for _, c := range cadSVGRules[parent].children {
					if c == name {
						allowed = true
					}
				}
				if !known || !allowed {
					return facts, cadSVGReject("element %s is not allowed inside %s", cadSVGClip(name), parent)
				}
			}
			if len(stack)+1 > cadSVGMaxDepth {
				return facts, cadSVGReject("element depth exceeds %d", cadSVGMaxDepth)
			}
			seen := make(map[string]bool, len(t.Attr))
			for _, a := range t.Attr {
				if a.Name.Space == "xmlns" || (a.Name.Space == "" && a.Name.Local == "xmlns" && len(stack) > 0) {
					return facts, cadSVGReject("namespace declaration %s on %s", cadSVGClip(a.Name.Local), name)
				}
				an, plain := cadSVGAttrName(a)
				check, ok := rule.attrs[an]
				if !plain || !ok {
					return facts, cadSVGReject("attribute %s is not allowed on %s", cadSVGClip(an), name)
				}
				if seen[an] {
					return facts, cadSVGReject("duplicate attribute %s on %s", an, name)
				}
				seen[an] = true
				if !check(a.Value) {
					return facts, cadSVGReject("attribute %s=%s on %s fails its grammar", an, cadSVGQuote(a.Value), name)
				}
			}
			for _, req := range rule.required {
				if !seen[req] {
					return facts, cadSVGReject("element %s lacks the attribute %s", name, req)
				}
			}
			if name == "svg" {
				for _, a := range t.Attr {
					v, _ := strconv.ParseFloat(strings.TrimSuffix(a.Value, "mm"), 64)
					switch a.Name.Local {
					case "width":
						facts.WidthMM = v
					case "height":
						facts.HeightMM = v
					case "viewBox":
						if m := cadSVGViewBoxRe.FindStringSubmatch(a.Value); m != nil {
							facts.ViewBoxW, _ = strconv.ParseFloat(m[1], 64)
							facts.ViewBoxH, _ = strconv.ParseFloat(m[2], 64)
						}
					}
				}
			}
			paint := cadSVGInitialPaint
			if len(paints) > 0 {
				paint = paints[len(paints)-1]
			}
			paint = cadSVGPaintOf(paint, t.Attr)
			if name == "defs" || name == "clipPath" {
				paint.hidden = true
			}
			for _, shape := range cadSVGShapes {
				if name == shape && cadSVGShapeVisible(name, t.Attr, paint) {
					facts.Visible++
				}
			}
			if name == "text" {
				text = &strings.Builder{}
				textInk = false
				textFont = []string{cadSVGFontOf(t.Attr)}
				textOpacity = paint.visible()
				textScale = 1
				for _, a := range t.Attr {
					if a.Name.Space == "" && a.Name.Local == "transform" {
						textScale = cadSVGMatrixScale(a.Value)
					}
				}
				textSize = math.Inf(1)
				if v, ok := cadSVGTextSize(t.Attr, textScale); ok {
					textSize = v
				}
			}
			if name == "tspan" {
				if v, ok := cadSVGTextSize(t.Attr, textScale); ok && v > textSize {
					textSize = v
				}
				textOpacity = math.Max(textOpacity, paint.visible())
				textFont = append(textFont, cadSVGFontOf(t.Attr))
			}
			bw.WriteByte('<')
			bw.WriteString(name)
			for _, a := range t.Attr {
				an, _ := cadSVGAttrName(a)
				bw.WriteByte(' ')
				bw.WriteString(an)
				bw.WriteString(`="`)
				cadSVGEscapeAttr(bw, a.Value)
				bw.WriteByte('"')
			}
			bw.WriteByte('>')
			stack = append(stack, name)
			paints = append(paints, paint)
		case xml.EndElement:
			name := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			paints = paints[:len(paints)-1]
			bw.WriteString("</")
			bw.WriteString(name)
			bw.WriteByte('>')
			if name == "text" {
				facts.Texts = append(facts.Texts, text.String())
				facts.TextSizes = append(facts.TextSizes, textSize)
				facts.TextOpacity = append(facts.TextOpacity, textOpacity)
				facts.TextFonts = append(facts.TextFonts, strings.Join(textFont, " | "))
				if textInk {
					facts.Visible++
				}
				text = nil
			}
			if len(stack) == 0 {
				rootDone = true
			}
		case xml.CharData:
			if len(stack) > 0 && cadSVGRules[stack[len(stack)-1]].text {
				text.Write(t)
				if p := paints[len(paints)-1]; !p.hidden && p.inked() && cadStripSpace(string(t)) != "" {
					textInk = true
				}
				if err := xml.EscapeText(bw, t); err != nil {
					return facts, fmt.Errorf("cad: write the validated SVG: %w", err)
				}
				continue
			}
			if !cadSVGOnlySpace(t) {
				if rootDone {
					return facts, cadSVGReject("character data after the root closed")
				}
				return facts, cadSVGReject("character data %s outside text", cadSVGQuote(string(t)))
			}
		default:
			return facts, cadSVGReject("unexpected XML token %T", tok)
		}
	}
	if !rootDone {
		return facts, cadSVGReject("no complete svg root element")
	}
	return facts, nil
}
