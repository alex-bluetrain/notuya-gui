package screensync

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Monitor is a connected output in compositor layout (logical) pixels.
type Monitor struct {
	Name       string
	X, Y, W, H int
}

// Window is a top-level window as the compositor reports it.
type Window struct {
	Class, Title string
	X, Y, W, H   int // layout pixels
	Fullscreen   bool
	Monitor      string
	recent       int // focus history: 0 = most recently focused
}

// Placement is where a target currently sits.
type Placement struct {
	Gone   bool    // the window is closed (or a monitor unplugged)
	Canvas Monitor // the target's bounding box in layout pixels (Name unused)
	// Monitors are the outputs the canvas overlaps, for the overlay.
	Monitors []Monitor
}

// Tracker answers where targets are. v1 has a Hyprland implementation
// and a stub for the test backend.
type Tracker interface {
	Monitors() ([]Monitor, error)
	Windows() ([]Window, error)
	// Watch reports t's placement now and on every change until ctx ends.
	// poll, when non-zero, also re-checks on a timer: Hyprland sends no
	// event for a floating window being moved or resized by drag.
	Watch(ctx context.Context, t Target, poll time.Duration, f func(Placement))
}

var ErrNoHyprland = errors.New("screen sync requires Hyprland")

// NewTracker returns the compositor tracker, or ErrNoHyprland.
func NewTracker() (Tracker, error) {
	if testBackend() {
		return stubTracker{}, nil
	}
	sig := os.Getenv("HYPRLAND_INSTANCE_SIGNATURE")
	if sig == "" {
		return nil, ErrNoHyprland
	}
	dir := filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "hypr", sig)
	if _, err := os.Stat(filepath.Join(dir, ".socket.sock")); err != nil {
		return nil, ErrNoHyprland
	}
	return &hyprland{dir: dir}, nil
}

// MatchWindow picks the window t means: same class, title containing
// TitleMatch (case-insensitive), most recently focused first.
func MatchWindow(t Target, ws []Window) (Window, bool) {
	best, found := Window{}, false
	for _, w := range ws {
		if w.Class != t.WindowClass {
			continue
		}
		if t.TitleMatch != "" && !strings.Contains(strings.ToLower(w.Title), strings.ToLower(t.TitleMatch)) {
			continue
		}
		if !found || w.recent < best.recent {
			best, found = w, true
		}
	}
	return best, found
}

// place computes t's placement from the compositor state.
func place(t Target, mons []Monitor, wins []Window) Placement {
	var c Monitor
	switch t.Kind {
	case TargetWindow:
		w, ok := MatchWindow(t, wins)
		if !ok {
			return Placement{Gone: true}
		}
		c = Monitor{X: w.X, Y: w.Y, W: w.W, H: w.H}
		if m, ok := monitorNamed(mons, w.Monitor); ok && w.Fullscreen {
			c = Monitor{X: m.X, Y: m.Y, W: m.W, H: m.H}
		}
	default:
		first := true
		for _, n := range t.Monitors {
			m, ok := monitorNamed(mons, n)
			if !ok {
				return Placement{Gone: true}
			}
			if first {
				c, first = Monitor{X: m.X, Y: m.Y, W: m.W, H: m.H}, false
				continue
			}
			x1, y1 := max(c.X+c.W, m.X+m.W), max(c.Y+c.H, m.Y+m.H)
			c.X, c.Y = min(c.X, m.X), min(c.Y, m.Y)
			c.W, c.H = x1-c.X, y1-c.Y
		}
		if first {
			return Placement{Gone: true}
		}
	}
	p := Placement{Canvas: c}
	for _, m := range mons {
		if m.X < c.X+c.W && c.X < m.X+m.W && m.Y < c.Y+c.H && c.Y < m.Y+m.H {
			p.Monitors = append(p.Monitors, m)
		}
	}
	return p
}

func samePlacement(a, b Placement) bool {
	if a.Gone != b.Gone || a.Canvas != b.Canvas || len(a.Monitors) != len(b.Monitors) {
		return false
	}
	for i := range a.Monitors {
		if a.Monitors[i] != b.Monitors[i] {
			return false
		}
	}
	return true
}

// hyprland talks to Hyprland's IPC sockets directly (no hyprctl).
type hyprland struct{ dir string }

func (h *hyprland) request(cmd string, out any) error {
	c, err := net.DialTimeout("unix", filepath.Join(h.dir, ".socket.sock"), time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(c, cmd); err != nil {
		return err
	}
	b, err := io.ReadAll(c)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("hyprland %s: %w", cmd, err)
	}
	return nil
}

