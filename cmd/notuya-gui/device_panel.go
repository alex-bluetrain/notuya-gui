package main

import (
	"context"
	"fmt"
	"math"
	"os"

	coreglib "github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// devicePanel is a headless per-device status cache. It no longer owns any
// widgets: the Lights tab is now the shared "playground" (playground.go), and
// the Rooms tab only needs each device's last-known on/off state to build its
// summary rows. devicePanel wraps one control (its persistent session) and
// caches the result of the most recent Refresh. All device I/O runs off the
// GTK thread; results are marshalled back with IdleAdd.
type devicePanel struct {
	ctl *control

	// lastOn / hasState mirror the most recent refresh so the Rooms tab can
	// summarise a room without issuing its own device query. onRefresh, when
	// set, fires after a refresh so the Rooms tab can recompute; onToggle
	// fires when a room master switch flips this device so the summary updates
	// optimistically without waiting for a refresh.
	lastOn    bool
	hasState  bool
	onRefresh func()
	onToggle  func(on bool)
}

func newDevicePanel(ctl *control) *devicePanel {
	return &devicePanel{ctl: ctl}
}

// refresh queries the device off-thread and caches its on/off state, then
// notifies onRefresh on the GTK thread so the Rooms summary recomputes.
func (p *devicePanel) refresh() {
	go func() {
		st, err := p.ctl.Refresh(context.Background())
		coreglib.IdleAdd(func() {
			if err != nil {
				fmt.Fprintf(os.Stderr, "notuya-gui: %s -> %v\n", p.ctl.name(), err)
				return
			}
			p.lastOn = st.On
			p.hasState = true
			if p.onRefresh != nil {
				p.onRefresh()
			}
		})
	}()
}

// setPowerOptimistic reflects a power change made elsewhere (a room master
// switch) into this panel's cached state, without issuing a command. The
// actual command is sent by the caller.
func (p *devicePanel) setPowerOptimistic(on bool) {
	p.lastOn = on
	p.hasState = true
}

// disableScaleScroll prevents a GtkScale from capturing mouse-wheel events so
// the parent ScrolledWindow scrolls normally when the cursor passes over a
// slider. A capture-phase controller intercepts the scroll before the Scale's
// own handler and marks it handled, letting the ScrolledWindow keep scrolling.
func disableScaleScroll(scale *gtk.Scale) {
	ec := gtk.NewEventControllerScroll(gtk.EventControllerScrollVertical)
	ec.SetPropagationPhase(gtk.PhaseCapture)
	ec.ConnectScroll(func(dx, dy float64) bool { return true })
	scale.AddController(ec)
}

// coordsToHSSized maps a point in a colour wheel of the given diameter to
// (hue, saturation), so the full-screen picker, the per-scene editor wheel and
// the Lights-tab playground wheel all share one mapping.
func coordsToHSSized(x, y, size float64) (h, s float64) {
	radius := size / 2.0
	dx := x - radius
	dy := y - radius
	dist := math.Sqrt(dx*dx + dy*dy)
	if dist > radius {
		dist = radius
	}
	h = math.Mod(math.Atan2(dy, dx)/(2*math.Pi), 1.0)
	if h < 0 {
		h += 1.0
	}
	s = dist / radius
	return h, s
}
