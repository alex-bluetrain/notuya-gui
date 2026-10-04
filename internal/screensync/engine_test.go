package screensync

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alex-bluetrain/notuya-go/pkg/dp"
)

// noUdmabuf stops glupload picking its udmabuf path for system-memory
// test frames: on this NVIDIA driver that path uploads black. Real
// sources arrive as DMA-BUF and never take it.
const noUdmabuf = " ! identity drop-allocation=true"

// solid is a live test source of one colour (0xRRGGBB).
func solid(rgb uint32, canvas Rect) Source {
	return Source{
		Fragment: fmt.Sprintf("videotestsrc is-live=true pattern=solid-color foreground-color=0xff%06x"+
			" ! video/x-raw,format=RGBA,width=320,height=240,framerate=30/1"+noUdmabuf, rgb),
		Canvas: canvas,
	}
}

// halves is a source whose left half is one colour and right half another.
func halves(left, right uint32) Source {
	return Source{
		Fragment: fmt.Sprintf(
			"videotestsrc is-live=true pattern=solid-color foreground-color=0xff%06x ! video/x-raw,format=RGBA,width=160,height=240,framerate=30/1 ! m.sink_0 "+
				"videotestsrc is-live=true pattern=solid-color foreground-color=0xff%06x ! video/x-raw,format=RGBA,width=160,height=240,framerate=30/1 ! m.sink_1 "+
				"compositor name=m sink_1::xpos=160 ! video/x-raw,format=RGBA,width=320,height=240,framerate=30/1"+noUdmabuf, left, right),
		Canvas: Rect{0, 0, 1, 1},
	}
}

// fakeLight records what the engine sends and can drop its stream on demand.
type fakeLight struct {
	mu        sync.Mutex
	seed      dp.RGB
	failBegin int // BeginLive calls left that fail
	begins    int
	ends      int
	owner     any
	last      dp.RGB
	updates   int
	dead      chan struct{}
}

func (f *fakeLight) Seed(context.Context) dp.RGB { return f.seed }

func (f *fakeLight) BeginLive(owner any, seed dp.RGB, _ dp.ChangeMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.begins++
	if f.failBegin > 0 {
		f.failBegin--
		return errors.New("refused")
	}
	if f.dead != nil {
		return nil // same owner re-begin is a no-op
	}
	f.owner, f.last, f.dead = owner, seed, make(chan struct{})
	return nil
}

func (f *fakeLight) UpdateLive(owner any, rgb dp.RGB) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dead != nil && owner == f.owner {
		f.last = rgb
		f.updates++
	}
}

func (f *fakeLight) EndLive(owner any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dead != nil && owner == f.owner {
		f.dead, f.owner = nil, nil
		f.ends++
	}
}

func (f *fakeLight) Streaming() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dead != nil
}

func (f *fakeLight) Dead() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dead == nil {
		return nil
	}
	return f.dead
}

// drop simulates the bulb closing its live stream.
func (f *fakeLight) drop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dead != nil {
		close(f.dead)
		f.dead = nil
	}
}

