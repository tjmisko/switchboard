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

	if _, err := Read(testsupport.DeadPID()); !errors.Is(err, ErrGone) {
		t.Errorf("Read(dead pid) err = %v, want ErrGone", err)
	}
}
