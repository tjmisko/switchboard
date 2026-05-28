package wm

import (
	"testing"

	"github.com/jezek/xgb/xproto"
)

// parseWindowIDs decodes a _NET_CLIENT_LIST value (little-endian 32-bit ids).
func TestParseWindowIDs(t *testing.T) {
	// window 1 and window 0x0a000001, each little-endian.
	buf := []byte{0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x0a}
	got := parseWindowIDs(buf)
	if len(got) != 2 {
		t.Fatalf("got %d windows, want 2", len(got))
	}
	if got[0] != 1 || got[1] != 0x0a000001 {
		t.Errorf("ids = %v, want [1 0x0a000001]", got)
	}

	// A trailing partial word is ignored (no panic, no phantom id).
	if n := len(parseWindowIDs([]byte{0x01, 0x00, 0x00})); n != 0 {
		t.Errorf("partial word produced %d ids, want 0", n)
	}
}

// The opaque address round-trips through format/parse; parse also tolerates a
// 0x-hex ref defensively.
func TestX11WindowAddressRoundTrip(t *testing.T) {
	for _, w := range []xproto.Window{1, 0x0a000001, 4294967295} {
		got, err := parseX11Window(formatX11Window(w))
		if err != nil {
			t.Fatalf("parse(format(%d)): %v", w, err)
		}
		if got != w {
			t.Errorf("round-trip %d = %d", w, got)
		}
	}
	if got, err := parseX11Window("0x0a000001"); err != nil || got != 0x0a000001 {
		t.Errorf("parse hex = %d, %v; want 0x0a000001", got, err)
	}
	if _, err := parseX11Window("not-a-window"); err == nil {
		t.Error("expected error parsing garbage address")
	}
}
