package hyprland

import (
	"path/filepath"
	"testing"
)

// §6.2 socketPath — the Available()==false precondition: without the Hyprland
// instance signature or XDG_RUNTIME_DIR we cannot locate the IPC sockets and
// must error cleanly rather than dialing a bogus path. The Subscribe parse
// loop gets its harness (ScriptedConn) consumer in §0.4 once it is extracted
// to take an io.Reader.
func TestSocketPathRequiresEnv(t *testing.T) {
	t.Setenv("HYPRLAND_INSTANCE_SIGNATURE", "")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if _, err := requestSocketPath(); err == nil {
		t.Error("requestSocketPath should error when HYPRLAND_INSTANCE_SIGNATURE is unset")
	}

	t.Setenv("HYPRLAND_INSTANCE_SIGNATURE", "deadbeef")
	t.Setenv("XDG_RUNTIME_DIR", "")
	if _, err := socket2Path(); err == nil {
		t.Error("socket2Path should error when XDG_RUNTIME_DIR is unset")
	}

	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	got, err := requestSocketPath()
	if err != nil {
		t.Fatalf("requestSocketPath with env set: %v", err)
	}
	want := filepath.Join("/run/user/1000", "hypr", "deadbeef", ".socket.sock")
	if got != want {
		t.Errorf("requestSocketPath = %q, want %q", got, want)
	}
}
