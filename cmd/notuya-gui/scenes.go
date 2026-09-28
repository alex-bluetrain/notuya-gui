package main

import (
	"context"
	"fmt"
	"strings"

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

// sceneStateColour turns one light's stored state into the swatch colour used
// for the scene-row gradient. Off lights read as a dark, near-black grey so an
// all-off scene still renders (and looks "off"). On lights use their hue/sat
// (colour) or warm→cool temp ramp (white), dimmed by brightness so a dim scene
// reads darker than a bright one — with a floor so a dim colour still shows its
// hue rather than collapsing to black.
func sceneStateColour(st SceneState) (r, g, b uint8) {
	if !st.On {
		return 40, 40, 44
	}
	// Brightness as a 0..1 multiplier with a floor, so dim lights darken but a
	// dim colour still reads as its hue, not black. Bright is a 0-100 percentage.
	bright := st.Bright / 100.0
	if bright < 0 {
		bright = 0
	} else if bright > 1 {
		bright = 1
	}
	dim := 0.35 + 0.65*bright
	scale := func(v uint8) uint8 { return uint8(float64(v) * dim) }
	switch st.Mode {
	case device.ModeColour:
		return hsvToRGBInt(st.Hue, st.Sat, dim)
	case device.ModeWhite:
		// temp 0 = warm (2700K-ish), 100 = cool (6500K-ish); lerp between two
		// representative sRGB whites so the gradient shows the tint difference.
		t := st.Temp / 100.0
		if t < 0 {
			t = 0
		} else if t > 1 {
			t = 1
		}
		lerp := func(a, b float64) uint8 { return scale(uint8(a + (b-a)*t)) }
		return lerp(255, 201), lerp(190, 226), lerp(120, 255)
	default:
		return scale(255), scale(255), scale(255)
	}
}

// sceneTileBaseCSS is the shared, scene-independent styling for the Hue-style
// scene tiles: a rounded card shape with a minimum size, a bottom scrim behind
// the name for legibility, a subtle hover lift, and small translucent corner
// action buttons. Per-tile gradients (from sceneGradientCSS) supply the fill.
const sceneTileBaseCSS = `
.scenes-flow > flowboxchild {
  padding: 0;
  margin: 0;
  border: none;
  outline: none;
  background: none;
  box-shadow: none;
  border-radius: 16px;
}
.scenes-flow > flowboxchild:hover,
.scenes-flow > flowboxchild:selected,
.scenes-flow > flowboxchild:focus,
.scenes-flow > flowboxchild:active {
  background: none;
  box-shadow: none;
  outline: none;
}
.scene-tile {
  min-width: 150px;
  min-height: 110px;
  padding: 0;
  margin: 0;
  border: none;
  outline: none;
  background-color: transparent;
  background-repeat: no-repeat;
  background-clip: padding-box;
  border-radius: 16px;
  box-shadow: none;
  opacity: 0.55;
  transition: opacity 200ms ease;
}
.scene-tile:hover,
.scene-tile:active,
.scene-tile:focus {
  box-shadow: none;
  border: none;
  outline: none;
}
.scene-tile:hover {
  opacity: 1;
}
.scene-add {
  min-width: 150px;
  min-height: 110px;
  padding: 0;
  margin: 0;
  border: 2px dashed alpha(currentColor, 0.25);
  border-radius: 16px;
  background: none;
  box-shadow: none;
  color: alpha(currentColor, 0.55);
  transition: color 200ms ease, border-color 200ms ease;
}
.scene-add:hover {
  border-color: alpha(currentColor, 0.5);
  color: currentColor;
  background: none;
  box-shadow: none;
}
.scene-tile-name {
  margin: 0 12px 10px 12px;
  padding-top: 24px;
  color: #ffffff;
  font-weight: bold;
  text-shadow: 0 1px 3px alpha(#000, 0.75);
}
.scene-tile-action {
  margin: 6px;
  min-width: 24px;
  min-height: 24px;
  padding: 2px;
  border-radius: 999px;
  color: #ffffff;
  background-color: alpha(#000, 0.32);
  box-shadow: none;
  opacity: 0;
  transition: opacity 300ms ease, background-color 300ms ease;
}
.scene-tile-action:hover {
  background-color: alpha(#000, 0.55);
}
overlay:hover .scene-tile-action {
  opacity: 1;
}
`

// sceneGradientCSS builds the gradient fill for one scene tile: a diagonal
// linear gradient across every light's colour in the scene, previewing what
// applying it will do. A scene with a single light still yields a (solid)
// two-stop gradient. The rule is scoped to the given unique class so tiles do
// not bleed into one another. An empty scene yields no rule.
func sceneGradientCSS(class string, sc Scene) string {
	var stops []string
	for _, st := range sc.States {
		r, g, b := sceneStateColour(st)
		stops = append(stops, fmt.Sprintf("rgb(%d,%d,%d)", r, g, b))
	}
	if len(stops) == 0 {
		return ""
	}
	if len(stops) == 1 {
		stops = append(stops, stops[0])
	}
	// Two stacked layers: a top-to-bottom vignette (transparent → subtle black)
	// painted over the diagonal colour gradient, so flat tiles gain depth and
	// the bottom name label stays legible. The vignette is listed first because
	// earlier background-image layers paint on top.
	return fmt.Sprintf(
		"button.%s { background-image: linear-gradient(to bottom, transparent 55%%, alpha(#000, 0.28) 100%%), linear-gradient(135deg, %s); border-radius: 16px; }\n",
		class, strings.Join(stops, ", "),
	)
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
