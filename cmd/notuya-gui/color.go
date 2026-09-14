package main

import (
	"fmt"
	"math"
)

// hsvToRGB is a direct port of Python's colorsys.hsv_to_rgb: h, s, v in
// [0,1] to r, g, b in [0,1]. Used both to fill the wheel bitmap and to turn
// the current selection into a colour.
func hsvToRGB(h, s, v float64) (r, g, b float64) {
	if s == 0.0 {
		return v, v, v
	}
	i := int(math.Floor(h * 6.0))
	f := h*6.0 - float64(i)
	p := v * (1.0 - s)
	q := v * (1.0 - s*f)
	t := v * (1.0 - s*(1.0-f))
	switch i % 6 {
	case 0:
		return v, t, p
	case 1:
		return q, v, p
	case 2:
		return p, v, t
	case 3:
		return p, q, v
	case 4:
		return t, p, v
	default: // 5
		return v, p, q
	}
}

// rgbToHSV is a direct port of Python's colorsys.rgb_to_hsv: r, g, b in
// [0,1] to h, s, v in [0,1].
func rgbToHSV(r, g, b float64) (h, s, v float64) {
	maxc := math.Max(r, math.Max(g, b))
	minc := math.Min(r, math.Min(g, b))
	v = maxc
	if minc == maxc {
		return 0.0, 0.0, v
	}
	rangec := maxc - minc
	s = rangec / maxc
	rc := (maxc - r) / rangec
	gc := (maxc - g) / rangec
	bc := (maxc - b) / rangec
	switch {
	case r == maxc:
		h = bc - gc
	case g == maxc:
		h = 2.0 + rc - bc
	default:
		h = 4.0 + gc - rc
	}
	h = math.Mod(h/6.0, 1.0)
	if h < 0 {
		h += 1.0
	}
	return h, s, v
}

// hsvToRGBInt converts an HSV selection to 8-bit RGB, rounding like
// picker.py's _hsv_to_rgb_int (round, not truncate) so the swatch and the
// streamed colour match.
func hsvToRGBInt(h, s, v float64) (r, g, b uint8) {
	rf, gf, bf := hsvToRGB(h, s, v)
	return uint8(math.Round(rf * 255)), uint8(math.Round(gf * 255)), uint8(math.Round(bf * 255))
}

// hexToHSV parses a 6-digit RRGGBB hex string into HSV, mirroring
// picker.py's hex_to_hsv.
func hexToHSV(hexColor string) (h, s, v float64) {
	var ri, gi, bi int
	fmt.Sscanf(hexColor, "%02x%02x%02x", &ri, &gi, &bi)
	return rgbToHSV(float64(ri)/255.0, float64(gi)/255.0, float64(bi)/255.0)
}

// rgbToHex formats 8-bit RGB as a lowercase RRGGBB hex string (no '#').
func rgbToHex(r, g, b uint8) string {
	return fmt.Sprintf("%02x%02x%02x", r, g, b)
}
