# notuya-gui

> **Response style:** always answer "tl;dr" — brief, high-level responses.

Native GTK4/libadwaita desktop controller for Tuya smart bulbs, in Go. Drives
bulbs **in-process** via the `notuya-go` library — no daemon, no subprocess. One
window: per-device light control grouped by room, plus a Settings tab for LAN
discovery and key entry. A first-run wizard bootstraps the config when none
exists.

## Commands

```bash
make build   # go build (CGO_ENABLED=1); slow first link, fast after
make check   # go vet + go test
```

Needs `gtk4` + `libadwaita` (pkg-config) and a C compiler. Only the final binary
uses CGO; the `notuya-go` packages it imports stay CGO-free.

## Layout

Everything is under `cmd/notuya-gui/` (plus `internal/screensync/`):

- `main.go` — entry: no/empty config → wizard, else app
- `app.go` — window, tabs, room helpers, one `control` per device
- `lights_tab.go` / `colour_controls.go` — Lights tab + shared colour widget
- `device_panel.go` — per-device cached state for the Rooms summary
- `control.go` — per-device session, commands, live-stream ownership
- `keeper.go` — per-device link keeper: holds the session, probes, redials
- `stream.go` — live colour streamer (DP 28) for drags and Screen Sync
- `scenes.go` / `scene_editor.go` — scenes
- `screen_sync_tab.go` / `sync_overlay.go` — Screen Sync tab + region overlay
- `wizard.go` — first-run window AND the embedded Settings tab
- `config.go` — Config/Device/Room/Scene/ScreenSync types, `groupByRoom`, `saveConfig`
- `wheel.go` — colour wheel bitmap (colour maths comes from notuya-go `dp`)
- `internal/screensync/` — capture (portal + GStreamer), Hyprland window
  tracker, region averaging engine

## Key facts

- Config: `~/.config/notuya-gui/config.json` (override `$NOTUYA_CONFIG`). Rooms
  are top-level, not a per-device field. `saveConfig` is atomic and preserves
  unknown keys (other tools may share the file).
- Sessions come from `session/v35.Open`; bulbs are driven via `bulb.Bulb` and
  `dp` types. Each bulb's session is **held open** by its keeper. The
  session's own idle heartbeat keeps the link alive and a socket error
  closes `Done()`, but a bulb that loses power never errors the socket: the
  keeper's waited heartbeat every 10s is what notices. It redials with
  1s–10s backoff when the link is dead.
- Tabs **listen, never poll**: Lights rows and Rooms panels `Subscribe` to
  the status the keeper publishes on every (re)connect and after a failed
  command.
- Commands may run concurrently over the session; `c.mu` guards only the
  control's fields and the dial. Live colour drags and Screen Sync borrow
  the bulb via the streamer (DP 28), which closes the command session; the
  keeper idles meanwhile and redials when the stream ends.
- The GUI holds each bulb's **only** local connection. Another local client
  (tinytuya, the Tuya app on LAN) knocks the GUI off and vice versa — they
  fight in a reconnect loop. Close the GUI before using other tools.
- Local keys come from Tuya's cloud, entered by hand — discovery only yields
  `device_id` + `ip`.
- Develop against a local `notuya-go` via a gitignored `go.work`, never a
  `replace` in `go.mod`.

## Boundaries

1. **Copy the existing pattern.** Drive bulbs through the per-device `control`
   (`ctl.async(...)` + `SetColour`/`SetPower`), as `lights_tab.go` does. No
   throwaway sessions or one-off streams.
2. **Prefer native `Adw*` widgets** over hand-built `gtk.Box` + CSS. Ask for a
   reference rather than guessing at cosmetics.
3. **Never touch the user's real config** (`~/.config/notuya-gui`,
   `$NOTUYA_CONFIG`) without explicit approval.
4. **Test the GUI via AT-SPI, not screenshots** — `scripts/uitest.py` (use the
   `cage` subcommand for isolated capture, never the live desktop).
   `scripts/synctest.py` runs the Screen Sync suite on top of it, in a nested
   cage with a temp config and fake bulbs.
