package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/alex-bluetrain/notuya-go/pkg/bulb"
	"github.com/alex-bluetrain/notuya-go/pkg/dp"
	"github.com/alex-bluetrain/notuya-go/pkg/session"
	v35 "github.com/alex-bluetrain/notuya-go/pkg/session/v35"
)

// errLiveStream is returned by withBulb while a live colour stream owns the
// bulb: opening a command session beside it would give the bulb two sessions,
// and Tuya bulbs drop the older one. Callers that need the bulb mid-stream use
// UpdateLive instead.
var errLiveStream = errors.New("live stream active, command refused")

// deviceStatus is the parsed snapshot the desktop app renders per device. It
// is derived from one bulb.Status round-trip so a panel refresh costs a
// single query.
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

// control owns one persistent session for a single device and
// serializes every command behind a mutex, since a session is not
// concurrency-safe. The app issues discrete waited commands over this session
// and only borrows a live stream (the streamer) during a live colour drag.
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

	// live is a live colour streamer borrowed for a colour drag or Screen Sync.
	// While non-nil the command session is closed so the bulb only ever has
	// one session open at a time.
	live *streamer

	// liveOwner identifies who opened live; only it may update or end it.
	liveOwner any

	// liveMode is the DP 28 change mode (jump/fade) sent with every
	// streamed colour during the current stream. Set by BeginLive.
	liveMode dp.ChangeMode

	// keep holds the link keeper's state (see keeper.go). Only the app
	// starts it; the wizard's controls stay lazy.
	keep *keeper
}

func newControl(d Device) *control {
	return &control{dev: d, dial: dialV35, keep: newKeeper()}
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

// name returns a human label for logs and the panel header.
func (c *control) name() string {
	if c.dev.Name != "" {
		return c.dev.Name
	}
	return c.dev.DeviceID
}

// connect lazily opens the session (idempotent). The caller must hold c.mu.
func (c *control) connectLocked(ctx context.Context) error {
	if c.sess != nil {
		return nil
	}
	openCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	sess, err := c.dial(openCtx, c.dev.IPAddress, []byte(c.dev.LocalKey))
	if err != nil {
		return fmt.Errorf("%s: opening session: %w", c.name(), err)
	}
	c.sess = sess
	c.bulb = bulb.New(sess, c.name())
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

// withBulb runs fn against the connected bulb under the mutex, opening the
// session first if needed. All device I/O funnels through here so a single
// session is never touched concurrently.
//
// It refuses to run while a live stream owns the bulb (errLiveStream). A
// failing command drops the session so the next command reconnects: a session
// the bulb has silently dropped (another connection won, the bulb rebooted)
// otherwise fails forever. When the session already existed before this call
// and the caller's context is still alive, it reconnects and retries once
// right away, so a stale session costs one handshake rather than one lost
// command.
func (c *control) withBulb(ctx context.Context, fn func(b *bulb.Bulb) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.live != nil {
		return fmt.Errorf("%s: %w", c.name(), errLiveStream)
	}
	stale := c.sess != nil
	if err := c.connectLocked(ctx); err != nil {
		return err
	}
	err := fn(c.bulb)
	if err == nil {
		return nil
	}
	c.closeLocked()
	if !stale || ctx.Err() != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "notuya-gui: %s: reconnecting after %v\n", c.name(), err)
	if rerr := c.connectLocked(ctx); rerr != nil {
		return rerr
	}
	if rerr := fn(c.bulb); rerr != nil {
		c.closeLocked()
		return rerr
	}
	return nil
}

// async runs one bounded device command off the GTK thread, so a slow or
// unreachable bulb never stalls the UI. The UI has already updated
// optimistically and the next refresh reconciles, so a failure is only logged.
func (c *control) async(what string, fn func(ctx context.Context) error) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		if err := fn(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "notuya-gui: %s %s: %v\n", c.name(), what, err)
		}
	}()
}

// Close stops the link keeper and tears down the session. Safe to call more
// than once.
func (c *control) Close() {
	c.keep.halt()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeLocked()
}

// Refresh queries the device once and parses a deviceStatus snapshot.
func (c *control) Refresh(ctx context.Context) (deviceStatus, error) {
	var st deviceStatus
	err := c.withBulb(ctx, func(b *bulb.Bulb) error {
		qctx, cancel := context.WithTimeout(ctx, commandTimeout)
		defer cancel()
		s, err := b.Status(qctx)
		if err != nil {
			return err
		}
		st = parseStatus(s)
		return nil
	})
	return st, err
}

