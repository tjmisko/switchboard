// Package panetitle normalizes terminal window titles used as presentation and
// best-effort window-manager join keys.
package panetitle

import (
	"strings"
	"unicode/utf8"
)

// SpinnerGlyphs contains the leading activity glyphs coding agents place in
// pane and window titles. They are presentation, not title identity.
const SpinnerGlyphs = "◐◑◒◓⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏✳⠂⠐⠁⠈⠠⠄⡀⢀"

// Normalize removes leading activity glyphs and surrounding whitespace. It
// strips repeatedly so applying it at both the terminal and window-manager
// seams is a fixed point, including for malformed multi-spinner prefixes.
func Normalize(title string) string {
	for {
		title = strings.TrimSpace(title)
		if title == "" {
			return ""
		}
		r, size := utf8.DecodeRuneInString(title)
		if !strings.ContainsRune(SpinnerGlyphs, r) {
			return title
		}
		title = title[size:]
	}
}
