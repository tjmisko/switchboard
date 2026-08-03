package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/proc"
	"github.com/tjmisko/switchboard/internal/state"
)

// TestMemorySamplerLiveCost measures what memory sampling costs on the machine
// it is running on, split into the part that runs outside the store lock (the
// /proc reads) and the part that runs inside it (the assignment and the
// sink.Record). The second figure is the one that matters: it is what every RPC
// reader and every hook waits behind.
//
// Off by default — it reads the real /proc and its numbers depend on what is
// running, so it is a measuring instrument rather than an assertion:
//
//	SWITCHBOARD_LIVE_MEMORY=1 go test ./cmd/switchboard -run LiveCost -v
//
// Follows the SWITCHBOARD_LIVE_CONFORMANCE precedent for a test that needs the
// real system underneath it.
func TestMemorySamplerLiveCost(t *testing.T) {
	if os.Getenv("SWITCHBOARD_LIVE_MEMORY") != "1" {
		t.Skip("set SWITCHBOARD_LIVE_MEMORY=1 to measure against the live /proc")
	}
	pids := liveAgentPIDs(t)
	if len(pids) == 0 {
		t.Skip("no live claude/codex processes to measure")
	}
	all, err := proc.AllPIDs()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("machine: %d processes, %d agent sessions (%v)", len(all), len(pids), pids)

	const rounds = 20
	ms := newMemorySampler()

	// The process-table scan, which one tick pays once no matter how many
	// sessions are live.
	start := time.Now()
	for range rounds {
		if _, err := proc.ParentMap(); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("ParentMap:                    %v", time.Since(start)/rounds)

	// A whole pass: the pressure read, the one table scan, and a tree walk per
	// session. All of it OUTSIDE the lock.
	var tick memoryTick
	start = time.Now()
	for range rounds {
		tick = ms.sample(pids)
	}
	outside := time.Since(start) / rounds
	t.Logf("full sample pass (unlocked):  %v  for %d sessions", outside, len(tick.Sessions))

	// The naive shape for contrast: TreeMemory per session re-scans the process
	// table each time, so the scan is paid once per session rather than once per
	// tick.
	start = time.Now()
	for range rounds {
		for _, pid := range pids {
			_, _ = proc.TreeMemory(pid)
		}
	}
	t.Logf("per-session TreeMemory:       %v  (the shape NOT used)", time.Since(start)/rounds)

	// What the tick actually added inside store.Apply: a map lookup, two field
	// assignments, the event, and a non-blocking channel send.
	histDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailMinimal, Dir: histDir})
	sessions := make([]*state.Session, 0, len(pids))
	for _, pid := range pids {
		sessions = append(sessions, memSession(pid, "live"))
	}
	now := time.Now()
	start = time.Now()
	for range rounds {
		for _, sess := range sessions {
			if reading, ok := tick.Sessions[sess.PID]; ok {
				sess.MemAgentBytes = reading.Agent.Pss
				sess.MemTreeBytes = reading.Tree.Pss
			}
			if ev, ok := tick.event(sess, now); ok {
				sink.Record(ev)
			}
		}
	}
	inside := time.Since(start) / rounds
	t.Logf("added lock-hold (in Apply):   %v  for %d sessions", inside, len(sessions))
	t.Logf("kept out of the lock:         %v", outside-inside)

	for pid, mem := range tick.Sessions {
		t.Logf("  pid %-7d agent %6.1f MB  tree %6.1f MB  %d procs",
			pid, float64(mem.Agent.Pss)/(1<<20), float64(mem.Tree.Pss)/(1<<20), mem.Procs)
	}
	t.Logf("  MemAvailable %.1f MB  psi some avg10 %.2f total %d us",
		float64(tick.Sys.AvailBytes)/(1<<20), tick.Sys.PSI.Avg10, tick.Sys.PSI.TotalUS)

	// End to end: the samples just recorded must come back off disk with their
	// figures intact, at the minimal tier the daemon writes by default.
	sink.Close()
	written, err := history.ReadDay(histDir, now.Format("2006-01-02"))
	if err != nil {
		t.Fatal(err)
	}
	if len(written) == 0 {
		t.Fatal("no memory_sample lines were written")
	}
	for _, ev := range written[:min(len(written), len(pids))] {
		if ev.Type != history.EventMemorySample || ev.MemAgentPssBytes == 0 || ev.MemTreeProcs == 0 {
			t.Errorf("round-tripped sample is missing its figures: %+v", ev)
		}
		if ev.CWD != "" {
			t.Errorf("minimal tier leaked cwd %q", ev.CWD)
		}
	}
	t.Logf("wrote %d memory_sample lines; first: %+v", len(written), written[0])
}

// liveAgentPIDs finds the claude/codex processes currently running, by comm.
func liveAgentPIDs(t *testing.T) []int {
	t.Helper()
	pids, err := proc.AllPIDs()
	if err != nil {
		t.Fatal(err)
	}
	var agents []int
	for _, pid := range pids {
		comm, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
		if err != nil {
			continue
		}
		switch strings.TrimSpace(string(comm)) {
		case "claude", "codex":
			agents = append(agents, pid)
		}
	}
	return agents
}
