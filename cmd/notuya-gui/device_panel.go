package main

import (
	"context"
	"fmt"
	"math"
	"os"

	"github.com/averstraeten/notuya-go/pkg/device"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/cairo"
	coreglib "github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// panelWheelSize is the diameter of the per-device colour wheel in the desktop
// app. Smaller than the picker's full-screen wheel so several panels fit.
const panelWheelSize = 180

// devicePanel is the per-device control widget in the desktop app: a status
// line plus power, colour wheel, brightness, colour-temperature and scene
// controls, all wired to one control (its persistent session). All device I/O
// runs off the GTK thread; results are marshalled back with IdleAdd.
type devicePanel struct {
	ctl          *control
	wheelSurface *cairo.Surface

	root        *gtk.Box
	headerRow   *adw.ActionRow
	powerSwitch *gtk.Switch
	wheel       *gtk.DrawingArea
	swatch      *gtk.DrawingArea
	brightScale *gtk.Scale
	tempScale   *gtk.Scale

	// current selection, mirrored from the last refresh / drag
	selHue float64
	selSat float64

	// mode is the device's last-known work mode (colour/white), so the
	// brightness slider knows whether brightness is the colour's "v" (DP 24)
	// or the white brightness DP (DP 22).
	mode string

	// lastOn / hasState mirror the most recent refresh so the Rooms tab can
	// summarise a room without issuing its own device query. onRefresh, when
	// set, fires after applyStatus so dependent views (the Rooms tab) can
	// recompute; onToggle fires when the user flips the power switch so the
	// summary updates optimistically without waiting for a refresh.
	lastOn    bool
	hasState  bool
	onRefresh func()
	onToggle  func(on bool)

	// suppress guards the value-changed / state-set handlers while we
	// programmatically set widget values during a refresh, so reflecting
	// device state back into the UI doesn't fire a command back at the bulb.
	suppress bool
}

func newDevicePanel(ctl *control, wheel *cairo.Surface) *devicePanel {
	return &devicePanel{ctl: ctl, wheelSurface: wheel}
}

// build constructs the widget tree and returns the root card.
func (p *devicePanel) build() *gtk.Box {
	box := gtk.NewBox(gtk.OrientationVertical, 8)
	box.SetMarginTop(6)
	box.SetMarginBottom(10)
	box.SetMarginStart(10)
	box.SetMarginEnd(10)

	// Header: name (title) + live state (subtitle) + power switch, as an
	// Adwaita ActionRow so it reads like a native GNOME preferences row.
	p.headerRow = adw.NewActionRow()
	p.headerRow.SetTitle(p.ctl.name())
	p.headerRow.SetSubtitle("…")
	p.powerSwitch = gtk.NewSwitch()
	p.powerSwitch.SetVAlign(gtk.AlignCenter)
	p.powerSwitch.ConnectStateSet(func(state bool) bool {
		if p.suppress {
			return false
		}
		p.lastOn = state
		p.hasState = true
		if p.onToggle != nil {
			p.onToggle(state)
		}
		p.runAsync(func(ctx context.Context) error { return p.ctl.SetPower(ctx, state) })
		return false
	})
	p.headerRow.AddSuffix(p.powerSwitch)
	p.headerRow.SetActivatableWidget(p.powerSwitch)
	box.Append(p.headerRow)

	// Colour wheel + swatch.
	wheelRow := gtk.NewBox(gtk.OrientationHorizontal, 10)
	p.wheel = gtk.NewDrawingArea()
	p.wheel.SetContentWidth(panelWheelSize)
	p.wheel.SetContentHeight(panelWheelSize)
	p.wheel.SetDrawFunc(p.drawWheel)
	wheelRow.Append(p.wheel)

	swatchBox := gtk.NewBox(gtk.OrientationVertical, 6)
	swatchBox.SetVAlign(gtk.AlignCenter)
	p.swatch = gtk.NewDrawingArea()
	p.swatch.SetContentWidth(60)
	p.swatch.SetContentHeight(60)
	p.swatch.SetDrawFunc(p.drawSwatch)
	swatchBox.Append(p.swatch)
	wheelRow.Append(swatchBox)
	box.Append(wheelRow)

	// A single drag gesture handles both taps and drags: a tap is a
	// zero-distance drag whose begin→end still commits the final colour, so
	// no separate click gesture is needed (which would race the drag on the
	// one session).
	drag := gtk.NewGestureDrag()
	var startX, startY float64
	drag.ConnectDragBegin(func(x, y float64) {
		startX, startY = x, y
		p.setSelection(x, y)
		// Borrow music mode for the drag so it streams smoothly.
		p.ctl.BeginLiveDrag(p.selRGB())
	})
	drag.ConnectDragUpdate(func(ox, oy float64) {
		p.setSelection(startX+ox, startY+oy)
		p.ctl.UpdateLiveDrag(p.selRGB())
	})
	drag.ConnectDragEnd(func(ox, oy float64) {
		p.setSelection(startX+ox, startY+oy)
		p.ctl.UpdateLiveDrag(p.selRGB())
		// End the drag off the GTK thread: Close blocks while the streamer
		// leaves music mode with a final SetColour, making the colour stick.
		// Optimistic UI: the wheel already shows the chosen colour, so no
		// re-read follows (it could otherwise overwrite the selection).
		go p.ctl.EndLiveDrag()
	})
	p.wheel.AddController(drag)

	// Brightness slider. In colour mode brightness is the "v" of the colour,
	// so we rewrite the current selection's colour with the new value in a
	// single write; in white mode it is the dedicated brightness DP.
	box.Append(labelledScale("Brightness", &p.brightScale, 1, 100, float64(1), func(v float64) {
		if p.mode == device.ModeColour {
			r, g, b := hsvToRGBInt(p.selHue, p.selSat, v/100.0)
			p.runAsync(func(ctx context.Context) error {
				return p.ctl.SetColour(ctx, device.RGB{R: r, G: g, B: b})
			})
			return
		}
		p.runAsync(func(ctx context.Context) error { return p.ctl.SetWhiteBrightness(ctx, v) })
	}, p))

	// Colour-temperature slider (cold ↔ warm percentage).
	box.Append(labelledScale("Temp (cold→warm)", &p.tempScale, 0, 100, float64(1), func(v float64) {
		p.runAsync(func(ctx context.Context) error { return p.ctl.SetColourTempPercent(ctx, v) })
	}, p))

	// Scene buttons.
	sceneRow := gtk.NewBox(gtk.OrientationHorizontal, 6)
	sceneLabel := gtk.NewLabel("Scenes")
	sceneLabel.SetXAlign(0.0)
	sceneRow.Append(sceneLabel)
	for i := 1; i <= 4; i++ {
		n := i
		btn := gtk.NewButtonWithLabel(fmt.Sprintf("%d", n))
		btn.ConnectClicked(func() {
			p.runAsync(func(ctx context.Context) error { return p.ctl.SetScene(ctx, n) })
		})
		sceneRow.Append(btn)
	}
	refresh := gtk.NewButtonWithLabel("Refresh")
	refresh.SetHExpand(true)
	refresh.SetHAlign(gtk.AlignEnd)
	refresh.ConnectClicked(func() { p.refresh() })
	sceneRow.Append(refresh)
	box.Append(sceneRow)

	p.root = box
	p.root.AddCSSClass("card")
	return p.root
}

// labelledScale builds a "label + horizontal scale" row, stores the scale in
// *dst, and calls onChange with the new value when the user (not a refresh)
// moves it.
func labelledScale(label string, dst **gtk.Scale, min, max, step float64, onChange func(float64), p *devicePanel) *gtk.Box {
	row := gtk.NewBox(gtk.OrientationHorizontal, 8)
	lbl := gtk.NewLabel(label)
	lbl.SetWidthChars(16)
	lbl.SetXAlign(0.0)
	row.Append(lbl)
	scale := gtk.NewScaleWithRange(gtk.OrientationHorizontal, min, max, step)
	scale.SetHExpand(true)
	scale.SetDrawValue(true)
	scale.SetRoundDigits(0)
	scale.ConnectValueChanged(func() {
		if p.suppress {
			return
		}
		onChange(scale.Value())
	})
	row.Append(scale)
	*dst = scale
	return row
}

// runAsync runs a device command off the GTK thread and reports failures to
// the status label via IdleAdd.
//
// The UI is optimistic: the widget already shows the user's value, so a
// successful command does NOT re-read the device. Re-reading would let the
// bulb's (possibly stale, mid-command) state overwrite the control the user is
// still touching — the source of the slider "jumping back" on its own. State
// is only refreshed explicitly (window open, the "Refresh" button).
func (p *devicePanel) runAsync(fn func(ctx context.Context) error) {
	go func() {
		if err := fn(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "notuya-gui: %s -> %v\n", p.ctl.name(), err)
			coreglib.IdleAdd(func() { p.headerRow.SetSubtitle("Error: " + err.Error()) })
		}
	}()
}

