package main

import (
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// syncTab is the Windows placeholder for the Screen Sync tab. The real tab is
// Linux-only (see screen_sync_tab_linux.go); Windows has no capture, tracking
// or overlay backend yet, so this holds no state and does nothing on shutdown.
// It exists only so app.go, which is shared across operating systems, can keep
// referring to *syncTab and calling shutdown().
type syncTab struct{}

func (t *syncTab) shutdown() {}

// buildScreenSyncTab shows a notice that Screen Sync is not implemented on
// Windows yet. It still assigns a.sync so the shared shutdown path never sees a
// nil *syncTab.
func (a *desktopApp) buildScreenSyncTab() gtk.Widgetter {
	a.sync = &syncTab{}

	page := adw.NewStatusPage()
	page.SetTitle("Screen Sync")
	page.SetDescription("Screen Sync isn't available on Windows yet.")
	page.SetIconName("video-display-symbolic")
	return page
}
