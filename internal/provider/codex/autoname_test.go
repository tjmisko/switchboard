package codex

import "testing"

func TestNormalizeGeneratedName(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
		ok    bool
	}{
		{"`Fix The API!!!`", "fix-the-api", true},
		{"one", "", false},
		{"one two three four five six", "one-two-three-four-five", true},
		{"  rename___while   pending ", "rename-while-pending", true},
		{"symbols only !!!", "symbols-only", true},
	} {
		got, ok := NormalizeGeneratedName(test.input)
		if got != test.want || ok != test.ok {
			t.Errorf("NormalizeGeneratedName(%q) = %q, %t; want %q, %t", test.input, got, ok, test.want, test.ok)
		}
	}
}

func TestFallbackNameIsDeterministicAndValid(t *testing.T) {
	first := FallbackName("Please fix the flaky integration tests in RPC")
	second := FallbackName("Please fix the flaky integration tests in RPC")
	if first != second {
		t.Fatalf("fallback is not deterministic: %q != %q", first, second)
	}
	if normalized, ok := NormalizeGeneratedName(first); !ok || normalized != first {
		t.Fatalf("fallback %q does not satisfy naming contract", first)
	}
	if got := FallbackName("!!!"); got == "" {
		t.Fatal("punctuation-only prompt produced an empty fallback")
	}
}
