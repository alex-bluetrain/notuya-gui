package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/averstraeten/notuya-go/pkg/discovery"
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
// seeded from the loaded config; nothing is written until Guardar.
type settings struct {
	app        *gtk.Application
	window     *gtk.Window
	configPath string

	devices  []Device
	selected int // index into devices, or -1

	list      *gtk.ListBox
	nameEntry *gtk.Entry
	ipEntry   *gtk.Entry
	idEntry   *gtk.Entry
	keyEntry  *gtk.Entry
	status    *gtk.Label
	scanBtn   *gtk.Button
	scanList  *gtk.ListBox
}

func runSettings(configPath string, devices []Device) int {
	s := &settings{
		configPath: configPath,
		devices:    append([]Device(nil), devices...),
		selected:   -1,
	}
	s.app = gtk.NewApplication("ar.averstraeten.tuyawheel.settings", gio.ApplicationNonUnique)
	s.app.ConnectActivate(func() { s.activate() })
	return s.app.Run(os.Args[:1])
}

func (s *settings) activate() {
	window := gtk.NewWindow()
	window.SetTitle("Luces — Configuración")
	window.SetDefaultSize(560, 520)
	s.window = window

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
	s.nameEntry = gtk.NewEntry()
	s.nameEntry.SetPlaceholderText("Nombre")
	s.ipEntry = gtk.NewEntry()
	s.ipEntry.SetPlaceholderText("Dirección IP")
	s.idEntry = gtk.NewEntry()
	s.idEntry.SetPlaceholderText("Device ID")
	s.keyEntry = gtk.NewEntry()
	s.keyEntry.SetPlaceholderText("Local Key")

	form := gtk.NewGrid()
	form.SetRowSpacing(6)
	form.SetColumnSpacing(8)
	addRow := func(row int, label string, entry *gtk.Entry) {
		l := gtk.NewLabel(label)
		l.SetXAlign(0.0)
		entry.SetHExpand(true)
		form.Attach(l, 0, row, 1, 1)
		form.Attach(entry, 1, row, 1, 1)
	}
	addRow(0, "Nombre", s.nameEntry)
	addRow(1, "IP", s.ipEntry)
	addRow(2, "Device ID", s.idEntry)
	addRow(3, "Local Key", s.keyEntry)

	addBtn := gtk.NewButtonWithLabel("Añadir / Actualizar")
	addBtn.ConnectClicked(func() { s.upsert() })
	removeBtn := gtk.NewButtonWithLabel("Eliminar")
	removeBtn.ConnectClicked(func() { s.remove() })
	formButtons := gtk.NewBox(gtk.OrientationHorizontal, 8)
	formButtons.SetHAlign(gtk.AlignEnd)
	formButtons.Append(removeBtn)
	formButtons.Append(addBtn)

	// --- Discovery panel ---
	s.scanBtn = gtk.NewButtonWithLabel("Buscar dispositivos")
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

	// --- Status + window buttons ---
	s.status = gtk.NewLabel("")
	s.status.SetXAlign(0.0)
	s.status.SetWrap(true)

	saveBtn := gtk.NewButtonWithLabel("Guardar")
	saveBtn.AddCSSClass("suggested-action")
	saveBtn.ConnectClicked(func() { s.save() })
	closeBtn := gtk.NewButtonWithLabel("Cerrar")
	closeBtn.ConnectClicked(func() { s.window.Close() })
	winButtons := gtk.NewBox(gtk.OrientationHorizontal, 8)
	winButtons.SetHAlign(gtk.AlignEnd)
	winButtons.Append(closeBtn)
	winButtons.Append(saveBtn)

	// --- Layout ---
	sep := func() *gtk.Separator { return gtk.NewSeparator(gtk.OrientationHorizontal) }
	content := gtk.NewBox(gtk.OrientationVertical, 12)
	content.SetMarginTop(16)
	content.SetMarginBottom(16)
	content.SetMarginStart(16)
	content.SetMarginEnd(16)
	content.Append(gtk.NewLabel("Dispositivos"))
	content.Append(listScroll)
	content.Append(form)
	content.Append(formButtons)
	content.Append(sep())
	content.Append(s.scanBtn)
	content.Append(scanScroll)
	content.Append(sep())
	content.Append(s.status)
	content.Append(winButtons)
	window.SetChild(content)

	s.refreshList()
	s.app.Hold()
	window.ConnectCloseRequest(func() bool {
		s.app.Release()
		return false
	})
	window.Present()
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
			name = "(sin nombre)"
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
		s.setStatus("Device ID es obligatorio.")
		return
	}
	for i := range s.devices {
		if s.devices[i].DeviceID == d.DeviceID {
			s.devices[i] = d
			s.refreshList()
			s.setStatus(fmt.Sprintf("Actualizado: %s", d.DeviceID))
			return
		}
	}
	s.devices = append(s.devices, d)
	s.refreshList()
	s.setStatus(fmt.Sprintf("Añadido: %s", d.DeviceID))
}

func (s *settings) remove() {
	if s.selected < 0 || s.selected >= len(s.devices) {
		s.setStatus("Selecciona un dispositivo para eliminar.")
		return
	}
	removed := s.devices[s.selected]
	s.devices = append(s.devices[:s.selected], s.devices[s.selected+1:]...)
	s.selected = -1
	s.refreshList()
	s.setStatus(fmt.Sprintf("Eliminado: %s", removed.DeviceID))
}

func (s *settings) save() {
	if err := saveConfig(s.configPath, s.devices); err != nil {
		s.setStatus("Error al guardar: " + err.Error())
		return
	}
	s.setStatus(fmt.Sprintf("Guardado en %s", s.configPath))
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
	s.setStatus("Buscando…")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), scanTimeout)
		defer cancel()
		devs, err := discovery.Scan(ctx, scanTimeout)
		coreglib.IdleAdd(func() {
			s.scanBtn.SetSensitive(true)
			if err != nil {
				s.setStatus("Error de búsqueda: " + err.Error())
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
			suffix = "  (ya añadido)"
		}
		label := gtk.NewLabel(fmt.Sprintf("%s — %s  v%s%s", d.ID, d.IP, d.Version, suffix))
		label.SetXAlign(0.0)
		label.SetMarginStart(6)
		s.scanList.Append(label)
	}

	if len(devs) == 0 {
		s.setStatus("No se encontraron dispositivos.")
		return
	}
	s.setStatus(fmt.Sprintf("Encontrados %d dispositivo(s). Selecciona uno para rellenar el formulario.", len(devs)))
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
		s.setStatus("Introduce el Local Key y pulsa Añadir.")
	} else {
		s.setStatus("Este dispositivo ya está en la configuración.")
	}
}
