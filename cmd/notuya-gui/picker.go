package main

import (
	"fmt"
	"math"
	"os"
	"sync"

	"github.com/alex-bluetrain/notuya-go/pkg/bulb"
	"github.com/alex-bluetrain/notuya-go/pkg/device"
	"github.com/diamondburned/gotk4-layer-shell/pkg/gtk4layershell"
	"github.com/diamondburned/gotk4/pkg/cairo"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	coreglib "github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// picker holds the whole GUI + streaming state, mirroring picker.py's
// WheelPickerApp. It is only ever touched from the GTK main thread except
// where noted (streamer start-up runs on its own goroutine).
type picker struct {
	app          *gtk.Application
	window       *gtk.Window
	drawingArea  *gtk.DrawingArea
	swatch       *gtk.DrawingArea
	wheelSurface *cairo.Surface

	devices     []Device
	lastColPath string

	prevHex string
	handled bool

	// selection (HSV) and controls
	selHue        float64
	selSat        float64
	selBrightness int // percent 1-100
	selTransition int // 0-10
	lightsOn      bool
	dragging      bool
	dragStartX    float64
	dragStartY    float64

	// streamer is built on a background goroutine after the window shows,
	// so the UI appears instantly. Guarded because Set is called from the
	// GTK thread while newStreamer runs elsewhere.
	mu       sync.Mutex
	streamer *streamer
}

func runPicker(configPath string, cfg *Config) {
	p := &picker{
		devices:       cfg.Devices,
		lastColPath:   lastColorPath(configPath),
		selBrightness: 100,
		selTransition: device.DefaultTransition,
		lightsOn:      true,
		wheelSurface:  loadWheelSurface(wheelCachePath(configPath)),
	}

	p.app = gtk.NewApplication("ar.averstraeten.tuyawheel", gio.ApplicationNonUnique)
	p.app.ConnectActivate(func() { p.activate() })
	os.Exit(p.app.Run(os.Args[:1]))
}

func (p *picker) activate() {
	p.prevHex = readLastColor(p.lastColPath)
	p.handled = false
	p.selHue, p.selSat, _ = hexToHSV(p.prevHex)

	// Build the streamer off the UI thread, seeded with the initial colour
	// so music mode is entered on the colour the wheel already shows.
	r, g, b := hsvToRGBInt(p.selHue, p.selSat, 1.0)
	go func() {
		s := newStreamer(p.devices, device.RGB{R: r, G: g, B: b}, bulb.StreamOptions{
			Transition: bulb.Transition(p.selTransition),
		})
		p.mu.Lock()
		p.streamer = s
		p.mu.Unlock()
	}()

	p.showWindow()
}

func (p *picker) showWindow() {
	window := gtk.NewWindow()
	window.SetTitle("Lights")
	p.window = window

	gtk4layershell.InitForWindow(window)
	gtk4layershell.SetLayer(window, gtk4layershell.LayerShellLayerOverlay)
	gtk4layershell.SetKeyboardMode(window, gtk4layershell.LayerShellKeyboardModeOnDemand)
	gtk4layershell.SetNamespace(window, "tuya-wheel")
	for _, edge := range []gtk4layershell.Edge{
		gtk4layershell.LayerShellEdgeTop,
		gtk4layershell.LayerShellEdgeBottom,
		gtk4layershell.LayerShellEdgeLeft,
		gtk4layershell.LayerShellEdgeRight,
	} {
		gtk4layershell.SetAnchor(window, edge, false)
	}

	// --- Colour wheel ---
	da := gtk.NewDrawingArea()
	da.SetContentWidth(wheelSize)
	da.SetContentHeight(wheelSize)
	da.SetHAlign(gtk.AlignCenter)
	da.SetMarginTop(16)
	da.SetDrawFunc(p.drawWheel)
	p.drawingArea = da

	click := gtk.NewGestureClick()
	click.ConnectPressed(func(_ int, x, y float64) { p.pickAt(x, y) })
	da.AddController(click)

	drag := gtk.NewGestureDrag()
	drag.ConnectDragBegin(func(x, y float64) {
		p.dragging = true
		p.dragStartX, p.dragStartY = x, y
		p.pickAt(x, y)
	})
	drag.ConnectDragUpdate(func(ox, oy float64) {
		if p.dragging {
			p.pickAt(p.dragStartX+ox, p.dragStartY+oy)
		}
	})
	drag.ConnectDragEnd(func(_, _ float64) { p.dragging = false })
	da.AddController(drag)

	// --- Swatch ---
	swatch := gtk.NewDrawingArea()
	swatch.SetContentWidth(wheelSize / 3)
	swatch.SetContentHeight(36)
	swatch.SetMarginTop(20)
	swatch.SetHAlign(gtk.AlignCenter)
	swatch.SetDrawFunc(p.drawSwatch)
	p.swatch = swatch

	// --- Brightness slider ---
	brightBox := gtk.NewBox(gtk.OrientationHorizontal, 8)
	brightBox.SetMarginTop(16)
	brightLabel := gtk.NewLabel("Brightness")
	brightBox.Append(brightLabel)
	brightScale := gtk.NewScaleWithRange(gtk.OrientationHorizontal, 1, 100, 1)
	brightScale.SetValue(float64(p.selBrightness))
	brightScale.SetHExpand(true)
	brightScale.SetDrawValue(false)
	brightScale.SetRoundDigits(0)
	brightPct := gtk.NewLabel("100%")
	brightPct.SetWidthChars(4)
	brightPct.SetXAlign(1.0)
	brightScale.ConnectValueChanged(func() {
		v := int(brightScale.Value())
		p.selBrightness = v
		brightPct.SetLabel(fmt.Sprintf("%d%%", v))
		if p.lightsOn {
			p.applyCurrent()
		}
	})
	brightBox.Append(brightScale)
	brightBox.Append(brightPct)

	// --- Transition slider ---
	transBox := gtk.NewBox(gtk.OrientationHorizontal, 8)
	transBox.SetMarginTop(12)
	transLabel := gtk.NewLabel("Transition")
	transBox.Append(transLabel)
	transScale := gtk.NewScaleWithRange(gtk.OrientationHorizontal, 0, float64(device.MaxTransition), 1)
	transScale.SetValue(float64(p.selTransition))
	transScale.SetHExpand(true)
	transScale.SetDrawValue(false)
	transScale.SetRoundDigits(0)
	transVal := gtk.NewLabel(fmt.Sprintf("%d", p.selTransition))
	transVal.SetWidthChars(2)
	transVal.SetXAlign(1.0)
	transScale.ConnectValueChanged(func() {
		v := int(transScale.Value())
		p.selTransition = v
		transVal.SetLabel(fmt.Sprintf("%d", v))
	})
	transBox.Append(transScale)
	transBox.Append(transVal)

	// --- Power switch ---
	toggleBox := gtk.NewBox(gtk.OrientationHorizontal, 8)
	toggleBox.SetMarginTop(12)
	toggleLabel := gtk.NewLabel("Lights")
	toggleLabel.SetHExpand(true)
	toggleLabel.SetXAlign(0.0)
	toggleBox.Append(toggleLabel)
	powerSwitch := gtk.NewSwitch()
	powerSwitch.SetActive(true)
	powerSwitch.SetHAlign(gtk.AlignEnd)
	powerSwitch.ConnectStateSet(func(state bool) bool {
		p.lightsOn = state
		if s := p.currentStreamer(); s != nil {
			s.SetPower(state)
			if state {
				p.applyCurrent()
			}
		}
		return false
	})
	toggleBox.Append(powerSwitch)

	// --- Buttons ---
	buttons := gtk.NewBox(gtk.OrientationHorizontal, 8)
	buttons.SetMarginTop(20)
	buttons.SetHAlign(gtk.AlignEnd)
	cancel := gtk.NewButtonWithLabel("Cancel")
	cancel.ConnectClicked(func() { p.finish(false) })
	accept := gtk.NewButtonWithLabel("Accept")
	accept.AddCSSClass("suggested-action")
	accept.ConnectClicked(func() { p.finish(true) })
	buttons.Append(cancel)
	buttons.Append(accept)

	title := gtk.NewLabel("Lights")
	title.SetHAlign(gtk.AlignCenter)

	// --- Layout ---
	content := gtk.NewBox(gtk.OrientationVertical, 0)
	content.SetMarginTop(24)
	content.SetMarginBottom(24)
	content.SetMarginStart(28)
	content.SetMarginEnd(28)
	content.Append(title)
	content.Append(da)
	content.Append(swatch)
	content.Append(brightBox)
	content.Append(transBox)
	content.Append(toggleBox)
	content.Append(buttons)
	window.SetChild(content)

	window.ConnectCloseRequest(func() bool {
		p.finish(false)
		return true
	})

	keys := gtk.NewEventControllerKey()
	keys.ConnectKeyPressed(func(keyval, _ uint, _ gdk.ModifierType) bool {
		switch keyval {
		case gdk.KEY_Escape:
			p.finish(false)
			return true
		case gdk.KEY_Return, gdk.KEY_KP_Enter:
			p.finish(true)
			return true
		}
		return false
	})
	window.AddController(keys)

	p.app.Hold()
	window.Present()
}

// --- Drawing ---

func (p *picker) drawWheel(_ *gtk.DrawingArea, cr *cairo.Context, width, height int) {
	ox := (float64(width) - wheelSize) / 2
	oy := (float64(height) - wheelSize) / 2

	cr.SetSourceSurface(p.wheelSurface, ox, oy)
	cr.Paint()

	radius := wheelSize / 2.0
	angle := p.selHue * 2 * math.Pi
	dist := p.selSat * radius
	sx := ox + radius + dist*math.Cos(angle)
	sy := oy + radius + dist*math.Sin(angle)

	cr.Arc(sx, sy, 8, 0, 2*math.Pi)
	cr.SetSourceRGBA(0, 0, 0, 0.7)
	cr.SetLineWidth(2.5)
	cr.Stroke()

	cr.Arc(sx, sy, 6, 0, 2*math.Pi)
	cr.SetSourceRGBA(1, 1, 1, 0.95)
	cr.SetLineWidth(2)
	cr.Stroke()
}

func roundedRect(cr *cairo.Context, x, y, w, h, r float64) {
	cr.NewSubPath()
	cr.Arc(x+w-r, y+r, r, -math.Pi/2, 0)
	cr.Arc(x+w-r, y+h-r, r, 0, math.Pi/2)
	cr.Arc(x+r, y+h-r, r, math.Pi/2, math.Pi)
	cr.Arc(x+r, y+r, r, math.Pi, 3*math.Pi/2)
	cr.ClosePath()
}

func (p *picker) drawSwatch(_ *gtk.DrawingArea, cr *cairo.Context, width, height int) {
	r, g, b := hsvToRGBInt(p.selHue, p.selSat, 1.0)
	w, h := float64(width), float64(height)

	roundedRect(cr, 0, 0, w, h, 8)
	cr.SetSourceRGB(float64(r)/255, float64(g)/255, float64(b)/255)
	cr.Fill()

	roundedRect(cr, 0.5, 0.5, w-1, h-1, 8)
	cr.SetSourceRGBA(0, 0, 0, 0.15)
	cr.SetLineWidth(1)
	cr.Stroke()
}

// --- Input ---

// coordsToHS converts widget coordinates to (hue, saturation), clamping to
// the wheel edge. It matches picker.py's _coords_to_hs.
func coordsToHS(x, y float64) (h, s float64) {
	return coordsToHSSized(x, y, wheelSize)
}

func (p *picker) pickAt(x, y float64) {
	p.selHue, p.selSat = coordsToHS(x, y)
	p.drawingArea.QueueDraw()
	p.swatch.QueueDraw()
	p.applyCurrent()
}

// --- Colour application ---

func (p *picker) currentStreamer() *streamer {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.streamer
}

func (p *picker) currentHex() string {
	r, g, b := hsvToRGBInt(p.selHue, p.selSat, 1.0)
	return rgbToHex(r, g, b)
}

// scaledRGB returns the current selection scaled by the brightness percent,
// matching picker.py (brightness is not a stream channel; it scales RGB).
func (p *picker) scaledRGB() device.RGB {
	r, g, b := hsvToRGBInt(p.selHue, p.selSat, 1.0)
	f := float64(p.selBrightness) / 100.0
	return device.RGB{
		R: uint8(math.Round(float64(r) * f)),
		G: uint8(math.Round(float64(g) * f)),
		B: uint8(math.Round(float64(b) * f)),
	}
}

func (p *picker) applyCurrent() {
	s := p.currentStreamer()
	if s == nil {
		return
	}
	s.Set(p.scaledRGB(), bulb.Transition(p.selTransition))
}

func (p *picker) finish(accepted bool) {
	if p.handled {
		return
	}
	p.handled = true

	var final device.RGB
	if accepted {
		hex := p.currentHex()
		if err := writeLastColor(p.lastColPath, hex); err != nil {
			fmt.Fprintln(os.Stderr, "notuya-gui: could not save last color:", err)
		}
		final = p.scaledRGB()
	} else {
		r, g, b := hexToRGB(p.prevHex)
		final = device.RGB{R: r, G: g, B: b}
	}

	// Close the streamer off the UI thread (it waits for each device's
	// final SetColour), then tear down the window once done.
	go func() {
		if s := p.currentStreamer(); s != nil {
			s.Set(final, bulb.Transition(p.selTransition))
			s.Close()
		}
		coreglib.IdleAdd(func() {
			p.window.Destroy()
			p.app.Release()
		})
	}()
}
