package fanout

import (
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tjmisko/switchboard/internal/history"
)

// Seeding telemetry: one fanout-seed line per seed-index build, in the journal
// the 2026-08-26 OOM forensics were reconstructed from
// (docs/seed-replay-memory-plan.md). It is deliberately permanent, not a bench
// artifact — a future store-shape regression should surface as a fat
// fanout-seed line long before it surfaces as an OOM kill.
//
// The heap figures are process-wide (ReadMemStats and VmHWM cannot be scoped to
// one goroutine), so on a busy daemon they carry everything else in flight too.
// That is the right reading for what this line answers — "what did seeding cost
// the PROCESS" — and the wall/CPU pair beside them is per-build.

// ensureSeedIndexLocked builds the shared seed index on first need (caller
// holds o.mu). A failed scan keeps whatever was folded before the failure: a
// partial index re-emits at most what a first-ever run would, and strictly
// less than an empty one.
func (o *Observer) ensureSeedIndexLocked() {
	if o.seedIndex != nil {
		return
	}
	start, cpu0 := time.Now(), processCPU()
	index, stats, err := history.SeedScan(o.dir)
	wall, cpu := time.Since(start), processCPU()-cpu0
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	errText := "-"
	if err != nil {
		errText = err.Error()
	}
	log.Printf("fanout-seed: source=scan sessions=%d files=%d lines=%d matched=%d mb=%d wall=%s cpu=%s heap_alloc_mb=%d heap_sys_mb=%d vm_hwm_mb=%d err=%s",
		len(index), stats.Files, stats.Lines, stats.Matched, stats.Bytes>>20,
		wall.Round(time.Millisecond), cpu.Round(time.Millisecond),
		ms.HeapAlloc>>20, ms.HeapSys>>20, vmHWMKB()>>10, errText)
	o.seedIndex = index
}

// processCPU is this process's cumulative CPU (user+system). Process-wide for
// the same reason the heap figures are; differences bracket a pass usefully on
// a daemon that is otherwise ticking in the background.
func processCPU() time.Duration {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// vmHWMKB is the kernel's peak-RSS figure for this process in KB, 0 when
// unreadable (non-Linux).
func vmHWMKB() int64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "VmHWM:"); ok {
			fields := strings.Fields(rest)
			if len(fields) >= 1 {
				if kb, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
					return kb
				}
			}
		}
	}
	return 0
}
