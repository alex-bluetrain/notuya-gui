package main

import (
	"context"
	"math"

	"github.com/averstraeten/notuya-go/pkg/device"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/cairo"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// sceneEditorWheelSize is the diameter of the per-light colour wheel inside the
// scene editor. Matches the device panel's wheel for a consistent feel.
const sceneEditorWheelSize = 180

// sceneEditor is the modal dialog that edits a scene's snapshot with live
// preview: moving a wheel or slider drives the real bulb (via the shared
// control), and each change is also written into the in-memory SceneState so
// Guardar persists exactly what was previewed. The preview is destructive —
// closing the dialog leaves the lights at their last previewed value, matching
// the picker and the Luces tab.
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

	card      *gtk.Box
	include   *gtk.CheckButton
	power     *gtk.Switch
	modeCombo *adw.ToggleGroup
	wheelRow  *gtk.Box
	wheel     *gtk.DrawingArea
	swatch    *gtk.DrawingArea
	bright    *gtk.Scale
	tempRow   *gtk.Box
	temp      *gtk.Scale

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
			row.prefill(true, SceneState{DeviceID: dev.DeviceID, On: true, Mode: device.ModeColour, Bright: 100})
		} else {
			// Editing: this device isn't in the scene → excluded, defaults.
			row.prefill(false, SceneState{DeviceID: dev.DeviceID, On: true, Mode: device.ModeColour, Bright: 100})
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

	// Mode selector: Color / Blanco.
	modeRow := gtk.NewBox(gtk.OrientationHorizontal, 8)
	modeLabel := gtk.NewLabel("Mode")
	modeLabel.SetWidthChars(16)
	modeLabel.SetXAlign(0.0)
	modeRow.Append(modeLabel)
	r.modeCombo = newModeToggle(func(bool) {
		if r.suppress {
			return
		}
		r.onModeChanged()
	})
	modeRow.Append(r.modeCombo)
	inner.Append(modeRow)

	// Colour wheel + swatch (colour mode).
	r.wheelRow = gtk.NewBox(gtk.OrientationHorizontal, 10)
	r.wheel = gtk.NewDrawingArea()
	r.wheel.SetContentWidth(sceneEditorWheelSize)
	r.wheel.SetContentHeight(sceneEditorWheelSize)
	r.wheel.SetDrawFunc(r.drawWheel)
	r.wheelRow.Append(r.wheel)

	swatchBox := gtk.NewBox(gtk.OrientationVertical, 6)
	swatchBox.SetVAlign(gtk.AlignCenter)
	r.swatch = gtk.NewDrawingArea()
	r.swatch.SetContentWidth(60)
	r.swatch.SetContentHeight(60)
	r.swatch.SetDrawFunc(r.drawSwatch)
	swatchBox.Append(r.swatch)
	r.wheelRow.Append(swatchBox)
	inner.Append(r.wheelRow)

	// Wheel drag → live preview + in-memory hue/sat.
	drag := gtk.NewGestureDrag()
	var startX, startY float64
	drag.ConnectDragBegin(func(x, y float64) {
		startX, startY = x, y
		r.setSelection(x, y)
		r.ctl.BeginLiveDrag(r.selRGB(), device.DefaultTransition)
	})
	drag.ConnectDragUpdate(func(ox, oy float64) {
		r.setSelection(startX+ox, startY+oy)
		r.ctl.UpdateLiveDrag(r.selRGB())
	})
	drag.ConnectDragEnd(func(ox, oy float64) {
		r.setSelection(startX+ox, startY+oy)
		r.ctl.UpdateLiveDrag(r.selRGB())
		go r.ctl.EndLiveDrag()
	})
	r.wheel.AddController(drag)

	// Brightness slider.
	inner.Append(labelledScaleSimple("Brightness", &r.bright, 1, 100, func(v float64) {
		if r.suppress {
			return
		}
		r.st.Bright = v
		r.previewBrightness(v)
	}))

	// Temperature slider (white mode).
	r.tempRow = labelledScaleSimple("Temp (cold→warm)", &r.temp, 0, 100, func(v float64) {
		if r.suppress {
			return
		}
		r.st.Temp = v
		r.previewTemp(v)
	})
	inner.Append(r.tempRow)

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
		r.applyModeVisibility()
	}()

	r.st = st
	r.st.DeviceID = r.deviceID
	r.include.SetActive(include)
	r.power.SetActive(st.On)

	if st.Mode == device.ModeWhite {
		r.modeCombo.SetActiveName("white")
	} else {
		r.modeCombo.SetActiveName("colour")
		if r.st.Mode == "" {
			r.st.Mode = device.ModeColour
		}
	}

	bright := st.Bright
	if bright < 1 {
		bright = 1
	}
	r.bright.SetValue(math.Round(bright))
	r.temp.SetValue(math.Round(st.Temp))
	r.wheel.QueueDraw()
	r.swatch.QueueDraw()
}

