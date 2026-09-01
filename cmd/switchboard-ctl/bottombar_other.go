//go:build !linux

package main

// cmdBottombar is intentionally unavailable outside Linux. The session and
// presentation contracts are portable; pidfds, realtime Waybar signals, and
// Hyprland visibility are one Linux adapter rather than controller-wide
// assumptions.
func cmdBottombar(_ []string, _ string) {
	fail("bottombar: the Waybar/Hyprland adapter is available only on Linux")
}
