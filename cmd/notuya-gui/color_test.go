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

func TestHexRoundTrip(t *testing.T) {
	for _, hex := range []string{"ffffff", "ff0000", "00ff00", "0000ff", "336699"} {
		h, s, v := hexToHSV(hex)
		r, g, b := hsvToRGBInt(h, s, v)
		got := rgbToHex(r, g, b)
		if got != hex {
			t.Errorf("round trip %s -> %s", hex, got)
		}
	}
}

func TestRGBToHSVGrey(t *testing.T) {
	h, s, v := rgbToHSV(0.5, 0.5, 0.5)
	if h != 0 || s != 0 {
		t.Errorf("grey should have h=0 s=0, got h=%v s=%v", h, s)
	}
	if v != 0.5 {
		t.Errorf("grey value = %v want 0.5", v)
	}
}
