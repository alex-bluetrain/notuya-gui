package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/alex-bluetrain/notuya-go/pkg/dp"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/cairo"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	coreglib "github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"

	"github.com/alex-bluetrain/notuya-gui/internal/screensync"
)

const (
	syncSaveDelay = 500 // ms: edits are saved this long after the last one
	syncEditPoll  = 500 * time.Millisecond
)

// syncTab is the Screen Sync tab: a grid of preset tiles (click to sync),
// one page per preset with its regions, and the overlay that edits them over the target. One
// preset syncs at a time.
type syncTab struct {
	a       *desktopApp
	nav     *adw.NavigationView
	banner  *adw.Banner
	tracker screensync.Tracker // nil: no Hyprland

	flow      *gtk.FlowBox
	tiles     []*gtk.ToggleButton
	suppress  bool
	css       *gtk.CSSProvider
	tileCSS   string    // per-tile idle fills, rebuilt with the grid
	lastPaint time.Time // last live repaint of the running tile

	// The open preset page (nil when on the list).
	page       *adw.NavigationPage
	pageIdx    int
	regionsBox *gtk.Box
	regionList gtk.Widgetter
	swatches   []*regionSwatch

	// The running sync. active is the preset index, -1 when idle.
	active   int
	engine   *screensync.Engine
	capture  screensync.Capture
	stopSync context.CancelFunc // stops watching the target
	starting bool

	overlay    *syncOverlay
	stopEdit   context.CancelFunc
	editTarget int  // preset being edited, -1 when not editing
	editOwns   bool // editing started the sync, so leaving it stops it

	saveTimer coreglib.SourceHandle
}

type regionSwatch struct {
	da  *gtk.DrawingArea
	rgb dp.RGB
	set bool
}

func (a *desktopApp) buildScreenSyncTab() gtk.Widgetter {
	if a.cfg.ScreenSync == nil {
		a.cfg.ScreenSync = &ScreenSync{Brightness: 1}
	}
	if a.cfg.ScreenSync.Brightness == 0 {
		a.cfg.ScreenSync.Brightness = 1
	}
	t := &syncTab{a: a, active: -1, pageIdx: -1, editTarget: -1, overlay: newSyncOverlay()}
	a.sync = t
	t.tracker, _ = screensync.NewTracker()

	t.banner = adw.NewBanner("")

	// Tiles reuse the Scenes tile classes; this provider adds each preset's
	// fill and the running tile's live colours.
	t.css = gtk.NewCSSProvider()
	if disp := gdk.DisplayGetDefault(); disp != nil {
		gtk.StyleContextAddProviderForDisplay(disp, t.css, uint(gtk.STYLE_PROVIDER_PRIORITY_APPLICATION))
	}

	box := gtk.NewBox(gtk.OrientationVertical, 12)
	box.SetMarginTop(14)
	box.SetMarginBottom(14)
	box.SetMarginStart(14)
	box.SetMarginEnd(14)

	title := gtk.NewLabel("Screen Sync")
	title.AddCSSClass("title-2")
	title.SetXAlign(0)

	t.flow = gtk.NewFlowBox()
	t.flow.AddCSSClass("scenes-flow")
	t.flow.SetSelectionMode(gtk.SelectionNone)
	t.flow.SetHomogeneous(true)
	t.flow.SetColumnSpacing(12)
	t.flow.SetRowSpacing(12)
	t.flow.SetMinChildrenPerLine(2)
	t.flow.SetMaxChildrenPerLine(3)
	t.flow.SetVAlign(gtk.AlignStart)

	bright := adw.NewPreferencesGroup()
	bright.SetTitle("Brightness")
	bright.SetDescription("Scales every region's colour while syncing")
	scale := gtk.NewScaleWithRange(gtk.OrientationHorizontal, 1, 200, 5)
	scale.SetValue(a.cfg.ScreenSync.Brightness * 100)
	scale.AddMark(100, gtk.PosBottom, "")
	scale.SetFormatValueFunc(func(_ *gtk.Scale, v float64) string { return fmt.Sprintf("%d %%", int(v+0.5)) })
	scale.SetDrawValue(true)
	scale.ConnectValueChanged(func() {
		f := scale.Value() / 100
		a.cfg.ScreenSync.Brightness = f
		if t.engine != nil {
			t.engine.SetBrightness(f)
		}
		t.saveSoon()
	})
	bright.Add(scale)
	bright.SetMarginTop(12)

	box.Append(title)
	box.Append(t.flow)
	box.Append(bright)

	clamp := adw.NewClamp()
	clamp.SetMaximumSize(640)
	clamp.SetChild(box)
	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	scroll.SetChild(clamp)

	root := gtk.NewBox(gtk.OrientationVertical, 0)
	root.Append(t.banner)
	root.Append(scroll)

	t.nav = adw.NewNavigationView()
	t.nav.Add(adw.NewNavigationPage(root, "Screen Sync"))
	t.nav.ConnectPopped(func(*adw.NavigationPage) {
		t.page, t.pageIdx, t.swatches = nil, -1, nil
		t.rebuildList()
	})

	t.rebuildList()

	switch {
	case t.tracker == nil:
		t.showBanner("Screen Sync requires Hyprland")
	case !layerShellSupported():
		t.showBanner("Region editing needs layer-shell support")
	}
	return t.nav
}

