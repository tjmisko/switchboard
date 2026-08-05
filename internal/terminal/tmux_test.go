package terminal

import (
	"context"
	"testing"
)

func TestParseTmuxPanes(t *testing.T) {
	// Two panes; the format is pane_tty\tpane_id\tpane_current_path\twindow_name.
	out := []byte("/dev/pts/5\t%3\t/home/u/proj\tclaude\n" +
		"/dev/pts/9\t%7\t/home/u/other\tzsh\n" +
		"malformed-line\n")
	got := parseTmuxPanes(out)
	if len(got) != 2 {
		t.Fatalf("got %d panes, want 2 (malformed line skipped): %+v", len(got), got)
	}
	if got[0] != (tmuxPane{TTY: "/dev/pts/5", PaneID: "%3", CWD: "/home/u/proj", WindowName: "claude"}) {
		t.Errorf("pane[0] = %+v", got[0])
	}
	if got[1].PaneID != "%7" || got[1].WindowName != "zsh" {
		t.Errorf("pane[1] = %+v", got[1])
	}
}

// tmuxPaneRef is the single conversion Locate and Snapshot share. The handle is
// the only focus token this backend has, so losing it here turns Navigate into
// the "pane ref has no handle" error rather than a wrong focus.
func TestTmuxPaneRefShouldCarryTheHandleAndTTY(t *testing.T) {
	got := tmuxPaneRef(tmuxPane{TTY: "/dev/pts/5", PaneID: "%3", CWD: "/home/u/proj", WindowName: "claude"})
	want := PaneRef{Backend: "tmux", Handle: "%3", WindowTitle: "claude", TTY: "/dev/pts/5", CWD: "/home/u/proj"}
	if got != want {
		t.Errorf("tmuxPaneRef = %+v, want %+v", got, want)
	}
}

// Activate refuses a ref with no pane handle rather than running a bogus tmux
// command.
func TestTmuxActivateRequiresHandle(t *testing.T) {
	if err := NewTmux().Activate(context.Background(), &PaneRef{Backend: "tmux"}); err == nil {
		t.Error("Activate with empty handle = nil err, want error")
	}
}