// refresh queries the device off-thread and updates the widgets on the GTK
// thread.
func (p *devicePanel) refresh() {
	go func() {
		st, err := p.ctl.Refresh(context.Background())
		coreglib.IdleAdd(func() {
			if err != nil {
				p.headerRow.SetSubtitle("Offline: " + err.Error())
				return
			}
			p.applyStatus(st)
		})
	}()
}

// applyStatus reflects a fresh deviceStatus into the widgets without firing
// their change handlers back at the bulb.
func (p *devicePanel) applyStatus(st deviceStatus) {
	p.suppress = true
	defer func() { p.suppress = false }()

	p.powerSwitch.SetActive(st.On)
	p.brightScale.SetValue(math.Round(st.BrightPct))
	if st.HasTemp {
		p.tempScale.SetValue(math.Round(st.TempPct))
	}
	if st.HasColour {
		p.selHue = st.Hue
		p.selSat = st.Sat
		p.wheel.QueueDraw()
		p.swatch.QueueDraw()
	}

	state := "on"
	if !st.On {
		state = "off"
	}
	p.mode = st.Mode
	p.lastOn = st.On
	p.hasState = true
	mode := st.Mode
	if mode == "" {
		mode = "?"
	}
	p.headerRow.SetSubtitle(fmt.Sprintf("%s · %s · %d%%", state, mode, int(math.Round(st.BrightPct))))

	if p.onRefresh != nil {
		p.onRefresh()
	}
}