func (t *syncTab) presets() []SyncPreset { return t.a.cfg.ScreenSync.Presets }

func (t *syncTab) showBanner(msg string) {
	t.banner.SetTitle(msg)
	t.banner.SetRevealed(msg != "")
}

// saveSoon writes the config once edits pause.
func (t *syncTab) saveSoon() {
	if t.saveTimer != 0 {
		coreglib.SourceRemove(t.saveTimer)
	}
	t.saveTimer = coreglib.TimeoutAdd(syncSaveDelay, func() bool {
		t.saveTimer = 0
		if err := t.a.saveCfg(); err != nil {
			fmt.Fprintf(os.Stderr, "notuya-gui: save: %v\n", err)
		}
		return false
	})
}

// flushSave writes a pending save now (on quit).
func (t *syncTab) flushSave() {
	if t.saveTimer != 0 {
		coreglib.SourceRemove(t.saveTimer)
		t.saveTimer = 0
		if err := t.a.saveCfg(); err != nil {
			fmt.Fprintf(os.Stderr, "notuya-gui: save: %v\n", err)
		}
	}
}

func targetLabel(tg SyncTarget) string {
	if tg.Kind == screensync.TargetWindow {
		s := "Window · " + tg.WindowClass
		if tg.TitleMatch != "" {
			s += " (“" + tg.TitleMatch + "”)"
		}
		return s
	}
	return "Monitors · " + strings.Join(tg.Monitors, " + ")
}

func screensyncTarget(tg SyncTarget) screensync.Target {
	return screensync.Target{Kind: tg.Kind, Monitors: tg.Monitors, WindowClass: tg.WindowClass, TitleMatch: tg.TitleMatch}
}

// --- preset grid ---

// presetTileCSS is what the Scenes tile classes lack: the subtitle, the
// accent ring kept on the running tile, and its "Live" badge.
const presetTileCSS = `
.sync-tile-sub {
  margin: 0 12px 10px 12px;
  color: alpha(#ffffff, 0.85);
  font-size: smaller;
  text-shadow: 0 1px 3px alpha(#000, 0.75);
}
.sync-tile-name {
  margin-bottom: 0;
}
.scene-tile:checked {
  outline-color: @accent_color;
}
.sync-tile-live {
  margin: 8px;
  padding: 1px 8px;
  border-radius: 999px;
  color: #ffffff;
  background-color: alpha(#000, 0.45);
  font-size: smaller;
  font-weight: bold;
}
`

// rebuildList rebuilds the preset grid: one tile per preset, then a dashed
// "add" tile.
func (t *syncTab) rebuildList() {
	t.flow.RemoveAll()
	t.tiles = nil
	var css strings.Builder
	css.WriteString(presetTileCSS)
	for i, p := range t.presets() {
		class := fmt.Sprintf("sync-tile-%d", i)
		css.WriteString(presetIdleCSS(class, p.ID))
		t.flow.Insert(t.buildPresetTile(i, p, class), -1)
	}
	t.tileCSS = css.String()
	t.css.LoadFromString(t.tileCSS)

	add := gtk.NewButton()
	add.AddCSSClass("flat")
	add.AddCSSClass("scene-add")
	add.SetHExpand(true)
	add.SetTooltipText("New preset")
	setA11yLabel(&add.Widget, "Add preset…")
	add.SetSensitive(t.tracker != nil)
	add.ConnectClicked(t.addPresetDialog)
	icon := gtk.NewImageFromIconName("list-add-symbolic")
	icon.SetPixelSize(28)
	add.SetChild(icon)
	t.flow.Insert(add, -1)
	t.syncSwitches()
}

