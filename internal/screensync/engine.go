package screensync

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/alex-bluetrain/notuya-go/pkg/dp"

	"github.com/alex-bluetrain/notuya-gui/internal/colour"
)

// LightSink is one bulb as the engine sees it. The app implements it with
// its shared per-device control, so sync never opens a session of its own.
type LightSink interface {
	// Live hands the bulb a frame's colour. It must not block: the bulb's
	// own writer sends it on DP 28; it stays unsaved (Stop does not save it).
	Live(c dp.HSV, mode dp.ChangeMode)
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
	Mapping    Mapping
	// OnFrame receives each region's colour (after brightness). It runs on
	// an engine goroutine; hop to the GTK thread before touching widgets.
	OnFrame func([]dp.HSV)
	// OnStop runs once if the engine stops by itself (source gone or
	// broken), with the reason. It is not called after Stop.
	OnStop func(error)
}

// Mapping is how a region's screen colour becomes a bulb colour.
type Mapping int

const (
	// MapLight decodes the screen's sRGB to linear light first, so the
	// bulb gives off what that part of the screen does: dark stays dark.
	MapLight Mapping = iota
	// MapValues sends the screen's sRGB values as they are: the bulb's
	// HSV is a colour picker's for that pixel. Dark colours glow brighter
	// and paler than the screen.
	MapValues
)

// minV is the dimmest colour value a bulb still lights at (1 %).
const minV = 10

const (
	frameTimeout   = 250 * time.Millisecond
	liveChangeMode = dp.ChangeJump
)

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
	mapping    Mapping
	rows       [][]byte // latest row per stream (nil until its first frame)
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
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{
		opts:       opts,
		ctx:        ctx,
		cancel:     cancel,
		brightness: opts.Brightness,
		mapping:    opts.Mapping,
		rows:       make([][]byte, len(opts.Sources)),
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

// SetRegions swaps regions and bindings live (used while editing). A light
// no longer bound keeps its last colour.
func (e *Engine) SetRegions(regions []Region) {
	regions = regions[:min(len(regions), MaxRegions)]
	rects := make([]Rect, len(regions))
	bindings := make([][]LightSink, len(regions))
	for i, r := range regions {
		rects[i] = r.Rect
		bindings[i] = r.Lights
	}

	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return
	}
	e.rects, e.bindings = rects, bindings
	e.mu.Unlock()

	for _, s := range e.streams {
		s.setRegions(rects)
	}
}

// SetBrightness changes the brightness multiplier live.
func (e *Engine) SetBrightness(f float64) {
	e.mu.Lock()
	e.brightness = f
	e.mu.Unlock()
}

// SetMapping changes how screen colours map to bulb colours, live.
func (e *Engine) SetMapping(m Mapping) {
	e.mu.Lock()
	e.mapping = m
	e.mu.Unlock()
}

// Stop ends the sync. Lights keep showing their last colour. It blocks until the capture pipelines are down: never call it on
// the GTK thread.
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

// decode maps an 8-bit sRGB value to colour.FromRGB's 0–1 input, per
// Mapping: linear light (IEC 61966-2-1) or the value as is.
var decode = func() (t [2][256]float64) {
	for i := range 256 {
		c := float64(i) / 255
		t[MapValues][i] = c
		if c <= 0.04045 {
			t[MapLight][i] = c / 12.92
		} else {
			t[MapLight][i] = math.Pow((c+0.055)/1.055, 2.4)
		}
	}
	return t
}()

// frame stores stream i's row and pushes the combined colours. A region
// spanning several sources is the mean of each source's average, weighted
// by how much of the region that source covers.
func (e *Engine) frame(i int, row []byte) {
	e.mu.Lock()
	if e.rows[i] == nil {
		e.rows[i] = make([]byte, len(row))
	}
	copy(e.rows[i], row)
	colours := make([]dp.HSV, len(e.rects))
	dec := &decode[e.mapping]
	for r := range e.rects {
		var sum [3]float64
		var total float64
		for si, s := range e.streams {
			if e.rows[si] == nil {
				continue
			}
			w := s.weights(e.rects[r : r+1])[0]
			for c := range 3 {
				sum[c] += dec[e.rows[si][r*4+c]] * w
			}
			total += w
		}
		if total > 0 {
			// Stay in floats down to the bulb's 0–1000 scale: the mean
			// of many pixels is finer than one 8-bit step.
			f := e.brightness / total
			c := colour.FromRGB(sum[0]*f, sum[1]*f, sum[2]*f)
			// Bulbs stay dark below V 10; keep the lamp lit on black.
			c.V = max(c.V, minV)
			colours[r] = c
		}
	}
	type send struct {
		l LightSink
		c dp.HSV
	}
	var sends []send
	for r, ls := range e.bindings {
		for _, l := range ls {
			sends = append(sends, send{l, colours[r]})
		}
	}
	e.mu.Unlock()

	for _, s := range sends {
		s.l.Live(s.c, liveChangeMode)
	}
	if e.opts.OnFrame != nil {
		e.opts.OnFrame(colours)
	}
}
