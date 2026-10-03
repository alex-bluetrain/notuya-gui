package main

import (
	"context"
	"errors"
	"syscall"
	"testing"

	"github.com/alex-bluetrain/notuya-go/pkg/device"
	"github.com/alex-bluetrain/notuya-go/pkg/protocol"
)

// fakeSession is an in-memory protocol.Session: it records opens/closes and
// fails the commands listed in cmdErrs (by call index), answering every other
// command with a canned status body.
type fakeSession struct {
	openErr error
	cmdErrs map[int]error
	opened  int
	closed  int
	calls   int
}

func (s *fakeSession) Open(context.Context) error {
	s.opened++
	return s.openErr
}

func (s *fakeSession) Command(ctx context.Context, _ uint32, _ []byte, _ bool) ([]byte, error) {
	i := s.calls
	s.calls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.cmdErrs[i]; err != nil {
		return nil, err
	}
	return []byte(`{"dps":{"20":true,"21":"colour"}}`), nil
}

func (s *fakeSession) Close() error {
	s.closed++
	return nil
}

var _ protocol.Session = (*fakeSession)(nil)

// fakeControl returns a control whose dial hands out the given sessions in
// order, plus a counter of dials.
func fakeControl(t *testing.T, sessions ...*fakeSession) (*control, *int) {
	t.Helper()
	c := newControl(Device{DeviceID: "dev1", IPAddress: "127.0.0.1", LocalKey: "0123456789abcdef", Name: "Desk"})
	dials := 0
	c.dial = func(string, []byte) protocol.Session {
		if dials >= len(sessions) {
			t.Fatalf("unexpected dial #%d", dials+1)
		}
		s := sessions[dials]
		dials++
		return s
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

	err := c.SetColour(ctx, device.RGB{R: 1, G: 2, B: 3})
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("want the command error back, got %v", err)
	}
	if *dials != 1 || s1.closed != 1 || c.sess != nil {
		t.Fatalf("dials=%d s1.closed=%d sess=%v; a fresh session must fail without retry and be dropped", *dials, s1.closed, c.sess)
	}
	// The next command reconnects on its own.
	if err := c.SetColour(ctx, device.RGB{R: 1, G: 2, B: 3}); err != nil {
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
	c.live = &streamer{} // what BeginLiveDrag leaves behind; never started here
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
	if !st.On || st.Mode != device.ModeColour {
		t.Fatalf("status = %+v; want On in colour mode", st)
	}
}
