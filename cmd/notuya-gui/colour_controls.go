package main

import (
	"fmt"
	"math"
	"sync"

	"github.com/alex-bluetrain/notuya-go/pkg/device"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/cairo"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// colourWheelPad pads the wheel's drawing area so the thumb — whose centre
// rides the disc edge at full saturation — can overhang without being clipped.
const colourWheelPad = 16

// tempScaleCSS paints the temperature scale's trough with the warm→cool
// gradient and neutralises GTK's default trough styling so the ramp reads as
// one clean bar: the filled ("highlight") and unfilled halves share the same
// gradient, and the slider keeps a round white knob like the Brightness scale.
const tempScaleCSS = `
scale.temp-scale trough {
  background-image: linear-gradient(to right,
    rgb(255,166,70) 0%,
    rgb(255,246,235) 50%,
    rgb(158,202,255) 100%);
  background-color: transparent;
  border: none;
  min-height: 10px;
  border-radius: 6px;
}
scale.temp-scale highlight {
  background-color: transparent;
  background-image: none;
  border: none;
}
scale.temp-scale fill {
  background-color: transparent;
  background-image: none;
}
scale.temp-scale slider {
  background-color: #ffffff;
  border: 1px solid alpha(#000, 0.2);
  box-shadow: 0 1px 3px alpha(#000, 0.35);
  min-width: 18px;
  min-height: 18px;
  border-radius: 50%;
  margin: -6px;
}
`

var tempScaleCSSOnce sync.Once

func ensureTempScaleCSS() {
	tempScaleCSSOnce.Do(func() {
		if prov := gtk.NewCSSProvider(); prov != nil {
			prov.LoadFromData(tempScaleCSS)
			if disp := gdk.DisplayGetDefault(); disp != nil {
				gtk.StyleContextAddProviderForDisplay(disp, prov, gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)
			}
		}
	})
}

// colourCallbacks are the hooks a colourControls owner wires to drive real
// bulbs. All fire on the GTK thread; every one of them should be guarded by
// the owner against programmatic changes (a suppress flag) where needed.
// hue/sat at the moment of a callback are available via the struct fields.
type colourCallbacks struct {
	// OnMode fires when the Colour/White toggle changes (visibility is
	// already updated by the component).
	OnMode func(isWhite bool)
	// OnDragBegin/Update/End bracket a wheel drag with the current RGB.
	OnDragBegin  func(rgb device.RGB)
	OnDragUpdate func(rgb device.RGB)
	OnDragEnd    func(rgb device.RGB)
	// OnBright / OnTemp fire on slider moves with the new 0-100 value.
	OnBright func(v float64)
	OnTemp   func(v float64)
}

// colourControls is the shared colour-selection widget set used by the Lights
// tab and the scene editor: a Colour|White segmented toggle, the
// soft-edged wheel whose ring thumb doubles as the live preview, a
// gradient-trough temperature scale, and a brightness scale with a percent
// readout. The owner appends the rows via AppendTo and reacts through
// colourCallbacks; hue/sat live here.
type colourControls struct {
	Mode      *adw.ToggleGroup
	WheelRow  *gtk.Box
	Wheel     *gtk.DrawingArea
	BrightRow *gtk.Box
	Bright    *gtk.Scale
	brightPct *gtk.Label
	TempRow   *gtk.Box
	Temp      *gtk.Scale

	surface *cairo.Surface
	size    int // wheel diameter in px

	hue float64
	sat float64

	cb colourCallbacks
}

// newColourControls builds the widget set. surface is the cached HSV wheel
// bitmap (wheelSize px); size is the displayed wheel diameter.
func newColourControls(surface *cairo.Surface, size int, cb colourCallbacks) *colourControls {
	cc := &colourControls{surface: surface, size: size, cb: cb}

	// Mode selector: a full-width Colour|White segmented toggle. No label —
	// the control is self-evident next to the wheel/temperature slider.
	cc.Mode = newModeToggle(func(isWhite bool) {
		cc.ApplyModeVisibility()
		if cc.cb.OnMode != nil {
			cc.cb.OnMode(isWhite)
		}
	})

	// Colour wheel (colour mode): a soft-edged disc centred in the card whose
	// thumb doubles as the live colour preview, Hue-app style — no swatch.
	cc.WheelRow = gtk.NewBox(gtk.OrientationVertical, 12)
	cc.WheelRow.SetHAlign(gtk.AlignCenter)
	cc.WheelRow.SetMarginTop(8)
	cc.WheelRow.SetMarginBottom(8)

	cc.Wheel = gtk.NewDrawingArea()
	cc.Wheel.SetContentWidth(size + 2*colourWheelPad)
	cc.Wheel.SetContentHeight(size + 2*colourWheelPad)
	cc.Wheel.SetHAlign(gtk.AlignCenter)
	cc.Wheel.SetDrawFunc(cc.drawWheel)
	cc.WheelRow.Append(cc.Wheel)

	drag := gtk.NewGestureDrag()
	var startX, startY float64
	drag.ConnectDragBegin(func(x, y float64) {
		startX, startY = x, y
		cc.setSelection(x, y)
		if cc.cb.OnDragBegin != nil {
			cc.cb.OnDragBegin(cc.SelRGB())
		}
	})
	drag.ConnectDragUpdate(func(ox, oy float64) {
		cc.setSelection(startX+ox, startY+oy)
		if cc.cb.OnDragUpdate != nil {
			cc.cb.OnDragUpdate(cc.SelRGB())
		}
	})
	drag.ConnectDragEnd(func(ox, oy float64) {
		cc.setSelection(startX+ox, startY+oy)
		if cc.cb.OnDragEnd != nil {
			cc.cb.OnDragEnd(cc.SelRGB())
		}
	})
	cc.Wheel.AddController(drag)

	// Temperature scale (white mode): the same native gtk.Scale as Brightness
	// so the two rows match; tempScaleCSS paints the warm→cool trough.
	cc.TempRow = gtk.NewBox(gtk.OrientationHorizontal, 8)
	tempIcon := gtk.NewImageFromIconName("night-light-symbolic")
	tempIcon.AddCSSClass("dim-label")
	cc.TempRow.Append(tempIcon)
	cc.Temp = gtk.NewScaleWithRange(gtk.OrientationHorizontal, 0, 100, 1)
	cc.Temp.SetHExpand(true)
	cc.Temp.SetDrawValue(false)
	cc.Temp.SetRoundDigits(0)
	cc.Temp.AddCSSClass("temp-scale")
	disableScaleScroll(cc.Temp)
	cc.Temp.ConnectValueChanged(func() {
		if cc.cb.OnTemp != nil {
			cc.cb.OnTemp(cc.Temp.Value())
		}
	})
	ensureTempScaleCSS()
	cc.TempRow.Append(cc.Temp)

	// Brightness scale with a live percent readout, prefixed by an icon
	// rather than a text label column so the card keeps one alignment rhythm.
	cc.BrightRow = gtk.NewBox(gtk.OrientationHorizontal, 8)
	brightIcon := gtk.NewImageFromIconName("display-brightness-symbolic")
	brightIcon.AddCSSClass("dim-label")
	cc.BrightRow.Append(brightIcon)
	cc.Bright = gtk.NewScaleWithRange(gtk.OrientationHorizontal, 1, 100, 1)
	cc.Bright.SetHExpand(true)
	cc.Bright.SetDrawValue(false)
	cc.Bright.SetRoundDigits(0)
	disableScaleScroll(cc.Bright)
	cc.brightPct = gtk.NewLabel("100%")
	cc.brightPct.SetWidthChars(4)
	cc.brightPct.SetXAlign(1.0)
	cc.brightPct.AddCSSClass("dim-label")
	cc.Bright.ConnectValueChanged(func() {
		v := cc.Bright.Value()
		cc.brightPct.SetText(fmt.Sprintf("%d%%", int(v)))
		if cc.cb.OnBright != nil {
			cc.cb.OnBright(v)
		}
	})
	cc.BrightRow.Append(cc.Bright)
	cc.BrightRow.Append(cc.brightPct)
	cc.Bright.SetValue(100)

	return cc
}

// AppendTo appends the rows in the canonical order: mode toggle, wheel,
// temperature, brightness (white mode mirrors colour mode's
// selector-then-brightness order).
func (cc *colourControls) AppendTo(box *gtk.Box) {
	box.Append(cc.Mode)
	box.Append(cc.WheelRow)
	box.Append(cc.TempRow)
	box.Append(cc.BrightRow)
}

// IsWhite reports whether the White toggle is active.
func (cc *colourControls) IsWhite() bool { return cc.Mode.ActiveName() == "white" }

// ApplyModeVisibility shows the wheel for colour mode and the temperature
// scale for white mode.
func (cc *colourControls) ApplyModeVisibility() {
	white := cc.IsWhite()
	cc.WheelRow.SetVisible(!white)
	cc.TempRow.SetVisible(white)
}

// SetSensitive greys out every control at once (scene editor rows use this
// when a light is excluded or off).
func (cc *colourControls) SetSensitive(on bool) {
	cc.Mode.SetSensitive(on)
	cc.WheelRow.SetSensitive(on)
	cc.Bright.SetSensitive(on)
	cc.Temp.SetSensitive(on)
}

// SetHS sets the wheel selection programmatically and redraws.
func (cc *colourControls) SetHS(h, s float64) {
	cc.hue, cc.sat = h, s
	cc.Wheel.QueueDraw()
}

// SelRGB is the current wheel selection at full value. Brightness is a
// separate control and is deliberately not mixed in.
func (cc *colourControls) SelRGB() device.RGB {
	r, g, b := hsvToRGBInt(cc.hue, cc.sat, 1.0)
	return device.RGB{R: r, G: g, B: b}
}

// setSelection updates hue/sat from wheel coordinates (compensating for the
// thumb pad) and redraws.
func (cc *colourControls) setSelection(x, y float64) {
	cc.hue, cc.sat = coordsToHSSized(x-colourWheelPad, y-colourWheelPad, float64(cc.size))
	cc.Wheel.QueueDraw()
}

func (cc *colourControls) drawWheel(_ *gtk.DrawingArea, cr *cairo.Context, width, height int) {
	size := float64(cc.size)
	radius := size / 2.0
	pad := float64(colourWheelPad)
	cx, cy := pad+radius, pad+radius

	// Soft, antialiased disc edge: clip the raw HSV bitmap to a circle a hair
	// inside its bounds so its hard pixel edge never shows.
	cr.Save()
	cr.Arc(cx, cy, radius-1.5, 0, 2*math.Pi)
	cr.Clip()
	cr.Translate(pad, pad)
	scale := size / float64(wheelSize)
	cr.Scale(scale, scale)
	cr.SetSourceSurface(cc.surface, 0, 0)
	cr.Paint()
	cr.Restore()

	// Subtle rim so the near-white centre region doesn't bleed into the card.
	cr.Arc(cx, cy, radius-1, 0, 2*math.Pi)
	cr.SetSourceRGBA(0, 0, 0, 0.18)
	cr.SetLineWidth(1.5)
	cr.Stroke()

	// Hue-style thumb: a large white ring whose centre is filled with the
	// selected hue/sat — the thumb IS the preview. The thumb's centre rides
	// all the way to the disc edge at full saturation; the padded drawing
	// area keeps the overhang from being clipped.
	const thumbR = 12.0
	angle := cc.hue * 2 * math.Pi
	dist := cc.sat * radius
	sx := cx + dist*math.Cos(angle)
	sy := cy + dist*math.Sin(angle)

	r, g, b := hsvToRGBInt(cc.hue, cc.sat, 1.0)

	// Drop shadow.
	cr.Arc(sx, sy+1.5, thumbR+1, 0, 2*math.Pi)
	cr.SetSourceRGBA(0, 0, 0, 0.30)
	cr.Fill()
	// Colour-filled centre (live preview).
	cr.Arc(sx, sy, thumbR, 0, 2*math.Pi)
	cr.SetSourceRGB(float64(r)/255, float64(g)/255, float64(b)/255)
	cr.Fill()
	// Thick white ring.
	cr.Arc(sx, sy, thumbR-1.5, 0, 2*math.Pi)
	cr.SetSourceRGBA(1, 1, 1, 0.98)
	cr.SetLineWidth(3)
	cr.Stroke()
}

// newModeToggle builds the Colour|White segmented toggle: "colour" (wheel) and
// "white" (colour temperature). onChange fires with isWhite = true when White
// becomes active — including on programmatic SetActiveName("colour"|"white"),
// so callers guard it with a suppress flag.
func newModeToggle(onChange func(isWhite bool)) *adw.ToggleGroup {
	group := adw.NewToggleGroup()
	group.SetHExpand(true)

	colour := adw.NewToggle()
	colour.SetName("colour")
	colour.SetLabel("Colour")
	colour.SetIconName("color-select-symbolic")
	group.Add(colour)

	white := adw.NewToggle()
	white.SetName("white")
	white.SetLabel("White")
	white.SetIconName("weather-clear-symbolic")
	group.Add(white)

	group.NotifyProperty("active-name", func() {
		onChange(group.ActiveName() == "white")
	})
	return group
}

// newTransitionToggle builds the Instant|Smooth segmented toggle for DP 28's
// boolean change mode: "jump" (instant snap) and "fade" (gradual). onChange
// fires with isFade = true when Smooth becomes active — including on
// programmatic SetActiveName("jump"|"fade"), so callers guard it.
func newTransitionToggle(onChange func(isFade bool)) *adw.ToggleGroup {
	group := adw.NewToggleGroup()
	group.SetHExpand(true)

	jump := adw.NewToggle()
	jump.SetName("jump")
	jump.SetLabel("Instant")
	group.Add(jump)

	fade := adw.NewToggle()
	fade.SetName("fade")
	fade.SetLabel("Smooth")
	group.Add(fade)

	group.NotifyProperty("active-name", func() {
		onChange(group.ActiveName() == "fade")
	})
	return group
}

// disableScaleScroll prevents a GtkScale from capturing mouse-wheel events so
// the parent ScrolledWindow scrolls normally when the cursor passes over a
// slider. A capture-phase controller intercepts the scroll before the Scale's
// own handler and marks it handled, letting the ScrolledWindow keep scrolling.
func disableScaleScroll(scale *gtk.Scale) {
	ec := gtk.NewEventControllerScroll(gtk.EventControllerScrollVertical)
	ec.SetPropagationPhase(gtk.PhaseCapture)
	ec.ConnectScroll(func(dx, dy float64) bool { return true })
	scale.AddController(ec)
}

// coordsToHSSized maps a point in a colour wheel of the given diameter to
// (hue, saturation), so the picker overlay and every colourControls wheel
// share one mapping.
func coordsToHSSized(x, y, size float64) (h, s float64) {
	radius := size / 2.0
	dx := x - radius
	dy := y - radius
	dist := math.Sqrt(dx*dx + dy*dy)
	if dist > radius {
		dist = radius
	}
	h = math.Mod(math.Atan2(dy, dx)/(2*math.Pi), 1.0)
	if h < 0 {
		h += 1.0
	}
	s = dist / radius
	return h, s
}
