package main

import (
	"context"
	"fmt"
	"math"

	"github.com/alex-bluetrain/notuya-go/pkg/dp"
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
// BeginLive/UpdateLive/EndLive, sliders/mode/power go through the
// discrete setters.
type lightsTab struct {
	app *desktopApp

	targets []*lightTarget

	cc    *colourControls
	trans *adw.ToggleGroup
	power *adw.SwitchRow

	// current shared mode (colour selection lives in cc)
	mode dp.WorkMode

	// brightDragging is true while the brightness slider is being dragged in
	// colour mode with a live colour stream open, so setBrightness streams the
	// colour (fades) instead of writing it discretely (snaps).
	brightDragging bool

	suppress bool
}

// lightTarget is one device's row in the target list. The checkbox is the row's
// only control: it selects whether the shared controls below drive this light.
// The swatch and subtitle are read-only — they report this light's live state
// (seeded from a Refresh on open, then kept current as broadcasts go out).
type lightTarget struct {
	ctl    *control
	row    *adw.ActionRow
	check  *gtk.CheckButton
	swatch *gtk.DrawingArea

	// state is the last known state for this light (seeded from a refresh,
	// then kept current optimistically as broadcasts go out); col is the
	// swatch colour derived from it. hasState is false until the first
	// refresh lands so the swatch can read as "unknown".
	state    SceneState
	col      dp.RGB
	hasState bool
	synced   bool // driven by Screen Sync: locked out here
}

// buildLightsTab builds the target list on top and the shared controls below. It returns the scrolled widget for the "Lights" tab.
func (a *desktopApp) buildLightsTab() *gtk.ScrolledWindow {
	lt := &lightsTab{app: a, mode: dp.ModeColour, suppress: true}

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
	targets.SetDescription("The controls below apply to the selected lights")
	for i := range a.cfg.Devices {
		dev := a.cfg.Devices[i]
		name := dev.Name
		if name == "" {
			name = dev.DeviceID
		}
		t := &lightTarget{ctl: a.byID[dev.DeviceID]}

		row := adw.NewActionRow()
		row.SetTitle(name)
		row.SetSubtitle("…")
		t.row = row

		check := gtk.NewCheckButton()
		check.SetActive(true)
		check.SetVAlign(gtk.AlignCenter)
		check.SetTooltipText("Apply the controls below to this light")
		// Selecting/deselecting changes who Power speaks for, so re-derive it.
		check.ConnectToggled(func() { lt.syncPowerSwitch() })
		row.AddPrefix(check)
		row.SetActivatableWidget(check)
		t.check = check

		// Live colour swatch: this light's current colour and on/off state,
		// read-only (the row's only control is the selection checkbox).
		t.swatch = gtk.NewDrawingArea()
		t.swatch.SetContentWidth(20)
		t.swatch.SetContentHeight(20)
		t.swatch.SetVAlign(gtk.AlignCenter)
		t.swatch.SetDrawFunc(t.drawSwatch)
		row.AddSuffix(t.swatch)

		targets.Add(row)
		lt.targets = append(lt.targets, t)
	}
	body.Append(targets)

	// Power for the selected lights. An AdwSwitchRow is one widget: the label
	// and the switch belong to the same row and the whole row toggles it, so
	// there is no loose caption floating beside an unrelated control.
	powerGroup := adw.NewPreferencesGroup()
	lt.power = adw.NewSwitchRow()
	lt.power.SetTitle("Power")
	lt.power.SetSubtitle("Turn the selected lights on or off")
	lt.power.NotifyProperty("active", func() {
		if lt.suppress {
			return
		}
		lt.setPower(lt.power.Active())
	})
	powerGroup.Add(lt.power)
	body.Append(powerGroup)

	// Shared controls, in a card.
	controls := gtk.NewBox(gtk.OrientationVertical, 8)
	controls.AddCSSClass("card")
	inner := gtk.NewBox(gtk.OrientationVertical, 8)
	inner.SetMarginTop(12)
	inner.SetMarginBottom(12)
	inner.SetMarginStart(12)
	inner.SetMarginEnd(12)
	controls.Append(inner)

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
		OnDragBegin: func(rgb dp.RGB) {
			mode := changeModeOf(lt.trans)
			for _, t := range lt.checked() {
				t.ctl.BeginLive(lt, rgb, mode)
			}
		},
		OnDragUpdate: func(rgb dp.RGB) {
			for _, t := range lt.checked() {
				t.ctl.UpdateLive(lt, rgb)
			}
			lt.mirrorColour()
		},
		OnDragEnd: func(rgb dp.RGB) {
			for _, t := range lt.checked() {
				t.ctl.UpdateLive(lt, rgb)
				ctl := t.ctl
				go ctl.EndLive(lt)
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

	// Bracket brightness drags with a live colour stream so the change fades
	// (DP 28) in colour mode when Fade is on, matching the wheel. Outside a
	// drag setBrightness falls back to a discrete write.
	brightDrag := gtk.NewGestureDrag()
	brightDrag.ConnectDragBegin(func(_, _ float64) {
		if lt.mode != dp.ModeColour {
			return
		}
		v := lt.cc.Bright.Value()
		r, g, b := hsvToRGBInt(lt.cc.hue, lt.cc.sat, v/100.0)
		rgb := dp.RGB{R: r, G: g, B: b}
		mode := changeModeOf(lt.trans)
		for _, t := range lt.checked() {
			t.ctl.BeginLive(lt, rgb, mode)
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
			go ctl.EndLive(lt)
		}
	})
	lt.cc.Bright.AddController(brightDrag)

	// Change-mode toggle (DP 28): Jump snaps to each colour instantly
	// (steppy); Fade smears one colour into the next. Takes effect live
	// mid-drag.
	lt.trans = newChangeModeToggle(func(mode dp.ChangeMode) {
		if lt.suppress {
			return
		}
		for _, t := range lt.checked() {
			t.ctl.SetLiveChangeMode(lt, mode)
		}
	})
	inner.Append(lt.trans)
	setChangeMode(lt.trans, dp.DefaultChangeMode)

	body.Append(controls)

	// Seeding the controls' initial values fires their change handlers; the
	// suppress flag keeps those programmatic assignments from broadcasting to
	// the bulbs (otherwise opening the app pushes white at full brightness to
	// every checked light).
	lt.suppress = false

	lt.cc.ApplyModeVisibility()

	// Paint each target row whenever its device (re)connects: at startup,
	// after a wall-switch power cycle, and after a live stream ends.
	for _, t := range lt.targets {
		if t.ctl == nil {
			continue
		}
		t := t
		t.ctl.Subscribe(func(st deviceStatus) {
			coreglib.IdleAdd(func() {
				t.applyStatus(st)
				lt.syncPowerSwitch()
			})
		})
	}

	clamp := adw.NewClamp()
	clamp.SetMaximumSize(600)
	clamp.SetChild(body)

	a.onSyncLock = lt.setSynced

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	scroll.SetChild(clamp)
	return scroll
}

// checked returns the currently selected targets.
func (lt *lightsTab) checked() []*lightTarget {
	var out []*lightTarget
	for _, t := range lt.targets {
		if t.ctl != nil && !t.synced && t.check.Active() {
			out = append(out, t)
		}
	}
	return out
}

// onModeChanged flips the shared mode and pushes the newly selected mode to
// every checked light (visibility is handled by the colourControls).
func (lt *lightsTab) onModeChanged() {
	if lt.cc.IsWhite() {
		lt.mode = dp.ModeWhite
	} else {
		lt.mode = dp.ModeColour
	}
	if lt.mode == dp.ModeWhite {
		lt.setTemp(lt.cc.Temp.Value())
	} else {
		lt.setColour()
	}
}

// syncPowerSwitch points the shared Power switch at the selected lights: on if
// any of them is on, so flipping it off is always the useful action. Setting it
// is guarded by suppress so seeding does not broadcast a power command back to
// the bulbs. Rows with no state yet (unreachable device) do not vote.
func (lt *lightsTab) syncPowerSwitch() {
	any := false
	for _, t := range lt.checked() {
		if t.hasState && t.state.On {
			any = true
			break
		}
	}
	if lt.power.Active() == any {
		return
	}
	was := lt.suppress
	lt.suppress = true
	lt.power.SetActive(any)
	lt.suppress = was
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
		s.Mode = dp.ModeColour
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
	// selection. While the slider is dragged a live colour stream is open (see
	// brightDrag in buildLightsTab), so the change honours Smooth — DP 28
	// carries the fade bit, a discrete SetColour cannot. Outside a drag
	// (keyboard, click) a discrete write is correct and cheaper. In white mode
	// brightness is its own DP.
	if lt.mode == dp.ModeColour {
		r, g, b := hsvToRGBInt(lt.cc.hue, lt.cc.sat, v/100.0)
		rgb := dp.RGB{R: r, G: g, B: b}
		if lt.brightDragging {
			for _, t := range lt.checked() {
				t.ctl.UpdateLive(lt, rgb)
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
		s.Mode = dp.ModeWhite
		s.Bright = v
	})
}

func (lt *lightsTab) setTemp(v float64) {
	for _, t := range lt.checked() {
		ctl := t.ctl
		ctl.async("temperature", func(ctx context.Context) error { return ctl.SetColourTempPercent(ctx, v) })
	}
	lt.mirrorChecked(func(s *SceneState) {
		s.Mode = dp.ModeWhite
		s.Temp = v
	})
}

// --- per-light target rows (live state) ---

// setSynced locks out the lights Screen Sync drives. Released lights are
// re-read by their control's link keeper once the stream ends.
func (lt *lightsTab) setSynced(synced map[string]bool) {
	for _, t := range lt.targets {
		if t.ctl == nil {
			continue
		}
		on := synced[t.ctl.dev.DeviceID]
		if on == t.synced {
			continue
		}
		t.synced = on
		t.row.SetSensitive(!on)
		t.repaint()
	}
	lt.syncPowerSwitch()
}

// applyStatus paints one row from a fresh device status: the on/off switch, the
// brightness readout, and the colour swatch. Runs on the GTK thread.
func (t *lightTarget) applyStatus(st deviceStatus) {
	t.state = stateFromStatus(t.ctl.dev.DeviceID, st)
	t.hasState = true
	t.repaint()
}

// repaint redraws the row from t.state: the subtitle reports on/off and
// brightness, the swatch shows the colour. Runs on the GTK thread.
func (t *lightTarget) repaint() {
	if t.synced {
		t.row.SetSubtitle("Controlled by Screen Sync")
	} else if t.state.On {
		t.row.SetSubtitle(fmt.Sprintf("On · %d%%", int(t.state.Bright+0.5)))
	} else {
		t.row.SetSubtitle("Off")
	}

	r, g, b := sceneStateColour(t.state)
	t.col = dp.RGB{R: r, G: g, B: b}
	t.swatch.QueueDraw()
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

// roundedRect traces a rounded rectangle path onto cr.
func roundedRect(cr *cairo.Context, x, y, w, h, r float64) {
	cr.NewSubPath()
	cr.Arc(x+w-r, y+r, r, -math.Pi/2, 0)
	cr.Arc(x+w-r, y+h-r, r, 0, math.Pi/2)
	cr.Arc(x+r, y+h-r, r, math.Pi/2, math.Pi)
	cr.Arc(x+r, y+r, r, math.Pi, 3*math.Pi/2)
	cr.ClosePath()
}
