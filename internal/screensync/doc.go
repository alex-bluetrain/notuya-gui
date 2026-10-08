// Package screensync drives the Screen Sync feature: it captures a monitor or
// window, averages configured regions on the GPU, and streams the resulting
// colours to the bulbs.
//
// The implementation is Linux-only (cgo GStreamer, the xdg-desktop-portal
// capture, and Hyprland's IPC sockets), so every source file carries the
// _linux suffix. This file has no suffix so the package still exists on other
// operating systems with nothing to compile — a Windows backend will add its
// own _windows files here when it is built. Without this file, a non-Linux
// build fails with "build constraints exclude all Go files".
package screensync
