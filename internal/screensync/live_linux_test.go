package screensync

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alex-bluetrain/notuya-go/pkg/dp"
)

// Live checks against the real desktop. They are skipped unless
// NOTUYA_LIVE is set, e.g.:
//
//	NOTUYA_LIVE=1 NOTUYA_LIVE_MONITORS=DP-3,HDMI-A-1 go test -run Live -v ./internal/screensync
//	NOTUYA_LIVE=1 NOTUYA_LIVE_WINDOW=chromium go test -run Live -v ./internal/screensync
//
// Restore tokens persist in $TMPDIR/notuya-live-tokens.json, so a second
// run should start without the picker.

func liveTarget(t *testing.T) Target {
	if os.Getenv("NOTUYA_LIVE") == "" {
		t.Skip("set NOTUYA_LIVE=1 to run against the desktop")
	}
	if w := os.Getenv("NOTUYA_LIVE_WINDOW"); w != "" {
		return Target{Kind: TargetWindow, WindowClass: w}
	}
	m := os.Getenv("NOTUYA_LIVE_MONITORS")
	if m == "" {
		t.Skip("set NOTUYA_LIVE_MONITORS or NOTUYA_LIVE_WINDOW")
	}
	return Target{Kind: TargetMonitors, Monitors: strings.Split(m, ",")}
}

func TestLiveTracker(t *testing.T) {
	tg := liveTarget(t)
	tr, err := NewTracker()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tr.Watch(ctx, tg, 500*time.Millisecond, func(p Placement) { t.Logf("placement: %+v", p) })
}

func TestLiveCapture(t *testing.T) {
	tg := liveTarget(t)
	path := os.TempDir() + "/notuya-live-tokens.json"
	tokens := map[string]string{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &tokens)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	t0 := time.Now()
	c, err := OpenCapture(ctx, tg, CaptureOptions{Tokens: tokens, OnPick: func(k string) { t.Logf("picker: choose %s", k) }})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	t.Logf("capture open in %.1fs", time.Since(t0).Seconds())
	for k, v := range c.Tokens() {
		tokens[k] = v
	}
	b, _ := json.Marshal(tokens)
	_ = os.WriteFile(path, b, 0o600)
	for _, s := range c.Sources() {
		t.Logf("source canvas=%+v", s.Canvas)
	}

	var frames atomic.Int64
	l := &fakeLight{}
	e, err := Start(Options{
		Sources:    c.Sources(),
		Regions:    []Region{{Rect: Rect{0, 0, 1, 1}, Lights: []LightSink{l}}},
		Brightness: 1,
		OnFrame:    func([]dp.HSV) { frames.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Second)
	e.Stop()
	last, _ := l.snapshot()
	t.Logf("%.1f frames/s, last colour %v", float64(frames.Load())/5, last)
	if frames.Load() == 0 {
		t.Error("no frames (static screen? move something and retry)")
	}
}
