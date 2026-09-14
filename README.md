# notuya-gui

A native color-wheel picker for Tuya smart bulbs, written in Go. It is a port
of `picker.py` (a GTK4 layer-shell overlay that drove the bulbs through the
`notuyad` HTTP daemon), rewritten to drive the bulbs **in-process** by
importing the [`notuya-go`](https://github.com/averstraeten/notuya-go) library
directly. No daemon, no subprocess, no Python.

The GUI opens a full-screen layer-shell overlay with an HSV color wheel; drag
across the wheel and each configured bulb is streamed the colour live via
`bulb.StreamColours` (the same loop the CLI's `music` command uses), throttled
and coalesced newest-wins so a slow bulb never stalls the UI.

## Requirements

- Go 1.27+
- A C toolchain and `pkg-config`
- GTK4 and gtk4-layer-shell system libraries

On Arch/Omarchy:

```bash
sudo pacman -S gtk4 gtk4-layer-shell base-devel
```

Verify the toolchain resolves the libraries:

```bash
make deps        # or: pkg-config --exists gtk4 gtk4-layer-shell-0 && echo ok
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

Shared with the CLI and daemon — the same files:

- **Config:** `~/.config/tuya/config.json` — the `devices` array
  (`device_id`, `ip_address`, `local_key`, `name`).
- **Last-colour cache:** `last-color.txt` beside the config — read on open to
  revert on cancel, written on a committed exit. `GET /color`, the CLI, and
  the GUI all agree on what is lit.

## Usage

- **Drag / click** the wheel to pick a hue and saturation; the swatch and the
  bulbs update live.
- **Brillo** (brightness, 1–100%) scales the streamed RGB client-side.
- **Transición** (0–10) is applied per colour, live.
- **Luces** toggles power.
- **Aceptar** / **Enter** persists the colour (writes the last-colour cache and
  leaves music mode with a final `SetColour`).
- **Cancelar** / **Escape** reverts to the pre-open colour.

## Settings & discovery

```bash
notuya-gui -config
```

Opens a settings window (a normal toplevel, not the overlay) to add, edit, and
remove bulbs, and to **discover** bulbs on the LAN via notuya-go's
`pkg/discovery`. Discovery finds each bulb's `device_id` and IP but **not** its
`local_key` — that comes from Tuya's cloud, so you paste each key by hand.
Saving preserves any keys this tool doesn't model (e.g. `wallpaper_sync`,
theme keys) and writes atomically. Launching the picker with no config file
prints a hint pointing here.

## Why CGO

`notuya-go` is strictly `CGO_ENABLED=0` with zero external dependencies. This
module deliberately breaks both — and only here — because a true always-on-top
overlay on Wayland needs the `wlr-layer-shell` protocol, which Gio (pure Go)
does not support. The path to a real layer-shell surface in Go is GTK4 via the
`gotk4` + `gotk4-layer-shell` bindings, which are CGO-only. This is confined to
the final GUI binary; the `notuya-go` packages it imports remain CGO-free.

## Relationship to notuya-go

A separate Go module that depends on `github.com/averstraeten/notuya-go`. In
development the two repos sit side by side, wired with a local `replace`:

```
require github.com/averstraeten/notuya-go v0.0.0
replace github.com/averstraeten/notuya-go => ../notuya-go
```

It consumes the promoted `pkg/` surface: `pkg/protocol`, `pkg/protocol35`,
`pkg/device`, `pkg/bulb`, and `pkg/discovery` (for the settings window's LAN
scan). It never touches DP numbers or wire framing — it
builds a `*bulb.Bulb` per device and feeds `bulb.StreamColours` a channel of
`bulb.StreamColour`.
