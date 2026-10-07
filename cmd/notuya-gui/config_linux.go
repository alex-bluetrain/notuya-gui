package main

import "slices"

// screenSyncConfig is the Linux half of the Screen Sync config: its single key
// is "screenSyncLinux". It is embedded into Config, so this key lives at the
// JSON root. Windows defines its own screenSyncConfig (config_windows.go) with
// a "screenSyncWindows" key instead; neither OS names the other's key, so each
// is preserved untouched across the other's saves.
type screenSyncConfig struct {
	ScreenSyncLinux *ScreenSyncLinux `json:"screenSyncLinux,omitempty"`
}

// ScreenSyncLinux holds the Linux Screen Sync presets. Brightness scales every
// region colour (0.25-2.0); zero means unset and reads as 1.
type ScreenSyncLinux struct {
	Brightness float64 `json:"brightness"`
	// Mapping is "light" (match the screen's light output, the default)
	// or "values" (send the screen's sRGB values as HSV, brighter).
	Mapping string       `json:"mapping,omitempty"`
	Presets []SyncPreset `json:"presets"`
}

// SyncPreset is a capture target and the regions drawn on it.
type SyncPreset struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	Target SyncTarget `json:"target"`
	// RestoreTokens are portal tokens that skip the share picker, keyed by
	// monitor connector or "window".
	RestoreTokens map[string]string `json:"restoreTokens,omitempty"`
	Regions       []SyncRegion      `json:"regions"`
}

// SyncTarget is what a preset captures: Kind "monitors" or "window".
type SyncTarget struct {
	Kind        string   `json:"kind"`
	Monitors    []string `json:"monitors,omitempty"`
	WindowClass string   `json:"windowClass,omitempty"`
	TitleMatch  string   `json:"titleMatch,omitempty"`
}

// SyncRegion is a rectangle normalised to the target canvas (x, y, w, h)
// and the lights it drives. A light belongs to at most one region of a
// preset.
type SyncRegion struct {
	ID      string     `json:"id"`
	Name    string     `json:"name"`
	Rect    [4]float64 `json:"rect"`
	Devices []string   `json:"devices"`
}

// bind gives region idx the light id, taking it from any other region of
// the preset (bindings are exclusive).
func (p *SyncPreset) bind(idx int, id string) {
	for i := range p.Regions {
		p.Regions[i].Devices = slices.DeleteFunc(p.Regions[i].Devices, func(d string) bool { return d == id })
	}
	p.Regions[idx].Devices = append(p.Regions[idx].Devices, id)
}
