package main

import (
	"bytes"
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

// Room is one entry of the top-level `rooms` array in config.json. Rooms are a
// first-class entity keyed by device_id, so the app can resolve "all lights in
// a room" from the config alone without querying any lamp. A device may belong
// to several rooms.
type Room struct {
	Name    string   `json:"name"`
	Devices []string `json:"devices"` // device_id references
}

// SceneState is one light's captured state within a scene. When Off (On is
// false) only DeviceID + On are meaningful; the mode/colour/temp/bright fields
// are omitted from JSON. Hue and Sat are stored in 0-1 units (matching
// deviceStatus) to avoid round-trip drift; Bright and Temp are 0-100 percents.
type SceneState struct {
	DeviceID string  `json:"device_id"`
	On       bool    `json:"on"`
	Mode     string  `json:"mode,omitempty"`   // device.ModeColour / ModeWhite
	Hue      float64 `json:"hue,omitempty"`    // 0-1 (colour mode)
	Sat      float64 `json:"sat,omitempty"`    // 0-1 (colour mode)
	Bright   float64 `json:"bright,omitempty"` // 0-100
	Temp     float64 `json:"temp,omitempty"`   // 0-100 (white mode)
}

// Scene is a named, software-only snapshot: applying it fans out discrete
// commands to the lights it lists. It is global (any subset of devices, across
// rooms) and depends on no firmware or cloud feature.
type Scene struct {
	Name   string       `json:"name"`
	States []SceneState `json:"states"`
}

// Config is the subset of config.json this tool cares about.
type Config struct {
	Devices []Device `json:"devices"`
	Rooms   []Room   `json:"rooms"`
	Scenes  []Scene  `json:"scenes"`
}

// roomGroup is a resolved room: its name and the devices it contains, in the
// order they appear in the config's devices array.
type roomGroup struct {
	Name    string
	Devices []*Device
}

// unassignedRoomName is the synthetic group holding devices not referenced by
// any room. It is computed at load time and never written back to the config.
const unassignedRoomName = "No room"

// groupByRoom resolves the config's rooms into ordered roomGroups. Each room in
// cfg.Rooms becomes a group containing the devices its device_id list points at
// (stale ids — no matching device — are skipped). Every device not referenced
// by any room is appended in a trailing synthetic "No room" group. Grouping is
// O(devices) and performs zero device I/O.
func groupByRoom(cfg *Config) []roomGroup {
	byID := make(map[string]*Device, len(cfg.Devices))
	for i := range cfg.Devices {
		byID[cfg.Devices[i].DeviceID] = &cfg.Devices[i]
	}

	assigned := make(map[string]bool, len(cfg.Devices))
	groups := make([]roomGroup, 0, len(cfg.Rooms)+1)
	for _, room := range cfg.Rooms {
		g := roomGroup{Name: room.Name}
		for _, id := range room.Devices {
			dev, ok := byID[id]
			if !ok {
				continue // stale reference, tolerated
			}
			g.Devices = append(g.Devices, dev)
			assigned[id] = true
		}
		groups = append(groups, g)
	}

	var unassigned roomGroup
	unassigned.Name = unassignedRoomName
	for i := range cfg.Devices {
		if !assigned[cfg.Devices[i].DeviceID] {
			unassigned.Devices = append(unassigned.Devices, &cfg.Devices[i])
		}
	}
	if len(unassigned.Devices) > 0 {
		groups = append(groups, unassigned)
	}
	return groups
}

// resolveConfigPath applies the precedence: $NOTUYA_CONFIG env >
// ~/.config/notuya-gui/config.json.
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

// saveConfig writes devices, rooms and scenes back into config.json at path,
// preserving every other top-level key (wallpaper_sync, theme keys, anything the
// CLI/daemon own) by round-tripping the file through a map of raw messages. The
// write is atomic: a temp file is written then renamed over path, so a crash
// mid-write can't corrupt the shared config.
func saveConfig(path string, devices []Device, rooms []Room, scenes []Scene) error {
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
	devicesJSON, err := marshalNoEscape(devices)
	if err != nil {
		return fmt.Errorf("config: encoding devices: %w", err)
	}
	root["devices"] = devicesJSON

	if rooms == nil {
		rooms = []Room{}
	}
	roomsJSON, err := marshalNoEscape(rooms)
	if err != nil {
		return fmt.Errorf("config: encoding rooms: %w", err)
	}
	root["rooms"] = roomsJSON

	if scenes == nil {
		scenes = []Scene{}
	}
	scenesJSON, err := marshalNoEscape(scenes)
	if err != nil {
		return fmt.Errorf("config: encoding scenes: %w", err)
	}
	root["scenes"] = scenesJSON

	out, err := marshalNoEscape(root)
	if err != nil {
		return fmt.Errorf("config: encoding %s: %w", path, err)
	}
	if out, err = indentJSON(out); err != nil {
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

// marshalNoEscape is json.Marshal without HTML escaping: local keys are raw
// AES bytes that may contain '<', '>' or '&', and escaping them as \uXXXX
// makes a key copied out of the file 21 characters instead of 16.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func indentJSON(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, data, "", "  "); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
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
