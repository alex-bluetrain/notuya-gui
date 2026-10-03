package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/alex-bluetrain/notuya-go/pkg/device"
	"github.com/alex-bluetrain/notuya-go/pkg/discovery"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	coreglib "github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// scanTimeout bounds a discovery.Scan.
const scanTimeout = 4 * time.Second

// wizardTestTimeout bounds one wizard test step.
const wizardTestTimeout = 5 * time.Second

// wizardDeviceName is the default name for the i-th discovered device (0-based).
func wizardDeviceName(i int) string {
	return fmt.Sprintf("Light %d", i+1)
}

// keyComplete reports whether a Local Key field holds enough to test. The real
// validation is the live connection in runWizardTest.
func keyComplete(key string) bool {
	return len(strings.TrimSpace(key)) > 0
}

// foundLightsTitle is the boxed-list title for n discovered devices.
func foundLightsTitle(n int) string {
	if n == 1 {
		return "Found 1 light"
	}
	return fmt.Sprintf("Found %d lights", n)
}

// runWizard runs the first-run setup flow in its own window: scan the LAN, let
// the user paste each bulb's Local Key and test it live, then write config.json
// on Apply. It returns the GApplication exit code and whether the user
// completed setup; on applied, main() loads the fresh config and starts the app.
func runWizard(configPath string) (code int, applied bool) {
	w := &wizard{configPath: configPath}
	w.app = adw.NewApplication("ar.averstraeten.tuyawheel.wizard", gio.ApplicationNonUnique)
	w.app.ConnectActivate(func() { w.activate() })
	code = w.app.Run(os.Args[:1])
	return code, w.applied
}

// buildEmbeddedWizard builds the wizard as a widget for the Settings tab. It
// seeds the list with the already-configured devices (existing) so a scan
// merges rather than replaces; scenesFn/roomsFn preserve the owner's
// scenes/rooms on Apply, and onSaved mirrors the written devices back. shared
// resolves a device_id to the control the app already drives that bulb with,
// so Test never opens a session beside it (nil for unknown devices).
func buildEmbeddedWizard(configPath string, existing []Device, shared func(deviceID string) *control, scenesFn func() []Scene, roomsFn func() []Room, onSaved func([]Device)) gtk.Widgetter {
	w := &wizard{
		configPath: configPath,
		existing:   append([]Device(nil), existing...),
		shared:     shared,
		scenesFn:   scenesFn,
		roomsFn:    roomsFn,
		onSaved:    onSaved,
	}
	return w.buildEmbedded()
}

type wizard struct {
	app        *adw.Application       // nil in embedded (Settings tab) mode
	window     *adw.ApplicationWindow // nil in embedded mode
	configPath string

	// existing seeds the list with the already-configured devices so a scan
	// merges instead of replacing. First-run leaves it nil.
	existing []Device
	// scenesFn/roomsFn/onSaved are set only in embedded mode: they preserve the
	// owner's scenes/rooms on Apply and mirror device edits back. First-run
	// leaves them nil and saves with no rooms/scenes.
	scenesFn func() []Scene
	roomsFn  func() []Room
	onSaved  func([]Device)
	// shared (embedded mode) looks up the app's own control for a device_id;
	// Test goes through it so the bulb keeps a single session (see controlFor).
	shared func(deviceID string) *control

	// embeddedRoot parents the error dialog in embedded mode (no window).
	embeddedRoot gtk.Widgetter

	toolbar     *adw.ToolbarView
	stack       *gtk.Stack
	scanSpinner *adw.Spinner
	deviceList  *adw.PreferencesGroup
	applyBar    *gtk.Box
	applyBtn    *gtk.Button

	rows    []*wizardRow
	applied bool // set once the user completes setup (Apply)
}

// wizardRow is one device shown as an AdwActionRow: a bulb icon, the name and
// IP · device-id, a masked Local Key entry, a Test button, and a spinner.
type wizardRow struct {
	dev  discovery.Device
	name string

	row        *adw.ActionRow
	icon       *gtk.Image
	keyEntry   *gtk.PasswordEntry
	testBtn    *gtk.Button
	statusSpin *gtk.Spinner

	ctl *control // session used by Test; see controlFor
	// shared marks ctl as borrowed from the app (never closed or recreated by
	// the wizard) rather than owned by this row.
	shared bool

	tested   bool // answered a full test sequence successfully
	inFlight bool // a test is currently running

	// preexisting marks a device already in config (Settings tab). While the
	// key field still holds savedKey the row is ready without a re-test; editing
	// the key clears that and forces a Test (see rowReady).
	preexisting bool
	savedKey    string
}

