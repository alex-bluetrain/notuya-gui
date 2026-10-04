package main

import (
	"math"

	"github.com/alex-bluetrain/notuya-go/pkg/dp"
	"github.com/diamondburned/gotk4-layer-shell/pkg/gtk4layershell"
	"github.com/diamondburned/gotk4/pkg/cairo"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"

	"github.com/alex-bluetrain/notuya-gui/internal/screensync"
)

const (
	overlayNamespace = "notuya-sync"
	handleSize       = 14.0 // px: corner grab area
	minRegionPx      = 24.0
)

// overlayRegion is one region as the overlay draws it.
type overlayRegion struct {
	Name   string
	Rect   [4]float64 // normalised to the canvas
	Colour dp.RGB
}

// syncOverlay is the region editor drawn over the target: one layer-shell
// surface per monitor the target touches. It exists only while editing, so
// the game keeps direct scanout while syncing.
type syncOverlay struct {
	windows  []*overlayWindow
	canvas   screensync.Monitor
	regions  []overlayRegion
	selected int

	// onChange fires while a region is dragged; onCreate when a new one
	// is drawn; onDelete for Delete on the selected one; onDone on Esc or
	// the Done button.
	onChange func(i int, rect [4]float64)
	onCreate func(rect [4]float64)
	onDelete func(i int)
	onDone   func()

	// drag state
	mode   int // dragNone, dragMove, dragResize, dragCreate
	corner int // 0..3: tl, tr, bl, br
	start  [4]float64
	ax, ay float64 // drag anchor, layout px
}

const (
	dragNone = iota
	dragMove
	dragResize
	dragCreate
)

type overlayWindow struct {
	win *gtk.Window
	da  *gtk.DrawingArea
	mon screensync.Monitor
}

// layerShellSupported reports whether the compositor speaks
// wlr-layer-shell and the library is linked ahead of libwayland-client.
func layerShellSupported() bool { return gtk4layershell.IsSupported() }

func newSyncOverlay() *syncOverlay {
	return &syncOverlay{selected: -1}
}

// Show maps (or re-places) the overlay over p.
func (o *syncOverlay) Show(app *gtk.Application, p screensync.Placement) {
	o.Hide()
	o.canvas = p.Canvas
	mons := gdk.DisplayGetDefault().Monitors()
	for _, m := range p.Monitors {
		var gm *gdk.Monitor
		for i := uint(0); i < mons.NItems(); i++ {
			if c := mons.Item(i).Cast().(*gdk.Monitor); c.Connector() == m.Name {
				gm = c
			}
		}
		if gm == nil {
			continue
		}
		o.windows = append(o.windows, o.newWindow(app, gm, m))
	}
}

// Hide unmaps every overlay surface.
func (o *syncOverlay) Hide() {
	for _, w := range o.windows {
		w.win.Destroy()
	}
	o.windows = nil
	o.mode = dragNone
}

// Visible reports whether the overlay is mapped.
func (o *syncOverlay) Visible() bool { return len(o.windows) > 0 }

// SetRegions replaces what the overlay draws.
func (o *syncOverlay) SetRegions(rs []overlayRegion) {
	o.regions = rs
	if o.selected >= len(rs) {
		o.selected = -1
	}
	o.redraw()
}

// SetColour updates one region's swatch without a full replace.
func (o *syncOverlay) SetColour(i int, c dp.RGB) {
	if i < len(o.regions) && o.regions[i].Colour != c {
		o.regions[i].Colour = c
		o.redraw()
	}
}

func (o *syncOverlay) redraw() {
	for _, w := range o.windows {
		w.da.QueueDraw()
	}
}

