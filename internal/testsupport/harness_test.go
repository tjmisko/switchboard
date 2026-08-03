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
// extraction in §0.4). They keep the harness honest until then.
//
// FakeProcTree is no longer in that category: proc.NewReader roots the real
// readers at it, so internal/proc's own tests are its consumer. These cases
// cover the on-disk shape the fixture promises, not the parsing.

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

func TestProcStatusState_CarriesTheRequestedRunState(t *testing.T) {
	if !strings.Contains(ProcStatusState(7, "T"), "State:\tT (stopped)") {
		t.Errorf("ProcStatusState(7, \"T\") missing the stopped state: %q", ProcStatusState(7, "T"))
	}
	if !strings.Contains(ProcStatus(7), "State:\tS (sleeping)") {
		t.Errorf("ProcStatus default is not sleeping: %q", ProcStatus(7))
	}
}

func TestFakeProcTree_WritesStatAndRollupWhenSpecified(t *testing.T) {
	tree := NewFakeProcTree(t)
	tree.AddProcess(t, 100, ProcSpec{
		Comm:   "claude",
		PPid:   42,
		State:  "T",
		Rollup: &RollupKB{Pss: 440078, SwapPss: 12},
	})

	stat, err := os.ReadFile(tree.PIDDir(100) + "/stat")
	if err != nil {
		t.Fatalf("read stat: %v", err)
	}
	if !strings.HasPrefix(string(stat), "100 (claude) T 42 ") {
		t.Errorf("stat = %q, want it to open with pid, comm, state, ppid", stat)
	}

	rollup, err := os.ReadFile(tree.PIDDir(100) + "/smaps_rollup")
	if err != nil {
		t.Fatalf("read smaps_rollup: %v", err)
	}
	// The trap the fixture exists to set: neighbours that a prefix match would
	// pick up carry different numbers.
	for _, want := range []string{"Pss:             440078 kB", "SwapPss:             12 kB"} {
		if !strings.Contains(string(rollup), want) {
			t.Errorf("smaps_rollup missing %q:\n%s", want, rollup)
		}
	}
	if strings.Contains(string(rollup), "Pss_Anon:        440078 kB") {
		t.Errorf("Pss_Anon must differ from Pss so a prefix match is caught:\n%s", rollup)
	}
}

func TestFakeProcTree_OmitsRollupWhenUnspecified(t *testing.T) {
	// nil Rollup is how a kernel thread, another user's process, and one that
	// died mid-walk all present to the reader.
	tree := NewFakeProcTree(t)
	tree.AddProcess(t, 100, ProcSpec{Comm: "kthreadd", PPid: 2})

	if _, err := os.Stat(tree.PIDDir(100) + "/smaps_rollup"); !os.IsNotExist(err) {
		t.Errorf("smaps_rollup exists for a spec with no Rollup, err = %v", err)
	}
}

func TestPressureMemory_GivesTheFullLineDifferentNumbers(t *testing.T) {
	body := PressureMemory(4.25, 556011549)
	if !strings.Contains(body, "some avg10=4.25") || !strings.Contains(body, "total=556011549") {
		t.Errorf("PressureMemory missing the requested `some` values: %q", body)
	}
	if strings.Contains(body, "full avg10=4.25") {
		t.Errorf("`full` must differ from `some` so a parser reading the wrong line is caught: %q", body)
	}
}
