package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/alex-bluetrain/notuya-go/pkg/bulb"
	"github.com/alex-bluetrain/notuya-go/pkg/dp"
)

// commandTimeout bounds one device's open handshake and the final
// leave-music-mode write.
const commandTimeout = 10 * time.Second

// streamer owns one live session per configured device and drives them
// in-process. Each device runs bulb.StreamColours on its own goroutine,
// fed by a buffered(1) newest-wins channel so a slow bulb never stalls the
// GUI thread.
type streamer struct {
	targets []*target
	wg      sync.WaitGroup
	cancel  context.CancelFunc

	// dead is closed when any device's stream ends on an error (session
	// dropped, bulb unreachable). A colour drag lasts seconds and ignores it;
	// Screen Sync runs for hours and restarts the stream.
	dead     chan struct{}
	deadOnce sync.Once
}

type target struct {
	name    string
	colours chan bulb.StreamColour
}

// newStreamer starts a streaming goroutine per device, seeded with the
// initial colour so music mode is entered without a flicker. opts carries
// the default change mode and interval; dial opens each device's session.
func newStreamer(devices []Device, initial dp.RGB, opts bulb.StreamOptions, dial dialFunc) *streamer {
	ctx, cancel := context.WithCancel(context.Background())
	s := &streamer{cancel: cancel, dead: make(chan struct{})}

	for _, d := range devices {
		name := d.Name
		if name == "" {
			name = d.DeviceID
		}
		t := &target{
			name:    name,
			colours: make(chan bulb.StreamColour, 1),
		}
		// Seed the initial colour so the first send enters music mode
		// on the colour the wheel already shows.
		t.colours <- bulb.StreamColour{RGB: initial}
		s.targets = append(s.targets, t)

		s.wg.Add(1)
		go func(d Device, t *target) {
			defer s.wg.Done()
			if err := streamDevice(ctx, dial, d, t, opts); err != nil {
				fmt.Fprintf(os.Stderr, "notuya-gui: %s -> %v\n", t.name, err)
				s.deadOnce.Do(func() { close(s.dead) })
				// Drain so a dead device cannot block the UI thread on
				// a full channel.
				for range t.colours {
				}
			}
		}(d, t)
	}
	return s
}

// Set broadcasts a colour (with optional per-colour change mode) to every
// device, newest-wins. Never blocks the caller.
func (s *streamer) Set(rgb dp.RGB, mode *dp.ChangeMode) {
	for _, t := range s.targets {
		offer(t.colours, bulb.StreamColour{RGB: rgb, ChangeMode: mode})
	}
}

// Close stops streaming; each goroutine flushes its pending colour and
// leaves music mode with a normal SetColour so the final colour sticks.
func (s *streamer) Close() {
	for _, t := range s.targets {
		close(t.colours)
	}
	s.wg.Wait()
	s.cancel()
}

// offer delivers v to ch, discarding an undelivered older value if one is
// still queued (newest-wins). It never blocks.
func offer[T any](ch chan T, v T) {
	for {
		select {
		case ch <- v:
			return
		default:
		}
		select {
		case <-ch:
		default:
		}
	}
}

// streamDevice opens a session to d and streams colours to it for the whole
// run, then leaves the bulb on the last colour it received.
func streamDevice(ctx context.Context, dial dialFunc, d Device, t *target, opts bulb.StreamOptions) error {
	openCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	sess, err := dial(openCtx, d.IPAddress, []byte(d.LocalKey))
	if err != nil {
		return err
	}
	defer sess.Close()

	b := bulb.New(sess, t.name)

	// Tap the colour stream to remember the last colour so the bulb can be
	// left holding it.
	var (
		last    bulb.StreamColour
		haveOne bool
	)
	tapped := make(chan bulb.StreamColour, 1)
	go func() {
		defer close(tapped)
		for c := range t.colours {
			last, haveOne = c, true
			// Newest-wins hand-off: never park holding a stale colour, or a
			// fast slider defeats the library's coalescing and the bulb
			// trails behind a backlog of intermediate values.
			offer(tapped, c)
		}
	}()

	streamErr := b.StreamColours(ctx, tapped, opts)

	// Drain whatever is still queued so the tap goroutine cannot leak.
	for range tapped {
	}

	if streamErr != nil {
		return streamErr
	}
	if !haveOne {
		return nil
	}

	// Re-issue the final colour as a normal colour-mode write: it takes the
	// bulb out of music mode and makes the colour stick after we hang up. A
	// fresh context because ctx is cancelled on close.
	finalCtx, cancelFinal := context.WithTimeout(context.Background(), commandTimeout)
	defer cancelFinal()
	if err := b.SetColour(finalCtx, last.RGB); err != nil {
		return fmt.Errorf("leaving music mode: %w", err)
	}
	return nil
}
