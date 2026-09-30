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

// wizardTestTimeout bounds one wizard test step (open a throwaway session and
// send one command). Generous relative to commandTimeout since the wizard
// talks to a bulb that has never been configured before, over a possibly
// slower first connection.
const wizardTestTimeout = 5 * time.Second

// localKeyLen is the exact length of a Tuya local key (a 16-byte AES-128 key
// entered as a 16-character string). The connectivity test only fires once a
// key field holds exactly this many characters.
const localKeyLen = 16

// wizardDeviceName is the auto-generated default name for the i-th discovered
// device (0-based), shown as the row title until renamed in Settings.
func wizardDeviceName(i int) string {
	return fmt.Sprintf("Light %d", i+1)
}

// keyComplete reports whether a Local Key field holds a complete key.
func keyComplete(key string) bool {
	return len(key) == localKeyLen
}

// foundLightsTitle is the boxed-list title for n discovered devices.
func foundLightsTitle(n int) string {
	if n == 1 {
		return "Found 1 light"
	}
	return fmt.Sprintf("Found %d lights", n)
}

// runWizard runs the first-run setup flow standalone: it scans the LAN,
// lets the user name each discovered bulb and paste in its Local Key, tests
// each one live (flashing red/blue/white so the user can visually confirm
// which physical bulb answered), and on Apply writes config.json and hands
// off to the normal app. It is a separate GApplication (like -config), not a
// tab, because it exists to build a config the rest of the app needs to run.
// runWizard runs the setup flow and returns the process exit code from the
// GApplication run, plus whether the user completed setup (Apply). When
// applied is true the caller should load the freshly written config and start
// the app; when false the user closed the wizard without finishing.
func runWizard(configPath string) (code int, applied bool) {
	w := &wizard{configPath: configPath}
	w.app = adw.NewApplication("ar.averstraeten.tuyawheel.wizard", gio.ApplicationNonUnique)
	w.app.ConnectActivate(func() { w.activate() })
	code = w.app.Run(os.Args[:1])
	return code, w.applied
}

type wizard struct {
	app        *adw.Application
	window     *adw.ApplicationWindow
	configPath string

	toolbar     *adw.ToolbarView
	stack       *gtk.Stack
	scanSpinner *adw.Spinner
	deviceList  *adw.PreferencesGroup // one boxed list holding every device row
	applyBar    *gtk.Box
	applyBtn    *gtk.Button

	rows    []*wizardRow
	applied bool // set true once the user completes setup (Apply)
}

// wizardRow is one discovered device shown as a single AdwActionRow in the
// shared boxed list: a leading bulb icon (filled once tested), the device name
// as the row title with its IP · device-id as the subtitle, then a masked
// Local Key entry, a "Test" button, and a spinner as suffixes.
type wizardRow struct {
	dev  discovery.Device
	name string

	row        *adw.ActionRow
	icon       *gtk.Image
	keyEntry   *gtk.PasswordEntry
	testBtn    *gtk.Button
	statusSpin *gtk.Spinner

	tested   bool // the device answered a full test sequence successfully
	inFlight bool // a test is currently running for this row
}

func (w *wizard) activate() {
	window := adw.NewApplicationWindow(&w.app.Application)
	window.SetTitle("Set Up Lights")
	window.SetDefaultSize(720, 640)
	w.window = window

	header := adw.NewHeaderBar()

	w.stack = gtk.NewStack()
	w.stack.SetVExpand(true)
	w.stack.SetTransitionType(gtk.StackTransitionTypeCrossfade)
	w.stack.AddNamed(w.buildScanningPage(), "scanning")
	w.stack.AddNamed(w.buildResultsPage(), "results")
	w.stack.AddNamed(w.buildEmptyPage(), "empty")
	w.stack.SetVisibleChildName("scanning")

	w.toolbar = adw.NewToolbarView()
	w.toolbar.AddTopBar(header)
	w.toolbar.SetContent(w.stack)
	w.toolbar.AddBottomBar(w.applyBar)
	w.toolbar.SetRevealBottomBars(false)

	window.SetContent(w.toolbar)
	window.Present()

	w.scan()
}

// buildScanningPage shows a spinner while the LAN scan runs.
func (w *wizard) buildScanningPage() *adw.StatusPage {
	page := adw.NewStatusPage()
	page.SetTitle("Looking for Lights")
	page.SetDescription("Scanning your network for Tuya bulbs…")
	w.scanSpinner = adw.NewSpinner()
	w.scanSpinner.SetSizeRequest(48, 48)
	page.SetChild(w.scanSpinner)
	return page
}

