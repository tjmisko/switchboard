package main

import (
	"bufio"
	"bytes"
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
// package's seeding functions directly, never a reimplementation — and so the
// before/after of any seeding change is captured by identical measurement code.
//
// One process = one measurement: VmHWM (the kernel's resident high-water mark)
// is process-lifetime, so scripts/sb-bench-seed runs this in a fresh subprocess
// per scenario. Output is a single JSON object on stdout.
//
// Modes:
//
//	-sessions a,b,c   seed exactly these session ids, in order
//	-storm            seed every session found in the log (the restart case)
//	(neither)         seed the busiest session (the single-discovery case)
//
// Session discovery streams the store with a small decode rather than the
// seeding path itself, so its cost stays out of the per-pass numbers (it still
// bounds VmHWM from below, which is the honest floor — the discovery scan is
// the cheap shape the remediation moves seeding onto).
func cmdSeedBench(args []string) {
	fs := flag.NewFlagSet("seed-bench", flag.ExitOnError)
	dir := fs.String("dir", history.DefaultDir(), "activity-log directory")
	sessionsFlag := fs.String("sessions", "", "comma-separated session ids to seed")
	storm := fs.Bool("storm", false, "seed every session found in the log (restart storm)")
	_ = fs.Parse(args)

	ids, counts, err := seedBenchSessions(*dir)
	if err != nil {
		fail("seed-bench: scan %s: %v", *dir, err)
	}

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
	default:
		if len(ids) == 0 {
			fail("seed-bench: no sessions with subagent/workflow events in %s", *dir)
		}
		targets = ids[:1]
	}

	type pass struct {
		Session   string `json:"session"`
		WallMs    int64  `json:"wall_ms"`
		Spawned   int    `json:"spawned"`
		Stopped   int    `json:"stopped"`
		WfStarted int    `json:"wf_started"`
		WfEnded   int    `json:"wf_ended"`
	}

	var ms0 runtime.MemStats
	runtime.ReadMemStats(&ms0)
	cpu0 := seedBenchCPU()
	start := time.Now()

	passes := make([]pass, 0, len(targets))
	for _, id := range targets {
		p := pass{Session: id}
		passStart := time.Now()
		if sp, st, err := history.PriorSubagentState(*dir, id); err == nil {
			p.Spawned, p.Stopped = len(sp), len(st)
		}
		if ws, we, err := history.PriorWorkflowState(*dir, id); err == nil {
			p.WfStarted, p.WfEnded = len(ws), len(we)
		}
		p.WallMs = time.Since(passStart).Milliseconds()
		passes = append(passes, p)
	}

	wall := time.Since(start)
	cpu := seedBenchCPU() - cpu0
	var ms1 runtime.MemStats
	runtime.ReadMemStats(&ms1)

	out := map[string]any{
		"dir":               *dir,
		"mode":              mode,
		"sessions":          len(targets),
		"known_sessions":    len(counts),
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

// seedBenchLine is the minimal decode the discovery scan needs: enough to
// attribute a fanout/workflow event to its session, nothing more.
type seedBenchLine struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
}

// seedBenchTypeMarkers admit a line to the discovery decode. They match the
// event-type VALUE, not the compact `"type":"…"` key-value form, because day
// files repaired by the Python scripts carry `"type": "…"` with a space —
// matching the compact form would silently skip exactly the repaired span
// lines. False positives (the value appearing in some other field) are fine:
// the JSON decode's Type field is the authoritative check.
var seedBenchTypeMarkers = [][]byte{
	[]byte(`"` + history.EventSubagentSpawn + `"`),
	[]byte(`"` + history.EventSubagentStop + `"`),
	[]byte(`"` + history.EventWorkflowStart + `"`),
	[]byte(`"` + history.EventWorkflowStop + `"`),
}

func seedBenchInteresting(t string) bool {
	return t == history.EventSubagentSpawn || t == history.EventSubagentStop ||
		t == history.EventWorkflowStart || t == history.EventWorkflowStop
}

// seedBenchSessions streams the store and returns the session ids that have
// any subagent/workflow events, busiest-first, plus each id's event count.
func seedBenchSessions(dir string) ([]string, map[string]int, error) {
	days, err := history.Days(dir)
	if err != nil {
		return nil, nil, err
	}
	counts := map[string]int{}
	for _, day := range days {
		f, err := os.Open(history.DayPath(dir, day))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Bytes()
			admitted := false
			for _, marker := range seedBenchTypeMarkers {
				if bytes.Contains(line, marker) {
					admitted = true
					break
				}
			}
			if !admitted {
				continue
			}
			var ev seedBenchLine
			if json.Unmarshal(line, &ev) != nil {
				continue
			}
			if ev.SessionID != "" && seedBenchInteresting(ev.Type) {
				counts[ev.SessionID]++
			}
		}
		f.Close()
	}
	ids := make([]string, 0, len(counts))
	for id := range counts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if counts[ids[i]] != counts[ids[j]] {
			return counts[ids[i]] > counts[ids[j]]
		}
		return ids[i] < ids[j]
	})
	return ids, counts, nil
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
