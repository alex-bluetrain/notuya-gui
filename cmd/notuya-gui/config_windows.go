package main

// screenSyncConfig is the Windows half of the Screen Sync config: its single
// key is "screenSyncWindows". It is embedded into Config, so this key lives at
// the JSON root. Linux defines its own screenSyncConfig (config_linux.go) with
// a "screenSyncLinux" key instead; neither OS names the other's key, so each
// is preserved untouched across the other's saves.
type screenSyncConfig struct {
	ScreenSyncWindows *ScreenSyncWindows `json:"screenSyncWindows,omitempty"`
}

// ScreenSyncWindows is intentionally empty. Screen Sync is not implemented on
// Windows yet; its fields will be defined by that implementation when it
// exists, discovered through building it rather than cloned from Linux. Until
// then this is a placeholder so the "screenSyncWindows" config key has an
// owner and is round-tripped untouched.
type ScreenSyncWindows struct{}
