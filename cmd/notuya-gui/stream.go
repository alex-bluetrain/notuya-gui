package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/alex-bluetrain/notuya-go/pkg/bulb"
	"github.com/alex-bluetrain/notuya-go/pkg/device"
	"github.com/alex-bluetrain/notuya-go/pkg/protocol35"
)

// commandTimeout bounds one device's open handshake and the final
// leave-music-mode write, mirroring the CLI.
const commandTimeout = 10 * time.Second

// streamer owns one live session per configured device and drives them
// in-process, exactly as the CLI's `music` command does over stdin — minus
// the HTTP hop. Each device runs bulb.StreamColours on its own goroutine,
// fed by a buffered(1) newest-wins channel so a slow bulb never stalls the
// GUI thread.
type streamer struct {
	targets []*target
	wg      sync.WaitGroup
	cancel  context.CancelFunc
}

type target struct {
	name    string
	colours chan bulb.StreamColour
	power   chan bool
}

// newStreamer starts a streaming goroutine per device, seeded with the
// initial colour so music mode is entered without a flicker. opts carries
// the default transition and interval.
func newStreamer(devices []Device, initial device.RGB, opts bulb.StreamOptions) *streamer {
	ctx, cancel := context.WithCancel(context.Background())
	s := &streamer{cancel: cancel}

	for _, d := range devices {
		name := d.Name
		if name == "" {
			name = d.DeviceID
		}
		t := &target{
			name:    name,
			colours: make(chan bulb.StreamColour, 1),
			power:   make(chan bool, 1),
		}
		// Seed the initial colour so the first send enters music mode
		// on the colour the wheel already shows.
		t.colours <- bulb.StreamColour{RGB: initial}
		s.targets = append(s.targets, t)

		s.wg.Add(1)
		go func(d Device, t *target) {
			defer s.wg.Done()
			if err := streamDevice(ctx, d, t, opts); err != nil {
				fmt.Fprintf(os.Stderr, "notuya-gui: %s -> %v\n", t.name, err)
				// Drain so a dead device cannot block the UI thread on
				// a full channel.
				for range t.colours {
				}
			}
		}(d, t)
	}
	return s
}

// Set broadcasts a colour (with optional per-colour transition) to every
// device, newest-wins. Never blocks the caller.
func (s *streamer) Set(rgb device.RGB, transition *int) {
	for _, t := range s.targets {
		offer(t.colours, bulb.StreamColour{RGB: rgb, Transition: transition})
	}
}

// SetPower toggles every device on or off. Non-blocking: the request is
// coalesced onto each device's power channel and applied by its goroutine,
// serialized with streaming so the two never race on one session.
func (s *streamer) SetPower(on bool) {
	for _, t := range s.targets {
		offer(t.power, on)
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

// streamDevice opens one session and streams colours to it for the whole
// run, then leaves the bulb on the last colour it received. It mirrors the
// CLI's streamDevice, adding a power channel that toggles the bulb inline so
// power changes and colour writes share the one session safely.
func streamDevice(ctx context.Context, d Device, t *target, opts bulb.StreamOptions) error {
	sess := protocol35.NewSession(d.IPAddress, []byte(d.LocalKey))

	openCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	if err := sess.Open(openCtx); err != nil {
		return err
	}
	defer sess.Close()

	b := bulb.NewBulb(sess, t.name)

	// Tap the colour stream to remember the last colour so the bulb can be
	// left holding it. Power requests are interleaved here, applied
	// serially with colour sends since both touch the one session.
	var (
		last    bulb.StreamColour
		haveOne bool
	)
	tapped := make(chan bulb.StreamColour, 1)
	go func() {
		defer close(tapped)
		for {
			select {
			case c, ok := <-t.colours:
				if !ok {
					return
				}
				last, haveOne = c, true
				// Newest-wins hand-off: never park holding a stale colour,
				// or a fast slider defeats the library's coalescing and the
				// bulb trails behind a backlog of intermediate values.
				offer(tapped, c)
			case on := <-t.power:
				pctx, pcancel := context.WithTimeout(context.Background(), commandTimeout)
				var err error
				if on {
					err = b.TurnOn(pctx)
					// Re-apply the current colour so turning on restores it.
					if err == nil && haveOne {
						offer(tapped, last)
					}
				} else {
					err = b.TurnOff(pctx)
				}
				pcancel()
				if err != nil {
					fmt.Fprintf(os.Stderr, "notuya-gui: %s power -> %v\n", t.name, err)
				}
			}
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
	if err := b.SetColour(finalCtx, last.RGB.R, last.RGB.G, last.RGB.B); err != nil {
		return fmt.Errorf("leaving music mode: %w", err)
	}
	return nil
}
