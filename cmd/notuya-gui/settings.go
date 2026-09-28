package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/alex-bluetrain/notuya-go/pkg/discovery"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	coreglib "github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// scanTimeout bounds a discovery.Scan; long enough to catch the ~1Hz
// unsolicited beacons most bulbs emit, short enough to feel responsive.
const scanTimeout = 4 * time.Second

// settings is the config-editing + discovery window. Unlike the picker it is
// an ordinary GTK toplevel (no layer-shell), and it never talks to the bulbs:
// it only edits the shared config.json. State lives in an in-memory slice
// seeded from the loaded config; nothing is written until Save.
type settings struct {
	app        *adw.Application
	window     *adw.ApplicationWindow
	configPath string

	devices  []Device
	rooms    []Room
	scenes   []Scene // preserved on save; not edited by the settings UI
	selected int     // index into devices, or -1

	// scenesFn / roomsFn, when set (embedded-tab mode), supply the current
	// scenes and rooms at save time so the settings tab doesn't clobber what
	// the Scenes tab or the Rooms tab changed (room management now lives in the
	// Rooms tab). onSaved, when set, mirrors settings' device edits back to the
	// owner (the desktop app's a.cfg) after a successful write.
	scenesFn func() []Scene
	roomsFn  func() []Room
	onSaved  func()

	list      *gtk.ListBox
	nameEntry *adw.EntryRow
	ipEntry   *adw.EntryRow
	idEntry   *adw.EntryRow
	keyEntry  *adw.EntryRow
	status    *gtk.Label
	scanBtn   *gtk.Button
	scanList  *gtk.ListBox
}

func runSettings(configPath string, devices []Device, rooms []Room, scenes []Scene) int {
	s := &settings{
		configPath: configPath,
		devices:    append([]Device(nil), devices...),
		rooms:      append([]Room(nil), rooms...),
		scenes:     append([]Scene(nil), scenes...),
		selected:   -1,
	}
	s.app = adw.NewApplication("ar.averstraeten.tuyawheel.settings", gio.ApplicationNonUnique)
	s.app.ConnectActivate(func() { s.activate() })
	return s.app.Run(os.Args[:1])
}

func (s *settings) activate() {
	window := adw.NewApplicationWindow(&s.app.Application)
	window.SetTitle("Lights — Settings")
	window.SetDefaultSize(560, 560)
	s.window = window

	header := adw.NewHeaderBar()
	root := gtk.NewBox(gtk.OrientationVertical, 0)
	root.Append(header)
	content := s.buildContent()
	content.SetVExpand(true)
	root.Append(content)
	window.SetContent(root)

	s.app.Hold()
	window.ConnectCloseRequest(func() bool {
		s.app.Release()
		return false
	})
	window.Present()
}

