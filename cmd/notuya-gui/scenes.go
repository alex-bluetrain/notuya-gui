package main

import (
	"context"
	"sync"

	"github.com/averstraeten/notuya-go/pkg/device"
)

// captureScene builds a Scene named `name` from the current state of the
// controls whose include[i] is true. It refreshes those devices concurrently;
// a device whose refresh fails is omitted rather than aborting the capture, so
// one unreachable bulb doesn't sink the whole snapshot.
//
// Hue and Sat are stored in the same 0-1 units deviceStatus reports (no
// conversion), so applying a scene round-trips cleanly.
func captureScene(name string, controls []*control, include []bool) Scene {
	type result struct {
		state SceneState
		ok    bool
	}
	results := make([]result, len(controls))

	var wg sync.WaitGroup
	for i := range controls {
		if i >= len(include) || !include[i] {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctl := controls[i]
			ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
			defer cancel()
			st, err := ctl.Refresh(ctx)
			if err != nil {
				return // omit this light
			}
			results[i] = result{state: stateFromStatus(ctl.dev.DeviceID, st), ok: true}
		}(i)
	}
	wg.Wait()

	scene := Scene{Name: name}
	for _, r := range results {
		if r.ok {
			scene.States = append(scene.States, r.state)
		}
	}
	return scene
}

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
