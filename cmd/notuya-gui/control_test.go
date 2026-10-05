package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/alex-bluetrain/notuya-go/pkg/dp"
	"github.com/alex-bluetrain/notuya-go/pkg/session"
)

// fakeSession is an in-memory session.Session: it records opens/closes and
// fails the requests listed in cmdErrs (by call index), answering every
// query with a canned status body.
type fakeSession struct {
	openErr error
	cmdErrs map[int]error
	opened  int
	closed  int
	calls   int
	done    chan struct{}
}

func (s *fakeSession) call(ctx context.Context) error {
	i := s.calls
	s.calls++
	if err := ctx.Err(); err != nil {
		return err
	}
	err := s.cmdErrs[i]
	if errors.Is(err, syscall.ECONNRESET) {
		// Like the real session: a socket error fails it for good.
		s.kill()
	}
	return err
}

func (s *fakeSession) kill() {
	if s.done == nil {
		s.done = make(chan struct{})
	}
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

func (s *fakeSession) Query(ctx context.Context) ([]byte, error) {
	if err := s.call(ctx); err != nil {
		return nil, err
	}
	return []byte(`{"dps":{"20":true,"21":"colour"}}`), nil
}

func (s *fakeSession) Control(ctx context.Context, _ []byte, _ bool) error { return s.call(ctx) }
func (s *fakeSession) Refresh(ctx context.Context, _ []int) error          { return s.call(ctx) }
func (s *fakeSession) Heartbeat(ctx context.Context, _ bool) error         { return s.call(ctx) }
func (s *fakeSession) Pushes() <-chan session.Push                         { return nil }
func (s *fakeSession) Done() <-chan struct{}                               { return s.done }
func (s *fakeSession) Err() error                                          { return nil }

func (s *fakeSession) Close() error {
	s.closed++
	return nil
}

var _ session.Session = (*fakeSession)(nil)

// fakeControl returns a control whose dial hands out the given sessions in
// order, plus a counter of dials.
func fakeControl(t *testing.T, sessions ...*fakeSession) (*control, *int) {
	t.Helper()
	c := newControl(Device{DeviceID: "dev1", IPAddress: "127.0.0.1", LocalKey: "0123456789abcdef", Name: "Desk"})
	dials := 0
	c.dial = func(context.Context, string, []byte) (session.Session, error) {
		if dials >= len(sessions) {
			t.Fatalf("unexpected dial #%d", dials+1)
		}
		s := sessions[dials]
		dials++
		s.opened++
		if s.openErr != nil {
			return nil, s.openErr
		}
		return s, nil
	}
	return c, &dials
}

func TestWithBulbReconnectsStaleSessionOnce(t *testing.T) {
	// s1 answers the first command, then the bulb has dropped it: the next
	// write sees a reset.
	s1 := &fakeSession{cmdErrs: map[int]error{1: syscall.ECONNRESET}}
	s2 := &fakeSession{}
	c, dials := fakeControl(t, s1, s2)
	ctx := context.Background()

	if err := c.SetPower(ctx, true); err != nil {
		t.Fatalf("first command: %v", err)
	}
	if err := c.SetPower(ctx, false); err != nil {
		t.Fatalf("command on stale session should reconnect and succeed, got %v", err)
	}
	if *dials != 2 || s1.closed != 1 || s2.opened != 1 || s2.calls != 1 {
		t.Fatalf("dials=%d s1.closed=%d s2.opened=%d s2.calls=%d; want 2,1,1,1", *dials, s1.closed, s2.opened, s2.calls)
	}
	if c.sess != s2 {
		t.Fatal("control should keep the fresh session")
	}
}

func TestWithBulbFreshSessionFailureIsNotRetriedButRecoversNextCommand(t *testing.T) {
	s1 := &fakeSession{cmdErrs: map[int]error{0: syscall.ECONNRESET}}
	s2 := &fakeSession{}
	c, dials := fakeControl(t, s1, s2)
	ctx := context.Background()

	err := c.SetPower(ctx, true)
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("want the command error back, got %v", err)
	}
	if *dials != 1 || s1.closed != 1 || c.sess != nil {
		t.Fatalf("dials=%d s1.closed=%d sess=%v; a fresh session must fail without retry and be dropped", *dials, s1.closed, c.sess)
	}
	// The next command reconnects on its own.
	if err := c.SetPower(ctx, true); err != nil {
		t.Fatalf("next command should reconnect: %v", err)
	}
	if *dials != 2 || c.sess != s2 {
		t.Fatalf("dials=%d sess=%v; want 2 and s2", *dials, c.sess)
	}
}

