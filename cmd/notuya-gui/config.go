package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// defaultColor is the colour assumed when the last-colour cache is missing
// or empty — matches picker.py and the CLI's get-color.
const defaultColor = "ffffff"

// Device is one entry of the `devices` array in config.json — the only part
// of that file this tool reads; theme/wallpaper keys used by the sibling
// Python project are ignored.
type Device struct {
	DeviceID  string `json:"device_id"`
	IPAddress string `json:"ip_address"`
	LocalKey  string `json:"local_key"`
	Name      string `json:"name"`
}

// Config is the subset of config.json this tool cares about.
type Config struct {
	Devices []Device `json:"devices"`
}

// resolveConfigPath applies the precedence: $NOTUYA_CONFIG env > the shared
// ~/.config/tuya/config.json that picker.py, the CLI and the daemon use.
func resolveConfigPath() string {
	if env := os.Getenv("NOTUYA_CONFIG"); env != "" {
		return env
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "notuya-gui", "config.json")
}

// lastColorPath returns the last-colour cache path beside the config file,
// named to agree with the CLI/daemon (last-color.txt).
func lastColorPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "last-color.txt")
}

// wheelCachePath returns the wheel bitmap cache path beside the config file,
// matching picker.py's .wheel_cache.bin.
func wheelCachePath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), ".wheel_cache.bin")
}

// loadConfig reads and parses config.json at path.
func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", path, err)
	}
	return &cfg, nil
}

// saveConfig writes devices back into config.json at path, preserving every
// other top-level key (wallpaper_sync, theme keys, anything the CLI/daemon
// own) by round-tripping the file through a map of raw messages. The write is
// atomic: a temp file is written then renamed over path, so a crash mid-write
// can't corrupt the shared config.
func saveConfig(path string, devices []Device) error {
	root := map[string]json.RawMessage{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &root); err != nil {
			return fmt.Errorf("config: parsing %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("config: reading %s: %w", path, err)
	}

	if devices == nil {
		devices = []Device{}
	}
	devicesJSON, err := json.Marshal(devices)
	if err != nil {
		return fmt.Errorf("config: encoding devices: %w", err)
	}
	root["devices"] = devicesJSON

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("config: encoding %s: %w", path, err)
	}

	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("config: creating %s: %w", dir, err)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return fmt.Errorf("config: writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("config: replacing %s: %w", path, err)
	}
	return nil
}

// readLastColor returns the cached last-applied colour (hex, no '#'), or the
// default white if the cache is missing or empty.
func readLastColor(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return defaultColor
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return defaultColor
	}
	return s
}

// writeLastColor persists the last-applied colour (hex, no '#').
func writeLastColor(path, hexColor string) error {
	return os.WriteFile(path, []byte(hexColor), 0o644)
}
