package main

import (
	"context"
	"fmt"
	"math"
	"os"

	"github.com/averstraeten/notuya-go/pkg/device"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/cairo"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	coreglib "github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// tempScaleCSS paints the temperature scale's trough with the warm→cool
// gradient and neutralises GTK's default trough styling so the ramp reads as
// one clean bar: the filled ("highlight") and unfilled halves share the same
// gradient, and the slider keeps a round white knob like the Brightness scale.
const tempScaleCSS = `
scale.temp-scale trough {
  background-image: linear-gradient(to right,
    rgb(255,166,70) 0%,
    rgb(255,246,235) 50%,
    rgb(158,202,255) 100%);
  background-color: transparent;
  border: none;
  min-height: 10px;
  border-radius: 6px;
}
scale.temp-scale highlight {
  background-color: transparent;
  background-image: none;
  border: none;
}
scale.temp-scale fill {
  background-color: transparent;
  background-image: none;
}
scale.temp-scale slider {
  background-color: #ffffff;
  border: 1px solid alpha(#000, 0.2);
  box-shadow: 0 1px 3px alpha(#000, 0.35);
  min-width: 18px;
  min-height: 18px;
  border-radius: 50%;
  margin: -6px;
}
`

// playgroundWheelSize is the diameter of the shared colour wheel in the Lights
// playground. Matches the scene editor's wheel for a consistent feel.
const playgroundWheelSize = 240

// playground is the Lights tab: a manual "playground" with a checkbox list of
// devices and a single set of controls (colour wheel, mode, brightness, temp)
// whose changes are broadcast to every checked light. It reuses the same
// control instances as the Rooms tab (via a.byID, mutex-serialized), driving
// the wheel through BeginLiveDrag/UpdateLiveDrag/EndLiveDrag and the sliders/
// mode/power through the discrete setters — exactly like the scene editor, so
// colour and white/temperature behave correctly.
type playground struct {
	app *desktopApp

	targets []*playgroundTarget

	modeToggle *adw.ToggleGroup
	wheelRow   *gtk.Box
	wheel     *gtk.DrawingArea
	swatch    *gtk.DrawingArea
	bright    *gtk.Scale
	brightPct *gtk.Label
	tempRow *gtk.Box
	temp    *gtk.Scale
	trans   *adw.ToggleGroup

	// current shared selection
	hue  float64
	sat  float64
	mode string

	// brightDragging is true while the brightness slider is being dragged in
	// colour mode with a live music stream open, so setBrightness streams the
	// colour (fades) instead of writing it discretely (snaps).
	brightDragging bool

	suppress bool
}

// playgroundTarget is one device's row in the target list. The checkbox selects
// whether the shared wheel/sliders drive this light; the swatch, brightness
// readout, and per-light power switch reflect and control this light's own live
// state (seeded from a Refresh on open), independent of the shared controls.
type playgroundTarget struct {
	ctl    *control
	check  *gtk.CheckButton
	swatch *gtk.DrawingArea
	bright *gtk.Label
	power  *gtk.Switch

	// col is the last known colour for this light's swatch; hasState is false
	// until the first refresh lands so the swatch can read as "unknown".
	col      device.RGB
	on       bool
	hasState bool

	// suppress guards the power switch's handler while it is being set
	// programmatically from a refresh, so seeding state does not fire a command.
	suppress bool
}

// buildLightsTab builds the playground: a target list on top, shared controls
// below. It returns the scrolled widget for the "Lights" tab.
func (a *desktopApp) buildLightsTab() *gtk.ScrolledWindow {
	pg := &playground{app: a, mode: device.ModeColour, suppress: true}

	body := gtk.NewBox(gtk.OrientationVertical, 12)
	body.SetMarginTop(18)
	body.SetMarginBottom(18)
	body.SetMarginStart(12)
	body.SetMarginEnd(12)

	// Target list: one row per device. The checkbox selects who the shared
	// controls drive; the swatch/brightness/switch show and control each
	// light's own live state.
	targets := adw.NewPreferencesGroup()
	targets.SetTitle("Lights")
	targets.SetDescription("Selected lights receive the colour below")
	for i := range a.cfg.Devices {
		dev := a.cfg.Devices[i]
		name := dev.Name
		if name == "" {
			name = dev.DeviceID
		}
		t := &playgroundTarget{ctl: a.byID[dev.DeviceID]}

		row := adw.NewActionRow()
		row.SetTitle(name)

		check := gtk.NewCheckButton()
		check.SetActive(true)
		check.SetVAlign(gtk.AlignCenter)
		check.SetTooltipText("Send the shared colour to this light")
		row.AddPrefix(check)
		row.SetActivatableWidget(check)
		t.check = check

		// Live colour swatch for this light.
		t.swatch = gtk.NewDrawingArea()
		t.swatch.SetContentWidth(20)
		t.swatch.SetContentHeight(20)
		t.swatch.SetVAlign(gtk.AlignCenter)
		t.swatch.SetDrawFunc(t.drawSwatch)
		row.AddSuffix(t.swatch)

		// Brightness readout for this light ("—" until first refresh).
		t.bright = gtk.NewLabel("—")
		t.bright.SetWidthChars(4)
		t.bright.SetXAlign(1.0)
		t.bright.SetVAlign(gtk.AlignCenter)
		t.bright.AddCSSClass("dim-label")
		row.AddSuffix(t.bright)

		// Per-light power switch (independent of the shared Power switch).
		t.power = gtk.NewSwitch()
		t.power.SetVAlign(gtk.AlignCenter)
		t.power.ConnectStateSet(func(state bool) bool {
			if t.suppress {
				return false
			}
			t.setPower(state)
			return false
		})
		row.AddSuffix(t.power)

		targets.Add(row)
		pg.targets = append(pg.targets, t)
	}
	body.Append(targets)

	// Shared controls, in a card.
	controls := gtk.NewBox(gtk.OrientationVertical, 8)
	controls.AddCSSClass("card")
	inner := gtk.NewBox(gtk.OrientationVertical, 8)
	inner.SetMarginTop(12)
	inner.SetMarginBottom(12)
	inner.SetMarginStart(12)
	inner.SetMarginEnd(12)
	controls.Append(inner)

	// Power switch for the selected lights.
	powerRow := gtk.NewBox(gtk.OrientationHorizontal, 8)
	powerLabel := gtk.NewLabel("Power")
	powerLabel.SetWidthChars(16)
	powerLabel.SetXAlign(0.0)
	powerRow.Append(powerLabel)
	powerSwitch := gtk.NewSwitch()
	powerSwitch.SetVAlign(gtk.AlignCenter)
	powerSwitch.ConnectStateSet(func(state bool) bool {
		pg.setPower(state)
		return false
	})
	powerRow.Append(powerSwitch)
	inner.Append(powerRow)

	// Mode selector: an AdwToggleGroup with Colour|White icon toggles.
	modeRow := gtk.NewBox(gtk.OrientationHorizontal, 8)
	modeLabel := gtk.NewLabel("Mode")
	modeLabel.SetWidthChars(16)
	modeLabel.SetXAlign(0.0)
	modeRow.Append(modeLabel)
	pg.modeToggle = newModeToggle(func(bool) {
		if pg.suppress {
			return
		}
		pg.onModeChanged()
	})
	modeRow.Append(pg.modeToggle)
	inner.Append(modeRow)

	// Colour wheel + preview swatch (colour mode). The wheel is the visual
	// focus: a large disc centred in the card with the live preview swatch
	// beneath it, Hue-app style, rather than a small wheel pinned to one corner.
	pg.wheelRow = gtk.NewBox(gtk.OrientationVertical, 12)
	pg.wheelRow.SetHAlign(gtk.AlignCenter)
	pg.wheelRow.SetMarginTop(8)
	pg.wheelRow.SetMarginBottom(8)

	pg.wheel = gtk.NewDrawingArea()
	pg.wheel.SetContentWidth(playgroundWheelSize)
	pg.wheel.SetContentHeight(playgroundWheelSize)
	pg.wheel.SetHAlign(gtk.AlignCenter)
	pg.wheel.SetDrawFunc(pg.drawWheel)
	pg.wheelRow.Append(pg.wheel)

	pg.swatch = gtk.NewDrawingArea()
	pg.swatch.SetContentWidth(120)
	pg.swatch.SetContentHeight(28)
	pg.swatch.SetHAlign(gtk.AlignCenter)
	pg.swatch.SetDrawFunc(pg.drawSwatch)
	pg.wheelRow.Append(pg.swatch)
	inner.Append(pg.wheelRow)

	// Wheel drag → live preview on every checked light + shared hue/sat.
	drag := gtk.NewGestureDrag()
	var startX, startY float64
	drag.ConnectDragBegin(func(x, y float64) {
		startX, startY = x, y
		pg.setSelection(x, y)
		rgb := pg.selRGB()
		tt := transitionValue(pg.trans.ActiveName() == "fade")
		for _, t := range pg.checked() {
			t.ctl.BeginLiveDrag(rgb, tt)
		}
	})
	drag.ConnectDragUpdate(func(ox, oy float64) {
		pg.setSelection(startX+ox, startY+oy)
		rgb := pg.selRGB()
		for _, t := range pg.checked() {
			t.ctl.UpdateLiveDrag(rgb)
		}
	})
	drag.ConnectDragEnd(func(ox, oy float64) {
		pg.setSelection(startX+ox, startY+oy)
		rgb := pg.selRGB()
		for _, t := range pg.checked() {
			t.ctl.UpdateLiveDrag(rgb)
			ctl := t.ctl
			go ctl.EndLiveDrag()
		}
	})
	pg.wheel.AddController(drag)

	// Brightness slider with a live percent readout. Built inline (rather than
	// via labelledScaleSimple) so the value shows as "72%" in a fixed-width
	// label instead of a bare number that shifts the slider width as it changes.
	brightRow := gtk.NewBox(gtk.OrientationHorizontal, 8)
	brightLbl := gtk.NewLabel("Brightness")
	brightLbl.SetWidthChars(16)
	brightLbl.SetXAlign(0.0)
	brightRow.Append(brightLbl)
	pg.bright = gtk.NewScaleWithRange(gtk.OrientationHorizontal, 1, 100, 1)
	pg.bright.SetHExpand(true)
	pg.bright.SetDrawValue(false)
	pg.bright.SetRoundDigits(0)
	disableScaleScroll(pg.bright)
	pg.brightPct = gtk.NewLabel("100%")
	pg.brightPct.SetWidthChars(4)
	pg.brightPct.SetXAlign(1.0)
	pg.brightPct.AddCSSClass("dim-label")
	pg.bright.ConnectValueChanged(func() {
		v := pg.bright.Value()
		pg.brightPct.SetText(fmt.Sprintf("%d%%", int(v)))
		pg.swatch.QueueDraw()
		if pg.suppress {
			return
		}
		pg.setBrightness(v)
	})
	brightRow.Append(pg.bright)
	brightRow.Append(pg.brightPct)
	pg.bright.SetValue(100)

	// Bracket brightness drags with a live music stream so the change fades
	// (DP 28) in colour mode when Fade is on, matching the wheel. Outside a
	// drag setBrightness falls back to a discrete write.
	brightDrag := gtk.NewGestureDrag()
	brightDrag.ConnectDragBegin(func(_, _ float64) {
		if pg.mode != device.ModeColour {
			return
		}
		v := pg.bright.Value()
		r, g, b := hsvToRGBInt(pg.hue, pg.sat, v/100.0)
		rgb := device.RGB{R: r, G: g, B: b}
		tt := transitionValue(pg.trans.ActiveName() == "fade")
		for _, t := range pg.checked() {
			t.ctl.BeginLiveDrag(rgb, tt)
		}
		pg.brightDragging = true
	})
	brightDrag.ConnectDragEnd(func(_, _ float64) {
		if !pg.brightDragging {
			return
		}
		pg.brightDragging = false
		for _, t := range pg.checked() {
			ctl := t.ctl
			go ctl.EndLiveDrag()
		}
	})
	pg.bright.AddController(brightDrag)

	// Temperature control (white mode): the same native gtk.Scale as Brightness,
	// so the two rows match. A CSS provider paints this one scale's trough with
	// the warm→cool gradient (see tempScaleCSS), and hides the filled/unfilled
	// split and the numeric value, so the gradient reads as one clean ramp.
	pg.tempRow = gtk.NewBox(gtk.OrientationHorizontal, 8)
	tempLbl := gtk.NewLabel("Temperature")
	tempLbl.SetWidthChars(16)
	tempLbl.SetXAlign(0.0)
	pg.tempRow.Append(tempLbl)
	pg.temp = gtk.NewScaleWithRange(gtk.OrientationHorizontal, 0, 100, 1)
	pg.temp.SetHExpand(true)
	pg.temp.SetDrawValue(false)
	pg.temp.SetRoundDigits(0)
	pg.temp.AddCSSClass("temp-scale")
	disableScaleScroll(pg.temp)
	pg.temp.ConnectValueChanged(func() {
		if pg.suppress {
			return
		}
		pg.setTemp(pg.temp.Value())
	})
	if prov := gtk.NewCSSProvider(); prov != nil {
		prov.LoadFromData(tempScaleCSS)
		if disp := gdk.DisplayGetDefault(); disp != nil {
			gtk.StyleContextAddProviderForDisplay(disp, prov, gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)
		}
	}
	pg.tempRow.Append(pg.temp)
	// Temperature sits above Brightness so white mode mirrors colour mode's
	// wheel-then-brightness order.
	inner.Append(pg.tempRow)
	inner.Append(brightRow)

	// Transition toggle: DP 28's change mode is boolean (0 = direct/jump,
	// 1 = gradual/fade), so this is a two-way toggle, not a range. Jump snaps
	// to each colour instantly (steppy); Fade smears one colour into the next.
	// Takes effect live mid-drag.
	fadeRow := gtk.NewBox(gtk.OrientationHorizontal, 8)
	fadeLabel := gtk.NewLabel("Transition")
	fadeLabel.SetWidthChars(16)
	fadeLabel.SetXAlign(0.0)
	fadeRow.Append(fadeLabel)
	pg.trans = newTransitionToggle(func(isFade bool) {
		if pg.suppress {
			return
		}
		for _, t := range pg.checked() {
			t.ctl.SetLiveTransition(transitionValue(isFade))
		}
	})
	fadeRow.Append(pg.trans)
	inner.Append(fadeRow)
	if device.DefaultTransition != 0 {
		pg.trans.SetActiveName("fade")
	} else {
		pg.trans.SetActiveName("jump")
	}

	body.Append(controls)

	// The initial SetValue above fires ConnectValueChanged; the suppress flag
	// keeps those programmatic assignments from broadcasting to the bulbs when
	// the tab is built (otherwise opening the app pushes white at full
	// brightness to every checked light).
	pg.suppress = false

	pg.applyModeVisibility()

	// Seed each target row from its device's live state (off-thread).
	pg.refreshTargets()

	clamp := adw.NewClamp()
	clamp.SetMaximumSize(600)
	clamp.SetChild(body)

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	scroll.SetChild(clamp)
	return scroll
}

// checked returns the currently selected targets.
func (pg *playground) checked() []*playgroundTarget {
	var out []*playgroundTarget
	for _, t := range pg.targets {
		if t.ctl != nil && t.check.Active() {
			out = append(out, t)
		}
	}
	return out
}

// onModeChanged flips the shared mode, toggles control visibility, and pushes
// the newly selected mode to every checked light.
func (pg *playground) onModeChanged() {
	if pg.modeToggle.ActiveName() == "white" {
		pg.mode = device.ModeWhite
	} else {
		pg.mode = device.ModeColour
	}
	pg.applyModeVisibility()
	if pg.mode == device.ModeWhite {
		pg.setTemp(pg.temp.Value())
	} else {
		pg.setColour()
	}
}

// applyModeVisibility shows the wheel for colour mode and the temp slider for
// white mode.
func (pg *playground) applyModeVisibility() {
	colour := pg.mode != device.ModeWhite
	pg.wheelRow.SetVisible(colour)
	pg.tempRow.SetVisible(!colour)
}

// transitionValue maps the Fade switch to DP 28's change-mode flag:
// off = 0 (direct/jump), on = 1 (gradual/fade).
func transitionValue(fade bool) int {
	if fade {
		return 1
	}
	return 0
}

// --- broadcast helpers (all off the GTK thread via the controls) ---

func (pg *playground) setPower(on bool) {
	for _, t := range pg.checked() {
		ctl := t.ctl
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
			defer cancel()
			_ = ctl.SetPower(ctx, on)
		}()
	}
}

