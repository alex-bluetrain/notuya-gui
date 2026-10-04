package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/cairo"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
)

// roomsTabEnabled gates the Rooms tab, which is parked until it is finished.
// Its code stays compiled (not commented out) so it cannot silently rot.
const roomsTabEnabled = false

// desktopApp is the default entry point: a libadwaita toplevel with Scenes,
// Lights and Settings tabs. It owns one control (persistent session) per device
// for the window's lifetime and closes them all on exit.
type desktopApp struct {
	app          *adw.Application
	window       *adw.ApplicationWindow
	configPath   string
	cfg          *Config
	wheelSurface *cairo.Surface

	controls []*control     // one per device, parallel to cfg.Devices
	panels   []*devicePanel // Rooms tab only; empty while it is disabled

	// byID resolves device_id → control, so the scenes tab can apply a scene
	// without re-deriving the mapping.
	byID map[string]*control

	// sync is the Screen Sync tab; onSyncLock tells the Lights tab which
	// lights the running sync drives.
	sync       *syncTab
	onSyncLock func(synced map[string]bool)

	// scenesFlow + scenesStatus back the Scenes tab; the tile grid is rebuilt
	// on every change.
	scenesFlow   *gtk.FlowBox
	scenesStatus *gtk.Label
	// scenesCSS holds the per-tile gradient rules; rebuilt and reloaded on
	// every refreshScenesList so each tile is filled by its lights' colours.
	scenesCSS *gtk.CSSProvider

	// roomRows backs the Rooms tab's control section: one summary row per
	// room, updated live as member panels refresh or are toggled.
	roomRows []*roomRow

	// Room-management widgets (moved out of the Settings tab). manageGroup
	// holds one editable ActionRow per room; roomNameEntry + membersGroup edit
	// the currently selected room. roomSelected indexes a.cfg.Rooms (-1 = new).
	manageGroup   *adw.PreferencesGroup
	membersGroup  *adw.PreferencesGroup
	roomNameEntry *gtk.Entry
	roomStatus    *gtk.Label
	roomSelected  int
	manageRows    []*adw.ActionRow
	memberRows    []*adw.ActionRow
}

// roomRow is one row in the Rooms tab: a master on/off switch plus a live
// summary of how many of the room's lights are on.
type roomRow struct {
	row     *adw.ActionRow
	toggle  *gtk.Switch
	members []*devicePanel
	// suppress guards the switch's state-set handler while we set it
	// programmatically from a summary update.
	suppress bool
}

func runApp(configPath string, cfg *Config) {
	a := &desktopApp{
		configPath:   configPath,
		cfg:          cfg,
		wheelSurface: loadWheelSurface(wheelCachePath(configPath)),
	}
	a.app = adw.NewApplication("ar.averstraeten.tuyawheel.app", gio.ApplicationNonUnique)
	a.app.ConnectActivate(func() { a.activate() })
	os.Exit(a.app.Run(os.Args[:1]))
}

func (a *desktopApp) activate() {
	window := adw.NewApplicationWindow(&a.app.Application)
	window.SetTitle("Lights")
	window.SetDefaultSize(520, 720)
	a.window = window

	a.byID = make(map[string]*control, len(a.cfg.Devices))

	// Build one control per device, keyed by device_id so every tab shares
	// the same session per bulb.
	for i := range a.cfg.Devices {
		ctl := newControl(a.cfg.Devices[i])
		a.controls = append(a.controls, ctl)
		a.byID[a.cfg.Devices[i].DeviceID] = ctl
	}

	// A ViewStack holds the views; a ViewSwitcher in the header bar selects
	// between them (the Adwaita replacement for a Notebook's tabs).
	stack := adw.NewViewStack()
	stack.SetVExpand(true)
	stack.AddTitledWithIcon(a.buildScenesTab(), "scenes", "Scenes", "starred-symbolic")
	stack.AddTitledWithIcon(a.buildLightsTab(), "lights", "Lights", "weather-clear-symbolic")
	if roomsTabEnabled {
		stack.AddTitledWithIcon(a.buildRoomsTab(), "rooms", "Rooms", "user-home-symbolic")
	}
	stack.AddTitledWithIcon(a.buildScreenSyncTab(), "sync", "Screen Sync", "video-display-symbolic")
	stack.AddTitledWithIcon(a.buildSettingsTab(), "settings", "Settings", "emblem-system-symbolic")
	stack.SetVisibleChildName("scenes")

	switcher := adw.NewViewSwitcher()
	switcher.SetPolicy(adw.ViewSwitcherPolicyWide)
	switcher.SetStack(stack)

	header := adw.NewHeaderBar()
	header.SetTitleWidget(switcher)

	root := gtk.NewBox(gtk.OrientationVertical, 0)
	root.Append(header)
	root.Append(stack)
	window.SetContent(root)

	// Closing the window tears down the device sessions. We don't call
	// app.Hold(): with no artificial hold, GApplication drops its last
	// reference when this window closes and exits on its own, so a window
	// close (from the title bar, the compositor, or a test harness) is a
	// clean, complete shutdown. We must NOT also call app.Quit() here —
	// that double-releases the use count and trips a GLib assertion.
	window.ConnectCloseRequest(func() bool {
		a.sync.shutdown()
		a.closeControls()
		return false // allow the window to close; the app quits with it
	})

	window.Present()

	// Populate the Rooms tab's panels (if any) with live device state.
	for _, panel := range a.panels {
		panel.refresh()
	}
}

