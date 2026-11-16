package convert

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/tileforge/octoconv-worker-cad/internal/measurements"
	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/f64"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

type cadPoint [2]float64

func (p cadPoint) add(q cadPoint) cadPoint { return cadPoint{p[0] + q[0], p[1] + q[1]} }
func (p cadPoint) mul(k float64) cadPoint  { return cadPoint{p[0] * k, p[1] * k} }
func (p cadPoint) dot(q cadPoint) float64  { return p[0]*q[0] + p[1]*q[1] }

type cadDimensionLine struct {
	Start, End cadPoint
	Normal     cadPoint
	Value      string
}

const cadMaxBodyDimensions = 12

// Match the orthographic camera in cmd/cad-worker/helper/cad-render. Bounds
// position lines in mesh coordinates; the document supplies values in display units.
type cadDimensionView struct {
	right, up [3]float64
	origin    [3]float64
	scale     float64
	center    cadPoint
}

func cadDimensionViewOf(b stlBounds, view string, frame image.Rectangle) cadDimensionView {
	var right, up [3]float64
	switch view {
	case "front":
		right, up = [3]float64{1, 0, 0}, [3]float64{0, 0, 1}
	case "back":
		right, up = [3]float64{-1, 0, 0}, [3]float64{0, 0, 1}
	case "left":
		right, up = [3]float64{0, -1, 0}, [3]float64{0, 0, 1}
	case "right":
		right, up = [3]float64{0, 1, 0}, [3]float64{0, 0, 1}
	case "top":
		right, up = [3]float64{1, 0, 0}, [3]float64{0, 1, 0}
	case "bottom":
		right, up = [3]float64{-1, 0, 0}, [3]float64{0, 1, 0}
	default:
		right = [3]float64{1 / math.Sqrt2, 1 / math.Sqrt2, 0}
		up = [3]float64{-1 / math.Sqrt(6), 1 / math.Sqrt(6), 2 / math.Sqrt(6)}
	}
	origin := [3]float64{
		(b.Min[0] + b.Max[0]) / 2,
		(b.Min[1] + b.Max[1]) / 2,
		(b.Min[2] + b.Max[2]) / 2,
	}
	var half float64
	for corner := range 8 {
		p := cadCornerOffset(b, corner, right, up)
		half = max(half, math.Abs(p[0]), math.Abs(p[1]))
	}
	cam := cadDimensionView{right: right, up: up, origin: origin, center: cadPoint{
		float64(frame.Min.X+frame.Max.X) / 2,
		float64(frame.Min.Y+frame.Max.Y) / 2,
	}}
	if half > 0 {
		// The raw renderer leaves 6% on each side of its square frame.
		cam.scale = float64(frame.Dx()) * 0.88 / (2 * half)
	}
	return cam
}

func cadCornerOffset(b stlBounds, corner int, right, up [3]float64) cadPoint {
	var p cadPoint
	for axis := range 3 {
		d := (b.Max[axis] - b.Min[axis]) / 2
		if corner&(1<<axis) == 0 {
			d = -d
		}
		p[0] += d * right[axis]
		p[1] -= d * up[axis]
	}
	return p
}

func cadDimensionProjection(b stlBounds, view string, frame image.Rectangle) [8]cadPoint {
	return cadDimensionViewOf(b, view, frame).project(b)
}

func (cam cadDimensionView) project(b stlBounds) [8]cadPoint {
	var points [8]cadPoint
	for corner := range points {
		var p cadPoint
		for axis := range 3 {
			coord := b.Max[axis]
			if corner&(1<<axis) == 0 {
				coord = b.Min[axis]
			}
			d := coord - cam.origin[axis]
			p[0] += d * cam.right[axis]
			p[1] -= d * cam.up[axis]
		}
		points[corner] = cam.center.add(p.mul(cam.scale))
	}
	return points
}