func TestWithBulbKeepsLiveSessionWhenCallerContextIsDone(t *testing.T) {
	s1 := &fakeSession{}
	c, dials := fakeControl(t, s1)
	if err := c.SetPower(context.Background(), true); err != nil {
		t.Fatalf("first command: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.SetPower(ctx, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if *dials != 1 || s1.closed != 0 || c.sess != s1 {
		t.Fatalf("dials=%d closed=%d sess=%v; a cancelled command must not drop a live session", *dials, s1.closed, c.sess)
	}
}

func TestWithBulbFailureOnLiveSessionDoesNotReconnect(t *testing.T) {
	rejected := errors.New("device rejected the value")
	s1 := &fakeSession{cmdErrs: map[int]error{1: rejected}}
	c, dials := fakeControl(t, s1)
	ctx := context.Background()

	if err := c.SetPower(ctx, true); err != nil {
		t.Fatalf("first command: %v", err)
	}
	if err := c.SetPower(ctx, false); !errors.Is(err, rejected) {
		t.Fatalf("want the command error back, got %v", err)
	}
	if err := c.SetPower(ctx, true); err != nil {
		t.Fatalf("next command: %v", err)
	}
	if *dials != 1 || s1.closed != 0 || s1.calls != 3 || c.sess != s1 {
		t.Fatalf("dials=%d closed=%d calls=%d; want 1,0,3 and the same session reused", *dials, s1.closed, s1.calls)
	}
}

func TestReconfigureDropsSessionOnlyWhenDeviceChanges(t *testing.T) {
	s1 := &fakeSession{}
	s2 := &fakeSession{}
	c, dials := fakeControl(t, s1, s2)
	if err := c.SetPower(context.Background(), true); err != nil {
		t.Fatalf("first command: %v", err)
	}
	same := c.device()
	c.reconfigure(same)
	if s1.closed != 0 || c.sess != s1 {
		t.Fatal("reconfigure with the same device must be a no-op")
	}
	changed := same
	changed.LocalKey = "fedcba9876543210"
	c.reconfigure(changed)
	if s1.closed != 1 || c.sess != nil || c.device() != changed {
		t.Fatalf("closed=%d sess=%v dev=%+v; want session dropped and device swapped", s1.closed, c.sess, c.device())
	}
	if err := c.SetPower(context.Background(), true); err != nil || *dials != 2 {
		t.Fatalf("next command should dial with the new credentials (err=%v dials=%d)", err, *dials)
	}
}

func TestRefreshParsesStatus(t *testing.T) {
	c, _ := fakeControl(t, &fakeSession{})
	st, err := c.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.On || st.Mode != dp.ModeColour {
		t.Fatalf("status = %+v; want On in colour mode", st)
	}
}

// recSession records every Control body and whether it waited for an ack.
// gate, when set, holds each Control until a value arrives, so a test can
// pile up writes behind one in flight.
type recSession struct {
	mu     sync.Mutex
	writes []recWrite
	gate   chan struct{}
}

type recWrite struct {
	dps  map[string]any
	wait bool
}

func (s *recSession) Query(context.Context) ([]byte, error) {
	return []byte(`{"dps":{"20":true,"21":"colour","24":"000003e803e8"}}`), nil
}

func (s *recSession) Control(ctx context.Context, body []byte, wait bool) error {
	if s.gate != nil {
		select {
		case <-s.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var m struct {
		Data struct {
			DPS map[string]any `json:"dps"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &m)
	s.mu.Lock()
	s.writes = append(s.writes, recWrite{m.Data.DPS, wait})
	s.mu.Unlock()
	return nil
}

func (s *recSession) Refresh(context.Context, []int) error  { return nil }
func (s *recSession) Heartbeat(context.Context, bool) error { return nil }
func (s *recSession) Pushes() <-chan session.Push           { return nil }
func (s *recSession) Done() <-chan struct{}                 { return nil }
func (s *recSession) Err() error                            { return nil }
func (s *recSession) Close() error                          { return nil }

func (s *recSession) snapshot() []recWrite {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recWrite(nil), s.writes...)
}

func recControl(s *recSession) *control {
	c := newControl(Device{DeviceID: "dev1", Name: "Desk"})
	c.dial = func(context.Context, string, []byte) (session.Session, error) { return s, nil }
	return c
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func hsvDP(c dp.HSV) string { h, _ := c.Hex(); return h }

func TestLiveSendsOnlyTheNewestColour(t *testing.T) {
	s := &recSession{gate: make(chan struct{})}
	c := recControl(s)
	defer c.Close()
	c.Live(dp.HSV{H: 1, S: 1000, V: 1000}, dp.ChangeJump) // held in flight by the gate
	for v := 10; v <= 500; v += 10 {
		c.Live(dp.HSV{H: 2, S: 1000, V: v}, dp.ChangeJump)
	}
	close(s.gate)
	last := dp.HSV{H: 2, S: 1000, V: 500}
	waitFor(t, "newest colour", func() bool {
		w := s.snapshot()
		return len(w) > 0 && w[len(w)-1].dps["28"] != nil
	})
	time.Sleep(20 * time.Millisecond)
	w := s.snapshot()
	if len(w) > 3 {
		t.Fatalf("%d sends; scrubbed values should be skipped", len(w))
	}
	for _, x := range w {
		if x.wait || x.dps["24"] != nil {
			t.Fatalf("Live wrote %v (wait=%v); want DP 28 only, unacked", x.dps, x.wait)
		}
	}
	if got := w[len(w)-1].dps["28"].(string); got[1:13] != hsvDP(last) {
		t.Fatalf("last live %s, want colour %s", got, hsvDP(last))
	}
}

func TestSaveWritesTheShownColourToDP24(t *testing.T) {
	s := &recSession{}
	c := recControl(s)
	defer c.Close()
	ctx := context.Background()
	if err := c.SaveWait(ctx); err != nil || len(s.snapshot()) != 0 {
		t.Fatalf("Save with nothing shown: err=%v writes=%v; want a no-op", err, s.snapshot())
	}
	col := dp.HSV{H: 120, S: 500, V: 250}
	c.Live(col, dp.ChangeJump)
	if err := c.SaveWait(ctx); err != nil {
		t.Fatal(err)
	}
	w := s.snapshot()
	if len(w) != 2 || w[1].dps["24"] != hsvDP(col) || !w[1].wait {
		t.Fatalf("writes %v; want DP 28 then an acked DP 24 = %s", w, hsvDP(col))
	}
	if err := c.SaveWait(ctx); err != nil || len(s.snapshot()) != 2 {
		t.Fatal("a second Save must not rewrite an already saved colour")
	}
}

func TestCommandsRunInOrderAfterEarlierLiveColour(t *testing.T) {
	s := &recSession{}
	c := recControl(s)
	defer c.Close()
	c.Live(dp.HSV{H: 1, S: 1000, V: 1000}, dp.ChangeJump)
	c.Power(false)
	c.Power(true)
	if err := c.SaveWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := s.snapshot()
	if len(w) != 4 || w[0].dps["28"] == nil || w[1].dps["20"] != false || w[2].dps["20"] != true || w[3].dps["24"] == nil {
		t.Fatalf("writes %v; want live, off, on, save", w)
	}
}

func TestCloseSavesTheShownColour(t *testing.T) {
	s := &recSession{}
	c := recControl(s)
	col := dp.HSV{H: 30, S: 800, V: 600}
	c.Live(col, dp.ChangeJump)
	c.Close()
	w := s.snapshot()
	if len(w) == 0 || w[len(w)-1].dps["24"] != hsvDP(col) {
		t.Fatalf("writes %v; Close must save %s", w, hsvDP(col))
	}
}

func TestRefreshReportsTheShownColour(t *testing.T) {
	s := &recSession{}
	c := recControl(s)
	defer c.Close()
	c.Live(dp.HSV{H: 180, S: 500, V: 250}, dp.ChangeJump)
	st, err := c.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Hue != 0.5 || st.Sat != 0.5 || st.BrightPct != 25 {
		t.Fatalf("status %+v; want the live colour, not the saved one", st)
	}
}