// onModeChanged updates st.Mode, toggles which controls are visible, and pushes
// a live preview of the newly selected mode.
func (r *sceneDeviceRow) onModeChanged() {
	if r.modeCombo.ActiveName() == "white" {
		r.st.Mode = device.ModeWhite
	} else {
		r.st.Mode = device.ModeColour
	}
	r.applyModeVisibility()

	if !r.include.Active() || !r.st.On {
		return
	}
	if r.st.Mode == device.ModeWhite {
		r.previewTemp(r.st.Temp)
	} else {
		r.previewColour()
	}
}

// applyModeVisibility shows the wheel for colour mode and the temp slider for
// white mode.
func (r *sceneDeviceRow) applyModeVisibility() {
	colour := r.st.Mode != device.ModeWhite
	r.wheelRow.SetVisible(colour)
	r.tempRow.SetVisible(!colour)
}

// applySensitivity greys out the mode/colour/brightness controls when the light
// is excluded from the scene or powered off, so no stray preview fires.
func (r *sceneDeviceRow) applySensitivity() {
	on := r.include.Active() && r.power.Active()
	r.power.SetSensitive(r.include.Active())
	r.modeCombo.SetSensitive(on)
	r.wheelRow.SetSensitive(on)
	r.bright.SetSensitive(on)
	r.temp.SetSensitive(on)
}

// --- live preview helpers (all off the GTK thread via the control) ---

func (r *sceneDeviceRow) previewPower(on bool) {
	ctl := r.ctl
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		_ = ctl.SetPower(ctx, on)
	}()
}

func (r *sceneDeviceRow) previewColour() {
	ctl, rgb := r.ctl, r.selRGB()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		_ = ctl.SetColour(ctx, rgb)
	}()
}

func (r *sceneDeviceRow) previewBrightness(v float64) {
	ctl := r.ctl
	// In colour mode brightness is the colour's "v": rewrite the current
	// selection with the new value in one write. In white mode it is the
	// dedicated brightness DP.
	if r.st.Mode == device.ModeColour {
		rr, gg, bb := hsvToRGBInt(r.st.Hue, r.st.Sat, v/100.0)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
			defer cancel()
			_ = ctl.SetColour(ctx, device.RGB{R: rr, G: gg, B: bb})
		}()
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		_ = ctl.SetWhiteBrightness(ctx, v)
	}()
}

func (r *sceneDeviceRow) previewTemp(v float64) {
	ctl := r.ctl
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		_ = ctl.SetColourTempPercent(ctx, v)
	}()
}

// setSelection updates hue/sat from wheel coordinates, mirrors them into the
// in-memory state, and redraws.
func (r *sceneDeviceRow) setSelection(x, y float64) {
	h, s := coordsToHSSized(x, y, sceneEditorWheelSize)
	r.st.Hue = h
	r.st.Sat = s
	r.wheel.QueueDraw()
	r.swatch.QueueDraw()
}

func (r *sceneDeviceRow) selRGB() device.RGB {
	rr, gg, bb := hsvToRGBInt(r.st.Hue, r.st.Sat, 1.0)
	return device.RGB{R: rr, G: gg, B: bb}
}

