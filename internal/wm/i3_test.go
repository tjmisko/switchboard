package wm

import (
	"bytes"
	"testing"
)

// i3 framing round-trips: a written message reads back with the same type and
// payload (native byte order on both ends).
func TestI3MessageFramingRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte(`["window","workspace"]`)
	if err := writeI3Message(&buf, i3Subscribe, payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	gotType, gotPayload, err := readI3Message(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if gotType != uint32(i3Subscribe) {
		t.Errorf("type = %d, want %d", gotType, i3Subscribe)
	}
	if !bytes.Equal(gotPayload, payload) {
		t.Errorf("payload = %q, want %q", gotPayload, payload)
	}
}

func TestReadI3MessageRejectsBadMagic(t *testing.T) {
	// 14-byte header with the wrong magic, zero length.
	bad := append([]byte("x-ipc-"), make([]byte, 8)...)
	if _, _, err := readI3Message(bytes.NewReader(bad)); err == nil {
		t.Error("expected error on bad magic, got nil")
	}
}

// A representative GET_TREE: one sway-style window (app_id + pid, window null)
// on workspace 1, one i3-style window (X11 window id, no pid) on workspace 2,
// plus non-window containers that must be skipped.
const sampleTree = `{
  "id": 1, "type": "root", "name": "root", "nodes": [
    {"id": 2, "type": "output", "name": "eDP-1", "nodes": [
      {"id": 3, "type": "workspace", "name": "1", "num": 1, "nodes": [
        {"id": 100, "type": "con", "name": "claude — proj", "app_id": "org.wezfurlong.wezterm",
         "pid": 4790, "focused": true, "nodes": [], "floating_nodes": []}
      ], "floating_nodes": []},
      {"id": 4, "type": "workspace", "name": "2", "num": 2, "nodes": [
        {"id": 200, "type": "con", "name": "vim", "window": 12582913,
         "window_properties": {"title": "vim"}, "focused": false, "nodes": [], "floating_nodes": []}
      ], "floating_nodes": []}
    ], "floating_nodes": []}
  ], "floating_nodes": []
}`

func TestParseI3Tree(t *testing.T) {
	got, err := parseI3Tree([]byte(sampleTree))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d windows, want 2 (containers/workspaces must be skipped): %+v", len(got), got)
	}

	sway := got[0]
	if sway.Address != "100" || sway.PID != 4790 {
		t.Errorf("sway window = {addr %s pid %d}, want {100 4790}", sway.Address, sway.PID)
	}
	if sway.Title != "claude — proj" {
		t.Errorf("sway title = %q", sway.Title)
	}
	if sway.Workspace != "1" || sway.WorkspaceID != 1 {
		t.Errorf("sway workspace = %q/%d, want 1/1", sway.Workspace, sway.WorkspaceID)
	}

	i3 := got[1]
	if i3.Address != "200" {
		t.Errorf("i3 window addr = %s, want 200", i3.Address)
	}
	// i3 omits pid → 0 (Observe-only join), the documented limitation.
	if i3.PID != 0 {
		t.Errorf("i3 window pid = %d, want 0 (i3 omits pid)", i3.PID)
	}
	if i3.WorkspaceID != 2 {
		t.Errorf("i3 workspace id = %d, want 2", i3.WorkspaceID)
	}
}

func TestParseI3Active(t *testing.T) {
	addr, err := parseI3Active([]byte(sampleTree))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if addr != "100" {
		t.Errorf("active = %q, want 100 (the focused window leaf)", addr)
	}
}

func TestTranslateI3Event(t *testing.T) {
	tests := []struct {
		name     string
		evType   uint32
		payload  string
		wantKind EventKind
		wantAddr string
		wantOK   bool
	}{
		{"window close", i3EventWindow, `{"change":"close","container":{"id":100}}`, EventWindowClosed, "100", true},
		{"window focus", i3EventWindow, `{"change":"focus","container":{"id":200}}`, EventFocusChanged, "200", true},
		{"window title", i3EventWindow, `{"change":"title","container":{"id":200}}`, EventLayoutChanged, "", true},
		{"window move", i3EventWindow, `{"change":"move","container":{"id":200}}`, EventLayoutChanged, "", true},
		{"window urgent (ignored)", i3EventWindow, `{"change":"urgent","container":{"id":200}}`, "", "", false},
		{"workspace focus", i3EventWorkspace, `{"change":"focus"}`, EventLayoutChanged, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev, ok := translateI3Event(tt.evType, []byte(tt.payload))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if ev.Kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", ev.Kind, tt.wantKind)
			}
			if ev.Address != tt.wantAddr {
				t.Errorf("addr = %q, want %q", ev.Address, tt.wantAddr)
			}
		})
	}
}
