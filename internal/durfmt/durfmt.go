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