func (r *sceneDeviceRow) drawWheel(_ *gtk.DrawingArea, cr *cairo.Context, width, height int) {
	scale := float64(sceneEditorWheelSize) / float64(wheelSize)
	cr.Save()
	cr.Scale(scale, scale)
	cr.SetSourceSurface(r.editor.app.wheelSurface, 0, 0)
	cr.Paint()
	cr.Restore()

	radius := float64(sceneEditorWheelSize) / 2.0
	angle := r.st.Hue * 2 * math.Pi
	dist := r.st.Sat * radius
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

func (r *sceneDeviceRow) drawSwatch(_ *gtk.DrawingArea, cr *cairo.Context, width, height int) {
	rr, gg, bb := hsvToRGBInt(r.st.Hue, r.st.Sat, 1.0)
	w, h := float64(width), float64(height)
	roundedRect(cr, 0, 0, w, h, 8)
	cr.SetSourceRGB(float64(rr)/255, float64(gg)/255, float64(bb)/255)
	cr.Fill()
	roundedRect(cr, 0.5, 0.5, w-1, h-1, 8)
	cr.SetSourceRGBA(0, 0, 0, 0.15)
	cr.SetLineWidth(1)
	cr.Stroke()
}

// labelledScaleSimple builds a "label + horizontal scale" row and stores the
// scale in *dst. Unlike labelledScale it has no devicePanel dependency; the
// caller's onChange guards against programmatic changes itself.
func labelledScaleSimple(label string, dst **gtk.Scale, min, max float64, onChange func(float64)) *gtk.Box {
	row := gtk.NewBox(gtk.OrientationHorizontal, 8)
	lbl := gtk.NewLabel(label)
	lbl.SetWidthChars(16)
	lbl.SetXAlign(0.0)
	row.Append(lbl)
	scale := gtk.NewScaleWithRange(gtk.OrientationHorizontal, min, max, 1)
	scale.SetHExpand(true)
	scale.SetDrawValue(true)
	scale.SetRoundDigits(0)
	scale.ConnectValueChanged(func() { onChange(scale.Value()) })
	disableScaleScroll(scale)
	row.Append(scale)
	*dst = scale
	return row
}

// newModeToggle builds an AdwToggleGroup with two icon+label toggles —
// "colour" (colour wheel) and "white" (colour temperature) — for the
// Colour/White mode selector shared by the scene editor and the Lights
// playground. onChange fires with isWhite = true when White becomes active.
// The returned group's active toggle is set programmatically with
// SetActiveName("colour"|"white"); guard onChange against those with a
// suppress flag in the caller.
func newModeToggle(onChange func(isWhite bool)) *adw.ToggleGroup {
	group := adw.NewToggleGroup()
	group.SetHExpand(true)

	colour := adw.NewToggle()
	colour.SetName("colour")
	colour.SetLabel("Colour")
	colour.SetIconName("color-select-symbolic")
	group.Add(colour)

	white := adw.NewToggle()
	white.SetName("white")
	white.SetLabel("White")
	white.SetIconName("weather-clear-symbolic")
	group.Add(white)

	group.NotifyProperty("active-name", func() {
		onChange(group.ActiveName() == "white")
	})
	return group
}

// newTransitionToggle builds an AdwToggleGroup with two icon+label toggles —
// "jump" (instant snap) and "fade" (gradual) — for DP 28's change mode. onChange
// fires with isFade = true when Fade becomes active. Set the active toggle
// programmatically with SetActiveName("jump"|"fade"); guard onChange with a
// suppress flag in the caller.
func newTransitionToggle(onChange func(isFade bool)) *adw.ToggleGroup {
	group := adw.NewToggleGroup()
	group.SetHExpand(true)

	jump := adw.NewToggle()
	jump.SetName("jump")
	jump.SetLabel("Instant")
	group.Add(jump)

	fade := adw.NewToggle()
	fade.SetName("fade")
	fade.SetLabel("Smooth")
	group.Add(fade)

	group.NotifyProperty("active-name", func() {
		onChange(group.ActiveName() == "fade")
	})
	return group
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
			if r.st.Mode == device.ModeWhite {
				st.Mode = device.ModeWhite
				st.Temp = r.temp.Value()
				st.Bright = r.bright.Value()
			} else {
				st.Mode = device.ModeColour
				st.Hue = r.st.Hue
				st.Sat = r.st.Sat
				st.Bright = r.bright.Value()
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
