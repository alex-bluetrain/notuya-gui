package screensync

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
)

// ErrNoDMABuf means the capture offers no DMA-BUF format the GPU path
// accepts. Sync refuses to start rather than copying frames on the CPU.
var ErrNoDMABuf = errors.New("GPU capture (DMA-BUF) unavailable")

// ErrPickCancelled means the user dismissed the system share picker.
var ErrPickCancelled = errors.New("screen share cancelled")

const (
	portalDest = "org.freedesktop.portal.Desktop"
	portalPath = dbus.ObjectPath("/org/freedesktop/portal/desktop")
	screenCast = "org.freedesktop.portal.ScreenCast"
	// probeFor is how long a DMA-BUF format gets to prove it negotiates.
	probeFor = 3 * time.Second
)

// portalCapture is one ScreenCast portal session per monitor (xdph hands
// out one source per request) or one for a window, all on a private
// D-Bus connection: closing it ends every session.
type portalCapture struct {
	conn   *dbus.Conn
	srcs   []Source
	tokens map[string]string
	fds    []int
}

func openPortalCapture(ctx context.Context, t Target, opts CaptureOptions) (_ Capture, err error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, err
	}
	c := &portalCapture{conn: conn, tokens: map[string]string{}}
	defer func() {
		if err != nil {
			c.Close()
		}
	}()

	keys, canvas := []string{WindowTokenKey}, []Rect{{0, 0, 1, 1}}
	if t.Kind == TargetMonitors {
		tr, err := NewTracker()
		if err != nil {
			return nil, err
		}
		mons, err := tr.Monitors()
		if err != nil {
			return nil, err
		}
		if canvas, err = canvasOf(t.Monitors, mons); err != nil {
			return nil, err
		}
		keys = t.Monitors
	}
	for i, key := range keys {
		frag, err := c.open(ctx, t.Kind, key, opts)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		c.srcs = append(c.srcs, Source{Fragment: frag, Canvas: canvas[i]})
	}
	return c, nil
}

func (c *portalCapture) Sources() []Source         { return c.srcs }
func (c *portalCapture) Tokens() map[string]string { return c.tokens }

func (c *portalCapture) Close() {
	for _, fd := range c.fds {
		syscall.Close(fd)
	}
	c.fds = nil
	c.conn.Close()
}

// open runs one CreateSession → SelectSources → Start → OpenPipeWireRemote
// round and returns the GStreamer fragment for the resulting stream.
func (c *portalCapture) open(ctx context.Context, kind, key string, opts CaptureOptions) (string, error) {
	res, err := c.request(ctx, "CreateSession", nil, map[string]dbus.Variant{
		"session_handle_token": dbus.MakeVariant(uniqueToken()),
	})
	if err != nil {
		return "", err
	}
	var sess dbus.ObjectPath
	if s, ok := res["session_handle"].Value().(string); ok {
		sess = dbus.ObjectPath(s)
	} else {
		return "", errors.New("portal: no session handle")
	}

	types := uint32(1) // monitor
	if kind == TargetWindow {
		types = 2
	}
	sel := map[string]dbus.Variant{
		"types":        dbus.MakeVariant(types),
		"multiple":     dbus.MakeVariant(false),
		"cursor_mode":  dbus.MakeVariant(uint32(1)), // hidden: keep the pointer out of the average
		"persist_mode": dbus.MakeVariant(uint32(2)), // until revoked
	}
	tok := opts.Tokens[key]
	if tok != "" {
		sel["restore_token"] = dbus.MakeVariant(tok)
	} else if opts.OnPick != nil {
		opts.OnPick(key)
	}
	if _, err := c.request(ctx, "SelectSources", []any{sess}, sel); err != nil {
		return "", err
	}
	res, err = c.request(ctx, "Start", []any{sess, ""}, map[string]dbus.Variant{})
	if err != nil {
		return "", err
	}
	if s, ok := res["restore_token"].Value().(string); ok && s != "" {
		c.tokens[key] = s
	}
	var streams []struct {
		Node  uint32
		Props map[string]dbus.Variant
	}
	if v, ok := res["streams"]; !ok || dbus.Store([]any{v.Value()}, &streams) != nil || len(streams) == 0 {
		return "", errors.New("portal: no stream")
	}
	node := streams[0].Node

	var fd dbus.UnixFD
	if err := c.conn.Object(portalDest, portalPath).CallWithContext(ctx, screenCast+".OpenPipeWireRemote", 0,
		sess, map[string]dbus.Variant{}).Store(&fd); err != nil {
		return "", err
	}
	c.fds = append(c.fds, int(fd))

	formats, err := nodeFormats(ctx, node)
	if err != nil {
		return "", err
	}
	for _, f := range formats {
		if probeFormat(int(fd), node, f) {
			pfd, err := syscall.Dup(int(fd))
			if err != nil {
				return "", err
			}
			return pipewireFragment(pfd, node, f), nil
		}
	}
	return "", ErrNoDMABuf
}

// pipewireFragment hands pipewiresrc its own fd (it takes ownership) and
// pins DMA-BUF caps: free negotiation silently falls back to SHM.
func pipewireFragment(fd int, node uint32, drmFormat string) string {
	return fmt.Sprintf("pipewiresrc fd=%d path=%d always-copy=false ! "+
		`capsfilter caps="video/x-raw(memory:DMABuf),format=DMA_DRM,drm-format=%s"`, fd, node, drmFormat)
}

