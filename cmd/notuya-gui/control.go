package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/alex-bluetrain/notuya-go/pkg/bulb"
	"github.com/alex-bluetrain/notuya-go/pkg/device"
	"github.com/alex-bluetrain/notuya-go/pkg/protocol"
	"github.com/alex-bluetrain/notuya-go/pkg/protocol35"
)

// errLiveStream is returned by withBulb while a music-mode stream owns the
// bulb: opening a command session beside it would give the bulb two sessions,
// and Tuya bulbs drop the older one. Callers that need the bulb mid-stream use
// UpdateLiveDrag instead.
var errLiveStream = errors.New("live stream active, command refused")

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
// concurrency-safe. The app issues discrete waited commands over this session
// and only borrows music mode (the streamer) during a live colour drag.
//
// It is the bulb's single channel for the whole app: every tab (Lights,
// Scenes, the scene editor, Settings' Test) must drive a device through the
// one control the app keys by device_id, never a private session. A Tuya bulb
// tolerates one connection at a time and silently drops the older one, so a
// second session anywhere kills the shared one for every other tab.
type control struct {
	dev Device

	// dial builds a session for dev; a test swaps in a fake. Defaults to
	// protocol35.NewSession.
	dial func(addr string, localKey []byte) protocol.Session

	mu   sync.Mutex
	sess protocol.Session
	bulb *bulb.Bulb

	// live is a short-lived music-mode streamer borrowed for a colour drag.
	// While non-nil the command session is closed so the bulb only ever has
	// one session open at a time.
	live *streamer

	// liveMode is the DP 28 change mode (jump/fade) sent with every
	// streamed colour during the current drag. Set by BeginLiveDrag.
	liveMode device.ChangeMode
}

func newControl(d Device) *control {
	return &control{dev: d, dial: dialProtocol35}
}

func dialProtocol35(addr string, localKey []byte) protocol.Session {
	return protocol35.NewSession(addr, localKey)
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
	sess := c.dial(c.dev.IPAddress, []byte(c.dev.LocalKey))
	openCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	if err := sess.Open(openCtx); err != nil {
		return fmt.Errorf("%s: opening session: %w", c.name(), err)
	}
	c.sess = sess
	c.bulb = bulb.NewBulb(sess, c.name())
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

// Close tears down the session. Safe to call more than once.
func (c *control) Close() {
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
	if h, s, v, err := device.ColourHSVFrom(dps); err == nil {
		st.Hue = h
		st.Sat = s
		st.HasColour = true
		// In colour mode brightness is DP 24's V component, not the white-mode
		// brightness DP (which goes stale when the bulb leaves white mode).
		if st.Mode == device.ModeColour {
			st.BrightPct = v * 100
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
func (c *control) SetColour(ctx context.Context, rgb device.RGB) error {
	return c.withBulb(ctx, func(b *bulb.Bulb) error {
		cctx, cancel := context.WithTimeout(ctx, commandTimeout)
		defer cancel()
		return b.SetColour(cctx, rgb.R, rgb.G, rgb.B)
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
		return b.Raw().SetColourTempPercent(cctx, pct, true)
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
	case device.ModeColour:
		r, g, b := hsvToRGBInt(st.Hue, st.Sat, st.Bright/100.0)
		return c.SetColour(ctx, device.RGB{R: r, G: g, B: b})
	case device.ModeWhite:
		if err := c.SetColourTempPercent(ctx, st.Temp); err != nil {
			return err
		}
		return c.SetWhiteBrightness(ctx, st.Bright)
	default:
		// Captured from a mode we don't model; the switch is already on.
		return nil
	}
}

// BeginLiveDrag closes the command session and opens a short-lived music-mode
// streamer seeded with the current colour, so wheel drags stream smoothly. It
// is a no-op if a drag is already in progress. Safe to call from the GTK
// thread — the streamer starts its own goroutines.
func (c *control) BeginLiveDrag(seed device.RGB, mode device.ChangeMode) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.live != nil {
		return
	}
	// Hand the single session over to the streamer: close command mode first.
	c.closeLocked()
	c.liveMode = mode
	c.live = newStreamer([]Device{c.dev}, seed, bulb.StreamOptions{ChangeMode: mode.Ptr()})
}

// SetLiveChangeMode changes the jump/fade mode applied to subsequent
// UpdateLiveDrag sends without restarting the stream, so the toggle takes
// effect mid-drag.
func (c *control) SetLiveChangeMode(mode device.ChangeMode) {
	c.mu.Lock()
	c.liveMode = mode
	c.mu.Unlock()
}

// UpdateLiveDrag streams a colour during a drag (newest-wins, non-blocking),
// carrying the drag's current change mode so it takes effect live.
func (c *control) UpdateLiveDrag(rgb device.RGB) {
	c.mu.Lock()
	live := c.live
	mode := c.liveMode
	c.mu.Unlock()
	if live != nil {
		live.Set(rgb, mode.Ptr())
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
