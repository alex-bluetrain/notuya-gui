package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/cairo"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	coreglib "github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"

	"github.com/alex-bluetrain/notuya-gui/internal/colour"
)

// newPreset is the editor index of a preset that isn't in the list yet.
const newPreset = -2

// presetEditor is the one window that creates and edits a Screen Sync
// preset: name, target (picked only via Select), then region cards and an
// Edit regions button that opens the overlay. It edits a draft; Save writes it back, Cancel drops it.
// Once the draft has a target the sync previews it live.
type presetEditor struct {
	t        *syncTab
	index    int // preset index, newPreset until saved
	draft    SyncPreset
	win      *adw.Window
	target   *gtk.Label
	selectB  *gtk.Button
	editB    *gtk.Button
	grid     *gtk.FlowBox
	rate     *gtk.Label
	rateTick coreglib.SourceHandle

	swatches   []*regionSwatch
	saved      bool
	owns       bool // the editor started the sync, so closing it stops it
	retargeted bool // the sync runs on a target that isn't saved yet
}

// openPreset opens the editor on preset i, or on a blank draft for
// newPreset.
func (t *syncTab) openPreset(i int) {
	if t.editor != nil {
		t.editor.win.Present()
		return
	}
	e := &presetEditor{t: t, index: i}
	if i == newPreset {
		e.draft = SyncPreset{ID: newSyncID(), Name: t.defaultPresetName()}
	} else {
		p := t.presets()[i]
		e.draft = p
		e.draft.Target.Monitors = slices.Clone(p.Target.Monitors)
		e.draft.Regions = make([]SyncRegion, len(p.Regions))
		for ri, r := range p.Regions {
			r.Devices = slices.Clone(r.Devices)
			e.draft.Regions[ri] = r
		}
	}
	t.editor = e

	win := adw.NewWindow()
	win.SetModal(true)
	win.SetTransientFor(&t.a.window.Window)
	win.SetDefaultSize(560, 680)
	e.win = win

	title := "Edit preset"
	if i == newPreset {
		title = "New preset"
	}
	header := adw.NewHeaderBar()
	header.SetTitleWidget(adw.NewWindowTitle(title, ""))
	header.SetShowStartTitleButtons(false)
	header.SetShowEndTitleButtons(false)
	cancel := gtk.NewButtonWithLabel("Cancel")
	cancel.ConnectClicked(win.Close)
	header.PackStart(cancel)
	save := gtk.NewButtonWithLabel("Save")
	save.AddCSSClass("suggested-action")
	save.ConnectClicked(e.save)
	header.PackEnd(save)

	esc := gtk.NewEventControllerKey()
	esc.SetPropagationPhase(gtk.PhaseCapture)
	esc.ConnectKeyPressed(func(keyval, _ uint, _ gdk.ModifierType) bool {
		if keyval == gdk.KEY_Escape {
			win.Close()
			return true
		}
		return false
	})
	win.AddController(esc)
	win.ConnectCloseRequest(func() bool {
		e.closed()
		return false
	})

	body := gtk.NewBox(gtk.OrientationVertical, 18)
	body.SetMarginTop(14)
	body.SetMarginBottom(14)
	body.SetMarginStart(14)
	body.SetMarginEnd(14)

	nameGroup := adw.NewPreferencesGroup()
	nameGroup.SetTitle("Name")
	name := gtk.NewEntry()
	name.SetText(e.draft.Name)
	name.SetPlaceholderText("Preset name")
	setA11yLabel(&name.Widget, "Preset name")
	name.ConnectChanged(func() { e.draft.Name = name.Text() })
	nameGroup.Add(name)
	body.Append(nameGroup)

	targetGroup := adw.NewPreferencesGroup()
	targetGroup.SetTitle("Target")
	e.target = gtk.NewLabel("")
	e.target.SetXAlign(0)
	e.target.SetHExpand(true)
	e.target.SetEllipsize(pango.EllipsizeEnd)
	e.selectB = gtk.NewButton()
	e.selectB.SetSensitive(t.tracker != nil)
	e.selectB.ConnectClicked(e.selectTarget)
	targetLine := gtk.NewBox(gtk.OrientationHorizontal, 12)
	targetLine.Append(e.target)
	targetLine.Append(e.selectB)
	targetGroup.Add(targetLine)
	body.Append(targetGroup)

	regions := adw.NewPreferencesGroup()
	regions.SetTitle("Regions")
	regions.SetDescription("Each light follows at most one region")
	e.grid = gtk.NewFlowBox()
	e.grid.SetSelectionMode(gtk.SelectionNone)
	e.grid.SetRowSpacing(12)
	e.grid.SetMinChildrenPerLine(1)
	e.grid.SetMaxChildrenPerLine(1)
	e.grid.SetVAlign(gtk.AlignStart)
	regions.Add(e.grid)
	e.editB = gtk.NewButtonWithLabel("Edit regions")
	e.editB.AddCSSClass("pill")
	e.editB.SetHAlign(gtk.AlignCenter)
	e.editB.SetMarginTop(6)
	e.editB.ConnectClicked(func() { t.beginEdit(e.index) })
	regions.Add(e.editB)
	body.Append(regions)

	e.rate = gtk.NewLabel("")
	e.rate.AddCSSClass("dim-label")
	e.rate.AddCSSClass("caption")
	e.rate.SetXAlign(0)
	body.Append(e.rate)
	e.startRate()

	clamp := adw.NewClamp()
	clamp.SetMaximumSize(640)
	clamp.SetChild(body)
	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	scroll.SetChild(clamp)

	root := gtk.NewBox(gtk.OrientationVertical, 0)
	root.Append(header)
	root.Append(scroll)
	win.SetContent(root)

	e.showTarget()
	e.rebuildRegions()
	win.Present()

	// Preview live once there is a target; nothing opens on its own.
	if e.draft.Target.Kind != "" && t.active != i {
		e.owns = true
		t.start(i)
	}
}

