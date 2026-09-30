package main

import (
	"errors"
	"fmt"
	"os"
)

func main() {
	configPath := resolveConfigPath()

	cfg, err := loadConfig(configPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "notuya-gui:", err)
		os.Exit(1)
	}

	if err != nil || len(cfg.Devices) == 0 {
		code, applied := runWizard(configPath)
		if !applied {
			os.Exit(code)
		}
		// Setup finished: load the config the wizard just wrote and start
		// the app at top level (not nested inside the wizard's GApplication).
		cfg, err = loadConfig(configPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "notuya-gui:", err)
			os.Exit(1)
		}
	}

	runApp(configPath, cfg)
}
