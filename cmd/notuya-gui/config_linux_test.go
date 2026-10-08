package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScreenSyncRoundTripKeepsOtherKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"devices":[],"theme":{"name":"gruvbox"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sync := screenSyncConfig{ScreenSyncLinux: &ScreenSyncLinux{Brightness: 1.5, Presets: []SyncPreset{{
		ID: "p1", Name: "Game",
		Target:        SyncTarget{Kind: "window", WindowClass: "steam_app_1", TitleMatch: "Elden"},
		RestoreTokens: map[string]string{"window": "tok"},
		Regions:       []SyncRegion{{ID: "r1", Name: "Left", Rect: [4]float64{0, 0, 0.3, 1}, Devices: []string{"a"}}},
	}}}}
	if err := saveConfig(path, nil, nil, nil, sync); err != nil {
		t.Fatal(err)
	}
	// A save without screenSyncLinux (the wizard's) must keep it.
	if err := saveConfig(path, nil, nil, nil, screenSyncConfig{}); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ScreenSyncLinux == nil || cfg.ScreenSyncLinux.Brightness != 1.5 || len(cfg.ScreenSyncLinux.Presets) != 1 {
		t.Fatalf("screenSyncLinux = %+v", cfg.ScreenSyncLinux)
	}
	p := cfg.ScreenSyncLinux.Presets[0]
	if p.Target.WindowClass != "steam_app_1" || p.RestoreTokens["window"] != "tok" || p.Regions[0].Rect[2] != 0.3 {
		t.Errorf("preset = %+v", p)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "gruvbox") {
		t.Error("theme key lost")
	}
}

// TestScreenSyncLinuxSaveKeepsWindowsKey is the guarantee that one OS never
// writes over the other's Screen Sync block: a config that already holds a
// screenSyncWindows key must still hold it, unchanged, after a Linux save that
// writes its own screenSyncLinux key.
func TestScreenSyncLinuxSaveKeepsWindowsKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	const windows = `{"foo":"bar","nested":{"n":1}}`
	if err := os.WriteFile(path, []byte(`{"devices":[],"screenSyncWindows":`+windows+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sync := screenSyncConfig{ScreenSyncLinux: &ScreenSyncLinux{Brightness: 1.5}}
	if err := saveConfig(path, nil, nil, nil, sync); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	if _, ok := root["screenSyncLinux"]; !ok {
		t.Errorf("screenSyncLinux key not written: %s", data)
	}
	// The Windows block must be present and structurally identical; compare
	// re-marshalled forms so reformatting (indentation) does not matter.
	got := compact(t, root["screenSyncWindows"])
	want := compact(t, json.RawMessage(windows))
	if got != want {
		t.Errorf("screenSyncWindows changed:\n got %s\nwant %s", got, want)
	}
}

func compact(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("screenSyncWindows not valid JSON: %v", err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSyncBindIsExclusive(t *testing.T) {
	p := SyncPreset{Regions: []SyncRegion{{Devices: []string{"a", "b"}}, {Devices: []string{"c"}}}}
	p.bind(1, "a")
	if strings.Join(p.Regions[0].Devices, ",") != "b" || strings.Join(p.Regions[1].Devices, ",") != "c,a" {
		t.Errorf("regions = %+v", p.Regions)
	}
}
