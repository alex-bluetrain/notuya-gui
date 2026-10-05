package screensync

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alex-bluetrain/notuya-go/pkg/dp"

	"github.com/alex-bluetrain/notuya-gui/internal/colour"
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

// fakeLight records what the engine sends.
type fakeLight struct {
	mu      sync.Mutex
	last    dp.HSV
	updates int
}

func (f *fakeLight) Live(c dp.HSV, _ dp.ChangeMode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = c
	f.updates++
}

func (f *fakeLight) snapshot() (last dp.HSV, updates int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last, f.updates
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

func near(c dp.HSV, b colour.RGB) bool {
	a := colour.ToRGB(c)
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
	var got []dp.HSV
	startEngine(t, Options{
		Sources:    []Source{halves(0xff0000, 0x0000ff)},
		Brightness: 1,
		Regions: []Region{
			{Rect: Rect{0.05, 0.1, 0.4, 0.8}, Lights: []LightSink{left}},
			{Rect: Rect{0.55, 0.1, 0.4, 0.8}, Lights: []LightSink{right}},
		},
		OnFrame: func(c []dp.HSV) { mu.Lock(); got = c; mu.Unlock() },
	})
	red, blue := colour.RGB{R: 255}, colour.RGB{B: 255}
	eventually(t, "region colours", func() bool {
		l, _ := left.snapshot()
		r, _ := right.snapshot()
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
	want := colour.RGB{R: 191, B: 64}
	eventually(t, "weighted colour", func() bool { c, _ := l.snapshot(); return near(c, want) })
}

func TestBrightnessScalesAndClamps(t *testing.T) {
	l := &fakeLight{}
	e := startEngine(t, Options{
		Sources:    []Source{solid(0x804020, Rect{0, 0, 1, 1})},
		Brightness: 0.5,
		Regions:    []Region{{Rect: Rect{0, 0, 1, 1}, Lights: []LightSink{l}}},
	})
	eventually(t, "half brightness", func() bool { c, _ := l.snapshot(); return near(c, colour.RGB{R: 64, G: 32, B: 16}) })
	e.SetBrightness(4)
	eventually(t, "clamped boost", func() bool { c, _ := l.snapshot(); return near(c, colour.RGB{R: 255, G: 255, B: 128}) })
}

func TestUnboundLightStopsGettingFrames(t *testing.T) {
	a, b := &fakeLight{}, &fakeLight{}
	e := startEngine(t, Options{
		Sources:    []Source{solid(0x00ff00, Rect{0, 0, 1, 1})},
		Brightness: 1,
		Regions:    []Region{{Rect: Rect{0, 0, 1, 1}, Lights: []LightSink{a, b}}},
	})
	eventually(t, "both fed", func() bool { _, na := a.snapshot(); _, nb := b.snapshot(); return na > 0 && nb > 0 })
	e.SetRegions([]Region{{Rect: Rect{0, 0, 1, 1}, Lights: []LightSink{a}}})
	time.Sleep(100 * time.Millisecond) // let an in-flight frame land
	_, nb := b.snapshot()
	_, na := a.snapshot()
	eventually(t, "a still fed", func() bool { _, n := a.snapshot(); return n > na })
	if _, n := b.snapshot(); n != nb {
		t.Fatalf("unbound light got %d more frames", n-nb)
	}
}

func TestStopLeavesLastColour(t *testing.T) {
	l := &fakeLight{}
	e, err := Start(Options{
		Sources:    []Source{solid(0x112233, Rect{0, 0, 1, 1})},
		Brightness: 1,
		Regions:    []Region{{Rect: Rect{0, 0, 1, 1}, Lights: []LightSink{l}}},
	})
	if err != nil {
		t.Skipf("GStreamer GL unavailable: %v", err)
	}
	want := colour.RGB{R: 0x11, G: 0x22, B: 0x33}
	eventually(t, "colour", func() bool { c, _ := l.snapshot(); return near(c, want) })
	e.Stop()
	_, n := l.snapshot()
	time.Sleep(100 * time.Millisecond)
	c, n2 := l.snapshot()
	if n2 != n {
		t.Fatalf("%d frames after Stop", n2-n)
	}
	if !near(c, want) {
		t.Fatalf("last colour %v, want %v", c, want)
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
	if _, n := l.snapshot(); n != 0 {
		t.Fatalf("light touched %d times", n)
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