// buildPresetTile is one preset card: clicking it starts or stops the sync;
// edit and delete sit in the corner as on scene tiles.
func (t *syncTab) buildPresetTile(i int, p SyncPreset, class string) *gtk.Overlay {
	tile := gtk.NewToggleButton()
	tile.AddCSSClass("flat")
	tile.AddCSSClass("scene-tile")
	tile.AddCSSClass(class)
	tile.SetHExpand(true)
	tile.SetSensitive(t.tracker != nil)
	tile.SetTooltipText("Start or stop syncing the lights to this preset")
	setA11yLabel(&tile.Widget, p.Name)
	tile.ConnectToggled(func() {
		if !t.suppress {
			t.toggle(i, tile.Active())
		}
	})

	name := gtk.NewLabel(p.Name)
	name.AddCSSClass("scene-tile-name")
	name.AddCSSClass("sync-tile-name")
	name.SetXAlign(0)
	name.SetEllipsize(pango.EllipsizeEnd)
	sub := gtk.NewLabel(targetLabel(p.Target))
	sub.AddCSSClass("sync-tile-sub")
	sub.SetXAlign(0)
	sub.SetEllipsize(pango.EllipsizeEnd)
	text := gtk.NewBox(gtk.OrientationVertical, 0)
	text.Append(name)
	text.Append(sub)
	text.SetVAlign(gtk.AlignEnd)
	text.SetCanTarget(false)

	live := gtk.NewLabel("Live")
	live.AddCSSClass("sync-tile-live")
	live.SetHAlign(gtk.AlignStart)
	live.SetVAlign(gtk.AlignStart)
	live.SetCanTarget(false)
	live.SetVisible(false)
	tile.ConnectToggled(func() { live.SetVisible(tile.Active()) })

	edit := gtk.NewButtonFromIconName("document-edit-symbolic")
	edit.AddCSSClass("scene-tile-action")
	edit.SetTooltipText("Edit preset")
	setA11yLabel(&edit.Widget, "Edit preset "+p.Name)
	edit.SetHAlign(gtk.AlignEnd)
	edit.SetVAlign(gtk.AlignStart)
	edit.SetMarginEnd(34) // left of the delete button
	edit.ConnectClicked(func() { t.openPreset(i) })

	del := gtk.NewButtonFromIconName("user-trash-symbolic")
	del.AddCSSClass("scene-tile-action")
	del.SetTooltipText("Delete preset")
	setA11yLabel(&del.Widget, "Delete preset "+p.Name)
	del.SetHAlign(gtk.AlignEnd)
	del.SetVAlign(gtk.AlignStart)
	del.ConnectClicked(func() { t.deletePreset(i) })

	o := gtk.NewOverlay()
	o.SetChild(tile)
	o.AddOverlay(text)
	o.AddOverlay(live)
	o.AddOverlay(edit)
	o.AddOverlay(del)
	t.tiles = append(t.tiles, tile)
	return o
}

// presetIdleCSS fills an idle tile with a gradient whose hue is derived
// from the preset ID, so each preset keeps its own colour.
func presetIdleCSS(class, id string) string {
	var h uint32 = 2166136261
	for i := 0; i < len(id); i++ {
		h = (h ^ uint32(id[i])) * 16777619
	}
	hue := float64(h%360) / 360
	a := hsvRGB(hue, 0.55, 0.75)
	b := hsvRGB(math.Mod(hue+0.12, 1), 0.65, 0.55)
	return tileGradientCSS(class, []dp.RGB{a, b})
}

// tileGradientCSS is the scene-tile fill: a diagonal gradient across the
// colours under a bottom vignette that keeps the name legible (earlier
// background layers paint on top).
func tileGradientCSS(class string, cs []dp.RGB) string {
	stops := make([]string, 0, len(cs)+1)
	for _, c := range cs {
		stops = append(stops, fmt.Sprintf("rgb(%d,%d,%d)", c.R, c.G, c.B))
	}
	if len(stops) == 1 {
		stops = append(stops, stops[0])
	}
	return fmt.Sprintf("button.%s { background-image: linear-gradient(to bottom, transparent 55%%, alpha(#000, 0.28) 100%%), linear-gradient(135deg, %s); }\n",
		class, strings.Join(stops, ", "))
}

// paintLive fills the running tile with its regions' live colours, at most
// five times a second.
func (t *syncTab) paintLive(i int, cs []dp.RGB) {
	if len(cs) == 0 || time.Since(t.lastPaint) < 200*time.Millisecond {
		return
	}
	t.lastPaint = time.Now()
	t.css.LoadFromString(t.tileCSS + tileGradientCSS(fmt.Sprintf("sync-tile-%d", i), cs))
}

// syncSwitches reflects t.active on the tiles; a stopped tile gets its idle
// fill back.
func (t *syncTab) syncSwitches() {
	t.suppress = true
	for i, tile := range t.tiles {
		tile.SetActive(t.active == i)
	}
	t.suppress = false
	if t.active < 0 {
		t.css.LoadFromString(t.tileCSS)
	}
}

