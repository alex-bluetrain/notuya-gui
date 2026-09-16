package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/averstraeten/notuya-go/pkg/bulb"
	"github.com/averstraeten/notuya-go/pkg/device"
	"github.com/averstraeten/notuya-go/pkg/protocol35"
)

// deviceStatus is the parsed snapshot the desktop app renders per device. It
// is derived from one device.Status round-trip so a panel refresh costs a
// single query.
type deviceStatus struct {
	On        bool
	Mode      string  // device.ModeWhite / ModeColour / ModeScene / music
	BrightPct float64 // 0-100
	TempPct   float64 // 0-100 (white mode colour temperature)
	Hue       float64 // 0-1
	Sat       float64 // 0-1
	HasColour bool    // DP 24 was present and parseable
	HasTemp   bool    // DP 23 was present and parseable
}

// control owns one persistent protocol35 session for a single device and
// serializes every command behind a mutex, since a session is not
// concurrency-safe. Unlike the picker's streamer (one lifelong music-mode
// stream), the app mostly issues discrete waited commands over this session
// and only borrows music mode during a live colour drag.
type control struct {
	dev Device

	mu   sync.Mutex
	sess *protocol35Session
	bulb *bulb.Bulb

	// live is a short-lived music-mode streamer borrowed for a colour drag.
	// While non-nil the command session is closed so the bulb only ever has
	// one session open at a time.
	live *streamer
}

// protocol35Session is the concrete session type returned by
// protocol35.NewSession. Aliased so control can hold it without leaking the
// import into other files.
type protocol35Session = protocol35.Session

func newControl(d Device) *control {
	return &control{dev: d}
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
	sess := protocol35.NewSession(c.dev.IPAddress, []byte(c.dev.LocalKey))
	openCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	if err := sess.Open(openCtx); err != nil {
		return fmt.Errorf("%s: opening session: %w", c.name(), err)
	}
	c.sess = sess
	c.bulb = bulb.NewBulb(sess, c.name())
	return nil
}

// withBulb runs fn against the connected bulb under the mutex, opening the
// session first if needed. All device I/O funnels through here so a single
// session is never touched concurrently.
func (c *control) withBulb(ctx context.Context, fn func(b *bulb.Bulb) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.connectLocked(ctx); err != nil {
		return err
	}
	return fn(c.bulb)
}

// Close tears down the session. Safe to call more than once.
func (c *control) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess != nil {
		c.sess.Close()
		c.sess = nil
		c.bulb = nil
	}
}

// Refresh queries the device once and parses a deviceStatus snapshot.
func (c *control) Refresh(ctx context.Context) (deviceStatus, error) {
	var st deviceStatus
	err := c.withBulb(ctx, func(b *bulb.Bulb) error {
		qctx, cancel := context.WithTimeout(ctx, commandTimeout)
		defer cancel()
		dps, err := b.Raw().Status(qctx)
		if err != nil {
			return err
		}
		st = parseStatus(dps)
		return nil
	})
	return st, err
}