// defaultPresetName is "Preset N" for the first N not already taken.
func (t *syncTab) defaultPresetName() string {
	for n := len(t.presets()) + 1; ; n++ {
		name := fmt.Sprintf("Preset %d", n)
		if !slices.ContainsFunc(t.presets(), func(p SyncPreset) bool { return p.Name == name }) {
			return name
		}
	}
}

// showTarget reflects the draft's target in the Target row, the Select
// button and the Edit regions button.
func (e *presetEditor) showTarget() {
	e.target.SetText(targetLabel(e.draft.Target))
	if e.draft.Target.Kind == "" {
		e.selectB.SetLabel("Select")
	} else {
		e.selectB.SetLabel("Change")
	}
	switch {
	case e.draft.Target.Kind == "":
		e.editB.SetSensitive(false)
		e.editB.SetTooltipText("Select a target first")
	case !layerShellSupported():
		e.editB.SetSensitive(false)
		e.editB.SetTooltipText("Drawing regions needs layer-shell support")
	default:
		e.editB.SetSensitive(true)
		e.editB.SetTooltipText("Draw, move and resize regions over the target")
	}
}

// selectTarget opens the system share dialog; the pick becomes the draft's
// target and the preview switches to it.
func (e *presetEditor) selectTarget() {
	t := e.t
	if t.starting {
		return
	}
	if t.active == e.index {
		t.stop("")
	} else {
		e.owns = true
	}
	t.run(e.index, true)
}

// save writes the draft into the config.
func (e *presetEditor) save() {
	t := e.t
	e.draft.Name = strings.TrimSpace(e.draft.Name)
	if e.draft.Name == "" {
		e.draft.Name = t.defaultPresetName()
	}
	e.saved = true
	if e.index == newPreset {
		// The preview of an unsaved preset is keyed by newPreset; it ends
		// with the editor.
		if t.active == newPreset {
			t.stop("")
		}
		t.a.cfg.ScreenSync.Presets = append(t.a.cfg.ScreenSync.Presets, e.draft)
		t.saveSoon()
	} else {
		t.a.cfg.ScreenSync.Presets[e.index] = e.draft
		t.regionsChanged(e.index)
	}
	e.win.Close()
}

// closed tears the editor down. Without Save a running sync goes back to
// the stored preset, or stops if it was showing an unsaved target.
func (e *presetEditor) closed() {
	t := e.t
	t.editor = nil
	coreglib.SourceRemove(e.rateTick)
	if t.editTarget == e.index {
		t.overlay.finish()
	}
	switch {
	case t.active != e.index:
	case e.owns, !e.saved && e.retargeted:
		t.stop("")
	case !e.saved && t.engine != nil:
		t.engine.SetRegions(t.engineRegions(e.index))
		t.updateLock()
	}
	t.rebuildList()
}

// rebuildRegions lays out one card per draft region.
func (e *presetEditor) rebuildRegions() {
	e.grid.RemoveAll()
	e.swatches = nil
	for ri := range e.draft.Regions {
		e.grid.Insert(e.regionCard(ri), -1)
	}
}

