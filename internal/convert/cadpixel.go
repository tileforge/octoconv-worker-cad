package convert

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
)

const (
	cadPixelMaxBytes          = 96 << 20
	cadBackgroundCutoff       = 250
	cadDarkCutoff             = 80
	cadSurfaceMinContrast     = 20 // 8-bit luma difference from the white background
	cadBlobFracMin            = 0.02
	cadBlobFracMax            = 0.90
	cadBlobSurfaceMin         = 0.001
	cadThinAspectMax          = 0.15
	cadThinSurfacePerLongSide = 0.25
	cadExtentMin              = 0.50
	cadMarginMin              = 0.03
	cadDrawingSpanMin         = 0.5
	cadLetterboxSlack         = 2

	CADDXFInkSize       = CADMaxRenderSize
	CADInkChannelCutoff = cadBackgroundCutoff
)

type CADPixelFacts struct {
	Width, Height      int
	Count, DarkCount   int
	BoxW, BoxH         int
	BoxX, BoxY         int
	Margin             int
	Frac, Dark         float64
	FracTransparent    float64
	RingNonTransparent int
	HasAlpha           bool
	Mode               string
}

func cadPNGHasAlpha(data []byte) bool {
	if len(data) < 33 {
		return false
	}
	if colorType := data[25]; colorType == 4 || colorType == 6 {
		return true
	}
	off := 8
	for off+8 <= len(data) {
		n := int(binary.BigEndian.Uint32(data[off : off+4]))
		kind := string(data[off+4 : off+8])
		if kind == "tRNS" {
			return true
		}
		if kind == "IDAT" || kind == "IEND" || n < 0 || off+12+n > len(data) {
			return false
		}
		off += 12 + n
	}
	return false
}

func JudgeCADPNG(r io.Reader, size int, background string) (CADPixelFacts, error) {
	var facts CADPixelFacts
	facts.Mode = "none"
	img, err := cadReadFrame(r, size, background, &facts)
	if err != nil {
		return facts, err
	}
	profile := cadProfile{rows: make([]cadScanline, size), visibleColumns: make([]bool, size)}
	_, surfacePx := cadMeasure(img, &facts, &profile)
	return facts, cadJudgeFacts(&facts, size, background, surfacePx, &profile)
}

func cadReadFrame(r io.Reader, size int, background string, facts *CADPixelFacts) (image.Image, error) {
	if size < CADMinRenderSize || size > CADMaxRenderSize {
		return nil, fmt.Errorf("cad: judge size %d is outside %d..%d", size, CADMinRenderSize, CADMaxRenderSize)
	}
	if background != CADBackgroundOpq && background != CADBackgroundTr {
		return nil, fmt.Errorf("cad: judge background %q is not opaque or transparent", background)
	}
	data, err := io.ReadAll(io.LimitReader(r, cadPixelMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("cad: read the rendered image: %w", err)
	}
	if len(data) > cadPixelMaxBytes {
		return nil, fmt.Errorf("%w: the image exceeds %d bytes", ErrCADRenderRejected, cadPixelMaxBytes)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: not a decodable PNG: %v", ErrCADRenderRejected, err)
	}
	facts.Width, facts.Height = cfg.Width, cfg.Height
	if cfg.Width != size || cfg.Height != size {
		return nil, fmt.Errorf("%w: J0 frame is %dx%d, want %dx%d", ErrCADRenderRejected, cfg.Width, cfg.Height, size, size)
	}
	facts.HasAlpha = cadPNGHasAlpha(data)
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: not a decodable PNG: %v", ErrCADRenderRejected, err)
	}
	return img, nil
}

type CADLetterbox struct{ Left, Top, Width, Height int }