func (f *fakeLight) snapshot() (last dp.RGB, begins, updates int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last, f.begins, f.updates
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func near(a, b dp.RGB) bool {
	d := func(x, y uint8) bool { return int(x)-int(y) <= 2 && int(y)-int(x) <= 2 }
	return d(a.R, b.R) && d(a.G, b.G) && d(a.B, b.B)
}

func startEngine(t *testing.T, opts Options) *Engine {
	t.Helper()
	e, err := Start(opts)
	if err != nil {
		t.Skipf("GStreamer GL unavailable: %v", err)
	}
	t.Cleanup(e.Stop)
	return e
}

func TestRegionsGetTheirOwnColour(t *testing.T) {
	left, right := &fakeLight{}, &fakeLight{}
	var mu sync.Mutex
	var got []dp.RGB
	startEngine(t, Options{
		Sources:    []Source{halves(0xff0000, 0x0000ff)},
		Brightness: 1,
		Regions: []Region{
			{Rect: Rect{0.05, 0.1, 0.4, 0.8}, Lights: []LightSink{left}},
			{Rect: Rect{0.55, 0.1, 0.4, 0.8}, Lights: []LightSink{right}},
		},
		OnFrame: func(c []dp.RGB) { mu.Lock(); got = c; mu.Unlock() },
	})
	red, blue := dp.RGB{R: 255}, dp.RGB{B: 255}
	eventually(t, "region colours", func() bool {
		l, _, _ := left.snapshot()
		r, _, _ := right.snapshot()
		return near(l, red) && near(r, blue)
	})
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || !near(got[0], red) || !near(got[1], blue) {
		t.Fatalf("OnFrame got %v", got)
	}
}

func TestRegionSpanningSourcesIsAreaWeighted(t *testing.T) {
	l := &fakeLight{}
	startEngine(t, Options{
		Sources: []Source{
			solid(0xff0000, Rect{0, 0, 0.5, 1}),
			solid(0x0000ff, Rect{0.5, 0, 0.5, 1}),
		},
		Brightness: 1,
		// 3/4 of the region on the red source, 1/4 on the blue one.
		Regions: []Region{{Rect: Rect{0.2, 0, 0.4, 1}, Lights: []LightSink{l}}},
	})
	want := dp.RGB{R: 191, B: 64}
	eventually(t, "weighted colour", func() bool { c, _, _ := l.snapshot(); return near(c, want) })
}

func TestBrightnessScalesAndClamps(t *testing.T) {
	l := &fakeLight{}
	e := startEngine(t, Options{
		Sources:    []Source{solid(0x804020, Rect{0, 0, 1, 1})},
		Brightness: 0.5,
		Regions:    []Region{{Rect: Rect{0, 0, 1, 1}, Lights: []LightSink{l}}},
	})
	eventually(t, "half brightness", func() bool { c, _, _ := l.snapshot(); return near(c, dp.RGB{R: 64, G: 32, B: 16}) })
	e.SetBrightness(4)
	eventually(t, "clamped boost", func() bool { c, _, _ := l.snapshot(); return near(c, dp.RGB{R: 255, G: 255, B: 128}) })
}

func TestUnboundLightIsReleased(t *testing.T) {
	a, b := &fakeLight{}, &fakeLight{}
	e := startEngine(t, Options{
		Sources:    []Source{solid(0x00ff00, Rect{0, 0, 1, 1})},
		Brightness: 1,
		Regions:    []Region{{Rect: Rect{0, 0, 1, 1}, Lights: []LightSink{a, b}}},
	})
	eventually(t, "both streaming", func() bool { return a.Streaming() && b.Streaming() })
	e.SetRegions([]Region{{Rect: Rect{0, 0, 1, 1}, Lights: []LightSink{a}}})
	eventually(t, "b released", func() bool { return !b.Streaming() })
	if !a.Streaming() {
		t.Fatal("a stopped streaming")
	}
}

func TestStopReleasesLightsKeepingLastColour(t *testing.T) {
	l := &fakeLight{}
	e, err := Start(Options{
		Sources:    []Source{solid(0x112233, Rect{0, 0, 1, 1})},
		Brightness: 1,
		Regions:    []Region{{Rect: Rect{0, 0, 1, 1}, Lights: []LightSink{l}}},
	})
	if err != nil {
		t.Skipf("GStreamer GL unavailable: %v", err)
	}
	want := dp.RGB{R: 0x11, G: 0x22, B: 0x33}
	eventually(t, "colour", func() bool { c, _, _ := l.snapshot(); return near(c, want) })
	e.Stop()
	if l.Streaming() {
		t.Fatal("still streaming after Stop")
	}
	if c, _, _ := l.snapshot(); !near(c, want) {
		t.Fatalf("last colour %v, want %v", c, want)
	}
}

func TestDroppedLightReconnectsWithBackoff(t *testing.T) {
	l := &fakeLight{}
	var mu sync.Mutex
	var trouble []bool
	startEngine(t, Options{
		Sources:        []Source{solid(0xffffff, Rect{0, 0, 1, 1})},
		Brightness:     1,
		Regions:        []Region{{Rect: Rect{0, 0, 1, 1}, Lights: []LightSink{l}}},
		OnLightTrouble: func(_ LightSink, ok bool) { mu.Lock(); trouble = append(trouble, ok); mu.Unlock() },
		reconnect:      reconnect{20 * time.Millisecond, 80 * time.Millisecond, 100 * time.Millisecond},
	})
	eventually(t, "streaming", l.Streaming)

	l.mu.Lock()
	l.failBegin = 3
	l.mu.Unlock()
	l.drop()
	eventually(t, "reconnect", func() bool { _, b, _ := l.snapshot(); return b >= 5 && l.Streaming() })
	eventually(t, "recovery reported", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(trouble) == 2
	})
	mu.Lock()
	defer mu.Unlock()
	if trouble[0] || !trouble[1] {
		t.Fatalf("trouble events %v, want [false true]", trouble)
	}
}

