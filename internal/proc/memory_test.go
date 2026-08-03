package proc

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tjmisko/switchboard/internal/testsupport"
)

// A verbatim /proc/self/smaps_rollup from the development machine. Copied
// rather than generated so the parser is pinned against the kernel's real
// output — five Pss* lines and a Swap line that is not SwapPss.
const liveRollup = `00200000-ffffde214000 ---p 00000000 00:00 0                              [rollup]
Rss:              448896 kB
Pss:              423629 kB
Pss_Dirty:        372448 kB
Pss_Anon:         408032 kB
Pss_File:          15597 kB
Pss_Shmem:             0 kB
Shared_Clean:      34544 kB
Shared_Dirty:          0 kB
Private_Clean:     41904 kB
Private_Dirty:    372448 kB
Referenced:       433056 kB
Anonymous:        408032 kB
KSM:                   0 kB
LazyFree:              0 kB
AnonHugePages:         0 kB
ShmemPmdMapped:        0 kB
FilePmdMapped:         0 kB
Shared_Hugetlb:        0 kB
Private_Hugetlb:       0 kB
Swap:              53968 kB
SwapPss:           53968 kB
Locked:                0 kB
`

func TestParseSmapsRollup(t *testing.T) {
	tests := []struct {
		name        string
		rollup      string
		wantPss     int64
		wantSwapPss int64
	}{
		{
			name:        "should return pss and swap pss in bytes when the rollup is a live kernel's output",
			rollup:      liveRollup,
			wantPss:     423629 * 1024,
			wantSwapPss: 53968 * 1024,
		},
		{
			// Swap is deliberately nonzero: this is the case where confusing it
			// for SwapPss does the most damage, because there is no later
			// SwapPss line to overwrite the wrong value.
			name:        "should report swap pss as zero when an older kernel omits the line",
			rollup:      "Rss:\t515312 kB\nPss:\t440078 kB\nSwap:\t53968 kB\n",
			wantPss:     440078 * 1024,
			wantSwapPss: 0,
		},
		{
			// Pss_Anon is within 4% of Pss on the live fixture, so a prefix
			// match reads a number that looks entirely plausible. The kernel
			// prints Pss BEFORE its four companions, which means the live
			// ordering cannot catch a first-match-wins prefix parser — such a
			// parser returns the correct Pss for liveRollup. Only the reversed
			// ordering discriminates, so that is what this row feeds; the live
			// ordering is already covered by the first row.
			name:        "should not mistake pss dirty anon file or shmem for pss when all five are present",
			rollup:      "Pss_Dirty:\t372448 kB\nPss_Anon:\t408032 kB\nPss_File:\t15597 kB\nPss_Shmem:\t0 kB\nPss:\t423629 kB\nSwapPss:\t53968 kB\n",
			wantPss:     423629 * 1024,
			wantSwapPss: 53968 * 1024,
		},
		{
			name:        "should not mistake swap for swap pss when the two differ",
			rollup:      "Pss:\t100 kB\nSwap:\t99999 kB\nSwapPss:\t7 kB\n",
			wantPss:     100 * 1024,
			wantSwapPss: 7 * 1024,
		},
		{
			name:        "should ignore the address range header when it contains colons",
			rollup:      "00200000-ffffde214000 ---p 00000000 00:00 0     [rollup]\nPss:\t8 kB\n",
			wantPss:     8 * 1024,
			wantSwapPss: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSmapsRollup(tt.rollup)
			if err != nil {
				t.Fatalf("parseSmapsRollup: %v", err)
			}
			if got.Pss != tt.wantPss {
				t.Errorf("Pss = %d bytes, want %d", got.Pss, tt.wantPss)
			}
			if got.SwapPss != tt.wantSwapPss {
				t.Errorf("SwapPss = %d bytes, want %d", got.SwapPss, tt.wantSwapPss)
			}
		})
	}
}

func TestParseSmapsRollup_ShouldReportNoRollupWhenTheBodyIsEmpty(t *testing.T) {
	// Kernel threads present a zero-length smaps_rollup. Reporting 0 bytes
	// would put them in a sample as measured-and-empty.
	if _, err := parseSmapsRollup(""); !errors.Is(err, ErrNoRollup) {
		t.Errorf("parseSmapsRollup(\"\") err = %v, want ErrNoRollup", err)
	}
}

func TestParseSmapsRollup_ShouldReportNoRollupWhenOnlyPssDecoysArePresent(t *testing.T) {
	// The sharpest form of the prefix trap: with no Pss line at all, a prefix
	// matcher reports Pss_Dirty as the session's memory rather than declining
	// to answer, so the sample reads as measured instead of missing.
	rollup := "Rss:\t448896 kB\nPss_Dirty:\t372448 kB\nPss_Anon:\t408032 kB\n"
	if _, err := parseSmapsRollup(rollup); !errors.Is(err, ErrNoRollup) {
		t.Errorf("parseSmapsRollup(decoys only) err = %v, want ErrNoRollup", err)
	}
}

func TestMemory_ShouldReadOurOwnProcessWhenAskedForTheLivePid(t *testing.T) {
	mem, err := Memory(os.Getpid())
	if err != nil {
		t.Fatalf("Memory(self): %v", err)
	}
	if mem.Pss <= 0 {
		t.Errorf("Memory(self).Pss = %d, want a positive byte count", mem.Pss)
	}
	// PSS charges shared pages fractionally, so it can never exceed RSS.
	if mem.Pss > mem.Rss {
		t.Errorf("Memory(self).Pss = %d exceeds Rss = %d", mem.Pss, mem.Rss)
	}
}

func TestMemory_ShouldReportGoneWhenTheProcessHasVanished(t *testing.T) {
	if _, err := Memory(testsupport.DeadPID()); !errors.Is(err, ErrGone) {
		t.Errorf("Memory(dead pid) err = %v, want ErrGone", err)
	}
}

func TestMemory_ShouldReadTheFixtureWhenTheReaderIsRootedAtAFakeProc(t *testing.T) {
	tree := testsupport.NewFakeProcTree(t)
	tree.AddProcess(t, 100, testsupport.ProcSpec{
		Comm:   "claude",
		PPid:   1,
		Rollup: &testsupport.RollupKB{Pss: 440078, SwapPss: 0},
	})

	mem, err := NewReader(tree.Root).Memory(100)
	if err != nil {
		t.Fatalf("Memory(100): %v", err)
	}
	if want := int64(440078) * 1024; mem.Pss != want {
		t.Errorf("Pss = %d, want %d", mem.Pss, want)
	}
	// The fixture writes a nonzero Swap alongside a zero SwapPss precisely so
	// this assertion can tell them apart.
	if mem.SwapPss != 0 {
		t.Errorf("SwapPss = %d, want 0", mem.SwapPss)
	}
}

func TestMemory_ShouldReportNoRollupWhenTheProcessIsAKernelThread(t *testing.T) {
	tree := testsupport.NewFakeProcTree(t)
	tree.AddProcess(t, 100, testsupport.ProcSpec{Comm: "kthreadd", PPid: 2})
	testsupport.WriteFile(t, filepath.Join(tree.PIDDir(100), "smaps_rollup"), "")

	if _, err := NewReader(tree.Root).Memory(100); !errors.Is(err, ErrNoRollup) {
		t.Errorf("Memory(kernel thread) err = %v, want ErrNoRollup", err)
	}
}