func (pg *playground) setColour() {
	rgb := pg.selRGB()
	for _, t := range pg.checked() {
		ctl := t.ctl
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
			defer cancel()
			_ = ctl.SetColour(ctx, rgb)
		}()
	}
}

func (pg *playground) setBrightness(v float64) {
	// In colour mode brightness is the colour's "v": rewrite the current
	// selection with the new value in one write. In white mode it is the
	// dedicated brightness DP.
	if pg.mode == device.ModeColour {
		r, g, b := hsvToRGBInt(pg.hue, pg.sat, v/100.0)
		rgb := device.RGB{R: r, G: g, B: b}
		// Brightness in colour mode is a "v" rewrite of the current colour.
		// While the slider is being dragged we hold a live music stream open
		// (see the drag gesture on pg.bright), so the change fades when Fade
		// is on — DP 28 carries the fade bit, a discrete SetColour cannot.
		// Outside a drag (keyboard, click-to-value) fall back to a discrete
		// write, which is correct for jump and cheaper.
		if pg.brightDragging {
			for _, t := range pg.checked() {
				t.ctl.UpdateLiveDrag(rgb)
			}
			return
		}
		for _, t := range pg.checked() {
			ctl := t.ctl
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
				defer cancel()
				_ = ctl.SetColour(ctx, rgb)
			}()
		}
		return
	}
	for _, t := range pg.checked() {
		ctl := t.ctl
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
			defer cancel()
			_ = ctl.SetWhiteBrightness(ctx, v)
		}()
	}
}

