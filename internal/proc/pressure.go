package proc

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
)

// SysMem is the machine-wide memory reading, taken once per reconcile tick and
// stamped onto every session's sample in that tick. Byte counts throughout.
//
// This is the first /proc read in the package that is not per-pid.
type SysMem struct {
	AvailBytes     int64 // /proc/meminfo MemAvailable
	SwapFreeBytes  int64 // /proc/meminfo SwapFree
	SwapTotalBytes int64 // /proc/meminfo SwapTotal
	PSI            PSISome
}

// PSISome is the `some` line of /proc/pressure/memory — the share of time at
// least one task was stalled on memory.
//
// Present distinguishes "not measured" from "measured, and no stall". A kernel
// built without CONFIG_PSI has no pressure file at all, and some container
// runtimes hide it; reporting zero there would be a lie that reads as a
// perfectly healthy machine, which is precisely the reading OOM forensics must
// not be given. Callers omit the fields entirely when Present is false.
type PSISome struct {
	Present bool
	Avg10   float64 // decaying 10-second average, percent
	TotalUS int64   // monotonic microseconds stalled since boot
}

// ErrNoMemAvailable means /proc/meminfo was readable but carried no
// MemAvailable line (kernels before 3.14).
var ErrNoMemAvailable = errors.New("meminfo has no MemAvailable line")

// SystemMemory reads machine-wide memory availability and pressure.
//
// Only /proc/meminfo can fail the call. PSI absence is a degraded case, not an
// error: it must not fail the tick, so it comes back as PSI.Present == false
// with the availability figures intact.
func SystemMemory() (SysMem, error) { return hostProc.SystemMemory() }

func (r *Reader) SystemMemory() (SysMem, error) {
	meminfo, err := readSmallFile(filepath.Join(r.procRoot(), "meminfo"))
	if err != nil {
		return SysMem{}, err
	}
	out, err := parseMemInfo(meminfo)
	if err != nil {
		return SysMem{}, err
	}
	if pressure, err := readSmallFile(filepath.Join(r.procRoot(), "pressure", "memory")); err == nil {
		out.PSI = parsePSISome(pressure)
	}
	return out, nil
}

// parseMemInfo pulls MemAvailable (and the swap totals) out of a meminfo body,
// converting kB to bytes. Keys are matched exactly so MemFree and MemTotal
// cannot be mistaken for MemAvailable.
//
// MemAvailable rather than MemFree is the right figure: free memory on a
// healthy Linux box is near zero because the page cache uses the rest, so
// MemFree would report every machine as perpetually about to die.
func parseMemInfo(meminfo string) (SysMem, error) {
	var out SysMem
	sawAvail := false
	for line := range strings.SplitSeq(meminfo, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch key {
		case "MemAvailable":
			out.AvailBytes = parseKBToBytes(value)
			sawAvail = true
		case "SwapFree":
			out.SwapFreeBytes = parseKBToBytes(value)
		case "SwapTotal":
			out.SwapTotalBytes = parseKBToBytes(value)
		}
	}
	if !sawAvail {
		return SysMem{}, ErrNoMemAvailable
	}
	return out, nil
}

// parsePSISome reads the `some` line of a /proc/pressure/memory body:
//
//	some avg10=0.00 avg60=0.00 avg300=0.00 total=558307372
//	full avg10=0.00 avg60=0.00 avg300=0.00 total=443261645
//
// The `full` line is ignored deliberately. `some` means at least one task
// stalled; `full` means every runnable task did — a far rarer and much more
// severe condition whose numbers are always smaller. Reading `full` where
// `some` was meant would under-report stalls by roughly the ratio between them
// (20% on this machine at rest).
//
// Both avg10 and total are kept: avg10 is a decaying average that a 5-second
// sampler can alias straight past a spike, while total is monotonic, so the
// delta between adjacent samples is the exact stall time in that interval.
// Anything deriving a number uses the delta; avg10 is the human glance.
//
// Returns Present == false when no `some` line is found.
func parsePSISome(pressure string) PSISome {
	for line := range strings.SplitSeq(pressure, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "some" {
			continue
		}
		out := PSISome{Present: true}
		for _, field := range fields[1:] {
			key, value, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			switch key {
			case "avg10":
				out.Avg10, _ = strconv.ParseFloat(value, 64)
			case "total":
				out.TotalUS, _ = strconv.ParseInt(value, 10, 64)
			}
		}
		return out
	}
	return PSISome{}
}