// parseStatus derives a deviceStatus from a raw DP status map. Missing or
// unparseable DPs leave their fields at zero rather than failing the whole
// refresh — a scene-mode bulb, say, may not report a colour.
func parseStatus(dps map[string]json.RawMessage) deviceStatus {
	var st deviceStatus

	if raw, ok := dps[device.DPSwitch]; ok {
		_ = json.Unmarshal(raw, &st.On)
	}
	if mode, err := device.GetModeFrom(dps); err == nil {
		st.Mode = mode
	}
	if pct, err := device.GetBrightnessPercentFrom(dps); err == nil {
		st.BrightPct = pct
	}
	if pct, err := device.GetColourTempPercentFrom(dps); err == nil {
		st.TempPct = pct
		st.HasTemp = true
	}
	if h, s, _, err := device.ColourHSVFrom(dps); err == nil {
		st.Hue = h
		st.Sat = s
		st.HasColour = true
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
func (c *control) SetColour(ctx context.Context, rgb device.RGB) error {
	return c.withBulb(ctx, func(b *bulb.Bulb) error {
		cctx, cancel := context.WithTimeout(ctx, commandTimeout)
		defer cancel()
		return b.SetColour(cctx, rgb.R, rgb.G, rgb.B)
	})
}

// SetBrightnessPercent sets brightness (mode-aware: preserves hue/sat in
// colour mode, DP 22 in white mode).
func (c *control) SetBrightnessPercent(ctx context.Context, pct float64) error {
	return c.withBulb(ctx, func(b *bulb.Bulb) error {
		cctx, cancel := context.WithTimeout(ctx, commandTimeout)
		defer cancel()
		return b.SetBrightnessPercent(cctx, pct)
	})
}

// SetColourTempPercent switches to white mode and sets colour temperature as
// a cold↔warm percentage (the DP is a raw 0-1000 scale, not Kelvin).
func (c *control) SetColourTempPercent(ctx context.Context, pct float64) error {
	return c.withBulb(ctx, func(b *bulb.Bulb) error {
		cctx, cancel := context.WithTimeout(ctx, commandTimeout)
		defer cancel()
		return b.Raw().SetColourTempPercent(cctx, pct, true)
	})
}

// SetScene switches to one of the four built-in scenes (1-4). The device may
// reject scenes; callers should surface but not hard-fail on the error.
func (c *control) SetScene(ctx context.Context, scene int) error {
	return c.withBulb(ctx, func(b *bulb.Bulb) error {
		cctx, cancel := context.WithTimeout(ctx, commandTimeout)
		defer cancel()
		return b.Raw().SetScene(cctx, scene, true)
	})
}

// ApplyState drives the device to the state captured in a scene. An off state
// only cuts power; an on state restores mode + colour/temp first, then
// brightness last so the brightness write isn't stomped by the colour write.
// It composes the existing setters (each mutex-guarded) rather than holding a
// lock across the whole sequence.
func (c *control) ApplyState(ctx context.Context, st SceneState) error {
	if !st.On {
		return c.SetPower(ctx, false)
	}
	switch st.Mode {
	case device.ModeColour:
		r, g, b := hsvToRGBInt(st.Hue, st.Sat, 1.0)
		if err := c.SetColour(ctx, device.RGB{R: r, G: g, B: b}); err != nil {
			return err
		}
		return c.SetBrightnessPercent(ctx, st.Bright)
	case device.ModeWhite:
		if err := c.SetColourTempPercent(ctx, st.Temp); err != nil {
			return err
		}
		return c.SetBrightnessPercent(ctx, st.Bright)
	default:
		// Captured from a mode we don't model; just ensure it's on.
		return c.SetPower(ctx, true)
	}
}

// BeginLiveDrag closes the command session and opens a short-lived music-mode
// streamer seeded with the current colour, so wheel drags stream smoothly. It
// is a no-op if a drag is already in progress. Safe to call from the GTK
// thread — the streamer starts its own goroutines.
func (c *control) BeginLiveDrag(seed device.RGB) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.live != nil {
		return
	}
	// Hand the single session over to the streamer: close command mode first.
	if c.sess != nil {
		c.sess.Close()
		c.sess = nil
		c.bulb = nil
	}
	c.live = newStreamer([]Device{c.dev}, seed, bulb.StreamOptions{})
}

// UpdateLiveDrag streams a colour during a drag (newest-wins, non-blocking).
func (c *control) UpdateLiveDrag(rgb device.RGB) {
	c.mu.Lock()
	live := c.live
	c.mu.Unlock()
	if live != nil {
		live.Set(rgb, nil)
	}
}

// EndLiveDrag closes the streamer, which flushes the pending colour and leaves
// music mode with a normal SetColour so the final colour sticks; the command
// session then re-opens lazily on the next command. Runs the blocking Close
// off the caller's control path is the caller's responsibility.
func (c *control) EndLiveDrag() {
	c.mu.Lock()
	live := c.live
	c.live = nil
	c.mu.Unlock()
	if live != nil {
		live.Close()
	}
}
