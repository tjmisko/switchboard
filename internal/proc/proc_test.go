package proc

import (
	"errors"
	"os"
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
