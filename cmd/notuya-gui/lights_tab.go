package main

import (
	"context"
	"fmt"
	"os"

	"github.com/alex-bluetrain/notuya-go/pkg/device"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/cairo"
	coreglib "github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// lightsWheelSize is the diameter of the shared colour wheel on the Lights tab.
const lightsWheelSize = 240

// lightsTab is the Lights tab: a checkbox list of devices above one set of
// shared controls (colourControls + transition) whose changes are broadcast to
// every checked light. It drives the app's per-device control instances
// (mutex-serialized, shared with the scene editor): the wheel streams through
// BeginLiveDrag/UpdateLiveDrag/EndLiveDrag, sliders/mode/power go through the
// discrete setters.
type lightsTab struct {
	app *desktopApp

	targets []*lightTarget

	cc    *colourControls
	trans *adw.ToggleGroup

	// current shared mode (colour selection lives in cc)
	mode string

	// brightDragging is true while the brightness slider is being dragged in
	// colour mode with a live music stream open, so setBrightness streams the
	// colour (fades) instead of writing it discretely (snaps).
	brightDragging bool

	suppress bool
}

// lightTarget is one device's row in the target list. The checkbox selects
// whether the shared wheel/sliders drive this light; the swatch, brightness
// readout, and per-light power switch reflect and control this light's own live
// state (seeded from a Refresh on open), independent of the shared controls.
type lightTarget struct {
	ctl    *control
	check  *gtk.CheckButton
	swatch *gtk.DrawingArea
	bright *gtk.Label
	power  *gtk.Switch

	// state is the last known state for this light (seeded from a refresh,
	// then kept current optimistically as broadcasts go out); col is the
	// swatch colour derived from it. hasState is false until the first
	// refresh lands so the swatch can read as "unknown".
	state    SceneState
	col      device.RGB
	hasState bool

	// suppress guards the power switch's handler while it is being set
	// programmatically from a refresh, so seeding state does not fire a command.
	suppress bool
}

// buildLightsTab builds the target list on top and the shared controls below. It returns the scrolled widget for the "Lights" tab.
func (a *desktopApp) buildLightsTab() *gtk.ScrolledWindow {
	lt := &lightsTab{app: a, mode: device.ModeColour, suppress: true}

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
		t := &lightTarget{ctl: a.byID[dev.DeviceID]}

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
		lt.targets = append(lt.targets, t)
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

	// Power switch for the selected lights: label at the start, switch at the
	// end, spanning the card like an AdwActionRow so the card keeps one
	// alignment system.
	powerRow := gtk.NewBox(gtk.OrientationHorizontal, 8)
	powerLabel := gtk.NewLabel("Power")
	powerLabel.SetXAlign(0.0)
	powerLabel.SetHExpand(true)
	powerRow.Append(powerLabel)
	powerSwitch := gtk.NewSwitch()
	powerSwitch.SetVAlign(gtk.AlignCenter)
	powerSwitch.ConnectStateSet(func(state bool) bool {
		lt.setPower(state)
		return false
	})
	powerRow.Append(powerSwitch)
	inner.Append(powerRow)

	// Shared colour-selection controls (mode toggle, wheel, temperature,
	// brightness) — the same widget set the scene editor uses.
	lt.cc = newColourControls(a.wheelSurface, lightsWheelSize, colourCallbacks{
		OnMode: func(bool) {
			if lt.suppress {
				return
			}
			lt.onModeChanged()
		},
		// Wheel drag → live preview on every checked light.
		OnDragBegin: func(rgb device.RGB) {
			tt := transitionValue(lt.trans.ActiveName() == "fade")
			for _, t := range lt.checked() {
				t.ctl.BeginLiveDrag(rgb, tt)
			}
		},
		OnDragUpdate: func(rgb device.RGB) {
			for _, t := range lt.checked() {
				t.ctl.UpdateLiveDrag(rgb)
			}
			lt.mirrorColour()
		},
		OnDragEnd: func(rgb device.RGB) {
			for _, t := range lt.checked() {
				t.ctl.UpdateLiveDrag(rgb)
				ctl := t.ctl
				go ctl.EndLiveDrag()
			}
			lt.mirrorColour()
		},
		OnBright: func(v float64) {
			if lt.suppress {
				return
			}
			lt.setBrightness(v)
		},
		OnTemp: func(v float64) {
			if lt.suppress {
				return
			}
			lt.setTemp(v)
		},
	})
	lt.cc.AppendTo(inner)

	// Bracket brightness drags with a live music stream so the change fades
	// (DP 28) in colour mode when Fade is on, matching the wheel. Outside a
	// drag setBrightness falls back to a discrete write.
	brightDrag := gtk.NewGestureDrag()
	brightDrag.ConnectDragBegin(func(_, _ float64) {
		if lt.mode != device.ModeColour {
			return
		}
		v := lt.cc.Bright.Value()
		r, g, b := hsvToRGBInt(lt.cc.hue, lt.cc.sat, v/100.0)
		rgb := device.RGB{R: r, G: g, B: b}
		tt := transitionValue(lt.trans.ActiveName() == "fade")
		for _, t := range lt.checked() {
			t.ctl.BeginLiveDrag(rgb, tt)
		}
		lt.brightDragging = true
	})
	brightDrag.ConnectDragEnd(func(_, _ float64) {
		if !lt.brightDragging {
			return
		}
		lt.brightDragging = false
		for _, t := range lt.checked() {
			ctl := t.ctl
			go ctl.EndLiveDrag()
		}
	})
	lt.cc.Bright.AddController(brightDrag)

	// Transition toggle: DP 28's change mode is boolean (0 = direct/jump,
	// 1 = gradual/fade), so this is a two-way toggle, not a range. Jump snaps
	// to each colour instantly (steppy); Fade smears one colour into the next.
	// Takes effect live mid-drag.
	lt.trans = newTransitionToggle(func(isFade bool) {
		if lt.suppress {
			return
		}
		for _, t := range lt.checked() {
			t.ctl.SetLiveTransition(transitionValue(isFade))
		}
	})
	inner.Append(lt.trans)
	if device.DefaultTransition != 0 {
		lt.trans.SetActiveName("fade")
	} else {
		lt.trans.SetActiveName("jump")
	}

	body.Append(controls)

	// Seeding the controls' initial values fires their change handlers; the
	// suppress flag keeps those programmatic assignments from broadcasting to
	// the bulbs (otherwise opening the app pushes white at full brightness to
	// every checked light).
	lt.suppress = false

	lt.cc.ApplyModeVisibility()

	// Seed each target row from its device's live state (off-thread).
	lt.refreshTargets()

	clamp := adw.NewClamp()
	clamp.SetMaximumSize(600)
	clamp.SetChild(body)

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	scroll.SetChild(clamp)
	return scroll
}

// checked returns the currently selected targets.
func (lt *lightsTab) checked() []*lightTarget {
	var out []*lightTarget
	for _, t := range lt.targets {
		if t.ctl != nil && t.check.Active() {
			out = append(out, t)
		}
	}
	return out
}

// onModeChanged flips the shared mode and pushes the newly selected mode to
// every checked light (visibility is handled by the colourControls).
func (lt *lightsTab) onModeChanged() {
	if lt.cc.IsWhite() {
		lt.mode = device.ModeWhite
	} else {
		lt.mode = device.ModeColour
	}
	if lt.mode == device.ModeWhite {
		lt.setTemp(lt.cc.Temp.Value())
	} else {
		lt.setColour()
	}
}

// transitionValue maps the Instant|Smooth toggle to DP 28's change-mode flag:
// 0 = direct (Instant), 1 = gradual (Smooth).
func transitionValue(fade bool) int {
	if fade {
		return 1
	}
	return 0
}

// --- broadcast helpers (all off the GTK thread via the controls) ---

// mirrorChecked applies fn to every checked row's cached state and repaints
// it, so the target list follows what was just broadcast without querying any
// device. Rows that never got a first refresh (device unreachable) are left
// alone rather than shown a state they may not have taken.
func (lt *lightsTab) mirrorChecked(fn func(*SceneState)) {
	for _, t := range lt.checked() {
		if !t.hasState {
			continue
		}
		fn(&t.state)
		t.repaint()
	}
}

// mirrorColour mirrors the shared colour selection (hue/sat/brightness) into
// the checked rows.
func (lt *lightsTab) mirrorColour() {
	hue, sat, bright := lt.cc.hue, lt.cc.sat, lt.cc.Bright.Value()
	lt.mirrorChecked(func(s *SceneState) {
		s.Mode = device.ModeColour
		s.Hue = hue
		s.Sat = sat
		s.Bright = bright
	})
}

func (lt *lightsTab) setPower(on bool) {
	for _, t := range lt.checked() {
		ctl := t.ctl
		ctl.async("power", func(ctx context.Context) error { return ctl.SetPower(ctx, on) })
	}
	lt.mirrorChecked(func(s *SceneState) { s.On = on })
}

func (lt *lightsTab) setColour() {
	rgb := lt.cc.SelRGB()
	for _, t := range lt.checked() {
		ctl := t.ctl
		ctl.async("colour", func(ctx context.Context) error { return ctl.SetColour(ctx, rgb) })
	}
	lt.mirrorColour()
}

func (lt *lightsTab) setBrightness(v float64) {
	// In colour mode brightness is the colour's "v", so rewrite the current
	// selection. While the slider is dragged a live music stream is open (see
	// brightDrag in buildLightsTab), so the change honours Smooth — DP 28
	// carries the fade bit, a discrete SetColour cannot. Outside a drag
	// (keyboard, click) a discrete write is correct and cheaper. In white mode
	// brightness is its own DP.
	if lt.mode == device.ModeColour {
		r, g, b := hsvToRGBInt(lt.cc.hue, lt.cc.sat, v/100.0)
		rgb := device.RGB{R: r, G: g, B: b}
		if lt.brightDragging {
			for _, t := range lt.checked() {
				t.ctl.UpdateLiveDrag(rgb)
			}
			lt.mirrorColour()
			return
		}
		for _, t := range lt.checked() {
			ctl := t.ctl
			ctl.async("colour", func(ctx context.Context) error { return ctl.SetColour(ctx, rgb) })
		}
		lt.mirrorColour()
		return
	}
	for _, t := range lt.checked() {
		ctl := t.ctl
		ctl.async("brightness", func(ctx context.Context) error { return ctl.SetWhiteBrightness(ctx, v) })
	}
	lt.mirrorChecked(func(s *SceneState) {
		s.Mode = device.ModeWhite
		s.Bright = v
	})
}

func (lt *lightsTab) setTemp(v float64) {
	for _, t := range lt.checked() {
		ctl := t.ctl
		ctl.async("temperature", func(ctx context.Context) error { return ctl.SetColourTempPercent(ctx, v) })
	}
	lt.mirrorChecked(func(s *SceneState) {
		s.Mode = device.ModeWhite
		s.Temp = v
	})
}

// --- per-light target rows (live state) ---

// refreshTargets queries every target's device once (off the GTK thread) and
// paints its row from the result. Each target has its own control/session, so
// the queries run in parallel; results are marshalled back with IdleAdd.
func (lt *lightsTab) refreshTargets() {
	for _, t := range lt.targets {
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
func (t *lightTarget) applyStatus(st deviceStatus) {
	t.state = stateFromStatus(t.ctl.dev.DeviceID, st)
	t.hasState = true
	t.repaint()
}

// repaint redraws the row's widgets from t.state. Runs on the GTK thread.
func (t *lightTarget) repaint() {
	// Seed the power switch without firing its command handler.
	t.suppress = true
	t.power.SetActive(t.state.On)
	t.suppress = false

	if t.state.On {
		t.bright.SetText(fmt.Sprintf("%d%%", int(t.state.Bright+0.5)))
	} else {
		t.bright.SetText("off")
	}

	r, g, b := sceneStateColour(t.state)
	t.col = device.RGB{R: r, G: g, B: b}
	t.swatch.QueueDraw()
}

// setPower turns just this light on or off (off the GTK thread), independent
// of the shared Power switch, and optimistically repaints the row.
func (t *lightTarget) setPower(on bool) {
	ctl := t.ctl
	if ctl == nil {
		return
	}
	ctl.async("power", func(ctx context.Context) error { return ctl.SetPower(ctx, on) })
	if t.hasState {
		t.state.On = on
		t.repaint()
	}
}

// drawSwatch paints this light's current colour as a small rounded square, or a
// muted placeholder before the first refresh lands.
func (t *lightTarget) drawSwatch(_ *gtk.DrawingArea, cr *cairo.Context, width, height int) {
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
