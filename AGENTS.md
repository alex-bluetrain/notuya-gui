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

Everything is under `cmd/notuya-gui/`:

- `main.go` — entry: no/empty config → wizard, else app
- `app.go` — window, tabs, room helpers
- `lights_tab.go` / `colour_controls.go` — Lights tab + shared colour widget
- `control.go` / `stream.go` — per-device session; music-mode streamer for drags
- `scenes.go` / `scene_editor.go` — scenes
- `wizard.go` — first-run window AND the embedded Settings tab
- `config.go` — Config/Device/Room/Scene types, `groupByRoom`, `saveConfig`
- `color.go` / `wheel.go` — colour math + wheel bitmap

## Key facts

- Config: `~/.config/notuya-gui/config.json` (override `$NOTUYA_CONFIG`). Rooms
  are top-level, not a per-device field. `saveConfig` is atomic and preserves
  unknown keys (the file is co-owned with the CLI).
- Sessions (`protocol35`) are not concurrency-safe: `control` serializes commands
  behind a mutex. Live colour drags borrow the streamer (music mode).
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