// regionCard is one region: swatch, name, delete, and
// a chip per light.
func (e *presetEditor) regionCard(ri int) gtk.Widgetter {
	t := e.t
	p := &e.draft
	r := &p.Regions[ri]

	sw := &regionSwatch{da: gtk.NewDrawingArea()}
	sw.da.SetContentWidth(28)
	sw.da.SetContentHeight(28)
	sw.da.SetDrawFunc(func(_ *gtk.DrawingArea, cr *cairo.Context, w, h int) {
		roundedRect(cr, 0, 0, float64(w), float64(h), 7)
		if sw.set {
			rgb := colour.ToRGB(sw.c)
			cr.SetSourceRGB(float64(rgb.R)/255, float64(rgb.G)/255, float64(rgb.B)/255)
		} else {
			cr.SetSourceRGBA(0.5, 0.5, 0.5, 0.25)
		}
		cr.Fill()
	})
	e.swatches = append(e.swatches, sw)
	sw.da.SetVAlign(gtk.AlignCenter)

	name := gtk.NewEntry()
	name.SetText(r.Name)
	name.SetHExpand(true)
	name.SetVAlign(gtk.AlignCenter)
	setA11yLabel(&name.Widget, "Region name")
	name.ConnectChanged(func() {
		r.Name = name.Text()
		t.pushOverlayRegions()
	})

	del := gtk.NewButtonFromIconName("user-trash-symbolic")
	del.AddCSSClass("flat")
	del.SetVAlign(gtk.AlignCenter)
	del.SetTooltipText("Delete region")
	setA11yLabel(&del.Widget, "Delete region "+r.Name)
	del.ConnectClicked(func() { e.deleteRegion(ri) })

	top := gtk.NewBox(gtk.OrientationHorizontal, 8)
	top.Append(sw.da)
	top.Append(name)
	top.Append(del)

	chips := gtk.NewFlowBox()
	chips.SetSelectionMode(gtk.SelectionNone)
	chips.SetColumnSpacing(6)
	chips.SetRowSpacing(6)
	chips.SetMaxChildrenPerLine(8)
	for _, d := range t.a.cfg.Devices {
		id := d.DeviceID
		c := gtk.NewToggleButtonWithLabel(t.deviceName(id))
		c.AddCSSClass("light-chip")
		setA11yLabel(&c.Widget, t.deviceName(id)+" in "+r.Name)
		if owner := regionOf(p, id); owner >= 0 && owner != ri {
			c.SetTooltipText("In " + p.Regions[owner].Name + " — turning it on here moves it")
		}
		c.SetActive(slices.Contains(r.Devices, id))
		c.ConnectToggled(func() {
			if c.Active() == slices.Contains(r.Devices, id) {
				return
			}
			if c.Active() {
				p.bind(ri, id)
			} else {
				r.Devices = slices.DeleteFunc(r.Devices, func(x string) bool { return x == id })
			}
			e.preview()
			// Defer the rebuild: this handler's widget is in the grid, and
			// other cards may have lost the light.
			coreglib.IdleAdd(e.rebuildRegions)
		})
		chips.Insert(c, -1)
	}

	card := gtk.NewBox(gtk.OrientationVertical, 10)
	card.AddCSSClass("card")
	card.AddCSSClass("region-card")
	card.Append(top)
	if len(t.a.cfg.Devices) == 0 {
		none := gtk.NewLabel("No lights configured")
		none.AddCSSClass("dim-label")
		none.SetXAlign(0)
		card.Append(none)
	} else {
		card.Append(chips)
	}
	return card
}

func (e *presetEditor) deleteRegion(ri int) {
	if ri < 0 || ri >= len(e.draft.Regions) {
		return
	}
	e.draft.Regions = slices.Delete(e.draft.Regions, ri, ri+1)
	e.preview()
	e.t.pushOverlayRegions()
	coreglib.IdleAdd(e.rebuildRegions)
}

// preview pushes the draft's regions to a running sync of this preset.
func (e *presetEditor) preview() {
	t := e.t
	if t.engine != nil && t.active == e.index {
		t.engine.SetRegions(t.engineRegions(e.index))
		t.updateLock()
	}
}

// startRate shows, once a second, how many capture frames arrived and how
// many messages went out to the bulbs.
func (e *presetEditor) startRate() {
	frames, sends := captureFrames.Load(), liveSends.Load()
	update := func() bool {
		f, s := captureFrames.Load(), liveSends.Load()
		if e.t.active == e.index {
			e.rate.SetText(fmt.Sprintf("Capture %d fps · sent %d/s", f-frames, s-sends))
		} else {
			e.rate.SetText("Live rate appears while the preview runs")
		}
		frames, sends = f, s
		return true
	}
	update()
	e.rateTick = coreglib.TimeoutAdd(1000, update)
}