func (o *syncOverlay) newWindow(app *gtk.Application, gm *gdk.Monitor, m screensync.Monitor) *overlayWindow {
	w := gtk.NewWindow()
	w.SetApplication(app)
	w.SetDecorated(false)
	w.AddCSSClass("sync-overlay")
	gtk4layershell.InitForWindow(w)
	gtk4layershell.SetLayer(w, gtk4layershell.LayerShellLayerOverlay)
	gtk4layershell.SetNamespace(w, overlayNamespace)
	// on_demand: Esc and Delete reach us, clicks elsewhere give focus back.
	gtk4layershell.SetKeyboardMode(w, gtk4layershell.LayerShellKeyboardModeOnDemand)
	for _, e := range []gtk4layershell.Edge{gtk4layershell.LayerShellEdgeTop, gtk4layershell.LayerShellEdgeBottom, gtk4layershell.LayerShellEdgeLeft, gtk4layershell.LayerShellEdgeRight} {
		gtk4layershell.SetAnchor(w, e, true)
	}
	gtk4layershell.SetExclusiveZone(w, -1)
	gtk4layershell.SetMonitor(w, gm)

	ow := &overlayWindow{win: w, mon: m}
	ow.da = gtk.NewDrawingArea()
	ow.da.SetDrawFunc(func(_ *gtk.DrawingArea, cr *cairo.Context, _, _ int) { o.draw(ow, cr) })

	done := gtk.NewButtonWithLabel("Done")
	done.AddCSSClass("pill")
	done.AddCSSClass("suggested-action")
	done.SetHAlign(gtk.AlignCenter)
	done.SetVAlign(gtk.AlignStart)
	done.SetMarginTop(24)
	done.SetTooltipText("Finish editing regions (Esc)")
	done.ConnectClicked(func() { o.finish() })

	ov := gtk.NewOverlay()
	ov.SetChild(ow.da)
	ov.AddOverlay(done)
	w.SetChild(ov)

	drag := gtk.NewGestureDrag()
	drag.ConnectDragBegin(func(x, y float64) { o.dragBegin(ow, x, y) })
	drag.ConnectDragUpdate(func(dx, dy float64) { o.dragUpdate(dx, dy) })
	drag.ConnectDragEnd(func(dx, dy float64) { o.dragEnd(dx, dy) })
	ow.da.AddController(drag)

	keys := gtk.NewEventControllerKey()
	keys.ConnectKeyPressed(func(keyval, _ uint, _ gdk.ModifierType) bool {
		switch keyval {
		case gdk.KEY_Escape:
			o.finish()
			return true
		case gdk.KEY_Delete, gdk.KEY_BackSpace:
			if o.selected >= 0 && o.onDelete != nil {
				i := o.selected
				o.selected = -1
				o.onDelete(i)
			}
			return true
		}
		return false
	})
	w.AddController(keys)
	w.SetVisible(true)
	return ow
}

func (o *syncOverlay) finish() {
	o.Hide()
	if o.onDone != nil {
		o.onDone()
	}
}

// toLayout maps a normalised canvas rect to layout pixels.
func (o *syncOverlay) toLayout(r [4]float64) (x, y, w, h float64) {
	c := o.canvas
	return float64(c.X) + r[0]*float64(c.W), float64(c.Y) + r[1]*float64(c.H), r[2] * float64(c.W), r[3] * float64(c.H)
}

