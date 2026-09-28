# notuya-gui

> **Response style:** always answer "tl;dr" — brief, high-level responses.

A native Tuya smart-bulb controller, written in Go. It is a port of
`~/.config/tuya/picker.py` (a GTK4 layer-shell overlay that drove the bulbs
through the `notuyad` HTTP daemon), rewritten to drive the bulbs **in-process**
by importing the `notuya-go` library directly. No daemon, no subprocess, no
Python.

The binary is **dual-mode**:

- **`notuya-gui`** (default) — a normal desktop window (an xdg-toplevel, not a
  layer-shell overlay) with per-device light control, grouped by room.
- **`notuya-gui --picker`** — the original `wlr-layer-shell` colour-wheel
  overlay, unchanged.
- **`notuya-gui -config`** — the settings window (devices, discovery, rooms).

## Why this exists

`picker.py` already works: a GTK4 layer-shell color wheel that opens one
persistent chunked `POST /stream` to `notuyad` and writes `RRGGBB [TT]` lines
on drag. `notuya-gui` collapses that two-process design (Python GUI → HTTP →
Go daemon → bulbs) into a single Go binary that speaks the Tuya protocol
itself. The GUI thread calls `bulb.StreamColours` on a background goroutine
over a channel, exactly as the CLI's `music` command and the daemon's
`/stream` handler do — the same library loop, minus the HTTP hop.

This is the reason the five library packages were promoted from `internal/`
to `pkg/` in notuya-go: so a sibling module could consume them. `notuya-gui`
is that sibling.

## Relationship to notuya-go

`notuya-gui` is a **separate Go module** that depends on
`github.com/averstraeten/notuya-go`. In development the two repos sit
side by side and are wired with a local `replace`:

```
// go.mod
require github.com/averstraeten/notuya-go v0.0.0
replace github.com/averstraeten/notuya-go => ../notuya-go
```

