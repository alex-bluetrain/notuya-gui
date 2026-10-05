package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/alex-bluetrain/notuya-go/pkg/bulb"
	"github.com/alex-bluetrain/notuya-go/pkg/dp"

	"github.com/alex-bluetrain/notuya-go/pkg/session"
	v35 "github.com/alex-bluetrain/notuya-go/pkg/session/v35"
	"github.com/alex-bluetrain/notuya-gui/internal/colour"
)

// deviceStatus is the parsed snapshot the desktop app renders per device,
// derived from one bulb.Status round-trip. The keeper publishes one on every
// (re)connect.
type deviceStatus struct {
	On        bool
	Mode      dp.WorkMode
	BrightPct float64 // 0-100
	TempPct   float64 // 0-100 (white mode colour temperature)
	Hue       float64 // 0-1
	Sat       float64 // 0-1
	HasColour bool    // DP 24 was present and parseable
	HasTemp   bool    // DP 23 was present and parseable
}

// control owns one persistent session for a single device. Its keeper holds
// the session open and redials it when the link drops. Every write and status
// read goes through the control's writer, one at a time in the order asked;
// c.mu guards only the fields below and the dial.
//
// It is the bulb's single channel for the whole app: every tab (Lights,
// Scenes, the scene editor, Settings' Test) must drive a device through the
// one control the app keys by device_id, never a private session. A Tuya bulb
// tolerates one connection at a time and silently drops the older one, so a
// second session anywhere kills the shared one for every other tab.
type control struct {
	dev Device

	// dial opens a session for dev; a test swaps in a fake. Defaults to
	// dialV35.
	dial dialFunc

	mu   sync.Mutex
	sess session.Session
	bulb *bulb.Bulb

	// w is the request queue; its goroutine is the only writer to the bulb
	// (see writer.go).
	w *writer
	// shown is the last Live colour sent but not yet saved to DP 24. Only
	// the sender touches it.
	shown *dp.HSV

	// keep holds the link keeper's state (see keeper.go). Only the app
	// starts it; the wizard's controls stay lazy.
	keep *keeper
}

func newControl(d Device) *control {
	return &control{dev: d, dial: dialV35, keep: newKeeper(), w: newWriter()}
}

// dialFunc opens (dials and handshakes) a session to one bulb.
type dialFunc func(ctx context.Context, addr string, localKey []byte) (session.Session, error)

func dialV35(ctx context.Context, addr string, localKey []byte) (session.Session, error) {
	return v35.Open(ctx, addr, localKey, v35.Options{})
}

// device returns the device this control currently drives.
func (c *control) device() Device {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dev
}

// reconfigure points the control at d (a new key or IP for the same bulb),
// dropping any open command session so the next command dials with the new
// credentials. A no-op when nothing changed. Settings' Test uses it so a
// re-keyed device is still driven through the app's one control.
func (c *control) reconfigure(d Device) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dev == d {
		return
	}
	c.closeLocked()
	c.dev = d
	c.keep.poke()
}

// deviceName is a human label for logs and banners.
func deviceName(d Device) string {
	if d.Name != "" {
		return d.Name
	}
	return d.DeviceID
}

// name is deviceName for callers not holding c.mu (the keeper, the
// writer, the GTK thread); locked code uses deviceName(c.dev).
func (c *control) name() string { return deviceName(c.device()) }

// connectLocked opens the session if there is none (startup, after a drop).
// The caller must hold c.mu.
func (c *control) connectLocked(ctx context.Context) error {
	if c.sess != nil {
		return nil
	}
	openCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	sess, err := c.dial(openCtx, c.dev.IPAddress, []byte(c.dev.LocalKey))
	if err != nil {
		return fmt.Errorf("%s: opening session: %w", deviceName(c.dev), err)
	}
	c.sess = sess
	c.bulb = bulb.New(wireSession{Session: sess, id: c.dev.DeviceID}, deviceName(c.dev))
	return nil
}

// closeLocked tears down the command session, if any. The caller must hold
// c.mu.
func (c *control) closeLocked() {
	if c.sess != nil {
		c.sess.Close()
		c.sess = nil
		c.bulb = nil
	}
}

// acquire returns the connected bulb, dialling first if needed, and whether
// the session already existed before this call. Only the lookup and the dial
// hold c.mu; the command itself runs outside it, since a session is safe for
// concurrent use.
func (c *control) acquire(ctx context.Context) (session.Session, *bulb.Bulb, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	existed := c.sess != nil
	if err := c.connectLocked(ctx); err != nil {
		return nil, nil, false, err
	}
	return c.sess, c.bulb, existed, nil
}

// withBulb runs fn against the connected bulb, opening the session first if
// needed. Only the writer calls it.
//
// A failed command drops its session only when that session is dead (Done
// closed); a rejected value, a timeout or a cancelled context leaves a live
// link alone. When a session that already existed turns out dead and the
// caller's context is still alive, it reconnects and retries once right away:
// the keeper would redial too, but only after its next probe, and the retry
// keeps a command issued in those seconds from being lost.
func (c *control) withBulb(ctx context.Context, fn func(b *bulb.Bulb) error) error {
	sess, b, existed, err := c.acquire(ctx)
	if err != nil {
		return err
	}
	if err = fn(b); err == nil || !isDead(sess) {
		return err
	}
	c.dropSession(sess)
	if !existed || ctx.Err() != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "notuya-gui: %s: reconnecting after %v\n", c.name(), err)
	sess, b, _, rerr := c.acquire(ctx)
	if rerr != nil {
		return rerr
	}
	if rerr = fn(b); rerr != nil && isDead(sess) {
		c.dropSession(sess)
	}
	return rerr
}