// setPowerOptimistic reflects a power change made elsewhere (a room master
// switch) into this panel's switch and cached state, without issuing a command
// or firing the switch handler. The actual command is sent by the caller.
func (p *devicePanel) setPowerOptimistic(on bool) {
	p.lastOn = on
	p.hasState = true
	if p.powerSwitch != nil {
		p.suppress = true
		p.powerSwitch.SetActive(on)
		p.suppress = false
	}
}

// setSelection updates the selected hue/sat from wheel coordinates and
// redraws, without touching the device.
func (p *devicePanel) setSelection(x, y float64) {
	p.selHue, p.selSat = coordsToHSSized(x, y, panelWheelSize)
	p.wheel.QueueDraw()
	p.swatch.QueueDraw()
}

// selRGB is the current selection as full-value RGB.
func (p *devicePanel) selRGB() device.RGB {
	r, g, b := hsvToRGBInt(p.selHue, p.selSat, 1.0)
	return device.RGB{R: r, G: g, B: b}
}

func (p *devicePanel) drawWheel(_ *gtk.DrawingArea, cr *cairo.Context, width, height int) {
	// The shared surface is rendered at wheelSize; scale it down to fit the
	// panel's smaller wheel.
	scale := float64(panelWheelSize) / float64(wheelSize)
	cr.Save()
	cr.Scale(scale, scale)
	cr.SetSourceSurface(p.wheelSurface, 0, 0)
	cr.Paint()
	cr.Restore()

	radius := float64(panelWheelSize) / 2.0
	angle := p.selHue * 2 * math.Pi
	dist := p.selSat * radius
	sx := radius + dist*math.Cos(angle)
	sy := radius + dist*math.Sin(angle)

	cr.Arc(sx, sy, 7, 0, 2*math.Pi)
	cr.SetSourceRGBA(0, 0, 0, 0.7)
	cr.SetLineWidth(2.5)
	cr.Stroke()
	cr.Arc(sx, sy, 5, 0, 2*math.Pi)
	cr.SetSourceRGBA(1, 1, 1, 0.95)
	cr.SetLineWidth(2)
	cr.Stroke()
}

func (p *devicePanel) drawSwatch(_ *gtk.DrawingArea, cr *cairo.Context, width, height int) {
	r, g, b := hsvToRGBInt(p.selHue, p.selSat, 1.0)
	w, h := float64(width), float64(height)
	roundedRect(cr, 0, 0, w, h, 8)
	cr.SetSourceRGB(float64(r)/255, float64(g)/255, float64(b)/255)
	cr.Fill()
	roundedRect(cr, 0.5, 0.5, w-1, h-1, 8)
	cr.SetSourceRGBA(0, 0, 0, 0.15)
	cr.SetLineWidth(1)
	cr.Stroke()
}

// coordsToHSSized is coordsToHS parameterised by the wheel diameter, so both
// the full-screen picker and the smaller panel wheel can share the mapping.
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