// addPresetDialog asks for a target and a name, then opens the new preset.
func (t *syncTab) addPresetDialog() {
	body := gtk.NewBox(gtk.OrientationVertical, 12)

	kind := adw.NewToggleGroup()
	for _, k := range [][2]string{{screensync.TargetMonitors, "Monitors"}, {screensync.TargetWindow, "Window"}} {
		tg := adw.NewToggle()
		tg.SetName(k[0])
		tg.SetLabel(k[1])
		kind.Add(tg)
	}
	body.Append(kind)

	name := adw.NewEntryRow()
	name.SetTitle("Name")
	names := gtk.NewListBox()
	names.AddCSSClass("boxed-list")
	names.Append(name)
	body.Append(names)

	monitors := gtk.NewListBox()
	monitors.AddCSSClass("boxed-list")
	var monChecks []*gtk.CheckButton
	var monNames []string
	if ms, err := t.tracker.Monitors(); err == nil {
		for _, m := range ms {
			row := adw.NewActionRow()
			row.SetTitle(m.Name)
			row.SetSubtitle(fmt.Sprintf("%d × %d", m.W, m.H))
			c := gtk.NewCheckButton()
			row.AddPrefix(c)
			row.SetActivatableWidget(c)
			monitors.Append(row)
			monChecks = append(monChecks, c)
			monNames = append(monNames, m.Name)
		}
	}

	windows := gtk.NewListBox()
	windows.AddCSSClass("boxed-list")
	var winChecks []*gtk.CheckButton
	var wins []screensync.Window
	if ws, err := t.tracker.Windows(); err == nil {
		for _, w := range ws {
			if w.Class == "" {
				continue
			}
			row := adw.NewActionRow()
			row.SetTitle(w.Title)
			row.SetSubtitle(w.Class)
			c := gtk.NewCheckButton()
			if len(winChecks) > 0 {
				c.SetGroup(winChecks[0])
			}
			row.AddPrefix(c)
			row.SetActivatableWidget(c)
			windows.Append(row)
			winChecks = append(winChecks, c)
			wins = append(wins, w)
		}
	}
	titleMatch := adw.NewEntryRow()
	titleMatch.SetTitle("Title contains (optional)")
	titles := gtk.NewListBox()
	titles.AddCSSClass("boxed-list")
	titles.Append(titleMatch)

	pick := gtk.NewBox(gtk.OrientationVertical, 12)
	scroll := gtk.NewScrolledWindow()
	scroll.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	scroll.SetMaxContentHeight(320)
	scroll.SetPropagateNaturalHeight(true)
	scroll.SetChild(pick)
	body.Append(scroll)

	showKind := func() {
		for c := pick.FirstChild(); c != nil; c = pick.FirstChild() {
			pick.Remove(c)
		}
		if kind.ActiveName() == screensync.TargetWindow {
			pick.Append(windows)
			pick.Append(titles)
		} else {
			pick.Append(monitors)
		}
	}
	kind.SetActiveName(screensync.TargetMonitors)
	showKind()
	kind.NotifyProperty("active-name", showKind)

	dlg := adw.NewAlertDialog("New preset", "")
	dlg.SetExtraChild(body)
	dlg.AddResponse("cancel", "Cancel")
	dlg.AddResponse("add", "Add")
	dlg.SetResponseAppearance("add", adw.ResponseSuggested)
	dlg.SetDefaultResponse("add")
	dlg.SetCloseResponse("cancel")
	dlg.ConnectResponse(func(resp string) {
		if resp != "add" {
			return
		}
		p := SyncPreset{ID: newSyncID(), Name: strings.TrimSpace(name.Text())}
		if kind.ActiveName() == screensync.TargetWindow {
			for i, c := range winChecks {
				if c.Active() {
					p.Target = SyncTarget{
						Kind: screensync.TargetWindow, WindowClass: wins[i].Class,
						TitleMatch: strings.TrimSpace(titleMatch.Text()),
					}
				}
			}
		} else {
			var sel []string
			for i, c := range monChecks {
				if c.Active() {
					sel = append(sel, monNames[i])
				}
			}
			if len(sel) > 0 {
				p.Target = SyncTarget{Kind: screensync.TargetMonitors, Monitors: sel}
			}
		}
		if p.Target.Kind == "" {
			t.showBanner("Pick a monitor or a window for the preset")
			return
		}
		if p.Name == "" {
			p.Name = targetLabel(p.Target)
		}
		t.a.cfg.ScreenSync.Presets = append(t.a.cfg.ScreenSync.Presets, p)
		t.saveSoon()
		t.rebuildList()
		t.openPreset(len(t.presets()) - 1)
	})
	dlg.Present(t.a.window)
}

