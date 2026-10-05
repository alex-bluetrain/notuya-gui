package main

import (
	"math"

	"github.com/alex-bluetrain/notuya-go/pkg/dp"

	"github.com/alex-bluetrain/notuya-gui/internal/colour"
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
// (shared with the scene editor): wheel and brightness go to Live (DP 28,
// saved to DP 24 when the tab is left); white mode and power are commands.
type lightsTab struct {
	app *desktopApp

	targets []*lightTarget

	cc    *colourControls
	trans *adw.ToggleGroup
	power *adw.SwitchRow

	// current shared mode (colour selection lives in cc)
	mode dp.WorkMode

	suppress bool
}

// lightTarget is one device's row in the target list. The checkbox is the row's
// only control: it selects whether the shared controls below drive this light.
// The swatch and subtitle are read-only — they report this light's live state
// (published by the control's keeper on each (re)connect, then kept current
// as broadcasts go out).
type lightTarget struct {
	ctl    *control
	row    *adw.ActionRow
	check  *gtk.CheckButton
	swatch *gtk.DrawingArea

	// state is the last known state for this light (from the keeper's
	// latest status, then kept current optimistically as broadcasts go out);
	// col is the swatch colour derived from it. hasState is false until the
	// first status lands so the swatch can read as "unknown".
	state    SceneState
	col      colour.RGB
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
		// The wheel and the brightness slider both feed each checked light's
		// live colour; the bulbs show it at once and keep it until Save.
		OnDragBegin:  func(dp.HSV) { lt.setColour() },
		OnDragUpdate: func(dp.HSV) { lt.setColour() },
		OnDragEnd:    func(dp.HSV) { lt.setColour() },
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

	// Change-mode toggle (DP 28): Jump snaps to each colour instantly
	// (steppy); Fade smears one colour into the next. The next live colour
	// carries it.
	lt.trans = newChangeModeToggle(func(dp.ChangeMode) {})
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
	// and after a wall-switch power cycle.
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

	body.Append(lt.buildWireGroup())

	clamp := adw.NewClamp()
	clamp.SetMaximumSize(600)
	clamp.SetChild(body)

	a.onSyncLock = lt.setSynced
	a.saveLights = lt.saveColours

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
// device. Rows that never got a first status (device unreachable) are left
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
		t.ctl.Power(on)
	}
	lt.mirrorChecked(func(s *SceneState) { s.On = on })
}

// setColour shows the wheel's selection (hue, saturation, brightness) on
// every checked light. Nothing is saved until the tab is left (saveColours).
func (lt *lightsTab) setColour() {
	c, mode := lt.cc.Sel(), changeModeOf(lt.trans)
	for _, t := range lt.checked() {
		t.ctl.Live(c, mode)
	}
	lt.mirrorColour()
}

// saveColours makes the colours shown on the lights stick (DP 24). Called
// when the Lights tab is left; quitting saves through control.Close.
func (lt *lightsTab) saveColours() {
	for _, t := range lt.targets {
		if t.ctl != nil && !t.synced {
			t.ctl.Save()
		}
	}
}

func (lt *lightsTab) setBrightness(v float64) {
	// In colour mode brightness is the colour's "v"; in white mode it is its
	// own DP.
	if lt.mode == dp.ModeColour {
		lt.setColour()
		return
	}
	for _, t := range lt.checked() {
		t.ctl.WhiteBrightness(v)
	}
	lt.mirrorChecked(func(s *SceneState) {
		s.Mode = dp.ModeWhite
		s.Bright = v
	})
}

func (lt *lightsTab) setTemp(v float64) {
	for _, t := range lt.checked() {
		t.ctl.ColourTemp(v)
	}
	lt.mirrorChecked(func(s *SceneState) {
		s.Mode = dp.ModeWhite
		s.Temp = v
	})
}

// --- per-light target rows (live state) ---

// setSynced locks out the lights Screen Sync drives. Released lights keep
// showing Screen Sync's last colour until the user changes them.
func (lt *lightsTab) setSynced(synced map[string]bool) {
	for _, t := range lt.targets {
		if t.ctl == nil {
			continue
		}
		on := synced[t.ctl.device().DeviceID]
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
	t.state = stateFromStatus(t.ctl.device().DeviceID, st)
	t.hasState = true
	t.repaint()
}

// repaint redraws the row from t.state: the subtitle reports on/off and
// brightness, the swatch shows the colour. Runs on the GTK thread.
func (t *lightTarget) repaint() {
	if t.synced {
		t.row.SetSubtitle("Controlled by Screen Sync")
	} else if t.state.On {
		t.row.SetSubtitle("On · " + pctText(t.state.Bright))
	} else {
		t.row.SetSubtitle("Off")
	}

	r, g, b := sceneStateColour(t.state)
	t.col = colour.RGB{R: r, G: g, B: b}
	t.swatch.QueueDraw()
}

// drawSwatch paints this light's current colour as a small rounded square, or a
// muted placeholder before the first status lands.
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

// buildWireGroup shows, per light, the "dps" object of the last control
// message actually written to it (commands and live streams alike).
func (lt *lightsTab) buildWireGroup() *adw.PreferencesGroup {
	g := adw.NewPreferencesGroup()
	g.SetTitle("Sent to bulbs")
	g.SetDescription("The last DPs written to each light, as sent")
	labels := map[string]*gtk.Label{}
	for _, t := range lt.targets {
		row := adw.NewActionRow()
		row.SetTitle(t.ctl.name())
		l := gtk.NewLabel("nothing yet")
		l.AddCSSClass("monospace")
		l.AddCSSClass("dim-label")
		l.SetSelectable(true)
		l.SetWrap(true)
		l.SetXAlign(1)
		row.AddSuffix(l)
		g.Add(row)
		labels[t.ctl.device().DeviceID] = l
	}
	refresh := func() {
		for id, w := range wireSnapshot() {
			l := labels[id]
			if l == nil {
				continue
			}
			text := w.At.Format("15:04:05.000") + "  " + w.DPs
			if w.Err != nil {
				text += "  (failed: " + w.Err.Error() + ")"
			}
			l.SetText(text)
		}
	}
	fn := func() { coreglib.IdleAdd(refresh) }
	wireOnWrite.Store(&fn)
	return g
}