// buildContent assembles the device + discovery UI and returns its root
// widget, without creating a window or application. runSettings' activate wraps
// it in a standalone window; the desktop app embeds it as a tab.
func (s *settings) buildContent() *gtk.ScrolledWindow {
	// --- Device list (left) ---
	s.list = gtk.NewListBox()
	s.list.SetVExpand(true)
	s.list.ConnectRowSelected(func(row *gtk.ListBoxRow) {
		if row == nil {
			s.selected = -1
			return
		}
		s.selected = row.Index()
		s.loadForm(s.devices[s.selected])
	})
	listScroll := gtk.NewScrolledWindow()
	listScroll.SetChild(s.list)
	listScroll.SetVExpand(true)
	listScroll.SetHExpand(true)

	// --- Edit form ---
	s.nameEntry = adw.NewEntryRow()
	s.nameEntry.SetTitle("Name")
	s.ipEntry = adw.NewEntryRow()
	s.ipEntry.SetTitle("IP address")
	s.idEntry = adw.NewEntryRow()
	s.idEntry.SetTitle("Device ID")
	s.keyEntry = adw.NewEntryRow()
	s.keyEntry.SetTitle("Local Key")

	form := adw.NewPreferencesGroup()
	form.SetTitle("Device details")
	form.Add(s.nameEntry)
	form.Add(s.ipEntry)
	form.Add(s.idEntry)
	form.Add(s.keyEntry)

	addBtn := gtk.NewButtonWithLabel("Add / Update")
	addBtn.ConnectClicked(func() { s.upsert() })
	removeBtn := gtk.NewButtonWithLabel("Remove")
	removeBtn.ConnectClicked(func() { s.remove() })
	formButtons := gtk.NewBox(gtk.OrientationHorizontal, 8)
	formButtons.SetHAlign(gtk.AlignEnd)
	formButtons.Append(removeBtn)
	formButtons.Append(addBtn)

	// --- Discovery panel ---
	s.scanBtn = gtk.NewButtonWithLabel("Scan for devices")
	s.scanBtn.ConnectClicked(func() { s.scan() })
	s.scanList = gtk.NewListBox()
	s.scanList.ConnectRowSelected(func(row *gtk.ListBoxRow) {
		if row != nil {
			s.pickDiscovered(row.Index())
		}
	})
	scanScroll := gtk.NewScrolledWindow()
	scanScroll.SetChild(s.scanList)
	scanScroll.SetMinContentHeight(120)
	scanScroll.SetVExpand(true)

	// Room management lives in the Rooms tab now (see buildRoomManagement in
	// app.go); the settings UI only edits devices + discovery. It still keeps
	// the rooms slice so a standalone -config save doesn't clobber them.

	// --- Status + window buttons ---
	s.status = gtk.NewLabel("")
	s.status.SetXAlign(0.0)
	s.status.SetWrap(true)

	saveBtn := gtk.NewButtonWithLabel("Save")
	saveBtn.AddCSSClass("suggested-action")
	saveBtn.ConnectClicked(func() { s.save() })
	winButtons := gtk.NewBox(gtk.OrientationHorizontal, 8)
	winButtons.SetHAlign(gtk.AlignEnd)
	// The Cerrar button only makes sense in the standalone window; when the
	// settings UI is embedded as a tab (no s.window) there is nothing to close.
	if s.window != nil {
		closeBtn := gtk.NewButtonWithLabel("Close")
		closeBtn.ConnectClicked(func() { s.window.Close() })
		winButtons.Append(closeBtn)
	}
	winButtons.Append(saveBtn)

	// --- Layout ---
	devicesGroup := adw.NewPreferencesGroup()
	devicesGroup.SetTitle("Devices")
	devicesGroup.Add(listScroll)

	discoveryGroup := adw.NewPreferencesGroup()
	discoveryGroup.SetTitle("Discovery")
	discoveryGroup.SetHeaderSuffix(s.scanBtn)
	discoveryGroup.Add(scanScroll)

	content := gtk.NewBox(gtk.OrientationVertical, 18)
	content.SetMarginTop(16)
	content.SetMarginBottom(16)
	content.SetMarginStart(16)
	content.SetMarginEnd(16)
	content.Append(devicesGroup)
	content.Append(form)
	content.Append(formButtons)
	content.Append(discoveryGroup)
	content.Append(s.status)
	content.Append(winButtons)

	clamp := adw.NewClamp()
	clamp.SetMaximumSize(600)
	clamp.SetChild(content)

	contentScroll := gtk.NewScrolledWindow()
	contentScroll.SetChild(clamp)
	contentScroll.SetVExpand(true)

	s.refreshList()
	return contentScroll
}

// refreshList rebuilds the device ListBox from the in-memory slice.
func (s *settings) refreshList() {
	for {
		row := s.list.RowAtIndex(0)
		if row == nil {
			break
		}
		s.list.Remove(row)
	}
	for _, d := range s.devices {
		name := d.Name
		if name == "" {
			name = "(unnamed)"
		}
		label := gtk.NewLabel(fmt.Sprintf("%s — %s", name, d.IPAddress))
		label.SetXAlign(0.0)
		label.SetMarginTop(4)
		label.SetMarginBottom(4)
		label.SetMarginStart(6)
		s.list.Append(label)
	}
}

func (s *settings) loadForm(d Device) {
	s.nameEntry.SetText(d.Name)
	s.ipEntry.SetText(d.IPAddress)
	s.idEntry.SetText(d.DeviceID)
	s.keyEntry.SetText(d.LocalKey)
}

func (s *settings) formDevice() Device {
	return Device{
		Name:      s.nameEntry.Text(),
		IPAddress: s.ipEntry.Text(),
		DeviceID:  s.idEntry.Text(),
		LocalKey:  s.keyEntry.Text(),
	}
}

