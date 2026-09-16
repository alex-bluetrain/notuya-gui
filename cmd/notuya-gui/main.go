package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
)

func main() {
	settingsMode := flag.Bool("config", false, "DEPRECATED: open the standalone settings window (use the in-app Ajustes tab instead)")
	pickerMode := flag.Bool("picker", false, "open the layer-shell colour-wheel overlay instead of the desktop app")
	flag.Parse()

	configPath := resolveConfigPath()

	if *settingsMode {
		// The settings window can create a config from scratch, so a missing
		// file is not an error here — start with no devices.
		var devices []Device
		var rooms []Room
		var scenes []Scene
		if cfg, err := loadConfig(configPath); err == nil {
			devices = cfg.Devices
			rooms = cfg.Rooms
			scenes = cfg.Scenes
		}
		os.Exit(runSettings(configPath, devices, rooms, scenes))
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "notuya-gui:", err)
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(os.Stderr, "hint: run `notuya-gui -config` to add devices and discover bulbs.")
		}
		os.Exit(1)
	}

	if *pickerMode {
		runPicker(configPath, cfg)
		return
	}

	runApp(configPath, cfg)
}