// isDead reports whether sess has failed (its Done channel is closed).
func isDead(sess session.Session) bool {
	select {
	case <-sess.Done():
		return true
	default:
		return false
	}
}

// Close stops the link keeper and the writer. It waits (bounded) for the
// writer to save the shown live colour, so call it off the GTK thread when
// that matters. Safe to call more than once.
func (c *control) Close() {
	c.keep.halt()
	if c.w.close() {
		select {
		case <-c.w.done:
		case <-time.After(commandTimeout):
		}
	}
	c.dropCurrent()
}

// Refresh queries the device once, in turn with every write, and parses a
// deviceStatus snapshot. Bounded by ctx.
func (c *control) Refresh(ctx context.Context) (deviceStatus, error) {
	var st deviceStatus
	err := c.call(ctx, "status", false, func(ctx context.Context, b *bulb.Bulb) error {
		s, err := b.Status(ctx)
		if err != nil {
			return err
		}
		st = parseStatus(s)
		// The bulb reports its saved colour (DP 24), not the unsaved live
		// colour it is showing: report what it shows.
		if c.shown != nil {
			st.Mode = dp.ModeColour
			st.HasColour = true
			st.Hue = float64(c.shown.H) / 360
			st.Sat = float64(c.shown.S) / 1000
			st.BrightPct = float64(c.shown.V) / 10
		}
		return nil
	})
	return st, err
}

// parseStatus derives the panel snapshot from a decoded bulb state. DPs the
// bulb did not report leave their fields at zero — a scene-mode bulb, say,
// may not report a colour.
func parseStatus(s dp.State) deviceStatus {
	sc := s.Schema
	st := deviceStatus{
		On:        s.On,
		Mode:      s.Mode,
		BrightPct: s.BrightnessPercent(),
		HasTemp:   s.Has(sc.ColourTempDP),
		HasColour: s.Has(sc.ColourDP),
	}
	if st.HasTemp {
		st.TempPct = s.ColourTempPercent()
	}
	if st.HasColour {
		st.Hue = float64(s.Colour.H) / 360
		st.Sat = float64(s.Colour.S) / 1000
		// In colour mode brightness is DP 24's V component, not the white-mode
		// brightness DP (which goes stale when the bulb leaves white mode).
		if st.Mode == dp.ModeColour {
			st.BrightPct = float64(s.Colour.V) / 10
		}
	}
	return st
}

// SetPower turns the bulb on or off and waits for it to ack.
func (c *control) SetPower(ctx context.Context, on bool) error {
	return c.call(ctx, "power", false, powerFn(on))
}

// Power queues turning the bulb on or off.
func (c *control) Power(on bool) { c.do("power", false, powerFn(on)) }

// WhiteBrightness queues white mode at pct brightness (DP 22).
func (c *control) WhiteBrightness(pct float64) {
	c.do("brightness", true, func(ctx context.Context, b *bulb.Bulb) error {
		return b.SetWhiteBrightness(ctx, pct)
	})
}

// ColourTemp queues white mode at a cold↔warm percentage (the DP is a raw
// 0-1000 scale, not Kelvin).
func (c *control) ColourTemp(pct float64) {
	c.do("temperature", true, func(ctx context.Context, b *bulb.Bulb) error {
		return b.SetColourTempPercent(ctx, pct)
	})
}

// Apply queues driving the bulb to a scene state as one request, so nothing
// lands between its writes. An off state only cuts power. An on state first
// turns the switch on (colour/temp DPs do not power a bulb that is off), then
// applies the mode data. In colour mode brightness is the "v" of the colour,
// so it is one SetColour write; in white mode temp and brightness are distinct
// DPs, applied temp first, brightness last.
func (c *control) Apply(st SceneState) {
	c.do("apply scene", true, func(ctx context.Context, b *bulb.Bulb) error {
		if !st.On {
			return b.TurnOff(ctx)
		}
		if err := b.TurnOn(ctx); err != nil {
			return err
		}
		switch st.Mode {
		case dp.ModeColour:
			return b.SetColour(ctx, colour.HSV(st.Hue, st.Sat, st.Bright/100.0))
		case dp.ModeWhite:
			if err := b.SetColourTempPercent(ctx, st.Temp); err != nil {
				return err
			}
			return b.SetWhiteBrightness(ctx, st.Bright)
		default:
			// Captured from a mode we don't model; the switch is already on.
			return nil
		}
	})
}

func powerFn(on bool) func(ctx context.Context, b *bulb.Bulb) error {
	return func(ctx context.Context, b *bulb.Bulb) error {
		if on {
			return b.TurnOn(ctx)
		}
		return b.TurnOff(ctx)
	}
}
