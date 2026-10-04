package main

import (
	coreglib "github.com/diamondburned/gotk4/pkg/glib/v2"
)

// devicePanel is a headless per-device status cache for the Rooms tab, which
// only needs each device's last-known on/off state to build its summary rows.
// It wraps one control (its persistent session) and caches the status the
// control publishes on each (re)connect. Device I/O runs off the GTK thread; results return via IdleAdd.
type devicePanel struct {
	ctl *control

	// lastOn / hasState mirror the most recent refresh so the Rooms tab can
	// summarise a room without issuing its own device query. onRefresh, when
	// set, fires after a refresh so the Rooms tab can recompute; onToggle
	// fires when a room master switch flips this device so the summary updates
	// optimistically without waiting for a refresh.
	lastOn    bool
	hasState  bool
	onRefresh func()
	onToggle  func(on bool)
}

func newDevicePanel(ctl *control) *devicePanel {
	return &devicePanel{ctl: ctl}
}

// subscribe caches the device's on/off state every time its control
// (re)connects, then notifies onRefresh on the GTK thread so the Rooms
// summary recomputes.
func (p *devicePanel) subscribe() {
	p.ctl.Subscribe(func(st deviceStatus) {
		coreglib.IdleAdd(func() {
			p.lastOn = st.On
			p.hasState = true
			if p.onRefresh != nil {
				p.onRefresh()
			}
		})
	})
}

// setPowerOptimistic reflects a power change made elsewhere (a room master
// switch) into this panel's cached state, without issuing a command. The
// actual command is sent by the caller.
func (p *devicePanel) setPowerOptimistic(on bool) {
	p.lastOn = on
	p.hasState = true
}
