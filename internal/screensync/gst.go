package screensync

// Thin cgo shim over the few GStreamer calls the engine needs. Using a
// shim instead of go-gst keeps a single GLib binding (gotk4's) in the
// process.

/*
#cgo pkg-config: gstreamer-1.0 gstreamer-app-1.0
#include <stdlib.h>
#include <string.h>
#include <gst/gst.h>
#include <gst/app/gstappsink.h>

static GstElement *ss_parse(const char *desc, char **errmsg) {
	GError *err = NULL;
	GstElement *p = gst_parse_launch(desc, &err);
	if (err != NULL) {
		*errmsg = g_strdup(err->message);
		g_error_free(err);
		if (p != NULL) gst_object_unref(p);
		return NULL;
	}
	return p;
}

static GstElement *ss_by_name(GstElement *p, const char *name) {
	return gst_bin_get_by_name(GST_BIN(p), name);
}

static void ss_set_string(GstElement *e, const char *prop, const char *v) {
	g_object_set(e, prop, v, NULL);
}

static GstStructure *ss_structure(void) { return gst_structure_new_empty("uniforms"); }

static void ss_put_float(GstStructure *s, const char *k, float v) {
	GValue g = G_VALUE_INIT;
	g_value_init(&g, G_TYPE_FLOAT);
	g_value_set_float(&g, v);
	gst_structure_take_value(s, k, &g);
}

static void ss_put_int(GstStructure *s, const char *k, int v) {
	GValue g = G_VALUE_INIT;
	g_value_init(&g, G_TYPE_INT);
	g_value_set_int(&g, v);
	gst_structure_take_value(s, k, &g);
}

// Sets and frees s; g_object_set copies the boxed structure.
static void ss_set_uniforms(GstElement *e, GstStructure *s) {
	g_object_set(e, "uniforms", s, NULL);
	gst_structure_free(s);
}

static int ss_set_state(GstElement *e, int state) {
	return gst_element_set_state(e, (GstState)state) != GST_STATE_CHANGE_FAILURE;
}

// Pulls one sample and copies up to n bytes of it into out.
// Returns bytes copied, 0 on timeout, -1 at EOS.
static int ss_pull(GstElement *sink, guint64 timeout, unsigned char *out, int n) {
	GstSample *s = gst_app_sink_try_pull_sample(GST_APP_SINK(sink), timeout);
	if (s == NULL) return gst_app_sink_is_eos(GST_APP_SINK(sink)) ? -1 : 0;
	GstBuffer *b = gst_sample_get_buffer(s);
	GstMapInfo m;
	int got = 0;
	if (b != NULL && gst_buffer_map(b, &m, GST_MAP_READ)) {
		got = m.size < (gsize)n ? (int)m.size : n;
		memcpy(out, m.data, got);
		gst_buffer_unmap(b, &m);
	}
	gst_sample_unref(s);
	return got;
}

// Waits for a sample without mapping it: 1 = got one, 0 = timeout, -1 = EOS.
static int ss_wait_sample(GstElement *sink, guint64 timeout) {
	GstSample *s = gst_app_sink_try_pull_sample(GST_APP_SINK(sink), timeout);
	if (s == NULL) return gst_app_sink_is_eos(GST_APP_SINK(sink)) ? -1 : 0;
	gst_sample_unref(s);
	return 1;
}

// Returns a newly allocated error/EOS description, or NULL when the bus
// holds neither.
static char *ss_bus_problem(GstElement *p) {
	GstBus *bus = gst_element_get_bus(p);
	GstMessage *msg = gst_bus_pop_filtered(bus, GST_MESSAGE_ERROR | GST_MESSAGE_EOS);
	gst_object_unref(bus);
	if (msg == NULL) return NULL;
	char *out;
	if (GST_MESSAGE_TYPE(msg) == GST_MESSAGE_EOS) {
		out = g_strdup("end of stream");
	} else {
		GError *err = NULL;
		gst_message_parse_error(msg, &err, NULL);
		out = g_strdup(err->message);
		g_error_free(err);
	}
	gst_message_unref(msg);
	return out;
}

static void ss_unref(GstElement *e) { gst_object_unref(e); }

*/
import "C"

import (
	"errors"
	"sync"
	"time"
	"unsafe"
)

var initOnce sync.Once

func gstInit() {
	initOnce.Do(func() { C.gst_init(nil, nil) })
}

type pipeline struct {
	p *C.GstElement
}

func parsePipeline(desc string) (*pipeline, error) {
	gstInit()
	cdesc := C.CString(desc)
	defer C.free(unsafe.Pointer(cdesc))
	var cerr *C.char
	p := C.ss_parse(cdesc, &cerr)
	if p == nil {
		defer C.g_free(C.gpointer(cerr))
		return nil, errors.New("gstreamer: " + C.GoString(cerr))
	}
	return &pipeline{p: p}, nil
}

func (pl *pipeline) element(name string) (*C.GstElement, error) {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	e := C.ss_by_name(pl.p, cname)
	if e == nil {
		return nil, errors.New("gstreamer: no element " + name)
	}
	return e, nil
}

func setString(e *C.GstElement, prop, v string) {
	cp, cv := C.CString(prop), C.CString(v)
	defer C.free(unsafe.Pointer(cp))
	defer C.free(unsafe.Pointer(cv))
	C.ss_set_string(e, cp, cv)
}

// setUniforms replaces a glshader's uniforms. Floats must be G_TYPE_FLOAT
// or glshader silently ignores them.
func setUniforms(e *C.GstElement, floats map[string]float32, ints map[string]int) {
	s := C.ss_structure()
	for k, v := range floats {
		ck := C.CString(k)
		C.ss_put_float(s, ck, C.float(v))
		C.free(unsafe.Pointer(ck))
	}
	for k, v := range ints {
		ck := C.CString(k)
		C.ss_put_int(s, ck, C.int(v))
		C.free(unsafe.Pointer(ck))
	}
	C.ss_set_uniforms(e, s)
}

func (pl *pipeline) play() error {
	if C.ss_set_state(pl.p, C.int(C.GST_STATE_PLAYING)) == 0 {
		return errors.New("gstreamer: pipeline refused to start")
	}
	return nil
}

func (pl *pipeline) stop() {
	C.ss_set_state(pl.p, C.int(C.GST_STATE_NULL))
}

func (pl *pipeline) free() {
	C.ss_unref(pl.p)
}

// pull returns the next sample's bytes (up to len(out)), 0 on timeout,
// or errEOS once the stream has ended.
func pull(sink *C.GstElement, timeout time.Duration, out []byte) (int, error) {
	n := C.ss_pull(sink, C.guint64(timeout.Nanoseconds()), (*C.uchar)(unsafe.Pointer(&out[0])), C.int(len(out)))
	if n < 0 {
		return 0, errEOS
	}
	return int(n), nil
}

// waitSample reports whether sink produced a sample within timeout,
// without mapping it (GPU memory stays on the GPU).
func waitSample(sink *C.GstElement, timeout time.Duration) (bool, error) {
	switch C.ss_wait_sample(sink, C.guint64(timeout.Nanoseconds())) {
	case 1:
		return true, nil
	case -1:
		return false, errEOS
	}
	return false, nil
}

func (pl *pipeline) busProblem() error {
	msg := C.ss_bus_problem(pl.p)
	if msg == nil {
		return nil
	}
	defer C.g_free(C.gpointer(msg))
	return errors.New("gstreamer: " + C.GoString(msg))
}

func unref(e *C.GstElement) { C.ss_unref(e) }

var errEOS = errors.New("gstreamer: end of stream")
