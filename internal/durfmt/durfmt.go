// Package durfmt formats a status age into the compact, low-jitter string the
// renderers show on hover ("45s", "3m", "1h04m", "2d3h"). It is the single home
// for that vocabulary so the waybar tooltip and the reference TUI agree.
package durfmt

import (
	"fmt"
	"time"
)

// Compact renders d as a short human duration. Resolution coarsens with
// magnitude to keep the hover counter from flickering at the daemon's snapshot
// cadence: seconds only under a minute, whole minutes under an hour, then
// h+m, then d+h. A negative d (clock skew between daemon and renderer) clamps to
// "0s" rather than printing a nonsense negative.
func Compact(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d / time.Hour)
		m := int((d % time.Hour) / time.Minute)
		return fmt.Sprintf("%dh%02dm", h, m)
	default:
		days := int(d / (24 * time.Hour))
		h := int((d % (24 * time.Hour)) / time.Hour)
		return fmt.Sprintf("%dd%dh", days, h)
	}
}

// Since renders the age of an instant relative to now via Compact. A zero or nil
// since yields "" (the status timestamp is unknown — show no counter).
func Since(since *time.Time, now time.Time) string {
	if since == nil || since.IsZero() {
		return ""
	}
	return Compact(now.Sub(*since))
}

// Coarse renders d like Compact but never at second resolution: under a minute
// it is "<1m", so the string it returns changes at most once per minute.
//
// That ceiling is the point, not a rounding convenience. A waybar tooltip is
// part of the module's JSON, so any change to it makes waybar re-render the
// module — which dismisses an open hover. A counter at second resolution
// therefore rewrites the tooltip once a second, and a hover over that chip
// flickers out and back for as long as the counter is young. Measured on the
// live bar before this existed: one session with twelve subagent rows emitted
// 14 lines in 12s, 13 of them tooltip-only, 12 of those attributable to the
// single agent whose age was under a minute.
//
// Use Coarse for anything a hover displays; Compact remains right for a
// full-screen renderer that repaints on its own schedule.
func Coarse(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	return Compact(d)
}

// CoarseSince renders the age of an instant relative to now via Coarse. A zero
// or nil since yields "" (the timestamp is unknown — show no counter).
func CoarseSince(since *time.Time, now time.Time) string {
	if since == nil || since.IsZero() {
		return ""
	}
	return Coarse(now.Sub(*since))
}