var syncIDSeq int

func newSyncID() string {
	syncIDSeq++
	return fmt.Sprintf("%x-%x", time.Now().UnixNano(), syncIDSeq)
}

// --- preset page ---

func (t *syncTab) openPreset(i int) {
	p := &t.a.cfg.ScreenSync.Presets[i]
	t.pageIdx = i

	body := gtk.NewBox(gtk.OrientationVertical, 18)
	body.SetMarginTop(18)
	body.SetMarginBottom(18)
	body.SetMarginStart(12)
	body.SetMarginEnd(12)

	info := adw.NewPreferencesGroup()
	name := adw.NewEntryRow()
	name.SetTitle("Name")
	name.SetText(p.Name)
	name.ConnectChanged(func() {
		p.Name = name.Text()
		if t.page != nil {
			t.page.SetTitle(p.Name)
		}
		t.saveSoon()
	})
	info.Add(name)
	target := adw.NewActionRow()
	target.SetTitle("Target")
	target.SetSubtitle(targetLabel(p.Target))
	info.Add(target)
	edit := gtk.NewButtonWithLabel("Edit regions")
	edit.AddCSSClass("suggested-action")
	edit.SetVAlign(gtk.AlignCenter)
	edit.SetSensitive(t.tracker != nil && layerShellSupported())
	edit.SetTooltipText("Draw regions over the target; lights follow live")
	edit.ConnectClicked(func() { t.beginEdit(i) })
	target.AddSuffix(edit)
	body.Append(info)

	t.regionsBox = gtk.NewBox(gtk.OrientationVertical, 0)
	t.regionList = nil
	body.Append(t.regionsBox)
	t.rebuildRegions()

	del := gtk.NewButtonWithLabel("Delete preset")
	del.AddCSSClass("destructive-action")
	del.SetHAlign(gtk.AlignCenter)
	del.ConnectClicked(func() { t.deletePreset(i) })
	body.Append(del)

	clamp := adw.NewClamp()
	clamp.SetMaximumSize(600)
	clamp.SetChild(body)
	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	scroll.SetChild(clamp)

	header := adw.NewHeaderBar()
	header.SetShowEndTitleButtons(false)
	header.SetShowStartTitleButtons(false)
	tv := adw.NewToolbarView()
	tv.AddTopBar(header)
	tv.SetContent(scroll)

	t.page = adw.NewNavigationPage(tv, p.Name)
	t.nav.Push(t.page)
}

func (t *syncTab) deviceName(id string) string {
	for _, d := range t.a.cfg.Devices {
		if d.DeviceID == id {
			if d.Name != "" {
				return d.Name
			}
			return id
		}
	}
	return id
}

func (t *syncTab) rebuildRegions() {
	if t.regionsBox == nil || t.pageIdx < 0 {
		return
	}
	if t.regionList != nil {
		t.regionsBox.Remove(t.regionList)
	}
	p := &t.a.cfg.ScreenSync.Presets[t.pageIdx]
	t.swatches = nil

	g := adw.NewPreferencesGroup()
	g.SetTitle("Regions")
	if len(p.Regions) == 0 {
		g.SetDescription("Use Edit regions to draw rectangles over the target")
	} else {
		g.SetDescription("Each light follows at most one region")
	}
	for ri := range p.Regions {
		r := &p.Regions[ri]
		row := adw.NewExpanderRow()
		row.SetTitle(r.Name)
		row.SetSubtitle(t.boundSummary(r.Devices))

		sw := &regionSwatch{da: gtk.NewDrawingArea()}
		sw.da.SetContentWidth(20)
		sw.da.SetContentHeight(20)
		sw.da.SetVAlign(gtk.AlignCenter)
		sw.da.SetDrawFunc(func(_ *gtk.DrawingArea, cr *cairo.Context, w, h int) {
			roundedRect(cr, 0, 0, float64(w), float64(h), 5)
			if sw.set {
				cr.SetSourceRGB(float64(sw.rgb.R)/255, float64(sw.rgb.G)/255, float64(sw.rgb.B)/255)
			} else {
				cr.SetSourceRGBA(1, 1, 1, 0.12)
			}
			cr.Fill()
		})
		row.AddPrefix(sw.da)
		t.swatches = append(t.swatches, sw)

		name := adw.NewEntryRow()
		name.SetTitle("Name")
		name.SetText(r.Name)
		name.ConnectChanged(func() {
			r.Name = name.Text()
			row.SetTitle(r.Name)
			t.pushOverlayRegions()
			t.saveSoon()
		})
		row.AddRow(name)

		for _, d := range t.a.cfg.Devices {
			id := d.DeviceID
			lr := adw.NewActionRow()
			lr.SetTitle(t.deviceName(id))
			if owner := regionOf(p, id); owner >= 0 && owner != ri {
				lr.SetSubtitle("In " + p.Regions[owner].Name)
			}
			c := gtk.NewCheckButton()
			setA11yLabel(&c.Widget, t.deviceName(id)+" in "+r.Name)
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
				t.regionsChanged(t.pageIdx)
				// Defer the rebuild: this handler's widget is in the list.
				coreglib.IdleAdd(func() { t.rebuildRegions() })
			})
			lr.AddPrefix(c)
			lr.SetActivatableWidget(c)
			row.AddRow(lr)
		}

		del := gtk.NewButtonFromIconName("user-trash-symbolic")
		del.AddCSSClass("flat")
		del.SetVAlign(gtk.AlignCenter)
		del.SetTooltipText("Delete region")
		setA11yLabel(&del.Widget, "Delete region "+r.Name)
		del.ConnectClicked(func() { t.confirmDeleteRegion(ri) })
		row.AddSuffix(del)

		g.Add(row)
	}
	t.regionsBox.Append(g)
	t.regionList = g
}

