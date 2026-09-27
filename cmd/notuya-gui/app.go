package main

import (
	"context"
	"fmt"
	"os"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/cairo"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// desktopApp is the default entry point: an ordinary GTK4 toplevel (an
// xdg-toplevel, not a layer-shell overlay) offering per-device light control,
// grouped by room. It owns one control (persistent session) per device for the
// window's lifetime and closes them all on exit.
type desktopApp struct {
	app          *adw.Application
	window       *adw.ApplicationWindow
	configPath   string
	cfg          *Config
	wheelSurface *cairo.Surface

	controls []*control // one per device, parallel to cfg.Devices
	panels   []*devicePanel

	// byID resolves device_id → control, so the scenes tab can apply a scene
	// without re-deriving the mapping.
	byID map[string]*control

	// scenesGroup + scenesStatus back the Escenas tab; rebuilt on every
	// change. sceneRows tracks the ActionRows currently in the group so they
	// can be removed on rebuild.
	scenesGroup  *adw.PreferencesGroup
	sceneRows    []*adw.ActionRow
	scenesStatus *gtk.Label

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

	// Build one control per device, keyed by device_id so the Rooms tab and
	// the Lights playground share the same session per bulb.
	panelByID := make(map[string]*devicePanel, len(a.cfg.Devices))
	for i := range a.cfg.Devices {
		ctl := newControl(a.cfg.Devices[i])
		panel := newDevicePanel(ctl)
		a.controls = append(a.controls, ctl)
		a.panels = append(a.panels, panel)
		panelByID[a.cfg.Devices[i].DeviceID] = panel
		a.byID[a.cfg.Devices[i].DeviceID] = ctl
	}

	// A ViewStack holds the three views; a ViewSwitcher in the header bar
	// selects between them (the Adwaita replacement for a Notebook's tabs).
	stack := adw.NewViewStack()
	stack.SetVExpand(true)
	stack.AddTitledWithIcon(a.buildRoomsTab(panelByID), "rooms", "Rooms", "user-home-symbolic")
	stack.AddTitledWithIcon(a.buildLightsTab(), "lights", "Lights", "weather-clear-symbolic")
	stack.AddTitledWithIcon(a.buildScenesTab(), "scenes", "Scenes", "starred-symbolic")
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

	window.ConnectCloseRequest(func() bool {
		go a.closeControls()
		return false // allow the window to close
	})

	a.app.Hold()
	window.Present()

	// Populate every panel with live device state.
	for _, panel := range a.panels {
		panel.refresh()
	}
}

// buildRoomsTab builds a mobile-style overview: one row per room with an icon,
// the room name, a live "N of M lights on" summary, and a master switch that
// powers the whole room on or off. It reuses the same controls as the Lights
// tab (looked up by device_id) so no extra sessions are opened.
func (a *desktopApp) buildRoomsTab(byID map[string]*devicePanel) *gtk.ScrolledWindow {
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
	if err := saveConfig(a.configPath, a.cfg.Devices, a.cfg.Rooms, a.cfg.Scenes); err != nil {
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

// buildSettingsTab embeds the settings UI as a tab. It seeds a settings
// instance from a.cfg and routes its Guardar back through a.cfg so the scenes
// tab and settings tab never clobber each other's slice of the config.
func (a *desktopApp) buildSettingsTab() gtk.Widgetter {
	s := &settings{
		configPath: a.configPath,
		devices:    append([]Device(nil), a.cfg.Devices...),
		rooms:      a.cfg.Rooms,
		scenes:     a.cfg.Scenes,
		selected:   -1,
	}
	// Pull the owner's current scenes and rooms at save time (the Scenes tab
	// and the Rooms tab own those slices), and mirror settings' device edits
	// back into a.cfg on save. settings.save writes the full (devices, rooms,
	// scenes) triple.
	s.scenesFn = func() []Scene { return a.cfg.Scenes }
	s.roomsFn = func() []Room { return a.cfg.Rooms }
	s.onSaved = func() {
		a.cfg.Devices = s.devices
	}
	return s.buildContent()
}

// groupPower toggles every member panel's device off the GTK thread.
func (a *desktopApp) groupPower(members []*devicePanel, on bool) {
	for _, panel := range members {
		p := panel
		go func() {
			if err := p.ctl.SetPower(context.Background(), on); err != nil {
				fmt.Fprintf(os.Stderr, "notuya-gui: %s -> %v\n", p.ctl.name(), err)
			}
		}()
	}
}

// buildScenesTab builds the "Scenes" tab: a list of saved scenes (click a row
// to apply it), a "New scene" button that opens the editor, and per-row
// "Apply"/edit/delete buttons.
func (a *desktopApp) buildScenesTab() *gtk.ScrolledWindow {
	clamp := adw.NewClamp()
	clamp.SetMaximumSize(600)
	clamp.SetVExpand(true)

	box := gtk.NewBox(gtk.OrientationVertical, 12)
	box.SetMarginTop(14)
	box.SetMarginBottom(14)
	box.SetMarginStart(14)
	box.SetMarginEnd(14)

	a.scenesGroup = adw.NewPreferencesGroup()
	a.scenesGroup.SetTitle("Scenes")
	a.scenesGroup.SetDescription("Tap “Apply” to restore a saved scene")

	saveBtn := gtk.NewButtonWithLabel("New scene")
	saveBtn.AddCSSClass("suggested-action")
	saveBtn.SetVAlign(gtk.AlignCenter)
	saveBtn.ConnectClicked(func() { a.openSceneEditor(-1) })
	a.scenesGroup.SetHeaderSuffix(saveBtn)

	a.scenesStatus = gtk.NewLabel("")
	a.scenesStatus.SetXAlign(0.0)
	a.scenesStatus.SetWrap(true)
	a.scenesStatus.AddCSSClass("dim-label")

	box.Append(a.scenesGroup)
	box.Append(a.scenesStatus)

	a.refreshScenesList()

	clamp.SetChild(box)
	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	scroll.SetChild(clamp)
	return scroll
}

// refreshScenesList rebuilds the scenes boxed list from a.cfg.Scenes, one
// ActionRow per scene with per-row "Apply" and delete buttons.
func (a *desktopApp) refreshScenesList() {
	for _, row := range a.sceneRows {
		a.scenesGroup.Remove(row)
	}
	a.sceneRows = a.sceneRows[:0]

	if len(a.cfg.Scenes) == 0 {
		row := adw.NewActionRow()
		row.SetTitle("No saved scenes")
		row.SetSubtitle("Save one to get started")
		a.scenesGroup.Add(row)
		a.sceneRows = append(a.sceneRows, row)
		return
	}

	for i := range a.cfg.Scenes {
		idx := i
		sc := a.cfg.Scenes[i]
		row := adw.NewActionRow()
		row.SetTitle(sc.Name)

		apply := gtk.NewButtonWithLabel("Apply")
		apply.SetVAlign(gtk.AlignCenter)
		apply.ConnectClicked(func() { a.applySceneAt(idx) })

		edit := gtk.NewButtonFromIconName("document-edit-symbolic")
		edit.SetVAlign(gtk.AlignCenter)
		edit.AddCSSClass("flat")
		edit.SetTooltipText("Edit scene")
		edit.ConnectClicked(func() { a.openSceneEditor(idx) })

		del := gtk.NewButtonFromIconName("user-trash-symbolic")
		del.SetVAlign(gtk.AlignCenter)
		del.AddCSSClass("flat")
		del.SetTooltipText("Delete scene")
		del.ConnectClicked(func() { a.deleteSceneAt(idx) })

		row.AddSuffix(apply)
		row.AddSuffix(edit)
		row.AddSuffix(del)
		a.scenesGroup.Add(row)
		a.sceneRows = append(a.sceneRows, row)
	}
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

// deleteSceneAt removes the scene at index i, persists, and refreshes.
func (a *desktopApp) deleteSceneAt(i int) {
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
	return saveConfig(a.configPath, a.cfg.Devices, a.cfg.Rooms, a.cfg.Scenes)
}

// closeControls tears down every device session. Runs off the GTK thread, then
// releases the app so it can exit.
func (a *desktopApp) closeControls() {
	for _, ctl := range a.controls {
		ctl.Close()
	}
	a.app.Release()
}
