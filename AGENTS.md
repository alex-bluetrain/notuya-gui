# notuya-gui

A native color-wheel picker for the Tuya smart bulbs, written in Go. It is a
port of `~/.config/tuya/picker.py` (a GTK4 layer-shell overlay that drove the
bulbs through the `notuyad` HTTP daemon), rewritten to drive the bulbs
**in-process** by importing the `notuya-go` library directly. No daemon, no
subprocess, no Python.

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
  gtk4-layer-shell via pkg-config (`gtk4`, `gtk4-layer-shell-0`).
- This is confined to this module. The notuya-go library packages it
  imports remain CGO-free and are compiled by the Go compiler as usual;
  only the final GUI binary needs a C toolchain and the GTK4 headers.
- If a pure-Go overlay ever becomes possible, this is the dependency to
  drop — nothing in the streaming path depends on GTK4.

### Build prerequisites

System packages (Arch/Omarchy names): `gtk4`, `gtk4-layer-shell`, a C
compiler, and pkg-config. Verify the toolchain resolves the libraries:

```bash
pkg-config --exists gtk4 gtk4-layer-shell-0 && echo ok
```

Note the pkg-config module for the layer-shell library is
`gtk4-layer-shell-0` (the `.pc` is `gtk4-layer-shell-0.pc`), not
`gtk4-layer-shell`.

## Architecture

Single binary, one module:

```
cmd/notuya-gui/main.go     entry point: layer-shell window + GTK4 app
```

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

Shared with the CLI and daemon — the same `config.json`:

- Config: `~/.config/tuya/config.json` (the `devices` array:
  `device_id`, `ip_address`, `local_key`, `name`).
- Last colour cache: read on open (to revert on cancel), written on a
  committed exit — the same file `picker.py` and the daemon use, so
  `GET /color`, the CLI, and the GUI all agree on what is lit.

## Settings window

`notuya-gui -config` opens a settings window (an ordinary GTK4 toplevel,
**not** a layer-shell overlay) instead of the picker. It edits the shared
`config.json`'s `devices` array — add, edit, and remove bulbs — and can
**discover** bulbs on the LAN via notuya-go's `pkg/discovery.Scan`
(reused; no UDP code lives here).

- **Keys are entered by hand.** LAN discovery yields `device_id` + `ip`
  only; a bulb's `local_key` comes from Tuya's cloud, which this tool
  deliberately does not touch. Clicking a discovered device pre-fills IP +
  Device ID; the user pastes the Local Key.
- **Writes preserve unknown keys.** `saveConfig` round-trips the file
  through `map[string]json.RawMessage`, replacing only `devices`, so keys
  this tool doesn't model (`wallpaper_sync`, theme keys, anything the
  CLI/daemon own) survive. The write is atomic (temp file + rename) since
  the config is co-owned.
- If the picker is launched with **no** config file, it exits with a hint
  pointing at `notuya-gui -config` so a first-run user can create one.

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
