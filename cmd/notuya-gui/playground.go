package main

import (
	"context"
	"math"

	"github.com/averstraeten/notuya-go/pkg/device"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/cairo"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// playgroundWheelSize is the diameter of the shared colour wheel in the Lights
// playground. Matches the scene editor's wheel for a consistent feel.
const playgroundWheelSize = 200

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
	tempRow   *gtk.Box
	temp      *gtk.Scale

	// current shared selection
	hue  float64
	sat  float64
	mode string

	suppress bool
}

// playgroundTarget pairs a device's checkbox with its control.
type playgroundTarget struct {
	ctl   *control
	check *gtk.CheckButton
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

	// Target list: one checkbox per device (checked = receives changes).
	targets := adw.NewPreferencesGroup()
	targets.SetTitle("Lights")
	targets.SetDescription("Selected lights receive the colour below")
	for i := range a.cfg.Devices {
		dev := a.cfg.Devices[i]
		name := dev.Name
		if name == "" {
			name = dev.DeviceID
		}
		row := adw.NewActionRow()
		row.SetTitle(name)
		check := gtk.NewCheckButton()
		check.SetActive(true)
		check.SetVAlign(gtk.AlignCenter)
		row.AddPrefix(check)
		row.SetActivatableWidget(check)
		targets.Add(row)
		pg.targets = append(pg.targets, &playgroundTarget{ctl: a.byID[dev.DeviceID], check: check})
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

	// Colour wheel + swatch (colour mode).
	pg.wheelRow = gtk.NewBox(gtk.OrientationHorizontal, 10)
	pg.wheel = gtk.NewDrawingArea()
	pg.wheel.SetContentWidth(playgroundWheelSize)
	pg.wheel.SetContentHeight(playgroundWheelSize)
	pg.wheel.SetDrawFunc(pg.drawWheel)
	pg.wheelRow.Append(pg.wheel)

	swatchBox := gtk.NewBox(gtk.OrientationVertical, 6)
	swatchBox.SetVAlign(gtk.AlignCenter)
	pg.swatch = gtk.NewDrawingArea()
	pg.swatch.SetContentWidth(60)
	pg.swatch.SetContentHeight(60)
	pg.swatch.SetDrawFunc(pg.drawSwatch)
	swatchBox.Append(pg.swatch)
	pg.wheelRow.Append(swatchBox)
	inner.Append(pg.wheelRow)

	// Wheel drag → live preview on every checked light + shared hue/sat.
	drag := gtk.NewGestureDrag()
	var startX, startY float64
	drag.ConnectDragBegin(func(x, y float64) {
		startX, startY = x, y
		pg.setSelection(x, y)
		rgb := pg.selRGB()
		for _, t := range pg.checked() {
			t.ctl.BeginLiveDrag(rgb)
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

	// Brightness slider.
	inner.Append(labelledScaleSimple("Brightness", &pg.bright, 1, 100, func(v float64) {
		if pg.suppress {
			return
		}
		pg.setBrightness(v)
	}))
	pg.bright.SetValue(100)

	// Temperature slider (white mode).
	pg.tempRow = labelledScaleSimple("Temp (cold→warm)", &pg.temp, 0, 100, func(v float64) {
		if pg.suppress {
			return
		}
		pg.setTemp(v)
	})
	inner.Append(pg.tempRow)

	body.Append(controls)

	// The initial SetValue above fires ConnectValueChanged; the suppress flag
	// keeps those programmatic assignments from broadcasting to the bulbs when
	// the tab is built (otherwise opening the app pushes white at full
	// brightness to every checked light).
	pg.suppress = false

	pg.applyModeVisibility()

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
	r, g, b := hsvToRGBInt(pg.hue, pg.sat, 1.0)
	w, h := float64(width), float64(height)
	roundedRect(cr, 0, 0, w, h, 8)
	cr.SetSourceRGB(float64(r)/255, float64(g)/255, float64(b)/255)
	cr.Fill()
	roundedRect(cr, 0.5, 0.5, w-1, h-1, 8)
	cr.SetSourceRGBA(0, 0, 0, 0.15)
	cr.SetLineWidth(1)
	cr.Stroke()
}
