package testsupport

import (
	"bufio"
	"io"
	"os"
	"strings"
	"testing"
)

// These self-tests exercise the fixtures whose domain consumers arrive in
// later Phase-0 tasks (the stream-parser fakes feed the hyprland parse-loop
// extraction in §0.4; FakeProcTree feeds the injectable procSource in §0.5).
// They keep the harness honest until then.

func TestScriptedConn_ServesScriptThenBlocksUntilClose(t *testing.T) {
	c := ScriptedLines("activewindowv2>>2a3b", "closewindow>>2a3b")
	br := bufio.NewReader(c)

	l1, err := br.ReadString('\n')
	if err != nil || l1 != "activewindowv2>>2a3b\n" {
		t.Fatalf("line 1 = %q, err = %v", l1, err)
	}
	l2, err := br.ReadString('\n')
	if err != nil || l2 != "closewindow>>2a3b\n" {
		t.Fatalf("line 2 = %q, err = %v", l2, err)
	}

	// Script drained: the next read must block until Close, then EOF.
	c.Close()
	if _, err := br.ReadString('\n'); err != io.EOF {
		t.Fatalf("after close, err = %v, want EOF", err)
	}
}

func TestScriptedConn_CapturesWrites(t *testing.T) {
	c := NewScriptedConn("")
	if _, err := c.Write([]byte(`{"cmd":"list"}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := c.Written(); got != `{"cmd":"list"}`+"\n" {
		t.Errorf("Written() = %q", got)
	}
}

func TestLineReader_YieldsNewlineTerminatedLines(t *testing.T) {
	got, err := io.ReadAll(LineReader("a>>1", "b>>2"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "a>>1\nb>>2\n" {
		t.Errorf("LineReader = %q", got)
	}
}

func TestFakeProcTree_WritesStatusAndSymlinks(t *testing.T) {
	tree := NewFakeProcTree(t)
	tree.AddProcess(t, 100, ProcSpec{Comm: "claude", PPid: 42, TTY: "/dev/pts/5"})

	status, err := os.ReadFile(tree.PIDDir(100) + "/status")
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if !strings.Contains(string(status), "PPid:\t42") {
		t.Errorf("status missing PPid:\\t42:\n%s", status)
	}

	link, err := os.Readlink(tree.PIDDir(100) + "/fd/0")
	if err != nil || link != "/dev/pts/5" {
		t.Errorf("fd/0 link = %q, err = %v", link, err)
	}
}

func TestProcStatus_EmbedsPPid(t *testing.T) {
	if !strings.Contains(ProcStatus(7), "PPid:\t7") {
		t.Errorf("ProcStatus(7) missing PPid:\\t7: %q", ProcStatus(7))
	}
}