// Seed returns the bulb's current colour for a live stream to start from:
// its colour when it is on in colour mode, black otherwise. Screen Sync
// calls it before BeginLive, since status queries are refused once
// streaming.
func (c *control) Seed(ctx context.Context) dp.RGB {
	st, err := c.Refresh(ctx)
	if err != nil || !st.On || st.Mode != dp.ModeColour {
		return dp.RGB{}
	}
	r, g, b := hsvToRGBInt(st.Hue, st.Sat, st.BrightPct/100)
	return dp.RGB{R: r, G: g, B: b}
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

// SetPower turns the bulb on or off.
func (c *control) SetPower(ctx context.Context, on bool) error {
	return c.withBulb(ctx, func(b *bulb.Bulb) error {
		cctx, cancel := context.WithTimeout(ctx, commandTimeout)
		defer cancel()
		if on {
			return b.TurnOn(cctx)
		}
		return b.TurnOff(cctx)
	})
}

// SetColour switches to colour mode and applies an RGB colour, committing it
// so it sticks (used on drag release and for discrete colour picks).
func (c *control) SetColour(ctx context.Context, rgb dp.RGB) error {
	return c.withBulb(ctx, func(b *bulb.Bulb) error {
		cctx, cancel := context.WithTimeout(ctx, commandTimeout)
		defer cancel()
		return b.SetColour(cctx, rgb)
	})
}

// SetWhiteBrightness switches to white mode and sets brightness (DP 22).
func (c *control) SetWhiteBrightness(ctx context.Context, pct float64) error {
	return c.withBulb(ctx, func(b *bulb.Bulb) error {
		cctx, cancel := context.WithTimeout(ctx, commandTimeout)
		defer cancel()
		return b.SetWhiteBrightness(cctx, pct)
	})
}

// SetColourTempPercent switches to white mode and sets colour temperature as
// a cold↔warm percentage (the DP is a raw 0-1000 scale, not Kelvin).
func (c *control) SetColourTempPercent(ctx context.Context, pct float64) error {
	return c.withBulb(ctx, func(b *bulb.Bulb) error {
		cctx, cancel := context.WithTimeout(ctx, commandTimeout)
		defer cancel()
		return b.SetColourTempPercent(cctx, pct)
	})
}

// ApplyState drives the device to the state captured in a scene. An off state
// only cuts power. An on state first turns the switch on (colour/temp DPs do
// not power a bulb that is off), then applies the mode data. In colour mode
// brightness is the "v" of the colour, so it is baked into a single SetColour
// write (v = st.Bright) — no separate brightness command, no read-modify race.
// In white mode temp and brightness are distinct DPs, applied as two writes
// (temp first, brightness last).
func (c *control) ApplyState(ctx context.Context, st SceneState) error {
	if !st.On {
		return c.SetPower(ctx, false)
	}
	if err := c.SetPower(ctx, true); err != nil {
		return err
	}
	switch st.Mode {
	case dp.ModeColour:
		r, g, b := hsvToRGBInt(st.Hue, st.Sat, st.Bright/100.0)
		return c.SetColour(ctx, dp.RGB{R: r, G: g, B: b})
	case dp.ModeWhite:
		if err := c.SetColourTempPercent(ctx, st.Temp); err != nil {
			return err
		}
		return c.SetWhiteBrightness(ctx, st.Bright)
	default:
		// Captured from a mode we don't model; the switch is already on.
		return nil
	}
}

// BeginLive closes the command session and opens a live colour streamer
// seeded with the current colour, owned by owner (any comparable value
// identifying the caller: a tab, a scene-editor row, Screen Sync). It is a
// no-op if owner already streams, and refuses with errLiveStream if someone
// else does, so a Lights wheel drag cannot hijack or end Screen Sync's stream.
// Safe to call from the GTK thread — the streamer starts its own goroutines.
func (c *control) BeginLive(owner any, seed dp.RGB, mode dp.ChangeMode) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.live != nil {
		if c.liveOwner == owner {
			return nil
		}
		return fmt.Errorf("%s: %w", c.name(), errLiveStream)
	}
	// Hand the single session over to the streamer: close command mode first.
	c.closeLocked()
	c.liveOwner = owner
	c.liveMode = mode
	c.live = newStreamer([]Device{c.dev}, seed, bulb.StreamOptions{ChangeMode: mode.Ptr()}, c.dial)
	return nil
}

// SetLiveChangeMode changes the jump/fade mode applied to owner's subsequent
// UpdateLive sends without restarting the stream, so the toggle takes effect
// mid-drag.
func (c *control) SetLiveChangeMode(owner any, mode dp.ChangeMode) {
	c.mu.Lock()
	if c.live != nil && c.liveOwner == owner {
		c.liveMode = mode
	}
	c.mu.Unlock()
}

// UpdateLive streams a colour on owner's stream (newest-wins, non-blocking),
// carrying the current change mode. Ignored when owner does not stream.
func (c *control) UpdateLive(owner any, rgb dp.RGB) {
	c.mu.Lock()
	live := c.live
	mode := c.liveMode
	if c.liveOwner != owner {
		live = nil
	}
	c.mu.Unlock()
	if live != nil {
		live.Set(rgb, mode.Ptr())
	}
}

// EndLive closes owner's streamer, which flushes the pending colour and
// re-sends it with a normal SetColour so the final colour sticks; the
// command session then re-opens lazily on the next command. Ignored when
// owner does not stream. It blocks up to one command timeout, so call it off
// the GTK thread.
func (c *control) EndLive(owner any) {
	c.mu.Lock()
	live := c.live
	if live == nil || c.liveOwner != owner {
		c.mu.Unlock()
		return
	}
	c.live = nil
	c.liveOwner = nil
	c.mu.Unlock()
	live.Close()
	c.keep.poke() // reconnect and re-read the state the stream left behind
}

// Streaming reports whether a live stream owns the bulb. Tabs use it to skip
// or disable a light rather than hit errLiveStream.
func (c *control) Streaming() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.live != nil
}

// Dead returns a channel closed when the current live stream's session drops
// (the stream keeps nothing running after that). Nil when not streaming.
func (c *control) Dead() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.live == nil {
		return nil
	}
	return c.live.dead
}
