package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/alex-bluetrain/notuya-go/pkg/bulb"
	"github.com/alex-bluetrain/notuya-go/pkg/dp"
)

// The writer is the only thing that talks to a bulb. One goroutine per bulb
// (the sender) carries everything out over the one held session.
//
// Live colours go through a slot: Live overwrites it and returns, and the
// sender sends the slot on DP 28 (unacked, carrying the jump/fade bit) only
// when it changed. Values overwritten before the sender got to them are never
// sent — the newest wins. DP 28 is a preview: the bulb keeps showing it but
// does not save it, so Save writes the last shown colour to DP 24.
//
// Everything else (power, scenes, white mode, Save, status reads) is a
// command, run in the order asked. Queuing a command moves a changed slot into
// the queue ahead of it, and the sender sends the slot only once the queue is
// empty, so writes land in the order they were asked either way.
const liveInterval = time.Millisecond // minimum spacing between DP 28 sends

var errClosed = errors.New("control closed")

// writeReq is one queued command.
type writeReq struct {
	what string
	fn   func(ctx context.Context, b *bulb.Bulb) error
	// setsColour: fn writes the bulb's saved colour or mode itself, so the
	// shown live colour no longer needs saving.
	setsColour bool
	save       bool // write the shown live colour to DP 24
	live       bool // send col on DP 28 (a slot value moved into the queue)
	col        dp.HSV
	mode       dp.ChangeMode
	ctx        context.Context // nil: bounded by commandTimeout
	res        chan error      // nil: fire and forget (failures log + re-read)
}

// writer is a control's slot and command list.
type writer struct {
	once    sync.Once
	started bool          // guarded by mu
	kick    chan struct{} // buffered(1)
	done    chan struct{} // closed when the sender exits

	mu       sync.Mutex
	slot     dp.HSV
	slotMode dp.ChangeMode
	dirty    bool
	hasSlot  bool // Live has been called at least once
	queue    []writeReq
	closed   bool
}

func newWriter() *writer {
	return &writer{kick: make(chan struct{}, 1), done: make(chan struct{})}
}

func (w *writer) wake() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

func (w *writer) setSlot(c dp.HSV, mode dp.ChangeMode) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.slot, w.slotMode, w.dirty, w.hasSlot = c, mode, true, true
	w.wake()
}

// peekSlot returns the slot's current value for display; ok is false until
// the first Live.
func (w *writer) peekSlot() (c dp.HSV, mode dp.ChangeMode, ok bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.slot, w.slotMode, w.hasSlot
}

func (w *writer) takeSlot() (dp.HSV, dp.ChangeMode, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.dirty {
		return dp.HSV{}, 0, false
	}
	w.dirty = false
	return w.slot, w.slotMode, true
}

func (w *writer) push(r writeReq) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return false
	}
	if w.dirty {
		w.queue = append(w.queue, writeReq{what: "live colour", live: true, col: w.slot, mode: w.slotMode})
		w.dirty = false
	}
	w.queue = append(w.queue, r)
	w.wake()
	return true
}

// next pops the head command. closed reports that no more will come.
func (w *writer) next() (r writeReq, ok, closed bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.queue) == 0 {
		return writeReq{}, false, w.closed
	}
	r = w.queue[0]
	w.queue = w.queue[1:]
	return r, true, w.closed
}

// close stops new requests; the sender finishes what is queued, saves the
// shown live colour, then exits. It reports whether the sender was running.
func (w *writer) close() bool {
	w.mu.Lock()
	w.closed = true
	started := w.started
	w.mu.Unlock()
	w.wake()
	return started
}

// start launches the sender on first use.
func (c *control) start() {
	c.w.once.Do(func() {
		c.w.mu.Lock()
		closed := c.w.closed
		c.w.started = !closed
		c.w.mu.Unlock()
		if !closed {
			go c.sender()
		}
	})
}

// Live shows col on the bulb (DP 28) without saving it. It never blocks;
// call it from the GTK thread as often as a drag or a capture produces
// colours. Save makes the last one stick.
func (c *control) Live(col dp.HSV, mode dp.ChangeMode) {
	c.start()
	c.w.setSlot(col, mode)
}

// Save queues writing the last Live colour to DP 24, so the bulb keeps it
// after a power cycle and status reads report it. A no-op when nothing was
// shown since the last save.
func (c *control) Save() {
	c.start()
	c.w.push(writeReq{what: "save colour", save: true})
}

// do queues a command and returns at once. The UI has already updated
// optimistically; on failure the keeper is poked to re-read and publish the
// bulb's real state, which puts the UI right.
// SaveWait is Save that waits for the write, bounded by ctx.
func (c *control) SaveWait(ctx context.Context) error {
	c.start()
	res := make(chan error, 1)
	if !c.w.push(writeReq{what: "save colour", save: true, ctx: ctx, res: res}) {
		return fmt.Errorf("%s: %w", c.name(), errClosed)
	}
	select {
	case err := <-res:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *control) do(what string, setsColour bool, fn func(ctx context.Context, b *bulb.Bulb) error) {
	c.start()
	c.w.push(writeReq{what: what, fn: fn, setsColour: setsColour})
}

// call queues a command and waits for its result. ctx bounds both the wait
// and the command.
func (c *control) call(ctx context.Context, what string, setsColour bool, fn func(ctx context.Context, b *bulb.Bulb) error) error {
	c.start()
	res := make(chan error, 1)
	if !c.w.push(writeReq{what: what, fn: fn, setsColour: setsColour, ctx: ctx, res: res}) {
		return fmt.Errorf("%s: %w", c.name(), errClosed)
	}
	select {
	case err := <-res:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sender is the bulb's one writer. c.shown is owned by it.
func (c *control) sender() {
	defer close(c.w.done)
	var lastSend time.Time
	live := func(col dp.HSV, mode dp.ChangeMode) {
		if d := liveInterval - time.Since(lastSend); d > 0 {
			time.Sleep(d)
		}
		c.sendLive(col, mode)
		lastSend = time.Now()
		c.shown = &col
	}
	for {
		r, ok, closed := c.w.next()
		if ok && r.live {
			live(r.col, r.mode)
			continue
		}
		if ok {
			c.run(r)
			continue
		}
		if col, mode, ok := c.w.takeSlot(); ok {
			live(col, mode)
			continue
		}
		if closed {
			c.run(writeReq{what: "save colour", save: true})
			c.dropCurrent()
			return
		}
		<-c.w.kick
	}
}

// run carries out one command and reports its result.
func (c *control) run(r writeReq) {
	if r.save {
		if c.shown == nil {
			if r.res != nil {
				r.res <- nil
			}
			return
		}
		col := *c.shown
		r.fn = func(ctx context.Context, b *bulb.Bulb) error { return b.SetColour(ctx, col) }
		r.setsColour = true
	}
	ctx := r.ctx
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
	}
	err := c.withBulb(ctx, func(b *bulb.Bulb) error { return r.fn(ctx, b) })
	if err == nil && r.setsColour {
		c.shown = nil
	}
	if r.res != nil {
		r.res <- err
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "notuya-gui: %s %s: %v\n", c.name(), r.what, err)
		c.keep.poke()
	}
}

// sendLive writes one DP 28 colour. A failure is not retried: Save writes
// the colour again.
func (c *control) sendLive(col dp.HSV, mode dp.ChangeMode) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	sess, b, _, err := c.acquire(ctx)
	if err == nil {
		liveSends.Add(1)
		if err = b.SendLive(ctx, col, mode); err != nil && isDead(sess) {
			c.dropSession(sess)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "notuya-gui: %s live colour: %v\n", c.name(), err)
	}
}
