package screensync

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/alex-bluetrain/notuya-go/pkg/dp"
)

// LightSink is one bulb as the engine sees it. The app implements it with
// its shared per-device control, so sync never opens a session of its own.
type LightSink interface {
	// Seed returns the bulb's current colour. It is called before
	// BeginLive because the bulb refuses status queries once streaming.
	Seed(ctx context.Context) dp.RGB
	BeginLive(owner any, seed dp.RGB, mode dp.ChangeMode) error
	UpdateLive(owner any, rgb dp.RGB)
	// EndLive blocks; the engine only calls it off the GTK thread.
	EndLive(owner any)
	Streaming() bool
	// Dead fires if the music session drops; nil while not streaming.
	Dead() <-chan struct{}
}

// Region is a canvas rectangle and the lights that follow its colour.
type Region struct {
	Rect   Rect
	Lights []LightSink
}

// Options configures an engine run.
type Options struct {
	Sources    []Source
	Regions    []Region
	Brightness float64 // multiplier, 1 = unchanged
	// OnFrame receives each region's colour (after brightness). It runs on
	// an engine goroutine; hop to the GTK thread before touching widgets.
	OnFrame func([]dp.RGB)
	// OnStop runs once if the engine stops by itself (source gone or
	// broken), with the reason. It is not called after Stop.
	OnStop func(error)
	// OnLightTrouble runs when a light has failed to reconnect 3 times in
	// a row, and again with ok=true once it recovers.
	OnLightTrouble func(l LightSink, ok bool)

	reconnect reconnect // zero = defaults; tests shorten it
}

const (
	frameTimeout   = 250 * time.Millisecond
	troubleAfter   = 3
	seedTimeout    = 3 * time.Second
	liveChangeMode = dp.ChangeJump
)

// Reconnect timing defaults.
const (
	defaultBackoffStart = time.Second
	defaultBackoffMax   = 30 * time.Second
	defaultHealthyAfter = 5 * time.Second
)

// reconnect is how a dropped light is retried: wait start, doubling up to
// max; a stream that stays up for healthy resets the count.
type reconnect struct{ start, max, healthy time.Duration }

// Engine runs one sync: GPU streams in, bulb colours out.
type Engine struct {
	opts    Options
	streams []*stream
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	mu         sync.Mutex
	rects      []Rect
	bindings   [][]LightSink
	brightness float64
	rows       [][]byte // latest row per stream (nil until its first frame)
	lights     map[LightSink]*lightRunner
	stopped    bool
}

// Start opens every source and begins streaming to the bound lights. It
// fails without touching any light if a source cannot start, so a missing
// GPU path is reported instead of silently degrading.
func Start(opts Options) (*Engine, error) {
	if len(opts.Sources) == 0 {
		return nil, errors.New("screensync: no sources")
	}
	if len(opts.Regions) > MaxRegions {
		return nil, errors.New("screensync: too many regions")
	}
	if opts.reconnect == (reconnect{}) {
		opts.reconnect = reconnect{defaultBackoffStart, defaultBackoffMax, defaultHealthyAfter}
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{
		opts:       opts,
		ctx:        ctx,
		cancel:     cancel,
		brightness: opts.Brightness,
		rows:       make([][]byte, len(opts.Sources)),
		lights:     map[LightSink]*lightRunner{},
	}
	for _, src := range opts.Sources {
		s, err := openStream(src)
		if err == nil {
			err = s.start()
			if err != nil {
				s.close()
			}
		}
		if err != nil {
			e.closeStreams()
			cancel()
			return nil, err
		}
		e.streams = append(e.streams, s)
	}
	e.SetRegions(opts.Regions)
	for i, s := range e.streams {
		e.wg.Add(1)
		go e.pump(i, s)
	}
	return e, nil
}

// SetRegions swaps regions and bindings live (used while editing). Lights
// no longer bound are released; new ones start streaming.
func (e *Engine) SetRegions(regions []Region) {
	regions = regions[:min(len(regions), MaxRegions)]
	rects := make([]Rect, len(regions))
	bindings := make([][]LightSink, len(regions))
	want := map[LightSink]bool{}
	for i, r := range regions {
		rects[i] = r.Rect
		bindings[i] = r.Lights
		for _, l := range r.Lights {
			want[l] = true
		}
	}

	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return
	}
	e.rects, e.bindings = rects, bindings
	var gone []*lightRunner
	for l, lr := range e.lights {
		if !want[l] {
			gone = append(gone, lr)
			delete(e.lights, l)
		}
	}
	for l := range want {
		if e.lights[l] == nil {
			lr := &lightRunner{e: e, l: l, done: make(chan struct{})}
			e.lights[l] = lr
			e.wg.Add(1)
			go lr.run()
		}
	}
	e.mu.Unlock()

	for _, s := range e.streams {
		s.setRegions(rects)
	}
	for _, lr := range gone {
		lr.release()
	}
}

// SetBrightness changes the brightness multiplier live.
func (e *Engine) SetBrightness(f float64) {
	e.mu.Lock()
	e.brightness = f
	e.mu.Unlock()
}

