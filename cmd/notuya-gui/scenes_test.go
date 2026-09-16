package main

import (
	"testing"

	"github.com/averstraeten/notuya-go/pkg/device"
)

func TestStateFromStatus(t *testing.T) {
	tests := []struct {
		name   string
		id     string
		status deviceStatus
		want   SceneState
	}{
		{
			name:   "off stores only id+on",
			id:     "a",
			status: deviceStatus{On: false, Mode: device.ModeColour, Hue: 0.5, HasColour: true},
			want:   SceneState{DeviceID: "a", On: false},
		},
		{
			name:   "colour on",
			id:     "b",
			status: deviceStatus{On: true, Mode: device.ModeColour, Hue: 0.75, Sat: 0.8, BrightPct: 15, HasColour: true},
			want:   SceneState{DeviceID: "b", On: true, Mode: device.ModeColour, Hue: 0.75, Sat: 0.8, Bright: 15},
		},
		{
			name:   "white on",
			id:     "c",
			status: deviceStatus{On: true, Mode: device.ModeWhite, TempPct: 40, BrightPct: 90, HasTemp: true},
			want:   SceneState{DeviceID: "c", On: true, Mode: device.ModeWhite, Temp: 40, Bright: 90},
		},
		{
			name:   "colour mode without colour data leaves mode empty",
			id:     "d",
			status: deviceStatus{On: true, Mode: device.ModeColour, HasColour: false},
			want:   SceneState{DeviceID: "d", On: true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := stateFromStatus(tc.id, tc.status)
			if got != tc.want {
				t.Errorf("stateFromStatus = %+v, want %+v", got, tc.want)
			}
		})
	}
}
