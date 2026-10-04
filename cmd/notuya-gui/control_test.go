package main

import (
	"context"
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
	return s.cmdErrs[i]
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

	err := c.SetColour(ctx, dp.RGB{R: 1, G: 2, B: 3})
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("want the command error back, got %v", err)
	}
	if *dials != 1 || s1.closed != 1 || c.sess != nil {
		t.Fatalf("dials=%d s1.closed=%d sess=%v; a fresh session must fail without retry and be dropped", *dials, s1.closed, c.sess)
	}
	// The next command reconnects on its own.
	if err := c.SetColour(ctx, dp.RGB{R: 1, G: 2, B: 3}); err != nil {
		t.Fatalf("next command should reconnect: %v", err)
	}
	if *dials != 2 || c.sess != s2 {
		t.Fatalf("dials=%d sess=%v; want 2 and s2", *dials, c.sess)
	}
}

func TestWithBulbDoesNotRetryWhenCallerContextIsDone(t *testing.T) {
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
	if *dials != 1 || s1.closed != 1 || c.sess != nil {
		t.Fatalf("dials=%d closed=%d sess=%v; the dead session must be dropped, not redialled", *dials, s1.closed, c.sess)
	}
}

func TestWithBulbRefusedWhileLiveStreamOwnsBulb(t *testing.T) {
	c, dials := fakeControl(t)
	c.live = &streamer{} // what BeginLive leaves behind; never started here
	err := c.SetPower(context.Background(), true)
	if !errors.Is(err, errLiveStream) {
		t.Fatalf("want errLiveStream, got %v", err)
	}
	if *dials != 0 {
		t.Fatalf("dialled %d times beside a live stream; want 0", *dials)
	}
	if _, err := c.Refresh(context.Background()); !errors.Is(err, errLiveStream) {
		t.Fatalf("Refresh: want errLiveStream, got %v", err)
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

// liveSession is a session.Session for the streamer: safe for its
// goroutines, it counts requests and can fail the open to simulate a
// dropped bulb.
type liveSession struct {
	mu      sync.Mutex
	openErr error
	calls   int
	closed  bool
	done    chan struct{}
}

func (s *liveSession) count() {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
}

func (s *liveSession) Query(context.Context) ([]byte, error) {
	s.count()
	return []byte(`{"dps":{}}`), nil
}
func (s *liveSession) Control(context.Context, []byte, bool) error { s.count(); return nil }
func (s *liveSession) Refresh(context.Context, []int) error        { s.count(); return nil }
func (s *liveSession) Heartbeat(context.Context, bool) error       { s.count(); return nil }
func (s *liveSession) Pushes() <-chan session.Push                 { return nil }
func (s *liveSession) Done() <-chan struct{}                       { return s.done }
func (s *liveSession) Err() error                                  { return nil }

func (s *liveSession) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

func liveControl(sess *liveSession) *control {
	c := newControl(Device{DeviceID: "dev1", Name: "Desk"})
	c.dial = func(context.Context, string, []byte) (session.Session, error) {
		if sess.openErr != nil {
			return nil, sess.openErr
		}
		return sess, nil
	}
	return c
}

func TestLiveStreamBelongsToItsOwner(t *testing.T) {
	sess := &liveSession{}
	c := liveControl(sess)
	sync1, drag := new(int), new(int)
	seed := dp.RGB{R: 1}

	if err := c.BeginLive(sync1, seed, dp.ChangeFade); err != nil {
		t.Fatal(err)
	}
	if !c.Streaming() {
		t.Fatal("Streaming() = false after BeginLive")
	}
	if err := c.BeginLive(sync1, seed, dp.ChangeFade); err != nil {
		t.Fatalf("re-begin by the same owner must be a no-op, got %v", err)
	}
	if err := c.BeginLive(drag, seed, dp.ChangeFade); !errors.Is(err, errLiveStream) {
		t.Fatalf("another owner must be refused with errLiveStream, got %v", err)
	}
	c.EndLive(drag) // a Lights drag ending must not close sync's stream
	if !c.Streaming() {
		t.Fatal("EndLive from a different owner closed the stream")
	}
	c.SetLiveChangeMode(drag, dp.ChangeJump)
	if c.liveMode != dp.ChangeFade {
		t.Fatal("SetLiveChangeMode from a different owner changed the mode")
	}
	c.EndLive(sync1)
	if c.Streaming() || c.Dead() != nil {
		t.Fatal("owner's EndLive must release the bulb")
	}
	if !sess.closed {
		t.Fatal("stream session not closed on EndLive")
	}
}

func TestDeadFiresWhenStreamSessionDrops(t *testing.T) {
	c := liveControl(&liveSession{openErr: syscall.ECONNREFUSED})
	owner := new(int)
	if err := c.BeginLive(owner, dp.RGB{}, dp.ChangeFade); err != nil {
		t.Fatal(err)
	}
	dead := c.Dead()
	if dead == nil {
		t.Fatal("Dead() is nil while streaming")
	}
	select {
	case <-dead:
	case <-time.After(5 * time.Second):
		t.Fatal("Dead() did not fire after the stream failed")
	}
	c.UpdateLive(owner, dp.RGB{R: 9}) // must not block on a dead stream
	c.EndLive(owner)
	if c.Streaming() {
		t.Fatal("still streaming after EndLive")
	}
}

func TestDeadStaysOpenWhileStreamIsHealthy(t *testing.T) {
	sess := &liveSession{}
	c := liveControl(sess)
	owner := new(int)
	if err := c.BeginLive(owner, dp.RGB{R: 1}, dp.ChangeFade); err != nil {
		t.Fatal(err)
	}
	c.UpdateLive(owner, dp.RGB{R: 2})
	select {
	case <-c.Dead():
		t.Fatal("Dead() fired on a healthy stream")
	case <-time.After(200 * time.Millisecond):
	}
	c.EndLive(owner)
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.calls == 0 {
		t.Fatal("stream sent nothing")
	}
}
