package main

import (
	"context"
	"math"

	"github.com/alex-bluetrain/notuya-go/pkg/dp"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// sceneEditorWheelSize is the diameter of the per-light colour wheel inside the
// scene editor (smaller than the Lights tab's, since there is one per light).
const sceneEditorWheelSize = 180

// sceneEditor is the modal dialog that edits a scene's snapshot with live
// preview: moving a wheel or slider drives the real bulb (via the shared
// control), and each change is also written into the in-memory SceneState so
// Save persists exactly what was previewed. The preview is destructive —
// closing the dialog leaves the lights at their last previewed value, like the
// Lights tab.
type sceneEditor struct {
	app    *desktopApp
	index  int // -1 = new scene, >=0 = edit existing
	name   *gtk.Entry
	rows   []*sceneDeviceRow
	dialog *adw.Window
}

// sceneDeviceRow is one light's editor card: an include toggle, power switch,
// colour/white mode selector, colour wheel, brightness and temperature sliders.
// It owns the in-memory SceneState (st) it edits and the control it previews on.
type sceneDeviceRow struct {
	editor   *sceneEditor
	ctl      *control
	deviceID string

	card    *gtk.Box
	include *gtk.CheckButton
	power   *gtk.Switch
	cc      *colourControls

	st       SceneState
	suppress bool
}

// openSceneEditor builds and presents the scene editor. index<0 creates a new
// scene; index>=0 edits a.cfg.Scenes[index].
func (a *desktopApp) openSceneEditor(index int) {
	e := &sceneEditor{app: a, index: index}

	win := adw.NewWindow()
	win.SetModal(true)
	win.SetTransientFor(&a.window.Window)
	win.SetDefaultSize(560, 720)
	e.dialog = win

	title := "New scene"
	if index >= 0 && index < len(a.cfg.Scenes) {
		title = "Edit scene"
	}

	// Hue-style: the window close button dismisses (== cancel), and a single
	// prominent Save sits top-right. No competing text "Cancel" button.
	header := adw.NewHeaderBar()
	header.SetTitleWidget(adw.NewWindowTitle(title, ""))

	save := gtk.NewButtonWithLabel("Save")
	save.AddCSSClass("suggested-action")
	save.ConnectClicked(func() { e.save() })
	header.PackEnd(save)

	// Escape dismisses (cancel); the close button in the title bar does too.
	esc := gtk.NewEventControllerKey()
	esc.ConnectKeyPressed(func(keyval, _ uint, _ gdk.ModifierType) bool {
		if keyval == gdk.KEY_Escape {
			win.Close()
			return true
		}
		return false
	})
	win.AddController(esc)

	// Body: name entry + one editor card per device.
	body := gtk.NewBox(gtk.OrientationVertical, 12)
	body.SetMarginTop(14)
	body.SetMarginBottom(14)
	body.SetMarginStart(14)
	body.SetMarginEnd(14)

	nameGroup := adw.NewPreferencesGroup()
	nameGroup.SetTitle("Name")
	e.name = gtk.NewEntry()
	e.name.SetPlaceholderText("Scene name")
	if index >= 0 && index < len(a.cfg.Scenes) {
		e.name.SetText(a.cfg.Scenes[index].Name)
	}
	nameRow := adw.NewActionRow()
	nameRow.SetChild(e.name)
	nameGroup.Add(nameRow)
	body.Append(nameGroup)

	// Precompute existing states by device_id for prefill.
	existing := map[string]SceneState{}
	included := map[string]bool{}
	if index >= 0 && index < len(a.cfg.Scenes) {
		for _, st := range a.cfg.Scenes[index].States {
			existing[st.DeviceID] = st
			included[st.DeviceID] = true
		}
	}

	for i := range a.cfg.Devices {
		dev := a.cfg.Devices[i]
		row := e.newDeviceRow(dev)
		if st, ok := existing[dev.DeviceID]; ok {
			row.prefill(true, st)
		} else if index < 0 {
			// New scene: include all with sensible defaults.
			row.prefill(true, SceneState{DeviceID: dev.DeviceID, On: true, Mode: dp.ModeColour, Bright: 100})
		} else {
			// Editing: this device isn't in the scene → excluded, defaults.
			row.prefill(false, SceneState{DeviceID: dev.DeviceID, On: true, Mode: dp.ModeColour, Bright: 100})
		}
		e.rows = append(e.rows, row)
		body.Append(row.card)
	}

	clamp := adw.NewClamp()
	clamp.SetMaximumSize(600)
	clamp.SetChild(body)

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	scroll.SetChild(clamp)

	root := gtk.NewBox(gtk.OrientationVertical, 0)
	root.Append(header)
	root.Append(scroll)
	win.SetContent(root)
	win.Present()
}

// newDeviceRow builds one light's editor card (widgets only; prefill loads
// state). Live-preview handlers are wired here and gated by row.suppress so
// prefill doesn't fire commands at the bulb.
func (e *sceneEditor) newDeviceRow(dev Device) *sceneDeviceRow {
	r := &sceneDeviceRow{
		editor:   e,
		ctl:      e.app.byID[dev.DeviceID],
		deviceID: dev.DeviceID,
		st:       SceneState{DeviceID: dev.DeviceID},
	}

	card := gtk.NewBox(gtk.OrientationVertical, 8)
	card.SetMarginTop(6)
	card.SetMarginBottom(10)
	card.SetMarginStart(10)
	card.SetMarginEnd(10)
	card.AddCSSClass("card")

	// The .card class only paints the elevated background; it carries no
	// inner padding, so wrap the content in a padded box or it hugs the
	// rounded edges.
	inner := gtk.NewBox(gtk.OrientationVertical, 8)
	inner.SetMarginTop(12)
	inner.SetMarginBottom(12)
	inner.SetMarginStart(12)
	inner.SetMarginEnd(12)
	card.Append(inner)

	name := dev.Name
	if name == "" {
		name = dev.DeviceID
	}

	// Header row: include checkbox + name (left), power switch (right). A
	// plain box (not AdwActionRow) so the list-row background does not fight
	// the card's own elevated background.
	headerRow := gtk.NewBox(gtk.OrientationHorizontal, 8)
	r.include = gtk.NewCheckButton()
	r.include.SetVAlign(gtk.AlignCenter)
	r.include.SetTooltipText("Include this light in the scene")
	r.include.ConnectToggled(func() {
		if r.suppress {
			return
		}
		r.applySensitivity()
	})
	headerRow.Append(r.include)
	title := gtk.NewLabel(name)
	title.SetXAlign(0.0)
	title.SetHExpand(true)
	title.AddCSSClass("heading")
	headerRow.Append(title)
	r.power = gtk.NewSwitch()
	r.power.SetVAlign(gtk.AlignCenter)
	r.power.ConnectStateSet(func(state bool) bool {
		if r.suppress {
			return false
		}
		r.st.On = state
		r.applySensitivity()
		r.previewPower(state)
		return false
	})
	headerRow.Append(r.power)
	inner.Append(headerRow)

	// Shared colour controls: mode toggle, wheel (thumb = preview),
	// temperature and brightness — the same widget set as the Lights tab.
	r.cc = newColourControls(e.app.wheelSurface, sceneEditorWheelSize, colourCallbacks{
		OnMode: func(bool) {
			if r.suppress {
				return
			}
			r.onModeChanged()
		},
		OnDragBegin: func(rgb dp.RGB) {
			r.syncHS()
			r.ctl.BeginLive(r, rgb, dp.DefaultChangeMode)
		},
		OnDragUpdate: func(rgb dp.RGB) {
			r.syncHS()
			r.ctl.UpdateLive(r, rgb)
		},
		OnDragEnd: func(rgb dp.RGB) {
			r.syncHS()
			r.ctl.UpdateLive(r, rgb)
			go r.ctl.EndLive(r)
		},
		OnBright: func(v float64) {
			if r.suppress {
				return
			}
			r.st.Bright = v
			r.previewBrightness(v)
		},
		OnTemp: func(v float64) {
			if r.suppress {
				return
			}
			r.st.Temp = v
			r.previewTemp(v)
		},
	})
	r.cc.AppendTo(inner)

	r.card = card
	return r
}

// prefill loads a SceneState into the row's widgets without firing preview
// commands (suppress is held while setting values).
func (r *sceneDeviceRow) prefill(include bool, st SceneState) {
	r.suppress = true
	defer func() {
		r.suppress = false
		r.applySensitivity()
		r.cc.ApplyModeVisibility()
	}()

	r.st = st
	r.st.DeviceID = r.deviceID
	r.include.SetActive(include)
	r.power.SetActive(st.On)

	if st.Mode == dp.ModeWhite {
		r.cc.Mode.SetActiveName("white")
	} else {
		r.cc.Mode.SetActiveName("colour")
		if r.st.Mode == "" {
			r.st.Mode = dp.ModeColour
		}
	}

	bright := st.Bright
	if bright < 1 {
		bright = 1
	}
	r.cc.Bright.SetValue(math.Round(bright))
	r.cc.Temp.SetValue(math.Round(st.Temp))
	r.cc.SetHS(st.Hue, st.Sat)
}

// onModeChanged updates st.Mode (visibility is handled by colourControls) and
// pushes a live preview of the newly selected mode.
func (r *sceneDeviceRow) onModeChanged() {
	if r.cc.IsWhite() {
		r.st.Mode = dp.ModeWhite
	} else {
		r.st.Mode = dp.ModeColour
	}

	if !r.include.Active() || !r.st.On {
		return
	}
	if r.st.Mode == dp.ModeWhite {
		r.previewTemp(r.st.Temp)
	} else {
		r.previewColour()
	}
}

// applySensitivity greys out the mode/colour/brightness controls when the light
// is excluded from the scene or powered off, so no stray preview fires.
func (r *sceneDeviceRow) applySensitivity() {
	on := r.include.Active() && r.power.Active()
	r.power.SetSensitive(r.include.Active())
	r.cc.SetSensitive(on)
}

// --- live preview helpers (all off the GTK thread via the control) ---

func (r *sceneDeviceRow) previewPower(on bool) {
	ctl := r.ctl
	ctl.async("power", func(ctx context.Context) error { return ctl.SetPower(ctx, on) })
}

func (r *sceneDeviceRow) previewColour() {
	ctl, rgb := r.ctl, r.cc.SelRGB()
	ctl.async("colour", func(ctx context.Context) error { return ctl.SetColour(ctx, rgb) })
}

func (r *sceneDeviceRow) previewBrightness(v float64) {
	ctl := r.ctl
	// In colour mode brightness is the colour's "v": rewrite the current
	// selection with the new value in one write. In white mode it is the
	// dedicated brightness DP.
	if r.st.Mode == dp.ModeColour {
		rr, gg, bb := hsvToRGBInt(r.st.Hue, r.st.Sat, v/100.0)
		ctl.async("colour", func(ctx context.Context) error { return ctl.SetColour(ctx, dp.RGB{R: rr, G: gg, B: bb}) })
		return
	}
	ctl.async("brightness", func(ctx context.Context) error { return ctl.SetWhiteBrightness(ctx, v) })
}

func (r *sceneDeviceRow) previewTemp(v float64) {
	ctl := r.ctl
	ctl.async("temperature", func(ctx context.Context) error { return ctl.SetColourTempPercent(ctx, v) })
}

// syncHS mirrors the shared widget's wheel selection into the in-memory
// scene state so Save persists what was previewed.
func (r *sceneDeviceRow) syncHS() {
	r.st.Hue = r.cc.hue
	r.st.Sat = r.cc.sat
}

// save builds the scene from the included rows, writes it into a.cfg.Scenes
// (append or overwrite), persists, refreshes the list, and closes the dialog.
func (e *sceneEditor) save() {
	name := e.name.Text()
	if name == "" {
		e.app.scenesStatus.SetLabel("The scene needs a name")
		return
	}

	scene := Scene{Name: name}
	for _, r := range e.rows {
		if !r.include.Active() {
			continue
		}
		st := SceneState{DeviceID: r.deviceID, On: r.power.Active()}
		if st.On {
			if r.st.Mode == dp.ModeWhite {
				st.Mode = dp.ModeWhite
				st.Temp = r.cc.Temp.Value()
				st.Bright = r.cc.Bright.Value()
			} else {
				st.Mode = dp.ModeColour
				st.Hue = r.st.Hue
				st.Sat = r.st.Sat
				st.Bright = r.cc.Bright.Value()
			}
		}
		scene.States = append(scene.States, st)
	}

	if len(scene.States) == 0 {
		e.app.scenesStatus.SetLabel("Include at least one light")
		return
	}

	if e.index >= 0 && e.index < len(e.app.cfg.Scenes) {
		prev := e.app.cfg.Scenes[e.index]
		e.app.cfg.Scenes[e.index] = scene
		if err := e.app.saveCfg(); err != nil {
			e.app.cfg.Scenes[e.index] = prev
			e.app.scenesStatus.SetLabel("Save failed: " + err.Error())
			return
		}
	} else {
		e.app.cfg.Scenes = append(e.app.cfg.Scenes, scene)
		if err := e.app.saveCfg(); err != nil {
			e.app.cfg.Scenes = e.app.cfg.Scenes[:len(e.app.cfg.Scenes)-1]
			e.app.scenesStatus.SetLabel("Save failed: " + err.Error())
			return
		}
	}

	e.app.refreshScenesList()
	e.app.scenesStatus.SetLabel("Scene “" + name + "” saved")
	e.dialog.Close()
}
