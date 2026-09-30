# notuya-gui

> **Response style:** always answer "tl;dr" — brief, high-level responses.

A native Tuya smart-bulb controller in Go. It drives the bulbs **in-process** by
importing the `notuya-go` library directly — no daemon, no subprocess.

Two modes:

- **`notuya-gui`** (default) — a desktop window (xdg-toplevel) with per-device
  light control grouped by room. Device editing and LAN discovery live in the
  in-app **Settings** tab; a first-run wizard bootstraps the config when none
  exists.
- **`notuya-gui --picker`** — a `wlr-layer-shell` colour-wheel overlay.

## Relationship to notuya-go

Separate Go module depending on the published
`github.com/alex-bluetrain/notuya-go`. To develop against a local checkout, use
a **`go.work`** (gitignored, local-only) — never a `replace` in `go.mod`, which
breaks the build for anyone without the sibling repo:

```
go work init . ../notuya-go
```

The exported surface it consumes is public API; changing these upstream breaks
this consumer: `bulb.Bulb`, `bulb.StreamColours`, `bulb.StreamColour`,
`bulb.StreamOptions`, `bulb.Transition`, `device.RGB`, `protocol35.NewSession`,
`protocol.Session`.

Packages consumed:

```
pkg/protocol      version-agnostic Session interface
pkg/protocol35    v3.5 impl (handshake, 6699 framing, AES-GCM)
pkg/device        raw DP-centric API (device.RGB)
pkg/bulb          business layer + music streaming
pkg/discovery     LAN scan (Scan)
```

notuya-gui never touches DP numbers or wire framing — it builds a `*bulb.Bulb`
per device over a `protocol35` session.

## Building (the CGO exception)

notuya-go is `CGO_ENABLED=0` with zero deps. notuya-gui deliberately breaks
both, and only here: a real always-on-top Wayland overlay needs
`wlr-layer-shell`, which Gio can't do, so the app uses GTK4 + libadwaita via the
`gotk4`/`gotk4-adwaita` bindings plus `gtk4-layer-shell`. This is confined to
the final binary; the notuya-go packages it imports stay CGO-free.

- Builds with **`CGO_ENABLED=1`**, links via pkg-config: `gtk4`,
  `gtk4-layer-shell-0`, `libadwaita-1`.
- System packages (Arch/Omarchy): `gtk4`, `gtk4-layer-shell`, `libadwaita`, a C
  compiler, pkg-config. Verify: `pkg-config --exists gtk4 gtk4-layer-shell-0 libadwaita-1 && echo ok`.
- The layer-shell pkg-config module is `gtk4-layer-shell-0`, not
  `gtk4-layer-shell`.

## Architecture

Single binary, one module. Files under `cmd/notuya-gui/`:

```
main.go          entry: flag parsing → runApp | runPicker | runWizard
picker.go        the --picker layer-shell colour-wheel overlay
app.go           desktop window (AdwApplicationWindow + AdwViewSwitcher):
                 Scenes/Lights/Settings tabs, plus a Rooms tab hidden behind
                 roomsTabEnabled = false; room helpers live here
lights_tab.go    per-light rows (checkbox, swatch, brightness %, power switch)
                 over one shared colourControls + Instant|Smooth toggle that
                 broadcasts to every checked light; rows mirror optimistically
colour_controls.go  shared colour widget (Colour|White, wheel, temp scale,
                 brightness) used by Lights tab and scene editor
device_panel.go  headless per-device on/off cache for the Rooms summaries
control.go       per-device controller: persistent bulb session, command
                 methods + Refresh/ApplyState + async; borrows the streamer
                 for live colour drag
scenes.go        scene tiles, applyScene, deviceStatus→SceneState
scene_editor.go  Scenes editor (AdwWindow modal, live destructive preview)
stream.go        music-mode streamer (picker AND app live-drag)
wizard.go        discover-name-test-Apply flow: first-run window AND, embedded
                 and seeded with configured devices, the Settings tab
config.go        Config/Device/Room/Scene types, groupByRoom, saveConfig
color.go         HSV↔RGB helpers; wheel.go generates/caches the wheel bitmap
```

Bulb I/O model: each device gets a `control` holding a persistent, mutex-
serialized `protocol35` session (sessions are not concurrency-safe) for discrete
waited commands. A live colour drag closes that session and borrows the
`streamer` (music mode) for smoothness, then leaves music mode with a final
`SetColour` and reopens the command session lazily. The picker instead opens one
music-mode stream per device for its whole lifetime.

Streaming (same loop as the CLI `music` command and the daemon `/stream`):
build a `*bulb.Bulb` per device, run `bulb.StreamColours(ctx, ch, opts)` per
device on its own goroutine, push `bulb.StreamColour{RGB, Transition}` on drag
(library throttles ~25fps, coalesces newest-wins so a slow bulb never stalls the
UI), cancel the context on close. Brightness scales RGB before streaming;
transition is per-colour.

## Configuration

- Config: `~/.config/notuya-gui/config.json` (override with `$NOTUYA_CONFIG`).
- Last-colour cache: `last-color.txt` beside the config (revert on cancel).

Rooms are a **top-level entity**, not a per-device field, so "all lights in a
room" resolves from config without querying any bulb. `groupByRoom(cfg)` returns
ordered `[]roomGroup{Name, []*Device}`: stale ids skipped, unreferenced devices
land in a synthetic trailing **"No room"** group (computed, never written).

