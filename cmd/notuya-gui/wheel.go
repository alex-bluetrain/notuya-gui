package main

import (
	"encoding/binary"
	"math"
	"os"

	"github.com/diamondburned/gotk4/pkg/cairo"
)

const wheelSize = 300

// wheelStride is the ARGB32 row stride cairo uses for wheelSize. cairo aligns
// stride to a 4-byte boundary; for a 300px ARGB32 row that is exactly
// 300*4 = 1200, already aligned. FormatStrideForWidth confirms this at
// runtime in loadWheelSurface.
func generateWheelBytes(size, stride int) []byte {
	buf := make([]byte, stride*size)
	radius := float64(size) / 2.0
	twoPi := 2.0 * math.Pi
	for y := 0; y < size; y++ {
		rowOff := y * stride
		dy := float64(y) - radius
		for x := 0; x < size; x++ {
			dx := float64(x) - radius
			dist := math.Sqrt(dx*dx + dy*dy)
			if dist > radius {
				continue
			}
			hue := math.Mod(math.Atan2(dy, dx)/twoPi, 1.0)
			if hue < 0 {
				hue += 1.0
			}
			sat := dist / radius
			c := hsvRGB(hue, sat, 1.0)
			off := rowOff + x*4
			buf[off] = c.B   // B
			buf[off+1] = c.G // G
			buf[off+2] = c.R // R
			buf[off+3] = 255 // A
		}
	}
	return buf
}

// wheelBytes loads the raw BGRA buffer from cache, or generates and caches
// it. The cache file is prefixed with a little-endian (size, stride) header,
// matching picker.py's format so the two share the same .wheel_cache.bin.
func wheelBytes(cachePath string, size, stride int) []byte {
	expected := stride * size
	header := make([]byte, 8)
	binary.LittleEndian.PutUint32(header[0:4], uint32(size))
	binary.LittleEndian.PutUint32(header[4:8], uint32(stride))

	if raw, err := os.ReadFile(cachePath); err == nil {
		if len(raw) == len(header)+expected && string(raw[:len(header)]) == string(header) {
			return raw[len(header):]
		}
	}

	buf := generateWheelBytes(size, stride)
	out := make([]byte, 0, len(header)+len(buf))
	out = append(out, header...)
	out = append(out, buf...)
	_ = os.WriteFile(cachePath, out, 0o644)
	return buf
}

// loadWheelSurface returns a cairo ARGB32 surface holding the HSV wheel,
// loading or generating the cached bytes at cachePath.
func loadWheelSurface(cachePath string) *cairo.Surface {
	stride := cairo.FormatStrideForWidth(cairo.FormatARGB32, wheelSize)
	buf := wheelBytes(cachePath, wheelSize, stride)
	surface := cairo.CreateImageSurface(cairo.FormatARGB32, wheelSize, wheelSize)
	copy(surface.Data(), buf)
	surface.MarkDirty()
	return surface
}
