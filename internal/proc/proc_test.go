package proc

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/tjmisko/switchboard/internal/testsupport"
)

// §1.1 parsePPID — seed cases (0.3 expands the table). Uses the harness's
// realistic /proc/<pid>/status fixture alongside hand-built edge cases.
func TestParsePPID(t *testing.T) {
	tests := []struct {
		name   string
		status string
		want   int
	}{
		{"realistic status fixture", testsupport.ProcStatus(1234), 1234},
		{"whitespace around value", "Name:\tx\nPPid:\t  99  \nTracerPid:\t0\n", 99},
		{"missing ppid line", "Name:\tx\nState:\tS\n", 0},
		{"non-numeric value", "PPid:\tabc\n", 0},
		{"empty input", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parsePPID(tt.status); got != tt.want {
				t.Errorf("parsePPID = %d, want %d", got, tt.want)
			}
		})
	}
}

// parseState extracts the run-state char from the status "State:" line and
// tolerates missing/malformed lines.
func TestParseState(t *testing.T) {
	tests := []struct {
		name   string
		status string
		want   string
	}{
		{"realistic sleeping fixture", testsupport.ProcStatus(1234), "S"},
		{"stopped/suspended", "Name:\tx\nState:\tT (stopped)\nPPid:\t1\n", "T"},
		{"running, no parenthetical", "State:\tR\n", "R"},
		{"tracing stop", "State:\tt (tracing stop)\n", "t"},
		{"missing state line", "Name:\tx\nPPid:\t1\n", ""},
		{"empty value", "State:\t\n", ""},
		{"empty input", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseState(tt.status); got != tt.want {
				t.Errorf("parseState = %q, want %q", got, tt.want)
			}
		})
	}
}

// Suspended treats only job-control stop ("T") as suspended; "t" (tracing) and
// the live states are not.
func TestSuspended(t *testing.T) {
	for state, want := range map[string]bool{
		"T": true,
		"t": false,
		"S": false,
		"R": false,
		"":  false,
	} {
		if got := Suspended(state); got != want {
			t.Errorf("Suspended(%q) = %v, want %v", state, got, want)
		}
	}
}

// §1.4 AllPIDs / §1.3 Read — observable contract against real /proc: our own
// pid is enumerable and readable; a long-dead pid reads as ErrGone.
func TestReadAndAllPIDs(t *testing.T) {
	pids, err := AllPIDs()
	if err != nil {
		t.Fatalf("AllPIDs: %v", err)
	}
	self := os.Getpid()
	found := false
	for _, p := range pids {
		if p == self {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("AllPIDs did not include our own pid %d", self)
	}

	info, err := Read(self)
	if err != nil {
		t.Fatalf("Read(self): %v", err)
	}
	if info.Comm == "" {
		t.Errorf("Read(self).Comm is empty")
	}
	if info.State != "R" && info.State != "S" {
		t.Errorf("Read(self).State = %q, want a live state (R or S)", info.State)
	}
	// argv is populated from /proc/<pid>/cmdline; our own process always has at
	// least argv[0]. Assert the observable (non-empty), not the exact binary path.
	if len(info.Args) == 0 {
		t.Errorf("Read(self).Args is empty, want at least argv[0]")
	} else if info.Args[0] == "" {
		t.Errorf("Read(self).Args[0] is empty, want the program path")
	}

	if _, err := Read(testsupport.DeadPID()); !errors.Is(err, ErrGone) {
		t.Errorf("Read(dead pid) err = %v, want ErrGone", err)
	}

	// State() agrees with Read for a live process and reports ErrGone for a
	// dead one.
	st, err := State(self)
	if err != nil {
		t.Fatalf("State(self): %v", err)
	}
	if st != info.State {
		t.Errorf("State(self) = %q, Read(self).State = %q; want agreement", st, info.State)
	}
	if _, err := State(testsupport.DeadPID()); !errors.Is(err, ErrGone) {
		t.Errorf("State(dead pid) err = %v, want ErrGone", err)
	}
}

// parseVmHWMKB pulls the peak-RSS figure out of the status "VmHWM:" line. The
// realistic fixture deliberately has no such line — a status file without it is
// the non-Linux/masked case every caller must survive, and 0 is the answer.
func TestParseVmHWMKB(t *testing.T) {
	tests := []struct {
		name   string
		status string
		want   int64
	}{
		{"kb suffix", "Name:\tx\nVmHWM:\t   51200 kB\nVmRSS:\t 33000 kB\n", 51200},
		{"no suffix", "VmHWM:\t7\n", 7},
		{"realistic status fixture has no VmHWM", testsupport.ProcStatus(1234), 0},
		{"empty value", "VmHWM:\t\n", 0},
		{"non-numeric value", "VmHWM:\tlots kB\n", 0},
		{"empty input", "", 0},
		// VmHWM is a prefix of nothing else, but VmHWMx would be a different
		// field; CutPrefix matches on the colon so it cannot be confused.
		{"different field with the same stem", "VmHWMX:\t99 kB\n", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseVmHWMKB(tt.status); got != tt.want {
				t.Errorf("parseVmHWMKB = %d, want %d", got, tt.want)
			}
		})
	}
}