// buildEmptyPage shows when the scan finds nothing, with a way to retry.
func (w *wizard) buildEmptyPage() *adw.StatusPage {
	page := adw.NewStatusPage()
	page.SetIconName("network-wireless-offline-symbolic")
	page.SetTitle("No Bulbs Found")
	page.SetDescription("Make sure your lights are powered on and connected to the same network, then try again.")

	rescan := gtk.NewButtonWithLabel("Scan Again")
	rescan.AddCSSClass("suggested-action")
	rescan.SetHAlign(gtk.AlignCenter)
	rescan.ConnectClicked(func() { w.rescan() })
	page.SetChild(rescan)
	return page
}

// buildResultsPage lists the discovered devices for naming, key entry, and
// per-device testing. The primary action (Apply) lives in a bottom bar
// pinned by the ToolbarView, so it's always visible, never scrolled away.
func (w *wizard) buildResultsPage() *gtk.ScrolledWindow {
	rescan := gtk.NewButtonWithLabel("Scan Again")
	rescan.SetVAlign(gtk.AlignCenter)
	rescan.ConnectClicked(func() { w.rescan() })

	// One boxed list holding every discovered device as a row. The group
	// title ("Found N lights") and its Scan Again header-suffix are stock
	// AdwPreferencesGroup slots, and each row is a stock AdwActionRow, so
	// the theme owns all padding, shape and separators — no manual boxes,
	// no custom CSS.
	w.deviceList = adw.NewPreferencesGroup()
	w.deviceList.SetHeaderSuffix(rescan)

	content := gtk.NewBox(gtk.OrientationVertical, 18)
	content.SetMarginTop(24)
	content.SetMarginBottom(24)
	content.SetMarginStart(12)
	content.SetMarginEnd(12)
	content.Append(w.deviceList)

	clamp := adw.NewClamp()
	clamp.SetMaximumSize(720)
	clamp.SetChild(content)

	scroll := gtk.NewScrolledWindow()
	scroll.SetChild(clamp)
	scroll.SetVExpand(true)

	w.applyBtn = gtk.NewButtonWithLabel("Apply")
	w.applyBtn.AddCSSClass("suggested-action")
	w.applyBtn.SetSensitive(false)
	w.applyBtn.ConnectClicked(func() { w.apply() })

	w.applyBar = gtk.NewBox(gtk.OrientationHorizontal, 8)
	w.applyBar.AddCSSClass("toolbar")
	w.applyBar.SetHAlign(gtk.AlignEnd)
	w.applyBar.Append(w.applyBtn)

	return scroll
}

// rescan switches to the scanning page and starts a fresh scan.
func (w *wizard) rescan() {
	w.toolbar.SetRevealBottomBars(false)
	w.stack.SetVisibleChildName("scanning")
	w.scan()
}

// scan runs discovery.Scan off the GTK thread, then shows the empty page or
// populates the results page depending on what it found.
func (w *wizard) scan() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), scanTimeout)
		defer cancel()
		devs, err := discovery.Scan(ctx, scanTimeout)
		coreglib.IdleAdd(func() {
			if err != nil || len(devs) == 0 {
				w.stack.SetVisibleChildName("empty")
				return
			}
			w.showResults(devs)
			w.stack.SetVisibleChildName("results")
			w.toolbar.SetRevealBottomBars(true)
		})
	}()
}

// showResults rebuilds the results list from a fresh scan, discarding any
// cards from a previous scan so rescanning never leaves stale rows behind.
func (w *wizard) showResults(devs []discovery.Device) {
	for _, r := range w.rows {
		w.deviceList.Remove(r.row)
	}
	w.rows = w.rows[:0]

	w.deviceList.SetTitle(foundLightsTitle(len(devs)))

	for i, d := range devs {
		w.deviceList.Add(w.buildDeviceCard(i, d))
	}
	w.updateApplySensitivity()
}

