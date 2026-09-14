package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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

func TestLastColorRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "last-color.txt")
	if got := readLastColor(path); got != defaultColor {
		t.Errorf("missing cache = %q want %q", got, defaultColor)
	}
	if err := writeLastColor(path, "ff8800"); err != nil {
		t.Fatal(err)
	}
	if got := readLastColor(path); got != "ff8800" {
		t.Errorf("round trip = %q want ff8800", got)
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
	if err := saveConfig(path, devices); err != nil {
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
	if err := saveConfig(path, devices); err != nil {
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

func TestPathsBesideConfig(t *testing.T) {
	cfg := "/home/x/.config/tuya/config.json"
	if got := lastColorPath(cfg); got != "/home/x/.config/tuya/last-color.txt" {
		t.Errorf("lastColorPath = %q", got)
	}
	if got := wheelCachePath(cfg); got != "/home/x/.config/tuya/.wheel_cache.bin" {
		t.Errorf("wheelCachePath = %q", got)
	}
}
