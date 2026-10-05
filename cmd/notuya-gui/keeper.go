package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/alex-bluetrain/notuya-go/pkg/session"
)

// Link keeping: the app holds each bulb's session open and notices when it
// dies, so a bulb that was power-cycled (wall switch, outage) or kicked off by
// another client is reconnected and re-read without the user doing anything.
//
// A bulb that loses power never closes its TCP connection, so a dead link is
// found by a waited heartbeat timing out, not by the socket erroring.
const (
	keepBackoffMin   = 1 * time.Second
	keepBackoffMax   = 10 * time.Second
	keepProbeEvery   = 10 * time.Second
	keepProbeTimeout = 5 * time.Second
)

// keeper is the link-keeping state of one control.
type keeper struct {
	once sync.Once
	stop chan struct{}
	wake chan struct{} // buffered(1): poke the loop (command failed, re-keyed)

	subMu sync.Mutex
	subs  []func(deviceStatus)
}

func newKeeper() *keeper {
	return &keeper{stop: make(chan struct{}), wake: make(chan struct{}, 1)}
}

func (k *keeper) poke() {
	select {
	case k.wake <- struct{}{}:
	default:
	}
}

func (k *keeper) halt() { k.once.Do(func() { close(k.stop) }) }

// Subscribe registers fn to receive the device's status every time the
// control (re)connects. fn runs on the keeper goroutine; UI callers marshal
// to the GTK thread themselves.
func (c *control) Subscribe(fn func(deviceStatus)) {
	c.keep.subMu.Lock()
	c.keep.subs = append(c.keep.subs, fn)
	c.keep.subMu.Unlock()
}

func (c *control) publish(st deviceStatus) {
	c.keep.subMu.Lock()
	subs := append([]func(deviceStatus){}, c.keep.subs...)
	c.keep.subMu.Unlock()
	for _, fn := range subs {
		fn(st)
	}
}

// Start runs the link keeper until Close: connect, publish a status snapshot,
// watch the link, and on loss reconnect with exponential backoff. Its status
// reads queue behind the writer's pending writes, so they see their result.
func (c *control) Start() { go c.keepLoop() }

func (c *control) keepLoop() {
	k := c.keep
	backoff := keepBackoffMin
	failing := false
	for {
		select {
		case <-k.stop:
			return
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		st, err := c.Refresh(ctx)
		cancel()
		if err != nil {
			// The keeper is the probe: a failed read means the link is
			// unusable even when the socket has not errored, so redial.
			c.dropCurrent()
			if !failing {
				fmt.Fprintf(os.Stderr, "notuya-gui: %s: unreachable, retrying: %v\n", c.name(), err)
				failing = true
			}
			if !c.sleep(backoff) {
				return
			}
			backoff = min(backoff*2, keepBackoffMax)
			continue
		}
		if failing {
			fmt.Fprintf(os.Stderr, "notuya-gui: %s: reconnected\n", c.name())
		}
		failing = false
		backoff = keepBackoffMin
		c.publish(st)
		if !c.watchLink() {
			return
		}
	}
}

// sleep waits d (forever when d is 0) or until poked. False means stop.
func (c *control) sleep(d time.Duration) bool {
	var timer <-chan time.Time
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		timer = t.C
	}
	select {
	case <-c.keep.stop:
		return false
	case <-c.keep.wake:
	case <-timer:
	}
	return true
}

// watchLink blocks while the current session is healthy. It returns when the
// session ends, a heartbeat goes unanswered (the session is then dropped), or
// the loop is poked. False means stop.
func (c *control) watchLink() bool {
	c.mu.Lock()
	sess := c.sess
	c.mu.Unlock()
	if sess == nil {
		return true
	}
	t := time.NewTicker(keepProbeEvery)
	defer t.Stop()
	for {
		select {
		case <-c.keep.stop:
			return false
		case <-c.keep.wake:
			return true
		case <-sess.Done():
			return true
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), keepProbeTimeout)
			err := sess.Heartbeat(ctx, true)
			cancel()
			if err != nil {
				c.dropSession(sess)
				return true
			}
		}
	}
}

// dropCurrent closes whatever command session the control holds.
func (c *control) dropCurrent() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeLocked()
}

// dropSession closes sess if it is still the control's command session.
func (c *control) dropSession(sess session.Session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess == sess {
		c.closeLocked()
	}
}