// buildRoomsTab builds a mobile-style overview: one row per room with an icon,
// the room name, a live "N of M lights on" summary, and a master switch that
// powers the whole room on or off. Its panels wrap the shared controls, so no
// extra sessions are opened.
func (a *desktopApp) buildRoomsTab() *gtk.ScrolledWindow {
	byID := make(map[string]*devicePanel, len(a.controls))
	for _, ctl := range a.controls {
		panel := newDevicePanel(ctl)
		a.panels = append(a.panels, panel)
		byID[ctl.dev.DeviceID] = panel
	}

	group := adw.NewPreferencesGroup()
	group.SetTitle("My rooms")

	for _, grp := range groupByRoom(a.cfg) {
		members := make([]*devicePanel, 0, len(grp.Devices))
		for _, dev := range grp.Devices {
			if panel, ok := byID[dev.DeviceID]; ok {
				members = append(members, panel)
			}
		}

		rr := &roomRow{members: members}
		rr.row = adw.NewActionRow()
		rr.row.SetTitle(grp.Name)
		icon := gtk.NewImageFromIconName("user-home-symbolic")
		rr.row.AddPrefix(icon)

		rr.toggle = gtk.NewSwitch()
		rr.toggle.SetVAlign(gtk.AlignCenter)
		rr.toggle.ConnectStateSet(func(state bool) bool {
			if rr.suppress {
				return false
			}
			a.groupPower(rr.members, state)
			// Optimistically reflect the new state in each member panel's
			// switch and the summary; a later refresh reconciles.
			for _, p := range rr.members {
				p.setPowerOptimistic(state)
			}
			rr.updateSummary()
			return false
		})
		rr.row.AddSuffix(rr.toggle)

		// When any member panel refreshes or is toggled, recompute this
		// room's summary.
		for _, p := range members {
			prevRefresh := p.onRefresh
			p.onRefresh = func() {
				if prevRefresh != nil {
					prevRefresh()
				}
				rr.updateSummary()
			}
			prevToggle := p.onToggle
			p.onToggle = func(on bool) {
				if prevToggle != nil {
					prevToggle(on)
				}
				rr.updateSummary()
			}
		}

		rr.updateSummary()
		group.Add(rr.row)
		a.roomRows = append(a.roomRows, rr)
	}

	clamp := adw.NewClamp()
	clamp.SetMaximumSize(600)

	box := gtk.NewBox(gtk.OrientationVertical, 12)
	box.SetMarginTop(18)
	box.SetMarginBottom(18)
	box.SetMarginStart(12)
	box.SetMarginEnd(12)
	box.Append(group)
	box.Append(a.buildRoomManagement())
	clamp.SetChild(box)

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	scroll.SetChild(clamp)
	return scroll
}