func (pg *playground) setTemp(v float64) {
	for _, t := range pg.checked() {
		ctl := t.ctl
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
			defer cancel()
			_ = ctl.SetColourTempPercent(ctx, v)
		}()
	}
}

// setSelection updates the shared hue/sat from wheel coordinates and redraws.
func (pg *playground) setSelection(x, y float64) {
	pg.hue, pg.sat = coordsToHSSized(x, y, playgroundWheelSize)
	pg.wheel.QueueDraw()
	pg.swatch.QueueDraw()
}

func (pg *playground) selRGB() device.RGB {
	r, g, b := hsvToRGBInt(pg.hue, pg.sat, 1.0)
	return device.RGB{R: r, G: g, B: b}
}

// --- per-light target rows (live state) ---

// refreshTargets queries every target's device once (off the GTK thread) and
// paints its row from the result. Each target has its own control/session, so
// the queries run in parallel; results are marshalled back with IdleAdd.
func (pg *playground) refreshTargets() {
	for _, t := range pg.targets {
		if t.ctl == nil {
			continue
		}
		t := t
		go func() {
			st, err := t.ctl.Refresh(context.Background())
			coreglib.IdleAdd(func() {
				if err != nil {
					fmt.Fprintf(os.Stderr, "notuya-gui: %s -> %v\n", t.ctl.name(), err)
					return
				}
				t.applyStatus(st)
			})
		}()
	}
}