// buildDeviceCard builds one AdwActionRow for a discovered device: a leading
// bulb icon (filled once tested, dim otherwise), the device name as the row
// title with its IP · device-id as the subtitle, then a masked Local Key
// entry, a "Test" button, and a spinner as suffixes. It is a stock boxed-list
// row, so the theme owns all padding, shape, separators and spacing — no
// manual boxes, no custom CSS. The name is read-only here (renaming lives in
// Settings). The user enters the key, then clicks Test to flash the bulb and
// confirm the key works; the button enables only once the key is 16 chars.
func (w *wizard) buildDeviceCard(i int, d discovery.Device) *adw.ActionRow {
	row := &wizardRow{dev: d, name: wizardDeviceName(i)}

	card := adw.NewActionRow()
	card.SetTitle(row.name)
	card.SetSubtitle(d.IP + " · " + d.ID)
	row.row = card

	// Adwaita ships no lamp icon; emoji-objects-symbolic (the "Objects"
	// category in GTK's emoji chooser) is a light bulb and is the closest
	// stock match. It dims until the device tests good (see refreshIcon).
	row.icon = gtk.NewImageFromIconName("emoji-objects-symbolic")
	card.AddPrefix(row.icon)

	row.keyEntry = gtk.NewPasswordEntry()
	row.keyEntry.SetShowPeekIcon(true)
	row.keyEntry.SetVAlign(gtk.AlignCenter)
	row.keyEntry.SetWidthChars(18)
	row.keyEntry.SetObjectProperty("placeholder-text", "Local key")
	row.keyEntry.SetTooltipText("Local key (16 characters)")
	row.keyEntry.ConnectChanged(func() { w.onKeyChanged(row) })
	// Enter in the key field also runs the test, when the key is complete.
	row.keyEntry.ConnectActivate(func() {
		if !row.inFlight && keyComplete(row.keyEntry.Text()) {
			w.testRow(row)
		}
	})

	row.testBtn = gtk.NewButtonWithLabel("Test")
	row.testBtn.SetVAlign(gtk.AlignCenter)
	row.testBtn.SetTooltipText("Flash the bulb to confirm the key works")
	row.testBtn.SetSensitive(false)
	row.testBtn.ConnectClicked(func() { w.testRow(row) })

	row.statusSpin = gtk.NewSpinner()
	row.statusSpin.SetVisible(false)
	row.statusSpin.SetVAlign(gtk.AlignCenter)

	card.AddSuffix(row.keyEntry)
	card.AddSuffix(row.testBtn)
	card.AddSuffix(row.statusSpin)

	w.refreshIcon(row)
	w.rows = append(w.rows, row)
	return card
}

// refreshIcon colours the leading bulb icon to signal the row's state — the
// visual cue from the mockup, and the wizard's only status indicator now that
// the status label is gone. A tested-good device lights the bulb (accent); a
// device that failed its test shows error red; anything else (no key, key not
// yet 16 chars, or not yet tested) stays dim.
func (w *wizard) refreshIcon(row *wizardRow) {
	for _, c := range []string{"dim-label", "accent", "error"} {
		row.icon.RemoveCSSClass(c)
	}
	switch {
	case row.tested:
		row.icon.AddCSSClass("accent")
	case row.keyEntry.HasCSSClass("error"):
		row.icon.AddCSSClass("error")
	default:
		row.icon.AddCSSClass("dim-label")
	}
}

// onKeyChanged reacts to every keystroke in a row's Local Key field: it clears
// any prior test result and enables the Test button only once the field holds
// exactly 16 characters (the required AES key length).
func (w *wizard) onKeyChanged(row *wizardRow) {
	row.tested = false
	row.keyEntry.RemoveCSSClass("error")
	if !row.inFlight {
		row.testBtn.SetSensitive(keyComplete(row.keyEntry.Text()))
	}
	w.refreshIcon(row)
	w.updateApplySensitivity()
}

