package proc

import (
	"errors"
	"strings"
)

// Mem is one process's resident cost, in BYTES (the kernel reports kB; the
// conversion happens at the parse boundary so nothing downstream deals in kB).
//
// PSS — proportional set size — charges each shared page to its sharers in
// fractions, so summing PSS across processes or sessions never double-counts
// the pages they share. SwapPss is the same measure for pages pushed out to
// swap, kept separate because a swapped page is still memory the session is
// responsible for but is not resident.
type Mem struct {
	Rss     int64 // resident set size, for reference; double-counts shared pages
	Pss     int64
	SwapPss int64
}

// ErrNoRollup means smaps_rollup was readable but carried no Pss line. Kernel
// threads present a zero-length rollup, so this is a routine skip rather than
// a failure.
var ErrNoRollup = errors.New("smaps_rollup has no Pss line")

// Memory reads /proc/<pid>/smaps_rollup and returns the process's PSS and
// SwapPss in bytes. Returns ErrGone if the process vanished mid-read, and
// ErrNoRollup for a kernel thread's empty rollup. Reading another user's
// process yields the underlying permission error unchanged — smaps_rollup
// requires PTRACE_MODE_READ, which we hold for our own descendants and not for
// anyone else's.
func Memory(pid int) (Mem, error) { return hostProc.Memory(pid) }

func (r *Reader) Memory(pid int) (Mem, error) {
	rollup, err := readSmallFile(r.pidPath(pid, "smaps_rollup"))
	if err != nil {
		return Mem{}, wrapGone(err)
	}
	return parseSmapsRollup(rollup)
}

// parseSmapsRollup pulls Rss/Pss/SwapPss out of a smaps_rollup body.
//
// Keys are matched EXACTLY, never by prefix: a real rollup carries five other
// Pss* lines (Pss_Dirty, Pss_Anon, Pss_File, Pss_Shmem) and a Swap line that
// is not SwapPss, so a prefix match would silently report the wrong figure —
// on this machine Pss_Anon is within 4% of Pss, which is exactly the kind of
// wrong that never looks wrong.
//
// A missing SwapPss is tolerated as zero: kernels before 4.14 omit it from the
// rollup, and a machine with no swap has nothing to report either way.
func parseSmapsRollup(rollup string) (Mem, error) {
	var out Mem
	sawPss := false
	for line := range strings.SplitSeq(rollup, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch key {
		case "Rss":
			out.Rss = parseKBToBytes(value)
		case "Pss":
			out.Pss = parseKBToBytes(value)
			sawPss = true
		case "SwapPss":
			out.SwapPss = parseKBToBytes(value)
		}
	}
	if !sawPss {
		return Mem{}, ErrNoRollup
	}
	return out, nil
}