// Stop ends the sync. Lights keep their last colour. It blocks while each
// light leaves music mode: never call it on the GTK thread.
func (e *Engine) Stop() {
	e.shutdown()
	e.wg.Wait()
	e.closeStreams()
}

func (e *Engine) shutdown() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped {
		return false
	}
	e.stopped = true
	e.cancel()
	return true
}

func (e *Engine) closeStreams() {
	for _, s := range e.streams {
		s.close()
	}
	e.streams = nil
}

// pump reads frames from one stream until the engine stops or the source
// dies; a dying source stops the whole sync.
func (e *Engine) pump(i int, s *stream) {
	defer e.wg.Done()
	for e.ctx.Err() == nil {
		row, ok, err := s.next(frameTimeout)
		if err != nil {
			if e.shutdown() && e.opts.OnStop != nil {
				go func() {
					e.wg.Wait()
					e.closeStreams()
					e.opts.OnStop(err)
				}()
			}
			return
		}
		if ok {
			e.frame(i, row)
		}
	}
}

// frame stores stream i's row and pushes the combined colours. A region
// spanning several sources is the mean of each source's average, weighted
// by how much of the region that source covers.
func (e *Engine) frame(i int, row []byte) {
	e.mu.Lock()
	if e.rows[i] == nil {
		e.rows[i] = make([]byte, len(row))
	}
	copy(e.rows[i], row)
	colours := make([]dp.RGB, len(e.rects))
	for r := range e.rects {
		var sum [3]float64
		var total float64
		for si, s := range e.streams {
			if e.rows[si] == nil {
				continue
			}
			w := s.weights(e.rects[r : r+1])[0]
			for c := range 3 {
				sum[c] += float64(e.rows[si][r*4+c]) * w
			}
			total += w
		}
		if total > 0 {
			colours[r] = dp.RGB{
				R: scale(sum[0]/total, e.brightness),
				G: scale(sum[1]/total, e.brightness),
				B: scale(sum[2]/total, e.brightness),
			}
		}
	}
	type send struct {
		lr  *lightRunner
		rgb dp.RGB
	}
	var sends []send
	for r, ls := range e.bindings {
		for _, l := range ls {
			if lr := e.lights[l]; lr != nil {
				sends = append(sends, send{lr, colours[r]})
			}
		}
	}
	e.mu.Unlock()

	for _, s := range sends {
		s.lr.update(s.rgb)
	}
	if e.opts.OnFrame != nil {
		e.opts.OnFrame(colours)
	}
}

func scale(v, f float64) uint8 {
	return uint8(min(max(v*f+0.5, 0), 255))
}

// lightRunner keeps one light streaming for the whole sync, reconnecting
// with backoff when its music session drops.
type lightRunner struct {
	e    *Engine
	l    LightSink
	done chan struct{} // closed when the light is unbound

	mu   sync.Mutex
	last dp.RGB
	have bool
}

func (lr *lightRunner) update(rgb dp.RGB) {
	lr.mu.Lock()
	lr.last, lr.have = rgb, true
	lr.mu.Unlock()
	lr.l.UpdateLive(lr.e, rgb)
}

func (lr *lightRunner) release() { close(lr.done) }

func (lr *lightRunner) run() {
	defer lr.e.wg.Done()
	defer lr.l.EndLive(lr.e)

	sctx, cancel := context.WithTimeout(lr.e.ctx, seedTimeout)
	seed := lr.l.Seed(sctx)
	cancel()
	lr.mu.Lock()
	if !lr.have {
		lr.last = seed
	}
	lr.mu.Unlock()

	rc := lr.e.opts.reconnect
	backoff := rc.start
	failures := 0
	for {
		lr.mu.Lock()
		colour := lr.last
		lr.mu.Unlock()
		err := lr.l.BeginLive(lr.e, colour, liveChangeMode)
		var dead <-chan struct{}
		if err == nil {
			dead = lr.l.Dead()
		}
		if dead != nil {
			// The session dials asynchronously, so BeginLive succeeding
			// proves nothing: only a stream that stays up counts as healthy.
			healthy := time.NewTimer(rc.healthy)
		alive:
			for {
				select {
				case <-healthy.C:
					if failures >= troubleAfter && lr.e.opts.OnLightTrouble != nil {
						lr.e.opts.OnLightTrouble(lr.l, true)
					}
					failures, backoff = 0, rc.start
				case <-dead:
					healthy.Stop()
					break alive
				case <-lr.done:
					healthy.Stop()
					return
				case <-lr.e.ctx.Done():
					healthy.Stop()
					return
				}
			}
			lr.l.EndLive(lr.e)
		}
		failures++
		if failures == troubleAfter && lr.e.opts.OnLightTrouble != nil {
			lr.e.opts.OnLightTrouble(lr.l, false)
		}
		select {
		case <-time.After(backoff):
		case <-lr.done:
			return
		case <-lr.e.ctx.Done():
			return
		}
		backoff = min(backoff*2, rc.max)
	}
}
