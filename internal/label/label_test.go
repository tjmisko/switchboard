package label

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/tjmisko/switchboard/internal/state"
)

// writeSessionFile drops a ~/.claude/sessions/<pid>.json with the given name
// under a temp HOME, and points HOME at it for the duration of the test.
func writeSessionFile(t *testing.T, pid int, name string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"pid":%d,"name":%q}`, pid, name)
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", pid)), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRawName_prefersClaudeSessionName(t *testing.T) {
	writeSessionFile(t, 4242, "from-claude")
	s := state.Session{
		PID:     4242,
		CWD:     "/home/u/Projects/Arachne",
		Wezterm: &state.WeztermInfo{WindowTitle: "✳ from-window"},
	}
	if got := RawName(s); got != "from-claude" {
		t.Errorf("RawName = %q, want from-claude", got)
	}
}

func TestRawName_fallsBackToWindowTitleStrippingSpinner(t *testing.T) {
	// HOME points at an empty temp dir, so there is no sessions file.
	t.Setenv("HOME", t.TempDir())
	s := state.Session{
		PID:     4243,
		CWD:     "/home/u/Projects/Arachne",
		Wezterm: &state.WeztermInfo{WindowTitle: "✳ assess-npm"},
	}
	if got := RawName(s); got != "assess-npm" {
		t.Errorf("RawName = %q, want assess-npm", got)
	}
}

func TestRawName_fallsBackToCwdBasename(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := state.Session{PID: 4244, CWD: "/home/u/Projects/Arachne"}
	if got := RawName(s); got != "Arachne" {
		t.Errorf("RawName = %q, want Arachne", got)
	}
}
