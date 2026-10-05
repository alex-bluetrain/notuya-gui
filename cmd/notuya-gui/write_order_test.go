package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/alex-bluetrain/notuya-go/pkg/dp"
	"github.com/alex-bluetrain/notuya-go/pkg/session"
)

// This file is a detection harness, not a unit test of one function: it
// drives a control the way the tabs do (Live from the wheel, slider and
// Screen Sync; Apply from scenes; Save on leaving a tab) against one fake
// bulb whose writes land after a random network delay, and checks the rules
// the write path must keep:
//
//	R1 the bulb ends showing the last value the user set
//	R2 writes land in the order they were made
//	R3 after a save, the bulb's saved colour (DP 24) is the last value set
//	R4 the bulb has at most one session open at a time
//
// Every value the user "sets" carries its intent index in V (V = 10*i), so
// the landing order and the final state can be checked against intent.

// fakeBulb is the device side: every session dialled by the control talks to
// the same one.
type fakeBulb struct {
	mu      sync.Mutex
	cur     dp.HSV
	saved   dp.HSV
	landed  []int // intent index of each colour write, in landing order
	open    int
	maxOpen int
}

func (b *fakeBulb) dial(context.Context, string, []byte) (session.Session, error) {
	time.Sleep(jitter())
	b.mu.Lock()
	b.open++
	b.maxOpen = max(b.maxOpen, b.open)
	b.mu.Unlock()
	return &bulbSession{b: b, done: make(chan struct{})}, nil
}

func jitter() time.Duration { return time.Duration(rand.IntN(3000)) * time.Microsecond }

type bulbSession struct {
	b      *fakeBulb
	once   sync.Once
	done   chan struct{}
	closed bool
	mu     sync.Mutex
}

func (s *bulbSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *bulbSession) Query(context.Context) ([]byte, error) {
	if s.isClosed() {
		return nil, errors.New("use of closed session")
	}
	s.b.mu.Lock()
	c := s.b.cur
	s.b.mu.Unlock()
	return fmt.Appendf(nil, `{"dps":{"20":true,"21":"colour","24":"%04x%04x%04x"}}`, c.H, c.S, c.V), nil
}

// Control lands the colour after a random delay, so two writes issued
// concurrently may land in either order — as on a real network.
func (s *bulbSession) Control(_ context.Context, body []byte, _ bool) error {
	if s.isClosed() {
		return errors.New("use of closed session")
	}
	var msg struct {
		Data struct {
			DPs map[string]any `json:"dps"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return err
	}
	var (
		c     dp.HSV
		got   bool
		saves bool
	)
	if v, ok := msg.Data.DPs["24"].(string); ok {
		h, err := dp.ParseHSV(v)
		if err != nil {
			return err
		}
		c, got, saves = h, true, true
	}
	if v, ok := msg.Data.DPs["28"].(string); ok {
		a, err := dp.ParseAdjust(v)
		if err != nil {
			return err
		}
		c, got = a.Colour, true
	}
	time.Sleep(jitter())
	if s.isClosed() {
		return errors.New("use of closed session")
	}
	if got {
		s.b.mu.Lock()
		s.b.cur = c
		if saves {
			s.b.saved = c
		}
		s.b.landed = append(s.b.landed, c.V/10)
		s.b.mu.Unlock()
	}
	return nil
}

func (s *bulbSession) Refresh(context.Context, []int) error  { return nil }
func (s *bulbSession) Heartbeat(context.Context, bool) error { return nil }
func (s *bulbSession) Pushes() <-chan session.Push           { return nil }
func (s *bulbSession) Done() <-chan struct{}                 { return s.done }
func (s *bulbSession) Err() error                            { return nil }

func (s *bulbSession) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.done)
		s.b.mu.Lock()
		s.b.open--
		s.b.mu.Unlock()
	})
	return nil
}

// ui stands in for one tab (or Screen Sync) driving the control exactly the
// way the GUI code does, while recording what the user set.
type ui struct {
	ctl  *control
	last int // intent index of the last value set
}

func intent(i int) dp.HSV { return dp.HSV{H: 120, S: 1000, V: 10 * i} }

func (u *ui) set() int {
	u.last++
	return u.last
}

// live is a wheel/slider move or a Screen Sync frame.
func (u *ui) live() {
	time.Sleep(jitter() / 10)
	u.ctl.Live(intent(u.set()), dp.ChangeJump)
}

// applyScene is scenes.go applyScene for one light.
func (u *ui) applyScene() {
	i := u.set()
	u.ctl.Apply(SceneState{DeviceID: "dev1", On: true, Mode: dp.ModeColour, Hue: 1.0 / 3, Sat: 1, Bright: float64(i)})
}

// save is leaving the Lights tab, or the scene editor's Save.
func (u *ui) save() { u.ctl.Save() }

type outcome struct{ r1, r2, r3, r4 bool }

// runScenario plays fn against a fresh control and fake bulb, closes the
// control the way the app does on exit, and reports which rules broke.
func runScenario(fn func(u *ui)) (outcome, string) {
	b := &fakeBulb{}
	c := newControl(Device{DeviceID: "dev1", Name: "Desk"})
	c.dial = b.dial
	u := &ui{ctl: c}

	fn(u)
	c.Close()

	b.mu.Lock()
	defer b.mu.Unlock()
	var o outcome
	o.r1 = b.cur.V/10 != u.last
	for i := 1; i < len(b.landed); i++ {
		if b.landed[i] < b.landed[i-1] {
			o.r2 = true
		}
	}
	o.r3 = b.saved.V/10 != u.last
	o.r4 = b.maxOpen > 1
	detail := fmt.Sprintf("last set=%d bulb=%d saved=%d landed=%v maxOpen=%d", u.last, b.cur.V/10, b.saved.V/10, b.landed, b.maxOpen)
	return o, detail
}

// writeScenarios are the UI gestures, written as the call sequences the tabs
// actually issue. Each ends with the app closing, which saves.
var writeScenarios = []struct {
	name string
	run  func(u *ui)
}{
	{"TwoQuickLiveColours", func(u *ui) {
		u.live()
		u.live()
	}},
	{"SliderScrubToZeroThenLeaveTab", func(u *ui) {
		for range 20 {
			u.live()
		}
		u.save()
	}},
	{"WheelDragThenSceneThenDrag", func(u *ui) {
		u.live()
		u.live()
		u.applyScene()
		u.live()
	}},
	{"ScreenSyncFramesThenScene", func(u *ui) {
		for range 30 {
			u.live()
		}
		u.applyScene()
	}},
	{"SceneDuringDragThenSave", func(u *ui) {
		u.live()
		u.applyScene()
		u.live()
		u.save()
		u.live()
	}},
}

func TestWriteOrderRules(t *testing.T) {
	const reps = 100
	for _, sc := range writeScenarios {
		t.Run(sc.name, func(t *testing.T) {
			var n [4]int
			var example [4]string
			for range reps {
				o, d := runScenario(sc.run)
				for i, bad := range []bool{o.r1, o.r2, o.r3, o.r4} {
					if bad {
						n[i]++
						if example[i] == "" {
							example[i] = d
						}
					}
				}
			}
			for i, cnt := range n {
				if cnt > 0 {
					t.Errorf("R%d broken in %d/%d runs, e.g. %s", i+1, cnt, reps, example[i])
				}
			}
		})
	}
}
