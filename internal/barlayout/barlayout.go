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

// DefaultMetrics returns the chip footprint. Both numbers are derived from the
// bar's CSS box and confirmed against rendered chip widths measured on the live
// bar (grim screenshot, logical px):
//
//	a 14-glyph chip renders 133px wide; subtracting the CSS box that chip
//	was measured against (22px: padding 2×7 + border 2×1 + margin 2×2 +
//	spacing 2) leaves 7.93px per glyph. The box has since widened by 2px of
//	padding, which moves ChipFixedPx but not the per-glyph advance.
//
// ChipFixedPx is the non-text part of one chip plus its share of the gap to the
// next one, straight out of ~/.config/waybar/style.css and claude.jsonc:
//
//	padding 2×8 = 16 · border 2×1 = 2 · margin 2×2 = 4 · spacing = 2  →  24
//
// ONE number covers both chip variants on purpose. A remote session is drawn as
// a nested pill — `border: 3px double` instead of `1px solid` — and the CSS
// pays for those two extra pixels of ring out of the chip's own padding
// (2×6 + 2×3 = 2×8 + 2×1), so a remote chip's box is the same width as a local
// one to the pixel. That is what lets Fit stay a single shared pool of glyph
// cells: if the two variants had different overheads, the budget would depend on
// how many of the row's sessions happened to be remote, and every chip on the
// bar would re-abbreviate whenever a remote session came or went. Keep the
// trade balanced in style.css, or restore the divergence here deliberately.
//
// CharPx is set slightly above the measured 7.93 because the ellipsis glyph
// comes from a fallback font and is ~1.7px wider than a monospace cell;
// amortized over a ~20-rune chip that is ~0.1px per glyph. Rounding up here
// makes the estimate err toward abbreviating rather than overflowing the row.
func DefaultMetrics() Metrics {
	return Metrics{CharPx: 8.05, ChipFixedPx: 24}
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
//
// The fit is computed in runes, not pixels. Every chip pays the same fixed
// overhead, so once that is deducted the row is a single pool of glyph cells to
// share out — and sharing whole cells means no chip rounds its allowance down
// and silently donates the remainder back to the bar's empty margin.
func Fit(labels []string, availPx float64, m Metrics) []string {
	n := len(labels)
	if n == 0 {
		return labels
	}
	lens := make([]int, n)
	total := 0
	for i, l := range labels {
		lens[i] = utf8.RuneCountInString(l)
		total += lens[i]
	}
	budget := runeBudget(n, availPx, m)
	if total <= budget {
		return labels
	}

	allow := allowances(lens, budget)
	out := make([]string, n)
	for i, l := range labels {
		if allow[i] >= lens[i] {
			out[i] = l
			continue
		}
		out[i] = string([]rune(l)[:allow[i]-1]) + ellipsis
	}
	return out
}

// runeBudget is how many glyph cells the whole row can show once every chip has
// paid its fixed overhead. It never drops below the point where each chip can
// still render minLabelRunes: past that we accept a little overflow rather than
// a row of bare ellipses.
func runeBudget(n int, availPx float64, m Metrics) int {
	if m.CharPx <= 0 {
		return n * minLabelRunes
	}
	budget := int((availPx - float64(n)*m.ChipFixedPx) / m.CharPx)
	if floor := n * minLabelRunes; budget < floor {
		return floor
	}
	return budget
}

// allowances shares budget glyph cells among labels of the given natural
// lengths, max-min fair: every label under the cap keeps its full length and
// the cells it did not need are pooled for the longer ones. Assumes the labels
// do not already fit (sum(lens) > budget), so a binding cap always exists.
func allowances(lens []int, budget int) []int {
	n := len(lens)
	asc := append([]int(nil), lens...)
	sort.Ints(asc)

	capRunes := 0
	remaining := budget
	for i, l := range asc {
		c := remaining / (n - i)
		if l <= c {
			remaining -= l // fits under the cap; keeps its full length
			continue
		}
		capRunes = c // this and every longer label are capped here
		break
	}
	if capRunes < minLabelRunes {
		capRunes = minLabelRunes
	}

	out := make([]int, n)
	spent := 0
	for i, l := range lens {
		out[i] = min(l, capRunes)
		spent += out[i]
	}
	grow(out, lens, budget-spent)
	return out
}

// grow hands out the cells left over after the integer cap — the fair cap is a
// whole number, so up to one cell per capped chip is still unclaimed. A chip one
// cell short of its full label is served first: that cell buys back two
// characters, since it also retires the ellipsis. The rest go round-robin by
// index, which keeps the result stable across renders (every slot process
// computes this independently, so an unstable rule would flicker).
func grow(out, lens []int, left int) {
	for left > 0 {
		gave := false
		for i := range out {
			if left > 0 && out[i] == lens[i]-1 {
				out[i]++
				left--
				gave = true
			}
		}
		for i := range out {
			if left > 0 && out[i] < lens[i] {
				out[i]++
				left--
				gave = true
			}
		}
		if !gave {
			return // every label is already full
		}
	}
}

// safetyMarginPx is trimmed off the monitor's logical width so chips packed
// right up to the edge are not clipped by the bar's rounding/insets. The chip
// metrics are calibrated against the live bar and err high by ~1px per chip, so
// this only has to absorb the bar's own insets, not model error.
const safetyMarginPx = 12

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