func CADDrawingLetterbox(wMM, hMM float64, size int) (CADLetterbox, error) {
	if size < CADMinRenderSize || size > CADMaxRenderSize {
		return CADLetterbox{}, fmt.Errorf("cad: letterbox size %d is outside %d..%d", size, CADMinRenderSize, CADMaxRenderSize)
	}
	if !(wMM > 0) || !(hMM > 0) || math.IsInf(wMM, 0) || math.IsInf(hMM, 0) {
		return CADLetterbox{}, fmt.Errorf("cad: letterbox of a %v x %v mm drawing", wMM, hMM)
	}
	k := float64(size) * (1 - 2*cadMarginMin) / math.Max(wMM, hMM)
	sw := max(1, int(math.Floor(wMM*k+0.5)))
	sh := max(1, int(math.Floor(hMM*k+0.5)))
	return CADLetterbox{Left: (size - sw) / 2, Top: (size - sh) / 2, Width: sw, Height: sh}, nil
}

func JudgeCADDrawingPNG(r io.Reader, size int, background string, box CADLetterbox) (CADPixelFacts, error) {
	var facts CADPixelFacts
	facts.Mode = "drawing"
	if box.Width < 1 || box.Height < 1 || box.Left < 0 || box.Top < 0 || box.Left+box.Width > size || box.Top+box.Height > size {
		return facts, fmt.Errorf("cad: letterbox %+v does not fit a %d px frame", box, size)
	}
	img, err := cadReadFrame(r, size, background, &facts)
	if err != nil {
		return facts, err
	}
	cadMeasure(img, &facts, nil)
	if !cadDrawingInk(&facts) {
		return facts, fmt.Errorf("%w: J1 the frame holds no ink", ErrCADRenderRejected)
	}
	long, boxLong := max(facts.BoxW, facts.BoxH), max(box.Width, box.Height)
	if float64(long) < cadDrawingSpanMin*float64(boxLong) {
		return facts, fmt.Errorf("%w: J2 the ink spans %d px of a %d px letterbox", ErrCADRenderRejected, long, boxLong)
	}
	if need := int(cadMarginMin * float64(size)); facts.Margin < need {
		return facts, fmt.Errorf("%w: J3 the margin is %d px, want at least %d", ErrCADRenderRejected, facts.Margin, need)
	}
	x1, y1 := facts.BoxX+facts.BoxW-1, facts.BoxY+facts.BoxH-1
	if facts.BoxX < box.Left-cadLetterboxSlack || facts.BoxY < box.Top-cadLetterboxSlack ||
		x1 > box.Left+box.Width-1+cadLetterboxSlack || y1 > box.Top+box.Height-1+cadLetterboxSlack {
		return facts, fmt.Errorf("%w: J3 the ink box %d,%d..%d,%d leaves the letterbox %d,%d..%d,%d by more than %d px", ErrCADRenderRejected,
			facts.BoxX, facts.BoxY, x1, y1, box.Left, box.Top, box.Left+box.Width-1, box.Top+box.Height-1, cadLetterboxSlack)
	}
	return facts, cadJudgeAlpha(&facts, background)
}

func cadDrawingInk(f *CADPixelFacts) bool { return f.Count > 0 }

func JudgeCADDrawingInk(r io.Reader, size int) (CADPixelFacts, error) {
	var facts CADPixelFacts
	facts.Mode = "ink"
	img, err := cadReadFrame(r, size, CADBackgroundOpq, &facts)
	if err != nil {
		return facts, fmt.Errorf("%w: the ink check frame: %v", ErrCADDrawingFailed, err)
	}
	if opaquePx, _ := cadMeasure(img, &facts, nil); opaquePx != size*size {
		return facts, fmt.Errorf("%w: the ink check frame has %d of %d pixels not opaque, want a white page", ErrCADDrawingFailed, size*size-opaquePx, size*size)
	}
	if !cadDrawingInk(&facts) {
		return facts, fmt.Errorf("%w: %w: J1 the %d px frame holds no ink on a white page", ErrCADNothingDrawable, ErrCADEmptyResult, size)
	}
	return facts, nil
}

