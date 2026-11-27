package convert

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"testing"
)

func TestJudgeCADPNGSurfaceVisibility(t *testing.T) {
	const size = 256
	face := image.Rect(26, 60, 230, 196)
	edgeOn := image.Rect(26, 127, 230, 128)
	silver := color.NRGBA{R: 225, G: 228, B: 231, A: 255}
	white := color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	clear := color.NRGBA{}
	for _, tc := range []struct {
		name       string
		background color.NRGBA
		requested  string
		part       image.Rectangle
		material   color.NRGBA
		wantMode   string // empty for a rejected render
	}{
		{"flat silver face", white, CADBackgroundOpq, face, silver, "blob"},
		{"flat silver face on transparent", clear, CADBackgroundTr, face, silver, "blob"},
		{"edge-on silver plate", white, CADBackgroundOpq, edgeOn, silver, "thin"},
		{"antialiased edge-on plate", clear, CADBackgroundTr, edgeOn, color.NRGBA{R: 166, G: 170, B: 174, A: 128}, "thin"},
		{"washed out face", white, CADBackgroundOpq, face, color.NRGBA{R: 246, G: 246, B: 246, A: 255}, ""},
		{"nearly transparent dark plate", clear, CADBackgroundTr, edgeOn, color.NRGBA{A: 10}, ""},
		{"blank frame", white, CADBackgroundOpq, image.Rectangle{}, silver, ""},
		{"clipped face", white, CADBackgroundOpq, image.Rect(0, 60, 230, 196), silver, ""},
		{"tiny face", white, CADBackgroundOpq, image.Rect(125, 125, 131, 131), silver, ""},
		{"missing transparent background", white, CADBackgroundTr, face, silver, ""},
		{"missing opaque background", clear, CADBackgroundOpq, face, silver, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img := image.NewNRGBA(image.Rect(0, 0, size, size))
			draw.Draw(img, img.Bounds(), image.NewUniform(tc.background), image.Point{}, draw.Src)
			draw.Draw(img, tc.part, image.NewUniform(tc.material), image.Point{}, draw.Src)
			facts, err := judgeSurfaceFrame(t, img, tc.requested)
			if tc.wantMode == "" {
				if !errors.Is(err, ErrCADRenderRejected) {
					t.Fatalf("want ErrCADRenderRejected, got %v", err)
				}
				return
			}
			if err != nil || facts.Mode != tc.wantMode {
				t.Fatalf("want %s, got %s, err %v", tc.wantMode, facts.Mode, err)
			}
		})
	}
}

func TestJudgeCADPNGRejectsSparseThinPixels(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 256, 256))
	for x := 26; x < 230; x += 10 {
		img.SetNRGBA(x, 127, color.NRGBA{R: 140, G: 150, B: 160, A: 255})
	}
	if _, err := judgeSurfaceFrame(t, img, CADBackgroundTr); !errors.Is(err, ErrCADRenderRejected) {
		t.Fatalf("sparse row of dots, want ErrCADRenderRejected, got %v", err)
	}
}

func TestJudgeCADPNGDiagonalProfile(t *testing.T) {
	const size = 256
	metal := color.NRGBA{R: 200, G: 203, B: 205, A: 255}
	for _, tc := range []struct {
		name    string
		draw    func(*image.NRGBA, int)
		visible bool
	}{
		{"rising", func(img *image.NRGBA, x int) { img.SetNRGBA(x, 240-x*2/3, metal) }, true},
		{"falling", func(img *image.NRGBA, x int) { img.SetNRGBA(x, x, metal) }, true},
		{"half frame along the diagonal", func(img *image.NRGBA, x int) {
			if x >= 75 && x < 181 {
				img.SetNRGBA(x, x, metal)
			}
		}, true},
		{"isolated dots", func(img *image.NRGBA, x int) {
			if x%10 == 0 {
				img.SetNRGBA(x, x, metal)
			}
		}, false},
		{"only endpoints", func(img *image.NRGBA, x int) {
			if x < 37 || x > 219 {
				for y := x; y < x+8; y++ {
					img.SetNRGBA(x, y, metal)
				}
			}
		}, false},
		{"only endpoints at a shallow angle", func(img *image.NRGBA, x int) {
			if x < 37 || x > 219 {
				for dy := 0; dy < 8; dy++ {
					img.SetNRGBA(x, 100+x/5+dy, metal)
				}
			}
		}, false},
		{"cross", func(img *image.NRGBA, x int) {
			img.SetNRGBA(x, x, metal)
			img.SetNRGBA(x, size-x, metal)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img := image.NewNRGBA(image.Rect(0, 0, size, size))
			for x := 26; x < 230; x++ {
				tc.draw(img, x)
			}
			facts, err := judgeSurfaceFrame(t, img, CADBackgroundTr)
			if tc.visible {
				if err != nil || facts.Mode != "thin" {
					t.Fatalf("want thin, got %s, err %v", facts.Mode, err)
				}
			} else if !errors.Is(err, ErrCADRenderRejected) {
				t.Fatalf("want ErrCADRenderRejected, got %v", err)
			}
		})
	}
}

func judgeSurfaceFrame(t *testing.T, img image.Image, background string) (CADPixelFacts, error) {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return JudgeCADPNG(&buf, img.Bounds().Dx(), background)
}
