package barlayout

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// unitMetrics makes a chip's width equal its rune count: 1px per glyph, no
// fixed overhead. That keeps the fit arithmetic obvious in the table.
var unitMetrics = Metrics{CharPx: 1, ChipFixedPx: 0}

// label of n runes.
func label(n int) string { return strings.Repeat("x", n) }

func TestFitLeavesLabelsWhenTheyAlreadyFit(t *testing.T) {
	labels := []string{"ab", "cde", "f"}
	got := Fit(labels, 100, unitMetrics)
	for i := range labels {
		if got[i] != labels[i] {
			t.Errorf("label %d = %q, want unchanged %q", i, got[i], labels[i])
		}
	}
}

func TestFitAbbreviatesOnlyTheLongLabels(t *testing.T) {
	// Two chips, widths 2 and 8, only 6px of room. Max-min cap is 4: the short
	// chip keeps its width, the long one is trimmed to 4 runes incl. ellipsis.
	got := Fit([]string{label(2), label(8)}, 6, unitMetrics)
	if got[0] != label(2) {
		t.Errorf("short label was abbreviated: %q", got[0])
	}
	if want := "xxx" + ellipsis; got[1] != want {
		t.Errorf("long label = %q, want %q", got[1], want)
	}
	if total := width(got, unitMetrics); total > 6 {
		t.Errorf("fitted total width %v exceeds avail 6", total)
	}
}

func TestFitEndsAbbreviatedLabelsWithEllipsis(t *testing.T) {
	got := Fit([]string{label(20), label(20)}, 12, unitMetrics)
	for i, g := range got {
		if !strings.HasSuffix(g, ellipsis) {
			t.Errorf("label %d = %q, want trailing ellipsis", i, g)
		}
	}
}

func TestFitRespectsMinLabelRunes(t *testing.T) {
	// Almost no room, but an abbreviated label never shrinks below the floor.
	got := Fit([]string{label(40), label(40), label(40)}, 1, unitMetrics)
	for i, g := range got {
		if n := utf8.RuneCountInString(g); n < minLabelRunes {
			t.Errorf("label %d = %q has %d runes, want >= %d", i, g, n, minLabelRunes)
		}
	}
}

// The bug this guards: the fit used to cap chips in pixels and then floor the
// cap to whole runes, so every truncated chip discarded its fractional rune and
// the row's leftover surfaced as dead margin at the edges of the bar.
func TestFitSpendsTheWholeRuneBudgetWhenCrowded(t *testing.T) {
	// Three 10-rune labels into 20 cells: the fair cap is 6 (18 cells), leaving
	// 2 unclaimed. Those two must reach chips, not the margin.
	got := Fit([]string{label(10), label(10), label(10)}, 20, unitMetrics)
	total := 0
	for _, g := range got {
		total += utf8.RuneCountInString(g)
	}
	if total != 20 {
		t.Errorf("fitted labels use %d of 20 available cells, want all 20 (%q)", total, got)
	}
}

func TestFitCompletesALabelThatIsOneRuneShort(t *testing.T) {
	// Budget 9 caps both chips at 4. The single leftover cell is worth two
	// characters on the short label (it also retires the ellipsis), so it goes
	// there rather than to the label that would still be truncated.
	got := Fit([]string{label(20), label(5)}, 9, unitMetrics)
	if got[1] != label(5) {
		t.Errorf("one-rune-short label = %q, want it completed to %q", got[1], label(5))
	}
	if !strings.HasSuffix(got[0], ellipsis) {
		t.Errorf("long label = %q, want it to stay abbreviated", got[0])
	}
}

func TestFitStaysWithinAvailableWidthWithRealMetrics(t *testing.T) {
	// The eight-session row that motivated the change, on a 1512px logical
	// screen: long project-prefixed names, one short one.
	labels := []string{
		"sb-dash-chart-animation-enhancement",
		"switchboard-ae",
		"jobfeed-tune-job-feed-filter",
		"jobfeed-expand-screener-data-sources",
		"tjmisko-github-io-retend-timeline-integration",
		"tjmisko-github-io-mobile-trajectory-redesign",
		"arachne-ascii-dag-monitor-ui",
		"digestdownloads-status-update-request",
	}
	const avail float64 = 1512 - safetyMarginPx
	m := DefaultMetrics()
	got := Fit(labels, avail, m)
	if total := width(got, m); total > avail {
		t.Errorf("fitted row is %.1fpx wide, want <= %.1f", total, avail)
	}
	// And it must actually use the room: less than one chip's slack left over.
	if total := width(got, m); avail-total > m.ChipFixedPx+m.CharPx {
		t.Errorf("fitted row wastes %.1fpx of %.1f available", avail-total, avail)
	}
}

func TestFitEmptyInput(t *testing.T) {
	if got := Fit(nil, 100, unitMetrics); got != nil {
		t.Errorf("Fit(nil) = %v, want nil", got)
	}
}

// width sums the chip widths of the given labels.
func width(labels []string, m Metrics) float64 {
	total := 0.0
	for _, l := range labels {
		total += chipWidth(l, m)
	}
	return total
}

// chipWidth includes the fixed per-chip overhead on top of the glyph advance.
func TestChipWidthIncludesFixedOverhead(t *testing.T) {
	m := Metrics{CharPx: 7.2, ChipFixedPx: 30}
	if got, want := chipWidth("abcd", m), 30+4*7.2; got != want {
		t.Errorf("chipWidth = %v, want %v", got, want)
	}
}
