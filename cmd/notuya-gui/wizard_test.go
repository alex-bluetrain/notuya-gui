package main

import (
	"testing"

	"github.com/alex-bluetrain/notuya-go/pkg/discovery"
)

func TestWizardDeviceName(t *testing.T) {
	cases := []struct {
		i    int
		want string
	}{
		{0, "Light 1"},
		{1, "Light 2"},
		{9, "Light 10"},
	}
	for _, c := range cases {
		if got := wizardDeviceName(c.i); got != c.want {
			t.Errorf("wizardDeviceName(%d) = %q want %q", c.i, got, c.want)
		}
	}
}

func TestKeyComplete(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"", false},
		{"short", false},
		{"0123456789abcde", false},   // 15
		{"0123456789abcdef", true},   // 16
		{"0123456789abcdefg", false}, // 17
	}
	for _, c := range cases {
		if got := keyComplete(c.key); got != c.want {
			t.Errorf("keyComplete(%q) = %v want %v (len %d)", c.key, got, c.want, len(c.key))
		}
	}
}

func TestFoundLightsTitle(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "Found 0 lights"},
		{1, "Found 1 light"},
		{2, "Found 2 lights"},
	}
	for _, c := range cases {
		if got := foundLightsTitle(c.n); got != c.want {
			t.Errorf("foundLightsTitle(%d) = %q want %q", c.n, got, c.want)
		}
	}
}

func TestAllTested(t *testing.T) {
	cases := []struct {
		name  string
		flags []bool
		want  bool
	}{
		{"no rows", nil, false},
		{"single untested", []bool{false}, false},
		{"single tested", []bool{true}, true},
		{"one untested among tested", []bool{true, false, true}, false},
		{"all tested", []bool{true, true}, true},
	}
	for _, c := range cases {
		rows := make([]*wizardRow, len(c.flags))
		for i, f := range c.flags {
			rows[i] = &wizardRow{tested: f}
		}
		if got := allTested(rows); got != c.want {
			t.Errorf("%s: allTested = %v want %v", c.name, got, c.want)
		}
	}
}

func TestDeviceFrom(t *testing.T) {
	d := discovery.Device{ID: "ebfake1111111111111111", IP: "192.0.2.10"}
	got := deviceFrom(d, "Kitchen", "fake-local-key16")
	want := Device{
		DeviceID:  "ebfake1111111111111111",
		IPAddress: "192.0.2.10",
		LocalKey:  "fake-local-key16",
		Name:      "Kitchen",
	}
	if got != want {
		t.Errorf("deviceFrom = %+v want %+v", got, want)
	}
}