func cadDimensionLines(points [8]cadPoint, box measurements.BBox) []cadDimensionLine {
	lines := make([]cadDimensionLine, 0, 3)
	for axis := range 3 {
		if axis >= len(box.Min) || axis >= len(box.Max) {
			continue
		}
		v := points[1<<axis].add(points[0].mul(-1))
		length := math.Hypot(v[0], v[1])
		if length < 1 {
			continue // A hidden or subpixel axis still appears in the heading.
		}
		normal := cadPoint{-v[1] / length, v[0] / length}
		if normal[1] < -1e-9 || (math.Abs(normal[1]) < 1e-9 && normal[0] < 0) {
			normal = normal.mul(-1)
		}
		best, score := 0, math.Inf(-1)
		for corner, p := range points {
			if corner&(1<<axis) != 0 {
				continue
			}
			if d := p.dot(normal); d > score {
				best, score = corner, d
			}
		}
		lines = append(lines, cadDimensionLine{
			Start: points[best], End: points[best|(1<<axis)], Normal: normal,
			Value: cadDimensionNumber(box.Max[axis] - box.Min[axis]),
		})
	}
	return lines
}

func cadBodyDimensionLines(mesh stlBounds, doc measurements.Document, view string, frame image.Rectangle, factor float64) []cadDimensionLine {
	if !(factor > 0) || math.IsNaN(factor) || math.IsInf(factor, 0) {
		return nil
	}
	ranked := make([]int, 0, len(doc.Bodies))
	for i, body := range doc.Bodies {
		if cadBoxNear(body.BBox, doc.BBox) || !cadBoxFinite(body.BBox) {
			continue
		}
		ranked = append(ranked, i)
	}
	slices.SortStableFunc(ranked, func(a, b int) int {
		da, db := cadBoxDiagonal(doc.Bodies[a].BBox), cadBoxDiagonal(doc.Bodies[b].BBox)
		if da > db {
			return -1
		}
		if da < db {
			return 1
		}
		return a - b
	})
	if len(ranked) > cadMaxBodyDimensions {
		ranked = ranked[:cadMaxBodyDimensions]
	}
	cam := cadDimensionViewOf(mesh, view, frame)
	lines := make([]cadDimensionLine, 0, len(ranked)*3)
	for _, i := range ranked {
		body, ok := cadBoxInMesh(doc.Bodies[i].BBox, factor)
		if !ok {
			continue
		}
		lines = append(lines, cadDimensionLines(cam.project(body), doc.Bodies[i].BBox)...)
	}
	return lines
}

func cadBoxInMesh(box measurements.BBox, scale float64) (stlBounds, bool) {
	if len(box.Min) != 3 || len(box.Max) != 3 {
		return stlBounds{}, false
	}
	var b stlBounds
	for axis := range 3 {
		b.Min[axis] = box.Min[axis] * scale
		b.Max[axis] = box.Max[axis] * scale
	}
	return b, true
}

func cadBoxFinite(box measurements.BBox) bool {
	if len(box.Min) != 3 || len(box.Max) != 3 {
		return false
	}
	for axis := range 3 {
		if math.IsNaN(box.Min[axis]) || math.IsInf(box.Min[axis], 0) || math.IsNaN(box.Max[axis]) || math.IsInf(box.Max[axis], 0) {
			return false
		}
	}
	return true
}

func cadBoxNear(a, b measurements.BBox) bool {
	if len(a.Min) != 3 || len(a.Max) != 3 || len(b.Min) != 3 || len(b.Max) != 3 {
		return false
	}
	for axis := range 3 {
		span := math.Max(math.Abs(a.Max[axis]-a.Min[axis]), math.Abs(b.Max[axis]-b.Min[axis]))
		if !cadNearBoxCoord(a.Min[axis], b.Min[axis], span) || !cadNearBoxCoord(a.Max[axis], b.Max[axis], span) {
			return false
		}
	}
	return true
}

func cadNearBoxCoord(a, b, span float64) bool {
	return math.Abs(a-b) <= 1e-6*math.Max(span, 1e-9)
}

func cadBoxDiagonal(box measurements.BBox) float64 {
	var sum float64
	for axis := range 3 {
		d := box.Max[axis] - box.Min[axis]
		sum += d * d
	}
	return math.Sqrt(sum)
}

func cadDimensionNumber(v float64) string {
	if v != 0 && (math.Abs(v) < 0.01 || math.Abs(v) >= 1e6) {
		return strconv.FormatFloat(v, 'g', 4, 64)
	}
	text := strconv.FormatFloat(v, 'f', 2, 64)
	if strings.HasSuffix(text, "0") {
		text = strings.TrimSuffix(text, "0")
	}
	return text
}