func cadOnWhite(c, a uint8) uint8 {
	return uint8((int(c)*int(a) + 255*(255-int(a)) + 127) / 255)
}

type cadScanline struct {
	left, right int // half-open bounds of foreground pixels
}

type cadProfile struct {
	rows           []cadScanline
	visibleColumns []bool
	visibleRows    int
}

func cadMeasure(img image.Image, f *CADPixelFacts, profile *cadProfile) (opaquePx, surfacePx int) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	x0, y0, x1, y1 := w, h, -1, -1
	transparentPx, ring := 0, 0
	nrgba, _ := img.(*image.NRGBA)
	var rows, white []byte
	var stride, base int
	switch p := img.(type) {
	case *image.NRGBA:
		rows, stride, base = p.Pix, p.Stride, p.PixOffset(b.Min.X, b.Min.Y)
	case *image.RGBA:
		rows, stride, base = p.Pix, p.Stride, p.PixOffset(b.Min.X, b.Min.Y)
	}
	if rows != nil {
		white = bytes.Repeat([]byte{0xff}, 4*w)
	}
	for y := 0; y < h; y++ {
		edgeRow := y == 0 || y == h-1
		if white != nil {
			if o := base + y*stride; bytes.Equal(rows[o:o+4*w], white) {
				opaquePx += w
				if edgeRow {
					ring += w
				} else {
					ring += min(w, 2)
				}
				continue
			}
		}
		left, right, previousSurface := w, 0, surfacePx
		for x := 0; x < w; x++ {
			var r, g, bl, a uint8
			if nrgba != nil {
				i := nrgba.PixOffset(b.Min.X+x, b.Min.Y+y)
				r, g, bl, a = nrgba.Pix[i], nrgba.Pix[i+1], nrgba.Pix[i+2], nrgba.Pix[i+3]
			} else {
				c := color.NRGBAModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
				r, g, bl, a = c.R, c.G, c.B, c.A
			}
			if a == 0 {
				transparentPx++
				continue
			}
			if edgeRow || x == 0 || x == w-1 {
				ring++
			}
			if a == 255 {
				opaquePx++
			} else {
				r, g, bl = cadOnWhite(r, a), cadOnWhite(g, a), cadOnWhite(bl, a)
			}
			if r >= cadBackgroundCutoff && g >= cadBackgroundCutoff && bl >= cadBackgroundCutoff {
				continue
			}
			f.Count++
			if luma, _, _ := color.RGBToYCbCr(r, g, bl); 255-luma >= cadSurfaceMinContrast {
				surfacePx++
				if profile != nil {
					profile.visibleColumns[x] = true
				}
			}
			if r < cadDarkCutoff && g < cadDarkCutoff && bl < cadDarkCutoff {
				f.DarkCount++
			}
			left, right = min(left, x), x+1
		}
		if right > left {
			x0, x1 = min(x0, left), max(x1, right-1)
			y0, y1 = min(y0, y), y
			if profile != nil {
				profile.rows[y] = cadScanline{left, right}
				if surfacePx > previousSurface {
					profile.visibleRows++
				}
			}
		}
	}
	area := float64(w * h)
	f.Frac = float64(f.Count) / area
	f.Dark = float64(f.DarkCount) / area
	f.FracTransparent = float64(transparentPx) / area
	f.RingNonTransparent = ring
	f.Margin = -1
	if f.Count > 0 {
		f.BoxW, f.BoxH = x1-x0+1, y1-y0+1
		f.BoxX, f.BoxY = x0, y0
		f.Margin = min(x0, y0, w-1-x1, h-1-y1)
	}
	return opaquePx, surfacePx
}

