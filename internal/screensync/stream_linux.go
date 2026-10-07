package screensync

/*
#include <gst/gst.h>
*/
import "C"

import (
	"fmt"
	"strings"
	"time"
)

// MaxRegions is the number of regions one stream can average; it is the
// width of the 16×1 readback row.
const MaxRegions = 16

// Rect is a rectangle normalised to 0..1 of whatever it is relative to.
type Rect struct{ X, Y, W, H float64 }

func (r Rect) area() float64 { return r.W * r.H }

// intersect returns the overlap of r and o (zero size when disjoint).
func (r Rect) intersect(o Rect) Rect {
	x0, y0 := max(r.X, o.X), max(r.Y, o.Y)
	x1, y1 := min(r.X+r.W, o.X+o.W), min(r.Y+r.H, o.Y+o.H)
	if x1 <= x0 || y1 <= y0 {
		return Rect{}
	}
	return Rect{x0, y0, x1 - x0, y1 - y0}
}

// within maps r (in canvas coordinates) into the 0..1 space of frame.
func (r Rect) within(frame Rect) Rect {
	return Rect{(r.X - frame.X) / frame.W, (r.Y - frame.Y) / frame.H, r.W / frame.W, r.H / frame.H}
}

// Shaders run in two passes so the cost is bounded at any resolution:
// an 8×8 box reduce, then one fragment per region that samples a fixed
// 32×32 grid of the reduced texture. Each pass decodes sRGB to linear light,
// averages, and re-encodes, so the 8-bit textures between passes keep
// sRGB's dark-end precision while the mean is of light, not of codes.
const shaderHeader = "#version 100\n#ifdef GL_ES\nprecision highp float;\n#endif\nvarying vec2 v_texcoord;\nuniform sampler2D tex;\n" +
	"vec3 dec(vec3 c){ return mix(c/12.92, pow((c+0.055)/1.055, vec3(2.4)), step(0.04045, c)); }\n" +
	"vec3 enc(vec3 c){ return mix(c*12.92, 1.055*pow(c, vec3(1.0/2.4))-0.055, step(0.0031308, c)); }\n" +
	"vec3 lin(vec2 p){ return dec(texture2D(tex, p).rgb); }\n"

// The box pass writes a fixed reducedW×reducedH texture whatever the input
// size, so a window that changes size (e.g. goes fullscreen) never forces
// a renegotiation: each output texel averages an 8×8 grid over its cell.
const reducedW, reducedH = 256, 144

var boxShader = shaderHeader + fmt.Sprintf(`
void main(){
  vec2 cell = vec2(1.0/%d.0, 1.0/%d.0);
  vec2 base = floor(v_texcoord/cell)*cell;
  vec3 s = vec3(0.0);
  for(int y=0;y<8;y++) for(int x=0;x<8;x++) s += lin(base+(vec2(float(x),float(y))+0.5)/8.0*cell);
  gl_FragColor = vec4(enc(s/64.0), 1.0);
}`, reducedW, reducedH)

var regionShader = func() string {
	var b strings.Builder
	b.WriteString(shaderHeader)
	for i := range MaxRegions {
		fmt.Fprintf(&b, "uniform float r%[1]dx; uniform float r%[1]dy; uniform float r%[1]dw; uniform float r%[1]dh;\n", i)
	}
	b.WriteString("uniform int count;\nvoid main(){\n  int i = int(floor(v_texcoord.x*16.0));\n  if(i>=count){ gl_FragColor=vec4(0.0); return; }\n  vec4 r = vec4(0.0);\n")
	for i := range MaxRegions {
		fmt.Fprintf(&b, "  if(i==%[1]d) r=vec4(r%[1]dx,r%[1]dy,r%[1]dw,r%[1]dh);\n", i)
	}
	b.WriteString(`  vec3 s = vec3(0.0);
  for(int y=0;y<32;y++) for(int x=0;x<32;x++)
    s += lin(r.xy + (vec2(float(x),float(y))+0.5)/32.0*r.zw);
  gl_FragColor = vec4(enc(s/1024.0), 1.0);
}`)
	return b.String()
}()

// Source is one GPU frame source: a GStreamer fragment ending in caps
// glupload accepts, and where it sits on the target canvas (normalised).
type Source struct {
	Fragment string
	Canvas   Rect
}

// stream averages up to MaxRegions regions of one source on the GPU and
// reads back a single 16×1 RGBA row per frame.
type stream struct {
	src            Source
	pl             *pipeline
	box, reg, sink *C.GstElement
	buf            [MaxRegions * 4]byte
}

func openStream(src Source) (*stream, error) {
	desc := src.Fragment + " ! glupload ! glcolorconvert ! video/x-raw(memory:GLMemory),format=RGBA" +
		" ! glshader name=box ! " + fmt.Sprintf("video/x-raw(memory:GLMemory),width=%d,height=%d", reducedW, reducedH) +
		" ! glshader name=reg ! video/x-raw(memory:GLMemory),width=16,height=1" +
		" ! gldownload ! video/x-raw,format=RGBA ! appsink name=avg sync=false max-buffers=1 drop=true"
	pl, err := parsePipeline(desc)
	if err != nil {
		return nil, err
	}
	s := &stream{src: src, pl: pl}
	for name, dst := range map[string]**C.GstElement{"box": &s.box, "reg": &s.reg, "avg": &s.sink} {
		if *dst, err = pl.element(name); err != nil {
			s.close()
			return nil, err
		}
	}
	setString(s.box, "fragment", boxShader)
	setString(s.reg, "fragment", regionShader)
	s.setRegions(nil)
	return s, nil
}

// setRegions clips each canvas region to this source and uploads the
// result as uniforms. Regions that miss the source get zero size.
func (s *stream) setRegions(regions []Rect) {
	f := make(map[string]float32, MaxRegions*4)
	for i := range MaxRegions {
		var r Rect
		if i < len(regions) {
			if c := regions[i].intersect(s.src.Canvas); c.area() > 0 {
				r = c.within(s.src.Canvas)
			}
		}
		f[fmt.Sprintf("r%dx", i)] = float32(r.X)
		f[fmt.Sprintf("r%dy", i)] = float32(r.Y)
		f[fmt.Sprintf("r%dw", i)] = float32(r.W)
		f[fmt.Sprintf("r%dh", i)] = float32(r.H)
	}
	setUniforms(s.reg, f, map[string]int{"count": min(len(regions), MaxRegions)})
}

// weights returns, per region, the canvas area this source covers.
func (s *stream) weights(regions []Rect) []float64 {
	w := make([]float64, len(regions))
	for i, r := range regions {
		w[i] = r.intersect(s.src.Canvas).area()
	}
	return w
}

func (s *stream) start() error { return s.pl.play() }

// next waits up to timeout for a frame. ok is false on timeout.
func (s *stream) next(timeout time.Duration) (row []byte, ok bool, err error) {
	if err := s.pl.busProblem(); err != nil {
		return nil, false, err
	}
	n, err := pull(s.sink, timeout, s.buf[:])
	if err != nil || n < len(s.buf) {
		return nil, false, err
	}
	return s.buf[:], true, nil
}

func (s *stream) close() {
	s.pl.stop()
	for _, e := range []*C.GstElement{s.box, s.reg, s.sink} {
		if e != nil {
			unref(e)
		}
	}
	s.pl.free()
}