// setA11yLabel names a widget whose visible label lives elsewhere (icon
// buttons, row prefixes) so assistive tech can address it.
func setA11yLabel(w *gtk.Widget, label string) {
	w.UpdateProperty([]gtk.AccessibleProperty{gtk.AccessiblePropertyLabel},
		[]coreglib.Value{*coreglib.NewValue(label)})
}

func regionOf(p *SyncPreset, id string) int {
	for i, r := range p.Regions {
		if slices.Contains(r.Devices, id) {
			return i
		}
	}
	return -1
}

func (t *syncTab) boundSummary(ids []string) string {
	if len(ids) == 0 {
		return "No lights"
	}
	names := make([]string, len(ids))
	for i, id := range ids {
		names[i] = t.deviceName(id)
	}
	return strings.Join(names, ", ")
}

func (t *syncTab) confirmDeleteRegion(ri int) {
	p := &t.a.cfg.ScreenSync.Presets[t.pageIdx]
	dlg := adw.NewAlertDialog("Delete region?", "“"+p.Regions[ri].Name+"” will be removed.")
	dlg.AddResponse("cancel", "Cancel")
	dlg.AddResponse("delete", "Delete")
	dlg.SetResponseAppearance("delete", adw.ResponseDestructive)
	dlg.SetDefaultResponse("cancel")
	dlg.SetCloseResponse("cancel")
	dlg.ConnectResponse(func(resp string) {
		if resp == "delete" {
			t.deleteRegion(t.pageIdx, ri)
		}
	})
	dlg.Present(t.a.window)
}

func (t *syncTab) deleteRegion(pi, ri int) {
	p := &t.a.cfg.ScreenSync.Presets[pi]
	if ri < 0 || ri >= len(p.Regions) {
		return
	}
	p.Regions = slices.Delete(p.Regions, ri, ri+1)
	t.regionsChanged(pi)
	if t.pageIdx == pi {
		t.rebuildRegions()
	}
	if t.editTarget == pi {
		t.pushOverlayRegions()
	}
}

func (t *syncTab) deletePreset(i int) {
	name := t.presets()[i].Name
	dlg := adw.NewAlertDialog("Delete preset?", "“"+name+"” and its regions will be removed.")
	dlg.AddResponse("cancel", "Cancel")
	dlg.AddResponse("delete", "Delete")
	dlg.SetResponseAppearance("delete", adw.ResponseDestructive)
	dlg.SetDefaultResponse("cancel")
	dlg.SetCloseResponse("cancel")
	dlg.ConnectResponse(func(resp string) {
		if resp != "delete" {
			return
		}
		if t.editTarget == i {
			t.overlay.finish()
		}
		if t.active == i {
			t.stop("")
		}
		if t.active > i {
			t.active--
		}
		s := t.a.cfg.ScreenSync
		s.Presets = slices.Delete(s.Presets, i, i+1)
		t.saveSoon()
		if t.page != nil {
			t.nav.Pop() // rebuilds the grid
		} else {
			t.rebuildList()
		}
	})
	dlg.Present(t.a.window)
}

// regionsChanged persists the preset and pushes it to a running engine.
func (t *syncTab) regionsChanged(pi int) {
	t.saveSoon()
	if t.engine != nil && t.active == pi {
		t.engine.SetRegions(t.engineRegions(pi))
		t.updateLock()
	}
}

