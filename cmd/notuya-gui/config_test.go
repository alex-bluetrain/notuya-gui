package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
	  "devices": [
	    {"device_id": "abc", "ip_address": "10.0.0.5", "local_key": "k1", "name": "Lamp"},
	    {"device_id": "def", "ip_address": "10.0.0.6", "local_key": "k2", "name": "Strip"}
	  ],
	  "wallpaper_sync": true
	}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.Devices) != 2 {
		t.Fatalf("devices = %d want 2", len(cfg.Devices))
	}
	if cfg.Devices[0].Name != "Lamp" || cfg.Devices[1].IPAddress != "10.0.0.6" {
		t.Errorf("parsed devices wrong: %+v", cfg.Devices)
	}
}

func TestSaveConfigPreservesUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{
	  "devices": [
	    {"device_id": "old", "ip_address": "10.0.0.1", "local_key": "k0", "name": "Old"}
	  ],
	  "wallpaper_sync": true,
	  "theme": {"name": "gruvbox"}
	}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	devices := []Device{{DeviceID: "new", IPAddress: "10.0.0.2", LocalKey: "k1", Name: "New"}}
	if err := saveConfig(path, devices, nil, nil, nil); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("re-parse: %v", err)
	}

	if string(root["wallpaper_sync"]) != "true" {
		t.Errorf("wallpaper_sync changed/dropped: %s", root["wallpaper_sync"])
	}
	var theme map[string]string
	if err := json.Unmarshal(root["theme"], &theme); err != nil {
		t.Fatalf("theme key dropped or corrupt: %v", err)
	}
	if theme["name"] != "gruvbox" {
		t.Errorf("theme value changed: %+v", theme)
	}

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Devices) != 1 || cfg.Devices[0].DeviceID != "new" {
		t.Errorf("devices not replaced: %+v", cfg.Devices)
	}
}

func TestSaveConfigCreatesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	devices := []Device{{DeviceID: "a", IPAddress: "10.0.0.9", LocalKey: "k", Name: "A"}}
	if err := saveConfig(path, devices, nil, nil, nil); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Devices) != 1 || cfg.Devices[0].Name != "A" {
		t.Errorf("devices wrong: %+v", cfg.Devices)
	}
}

func TestGroupByRoom(t *testing.T) {
	cfg := &Config{
		Devices: []Device{
			{DeviceID: "a", Name: "A"},
			{DeviceID: "b", Name: "B"},
			{DeviceID: "c", Name: "C"}, // referenced by no room
		},
		Rooms: []Room{
			{Name: "Living", Devices: []string{"a", "b"}},
			{Name: "Bedroom", Devices: []string{"a", "gone"}}, // multi-room + stale id
		},
	}

	groups := groupByRoom(cfg)
	if len(groups) != 3 {
		t.Fatalf("groups = %d want 3 (Living, Bedroom, %s)", len(groups), unassignedRoomName)
	}

	if groups[0].Name != "Living" || len(groups[0].Devices) != 2 {
		t.Errorf("Living wrong: %+v", groups[0])
	}
	// Bedroom keeps only the valid id "a"; the stale "gone" is skipped.
	if groups[1].Name != "Bedroom" || len(groups[1].Devices) != 1 || groups[1].Devices[0].DeviceID != "a" {
		t.Errorf("Bedroom should drop stale id: %+v", groups[1])
	}
	// "a" belongs to two rooms — allowed.
	if groups[0].Devices[0].DeviceID != "a" {
		t.Errorf("Living first device = %q want a", groups[0].Devices[0].DeviceID)
	}
	// "c" is unreferenced → synthetic trailing group.
	last := groups[2]
	if last.Name != unassignedRoomName || len(last.Devices) != 1 || last.Devices[0].DeviceID != "c" {
		t.Errorf("unassigned group wrong: %+v", last)
	}
}

func TestGroupByRoomNoUnassignedGroupWhenAllAssigned(t *testing.T) {
	cfg := &Config{
		Devices: []Device{{DeviceID: "a"}, {DeviceID: "b"}},
		Rooms:   []Room{{Name: "All", Devices: []string{"a", "b"}}},
	}
	groups := groupByRoom(cfg)
	if len(groups) != 1 {
		t.Fatalf("groups = %d want 1 (no synthetic group)", len(groups))
	}
	if groups[0].Name != "All" {
		t.Errorf("group name = %q", groups[0].Name)
	}
}