// upsert adds a new device or updates an existing one keyed by device_id. A
// blank device_id is rejected — it is the identity used for dedupe and by the
// streaming layer.
func (s *settings) upsert() {
	d := s.formDevice()
	if d.DeviceID == "" {
		s.setStatus("Device ID is required.")
		return
	}
	for i := range s.devices {
		if s.devices[i].DeviceID == d.DeviceID {
			s.devices[i] = d
			s.refreshList()
			s.setStatus(fmt.Sprintf("Updated: %s", d.DeviceID))
			return
		}
	}
	s.devices = append(s.devices, d)
	s.refreshList()
	s.setStatus(fmt.Sprintf("Added: %s", d.DeviceID))
}

func (s *settings) remove() {
	if s.selected < 0 || s.selected >= len(s.devices) {
		s.setStatus("Select a device to remove.")
		return
	}
	removed := s.devices[s.selected]
	s.devices = append(s.devices[:s.selected], s.devices[s.selected+1:]...)
	s.selected = -1
	s.refreshList()
	s.setStatus(fmt.Sprintf("Removed: %s", removed.DeviceID))
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

func (s *settings) save() {
	scenes := s.scenes
	if s.scenesFn != nil {
		scenes = s.scenesFn() // pull the owner's current scenes
	}
	rooms := s.rooms
	if s.roomsFn != nil {
		rooms = s.roomsFn() // pull the owner's current rooms (Rooms tab owns them)
	}
	if err := saveConfig(s.configPath, s.devices, rooms, scenes); err != nil {
		s.setStatus("Save failed: " + err.Error())
		return
	}
	if s.onSaved != nil {
		s.onSaved()
	}
	s.setStatus(fmt.Sprintf("Saved to %s", s.configPath))
}

func (s *settings) setStatus(msg string) {
	s.status.SetText(msg)
}

// scanned holds discovery results between a Scan and a click on a result row;
// the row index maps back into this slice.
type scannedRow struct {
	dev     discovery.Device
	present bool // already in the config
}

var scannedRows []scannedRow

// scan runs discovery.Scan off the GTK thread and repopulates the results
// list via IdleAdd. Scan errors are surfaced inline and are non-fatal.
func (s *settings) scan() {
	s.scanBtn.SetSensitive(false)
	s.setStatus("Scanning…")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), scanTimeout)
		defer cancel()
		devs, err := discovery.Scan(ctx, scanTimeout)
		coreglib.IdleAdd(func() {
			s.scanBtn.SetSensitive(true)
			if err != nil {
				s.setStatus("Scan error: " + err.Error())
				return
			}
			s.showScanResults(devs)
		})
	}()
}

func (s *settings) showScanResults(devs []discovery.Device) {
	for {
		row := s.scanList.RowAtIndex(0)
		if row == nil {
			break
		}
		s.scanList.Remove(row)
	}
	scannedRows = scannedRows[:0]

	for _, d := range devs {
		present := false
		for _, cd := range s.devices {
			if cd.DeviceID == d.ID {
				present = true
				break
			}
		}
		scannedRows = append(scannedRows, scannedRow{dev: d, present: present})

		suffix := ""
		if present {
			suffix = "  (already added)"
		}
		label := gtk.NewLabel(fmt.Sprintf("%s — %s  v%s%s", d.ID, d.IP, d.Version, suffix))
		label.SetXAlign(0.0)
		label.SetMarginStart(6)
		s.scanList.Append(label)
	}

	if len(devs) == 0 {
		s.setStatus("No devices found.")
		return
	}
	s.setStatus(fmt.Sprintf("Found %d device(s). Select one to fill in the form.", len(devs)))
}

// pickDiscovered pre-fills IP + Device ID from a discovered device; the user
// still types the Local Key by hand (discovery can't supply it).
func (s *settings) pickDiscovered(index int) {
	if index < 0 || index >= len(scannedRows) {
		return
	}
	d := scannedRows[index].dev
	s.ipEntry.SetText(d.IP)
	s.idEntry.SetText(d.ID)
	if !scannedRows[index].present {
		s.keyEntry.GrabFocus()
		s.setStatus("Enter the Local Key and press Add.")
	} else {
		s.setStatus("This device is already in the configuration.")
	}
}