// buildRoomManagement builds the room create/rename/delete + device-assignment
// UI, moved out of the Settings tab so everything room-related lives in one
// place. It edits a.cfg.Rooms directly and persists via saveCfg. Structural
// changes (a room's membership) are reflected in the Lights/Rooms control views
// on the next launch, matching how device edits in Settings behave.
func (a *desktopApp) buildRoomManagement() gtk.Widgetter {
	a.roomSelected = -1

	a.manageGroup = adw.NewPreferencesGroup()
	a.manageGroup.SetTitle("Manage rooms")

	a.roomNameEntry = gtk.NewEntry()
	a.roomNameEntry.SetPlaceholderText("Room name")
	a.roomNameEntry.SetHExpand(true)
	addBtn := gtk.NewButtonWithLabel("Add / Rename")
	addBtn.ConnectClicked(func() { a.upsertRoom() })
	entryRow := gtk.NewBox(gtk.OrientationHorizontal, 8)
	entryRow.SetMarginTop(8)
	entryRow.Append(a.roomNameEntry)
	entryRow.Append(addBtn)
	a.manageGroup.Add(entryRow)

	a.membersGroup = adw.NewPreferencesGroup()
	a.membersGroup.SetTitle("Devices in room")

	a.roomStatus = gtk.NewLabel("")
	a.roomStatus.SetXAlign(0.0)
	a.roomStatus.SetWrap(true)
	a.roomStatus.AddCSSClass("dim-label")
	a.roomStatus.SetMarginTop(4)

	a.refreshManageRooms()

	box := gtk.NewBox(gtk.OrientationVertical, 12)
	box.Append(a.manageGroup)
	box.Append(a.membersGroup)
	box.Append(a.roomStatus)
	return box
}

// refreshManageRooms rebuilds the editable room list (one ActionRow per room
// with Select + Delete) from a.cfg.Rooms.
func (a *desktopApp) refreshManageRooms() {
	for _, row := range a.manageRows {
		a.manageGroup.Remove(row)
	}
	a.manageRows = a.manageRows[:0]

	for i := range a.cfg.Rooms {
		idx := i
		r := a.cfg.Rooms[idx]
		row := adw.NewActionRow()
		name := r.Name
		if name == "" {
			name = "(unnamed)"
		}
		row.SetTitle(name)
		row.SetSubtitle(fmt.Sprintf("%d devices", len(r.Devices)))
		row.SetActivatable(true)

		del := gtk.NewButtonFromIconName("user-trash-symbolic")
		del.SetVAlign(gtk.AlignCenter)
		del.AddCSSClass("flat")
		del.ConnectClicked(func() { a.removeRoom(idx) })
		row.AddSuffix(del)

		row.ConnectActivated(func() { a.selectRoom(idx) })

		a.manageGroup.Add(row)
		a.manageRows = append(a.manageRows, row)
	}

	if a.roomSelected >= len(a.cfg.Rooms) {
		a.roomSelected = -1
	}
	a.refreshMembers()
}

// selectRoom marks a room as the edit target, loads its name into the entry,
// and rebuilds the membership toggles.
func (a *desktopApp) selectRoom(idx int) {
	a.roomSelected = idx
	if idx >= 0 && idx < len(a.cfg.Rooms) {
		a.roomNameEntry.SetText(a.cfg.Rooms[idx].Name)
	}
	a.refreshMembers()
}

// refreshMembers rebuilds the membership checkboxes for the selected room.
func (a *desktopApp) refreshMembers() {
	for _, row := range a.memberRows {
		a.membersGroup.Remove(row)
	}
	a.memberRows = a.memberRows[:0]

	if a.roomSelected < 0 || a.roomSelected >= len(a.cfg.Rooms) {
		a.membersGroup.SetDescription("Select a room to edit its devices.")
		return
	}
	a.membersGroup.SetDescription("")
	room := &a.cfg.Rooms[a.roomSelected]
	for i := range a.cfg.Devices {
		d := a.cfg.Devices[i]
		id := d.DeviceID
		label := d.Name
		if label == "" {
			label = id
		}
		row := adw.NewActionRow()
		row.SetTitle(label)
		check := gtk.NewCheckButton()
		check.SetVAlign(gtk.AlignCenter)
		check.SetActive(roomHasDevice(room, id))
		check.ConnectToggled(func() {
			if check.Active() {
				addRoomDevice(room, id)
			} else {
				removeRoomDevice(room, id)
			}
			a.persistRooms(fmt.Sprintf("Room “%s”: %d devices", room.Name, len(room.Devices)))
			a.syncManageSubtitle(a.roomSelected)
		})
		row.AddSuffix(check)
		row.SetActivatableWidget(check)
		a.membersGroup.Add(row)
		a.memberRows = append(a.memberRows, row)
	}
}

func roomHasDevice(r *Room, id string) bool {
	for _, d := range r.Devices {
		if d == id {
			return true
		}
	}
	return false
}

func addRoomDevice(r *Room, id string) {
	if !roomHasDevice(r, id) {
		r.Devices = append(r.Devices, id)
	}
}