// applyStatus paints one row from a fresh device status: the on/off switch, the
// brightness readout, and the colour swatch. Runs on the GTK thread.
func (t *playgroundTarget) applyStatus(st deviceStatus) {
	t.hasState = true
	t.on = st.On

	// Seed the power switch without firing its command handler.
	t.suppress = true
	t.power.SetActive(st.On)
	t.suppress = false

	if st.On {
		t.bright.SetText(fmt.Sprintf("%d%%", int(st.BrightPct+0.5)))
	} else {
		t.bright.SetText("off")
	}

	r, g, b := sceneStateColour(stateFromStatus(t.ctl.dev.DeviceID, st))
	t.col = device.RGB{R: r, G: g, B: b}
	t.swatch.QueueDraw()
}

// setPower turns just this light on or off (off the GTK thread), independent of
// the shared Power switch. The optimistic swatch/label update lands on the next
// refresh; here we only issue the command.
func (t *playgroundTarget) setPower(on bool) {
	ctl := t.ctl
	if ctl == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		_ = ctl.SetPower(ctx, on)
	}()
}

// drawSwatch paints this light's current colour as a small rounded square, or a
// muted placeholder before the first refresh lands.
func (t *playgroundTarget) drawSwatch(_ *gtk.DrawingArea, cr *cairo.Context, width, height int) {
	w, h := float64(width), float64(height)
	roundedRect(cr, 0, 0, w, h, 5)
	if !t.hasState {
		cr.SetSourceRGBA(1, 1, 1, 0.12)
	} else {
		cr.SetSourceRGB(float64(t.col.R)/255, float64(t.col.G)/255, float64(t.col.B)/255)
	}
	cr.Fill()
	roundedRect(cr, 0.5, 0.5, w-1, h-1, 5)
	cr.SetSourceRGBA(0, 0, 0, 0.2)
	cr.SetLineWidth(1)
	cr.Stroke()
}