// buildStack assembles the scanning/results/empty pages and the pinned Apply
// bar, shared by the standalone window and the embedded tab. It leaves the
// stack on the scanning page; the caller starts the scan.
func (w *wizard) buildStack() {
	w.stack = gtk.NewStack()
	w.stack.SetVExpand(true)
	w.stack.SetTransitionType(gtk.StackTransitionTypeCrossfade)
	w.stack.AddNamed(w.buildScanningPage(), "scanning")
	w.stack.AddNamed(w.buildResultsPage(), "results")
	w.stack.AddNamed(w.buildEmptyPage(), "empty")
	w.stack.SetVisibleChildName("scanning")

	w.toolbar = adw.NewToolbarView()
	w.toolbar.SetContent(w.stack)
	w.toolbar.AddBottomBar(w.applyBar)
	w.toolbar.SetRevealBottomBars(false)
}

func (w *wizard) activate() {
	window := adw.NewApplicationWindow(&w.app.Application)
	window.SetTitle("Set Up Lights")
	window.SetDefaultSize(720, 640)
	w.window = window

	w.buildStack()
	w.toolbar.AddTopBar(adw.NewHeaderBar())

	window.SetContent(w.toolbar)
	window.ConnectCloseRequest(func() bool {
		w.closeControls()
		return false
	})
	window.Present()

	w.scan()
}

// buildEmbedded builds the wizard for the Settings tab: the same stack and
// Apply bar, minus the window and header bar. With configured devices it shows
// them without scanning (the user hits "Scan Again" to discover more, merging
// by device_id); with none it scans on open, like first-run.
func (w *wizard) buildEmbedded() gtk.Widgetter {
	w.buildStack()
	w.embeddedRoot = w.toolbar
	if len(w.existing) > 0 {
		w.showExisting()
	} else {
		w.scan()
	}
	return w.toolbar
}

// showExisting lists the configured devices without a LAN scan, so opening
// Settings with a populated config doesn't probe the network. A later "Scan
// Again" merges these with fresh discoveries.
func (w *wizard) showExisting() {
	devs := make([]discovery.Device, len(w.existing))
	for i, d := range w.existing {
		devs[i] = discovery.Device{ID: d.DeviceID, IP: d.IPAddress}
	}
	w.showResults(devs)
	w.stack.SetVisibleChildName("results")
	w.toolbar.SetRevealBottomBars(true)
}

// closeControls shuts every per-row session down when the wizard exits.
func (w *wizard) closeControls() {
	for _, r := range w.rows {
		releaseControl(r)
	}
}

// releaseControl drops the row's control, closing it only if the row owns it.
// A control borrowed from the app stays open: other tabs are still using it.
func releaseControl(r *wizardRow) {
	if r.ctl != nil && !r.shared {
		r.ctl.Close()
	}
	r.ctl = nil
	r.shared = false
}