func cadDimensionTitle(doc measurements.Document) string {
	values := make([]string, 3)
	for i := range values {
		values[i] = cadDimensionNumber(doc.BBox.Max[i] - doc.BBox.Min[i])
	}
	unit := doc.Unit
	if unit == measurements.UnitUnspecified {
		unit = "(units unspecified)"
	}
	if doc.UnitSource == measurements.SourceClientAssumed {
		unit += " (assumed)"
	}
	return "Size: " + strings.Join(values, " × ") + " " + unit
}

func cadDimensionImage(
	src image.Image,
	bounds stlBounds,
	doc measurements.Document,
	opts CADOpts,
	factor float64,
) (image.Image, error) {
	size := *opts.Size
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	if opts.Background == CADBackgroundOpq {
		draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src)
	}
	frame := image.Rect(size*14/100, size*16/100, size*84/100, size*86/100)
	// Keep the inset square even when rounding at small output sizes.
	frame.Max.Y = frame.Min.Y + frame.Dx()
	draw.CatmullRom.Scale(dst, frame, src, src.Bounds(), draw.Over, nil)
	parsed, err := opentype.Parse(goregular.TTF)
	if err != nil {
		return nil, fmt.Errorf("parse dimension font: %w", err)
	}
	textSize := max(9.0, float64(size)*0.022)
	face, err := opentype.NewFace(parsed, &opentype.FaceOptions{Size: textSize, DPI: 72})
	if err != nil {
		return nil, fmt.Errorf("create dimension font: %w", err)
	}
	defer face.Close()
	ink := image.NewUniform(color.RGBA{R: 74, G: 87, B: 94, A: 255})
	pen := cadDimensionPen{image: dst, ink: ink, width: max(0.8, float64(size)/1100)}
	points := cadDimensionProjection(bounds, opts.View, frame)
	for _, line := range cadDimensionLines(points, doc.BBox) {
		pen.dimension(line, face, textSize)
	}
	for _, line := range cadBodyDimensionLines(bounds, doc, opts.View, frame, factor) {
		pen.dimension(line, face, textSize)
	}
	title := cadDimensionTitle(doc)
	titleSize := float64(size) * 0.038
	titleFace, err := opentype.NewFace(parsed, &opentype.FaceOptions{Size: titleSize, DPI: 72})
	if err != nil {
		return nil, fmt.Errorf("create heading font: %w", err)
	}
	defer titleFace.Close()
	label := cadDimensionText(title, titleFace, ink)
	pen.text(label, cadPoint{float64(size) / 2, float64(size) * 0.07}, 0)
	return dst, nil
}

type cadDimensionPen struct {
	image  *image.RGBA
	ink    *image.Uniform
	width  float64
	labels []image.Rectangle
}

func (p cadDimensionPen) polygon(points ...cadPoint) {
	lo, hi := points[0], points[0]
	for _, point := range points[1:] {
		for axis := range 2 {
			lo[axis], hi[axis] = min(lo[axis], point[axis]), max(hi[axis], point[axis])
		}
	}
	rect := image.Rect(int(math.Floor(lo[0]))-1, int(math.Floor(lo[1]))-1,
		int(math.Ceil(hi[0]))+1, int(math.Ceil(hi[1]))+1).Intersect(p.image.Bounds())
	if rect.Empty() {
		return
	}
	r := vector.NewRasterizer(rect.Dx(), rect.Dy())
	local := func(point cadPoint) (float32, float32) {
		return float32(point[0] - float64(rect.Min.X)), float32(point[1] - float64(rect.Min.Y))
	}
	r.MoveTo(local(points[0]))
	for _, point := range points[1:] {
		r.LineTo(local(point))
	}
	r.ClosePath()
	r.Draw(p.image, rect, p.ink, image.Point{})
}

func (p cadDimensionPen) line(a, b cadPoint) {
	v := b.add(a.mul(-1))
	length := math.Hypot(v[0], v[1])
	if length == 0 {
		return
	}
	n := cadPoint{-v[1], v[0]}.mul(p.width / (2 * length))
	p.polygon(a.add(n), b.add(n), b.add(n.mul(-1)), a.add(n.mul(-1)))
}

