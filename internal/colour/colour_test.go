package colour

import (
	"testing"

	"github.com/alex-bluetrain/notuya-go/pkg/dp"
)

func TestFromRGB(t *testing.T) {
	cases := []struct {
		r, g, b float64
		want    dp.HSV
	}{
		{1, 0, 0, dp.HSV{H: 0, S: 1000, V: 1000}},
		{0, 1, 0, dp.HSV{H: 120, S: 1000, V: 1000}},
		{0, 0, 1, dp.HSV{H: 240, S: 1000, V: 1000}},
		{1, 1, 1, dp.HSV{H: 0, S: 0, V: 1000}},
		{0, 0, 0, dp.HSV{}},
		{1, 0, 1, dp.HSV{H: 300, S: 1000, V: 1000}},
		// Below one 8-bit step: still a distinct, non-zero value.
		{0.002, 0, 0, dp.HSV{H: 0, S: 1000, V: 2}},
	}
	for _, c := range cases {
		if got := FromRGB(c.r, c.g, c.b); got != c.want {
			t.Errorf("FromRGB(%v, %v, %v) = %+v, want %+v", c.r, c.g, c.b, got, c.want)
		}
	}
}

func TestHSVKeepsFullRange(t *testing.T) {
	if got := HSV(0.5, 0.5, 0.001); got != (dp.HSV{H: 180, S: 500, V: 1}) {
		t.Errorf("HSV = %+v", got)
	}
	if got := HSV(1, 1, 1); got.H != 0 {
		t.Errorf("hue 1 should wrap to 0, got %d", got.H)
	}
}

func TestToRGB(t *testing.T) {
	cases := []struct {
		in   dp.HSV
		want RGB
	}{
		{dp.HSV{H: 0, S: 0, V: 1000}, RGB{255, 255, 255}},
		{dp.HSV{H: 0, S: 1000, V: 1000}, RGB{255, 0, 0}},
		{dp.HSV{H: 120, S: 1000, V: 1000}, RGB{0, 255, 0}},
		{dp.HSV{H: 240, S: 1000, V: 1000}, RGB{0, 0, 255}},
		{dp.HSV{H: 360, S: 1000, V: 1000}, RGB{255, 0, 0}},
		{dp.HSV{H: 30, S: 1000, V: 500}, RGB{128, 64, 0}},
	}
	for _, c := range cases {
		if got := ToRGB(c.in); got != c.want {
			t.Errorf("ToRGB(%+v) = %+v, want %+v", c.in, got, c.want)
		}
	}
}