// --- sync ---

func (t *syncTab) engineRegions(pi int) []screensync.Region {
	p := t.presets()[pi]
	out := make([]screensync.Region, len(p.Regions))
	for i, r := range p.Regions {
		out[i].Rect = screensync.Rect{X: r.Rect[0], Y: r.Rect[1], W: r.Rect[2], H: r.Rect[3]}
		for _, id := range r.Devices {
			if ctl, ok := t.a.byID[id]; ok {
				out[i].Lights = append(out[i].Lights, ctl)
			}
		}
	}
	return out
}

func (t *syncTab) toggle(i int, on bool) {
	if on {
		t.start(i)
	} else if t.active == i {
		t.stop("")
	}
}

// start opens the capture off the GTK thread (the system picker may show)
// and starts the engine. A different running preset is stopped first.
func (t *syncTab) start(i int) {
	if t.starting || t.tracker == nil {
		t.syncSwitches()
		return
	}
	if t.active >= 0 && t.active != i {
		t.stop("")
	}
	if t.active == i {
		return
	}
	t.starting = true
	t.active = i
	t.syncSwitches()
	t.showBanner("")
	p := t.presets()[i]
	target := screensyncTarget(p.Target)
	tokens := p.RestoreTokens
	regions := t.engineRegions(i)
	brightness := t.a.cfg.ScreenSync.Brightness
	go func() {
		capture, err := screensync.OpenCapture(context.Background(), target, screensync.CaptureOptions{Tokens: tokens})
		var eng *screensync.Engine
		if err == nil {
			eng, err = screensync.Start(screensync.Options{
				Sources:    capture.Sources(),
				Regions:    regions,
				Brightness: brightness,
				OnFrame: func(cs []dp.RGB) {
					coreglib.IdleAdd(func() { t.showColours(i, cs) })
				},
				OnStop: func(err error) {
					coreglib.IdleAdd(func() {
						if t.active == i {
							t.stop("Capture ended — sync stopped: " + err.Error())
						}
					})
				},
				OnLightTrouble: func(l screensync.LightSink, ok bool) {
					coreglib.IdleAdd(func() {
						if ok {
							t.showBanner("")
						} else if ctl, isCtl := l.(*control); isCtl {
							t.showBanner(ctl.name() + " is not responding — retrying")
						}
					})
				},
			})
			if err != nil {
				capture.Close()
			}
		}
		coreglib.IdleAdd(func() {
			t.starting = false
			if err != nil {
				t.active = -1
				t.syncSwitches()
				t.showBanner(syncErrorText(err))
				return
			}
			if t.active != i { // switched off while starting
				go func() { eng.Stop(); capture.Close() }()
				return
			}
			t.engine, t.capture = eng, capture
			t.saveTokens(i, capture.Tokens())
			t.watchTarget(i)
			t.updateLock()
		})
	}()
}

func syncErrorText(err error) string {
	switch {
	case errors.Is(err, screensync.ErrNoDMABuf):
		return "GPU capture (DMA-BUF) unavailable — sync disabled"
	case errors.Is(err, screensync.ErrNoHyprland):
		return "Screen Sync requires Hyprland"
	}
	return "Sync failed: " + err.Error()
}

