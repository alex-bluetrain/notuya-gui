package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alex-bluetrain/notuya-go/pkg/dp"
	"github.com/alex-bluetrain/notuya-go/pkg/session"
)

// linkSession is a goroutine-safe session for the link keeper: its status
// reports on=true, and closing done simulates the bulb dropping the link.
type linkSession struct {
	done      chan struct{}
	closeOnce sync.Once
}

func newLinkSession() *linkSession { return &linkSession{done: make(chan struct{})} }

func (s *linkSession) Query(context.Context) ([]byte, error) {
	select {
	case <-s.done:
		return nil, errors.New("closed")
	default:
	}
	return []byte(`{"dps":{"20":true,"21":"white","22":500}}`), nil
}
func (s *linkSession) Control(context.Context, []byte, bool) error { return nil }
func (s *linkSession) Refresh(context.Context, []int) error        { return nil }
func (s *linkSession) Heartbeat(context.Context, bool) error       { return nil }
func (s *linkSession) Pushes() <-chan session.Push                 { return nil }
func (s *linkSession) Done() <-chan struct{}                       { return s.done }
func (s *linkSession) Err() error                                  { return nil }
func (s *linkSession) drop()                                       { s.closeOnce.Do(func() { close(s.done) }) }
func (s *linkSession) Close() error                                { s.drop(); return nil }

func TestKeeperRedialsAndRepublishesAfterLinkLoss(t *testing.T) {
	c := newControl(Device{DeviceID: "dev1", IPAddress: "127.0.0.1", LocalKey: "0123456789abcdef"})
	var mu sync.Mutex
	var sessions []*linkSession
	dials := 0
	c.dial = func(context.Context, string, []byte) (session.Session, error) {
		mu.Lock()
		defer mu.Unlock()
		dials++
		if dials == 1 {
			return nil, errors.New("bulb is off at the wall")
		}
		s := newLinkSession()
		sessions = append(sessions, s)
		return s, nil
	}
	got := make(chan deviceStatus, 4)
	c.Subscribe(func(st deviceStatus) { got <- st })
	c.Start()
	defer c.Close()

	wait := func(what string) deviceStatus {
		t.Helper()
		select {
		case st := <-got:
			return st
		case <-time.After(5 * time.Second):
			t.Fatalf("no status published %s", what)
			return deviceStatus{}
		}
	}

	// First dial fails; the keeper backs off and the second dial succeeds.
	if st := wait("after the first successful dial"); !st.On || st.BrightPct != 50 {
		t.Fatalf("snapshot = %+v, want on at 50%%", st)
	}

	// The bulb drops the link (power cut): the keeper redials and re-reads.
	mu.Lock()
	sessions[0].drop()
	mu.Unlock()
	wait("after the link dropped")

	mu.Lock()
	defer mu.Unlock()
	if dials != 3 {
		t.Fatalf("dials = %d, want 3 (fail, connect, reconnect)", dials)
	}
}

func TestKeeperIdlesWhileStreamingAndRereadsAfter(t *testing.T) {
	c := liveControl(&liveSession{})
	got := make(chan deviceStatus, 4)
	c.Subscribe(func(st deviceStatus) { got <- st })

	owner := new(int)
	if err := c.BeginLive(owner, dp.RGB{}, dp.ChangeJump); err != nil {
		t.Fatal(err)
	}
	c.Start()
	defer c.Close()
	select {
	case <-got:
		t.Fatal("keeper must not query a bulb a live stream owns")
	case <-time.After(200 * time.Millisecond):
	}
	c.EndLive(owner)
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("no status re-read after the stream ended")
	}
}

// Settings' Test re-keys a device while its keeper runs and the GTK thread
// reads the device's name for banners and its ID for Screen Sync's lockout.
func TestReconfigureWhileKeeperRunsIsRaceFree(t *testing.T) {
	c := newControl(Device{DeviceID: "dev1", Name: "Desk"})
	fail := true
	var mu sync.Mutex
	c.dial = func(context.Context, string, []byte) (session.Session, error) {
		mu.Lock()
		defer mu.Unlock()
		fail = !fail // alternate so the keeper logs "unreachable"/"reconnected"
		if fail {
			return nil, errors.New("unreachable")
		}
		s := newLinkSession()
		s.drop()
		return s, nil
	}
	c.Start()
	defer c.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			_ = deviceName(c.device())
			_ = c.device().DeviceID
		}
	}()
	for i := range 50 {
		c.reconfigure(Device{DeviceID: "dev1", Name: fmt.Sprintf("Desk %d", i)})
		time.Sleep(time.Millisecond)
	}
	<-done
}
