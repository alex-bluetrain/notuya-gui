package screensync

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"
)

var testMons = []Monitor{
	{Name: "HDMI-A-1", X: 0, Y: 0, W: 1920, H: 1080},
	{Name: "DP-3", X: 1920, Y: 0, W: 1920, H: 1080},
	{Name: "DP-2", X: 3840, Y: 0, W: 1920, H: 1080},
}

func TestCanvasOfLaysMonitorsOutInSelectionOrder(t *testing.T) {
	got, err := canvasOf([]string{"DP-2", "HDMI-A-1"}, testMons)
	if err != nil {
		t.Fatal(err)
	}
	// Bounding box spans all three (5760 wide); DP-3 in the gap is not captured.
	want := []Rect{{2.0 / 3, 0, 1.0 / 3, 1}, {0, 0, 1.0 / 3, 1}}
	for i := range want {
		if !rectEq(got[i], want[i]) {
			t.Errorf("canvas[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if _, err := canvasOf([]string{"DP-9"}, testMons); err == nil {
		t.Error("unknown monitor accepted")
	}
}

func TestMatchWindowPrefersTitleAndRecentFocus(t *testing.T) {
	ws := []Window{
		{Class: "steam_app_1", Title: "Launcher", recent: 0},
		{Class: "steam_app_1", Title: "The Game", recent: 3},
		{Class: "steam_app_1", Title: "The Game — menu", recent: 1},
		{Class: "foot", Title: "The Game", recent: 0},
	}
	w, ok := MatchWindow(Target{Kind: TargetWindow, WindowClass: "steam_app_1", TitleMatch: "the game"}, ws)
	if !ok || w.Title != "The Game — menu" {
		t.Errorf("got %q %v", w.Title, ok)
	}
	if _, ok := MatchWindow(Target{Kind: TargetWindow, WindowClass: "nope"}, ws); ok {
		t.Error("matched a missing class")
	}
}

func TestPlaceWindowFollowsFullscreenAndGone(t *testing.T) {
	tg := Target{Kind: TargetWindow, WindowClass: "game"}
	ws := []Window{{Class: "game", X: 2000, Y: 50, W: 800, H: 600, Monitor: "DP-3"}}
	p := place(tg, testMons, ws)
	if p.Gone || p.Canvas != (Monitor{X: 2000, Y: 50, W: 800, H: 600}) || len(p.Monitors) != 1 || p.Monitors[0].Name != "DP-3" {
		t.Errorf("windowed: %+v", p)
	}
	ws[0].Fullscreen = true
	if p := place(tg, testMons, ws); p.Canvas != (Monitor{X: 1920, W: 1920, H: 1080}) {
		t.Errorf("fullscreen: %+v", p)
	}
	ws[0] = Window{Class: "game", X: 1800, Y: 0, W: 400, H: 300, Monitor: "HDMI-A-1"}
	if p := place(tg, testMons, ws); len(p.Monitors) != 2 {
		t.Errorf("spanning window should touch 2 monitors: %+v", p.Monitors)
	}
	if p := place(tg, testMons, nil); !p.Gone {
		t.Error("closed window not reported gone")
	}
}

func TestPlaceMonitorsAndUnplug(t *testing.T) {
	p := place(Target{Kind: TargetMonitors, Monitors: []string{"HDMI-A-1", "DP-3"}}, testMons, nil)
	if p.Gone || p.Canvas != (Monitor{W: 3840, H: 1080}) || len(p.Monitors) != 2 {
		t.Errorf("%+v", p)
	}
	if !place(Target{Kind: TargetMonitors, Monitors: []string{"DP-9"}}, testMons, nil).Gone {
		t.Error("unplugged monitor not gone")
	}
}

func TestParseNodeFormats(t *testing.T) {
	dump := []byte(`[{"info":{"params":{"EnumFormat":[
		{"format":"BGRA","modifier":{"default":216172782119018512,"alt1":216172782119018512,"alt2":0}},
		{"format":"BGRx","modifier":[0]},
		{"format":"NV12","modifier":0},
		{"format":"BGRA"}
	]}}}]`)
	got, err := parseNodeFormats(dump)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"AR24:0x03000000004fe010", "AR24:0x0000000000000000", "XR24:0x0000000000000000"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

type fixedTracker struct{ ws []Window }

func (fixedTracker) Monitors() ([]Monitor, error)                                  { return nil, nil }
func (f fixedTracker) Windows() ([]Window, error)                                  { return f.ws, nil }
func (fixedTracker) Watch(context.Context, Target, time.Duration, func(Placement)) {}

func TestIdentify(t *testing.T) {
	tr := fixedTracker{ws: []Window{
		{Class: "kitty", Title: "a", W: 800, H: 800, recent: 0},
		{Class: "game", Title: "Game", W: 1920, H: 1080, recent: 2},
		{Class: "game2", Title: "Other", W: 1280, H: 720, recent: 1},
	}}
	cases := []struct {
		name  string
		sh    shared
		saved map[string]string
		want  Target
	}{
		{"monitor by mapping id", shared{mappingID: "DP-3"}, nil, Target{Kind: TargetMonitors, Monitors: []string{"DP-3"}}},
		{"monitor from saved output", shared{}, map[string]string{"output": "HDMI-A-1"}, Target{Kind: TargetMonitors, Monitors: []string{"HDMI-A-1"}}},
		{"window from saved class", shared{window: true}, map[string]string{"windowClass": "game"}, Target{Kind: TargetWindow, WindowClass: "game"}},
		{"window by shape, recent wins tie", shared{window: true, w: 1600, h: 900}, nil, Target{Kind: TargetWindow, WindowClass: "game2"}},
		{"window by shape", shared{window: true, w: 500, h: 500}, nil, Target{Kind: TargetWindow, WindowClass: "kitty"}},
	}
	for _, c := range cases {
		got, err := identify(c.sh, c.saved, tr)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v, %v; want %+v", c.name, got, err, c.want)
		}
	}
	if _, err := identify(shared{}, nil, tr); err == nil {
		t.Error("unnamed monitor: want error")
	}
}