Nothing in notuya-go has to be published or tagged for this to work. The
trade-off (documented in notuya-go's own AGENTS.md) is that the exported
surface it consumes — `bulb.Bulb`, `bulb.StreamColours`,
`bulb.StreamColour`, `bulb.StreamOptions`, `bulb.Transition`,
`device.RGB`, `protocol35.NewSession`, `protocol.Session` — is now public
API. Changing those signatures upstream can break this consumer.

The library packages consumed here:

```
pkg/protocol      the version-agnostic Session interface
pkg/protocol35    the v3.5 implementation (handshake, 6699 framing, AES-GCM)
pkg/device        raw DP-centric API (device.RGB lives here)
pkg/bulb          business layer + music streaming (StreamColours, StreamColour, StreamOptions)
```

`notuya-gui` never touches DP numbers or wire framing — it builds a
`*bulb.Bulb` per configured device (over a `protocol35` session) and feeds
`bulb.StreamColours` a channel of `bulb.StreamColour`, the same unit the CLI
and daemon use.

## The CGO exception

**notuya-go's defining constraint is `CGO_ENABLED=0` and zero external
dependencies.** `notuya-gui` deliberately breaks both — and only here.

A true always-on-top overlay on Wayland needs the `wlr-layer-shell`
protocol. Gio (the pure-Go GUI toolkit) does not support it: it owns its
`wl_surface` internally and gives no hook to wrap it in a
`zwlr_layer_surface_v1`. The realistic path to a real layer-shell surface in
Go is GTK4 via the `gotk4` bindings plus `gotk4-layer-shell`, which wrap the
same `libgtk4-layer-shell.so` that `picker.py` preloads today. Both are
CGO-only and link the system GTK4 stack.

So:

- **`notuya-gui` builds with `CGO_ENABLED=1`** and links GTK4 +
  gtk4-layer-shell + libadwaita via pkg-config (`gtk4`,
  `gtk4-layer-shell-0`, `libadwaita-1`). The desktop app uses libadwaita
  (via the `gotk4-adwaita` bindings) for its modern GNOME widgets; the
  picker overlay stays on pure GTK4 + layer-shell.
- This is confined to this module. The notuya-go library packages it
  imports remain CGO-free and are compiled by the Go compiler as usual;
  only the final GUI binary needs a C toolchain and the GTK4 headers.
- If a pure-Go overlay ever becomes possible, this is the dependency to
  drop — nothing in the streaming path depends on GTK4.

### Build prerequisites

System packages (Arch/Omarchy names): `gtk4`, `gtk4-layer-shell`,
`libadwaita`, a C compiler, and pkg-config. Verify the toolchain resolves
the libraries:

```bash
pkg-config --exists gtk4 gtk4-layer-shell-0 libadwaita-1 && echo ok
```

Note the pkg-config module for the layer-shell library is
`gtk4-layer-shell-0` (the `.pc` is `gtk4-layer-shell-0.pc`), not
`gtk4-layer-shell`.

## Architecture

Single binary, one module. Files under `cmd/notuya-gui/`:

```
main.go          entry point: flag parsing → runApp | runPicker | runSettings
picker.go        the --picker layer-shell colour-wheel overlay
app.go           the default desktop window (libadwaita: AdwApplicationWindow +
                 AdwHeaderBar + AdwViewSwitcher/ViewStack): Lights/Rooms/Scenes/
                 Settings tabs. Rooms tab = mobile-style summary rows (room name,
                 "N of M lights on", master switch) reusing the Lights panels,
                 plus room CRUD (create/rename/delete + membership).
device_panel.go  per-device control card (AdwActionRow header + power/bright/
                 colour/temp/status; colour wheel is a plain DrawingArea)
control.go       per-device controller: owns a bulb session, command methods +
                 Refresh + ApplyState; borrows the streamer for live colour drag
scenes.go        scene apply (applyScene) + deviceStatus→SceneState mapping
scene_editor.go  the Scenes editor: sceneEditor + per-device sceneDeviceRow,
                 AdwWindow modal with live (destructive) preview
stream.go        music-mode streamer (used by picker AND app live-drag)
settings.go      settings window/tab: devices + LAN discovery (rooms live in app.go)
config.go        Config/Device/Room/Scene types, groupByRoom, saveConfig
color.go         HSV↔RGB helpers; wheel.go generates/caches the wheel bitmap
```

The picker opens one music-mode stream per device for its whole lifetime. The
desktop app is longer-lived: each device gets a `control` holding a persistent
`protocol35` session (mutex-serialized, since a session is not concurrency-safe)
over which it issues discrete waited commands. A live colour drag temporarily
closes that command session and borrows the `streamer` (music mode) so the drag
is smooth, then leaves music mode with a final `SetColour` and lets the command
session re-open lazily.

Colour-wheel drawing and the coordinate→(hue,sat) mapping are shared between the
picker and the panel (`coordsToHSSized`, the cached wheel surface).

The UI is in English. Scenes are editable, not capture-only. **New scene** opens
`sceneEditor` (`scene_editor.go`) for a new scene; each row's **Edit** button
reopens it for an existing one. The editor is an `AdwWindow` modal with one
`sceneDeviceRow` per configured device (include checkbox, power switch,
Color/White selector, colour wheel + swatch, brightness/temp sliders) that edits
an in-memory `SceneState`. Preview is **live and destructive**: the row reuses
the same `control` instances as the Lights tab (via `a.byID`, mutex-serialized —
no second session per bulb), driving the wheel through `BeginLiveDrag`/
`UpdateLiveDrag`/`EndLiveDrag` and the sliders/power/mode through the discrete
setters, all off-thread. Nothing is restored on close. **Save** writes the built
`[]SceneState` over `a.cfg.Scenes[index]` (or appends when index == -1) and
persists via `saveCfg`; **Cancel** keeps the bulbs at their last preview.

The **Rooms** tab (`buildRoomsTab`) is a read/control overview built from
`groupByRoom`: one `roomRow` per room (an `AdwActionRow` with a `user-home`
icon, the room name, a live "N of M lights on" subtitle, and a master `Switch`).
Each `roomRow` holds the room's `*devicePanel`s and recomputes its summary from
their cached `lastOn`/`hasState` whenever a member panel refreshes (`onRefresh`)
or is toggled (`onToggle`) — no extra device queries. Flipping the master switch
calls `groupPower` and optimistically syncs each member panel's switch via
`setPowerOptimistic`. Below the control rows the same tab hosts **room
management**: a room list, a name entry with add/rename/remove buttons, and
membership checkboxes for the selected room (`refreshRoomList`, `loadRoomForm`,
`refreshMembers`, `upsertRoom`, `removeRoom`, all on `desktopApp`). Every edit
mutates `a.cfg.Rooms` and persists via `a.saveCfg`. Membership changes are
reflected in the control views on the next launch. The Settings tab no longer
owns any room UI.

Planned internal split (subject to change as the port lands):

```
color wheel      HSV polar bitmap, generated once and cached to disk as raw
                 BGRA bytes (port of picker.py's _generate_wheel_bytes /
                 _load_wheel), drawn with Cairo via gotk4.
sliders          brightness (0-100%) and transition (0-10) vertical scales.
streaming        one *bulb.Bulb per device; a background goroutine runs
                 bulb.StreamColours over a channel; drag events push
                 bulb.StreamColour (RGB + optional per-line Transition),
                 coalesced newest-wins by the library loop.
lifecycle        Escape / Cancel reverts to the pre-open colour; Enter /
                 Aceptar persists; on exit the final colour is written with
                 SetColour (leaves music mode) and the last-colour cache is
                 updated.
```

### Streaming model (mirrors the CLI and daemon)

1. On open, build a `*bulb.Bulb` per configured device.
2. Start `bulb.StreamColours(ctx, ch, opts)` per device on its own goroutine.
3. On drag, send a `bulb.StreamColour{RGB: ..., Transition: ...}` to each
   channel; the library throttles to `DefaultStreamInterval` (~25fps) and
   coalesces newest-wins, so a slow bulb never stalls the UI thread.
4. On close/cancel, cancel the context; each `StreamColours` flushes the
   pending colour, leaves music mode with a normal `SetColour`, and returns.

Brightness and transition semantics match `picker.py` and the CLI's stdin
protocol: transition is per-colour (slider takes effect live), brightness is
applied by scaling RGB before streaming (the stream unit carries colour, not
a separate brightness channel).

## Configuration

- Config: `~/.config/notuya-gui/config.json` (override with `$NOTUYA_CONFIG`).
  The `devices` array carries `device_id`, `ip_address`, `local_key`, `name`.
- Last colour cache: read on open (to revert on cancel), written on a
  committed exit — a `last-color.txt` beside the config file.

### Rooms (top-level entity)

Rooms are a **first-class, top-level entity** in `config.json`, not a
per-device field — so the desktop app can resolve "all lights in a room" from
config alone without querying any lamp. `Device` is unchanged; a new
top-level `rooms` array lists each room's `name` and the `device_id`s it
contains (a device may belong to several rooms):

```json
{
  "devices": [ { "device_id": "eb…14", "ip_address": "192.168.1.4", "local_key": "…", "name": "luz 1" } ],
  "rooms": [ { "name": "Salón", "devices": ["eb…14"] } ],
  "follow_mode": "wallpaper"
}
```

`groupByRoom(cfg)` resolves the `rooms` array into ordered
`[]roomGroup{Name, []*Device}`: stale `device_id`s (no matching device) are
skipped, and every device not referenced by any room is appended in a
trailing synthetic **"No room"** group (computed at load, never written).
`saveConfig` serialises `rooms` alongside `devices` via the same
unknown-key-preserving, atomic round-trip.

## Settings window

`notuya-gui -config` opens a settings window (an ordinary GTK4 toplevel,
**not** a layer-shell overlay) instead of the app or picker. It edits the
shared `config.json`'s `devices` array — add, edit, and remove bulbs — and can
**discover** bulbs on the LAN via notuya-go's `pkg/discovery.Scan` (reused; no
UDP code lives here). Room management (create/rename/remove rooms and assign
devices) lives in the desktop app's **Rooms** tab, not here; the standalone
window edits devices only and preserves the `rooms`/`scenes` arrays on save.
Both the standalone window and the in-app **Settings** tab share
`settings.buildContent()`.

