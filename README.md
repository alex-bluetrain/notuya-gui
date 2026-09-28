# notuya-gui

A native Tuya smart-bulb controller, written in Go. It is a port of
`picker.py` (a GTK4 layer-shell overlay that drove the bulbs through the
`notuyad` HTTP daemon), rewritten to drive the bulbs **in-process** by
importing the [`notuya-go`](https://github.com/alex-bluetrain/notuya-go) library
directly. No daemon, no subprocess, no Python.

The binary has three modes:

- **`notuya-gui`** (default) — a normal desktop window organised into tabs:
  **Lights** (per-device control — status, power, colour wheel, brightness,
  colour temperature), **Rooms** (a mobile-style overview: one row per room
  with a live "N of M lights on" summary and a master on/off switch),
  **Scenes** (save and apply global, cross-room snapshots of the lights you
  select), and **Settings** (add/edit/remove bulbs, discover bulbs on the LAN,
  and manage rooms).
- **`notuya-gui --picker`** — a full-screen `wlr-layer-shell` overlay with an
  HSV colour wheel; drag across the wheel and each configured bulb is streamed
  the colour live via `bulb.StreamColours` (the same loop the CLI's `music`
  command uses), throttled and coalesced newest-wins so a slow bulb never
  stalls the UI.
- **`notuya-gui -config`** — *deprecated.* Opens the standalone settings
  window; the same UI is now the **Settings** tab of the default app. Kept only
  to bootstrap a first-run config when no `config.json` exists yet.

## Requirements

- Go 1.27+
- A C toolchain and `pkg-config`
- GTK4, gtk4-layer-shell, and libadwaita system libraries

On Arch/Omarchy:

```bash
sudo pacman -S gtk4 gtk4-layer-shell libadwaita base-devel
```

Verify the toolchain resolves the libraries:

```bash
make deps        # or: pkg-config --exists gtk4 gtk4-layer-shell-0 libadwaita-1 && echo ok
```

Note the pkg-config module for the layer-shell library is
`gtk4-layer-shell-0`, not `gtk4-layer-shell`.

## Build & run

```bash
make build       # builds ./notuya-gui (CGO_ENABLED=1)
make run         # build + launch
make check       # go vet + go test
```

The first GTK4-linked build is slow (the C link step dominates); subsequent
builds are fast.

## Configuration

- **Config:** `~/.config/notuya-gui/config.json` (override with
  `$NOTUYA_CONFIG`) — the `devices` array (`device_id`, `ip_address`,
  `local_key`, `name`) and an optional top-level `rooms` array (see below).
- **Last-colour cache:** `last-color.txt` beside the config — read on open to
  revert on cancel, written on a committed exit.

### Rooms

Rooms are a **first-class, top-level entity** in `config.json` — not a
per-device field — so the app can resolve "all lights in a room" from the
config alone without querying any lamp. Each room lists the `device_id`s it
contains, and a device may belong to several rooms:

```json
{
  "devices": [
    { "device_id": "eb…14", "ip_address": "192.168.1.4", "local_key": "…", "name": "luz 1" }
  ],
  "rooms": [
    { "name": "Salón", "devices": ["eb…14"] }
  ],
  "follow_mode": "wallpaper"
}
```

Devices not referenced by any room are shown under a synthetic **"Sin sala"**
group; stale `device_id`s in a room (no matching device) are ignored.

## Usage — desktop app (default)

```bash
notuya-gui
```

A normal window built with **libadwaita** — an `AdwHeaderBar` with an
`AdwViewSwitcher` selecting four views, laid out with Adwaita cards, boxed
lists, and preference groups for a native GNOME look.

The **Lights** view lists each device as a `card`: an `AdwActionRow` header with
the name, live status, and a power switch, plus a colour wheel + swatch,
brightness and colour-temperature sliders, and a **Refresh** button that
re-reads live status. Dragging a device's wheel streams the colour
live (music mode); on release the final colour is committed so it sticks.

The **Rooms** view is a mobile-style overview plus room management. The top
section (**My rooms**) has one `AdwActionRow` per room with a home icon, the
room name, a live subtitle (**All lights on** / **N of M lights on** / **All
lights off**), and a master **switch** that powers the whole room on or off. The
summary and switch track the Lights tab's per-device state without issuing extra
queries — a room's row updates as its member panels refresh or are toggled.
Below it, **Manage rooms** lets you create/rename/delete rooms and assign
devices to the selected room (membership checkboxes); changes persist to
`config.json` immediately. Structural changes (a room's membership) show up in
the control views on the next launch.

The **Scenes** view manages **software-only scenes** — named, global snapshots
saved in `config.json`, shown as a boxed list of rows. **New scene** opens the
scene editor for a new scene; each existing row has an **Edit** button (pencil)
that reopens the editor to change it, an **Apply** button that fans out discrete
commands to its lights, and a trash button that removes it. Scenes are pure
software (no firmware, cloud, or protocol dependency) and reuse the same
per-device setters as the Lights tab.

The **scene editor** (an `AdwWindow` modal) shows a name field and one card per
device with an *include* checkbox and per-light controls: power switch, a
Color/White mode selector, colour wheel + swatch, and brightness/temperature
sliders. Editing is **live and destructive** — dragging the wheel or moving a
slider drives the real bulb immediately (via the same music-mode live-drag and
setters the Lights tab uses), and the lights are **not** restored when the
editor closes, exactly like the picker and the Lights tab. **Save** writes the
edited snapshot back into the scene (or appends a new one); **Cancel** discards
the edits but leaves the bulbs at their last previewed value.

The **Settings** tab embeds the settings UI (see below). Device edits made there
take effect on the next launch (open sessions aren't rebuilt live).

## Usage — picker overlay

```bash
notuya-gui --picker
```

- **Drag / click** the wheel to pick a hue and saturation; the swatch and the
  bulbs update live.
- **Brightness** (1–100%) scales the streamed RGB client-side.
- **Transition** (0–10) is applied per colour, live.
- **Lights** toggles power.
- **Accept** / **Enter** persists the colour (writes the last-colour cache and
  leaves music mode with a final `SetColour`).
- **Cancel** / **Escape** reverts to the pre-open colour.

## Settings & discovery

The settings UI (devices + discovery) lives in the **Settings** tab of the
default app. The standalone window is still reachable for bootstrapping a
first-run config (it edits devices only and preserves rooms/scenes on save):

```bash
notuya-gui -config   # deprecated; prefer the in-app Settings tab
```

It (and the Settings tab) let you add, edit, and remove bulbs and **discover**
bulbs on the LAN via notuya-go's `pkg/discovery`. Room management
(create/rename/remove rooms and assign devices) lives in the **Rooms** tab.
Discovery finds each bulb's `device_id` and IP but **not** its `local_key` —
that comes from Tuya's cloud, so you paste each key by hand. Saving preserves
any keys this tool doesn't model (e.g. `wallpaper_sync`, theme keys) and writes
atomically. Launching the app or picker with no config file prints a hint
pointing here.

## Why CGO

`notuya-go` is strictly `CGO_ENABLED=0` with zero external dependencies. This
module deliberately breaks both — and only here — because a true always-on-top
overlay on Wayland needs the `wlr-layer-shell` protocol, which Gio (pure Go)
does not support. The path to a real layer-shell surface in Go is GTK4 via the
`gotk4` + `gotk4-layer-shell` bindings, which are CGO-only. The desktop app also
uses libadwaita (via the `gotk4-adwaita` bindings) for its modern GNOME widgets.
This is confined to the final GUI binary; the `notuya-go` packages it imports
remain CGO-free.

## Relationship to notuya-go

A separate Go module that depends on `github.com/alex-bluetrain/notuya-go`. In
development the two repos sit side by side, wired with a local `replace`:

```
require github.com/alex-bluetrain/notuya-go v0.0.0
replace github.com/alex-bluetrain/notuya-go => ../notuya-go
```

It consumes the promoted `pkg/` surface: `pkg/protocol`, `pkg/protocol35`,
`pkg/device`, `pkg/bulb`, and `pkg/discovery` (for the settings window's LAN
scan). It never touches DP numbers or wire framing — it
builds a `*bulb.Bulb` per device and feeds `bulb.StreamColours` a channel of
`bulb.StreamColour`.