// saveTokens keeps the portal restore tokens; without any the picker will
// show on every start, so say how to allow them.
func (t *syncTab) saveTokens(i int, tokens map[string]string) {
	p := &t.a.cfg.ScreenSync.Presets[i]
	if len(tokens) == 0 {
		t.showBanner("Allow the share token in the picker to skip it next time")
	}
	if len(tokens) > 0 && !mapsEqual(p.RestoreTokens, tokens) {
		p.RestoreTokens = tokens
		t.saveSoon()
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// watchTarget stops the sync when a window target closes.
func (t *syncTab) watchTarget(i int) {
	ctx, cancel := context.WithCancel(context.Background())
	t.stopSync = cancel
	if t.presets()[i].Target.Kind != screensync.TargetWindow {
		return
	}
	go t.tracker.Watch(ctx, screensyncTarget(t.presets()[i].Target), 0, func(pl screensync.Placement) {
		if !pl.Gone {
			return
		}
		coreglib.IdleAdd(func() {
			if ctx.Err() == nil && t.active == i {
				t.stop("Window closed — sync stopped")
			}
		})
	})
}

// stop ends the running sync; lights keep their last colour.
func (t *syncTab) stop(msg string) {
	if t.stopSync != nil {
		t.stopSync()
		t.stopSync = nil
	}
	eng, capture := t.engine, t.capture
	t.engine, t.capture = nil, nil
	if t.editTarget == t.active && t.editTarget >= 0 {
		t.editOwns = false
	}
	t.active = -1
	t.syncSwitches()
	t.showBanner(msg)
	// Stop blocks while each bulb persists its final colour: off the GTK thread.
	// The lights unlock once their streams have closed.
	if eng != nil {
		go func() {
			eng.Stop()
			capture.Close()
			coreglib.IdleAdd(t.updateLock)
		}()
	}
}

// showColours paints the live region swatches (page and overlay).
func (t *syncTab) showColours(i int, cs []dp.RGB) {
	if t.active != i {
		return
	}
	t.paintLive(i, cs)
	if t.pageIdx == i {
		for ri, c := range cs {
			if ri < len(t.swatches) {
				s := t.swatches[ri]
				if !s.set || s.rgb != c {
					s.rgb, s.set = c, true
					s.da.QueueDraw()
				}
			}
		}
	}
	if t.editTarget == i {
		for ri, c := range cs {
			t.overlay.SetColour(ri, c)
		}
	}
}

// syncedDevices are the lights the running preset drives.
func (t *syncTab) syncedDevices() map[string]bool {
	out := map[string]bool{}
	if t.active < 0 || t.engine == nil {
		return out
	}
	for _, r := range t.presets()[t.active].Regions {
		for _, id := range r.Devices {
			out[id] = true
		}
	}
	return out
}

func (t *syncTab) updateLock() {
	if t.a.onSyncLock != nil {
		t.a.onSyncLock(t.syncedDevices())
	}
}

// shutdown stops sync and editing when the window closes.
func (t *syncTab) shutdown() {
	t.overlay.Hide()
	if t.stopEdit != nil {
		t.stopEdit()
	}
	if t.stopSync != nil {
		t.stopSync()
	}
	if t.engine != nil {
		t.engine.Stop()
		t.capture.Close()
		t.engine = nil
	}
	t.flushSave()
}

// --- region editing ---

// beginEdit maps the overlay over preset i and keeps it on the target as
// it moves. Lights follow live, so the sync starts if it isn't running.
func (t *syncTab) beginEdit(i int) {
	if t.editTarget >= 0 {
		return
	}
	t.editTarget = i
	t.editOwns = t.active != i
	if t.editOwns {
		t.start(i)
	}

	o := t.overlay
	o.onCreate = func(r [4]float64) {
		p := &t.a.cfg.ScreenSync.Presets[i]
		p.Regions = append(p.Regions, SyncRegion{ID: newSyncID(), Name: fmt.Sprintf("Region %d", len(p.Regions)+1), Rect: r})
		t.pushOverlayRegions()
		t.afterRegionEdit(i, true)
	}
	o.onChange = func(ri int, r [4]float64) {
		t.a.cfg.ScreenSync.Presets[i].Regions[ri].Rect = r
		t.afterRegionEdit(i, false)
	}
	o.onDelete = func(ri int) { t.deleteRegion(i, ri) }
	o.onDone = func() { t.endEdit() }
	t.pushOverlayRegions()

	ctx, cancel := context.WithCancel(context.Background())
	t.stopEdit = cancel
	go t.tracker.Watch(ctx, screensyncTarget(t.presets()[i].Target), syncEditPoll, func(pl screensync.Placement) {
		coreglib.IdleAdd(func() {
			if ctx.Err() != nil {
				return
			}
			if pl.Gone {
				t.overlay.finish()
				t.showBanner("Window not found")
				return
			}
			t.overlay.Show(&t.a.app.Application, pl)
		})
	})
}

func (t *syncTab) afterRegionEdit(i int, structural bool) {
	t.saveSoon()
	if t.engine != nil && t.active == i {
		t.engine.SetRegions(t.engineRegions(i))
	}
	if structural && t.pageIdx == i {
		t.rebuildRegions()
	}
}

func (t *syncTab) pushOverlayRegions() {
	if t.editTarget < 0 {
		return
	}
	p := t.presets()[t.editTarget]
	rs := make([]overlayRegion, len(p.Regions))
	for ri, r := range p.Regions {
		rs[ri] = overlayRegion{Name: r.Name, Rect: r.Rect}
		if ri < len(t.overlay.regions) {
			rs[ri].Colour = t.overlay.regions[ri].Colour
		}
	}
	t.overlay.SetRegions(rs)
}

func (t *syncTab) endEdit() {
	if t.stopEdit != nil {
		t.stopEdit()
		t.stopEdit = nil
	}
	i := t.editTarget
	t.editTarget = -1
	if t.editOwns && t.active == i {
		t.stop("")
	}
	t.editOwns = false
}