```json
{
  "devices": [ { "device_id": "ebfake1111111111111111", "ip_address": "192.0.2.10", "local_key": "fake-local-key16", "name": "luz 1" } ],
  "rooms": [ { "name": "Salón", "devices": ["ebfake1111111111111111"] } ]
}
```

`saveConfig` round-trips the file through `map[string]json.RawMessage`, replacing
only `devices`, `rooms`, `scenes`, so unknown keys survive; the write is atomic
(temp + rename) since the config is co-owned with the CLI/daemon.

## Settings tab

The Settings tab is the setup wizard embedded as a widget
(`buildEmbeddedWizard`): the same discover-name-test-Apply flow as first-run,
seeded with configured devices so a LAN scan merges by `device_id` instead of
replacing. A populated config shows its devices without scanning; "Scan Again"
discovers more. Devices can be added and keys re-tested, but not removed (delete
one by editing the JSON). Room management lives in the Rooms tab; on save the
wizard preserves `rooms`/`scenes` via callbacks.

Keys are entered by hand: discovery yields `device_id` + `ip` only; the
`local_key` comes from Tuya's cloud, which this tool never touches. The user
pastes each key and clicks **Test** to flash the bulb and confirm it.

## Testing the GUI (AT-SPI, not screenshots)

Verify via the **AT-SPI accessibility tree**, not pixels. GTK4/libadwaita
publishes every widget's role, name, state, and actions over `org.a11y.Bus`, so
a test can find a widget by name, invoke its own action (can't miss), and read
real state for assertions.

- **Never use the GTK Broadway backend** (`gtk4-broadwayd`,
  `GDK_BACKEND=broadway`) — no accessibility tree, verifies nothing.
- Don't use `ydotool` cursor warping or fuzzy screenshot matching — flaky.

One script owns the loop — `scripts/uitest.py`:

```bash
python3 scripts/uitest.py launch              # GTK_A11Y=atspi, waits for window, saves pid
python3 scripts/uitest.py dump                # interactive widgets + state
python3 scripts/uitest.py click White         # invoke a widget's own action by name
python3 scripts/uitest.py state Colour White  # read checked/active/selected
python3 scripts/uitest.py shot /tmp/x.png     # grim-capture the window (geometry from pid)
python3 scripts/uitest.py close               # Hyprland Lua close dispatcher, waits for reap
```

`close` reaps cleanly because the app quits on last-window-close: it holds no
`app.Hold()` and must not call `app.Quit()`/`app.Release()` itself, or GLib
trips a `g_application_release` assertion.

For full-height, deterministic, isolated capture, use the `cage` subcommand — it
renders in a nested headless `cage` at 800×1100 and clicks named widgets via
AT-SPI before grabbing:

```bash
python3 scripts/uitest.py cage /tmp/colour.png          # capture as-launched
python3 scripts/uitest.py cage /tmp/white.png White     # click White, then capture
```

Two hard requirements inside `cage` (do not simplify away):

- **`WLR_RENDERER=pixman` is mandatory.** On this box's NVIDIA proprietary
  driver the wlroots headless GPU path composites solid-black frames; the
  software renderer is the fix.
- **Set output size with `wlr-randr --output HEADLESS-1 --custom-mode`.** The
  headless output defaults to 1280×720 (clips the Lights tab) and there is no
  env var for it. `cage` and `wlr-randr` are Arch pkgs
  (`pkexec pacman -S --needed cage wlr-randr`).

Screenshots (`shot`, or `omarchy capture screenshot windows save`) are an
optional visual confirm on top of AT-SPI assertions, never the verifier.

## Hyprland

The picker is a layer surface, not a toplevel, so most `windowrule`/`o.window`
rules (float/center/size) don't apply the way they do to xdg-toplevels. Route
tweaks through Omarchy's `o.window` helper in `~/.config/hypr/`; window-rule
syntax changes between Hyprland versions, so check the current wiki
(<https://wiki.hypr.land/Configuring/Basics/Window-Rules/>). This repo ships no
Hyprland config.

## Out of scope

- The `notuyad` HTTP daemon stays in notuya-go; this module is an alternative
  front-end that skips it.
- Multi-window and remote control are not planned.

## Working rules for the agent (hard rules — do not violate)

1. **Copy the existing pattern before inventing one.** Driving a bulb goes
   through the persistent per-device `control` (`ctl.async(...)` +
   `SetColour`/`SetPower`/etc.), exactly as `lights_tab.go` and the scene editor
   do. Never invent a throwaway session, a music-mode stream, discrete
   white-brightness bursts, or rapid-fire `wait=true` writes for a one-off.
   `SetColour` works in any mode; `SetWhiteBrightness` does nothing while the
   bulb is in colour mode.

2. **Believe the user's diagnosis first.** When the machine's owner says what's
   wrong, verify that — check the actual bytes/data — before any theory of your
   own.

3. **Stop at the second friction point.** After two failed attempts at the same
   thing, halt and report facts + untried options instead of a third invention.
   This is absolute for UI/aesthetics: prefer native `Adw*` widgets
   (`AdwActionRow`, `AdwEntryRow`, `AdwPasswordEntryRow`, `AdwPreferencesGroup`)
   over hand-built `gtk.Box` + custom CSS, and ask for a reference or exact
   measurements rather than guessing at cosmetics.

4. **Never touch the user's real files without explicit approval.** The live
   config (`~/.config/notuya-gui/…`, `$NOTUYA_CONFIG`) and anything outside the
   repo are off limits unless the user says so for that change. Don't
   "normalise" a user file as a side effect.

5. **Test in `cage`, never on the live Hyprland desktop.** Use
   `scripts/uitest.py cage`.