func (pg *playground) drawWheel(_ *gtk.DrawingArea, cr *cairo.Context, width, height int) {
	scale := float64(playgroundWheelSize) / float64(wheelSize)
	cr.Save()
	cr.Scale(scale, scale)
	cr.SetSourceSurface(pg.app.wheelSurface, 0, 0)
	cr.Paint()
	cr.Restore()

	radius := float64(playgroundWheelSize) / 2.0
	angle := pg.hue * 2 * math.Pi
	dist := pg.sat * radius
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

func (pg *playground) drawSwatch(_ *gtk.DrawingArea, cr *cairo.Context, width, height int) {
	// Preview the colour at the current brightness so the swatch reflects what
	// the lights will actually show, not just the hue/sat picked on the wheel.
	v := 1.0
	if pg.bright != nil {
		v = pg.bright.Value() / 100.0
	}
	r, g, b := hsvToRGBInt(pg.hue, pg.sat, v)
	w, h := float64(width), float64(height)
	radius := h / 2.0
	roundedRect(cr, 0, 0, w, h, radius)
	cr.SetSourceRGB(float64(r)/255, float64(g)/255, float64(b)/255)
	cr.Fill()
	roundedRect(cr, 0.5, 0.5, w-1, h-1, radius)
	cr.SetSourceRGBA(0, 0, 0, 0.15)
	cr.SetLineWidth(1)
	cr.Stroke()
}
