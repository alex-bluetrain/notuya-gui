# notuya-gui

A native desktop controller for Tuya smart bulbs. Everything runs over your
local network — no cloud account, no vendor app, no background daemon.

![Go](https://img.shields.io/badge/Go-1.27-00ADD8?logo=go&logoColor=white)
![Platform](https://img.shields.io/badge/platform-Linux%20%2F%20Wayland-333)
![GTK4](https://img.shields.io/badge/GTK4-libadwaita-4A86CF)

## Overview

notuya-gui speaks the Tuya v3.5 protocol directly on the LAN. It is a single Go
binary that imports the [`notuya-go`](https://github.com/alex-bluetrain/notuya-go)
library and drives each bulb in-process, so there is nothing else to install or
keep running.

## Features

- **Colour and brightness control** — a colour wheel plus brightness and
  colour-temperature sliders, per light. Dragging the wheel updates the bulb in
  real time.
- **Rooms and scenes** — organise lights into rooms and save named colour
  snapshots you can apply to a group in one click.
- **LAN discovery** — find bulbs on your network from the app; keys stay local.
- **Native GNOME interface** — built with GTK4 and libadwaita, not a web view.

## Requirements

- Go 1.27 or newer and a C toolchain (the GUI links GTK4 through cgo)
- GTK4 and libadwaita, discoverable via `pkg-config`
- Linux with Wayland

## Installation

```bash
# System dependencies (Arch / Omarchy package names)
sudo pacman -S gtk4 libadwaita base-devel

# Build and install into ~/.local/bin
make build
make install
```

The first GTK4-linked build is slow because the C link step dominates; later
builds are fast.

## Usage

```bash
notuya-gui              # desktop app
```

On first launch with no configuration, a setup wizard scans the LAN, lists the
bulbs it finds, and walks you through pasting each one's local key.

The desktop app has three tabs:

- **Lights** — per-device power, colour wheel, and brightness / colour-temperature
  sliders.
- **Scenes** — apply, edit, or delete named colour snapshots.
- **Settings** — discover bulbs on the LAN and enter their keys.

Discovery reports each bulb's device ID and IP address, but not its local key.
That key comes from Tuya's cloud, so you enter it by hand once; it is stored
locally and never leaves your machine.

## Configuration

Configuration lives at `~/.config/notuya-gui/config.json` (override with
`$NOTUYA_CONFIG`). Devices and rooms are top-level entities, so the app resolves
the lights in a room from the file alone, without querying a bulb:

```json
{
  "devices": [
    { "device_id": "ebfake1111111111111111", "ip_address": "192.0.2.10", "local_key": "fake-local-key16", "name": "Desk Lamp" }
  ],
  "rooms": [
    { "name": "Living Room", "devices": ["ebfake1111111111111111"] }
  ]
}
```

A device in no room appears under a synthetic "No room" group; a room pointing
at a missing device is ignored. Writes are atomic and preserve any keys this
tool does not model.

## Development

```bash
make check    # go vet + go test
```

## Design notes

<details>
<summary>Why this binary links C, while the library does not</summary>

`notuya-go` is built with `CGO_ENABLED=0` and no external dependencies. This
module deliberately breaks both, and only in the final binary: the native GTK4 +
libadwaita interface needs the `gotk4` and `gotk4-adwaita` bindings, which are
cgo-only. The `notuya-go` packages it imports remain cgo-free.

</details>

<details>
<summary>Relationship to notuya-go</summary>

This is a separate Go module that depends on the published
`github.com/alex-bluetrain/notuya-go`. To build against a local checkout, use a
`go.work` file (gitignored, so it stays local) rather than a `replace` directive
in `go.mod`, which would affect everyone consuming this module:

```bash
go work init . ../notuya-go
```

It consumes the library's public `pkg/` surface — `pkg/protocol`,
`pkg/protocol35`, `pkg/device`, `pkg/bulb`, and `pkg/discovery` — and never
touches DP numbers or wire framing. It builds a `*bulb.Bulb` per device and feeds
`bulb.StreamColours` a channel of colours during live drags.

</details>