func removeRoomDevice(r *Room, id string) {
	for i, d := range r.Devices {
		if d == id {
			r.Devices = append(r.Devices[:i], r.Devices[i+1:]...)
			return
		}
	}
}

// upsertRoom adds a new room or renames the selected one, then persists.
func (a *desktopApp) upsertRoom() {
	name := a.roomNameEntry.Text()
	if name == "" {
		a.setRoomStatus("Room name is required.")
		return
	}
	if a.roomSelected >= 0 && a.roomSelected < len(a.cfg.Rooms) {
		a.cfg.Rooms[a.roomSelected].Name = name
		a.refreshManageRooms()
		a.persistRooms(fmt.Sprintf("Room renamed: %s", name))
		return
	}
	a.cfg.Rooms = append(a.cfg.Rooms, Room{Name: name})
	a.roomSelected = len(a.cfg.Rooms) - 1
	a.refreshManageRooms()
	a.persistRooms(fmt.Sprintf("Room added: %s", name))
}

// removeRoom deletes a room and persists.
func (a *desktopApp) removeRoom(idx int) {
	if idx < 0 || idx >= len(a.cfg.Rooms) {
		return
	}
	removed := a.cfg.Rooms[idx]
	a.cfg.Rooms = append(a.cfg.Rooms[:idx], a.cfg.Rooms[idx+1:]...)
	if a.roomSelected == idx {
		a.roomSelected = -1
		a.roomNameEntry.SetText("")
	} else if a.roomSelected > idx {
		a.roomSelected--
	}
	a.refreshManageRooms()
	a.persistRooms(fmt.Sprintf("Room removed: %s", removed.Name))
}

// syncManageSubtitle updates one manage-row's "N devices" subtitle after a
// membership change, without a full rebuild.
func (a *desktopApp) syncManageSubtitle(idx int) {
	if idx < 0 || idx >= len(a.manageRows) || idx >= len(a.cfg.Rooms) {
		return
	}
	a.manageRows[idx].SetSubtitle(fmt.Sprintf("%d devices", len(a.cfg.Rooms[idx].Devices)))
}

// persistRooms writes the current config to disk and reports status.
func (a *desktopApp) persistRooms(okMsg string) {
	if err := saveConfig(a.configPath, a.cfg.Devices, a.cfg.Rooms, a.cfg.Scenes, a.cfg.ScreenSync); err != nil {
		a.setRoomStatus("Save failed: " + err.Error())
		return
	}
	a.setRoomStatus(okMsg)
}

func (a *desktopApp) setRoomStatus(msg string) {
	if a.roomStatus != nil {
		a.roomStatus.SetText(msg)
	}
}

// updateSummary recomputes the room's subtitle ("All lights on" / "N of M
// lights on" / "All lights off") and syncs the master switch, without firing
// the switch's handler.
func (rr *roomRow) updateSummary() {
	total := len(rr.members)
	if total == 0 {
		rr.row.SetSubtitle("No lights")
		return
	}
	on := 0
	known := 0
	for _, p := range rr.members {
		if p.hasState {
			known++
			if p.lastOn {
				on++
			}
		}
	}

	var subtitle string
	switch {
	case known == 0:
		subtitle = "…"
	case on == 0:
		subtitle = "All lights off"
	case on == total:
		subtitle = "All lights on"
	default:
		subtitle = fmt.Sprintf("%d of %d lights on", on, total)
	}
	rr.row.SetSubtitle(subtitle)

	rr.suppress = true
	rr.toggle.SetActive(on > 0)
	rr.suppress = false
}

// buildSettingsTab embeds the setup wizard as the Settings tab, seeded with the
// configured devices so a scan merges rather than replaces them. The callbacks
// preserve the owner's scenes/rooms on save and mirror device edits back to
// a.cfg.
func (a *desktopApp) buildSettingsTab() gtk.Widgetter {
	return buildEmbeddedWizard(
		a.configPath,
		a.cfg.Devices,
		func(id string) *control { return a.byID[id] },
		func() []Scene { return a.cfg.Scenes },
		func() []Room { return a.cfg.Rooms },
		func(devices []Device) {
			a.cfg.Devices = devices
			// Keep each shared control on the credentials just saved, so the
			// bulb is driven with the same key the config now holds.
			for _, d := range devices {
				if ctl, ok := a.byID[d.DeviceID]; ok {
					ctl.reconfigure(d)
				}
			}
		},
	)
}