// controlFor returns the control a test of d must use. In the Settings tab a
// device the app already drives is tested through the app's own control, so
// the bulb only ever sees one session (AGENTS.md boundary #1); the control is
// re-pointed at the key/IP under test. A device the app doesn't know yet gets
// a private control owned by the row, recreated whenever the key or IP changes.
func (w *wizard) controlFor(row *wizardRow, d Device) *control {
	if w.shared != nil {
		if ctl := w.shared(d.DeviceID); ctl != nil {
			if row.ctl != ctl {
				releaseControl(row)
			}
			ctl.reconfigure(d)
			row.ctl, row.shared = ctl, true
			return ctl
		}
	}
	if row.ctl == nil || row.shared || row.ctl.device() != d {
		releaseControl(row)
		row.ctl = newControl(d)
	}
	return row.ctl
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

// buildResultsPage lists the discovered devices for key entry and testing. The
// Apply action lives in a bottom bar pinned by the ToolbarView.
func (w *wizard) buildResultsPage() *gtk.ScrolledWindow {
	rescan := gtk.NewButtonWithLabel("Scan Again")
	rescan.SetVAlign(gtk.AlignCenter)
	rescan.ConnectClicked(func() { w.rescan() })

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

// showResults rebuilds the results list, discarding any cards from a previous
// scan. Each device is matched against w.existing by device_id; a match seeds
// the row with its saved name and key so a Settings-tab scan merges with the
// config rather than replacing it.
func (w *wizard) showResults(devs []discovery.Device) {
	for _, r := range w.rows {
		releaseControl(r)
		w.deviceList.Remove(r.row)
	}
	w.rows = w.rows[:0]

	w.deviceList.SetTitle(foundLightsTitle(len(devs)))

	for i, d := range devs {
		w.deviceList.Add(w.buildDeviceCard(i, d))
	}
	w.updateApplySensitivity()
}

// existingFor returns the configured device with this device_id, or nil if the
// discovered device isn't in config yet.
func (w *wizard) existingFor(id string) *Device {
	for i := range w.existing {
		if w.existing[i].DeviceID == id {
			return &w.existing[i]
		}
	}
	return nil
}

// buildDeviceCard builds one AdwActionRow for a device: a bulb icon, the name
// and IP · device-id, a masked Local Key entry, a Test button, and a spinner.
// The user enters the key, then clicks Test to flash the bulb and confirm it.
func (w *wizard) buildDeviceCard(i int, d discovery.Device) *adw.ActionRow {
	row := &wizardRow{dev: d, name: wizardDeviceName(i)}

	// A device already in config (Settings tab) is seeded with its saved name
	// and key and marked preexisting, so it's ready without a re-test.
	if ex := w.existingFor(d.ID); ex != nil {
		if ex.Name != "" {
			row.name = ex.Name
		}
		row.preexisting = true
		row.savedKey = ex.LocalKey
	}

	card := adw.NewActionRow()
	card.SetTitle(row.name)
	card.SetSubtitle(d.IP + " · " + d.ID)
	row.row = card

	// emoji-objects-symbolic is a light bulb, the closest stock icon. It dims
	// until the device is ready (see refreshIcon).
	row.icon = gtk.NewImageFromIconName("emoji-objects-symbolic")
	card.AddPrefix(row.icon)

	row.keyEntry = gtk.NewPasswordEntry()
	row.keyEntry.SetShowPeekIcon(true)
	row.keyEntry.SetVAlign(gtk.AlignCenter)
	row.keyEntry.SetWidthChars(24)
	row.keyEntry.SetMaxWidthChars(24)
	row.keyEntry.SetObjectProperty("placeholder-text", "Local key")
	row.keyEntry.SetTooltipText("Local key (16 characters)")
	row.keyEntry.ConnectChanged(func() { w.onKeyChanged(row) })
	// Enter in the key field runs the test when the key is complete.
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

	// Preload the saved key for a configured device.
	if row.preexisting && row.savedKey != "" {
		row.keyEntry.SetText(row.savedKey)
	}

	w.refreshIcon(row)
	w.rows = append(w.rows, row)
	return card
}

// rowReady reports whether a row can enable Apply: it tested live this session,
// or it's a configured device whose key field still holds the saved key.
func rowReady(row *wizardRow) bool {
	if row.tested {
		return true
	}
	return row.preexisting && row.keyEntry.Text() == row.savedKey && row.savedKey != ""
}

// refreshIcon colours the bulb icon by state: accent when ready, error red on a
// failed test, dim otherwise.
func (w *wizard) refreshIcon(row *wizardRow) {
	for _, c := range []string{"dim-label", "accent", "error"} {
		row.icon.RemoveCSSClass(c)
	}
	switch {
	case rowReady(row):
		row.icon.AddCSSClass("accent")
	case row.keyEntry.HasCSSClass("error"):
		row.icon.AddCSSClass("error")
	default:
		row.icon.AddCSSClass("dim-label")
	}
}

// onKeyChanged clears any prior test result on each keystroke and enables Test
// once the field holds a key. Editing a preexisting device's key forces a Test.
func (w *wizard) onKeyChanged(row *wizardRow) {
	row.tested = false
	row.keyEntry.RemoveCSSClass("error")
	if !row.inFlight {
		row.testBtn.SetSensitive(keyComplete(row.keyEntry.Text()))
	}
	w.refreshIcon(row)
	w.updateApplySensitivity()
}

// testRow runs runWizardTest off the GTK thread: it flashes the bulb so the
// user can see which one answered. Success marks the row tested; an error fails
// it and shows an error tooltip on the key field.
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

	// Test through the app's control for a known device (one session per
	// bulb), else this row's own. A shared control that was re-pointed at a
	// key that then fails is restored, so a typo in Settings never breaks the
	// Lights/Scenes tabs underneath.
	d := deviceFrom(row.dev, row.name, row.keyEntry.Text())
	ctl := w.controlFor(row, d)
	prev := ctl.device()
	restore := row.shared && prev != d

	go func() {
		err := runWizardTest(ctl)
		if err != nil {
			fmt.Fprintf(os.Stderr, "notuya-gui: wizard test %s (%s) failed: %v\n", row.name, d.IPAddress, err)
			if restore {
				ctl.reconfigure(prev)
			}
		}
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

// wizardFlashHold is the pause between blink colours.
const wizardFlashHold = 166 * time.Millisecond

// wizardFlashColours is the blink sequence: red and blue alternating.
var wizardFlashColours = []device.RGB{
	{R: 255, G: 0, B: 0}, // red
	{R: 0, G: 0, B: 255}, // blue
	{R: 255, G: 0, B: 0}, // red
	{R: 0, G: 0, B: 255}, // blue
	{R: 255, G: 0, B: 0}, // red
	{R: 0, G: 0, B: 255}, // blue
}

// runWizardTest proves a bulb answers on its local key by powering it on and
// blinking it red↔blue through the live-drag streamer, then leaves it on. A bad
// key or wrong IP fails to connect here.
func runWizardTest(ctl *control) error {
	powCtx, cancel := context.WithTimeout(context.Background(), wizardTestTimeout)
	err := ctl.SetPower(powCtx, true)
	cancel()
	if err != nil {
		return err
	}

	ctl.BeginLiveDrag(wizardFlashColours[0], 0)
	for _, rgb := range wizardFlashColours {
		ctl.UpdateLiveDrag(rgb)
		time.Sleep(wizardFlashHold)
	}
	ctl.EndLiveDrag()
	return nil
}

// allReady reports whether there is at least one device and every row is ready
// (see rowReady). It gates Apply.
func allReady(rows []*wizardRow) bool {
	if len(rows) == 0 {
		return false
	}
	for _, r := range rows {
		if !rowReady(r) {
			return false
		}
	}
	return true
}

// updateApplySensitivity enables Apply once every row is ready.
func (w *wizard) updateApplySensitivity() {
	w.applyBtn.SetSensitive(allReady(w.rows))
}

// deviceFrom builds a config Device from a discovered device, name, and key.
func deviceFrom(d discovery.Device, name, key string) Device {
	return Device{
		DeviceID:  d.ID,
		IPAddress: d.IP,
		LocalKey:  normalizeKey(key),
		Name:      name,
	}
}

// normalizeKey turns a pasted local key into the raw bytes the cipher needs. A
// key copied out of the JSON config carries literal escape sequences (e.g.
// `\u003e` for `>`); unquote them back to the real character.
func normalizeKey(key string) string {
	k := strings.TrimSpace(key)
	if !strings.Contains(k, `\`) {
		return k
	}
	if unq, err := strconv.Unquote(`"` + k + `"`); err == nil {
		return unq
	}
	return k
}

// apply writes the ready devices to config.json. First-run saves no
// rooms/scenes and closes the window; embedded (Settings tab) mode preserves
// the owner's rooms/scenes via the callbacks, mirrors devices back through
// onSaved, and leaves the tab in place.
func (w *wizard) apply() {
	devices := make([]Device, 0, len(w.rows))
	for _, r := range w.rows {
		devices = append(devices, deviceFrom(r.dev, r.name, r.keyEntry.Text()))
	}

	var rooms []Room
	var scenes []Scene
	if w.roomsFn != nil {
		rooms = w.roomsFn()
	}
	if w.scenesFn != nil {
		scenes = w.scenesFn()
	}

	if err := saveConfig(w.configPath, devices, rooms, scenes); err != nil {
		dlg := adw.NewAlertDialog("Couldn't Save Configuration", err.Error())
		dlg.AddResponse("ok", "OK")
		if w.window != nil {
			dlg.Present(w.window)
		} else if w.embeddedRoot != nil {
			dlg.Present(w.embeddedRoot)
		}
		return
	}

	if w.onSaved != nil {
		w.onSaved(devices)
	}

	if w.window == nil {
		// Embedded mode: nothing to close.
		return
	}

	// First-run mode: mark success and close the window. Do not call Quit() —
	// with NonUnique and no Hold(), Run() returns once the window closes, and
	// Quit() would double-release (see app.go). main() sees w.applied.
	w.applied = true
	w.window.Close()
}