func (p *cadDimensionPen) dimension(line cadDimensionLine, face font.Face, textSize float64) {
	size := float64(p.image.Bounds().Dx())
	offset := line.Normal.mul(size * 0.045)
	a, b := line.Start.add(offset), line.End.add(offset)
	for _, end := range []cadPoint{line.Start, line.End} {
		p.line(end.add(line.Normal.mul(size*0.006)), end.add(line.Normal.mul(size*0.058)))
	}
	p.line(a, b)
	v := b.add(a.mul(-1))
	length := math.Hypot(v[0], v[1])
	tangent := v.mul(1 / length)
	arrow := max(3, size*0.008)
	label := cadDimensionText(line.Value, face, p.ink)
	outside := length < float64(label.Bounds().Dx())+4*arrow
	sign := 1.0
	if outside {
		sign = -1
		p.line(a.add(tangent.mul(-2*arrow)), b.add(tangent.mul(2*arrow)))
	}
	for i, end := range []cadPoint{a, b} {
		direction := tangent.mul(sign)
		if i == 1 {
			direction = direction.mul(-1)
		}
		base := end.add(direction.mul(arrow))
		n := line.Normal.mul(arrow * 0.3)
		p.polygon(end, base.add(n), base.add(n.mul(-1)))
	}
	angle := math.Atan2(v[1], v[0])
	if angle > math.Pi/2 {
		angle -= math.Pi
	}
	if angle < -math.Pi/2 {
		angle += math.Pi
	}
	center := a.add(b).mul(0.5).add(line.Normal.mul(textSize * 0.85))
	p.text(label, center, angle)
}

func cadDimensionText(text string, face font.Face, ink image.Image) *image.RGBA {
	bounds, _ := font.BoundString(face, text)
	rect := image.Rect(bounds.Min.X.Floor()-1, bounds.Min.Y.Floor()-1, bounds.Max.X.Ceil()+1, bounds.Max.Y.Ceil()+1)
	img := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
	d := font.Drawer{Dst: img, Src: ink, Face: face, Dot: fixed.P(-rect.Min.X, -rect.Min.Y)}
	d.DrawString(text)
	return img
}

func (p *cadDimensionPen) text(label *image.RGBA, center cadPoint, angle float64) {
	size := float64(p.image.Bounds().Dx())
	w, h := float64(label.Bounds().Dx()), float64(label.Bounds().Dy())
	c, s := math.Cos(angle), math.Sin(angle)
	halfW, halfH := (math.Abs(c)*w+math.Abs(s)*h)/2, (math.Abs(s)*w+math.Abs(c)*h)/2
	scale := min(1.0, size*0.90/(2*halfW), size*0.90/(2*halfH))
	halfW, halfH = halfW*scale, halfH*scale
	margin := size * 0.04
	normal, tangent := cadPoint{-s, c}, cadPoint{c, s}
	step := max(h, w/3)
	moves := []cadPoint{{0, 0}}
	for i := 1; i <= 8; i++ {
		moves = append(moves, normal.mul(float64(i)*step))
	}
	for i := 1; i <= 8; i++ {
		moves = append(moves, tangent.mul(float64(i)*step), tangent.mul(-float64(i)*step))
	}
	placed := false
	for _, move := range moves {
		candidate := center.add(move)
		candidate[0] = max(margin+halfW, min(size-margin-halfW, candidate[0]))
		candidate[1] = max(margin+halfH, min(size-margin-halfH, candidate[1]))
		rect := image.Rect(int(math.Floor(candidate[0]-halfW)), int(math.Floor(candidate[1]-halfH)),
			int(math.Ceil(candidate[0]+halfW)), int(math.Ceil(candidate[1]+halfH)))
		collides := false
		for _, occupied := range p.labels {
			if rect.Overlaps(occupied.Inset(-2)) {
				collides = true
				break
			}
		}
		if !collides {
			center = candidate
			p.labels = append(p.labels, rect)
			placed = true
			break
		}
	}
	if !placed {
		return // The heading retains the value if no clear label placement fits.
	}
	c, s = c*scale, s*scale
	transform := f64.Aff3{c, -s, center[0] - c*w/2 + s*h/2, s, c, center[1] - s*w/2 - c*h/2}
	draw.BiLinear.Transform(p.image, transform, label, label.Bounds(), draw.Over, nil)
}