// groupPower toggles every member panel's device off the GTK thread.
func (a *desktopApp) groupPower(members []*devicePanel, on bool) {
	for _, p := range members {
		ctl := p.ctl
		ctl.async("power", func(ctx context.Context) error { return ctl.SetPower(ctx, on) })
	}
}

// buildScenesTab builds the "Scenes" tab: a Hue-style grid of scene tiles.
// Each tile is filled with a gradient of its lights' colours, shows the scene
// name over a bottom scrim, and applies the scene when clicked. Edit/delete are
// small buttons overlaid on the tile. A "New scene" button sits in the header.
func (a *desktopApp) buildScenesTab() *gtk.ScrolledWindow {
	clamp := adw.NewClamp()
	clamp.SetMaximumSize(640)
	clamp.SetVExpand(true)

	box := gtk.NewBox(gtk.OrientationVertical, 12)
	box.SetMarginTop(14)
	box.SetMarginBottom(14)
	box.SetMarginStart(14)
	box.SetMarginEnd(14)

	// The static scene-tile styling (shape, scrim, hover) plus per-tile
	// gradient rules all live in one display-wide provider, reloaded on every
	// refresh. Priority above the theme so the gradients win.
	a.scenesCSS = gtk.NewCSSProvider()
	if disp := gdk.DisplayGetDefault(); disp != nil {
		gtk.StyleContextAddProviderForDisplay(disp, a.scenesCSS, uint(gtk.STYLE_PROVIDER_PRIORITY_APPLICATION))
	}

	header := gtk.NewBox(gtk.OrientationHorizontal, 8)
	title := gtk.NewLabel("Scenes")
	title.AddCSSClass("title-2")
	title.SetXAlign(0.0)
	title.SetHExpand(true)
	header.Append(title)

	a.scenesFlow = gtk.NewFlowBox()
	a.scenesFlow.AddCSSClass("scenes-flow")
	a.scenesFlow.SetSelectionMode(gtk.SelectionNone)
	a.scenesFlow.SetHomogeneous(true)
	a.scenesFlow.SetColumnSpacing(12)
	a.scenesFlow.SetRowSpacing(12)
	a.scenesFlow.SetMinChildrenPerLine(2)
	a.scenesFlow.SetMaxChildrenPerLine(3)
	a.scenesFlow.SetVAlign(gtk.AlignStart)

	a.scenesStatus = gtk.NewLabel("")
	a.scenesStatus.SetXAlign(0.0)
	a.scenesStatus.SetWrap(true)
	a.scenesStatus.AddCSSClass("dim-label")

	box.Append(header)
	box.Append(a.scenesFlow)
	box.Append(a.scenesStatus)

	a.refreshScenesList()

	clamp.SetChild(box)
	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	scroll.SetChild(clamp)
	return scroll
}

// refreshScenesList rebuilds the scene-tile grid from a.cfg.Scenes. Each scene
// becomes one tile (a gradient-filled clickable card) with edit/delete overlaid
// in the corner. All tile CSS — the shared shape/scrim rules plus each tile's
// gradient — is assembled here and loaded into the one provider.
func (a *desktopApp) refreshScenesList() {
	a.scenesFlow.RemoveAll()

	var css strings.Builder
	css.WriteString(sceneTileBaseCSS)

	for i := range a.cfg.Scenes {
		idx := i
		sc := a.cfg.Scenes[i]
		class := fmt.Sprintf("scene-tile-%d", idx)
		css.WriteString(sceneGradientCSS(class, sc))
		a.scenesFlow.Insert(a.buildSceneTile(idx, sc, class), -1)
	}

	a.scenesFlow.Insert(a.buildAddSceneTile(), -1)

	a.scenesCSS.LoadFromData(css.String())
}

