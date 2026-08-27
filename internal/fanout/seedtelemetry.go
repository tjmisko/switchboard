package fanout

import (
	"log"
	"runtime"
	"syscall"
	"time"

	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/proc"
)

// Seeding telemetry: one fanout-seed line per seed-index build, in the journal
// the 2026-08-26 OOM forensics were reconstructed from
// (docs/seed-replay-memory-plan.md; field-by-field in docs/telemetry.md beside
// the daemon's other permanent line, publish-stats). It is deliberately
// permanent, not a bench artifact — a future store-shape regression should
// surface as a fat fanout-seed line long before it surfaces as an OOM kill.
//
// The heap figures are process-wide (ReadMemStats and VmHWM cannot be scoped to
// one goroutine), so on a busy daemon they carry everything else in flight too.
// That is the right reading for what this line answers — "what did seeding cost
// the PROCESS" — and the wall/CPU pair beside them is per-build.

// ensureSeedIndexLocked builds the shared seed index on first need (caller
// holds o.mu): from the seed cursor plus a tail replay when the cursor
// validates, from a full scan otherwise — SeedLoad decides, and rebuilds
// silently by design. A failed scan keeps whatever was folded before the
// failure: a partial index re-emits at most what a first-ever run would, and
// strictly less than an empty one.
//
// The refreshed cursor is written back BEFORE any entry is handed to a
// session (handover mutates the sets), so the persisted reduction is always
// the scan-time truth. A failed write only costs the next start one full
// scan, so it is logged on the telemetry line and otherwise ignored.
func (o *Observer) ensureSeedIndexLocked() {
	if o.seedIndex != nil {
		return
	}
	start, cpu0 := time.Now(), processCPU()
	res, err := history.SeedLoad(o.dir)
	if err == nil {
		err = history.WriteSeedCursor(o.dir, res)
	}
	wall, cpu := time.Since(start), processCPU()-cpu0
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	errText := "-"
	if err != nil {
		errText = err.Error()
	}
	log.Printf("fanout-seed: source=%s sessions=%d files=%d lines=%d matched=%d mb=%d wall=%s cpu=%s heap_alloc_mb=%d heap_sys_mb=%d vm_hwm_mb=%d err=%s",
		res.Source, len(res.Index), res.Stats.Files, res.Stats.Lines, res.Stats.Matched, res.Stats.Bytes>>20,
		wall.Round(time.Millisecond), cpu.Round(time.Millisecond),
		ms.HeapAlloc>>20, ms.HeapSys>>20, proc.VmHWMKB()>>10, errText)
	o.seedIndex = res.Index
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
