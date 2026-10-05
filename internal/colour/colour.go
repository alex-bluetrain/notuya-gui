// Package colour converts between the bulb's HSV model and the 8-bit RGB
// the screen uses. Bulbs are only ever driven with dp.HSV; RGB exists here
// for painting widgets and for reading captured pixels.
package colour

import (
	"math"

	"github.com/alex-bluetrain/notuya-go/pkg/dp"
)

// RGB is an 8-bit display colour. It never goes to a bulb.
type RGB struct{ R, G, B uint8 }

// HSV builds a bulb colour from hue, saturation and value, each 0–1,
// rounded to the bulb's whole degrees and per-mille steps.
func HSV(h, s, v float64) dp.HSV {
	return dp.HSV{
		H: int(math.Round(math.Mod(h, 1) * 360)),
		S: int(math.Round(clamp01(s) * 1000)),
		V: int(math.Round(clamp01(v) * 1000)),
	}
}

// FromRGB converts a colour with channels 0–1 to the bulb's HSV, keeping
// the full 0–1000 resolution rather than going through 8-bit RGB.
func FromRGB(r, g, b float64) dp.HSV {
	r, g, b = clamp01(r), clamp01(g), clamp01(b)
	maxc := math.Max(r, math.Max(g, b))
	minc := math.Min(r, math.Min(g, b))
	if maxc == minc {
		return HSV(0, 0, maxc)
	}
	d := maxc - minc
	var h float64
	switch maxc {
	case r:
		h = (g - b) / d
	case g:
		h = 2 + (b-r)/d
	default:
		h = 4 + (r-g)/d
	}
	h = math.Mod(h/6+1, 1)
	return HSV(h, d/maxc, maxc)
}

// ToRGB converts a bulb colour to 8-bit RGB for display.
func ToRGB(c dp.HSV) RGB {
	h := math.Mod(float64(c.H)/360, 1)
	s, v := float64(c.S)/1000, float64(c.V)/1000
	i := math.Floor(h * 6)
	f := h*6 - i
	p, q, t := v*(1-s), v*(1-s*f), v*(1-s*(1-f))
	var r, g, b float64
	switch int(i) % 6 {
	case 0:
		r, g, b = v, t, p
	case 1:
		r, g, b = q, v, p
	case 2:
		r, g, b = p, v, t
	case 3:
		r, g, b = p, q, v
	case 4:
		r, g, b = t, p, v
	default:
		r, g, b = v, p, q
	}
	to8 := func(x float64) uint8 { return uint8(math.Round(x * 255)) }
	return RGB{to8(r), to8(g), to8(b)}
}

func clamp01(x float64) float64 { return min(max(x, 0), 1) }
