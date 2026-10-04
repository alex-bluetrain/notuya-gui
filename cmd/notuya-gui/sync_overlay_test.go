package main

import (
	"math"
	"testing"

	"github.com/alex-bluetrain/notuya-gui/internal/screensync"
)

func rectNear(a, b [4]float64) bool {
	for i := range a {
		if math.Abs(a[i]-b[i]) > 1e-9 {
			return false
		}
	}
	return true
}

// A 2-monitor canvas starting at x=1920; drags are in layout px.
func testOverlay() *syncOverlay {
	o := newSyncOverlay()
	o.canvas = screensync.Monitor{X: 1920, W: 3840, H: 1080}
	return o
}

func TestOverlayCreateThenResize(t *testing.T) {
	o := testOverlay()
	var created [4]float64
	o.onCreate = func(r [4]float64) {
		created = r
		o.regions = append(o.regions, overlayRegion{Rect: r})
	}
	// Drag up-left from the middle: the anchor stays the bottom-right.
	o.ax, o.ay, o.mode = 1920+1920, 540, dragCreate
	o.dragUpdate(-10, -10) // too small: nothing yet
	if len(o.regions) != 0 {
		t.Fatal("region created below the minimum size")
	}
	o.dragUpdate(-384, -108)
	if len(o.regions) != 1 || !rectNear(created, [4]float64{0.4, 0.4, 0.1, 0.1}) {
		t.Fatalf("created %v", created)
	}
	o.dragEnd(-768, -216)
	if got := o.regions[0].Rect; !rectNear(got, [4]float64{0.3, 0.3, 0.2, 0.2}) {
		t.Fatalf("after resize %v", got)
	}
}

func TestOverlayMoveIsClampedToCanvas(t *testing.T) {
	o := testOverlay()
	o.regions = []overlayRegion{{Rect: [4]float64{0.1, 0.1, 0.2, 0.2}}}
	o.dragBegin(&overlayWindow{mon: screensync.Monitor{X: 1920, W: 1920, H: 1080}}, 0.15*3840, 0.15*1080)
	if o.mode != dragMove || o.selected != 0 {
		t.Fatalf("mode %d selected %d", o.mode, o.selected)
	}
	o.dragEnd(-10000, 10000)
	if got := o.regions[0].Rect; !rectNear(got, [4]float64{0, 0.8, 0.2, 0.2}) {
		t.Fatalf("moved to %v", got)
	}
}

func TestOverlayResizeFromCornerOnSecondMonitor(t *testing.T) {
	o := testOverlay()
	o.regions = []overlayRegion{{Rect: [4]float64{0.5, 0.5, 0.25, 0.25}}}
	// The second monitor starts at layout x=3840; the region's top-left
	// corner sits at its origin x=1920+1920 → local 0.
	o.dragBegin(&overlayWindow{mon: screensync.Monitor{X: 3840, W: 1920, H: 1080}}, 2, 540+2)
	if o.mode != dragResize || o.corner != 0 {
		t.Fatalf("mode %d corner %d", o.mode, o.corner)
	}
	o.dragEnd(-384, 0)
	if got := o.regions[0].Rect; !rectNear(got, [4]float64{0.4, 0.5, 0.35, 0.25}) {
		t.Fatalf("resized to %v", got)
	}
}
