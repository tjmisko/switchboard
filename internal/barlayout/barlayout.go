// Package barlayout fits the bottom-bar session chips onto the single bar by
// abbreviating labels with an ellipsis when the row gets crowded. Waybar lays
// its modules out in one fixed-width row with no wrapping, so when the chips
// would overflow we shorten the longest ones just enough to fit.
//
// The fit is a pure function of the labels, the usable width, and fixed chip
// metrics — no time, no process state — so every slot renderer computes the
// same abbreviation and the result never flickers.
package barlayout

import (
	"encoding/json"
	"os/exec"
	"sort"
	"unicode/utf8"
)

// ellipsis is the single rune appended to an abbreviated label. It is one
// monospace cell wide, like any other glyph, so it costs one CharPx.
const ellipsis = "…"

// minLabelRunes is the floor on an abbreviated label's length (including the
// ellipsis). Below this a chip is unreadable, so we stop shrinking and accept a
// little overflow rather than render "…" alone.
const minLabelRunes = 3

// Metrics models a chip's pixel footprint in the bottom bar.
type Metrics struct {
	CharPx      float64 // horizontal advance of one monospace glyph
	ChipFixedPx float64 // per-chip overhead: padding + border + margin + inter-chip gap
}

// DefaultMetrics returns the chip footprint, calibrated by measuring rendered
// chip widths on the live bottom bar (grim screenshot, logical px):
//
//	chars  width      chars  width
//	  23   206          32   277
//	  25   221          34   293
//	  27   237
//
// A linear fit gives ~7.95 px per glyph and ~22.7px of chip overhead
// (padding+border+margin); the chips sit ~9.5px apart, so folding that gap into
// the per-chip footprint gives ChipFixedPx ≈ 32. CharPx is rounded up slightly
// so the estimate errs toward abbreviating rather than overflowing.
func DefaultMetrics() Metrics {
	return Metrics{CharPx: 8.0, ChipFixedPx: 32}
}

// chipWidth estimates the pixel width of a chip whose label has the given rune
// count.
func chipWidthRunes(runes int, m Metrics) float64 {
	return m.ChipFixedPx + float64(runes)*m.CharPx
}

// chipWidth estimates the pixel width of a single chip's label.
func chipWidth(label string, m Metrics) float64 {
	return chipWidthRunes(utf8.RuneCountInString(label), m)
}

// Fit returns the labels abbreviated so the chips fit within availPx. If they
// already fit, the labels are returned unchanged. Otherwise the widest labels
// are trimmed with a trailing ellipsis (max-min fairness): short labels are
// left intact and only the long ones are shortened, each only as much as
// needed. The returned slice has the same length and order as labels.
func Fit(labels []string, availPx float64, m Metrics) []string {
	n := len(labels)
	if n == 0 {
		return labels
	}
	widths := make([]float64, n)
	total := 0.0
	for i, l := range labels {
		widths[i] = chipWidth(l, m)
		total += widths[i]
	}
	if total <= availPx {
		return labels
	}

	capPx := fairCap(widths, availPx)
	out := make([]string, n)
	for i, l := range labels {
		if widths[i] <= capPx {
			out[i] = l
			continue
		}
		out[i] = abbreviate(l, capPx, m)
	}
	return out
}

// fairCap returns the max-min fair per-chip width cap: the largest cap such
// that giving every chip min(naturalWidth, cap) keeps the total within availPx.
// Chips narrower than the cap keep their full width; the freed space is shared
// among the wider chips. Assumes the labels do not already fit (total > avail),
// so the loop always finds a binding cap.
func fairCap(widths []float64, availPx float64) float64 {
	asc := append([]float64(nil), widths...)
	sort.Float64s(asc)

	remaining := availPx
	n := len(asc)
	for i, w := range asc {
		cap := remaining / float64(n-i)
		if w <= cap {
			remaining -= w // this chip fits under the cap; it keeps its width
			continue
		}
		return cap // this and every wider chip are capped here
	}
	// Unreachable when total > avail, but return a sane value defensively.
	return remaining / float64(n)
}

// abbreviate trims label to fit within capPx, ending in an ellipsis. It never
// shrinks below minLabelRunes runes (ellipsis included), accepting slight
// overflow when the row is extremely crowded rather than rendering an
// unreadable stub.
func abbreviate(label string, capPx float64, m Metrics) string {
	runes := []rune(label)
	maxRunes := int((capPx - m.ChipFixedPx) / m.CharPx)
	if maxRunes < minLabelRunes {
		maxRunes = minLabelRunes
	}
	if maxRunes >= len(runes) {
		return label
	}
	return string(runes[:maxRunes-1]) + ellipsis
}

// safetyMarginPx is trimmed off the monitor's logical width so chips packed
// right up to the edge are not clipped by the bar's rounding/insets, and so the
// abbreviation errs toward fitting.
const safetyMarginPx = 32

// fallbackWidthPx is the usable width assumed when the monitor cannot be
// queried (e.g. hyprctl missing). Chosen on the wide side so an unknown
// environment errs toward NOT abbreviating (chips render full, as before).
const fallbackWidthPx = 1920

// hyprMonitor is the slice of `hyprctl monitors -j` we need.
type hyprMonitor struct {
	Width   int     `json:"width"`
	Scale   float64 `json:"scale"`
	Focused bool    `json:"focused"`
}

// ScreenWidthPx returns the usable logical width (in CSS/GTK pixels) of the
// focused monitor, less a safety margin. It shells out to hyprctl; if that
// fails it returns the fallback. Callers query it once at startup — the monitor
// width is stable for the bar's lifetime — so the abbreviation never flickers.
func ScreenWidthPx() float64 {
	if w, ok := focusedLogicalWidth(); ok {
		return w - safetyMarginPx
	}
	return fallbackWidthPx - safetyMarginPx
}

// focusedLogicalWidth queries hyprctl for the focused monitor's logical width
// (physical width / scale). The bool is false if hyprctl is unavailable or
// reports no monitors.
func focusedLogicalWidth() (float64, bool) {
	out, err := exec.Command("hyprctl", "monitors", "-j").Output()
	if err != nil {
		return 0, false
	}
	var mons []hyprMonitor
	if json.Unmarshal(out, &mons) != nil || len(mons) == 0 {
		return 0, false
	}
	pick := mons[0]
	for _, m := range mons {
		if m.Focused {
			pick = m
			break
		}
	}
	if pick.Width <= 0 || pick.Scale <= 0 {
		return 0, false
	}
	return float64(pick.Width) / pick.Scale, true
}
