package main

import (
	"encoding/json"
	"flag"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tjmisko/switchboard/internal/history"
)

// cmdSeedBench measures the fanout Observer's history-seeding cost against a
// store directory. It exists so the numbers in docs/seed-replay-memory-plan.md
// are produced by THE SAME code path the daemon runs — it calls the history
// package's seeding entry point directly, never a reimplementation — and so
// the before/after of any seeding change is captured by identical measurement
// code. Through phase 2 that entry point was the per-session
// PriorSubagentState/PriorWorkflowState pair (one materializing double-scan
// per selected session); it is now the shared one-pass history.SeedScan, so
// the scan runs ONCE regardless of how many sessions are selected — which is
// precisely the change the phase-3 numbers exist to demonstrate.
//
// One process = one measurement: VmHWM (the kernel's resident high-water mark)
// is process-lifetime, so scripts/sb-bench-seed runs this in a fresh subprocess
// per run. Output is a single JSON object on stdout.
//
// Modes select which sessions' set sizes are REPORTED (the conformance
// numbers held stable across phases); since the shared pass seeds everyone at
// once, they no longer change what work runs:
//
//	-sessions a,b,c   report exactly these session ids
//	-storm [-top N]   report the N busiest sessions (the restart case)
//	(neither)         report the busiest session (the single-discovery case)
func cmdSeedBench(args []string) {
	fs := flag.NewFlagSet("seed-bench", flag.ExitOnError)
	dir := fs.String("dir", history.DefaultDir(), "activity-log directory")
	sessionsFlag := fs.String("sessions", "", "comma-separated session ids to report")
	storm := fs.Bool("storm", false, "report every session found in the log (restart storm)")
	top := fs.Int("top", 0, "with -storm: cap to the N busiest sessions (0 = all); models the live set a real restart re-seeds")
	_ = fs.Parse(args)

	var ms0 runtime.MemStats
	runtime.ReadMemStats(&ms0)
	cpu0 := seedBenchCPU()
	start := time.Now()

	index, stats, err := history.SeedScan(*dir)
	if err != nil {
		fail("seed-bench: scan %s: %v", *dir, err)
	}

	wall := time.Since(start)
	cpu := seedBenchCPU() - cpu0
	var ms1 runtime.MemStats
	runtime.ReadMemStats(&ms1)

	ids := seedBenchOrder(index)
	var targets []string
	mode := "single"
	switch {
	case *sessionsFlag != "":
		mode = "explicit"
		for _, id := range strings.Split(*sessionsFlag, ",") {
			if id = strings.TrimSpace(id); id != "" {
				targets = append(targets, id)
			}
		}
	case *storm:
		mode = "storm"
		targets = ids
		if *top > 0 && len(targets) > *top {
			targets = targets[:*top]
		}
	default:
		if len(ids) == 0 {
			fail("seed-bench: no sessions with subagent/workflow events in %s", *dir)
		}
		targets = ids[:1]
	}

	type pass struct {
		Session   string `json:"session"`
		Spawned   int    `json:"spawned"`
		Stopped   int    `json:"stopped"`
		WfStarted int    `json:"wf_started"`
		WfEnded   int    `json:"wf_ended"`
	}
	passes := make([]pass, 0, len(targets))
	for _, id := range targets {
		sets := index.Sets(id)
		passes = append(passes, pass{
			Session: id,
			Spawned: len(sets.Spawned), Stopped: len(sets.Stopped),
			WfStarted: len(sets.WorkflowStarted), WfEnded: len(sets.WorkflowStopped),
		})
	}

	out := map[string]any{
		"dir":               *dir,
		"mode":              mode,
		"sessions":          len(targets),
		"known_sessions":    len(index),
		"scan_files":        stats.Files,
		"scan_lines":        stats.Lines,
		"scan_matched":      stats.Matched,
		"scan_bytes":        stats.Bytes,
		"passes":            passes,
		"total_wall_ms":     wall.Milliseconds(),
		"cpu_ms":            cpu.Milliseconds(),
		"total_alloc_bytes": ms1.TotalAlloc - ms0.TotalAlloc,
		"num_gc":            ms1.NumGC - ms0.NumGC,
		"heap_sys_bytes":    ms1.HeapSys,
		"vm_hwm_kb":         seedBenchVmHWMKB(),
	}
	enc := json.NewEncoder(os.Stdout)
	if err := enc.Encode(out); err != nil {
		fail("seed-bench: encode: %v", err)
	}
}

// seedBenchOrder returns the index's session ids busiest-first (by total set
// size, then id), so "single" deterministically picks the heaviest session.
func seedBenchOrder(index history.SeedIndex) []string {
	weight := func(id string) int {
		s := index.Sets(id)
		return len(s.Spawned) + len(s.Stopped) + len(s.WorkflowStarted) + len(s.WorkflowStopped)
	}
	ids := make([]string, 0, len(index))
	for id := range index {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if weight(ids[i]) != weight(ids[j]) {
			return weight(ids[i]) > weight(ids[j])
		}
		return ids[i] < ids[j]
	})
	return ids
}

// seedBenchCPU is this process's cumulative CPU (user+system). Process-wide,
// so concurrent goroutine work is attributed too — fine for a benchmark whose
// process does nothing else.
func seedBenchCPU() time.Duration {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// seedBenchVmHWMKB reads the kernel's peak-RSS figure for this process, in KB.
// 0 when unreadable (non-Linux).
func seedBenchVmHWMKB() int64 {
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