- **Keys are entered by hand.** LAN discovery yields `device_id` + `ip`
  only; a bulb's `local_key` comes from Tuya's cloud, which this tool
  deliberately does not touch. Clicking a discovered device pre-fills IP +
  Device ID; the user pastes the Local Key.
- **Writes preserve unknown keys.** `saveConfig` round-trips the file
  through `map[string]json.RawMessage`, replacing only `devices`, `rooms`, and
  `scenes`, so keys this tool doesn't model (`wallpaper_sync`, theme keys,
  anything the CLI/daemon own) survive. The write is atomic (temp file +
  rename) since the config is co-owned.
- If the app or picker is launched with **no** config file, it exits with a
  hint pointing at `notuya-gui -config` so a first-run user can create one.

## Testing the GUI (AT-SPI, not screenshots)

**The correct, non-flaky way to drive and verify this app is the AT-SPI
accessibility tree — not screenshots, not synthetic mouse/keyboard input.**
GTK4/libadwaita publishes every widget's role, name, state, and actions over
the a11y D-Bus (`org.a11y.Bus`), so a test can find a widget by name, invoke
its *own* action (clicks can't miss), and read real state
(`checked`/`active`/slider value) for assertions. No app changes are needed —
the widgets already expose this.

**NEVER use the GTK Broadway backend (`gtk4-broadwayd`, `GDK_BACKEND=broadway`).
Banned outright — do not launch it, do not screenshot it, do not "fall back" to
it for any reason.** It renders to an HTML canvas with no accessibility tree, so
it verifies nothing and only tempts pixel-guessing.

Do **not** rely on the other approaches tried earlier and found flaky: `ydotool`
cursor warping or `grim` pixel captures. Those fight Wayland, leave stuck
`poll_schedule_timeout` zombies, and verify pixels instead of state. Fuzzy
screenshot matching (openQA "needles") is likewise fragile and out of scope here.

When a **visual** check is genuinely needed (e.g. "does this look right"), use
Omarchy's sanctioned capture of the real window and nothing else:
`omarchy capture screenshot windows save` (prints the saved PNG path). That is a
confirm on top of AT-SPI assertions, never the verification mechanism itself.

Prerequisites (already present on the dev box, Arch/Omarchy):
`at-spi2-core` (the a11y bus + GTK atk-bridge), the `Atspi` GObject-introspection
bindings (`python3 -c "import gi; gi.require_version('Atspi','2.0')"`), and a
running `org.a11y.Bus` (started automatically in a desktop session). `dogtail`
is optional sugar — raw `Atspi` GI is enough.

Workflow — one script, `scripts/uitest.py`, owns the whole loop
(launch → drive/assert → capture → close). It handles both halves: the
process/compositor side (`launch`/`shot`/`close`, via `hyprctl clients -j`
+ `grim` + Hyprland's Lua close dispatcher) and the a11y side
(`dump`/`click`/`state`, via `Atspi`):

```bash
python3 scripts/uitest.py launch              # start with GTK_A11Y=atspi, wait for the window, save pid
python3 scripts/uitest.py dump                # print interactive widgets + state
python3 scripts/uitest.py click White         # invoke a widget's own action by name
python3 scripts/uitest.py state Colour White  # read checked/active/selected
python3 scripts/uitest.py shot /tmp/x.png     # grim-capture the window (geometry resolved from pid, no slurp)
python3 scripts/uitest.py close               # Hyprland Lua close dispatcher, waits for the pid to reap
```

`launch` sets `GTK_A11Y=atspi` itself and records the pid in
`/tmp/notuya-gui.uitest.pid`; `shot`/`close` resolve the window from that pid
(falling back to the app class `ar.averstraeten.tuyawheel.app`), so no
coordinates are ever guessed. `close` reaps cleanly because the app quits when
its last window closes — it holds no artificial reference (`app.Hold()`) and
must not call `app.Quit()`/`app.Release()` itself, or GLib trips a
`g_application_release` use-count assertion. This end-to-end loop
(launch → click+assert → shot → close) has been verified with zero leftover
processes and zero GLib assertions.

For the a11y commands, `scripts/uitest.py` walks the desktop for the
`notuya-gui` application, finds widgets by role+name, invokes their action with
`Atspi.Action.do_action(n, 0)`, and reports `checked`/`active`/`selected`. The
Lights tab exposes: page tabs (`Scenes`/`Lights`/`Settings`), per-light
`check box` + `switch` rows, the `Colour`/`White` mode radios, the
`Instant`/`Smooth` transition radios, and the brightness/temperature sliders —
all addressable by name.

`python3 scripts/uitest.py shot <path>` (or, interactively,
`omarchy capture screenshot windows save`) is an *optional* visual confirm on
top of AT-SPI assertions, never the verification mechanism itself.

One caveat for `shot`: it captures the window at whatever size Hyprland has
tiled it to, so on a busy workspace the Lights tab (tall: wheel + sliders) can
be clipped at the bottom. When a *full-height* capture matters, use the `cage`
subcommand — it renders the app in a nested headless `cage` compositor at a
fixed 800×1100 output, clicks any named widgets via AT-SPI, and `grim`s the
frame. Deterministic geometry, nothing clipped, fully isolated from the live
Hyprland session:

```bash
python3 scripts/uitest.py cage /tmp/colour.png          # capture as-launched
python3 scripts/uitest.py cage /tmp/white.png White     # click White, then capture
```

Two hard-won requirements baked into the subcommand (do not "simplify" them
away):

- **`WLR_RENDERER=pixman` is mandatory.** On this box's NVIDIA proprietary
  driver, the wlroots headless backend's GPU path composites black frames —
  the app runs, grim succeeds, and the PNG is solid black. The pixman
  (software) renderer is the fix.
- **Output size is set with `wlr-randr --output HEADLESS-1 --custom-mode`**
  inside cage. The headless output defaults to 1280×720, which clips the
  Lights tab; there is no env var for the size (`WLR_HEADLESS_OUTPUT_WIDTH`
  does not exist). `wlr-randr` is an Arch pkg, installed alongside `cage`
  (`pkexec pacman -S --needed cage wlr-randr`).

The app inside cage joins the *session* a11y bus, so the same AT-SPI helpers
drive it — that's how the subcommand clicks widgets before grabbing. This is
still a visual-only aid: AT-SPI assertions remain the state/behaviour
verifier.

## Hyprland window rules

The picker is a layer-shell overlay, so under Hyprland it is a layer
surface, not a tiled/floating toplevel — most `windowrule`/`o.window` rules
(float, center, size) do not apply to layer surfaces the way they do to
xdg-toplevels. Any needed tweaks go through Omarchy's `o.window` helper in
`~/.config/hypr/`; window-rule syntax changes between Hyprland versions, so
verify against the current wiki before writing rules
(<https://wiki.hypr.land/Configuring/Basics/Window-Rules/>). This repo does
not ship Hyprland config; it documents that the surface is a layer-shell
overlay.

## Out of scope

- The `notuyad` HTTP daemon and `picker.py` stay in notuya-go /
  `~/.config/tuya`; this module does not replace or modify them. It is an
  alternative front-end that skips the daemon.
- Multi-window, remote control, and a scene picker are not planned.
  (Config editing and bulb discovery, previously out of scope, now ship in
  the settings window — see above.)
- Publishing/tagging notuya-go: dev uses the local `replace`.
