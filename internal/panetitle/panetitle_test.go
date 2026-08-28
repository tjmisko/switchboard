package panetitle

import (
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	for _, glyph := range SpinnerGlyphs {
		for _, tc := range []struct {
			name  string
			input string
			want  string
		}{
			{"with space", string(glyph) + " project", "project"},
			{"without space", string(glyph) + "project", "project"},
			{"glyph only", string(glyph), ""},
		} {
			t.Run(string(glyph)+"/"+tc.name, func(t *testing.T) {
				if got := Normalize(tc.input); got != tc.want {
					t.Fatalf("Normalize(%q) = %q, want %q", tc.input, got, tc.want)
				}
			})
		}
	}

	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"whitespace", " \t\n", ""},
		{"normal", "  project name  ", "project name"},
		{"interior glyph", "project ⠋ name", "project ⠋ name"},
		{"other emoji", " 🚀 project ", "🚀 project"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Normalize(tc.input); got != tc.want {
				t.Fatalf("Normalize(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestNormalizeIsIdempotent(t *testing.T) {
	inputs := []string{"", " \t", "project", "project ⠋ name", "🚀 project", "⠋ ⠙ foo"}
	for _, glyph := range SpinnerGlyphs {
		inputs = append(inputs, string(glyph)+" project", string(glyph)+"project", string(glyph))
	}
	for _, input := range inputs {
		first := Normalize(input)
		if second := Normalize(first); second != first {
			t.Fatalf("Normalize is not idempotent for %q: first=%q second=%q", input, first, second)
		}
	}
}

func TestSpinnerGlyphsCoversTheLegacyLabelTable(t *testing.T) {
	for _, glyph := range "✳⠂⠐⠁⠈⠠⠄⡀⢀" {
		if !strings.ContainsRune(SpinnerGlyphs, glyph) {
			t.Errorf("SpinnerGlyphs does not contain legacy label glyph %q", glyph)
		}
	}
}
