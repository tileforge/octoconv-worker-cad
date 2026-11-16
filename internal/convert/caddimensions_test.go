package convert

import (
	"image"
	"math"
	"testing"

	"github.com/tileforge/octoconv-worker-cad/internal/measurements"
)

func nearPoint(t *testing.T, got, want cadPoint) {
	t.Helper()
	for axis := range got {
		if math.Abs(got[axis]-want[axis]) > 1e-9 {
			t.Fatalf("point %v, want %v", got, want)
		}
	}
}

func TestDimensionProjectionKeepsTheOverallCorners(t *testing.T) {
	bounds := stlBounds{Min: [3]float64{0, 0, 0}, Max: [3]float64{2, 2, 2}}
	points := cadDimensionProjection(bounds, "front", image.Rect(0, 0, 100, 100))
	nearPoint(t, points[0], cadPoint{6, 94})
	nearPoint(t, points[1], cadPoint{94, 94})
	nearPoint(t, points[4], cadPoint{6, 6})
}

func TestBodyDimensionsSitOnTheOverallCamera(t *testing.T) {
	mesh := stlBounds{Min: [3]float64{0, 0, 0}, Max: [3]float64{2, 2, 2}}
	frame := image.Rect(0, 0, 100, 100)
	overall := measurements.BBox{Min: []float64{0, 0, 0}, Max: []float64{2, 2, 2}}
	part := measurements.BBox{Min: []float64{1, 1, 1}, Max: []float64{2, 2, 2}}
	doc := measurements.Document{BBox: overall, Bodies: []measurements.Body{{BBox: part}}}
	lines := cadBodyDimensionLines(mesh, doc, "front", frame)
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want the two visible axes", len(lines))
	}
	for _, line := range lines {
		if line.Value != "1.0" {
			t.Fatalf("value %q", line.Value)
		}
	}
	overallPoints := cadDimensionProjection(mesh, "front", frame)
	bodyPoints := cadDimensionViewOf(mesh, "front", frame).project(stlBounds{
		Min: [3]float64{1, 1, 1}, Max: [3]float64{2, 2, 2},
	})
	nearPoint(t, bodyPoints[0], cadPoint{50, 50})
	nearPoint(t, bodyPoints[7], overallPoints[7])
}

func TestBodyDimensionsUseTheDisplayUnit(t *testing.T) {
	mesh := stlBounds{Min: [3]float64{0, 0, 0}, Max: [3]float64{50.8, 50.8, 50.8}}
	doc := measurements.Document{
		BBox: measurements.BBox{Min: []float64{0, 0, 0}, Max: []float64{2, 2, 2}},
		Bodies: []measurements.Body{{BBox: measurements.BBox{
			Min: []float64{0, 0, 0}, Max: []float64{1, 1, 1},
		}}},
	}
	frame := image.Rect(0, 0, 100, 100)
	lines := cadBodyDimensionLines(mesh, doc, "front", frame)
	if len(lines) != 2 || lines[0].Value != "1.0" || lines[1].Value != "1.0" {
		t.Fatalf("lines = %+v", lines)
	}
	points := cadDimensionViewOf(mesh, "front", frame).project(stlBounds{
		Min: [3]float64{0, 0, 0}, Max: [3]float64{25.4, 25.4, 25.4},
	})
	if !lineOnProjectedEdge(lines[0], points) || !lineOnProjectedEdge(lines[1], points) {
		t.Fatalf("lines are not on the scaled body: %+v", lines)
	}
}

func lineOnProjectedEdge(line cadDimensionLine, points [8]cadPoint) bool {
	for corner := range points {
		for axis := range 3 {
			if corner&(1<<axis) != 0 {
				continue
			}
			if pointNear(line.Start, points[corner]) && pointNear(line.End, points[corner|(1<<axis)]) {
				return true
			}
			if pointNear(line.End, points[corner]) && pointNear(line.Start, points[corner|(1<<axis)]) {
				return true
			}
		}
	}
	return false
}

func pointNear(a, b cadPoint) bool {
	return math.Abs(a[0]-b[0]) < 1e-9 && math.Abs(a[1]-b[1]) < 1e-9
}

func TestBodyDimensionsSkipTheOverallBoxAndTheSmallest(t *testing.T) {
	overall := measurements.BBox{Min: []float64{0, 0, 0}, Max: []float64{100, 100, 100}}
	doc := measurements.Document{
		BBox:   overall,
		Bodies: []measurements.Body{{BBox: overall}},
	}
	mesh := stlBounds{Min: [3]float64{0, 0, 0}, Max: [3]float64{100, 100, 100}}
	frame := image.Rect(0, 0, 200, 200)
	if lines := cadBodyDimensionLines(mesh, doc, "iso", frame); len(lines) != 0 {
		t.Fatalf("overall body drew %d lines", len(lines))
	}
	doc.Bodies = nil
	for i := range cadMaxBodyDimensions + 1 {
		span := float64(cadMaxBodyDimensions - i + 1)
		doc.Bodies = append(doc.Bodies, measurements.Body{BBox: measurements.BBox{
			Min: []float64{0, 0, 0}, Max: []float64{span, span, span},
		}})
	}
	lines := cadBodyDimensionLines(mesh, doc, "iso", frame)
	seen := map[string]bool{}
	for _, line := range lines {
		seen[line.Value] = true
	}
	if seen["1.0"] {
		t.Fatalf("the smallest body was drawn: %v", seen)
	}
	if !seen[cadDimensionNumber(float64(cadMaxBodyDimensions+1))] {
		t.Fatalf("the largest body is missing: %v", seen)
	}
}

func TestBodyDimensionsFollowTheLongestAxisWhenTheMeshIsInset(t *testing.T) {
	mesh := stlBounds{Min: [3]float64{0, 0, 0}, Max: [3]float64{100, 100, 99.5}}
	doc := measurements.Document{
		BBox: measurements.BBox{Min: []float64{0, 0, 0}, Max: []float64{100, 100, 100}},
		Bodies: []measurements.Body{{BBox: measurements.BBox{
			Min: []float64{0, 0, 0}, Max: []float64{50, 50, 50},
		}}},
	}
	frame := image.Rect(0, 0, 100, 100)
	lines := cadBodyDimensionLines(mesh, doc, "front", frame)
	points := cadDimensionViewOf(mesh, "front", frame).project(stlBounds{
		Min: [3]float64{0, 0, 0}, Max: [3]float64{50, 50, 50},
	})
	if len(lines) != 2 || !lineOnProjectedEdge(lines[0], points) {
		t.Fatalf("inset mesh lines = %+v", lines)
	}
}
