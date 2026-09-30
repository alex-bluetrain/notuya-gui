package main

import "math"

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

// hsvToRGBInt converts an HSV selection to 8-bit RGB, rounding like
// picker.py's _hsv_to_rgb_int (round, not truncate) so the swatch and the
// streamed colour match.
func hsvToRGBInt(h, s, v float64) (r, g, b uint8) {
	rf, gf, bf := hsvToRGB(h, s, v)
	return uint8(math.Round(rf * 255)), uint8(math.Round(gf * 255)), uint8(math.Round(bf * 255))
}