// Observable contract against real /proc: a running Go test binary has always
// touched some pages, so its own high-water mark is positive. This is the
// assertion that would catch the field being renamed or the reader reading the
// wrong pid — the parser table above cannot.
func TestVmHWMKBReadsThisProcess(t *testing.T) {
	if _, err := os.Stat("/proc/self/status"); err != nil {
		t.Skip("no /proc/self/status on this kernel")
	}
	if kb := VmHWMKB(); kb <= 0 {
		t.Errorf("VmHWMKB() = %d, want a positive peak RSS for a live process", kb)
	}
}

// writeProcFixture builds /proc/<pid> under root with comm, status and the
// given fd symlinks (fd number → link target). Targets need not exist:
// readlink only reads the link.
func writeProcFixture(t *testing.T, root string, pid int, fds map[int]string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	testsupport.WriteFile(t, filepath.Join(dir, "comm"), "pi\n")
	testsupport.WriteFile(t, filepath.Join(dir, "status"), testsupport.ProcStatus(1))
	if err := os.MkdirAll(filepath.Join(dir, "fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	for fd, target := range fds {
		if err := os.Symlink(target, filepath.Join(dir, "fd", strconv.Itoa(fd))); err != nil {
			t.Fatal(err)
		}
	}
}

// The Pi spike (phase-1-pi-provider.md, "Spike results") found that a TUI Pi
// has its pty on fd 0, while a json-mode child Pi runs with /dev/null on fd 0.
// TTY takes the first pts among fd 0..2, so only StdinTTY separates them when
// the child's stderr still reaches the terminal.
func TestReadShouldReportStdinTTYOnlyWhenFDZeroIsAPTS(t *testing.T) {
	root := t.TempDir()
	writeProcFixture(t, root, 100, map[int]string{0: "/dev/pts/3", 1: "/dev/pts/3", 2: "/dev/pts/3"})
	writeProcFixture(t, root, 200, map[int]string{0: "/dev/null", 1: "pipe:[123]", 2: "/dev/pts/3"})
	writeProcFixture(t, root, 300, map[int]string{0: "/dev/null", 1: "pipe:[123]", 2: "pipe:[124]"})
	r := NewReader(root)

	for _, tc := range []struct {
		pid       int
		wantTTY   string
		wantStdin bool
	}{
		{100, "/dev/pts/3", true},
		{200, "/dev/pts/3", false},
		{300, "", false},
	} {
		info, err := r.Read(tc.pid)
		if err != nil {
			t.Fatalf("Read(%d): %v", tc.pid, err)
		}
		if info.TTY != tc.wantTTY || info.StdinTTY != tc.wantStdin {
			t.Errorf("Read(%d) TTY=%q StdinTTY=%v, want %q, %v", tc.pid, info.TTY, info.StdinTTY, tc.wantTTY, tc.wantStdin)
		}
	}
}

// statLine builds a /proc/<pid>/stat line whose field 22 (starttime) is start.
// Fields 3..21 and 23.. carry distinct numbers so an off-by-one reads a
// different value rather than the right one by accident.
func statLine(pid int, comm, start string) string {
	return strconv.Itoa(pid) + " (" + comm + ") S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 20 0 1 0 " + start + " 123456 789 18446744073709551615\n"
}

// #97: the birth token is field 22 of /proc/<pid>/stat, counted from the LAST
// ')'. A process may name itself anything, and a comm holding ") " would shift
// every field for a parser that splits on whitespace or stops at the first ')'.
func TestParseStartTimeShouldReadFieldTwentyTwoWhenCommHoldsParenthesesAndSpaces(t *testing.T) {
	tests := []struct {
		name   string
		stat   string
		want   string
		wantOK bool
	}{
		{"plain comm", statLine(42, "claude", "98765"), "98765", true},
		{"comm with a space", statLine(42, "Web Content", "98765"), "98765", true},
		{"comm that closes a paren early", statLine(42, "a) S 9 9 (b", "98765"), "98765", true},
		{"comm that is only parens", statLine(42, ")()", "98765"), "98765", true},
		{"line cut before starttime", "42 (claude) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 20 0 1 0\n", "", false},
		{"no closing paren", "42 claude S 1 2 3\n", "", false},
		{"non-numeric starttime", statLine(42, "claude", "soon"), "", false},
		{"empty", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseStartTime(tt.stat)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("parseStartTime = %q, %v; want %q, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func writeBootID(t *testing.T, root, id string) {
	t.Helper()
	testsupport.WriteFile(t, filepath.Join(root, "sys", "kernel", "random", "boot_id"), id+"\n")
}

// The token joins the boot id with starttime, so the same starttime after a
// reboot is a different lifetime, and a reused pid (new starttime) is too.
func TestReadShouldReportABirthTokenThatChangesWithStartTimeAndBoot(t *testing.T) {
	root := t.TempDir()
	writeBootID(t, root, "boot-a")
	writeProcFixture(t, root, 100, nil)
	testsupport.WriteFile(t, filepath.Join(root, "100", "stat"), statLine(100, "pi", "5000"))
	info, err := NewReader(root).Read(100)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if info.Birth == "" {
		t.Fatal("Birth is empty, want a token when stat and boot_id are readable")
	}

	t.Run("should report the same token when the same lifetime is read again", func(t *testing.T) {
		again, err := NewReader(root).Read(100)
		if err != nil || again.Birth != info.Birth {
			t.Fatalf("second Read Birth = %q, %v; want %q", again.Birth, err, info.Birth)
		}
	})
	t.Run("should report another token when the pid is reused", func(t *testing.T) {
		testsupport.WriteFile(t, filepath.Join(root, "100", "stat"), statLine(100, "pi", "7000"))
		reused, err := NewReader(root).Read(100)
		if err != nil || reused.Birth == "" || reused.Birth == info.Birth {
			t.Fatalf("reused Birth = %q, %v; want a token other than %q", reused.Birth, err, info.Birth)
		}
	})
	t.Run("should report another token when the host rebooted", func(t *testing.T) {
		testsupport.WriteFile(t, filepath.Join(root, "100", "stat"), statLine(100, "pi", "5000"))
		writeBootID(t, root, "boot-b")
		rebooted, err := NewReader(root).Read(100)
		if err != nil || rebooted.Birth == "" || rebooted.Birth == info.Birth {
			t.Fatalf("rebooted Birth = %q, %v; want a token other than %q", rebooted.Birth, err, info.Birth)
		}
	})
}

// An unavailable token is not an error: the process is still read, and its
// lifetime is left unverified (empty), which the layers above never match.
func TestReadShouldLeaveBirthEmptyWhenTheTokenIsUnavailable(t *testing.T) {
	t.Run("should leave birth empty when stat is missing", func(t *testing.T) {
		root := t.TempDir()
		writeBootID(t, root, "boot-a")
		writeProcFixture(t, root, 100, nil)
		info, err := NewReader(root).Read(100)
		if err != nil || info.Birth != "" {
			t.Fatalf("Read = Birth %q, err %v; want an empty birth and no error", info.Birth, err)
		}
	})
	t.Run("should leave birth empty when boot_id is unreadable", func(t *testing.T) {
		root := t.TempDir()
		writeProcFixture(t, root, 100, nil)
		testsupport.WriteFile(t, filepath.Join(root, "100", "stat"), statLine(100, "pi", "5000"))
		info, err := NewReader(root).Read(100)
		if err != nil || info.Birth != "" {
			t.Fatalf("Read = Birth %q, err %v; want an empty birth and no error", info.Birth, err)
		}
	})
	t.Run("should report gone when the birth of a vanished pid is read", func(t *testing.T) {
		if _, err := NewReader(t.TempDir()).Birth(100); !errors.Is(err, ErrGone) {
			t.Fatalf("Birth(vanished) err = %v, want ErrGone", err)
		}
	})
}

// Observable contract against the real /proc: this process has a token, and
// it is stable across reads.
func TestBirthShouldBeStableForThisProcess(t *testing.T) {
	if _, err := os.Stat("/proc/sys/kernel/random/boot_id"); err != nil {
		t.Skip("no boot_id on this host")
	}
	a, err := Birth(os.Getpid())
	if err != nil || a == "" {
		t.Fatalf("Birth(self) = %q, %v; want a token", a, err)
	}
	info, err := Read(os.Getpid())
	if err != nil || info.Birth != a {
		t.Fatalf("Read(self).Birth = %q, %v; want %q", info.Birth, err, a)
	}
}