// probeFormat reports whether drmFormat negotiates all the way into
// glupload: a frame arrives, or nothing fails within probeFor (a static
// window may not redraw).
func probeFormat(fd int, node uint32, drmFormat string) bool {
	pfd, err := syscall.Dup(fd)
	if err != nil {
		return false
	}
	pl, err := parsePipeline(pipewireFragment(pfd, node, drmFormat) +
		" ! glupload ! glcolorconvert ! video/x-raw(memory:GLMemory),format=RGBA"+
			" ! appsink name=probe sync=false max-buffers=1 drop=true")
	if err != nil {
		syscall.Close(pfd)
		return false
	}
	defer pl.free()
	defer pl.stop()
	sink, err := pl.element("probe")
	if err != nil {
		return false
	}
	defer unref(sink)
	if pl.play() != nil {
		return false
	}
	for deadline := time.Now().Add(probeFor); time.Now().Before(deadline); {
		if pl.busProblem() != nil {
			return false
		}
		if got, err := waitSample(sink, 100*time.Millisecond); err != nil {
			return false
		} else if got {
			return true
		}
	}
	return pl.busProblem() == nil
}

// request calls a portal method and waits for its Request.Response.
func (c *portalCapture) request(ctx context.Context, method string, args []any, opts map[string]dbus.Variant) (map[string]dbus.Variant, error) {
	handle := uniqueToken()
	opts["handle_token"] = dbus.MakeVariant(handle)
	sender := strings.ReplaceAll(strings.TrimPrefix(c.conn.Names()[0], ":"), ".", "_")
	path := dbus.ObjectPath(fmt.Sprintf("/org/freedesktop/portal/desktop/request/%s/%s", sender, handle))

	match := []dbus.MatchOption{dbus.WithMatchObjectPath(path), dbus.WithMatchInterface("org.freedesktop.portal.Request"), dbus.WithMatchMember("Response")}
	if err := c.conn.AddMatchSignal(match...); err != nil {
		return nil, err
	}
	defer c.conn.RemoveMatchSignal(match...)
	sig := make(chan *dbus.Signal, 4)
	c.conn.Signal(sig)
	defer c.conn.RemoveSignal(sig)

	if err := c.conn.Object(portalDest, portalPath).CallWithContext(ctx, screenCast+"."+method, 0,
		append(args, opts)...).Err; err != nil {
		return nil, err
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case s, ok := <-sig:
			if !ok {
				return nil, errors.New("portal: connection closed")
			}
			if s.Path != path || len(s.Body) < 2 {
				continue
			}
			code, _ := s.Body[0].(uint32)
			res, _ := s.Body[1].(map[string]dbus.Variant)
			switch code {
			case 0:
				return res, nil
			case 1:
				return nil, ErrPickCancelled
			default:
				return nil, fmt.Errorf("portal: %s failed", method)
			}
		}
	}
}

var tokenSeq int

func uniqueToken() string {
	tokenSeq++
	return fmt.Sprintf("notuya%d_%d", time.Now().UnixNano()%1e9, tokenSeq)
}

// spaFourcc maps PipeWire video formats to DRM fourcc names.
var spaFourcc = map[string]string{
	"BGRx": "XR24", "BGRA": "AR24", "RGBx": "XB24", "RGBA": "AB24",
	"xRGB": "BX24", "ARGB": "BA24", "xBGR": "RX24", "ABGR": "RA24",
	"BGR10x2_LE": "XR30", "BGR10A2_LE": "AR30", "RGB10x2_LE": "XB30", "RGB10A2_LE": "AB30",
}

// nodeFormats lists the DMA-BUF formats (FOURCC:0xMODIFIER) the PipeWire
// node offers, in its preference order. pipewiresrc's caps read ANY
// until connected, so the node's own params are the reliable source.
func nodeFormats(ctx context.Context, node uint32) ([]string, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "pw-dump", fmt.Sprint(node)).Output()
	if err != nil {
		return nil, fmt.Errorf("pw-dump: %w", err)
	}
	return parseNodeFormats(out)
}

func parseNodeFormats(dump []byte) ([]string, error) {
	var objs []struct {
		Info struct {
			Params struct {
				EnumFormat []struct {
					Format   json.RawMessage `json:"format"`
					Modifier json.RawMessage `json:"modifier"`
				} `json:"EnumFormat"`
			} `json:"params"`
		} `json:"info"`
	}
	if err := json.Unmarshal(dump, &objs); err != nil {
		return nil, fmt.Errorf("pw-dump: %w", err)
	}
	var out []string
	seen := map[string]bool{}
	for _, o := range objs {
		for _, ef := range o.Info.Params.EnumFormat {
			if len(ef.Modifier) == 0 {
				continue
			}
			for _, name := range choiceValues[string](ef.Format) {
				fourcc, ok := spaFourcc[name]
				if !ok {
					continue
				}
				for _, m := range choiceValues[int64](ef.Modifier) {
					f := fmt.Sprintf("%s:0x%016x", fourcc, uint64(m))
					if !seen[f] {
						seen[f] = true
						out = append(out, f)
					}
				}
			}
		}
	}
	return out, nil
}

// choiceValues reads a pw-dump value that is a scalar, a list, or a
// choice object ({"default": v, "alt1": v, ...}); default comes first.
func choiceValues[T any](raw json.RawMessage) []T {
	var v T
	if json.Unmarshal(raw, &v) == nil {
		return []T{v}
	}
	var l []T
	if json.Unmarshal(raw, &l) == nil {
		return l
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	var out []T
	keys := make([]string, 0, len(m))
	for k := range m {
		if k != "default" {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b string) int { return cmp.Compare(len(a), len(b))*2 + cmp.Compare(a, b) })
	for _, k := range append([]string{"default"}, keys...) {
		if r, ok := m[k]; ok && json.Unmarshal(r, &v) == nil {
			out = append(out, v)
		}
	}
	return out
}