// testRow runs runWizardTest against the device off the GTK thread: a quick
// red→blue→white flash so the user can see which physical bulb answered, after
// which the bulb is left in its pre-test state. Success marks the row tested;
// any error fails it and leaves Apply disabled. Triggered by the row's Test
// button (enabled once the key field holds 16 characters) or by pressing Enter
// in that field. Feedback is the leading icon (spinner while testing, then bulb
// state via refreshIcon) plus, on failure, an error tooltip on the key field.
func (w *wizard) testRow(row *wizardRow) {
	if row.inFlight {
		return
	}
	row.inFlight = true
	row.icon.SetVisible(false)
	row.statusSpin.SetVisible(true)
	row.statusSpin.Start()
	row.testBtn.SetSensitive(false)
	row.keyEntry.SetSensitive(false)
	row.keyEntry.SetTooltipText("Local key (16 characters)")

	d := Device{
		DeviceID:  row.dev.ID,
		IPAddress: row.dev.IP,
		LocalKey:  row.keyEntry.Text(),
		Name:      row.name,
	}

	go func() {
		err := runWizardTest(d)
		coreglib.IdleAdd(func() {
			row.inFlight = false
			row.statusSpin.Stop()
			row.statusSpin.SetVisible(false)
			row.icon.SetVisible(true)
			row.keyEntry.SetSensitive(true)
			row.testBtn.SetSensitive(keyComplete(row.keyEntry.Text()))
			row.tested = err == nil
			if err != nil {
				row.keyEntry.SetTooltipText("Connection failed: " + err.Error())
				row.keyEntry.AddCSSClass("error")
			} else {
				row.keyEntry.RemoveCSSClass("error")
			}
			w.refreshIcon(row)
			w.updateApplySensitivity()
		})
	}()
}

// wizardFlashHold is the extra pause between flash steps. Kept tiny because
// each step is already a network round trip; with the six steps below the whole
// blink lands in roughly 200ms.
const wizardFlashHold = 15 * time.Millisecond

// wizardFlashSteps is the white-brightness blink: three dark→full pulses. Each
// value is a percentage written straight to the white-mode brightness DP, so
// the bulb snaps between dark and full white with no transition. The run ends
// on 100 so the bulb is left on in bright white after the test.
var wizardFlashSteps = []float64{0, 100, 0, 100, 0, 100}

// runWizardTest proves a bulb answers on its local key by blinking it white,
// and leaves it on afterwards.
//
// It opens one command session and snaps the white-mode brightness between dark
// and full three times — each step a discrete, instantaneous write, so the bulb
// blinks white rather than fading, in about 200ms total. The first write also
// proves the key: a bad key or wrong IP fails to connect and fails the test.
// The bulb is left on at full white brightness.
func runWizardTest(d Device) error {
	ctl := newControl(d)
	defer ctl.Close()

	// Power on first so an off bulb still flashes. If this fails the key is
	// wrong or the bulb is unreachable — the test fails.
	powCtx, cancel := context.WithTimeout(context.Background(), wizardTestTimeout)
	err := ctl.SetPower(powCtx, true)
	cancel()
	if err != nil {
		return err
	}

	// Blink white brightness dark↔full. Each write is instantaneous.
	for _, pct := range wizardFlashSteps {
		stepCtx, cancel := context.WithTimeout(context.Background(), wizardTestTimeout)
		err := ctl.SetWhiteBrightness(stepCtx, pct)
		cancel()
		if err != nil {
			return err
		}
		time.Sleep(wizardFlashHold)
	}
	return nil
}

// allTested reports whether every discovered row passed its test. It gates
// Apply: at least one device, and all of them tested — no exceptions, no
// "skip".
func allTested(rows []*wizardRow) bool {
	if len(rows) == 0 {
		return false
	}
	for _, r := range rows {
		if !r.tested {
			return false
		}
	}
	return true
}

// updateApplySensitivity enables Apply only once every discovered row has
// passed its test.
func (w *wizard) updateApplySensitivity() {
	w.applyBtn.SetSensitive(allTested(w.rows))
}

// deviceFrom builds a config Device from a discovered device, a chosen name,
// and the entered local key.
func deviceFrom(d discovery.Device, name, key string) Device {
	return Device{
		DeviceID:  d.ID,
		IPAddress: d.IP,
		LocalKey:  key,
		Name:      name,
	}
}

// apply writes the tested devices to config.json, closes the wizard window,
// and launches the normal app in the same process.
func (w *wizard) apply() {
	devices := make([]Device, 0, len(w.rows))
	for _, r := range w.rows {
		devices = append(devices, deviceFrom(r.dev, r.name, r.keyEntry.Text()))
	}

	if err := saveConfig(w.configPath, devices, nil, nil); err != nil {
		dlg := adw.NewAlertDialog("Couldn't Save Configuration", err.Error())
		dlg.AddResponse("ok", "OK")
		dlg.Present(w.window)
		return
	}

	// Mark success and close the window. With NonUnique and no Hold(), the
	// GApplication drops its last reference and Run() returns on its own —
	// we must NOT call Quit() (that double-releases and trips a GLib
	// assertion, see app.go). main() sees w.applied and launches the app
	// at top level, rather than nesting a second GApplication in here.
	w.applied = true
	w.window.Close()
}