func TestSaveConfigRoundTripsRoomsAndCoOwnedKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{
	  "devices": [
	    {"device_id": "a", "ip_address": "10.0.0.1", "local_key": "k", "name": "A"}
	  ],
	  "follow_mode": "wallpaper"
	}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	devices := []Device{{DeviceID: "a", IPAddress: "10.0.0.1", LocalKey: "k", Name: "A"}}
	rooms := []Room{{Name: "Living", Devices: []string{"a"}}}
	if err := saveConfig(path, devices, rooms, nil, nil); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if string(root["follow_mode"]) != `"wallpaper"` {
		t.Errorf("co-owned key changed/dropped: %s", root["follow_mode"])
	}

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rooms) != 1 || cfg.Rooms[0].Name != "Living" || len(cfg.Rooms[0].Devices) != 1 || cfg.Rooms[0].Devices[0] != "a" {
		t.Errorf("rooms not persisted: %+v", cfg.Rooms)
	}
}

func TestSaveConfigRoundTripsScenesAndCoOwnedKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{
	  "devices": [
	    {"device_id": "a", "ip_address": "10.0.0.1", "local_key": "k", "name": "A"}
	  ],
	  "follow_mode": "wallpaper"
	}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	devices := []Device{{DeviceID: "a", IPAddress: "10.0.0.1", LocalKey: "k", Name: "A"}}
	scenes := []Scene{
		{Name: "Película", States: []SceneState{
			{DeviceID: "a", On: true, Mode: "colour", Hue: 0.75, Sat: 0.8, Bright: 15},
			{DeviceID: "b", On: false},
		}},
	}
	if err := saveConfig(path, devices, nil, scenes, nil); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if string(root["follow_mode"]) != `"wallpaper"` {
		t.Errorf("co-owned key changed/dropped: %s", root["follow_mode"])
	}

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Scenes) != 1 || cfg.Scenes[0].Name != "Película" {
		t.Fatalf("scenes not persisted: %+v", cfg.Scenes)
	}
	got := cfg.Scenes[0].States
	if len(got) != 2 {
		t.Fatalf("states = %d want 2: %+v", len(got), got)
	}
	if !got[0].On || got[0].Mode != "colour" || got[0].Hue != 0.75 || got[0].Sat != 0.8 || got[0].Bright != 15 {
		t.Errorf("on-state not persisted: %+v", got[0])
	}
	if got[1].On || got[1].DeviceID != "b" {
		t.Errorf("off-state not persisted: %+v", got[1])
	}
}

func TestSaveConfigDoesNotEscapeHTMLInKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	key := "c{J)1t6y/om!M>gP"
	devices := []Device{{DeviceID: "a", IPAddress: "10.0.0.1", LocalKey: key, Name: "A"}}
	if err := saveConfig(path, devices, nil, nil, nil); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `\u003e`) {
		t.Errorf("config.json escapes '>' as \\u003e:\n%s", data)
	}
	if !strings.Contains(string(data), key) {
		t.Errorf("config.json does not contain the literal key %q:\n%s", key, data)
	}
}

func TestPathsBesideConfig(t *testing.T) {
	cfg := "/home/x/.config/tuya/config.json"
	if got := wheelCachePath(cfg); got != "/home/x/.config/tuya/.wheel_cache.bin" {
		t.Errorf("wheelCachePath = %q", got)
	}
}

func TestScreenSyncRoundTripKeepsOtherKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"devices":[],"theme":{"name":"gruvbox"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sync := &ScreenSync{Brightness: 1.5, Presets: []SyncPreset{{
		ID: "p1", Name: "Game",
		Target:        SyncTarget{Kind: "window", WindowClass: "steam_app_1", TitleMatch: "Elden"},
		RestoreTokens: map[string]string{"window": "tok"},
		Regions:       []SyncRegion{{ID: "r1", Name: "Left", Rect: [4]float64{0, 0, 0.3, 1}, Devices: []string{"a"}}},
	}}}
	if err := saveConfig(path, nil, nil, nil, sync); err != nil {
		t.Fatal(err)
	}
	// A save without screenSync (the wizard's) must keep it.
	if err := saveConfig(path, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ScreenSync == nil || cfg.ScreenSync.Brightness != 1.5 || len(cfg.ScreenSync.Presets) != 1 {
		t.Fatalf("screenSync = %+v", cfg.ScreenSync)
	}
	p := cfg.ScreenSync.Presets[0]
	if p.Target.WindowClass != "steam_app_1" || p.RestoreTokens["window"] != "tok" || p.Regions[0].Rect[2] != 0.3 {
		t.Errorf("preset = %+v", p)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "gruvbox") {
		t.Error("theme key lost")
	}
}

func TestSyncBindIsExclusive(t *testing.T) {
	p := SyncPreset{Regions: []SyncRegion{{Devices: []string{"a", "b"}}, {Devices: []string{"c"}}}}
	p.bind(1, "a")
	if strings.Join(p.Regions[0].Devices, ",") != "b" || strings.Join(p.Regions[1].Devices, ",") != "c,a" {
		t.Errorf("regions = %+v", p.Regions)
	}
}
