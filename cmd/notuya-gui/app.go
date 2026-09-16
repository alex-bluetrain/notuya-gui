package main

import (
	"context"
	"os"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/cairo"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	coreglib "github.com/diamondburned/gotk4/pkg/glib/v2"
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

	// placed tracks which panels have already been added to the widget tree,
	// so a device that belongs to several rooms is shown (and built) once —
	// a GTK widget cannot have two parents.
	placed map[*devicePanel]bool

	// scenesGroup + scenesStatus back the Escenas tab; rebuilt on every
	// change. sceneRows tracks the ActionRows currently in the group so they
	// can be removed on rebuild.
	scenesGroup  *adw.PreferencesGroup
	sceneRows    []*adw.ActionRow
	scenesStatus *gtk.Label
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
	window.SetTitle("Luces")
	window.SetDefaultSize(520, 720)
	a.window = window

	a.placed = make(map[*devicePanel]bool, len(a.cfg.Devices))
	a.byID = make(map[string]*control, len(a.cfg.Devices))

	// Build one control per device, keyed by device_id so a room's panels
	// reuse the same session as the "Sin sala" listing would.
	panelByID := make(map[string]*devicePanel, len(a.cfg.Devices))
	for i := range a.cfg.Devices {
		ctl := newControl(a.cfg.Devices[i])
		panel := newDevicePanel(ctl, a.wheelSurface)
		a.controls = append(a.controls, ctl)
		a.panels = append(a.panels, panel)
		panelByID[a.cfg.Devices[i].DeviceID] = panel
		a.byID[a.cfg.Devices[i].DeviceID] = ctl
	}

	// A ViewStack holds the three views; a ViewSwitcher in the header bar
	// selects between them (the Adwaita replacement for a Notebook's tabs).
	stack := adw.NewViewStack()
	stack.SetVExpand(true)
	stack.AddTitledWithIcon(a.buildLightsTab(panelByID), "lights", "Luces", "weather-clear-symbolic")
	stack.AddTitledWithIcon(a.buildScenesTab(), "scenes", "Escenas", "starred-symbolic")
	stack.AddTitledWithIcon(a.buildSettingsTab(), "settings", "Ajustes", "emblem-system-symbolic")

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

// buildLightsTab builds the room-grouped device panels (the original app
// content) and returns the scrolled widget for the "Luces" tab.
func (a *desktopApp) buildLightsTab(byID map[string]*devicePanel) *gtk.ScrolledWindow {
	content := gtk.NewBox(gtk.OrientationVertical, 18)
	content.SetMarginTop(18)
	content.SetMarginBottom(18)
	content.SetMarginStart(12)
	content.SetMarginEnd(12)

	for _, group := range groupByRoom(a.cfg) {
		content.Append(a.buildRoomSection(group, byID))
	}

	// Clamp keeps the column at a comfortable width and centres it on wide
	// windows, like a native GNOME app.
	clamp := adw.NewClamp()
	clamp.SetMaximumSize(600)
	clamp.SetChild(content)

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	scroll.SetChild(clamp)
	return scroll
}

// buildSettingsTab embeds the settings UI as a tab. It seeds a settings
// instance from a.cfg and routes its Guardar back through a.cfg so the scenes
// tab and settings tab never clobber each other's slice of the config.
func (a *desktopApp) buildSettingsTab() gtk.Widgetter {
	s := &settings{
		configPath:   a.configPath,
		devices:      append([]Device(nil), a.cfg.Devices...),
		rooms:        append([]Room(nil), a.cfg.Rooms...),
		scenes:       a.cfg.Scenes,
		selected:     -1,
		roomSelected: -1,
	}
	// Always save the owner's current scenes, and mirror settings' edits back
	// into a.cfg on save, so the scenes tab and settings tab never clobber each
	// other's slice. settings.save writes the full (devices, rooms, scenes) triple.
	s.scenesFn = func() []Scene { return a.cfg.Scenes }
	s.onSaved = func() {
		a.cfg.Devices = s.devices
		a.cfg.Rooms = s.rooms
	}
	return s.buildContent()
}

// buildRoomSection renders one room: a header with group actions followed by
// the panels for its devices (looked up by id so each device has exactly one
// panel/session even if it appears in several rooms — here we build the panel
// once and reference it).
func (a *desktopApp) buildRoomSection(group roomGroup, byID map[string]*devicePanel) *adw.PreferencesGroup {
	section := adw.NewPreferencesGroup()
	section.SetTitle(group.Name)

	// Group actions fan out to every member device.
	members := make([]*devicePanel, 0, len(group.Devices))
	for _, dev := range group.Devices {
		if panel, ok := byID[dev.DeviceID]; ok {
			members = append(members, panel)
		}
	}

	// "Todo ON/OFF" live in the group's header suffix (top-right of the card).
	actions := gtk.NewBox(gtk.OrientationHorizontal, 6)
	actions.SetVAlign(gtk.AlignCenter)
	allOn := gtk.NewButtonWithLabel("Todo ON")
	allOn.AddCSSClass("flat")
	allOn.ConnectClicked(func() { a.groupPower(members, true) })
	allOff := gtk.NewButtonWithLabel("Todo OFF")
	allOff.AddCSSClass("flat")
	allOff.ConnectClicked(func() { a.groupPower(members, false) })
	actions.Append(allOn)
	actions.Append(allOff)
	section.SetHeaderSuffix(actions)

	for _, panel := range members {
		if a.placed[panel] {
			continue // already shown in an earlier room; one widget, one parent
		}
		a.placed[panel] = true
		section.Add(panel.build())
	}
	return section
}

// groupPower toggles every member panel's device, off the GTK thread, then
// refreshes each panel.
func (a *desktopApp) groupPower(members []*devicePanel, on bool) {
	for _, panel := range members {
		p := panel
		p.runAsync(func(ctx context.Context) error { return p.ctl.SetPower(ctx, on) })
	}
}

// buildScenesTab builds the "Escenas" tab: a list of saved scenes (click a row
// to apply it), a "Guardar escena" button that snapshots selected lights, and
// an "Eliminar" button that removes the selected scene.
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
	a.scenesGroup.SetTitle("Escenas")
	a.scenesGroup.SetDescription("Toca «Aplicar» para restaurar una escena guardada")

	saveBtn := gtk.NewButtonWithLabel("Guardar escena")
	saveBtn.AddCSSClass("suggested-action")
	saveBtn.SetVAlign(gtk.AlignCenter)
	saveBtn.ConnectClicked(func() { a.openSaveSceneDialog() })
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
// ActionRow per scene with per-row "Aplicar" and delete buttons.
func (a *desktopApp) refreshScenesList() {
	for _, row := range a.sceneRows {
		a.scenesGroup.Remove(row)
	}
	a.sceneRows = a.sceneRows[:0]

	if len(a.cfg.Scenes) == 0 {
		row := adw.NewActionRow()
		row.SetTitle("Sin escenas guardadas")
		row.SetSubtitle("Guarda una para empezar")
		a.scenesGroup.Add(row)
		a.sceneRows = append(a.sceneRows, row)
		return
	}

	for i := range a.cfg.Scenes {
		idx := i
		sc := a.cfg.Scenes[i]
		row := adw.NewActionRow()
		row.SetTitle(sc.Name)

		apply := gtk.NewButtonWithLabel("Aplicar")
		apply.SetVAlign(gtk.AlignCenter)
		apply.ConnectClicked(func() { a.applySceneAt(idx) })

		del := gtk.NewButtonFromIconName("user-trash-symbolic")
		del.SetVAlign(gtk.AlignCenter)
		del.AddCSSClass("flat")
		del.SetTooltipText("Eliminar escena")
		del.ConnectClicked(func() { a.deleteSceneAt(idx) })

		row.AddSuffix(apply)
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
	a.scenesStatus.SetLabel("Aplicando '" + sc.Name + "'…")
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
		a.scenesStatus.SetLabel("Error al guardar: " + err.Error())
		return
	}
	a.refreshScenesList()
	a.scenesStatus.SetLabel("Escena '" + name + "' eliminada")
}

// openSaveSceneDialog prompts for a name and which lights to include, then
// captures their current state into a new scene.
func (a *desktopApp) openSaveSceneDialog() {
	dialog := adw.NewMessageDialog(&a.window.Window, "Guardar escena", "Elige un nombre y las luces a incluir.")

	content := gtk.NewBox(gtk.OrientationVertical, 8)
	content.SetMarginTop(6)

	nameEntry := gtk.NewEntry()
	nameEntry.SetPlaceholderText("Nombre de la escena")
	content.Append(nameEntry)

	group := adw.NewPreferencesGroup()
	group.SetTitle("Luces a incluir")
	checks := make([]*gtk.CheckButton, len(a.cfg.Devices))
	for i := range a.cfg.Devices {
		name := a.cfg.Devices[i].Name
		if name == "" {
			name = a.cfg.Devices[i].DeviceID
		}
		row := adw.NewActionRow()
		row.SetTitle(name)
		cb := gtk.NewCheckButton()
		cb.SetActive(true) // default: all ticked
		cb.SetVAlign(gtk.AlignCenter)
		row.AddPrefix(cb)
		row.SetActivatableWidget(cb)
		checks[i] = cb
		group.Add(row)
	}
	content.Append(group)
	dialog.SetExtraChild(content)

	dialog.AddResponse("cancel", "Cancelar")
	dialog.AddResponse("save", "Guardar")
	dialog.SetResponseAppearance("save", adw.ResponseSuggested)
	dialog.SetDefaultResponse("save")
	dialog.SetCloseResponse("cancel")

	dialog.ConnectResponse(func(response string) {
		if response == "save" {
			name := nameEntry.Text()
			include := make([]bool, len(checks))
			for i, cb := range checks {
				include[i] = cb.Active()
			}
			a.captureAndSaveScene(name, include)
		}
	})
	dialog.Present()
}

// captureAndSaveScene snapshots the included lights off the GTK thread, appends
// the scene, persists, and refreshes the list back on the GTK thread.
func (a *desktopApp) captureAndSaveScene(name string, include []bool) {
	if name == "" {
		a.scenesStatus.SetLabel("La escena necesita un nombre")
		return
	}
	a.scenesStatus.SetLabel("Capturando '" + name + "'…")
	go func() {
		scene := captureScene(name, a.controls, include)
		coreglib.IdleAdd(func() {
			if len(scene.States) == 0 {
				a.scenesStatus.SetLabel("No se pudo leer ninguna luz; escena no guardada")
				return
			}
			a.cfg.Scenes = append(a.cfg.Scenes, scene)
			if err := a.saveCfg(); err != nil {
				a.cfg.Scenes = a.cfg.Scenes[:len(a.cfg.Scenes)-1]
				a.scenesStatus.SetLabel("Error al guardar: " + err.Error())
				return
			}
			a.refreshScenesList()
			a.scenesStatus.SetLabel("Escena '" + name + "' guardada")
		})
	}()
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
