package screensync

import (
	"context"
	"fmt"
	"os"
)

// Target is what a preset captures, independent of platform: one or more
// monitors (by connector name) forming one canvas, or one window.
type Target struct {
	Kind        string // TargetMonitors | TargetWindow
	Monitors    []string
	WindowClass string
	TitleMatch  string
}

const (
	TargetMonitors = "monitors"
	TargetWindow   = "window"
)

// WindowTokenKey is the restore-token key of a window target; monitor
// targets key their tokens by connector name.
const WindowTokenKey = "window"

// Capture is an open set of GPU frame sources for one target.
type Capture interface {
	Sources() []Source
	// Tokens are the restore tokens to persist for the next start.
	Tokens() map[string]string
	Close()
}

// CaptureOptions configure OpenCapture.
type CaptureOptions struct {
	// Tokens are restore tokens from a previous start (may be nil).
	Tokens map[string]string
	// OnPick runs before each capture request that may show the system
	// picker, with the monitor connector or WindowTokenKey it should select.
	OnPick func(key string)
}

// testBackend selects videotestsrc sources and a stub tracker, so the tab
// can be exercised where there is no Hyprland or portal (e.g. cage).
func testBackend() bool { return os.Getenv("NOTUYA_SYNC_BACKEND") == "test" }

// OpenCapture opens the platform capture for t.
func OpenCapture(ctx context.Context, t Target, opts CaptureOptions) (Capture, error) {
	if testBackend() {
		return openTestCapture(t)
	}
	switch t.Kind {
	case TargetMonitors:
		if len(t.Monitors) == 0 {
			return nil, fmt.Errorf("screensync: no monitors selected")
		}
	case TargetWindow:
	default:
		return nil, fmt.Errorf("screensync: unknown target kind %q", t.Kind)
	}
	return openPortalCapture(ctx, t, opts)
}

// canvasOf lays the named monitors out on one canvas (their bounding box
// in compositor layout) and returns each one's normalised rect, in order.
func canvasOf(names []string, all []Monitor) ([]Rect, error) {
	var mons []Monitor
	for _, n := range names {
		m, ok := monitorNamed(all, n)
		if !ok {
			return nil, fmt.Errorf("screensync: monitor %s not connected", n)
		}
		mons = append(mons, m)
	}
	x0, y0, x1, y1 := mons[0].X, mons[0].Y, mons[0].X+mons[0].W, mons[0].Y+mons[0].H
	for _, m := range mons[1:] {
		x0, y0 = min(x0, m.X), min(y0, m.Y)
		x1, y1 = max(x1, m.X+m.W), max(y1, m.Y+m.H)
	}
	w, h := float64(x1-x0), float64(y1-y0)
	out := make([]Rect, len(mons))
	for i, m := range mons {
		out[i] = Rect{float64(m.X-x0) / w, float64(m.Y-y0) / h, float64(m.W) / w, float64(m.H) / h}
	}
	return out, nil
}

func monitorNamed(all []Monitor, name string) (Monitor, bool) {
	for _, m := range all {
		if m.Name == name {
			return m, true
		}
	}
	return Monitor{}, false
}

// testCapture serves solid test colours, one per monitor (or one for a
// window), so sync can run without a compositor.
type testCapture struct{ srcs []Source }

func openTestCapture(t Target) (Capture, error) {
	colours := []uint32{0xff0000, 0x00ff00, 0x0000ff, 0xffff00}
	n := 1
	if t.Kind == TargetMonitors && len(t.Monitors) > 0 {
		n = len(t.Monitors)
	}
	c := &testCapture{}
	for i := range n {
		c.srcs = append(c.srcs, Source{
			Fragment: fmt.Sprintf("videotestsrc is-live=true pattern=solid-color foreground-color=0xff%06x"+
				" ! video/x-raw,format=RGBA,width=320,height=180,framerate=30/1 ! identity drop-allocation=true",
				colours[i%len(colours)]),
			Canvas: Rect{float64(i) / float64(n), 0, 1 / float64(n), 1},
		})
	}
	return c, nil
}

func (c *testCapture) Sources() []Source         { return c.srcs }
func (c *testCapture) Tokens() map[string]string { return nil }
func (c *testCapture) Close()                    {}