func TestBadSourceFailsWithoutTouchingLights(t *testing.T) {
	l := &fakeLight{}
	_, err := Start(Options{
		Sources: []Source{{Fragment: "nosuchelement", Canvas: Rect{0, 0, 1, 1}}},
		Regions: []Region{{Rect: Rect{0, 0, 1, 1}, Lights: []LightSink{l}}},
	})
	if err == nil {
		t.Fatal("Start succeeded with a broken source")
	}
	if _, b, _ := l.snapshot(); b != 0 {
		t.Fatalf("light touched %d times", b)
	}
}

func TestSourceEndStopsSync(t *testing.T) {
	l := &fakeLight{}
	stopped := make(chan error, 1)
	src := solid(0x00ff00, Rect{0, 0, 1, 1})
	src.Fragment = "videotestsrc num-buffers=10 pattern=solid-color foreground-color=0xff00ff00 ! video/x-raw,format=RGBA,width=320,height=240,framerate=30/1" + noUdmabuf
	_, err := Start(Options{
		Sources:    []Source{src},
		Brightness: 1,
		Regions:    []Region{{Rect: Rect{0, 0, 1, 1}, Lights: []LightSink{l}}},
		OnStop:     func(err error) { stopped <- err },
	})
	if err != nil {
		t.Skipf("GStreamer GL unavailable: %v", err)
	}
	select {
	case err := <-stopped:
		if err == nil {
			t.Fatal("OnStop with nil error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sync did not stop when its source ended")
	}
	if l.Streaming() {
		t.Fatal("light still streaming after source ended")
	}
}

func TestRectGeometry(t *testing.T) {
	r := Rect{0.2, 0.1, 0.6, 0.5}
	if got := r.intersect(Rect{0.5, 0, 0.5, 1}); !rectEq(got, Rect{0.5, 0.1, 0.3, 0.5}) {
		t.Fatalf("intersect %v", got)
	}
	if got := r.intersect(Rect{0.9, 0, 0.1, 1}); got.area() != 0 {
		t.Fatalf("disjoint intersect %v", got)
	}
	if got := (Rect{0.5, 0.1, 0.3, 0.5}).within(Rect{0.5, 0, 0.5, 1}); !rectEq(got, Rect{0, 0.1, 0.6, 0.5}) {
		t.Fatalf("within %v", got)
	}
}

func rectEq(a, b Rect) bool {
	d := func(x, y float64) bool { return x-y < 1e-9 && y-x < 1e-9 }
	return d(a.X, b.X) && d(a.Y, b.Y) && d(a.W, b.W) && d(a.H, b.H)
}