// toCanvas maps layout pixels to a normalised, clamped canvas rect.
func (o *syncOverlay) toCanvas(x, y, w, h float64) [4]float64 {
	c := o.canvas
	if c.W == 0 || c.H == 0 {
		return [4]float64{}
	}
	nx, ny := (x-float64(c.X))/float64(c.W), (y-float64(c.Y))/float64(c.H)
	nw, nh := w/float64(c.W), h/float64(c.H)
	nx, ny = clamp01(nx), clamp01(ny)
	nw, nh = math.Min(nw, 1-nx), math.Min(nh, 1-ny)
	return [4]float64{nx, ny, nw, nh}
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

func (o *syncOverlay) draw(ow *overlayWindow, cr *cairo.Context) {
	// Dim the whole monitor, then clear the canvas so the target reads as
	// the editable area.
	cr.SetSourceRGBA(0, 0, 0, 0.35)
	cr.Paint()
	cx, cy := float64(o.canvas.X-ow.mon.X), float64(o.canvas.Y-ow.mon.Y)
	cr.SetOperator(cairo.OperatorClear)
	cr.Rectangle(cx, cy, float64(o.canvas.W), float64(o.canvas.H))
	cr.Fill()
	cr.SetOperator(cairo.OperatorOver)
	cr.SetSourceRGBA(1, 1, 1, 0.6)
	cr.SetLineWidth(2)
	cr.SetDash([]float64{8, 6}, 0)
	cr.Rectangle(cx+1, cy+1, float64(o.canvas.W)-2, float64(o.canvas.H)-2)
	cr.Stroke()
	cr.SetDash(nil, 0)

	for i, r := range o.regions {
		x, y, w, h := o.toLayout(r.Rect)
		x -= float64(ow.mon.X)
		y -= float64(ow.mon.Y)
		c := r.Colour
		cr.SetSourceRGBA(float64(c.R)/255, float64(c.G)/255, float64(c.B)/255, 0.45)
		cr.Rectangle(x, y, w, h)
		cr.Fill()
		width := 2.0
		if i == o.selected {
			width = 4
		}
		cr.SetSourceRGBA(1, 1, 1, 0.95)
		cr.SetLineWidth(width)
		cr.Rectangle(x, y, w, h)
		cr.Stroke()
		for _, p := range corners(x, y, w, h) {
			cr.Rectangle(p[0]-handleSize/2, p[1]-handleSize/2, handleSize, handleSize)
			cr.Fill()
		}
		cr.SelectFontFace("sans-serif", cairo.FontSlantNormal, cairo.FontWeightBold)
		cr.SetFontSize(16)
		cr.MoveTo(x+10, y+24)
		cr.ShowText(r.Name)
	}
}

func corners(x, y, w, h float64) [4][2]float64 {
	return [4][2]float64{{x, y}, {x + w, y}, {x, y + h}, {x + w, y + h}}
}

func (o *syncOverlay) dragBegin(ow *overlayWindow, x, y float64) {
	lx, ly := x+float64(ow.mon.X), y+float64(ow.mon.Y)
	o.ax, o.ay = lx, ly
	o.mode = dragCreate
	// Topmost (last drawn) region wins; corners before bodies.
	for i := len(o.regions) - 1; i >= 0; i-- {
		rx, ry, rw, rh := o.toLayout(o.regions[i].Rect)
		for k, p := range corners(rx, ry, rw, rh) {
			if math.Abs(lx-p[0]) <= handleSize && math.Abs(ly-p[1]) <= handleSize {
				o.mode, o.corner, o.selected, o.start = dragResize, k, i, o.regions[i].Rect
				o.redraw()
				return
			}
		}
		if lx >= rx && lx <= rx+rw && ly >= ry && ly <= ry+rh {
			o.mode, o.selected, o.start = dragMove, i, o.regions[i].Rect
			o.redraw()
			return
		}
	}
	o.selected = -1
	o.redraw()
}

// dragRect is the rect the current drag describes, in layout px.
func (o *syncOverlay) dragRect(dx, dy float64) (x, y, w, h float64) {
	switch o.mode {
	case dragMove:
		x, y, w, h = o.toLayout(o.start)
		x = math.Max(float64(o.canvas.X), math.Min(x+dx, float64(o.canvas.X+o.canvas.W)-w))
		y = math.Max(float64(o.canvas.Y), math.Min(y+dy, float64(o.canvas.Y+o.canvas.H)-h))
		return
	case dragResize:
		x, y, w, h = o.toLayout(o.start)
		x0, y0, x1, y1 := x, y, x+w, y+h
		if o.corner == 0 || o.corner == 2 {
			x0 += dx
		} else {
			x1 += dx
		}
		if o.corner < 2 {
			y0 += dy
		} else {
			y1 += dy
		}
		return math.Min(x0, x1), math.Min(y0, y1), math.Abs(x1 - x0), math.Abs(y1 - y0)
	default:
		return math.Min(o.ax, o.ax+dx), math.Min(o.ay, o.ay+dy), math.Abs(dx), math.Abs(dy)
	}
}

func (o *syncOverlay) dragUpdate(dx, dy float64) {
	if o.mode == dragNone {
		return
	}
	x, y, w, h := o.dragRect(dx, dy)
	r := o.toCanvas(x, y, w, h)
	if o.mode == dragCreate {
		if w < minRegionPx || h < minRegionPx {
			return
		}
		if len(o.regions) >= screensync.MaxRegions {
			o.mode = dragNone
			return
		}
		// The new region becomes live as soon as it is big enough; the
		// rest of the drag resizes it.
		if o.onCreate != nil {
			o.onCreate(r)
		}
		o.selected = len(o.regions) - 1
		o.mode, o.corner = dragResize, 3
		// A zero-size rect at the anchor, grown by the corner under the
		// pointer.
		o.start = o.toCanvas(o.ax, o.ay, 0, 0)
		if dx < 0 {
			o.corner ^= 1
		}
		if dy < 0 {
			o.corner ^= 2
		}
		return
	}
	if w < minRegionPx || h < minRegionPx || o.selected < 0 || o.selected >= len(o.regions) {
		return
	}
	o.regions[o.selected].Rect = r
	if o.onChange != nil {
		o.onChange(o.selected, r)
	}
	o.redraw()
}

func (o *syncOverlay) dragEnd(dx, dy float64) {
	o.dragUpdate(dx, dy)
	o.mode = dragNone
}