type hyprMonitor struct {
	ID        int     `json:"id"`
	Name      string  `json:"name"`
	X         int     `json:"x"`
	Y         int     `json:"y"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	Scale     float64 `json:"scale"`
	Transform int     `json:"transform"`
	Disabled  bool    `json:"disabled"`
}

func (h *hyprland) monitors() ([]Monitor, map[int]string, error) {
	var raw []hyprMonitor
	if err := h.request("j/monitors", &raw); err != nil {
		return nil, nil, err
	}
	var out []Monitor
	ids := map[int]string{}
	for _, m := range raw {
		if m.Disabled {
			continue
		}
		w, hh := m.Width, m.Height
		if m.Transform%2 == 1 {
			w, hh = hh, w
		}
		if m.Scale > 0 {
			w, hh = int(float64(w)/m.Scale+0.5), int(float64(hh)/m.Scale+0.5)
		}
		out = append(out, Monitor{Name: m.Name, X: m.X, Y: m.Y, W: w, H: hh})
		ids[m.ID] = m.Name
	}
	return out, ids, nil
}

func (h *hyprland) Monitors() ([]Monitor, error) {
	m, _, err := h.monitors()
	return m, err
}

type hyprClient struct {
	Mapped         bool   `json:"mapped"`
	Hidden         bool   `json:"hidden"`
	At             [2]int `json:"at"`
	Size           [2]int `json:"size"`
	Monitor        int    `json:"monitor"`
	Class          string `json:"class"`
	Title          string `json:"title"`
	Fullscreen     int    `json:"fullscreen"`
	FocusHistoryID int    `json:"focusHistoryID"`
}

func (h *hyprland) windows(ids map[int]string) ([]Window, error) {
	var raw []hyprClient
	if err := h.request("j/clients", &raw); err != nil {
		return nil, err
	}
	var out []Window
	for _, c := range raw {
		if !c.Mapped || c.Hidden || c.Class == "" {
			continue
		}
		out = append(out, Window{
			Class: c.Class, Title: c.Title,
			X: c.At[0], Y: c.At[1], W: c.Size[0], H: c.Size[1],
			Fullscreen: c.Fullscreen > 0, Monitor: ids[c.Monitor], recent: c.FocusHistoryID,
		})
	}
	return out, nil
}

func (h *hyprland) Windows() ([]Window, error) {
	_, ids, err := h.monitors()
	if err != nil {
		return nil, err
	}
	return h.windows(ids)
}

func (h *hyprland) placement(t Target) (Placement, error) {
	mons, ids, err := h.monitors()
	if err != nil {
		return Placement{}, err
	}
	var wins []Window
	if t.Kind == TargetWindow {
		if wins, err = h.windows(ids); err != nil {
			return Placement{}, err
		}
	}
	return place(t, mons, wins), nil
}

// events streams Hyprland's event socket, sending one tick per line
// (coalesced) until ctx ends. It reconnects if the socket drops.
func (h *hyprland) events(ctx context.Context, tick chan<- struct{}) {
	for ctx.Err() == nil {
		var d net.Dialer
		c, err := d.DialContext(ctx, "unix", filepath.Join(h.dir, ".socket2.sock"))
		if err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			continue
		}
		stop := context.AfterFunc(ctx, func() { c.Close() })
		sc := bufio.NewScanner(c)
		for sc.Scan() {
			select {
			case tick <- struct{}{}:
			default:
			}
		}
		stop()
		c.Close()
	}
}

func (h *hyprland) Watch(ctx context.Context, t Target, poll time.Duration, f func(Placement)) {
	tick := make(chan struct{}, 1)
	go h.events(ctx, tick)
	var timer <-chan time.Time
	if poll > 0 {
		tk := time.NewTicker(poll)
		defer tk.Stop()
		timer = tk.C
	}
	var last Placement
	first := true
	check := func() {
		p, err := h.placement(t)
		if err != nil {
			return
		}
		if first || !samePlacement(p, last) {
			first, last = false, p
			f(p)
		}
	}
	check()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			check()
		case <-timer:
			check()
		}
	}
}

// stubTracker serves a fixed 1920×1080 layout for the test backend.
type stubTracker struct{}

var stubMonitors = []Monitor{{Name: "TEST-1", W: 1920, H: 1080}, {Name: "TEST-2", X: 1920, W: 1920, H: 1080}}

func (stubTracker) Monitors() ([]Monitor, error) { return stubMonitors, nil }
func (stubTracker) Windows() ([]Window, error) {
	return []Window{{Class: "test.game", Title: "Test Game", X: 100, Y: 100, W: 1280, H: 720, Monitor: "TEST-1"}}, nil
}

func (s stubTracker) Watch(ctx context.Context, t Target, _ time.Duration, f func(Placement)) {
	ws, _ := s.Windows()
	f(place(t, stubMonitors, ws))
	<-ctx.Done()
}