// buildSceneTile builds one scene card: a gradient-filled button that applies
// the scene, with its name over a bottom scrim and small edit/delete buttons
// overlaid top-right.
func (a *desktopApp) buildSceneTile(idx int, sc Scene, class string) *gtk.Overlay {
	tile := gtk.NewButton()
	tile.AddCSSClass("flat")
	tile.AddCSSClass("scene-tile")
	tile.AddCSSClass(class)
	tile.SetHExpand(true)
	tile.ConnectClicked(func() { a.applySceneAt(idx) })

	// Name label pinned to the bottom-left over a dark scrim for legibility.
	name := gtk.NewLabel(sc.Name)
	name.AddCSSClass("scene-tile-name")
	name.SetXAlign(0.0)
	name.SetEllipsize(pango.EllipsizeEnd)
	name.SetHAlign(gtk.AlignFill)
	name.SetVAlign(gtk.AlignEnd)
	name.SetHExpand(true)
	name.SetCanTarget(false) // clicks pass through to the tile button

	edit := gtk.NewButtonFromIconName("document-edit-symbolic")
	edit.AddCSSClass("scene-tile-action")
	edit.SetTooltipText("Edit scene")
	edit.SetHAlign(gtk.AlignEnd)
	edit.SetVAlign(gtk.AlignStart)
	edit.SetMarginEnd(34) // sit left of the delete button
	edit.ConnectClicked(func() { a.openSceneEditor(idx) })

	del := gtk.NewButtonFromIconName("user-trash-symbolic")
	del.AddCSSClass("scene-tile-action")
	del.SetTooltipText("Delete scene")
	del.SetHAlign(gtk.AlignEnd)
	del.SetVAlign(gtk.AlignStart)
	del.ConnectClicked(func() { a.deleteSceneAt(idx) })

	overlay := gtk.NewOverlay()
	overlay.SetChild(tile)
	overlay.AddOverlay(name)
	overlay.AddOverlay(edit)
	overlay.AddOverlay(del)
	return overlay
}

// buildAddSceneTile builds the trailing "+" card that opens the scene editor
// for a new scene. Same footprint as a scene tile but a dashed, contentless
// placeholder — the pro-app pattern for "add" within a grid.
func (a *desktopApp) buildAddSceneTile() *gtk.Button {
	tile := gtk.NewButton()
	tile.AddCSSClass("flat")
	tile.AddCSSClass("scene-add")
	tile.SetHExpand(true)
	tile.SetTooltipText("New scene")
	tile.ConnectClicked(func() { a.openSceneEditor(-1) })

	icon := gtk.NewImageFromIconName("list-add-symbolic")
	icon.SetPixelSize(28)
	tile.SetChild(icon)
	return tile
}

// applySceneAt fans the scene at index i out to its lights, off the GTK thread.
func (a *desktopApp) applySceneAt(i int) {
	if i < 0 || i >= len(a.cfg.Scenes) {
		return
	}
	sc := a.cfg.Scenes[i]
	a.scenesStatus.SetLabel("Applying “" + sc.Name + "”…")
	applyScene(sc, a.byID)
}

// deleteSceneAt asks for confirmation before removing the scene at index i.
func (a *desktopApp) deleteSceneAt(i int) {
	if i < 0 || i >= len(a.cfg.Scenes) {
		return
	}
	name := a.cfg.Scenes[i].Name
	dlg := adw.NewAlertDialog("Delete scene?", "“"+name+"” will be permanently removed.")
	dlg.AddResponse("cancel", "Cancel")
	dlg.AddResponse("delete", "Delete")
	dlg.SetResponseAppearance("delete", adw.ResponseDestructive)
	dlg.SetDefaultResponse("cancel")
	dlg.SetCloseResponse("cancel")
	dlg.ConnectResponse(func(response string) {
		if response == "delete" {
			a.doDeleteSceneAt(i)
		}
	})
	dlg.Present(a.window)
}

// doDeleteSceneAt removes the scene at index i, persists, and refreshes.
func (a *desktopApp) doDeleteSceneAt(i int) {
	if i < 0 || i >= len(a.cfg.Scenes) {
		return
	}
	name := a.cfg.Scenes[i].Name
	a.cfg.Scenes = append(a.cfg.Scenes[:i:i], a.cfg.Scenes[i+1:]...)
	if err := a.saveCfg(); err != nil {
		a.scenesStatus.SetLabel("Save failed: " + err.Error())
		return
	}
	a.refreshScenesList()
	a.scenesStatus.SetLabel("Scene “" + name + "” deleted")
}

// saveCfg writes the full (devices, rooms, scenes) triple from a.cfg — the
// single source of truth shared by the scenes and settings tabs.
func (a *desktopApp) saveCfg() error {
	return saveConfig(a.configPath, a.cfg.Devices, a.cfg.Rooms, a.cfg.Scenes, a.cfg.ScreenSync)
}

// closeControls tears down every device session. We don't Release() the app:
// with no matching Hold(), the app exits when its last window closes, and an
// extra Release() would underflow the use count (GLib assertion).
func (a *desktopApp) closeControls() {
	for _, ctl := range a.controls {
		ctl.Close()
	}
}
