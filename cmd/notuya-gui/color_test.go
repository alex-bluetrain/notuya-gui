package main

import "testing"

func TestHSVToRGBInt(t *testing.T) {
	cases := []struct {
		h, s, v float64
		r, g, b uint8
	}{
		{0, 0, 1, 255, 255, 255},     // white
		{0, 1, 1, 255, 0, 0},         // red
		{1.0 / 3.0, 1, 1, 0, 255, 0}, // green
		{2.0 / 3.0, 1, 1, 0, 0, 255}, // blue
		{0, 0, 0, 0, 0, 0},           // black
	}
	for _, c := range cases {
		r, g, b := hsvToRGBInt(c.h, c.s, c.v)
		if r != c.r || g != c.g || b != c.b {
			t.Errorf("hsvToRGBInt(%v,%v,%v) = %d,%d,%d want %d,%d,%d",
				c.h, c.s, c.v, r, g, b, c.r, c.g, c.b)
		}
	}
}