func cadJudgeAlpha(f *CADPixelFacts, background string) error {
	if background == CADBackgroundOpq {
		if f.FracTransparent > 0 {
			return fmt.Errorf("%w: background opaque but %.4f of the frame is transparent", ErrCADRenderRejected, f.FracTransparent)
		}
		return nil
	}
	if !f.HasAlpha {
		return fmt.Errorf("%w: background transparent but the PNG carries no alpha", ErrCADRenderRejected)
	}
	if f.RingNonTransparent > 0 {
		return fmt.Errorf("%w: background transparent but %d pixels on the frame's edge are not fully transparent", ErrCADRenderRejected, f.RingNonTransparent)
	}
	return nil
}

func (p *cadProfile) isDiagonal(f *CADPixelFacts, surfacePx int, minLength float64) bool {
	rows := p.rows[f.BoxY : f.BoxY+f.BoxH]
	// The first and last occupied row centers give the direction of an edge-on sheet.
	first, last := rows[0], rows[len(rows)-1]
	dx := float64(last.left+last.right-first.left-first.right) / 2
	dy := float64(len(rows) - 1)
	distance := math.Hypot(dx, dy)
	length := distance + 1
	if length < minLength || float64(surfacePx) < cadThinSurfacePerLongSide*length {
		return false
	}
	// Two dense endpoints can supply enough pixels while leaving the profile empty in between.
	visibleColumns := 0
	for _, visible := range p.visibleColumns[f.BoxX : f.BoxX+f.BoxW] {
		if visible {
			visibleColumns++
		}
	}
	if float64(p.visibleRows) < cadThinSurfacePerLongSide*float64(f.BoxH) ||
		float64(visibleColumns) < cadThinSurfacePerLongSide*float64(f.BoxW) {
		return false
	}
	// Project row endpoints onto the perpendicular to measure the full strip width.
	low, high := math.Inf(1), math.Inf(-1)
	for y, row := range rows {
		if row.right <= row.left {
			continue
		}
		low = min(low, dy*float64(row.left)-dx*float64(y))
		high = max(high, dy*float64(row.right-1)-dx*float64(y))
	}
	width := (high-low)/distance + 1
	return width < cadThinAspectMax*length
}

func cadJudgeFacts(f *CADPixelFacts, size int, background string, surfacePx int, profile *cadProfile) error {
	if f.Count == 0 {
		return fmt.Errorf("%w: J1 the frame holds no part pixels", ErrCADRenderRejected)
	}
	long, short := max(f.BoxW, f.BoxH), min(f.BoxW, f.BoxH)
	aspect := float64(short) / float64(long)
	minLength := cadExtentMin * float64(size)
	diagonal := f.Frac < cadBlobFracMin && aspect >= cadThinAspectMax &&
		float64(surfacePx) >= cadThinSurfacePerLongSide*float64(long) &&
		profile.isDiagonal(f, surfacePx, minLength)
	if float64(long) < minLength && !diagonal {
		return fmt.Errorf("%w: J2 the long side is %d px of a %d px frame", ErrCADRenderRejected, long, size)
	}
	if need := int(cadMarginMin * float64(size)); f.Margin < need {
		return fmt.Errorf("%w: J3 the margin is %d px, want at least %d", ErrCADRenderRejected, f.Margin, need)
	}
	surfaceFrac := float64(surfacePx) / float64(size*size)
	switch {
	case f.Frac >= cadBlobFracMin && f.Frac <= cadBlobFracMax && surfaceFrac >= cadBlobSurfaceMin:
		if f.Count < long {
			return fmt.Errorf("%w: J1 %d part pixels cannot span a %d px extent", ErrCADRenderRejected, f.Count, long)
		}
		f.Mode = "blob"
	case diagonal || (aspect < cadThinAspectMax && float64(surfacePx) >= cadThinSurfacePerLongSide*float64(long)):
		f.Mode = "thin"
	default:
		return fmt.Errorf("%w: J4 neither a blob (frac %.4f, visible surface %.4f) nor a thin part (aspect %.4f, %d visible surface pixels)", ErrCADRenderRejected, f.Frac, surfaceFrac, aspect, surfacePx)
	}
	return cadJudgeAlpha(f, background)
}
