package main

import (
	"context"

	"github.com/averstraeten/notuya-go/pkg/device"
)

// stateFromStatus maps a refreshed deviceStatus to a SceneState. An off light
// stores only device_id + on; an on light stores its mode plus the data that
// mode needs (colour hue/sat or white temp) and brightness.
func stateFromStatus(deviceID string, st deviceStatus) SceneState {
	out := SceneState{DeviceID: deviceID, On: st.On}
	if !st.On {
		return out
	}
	switch {
	case st.Mode == device.ModeColour && st.HasColour:
		out.Mode = device.ModeColour
		out.Hue = st.Hue
		out.Sat = st.Sat
		out.Bright = st.BrightPct
	case st.Mode == device.ModeWhite && st.HasTemp:
		out.Mode = device.ModeWhite
		out.Temp = st.TempPct
		out.Bright = st.BrightPct
	}
	return out
}

// applyScene fans a scene out to the lights it names, one goroutine per state,
// off the GTK thread. Stale device_ids (no matching control) are skipped and
// per-device errors are ignored — applying is best-effort, matching the
// group-power behaviour in app.go.
func applyScene(scene Scene, byID map[string]*control) {
	for _, st := range scene.States {
		ctl, ok := byID[st.DeviceID]
		if !ok {
			continue
		}
		go func(ctl *control, st SceneState) {
			ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
			defer cancel()
			_ = ctl.ApplyState(ctx, st)
		}(ctl, st)
	}
}
