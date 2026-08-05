package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/testsupport"
)

const kB = 1024

// memFixture lays down a fake /proc holding two agent sessions and one
// unrelated process, and returns a sampler rooted at it.
//
//	100 claude            (agent A)   40 MB Pss,  1 MB SwapPss
//	  ├─ 101 bash                     10 MB Pss
//	  │    └─ 103 rg                   4 MB Pss   (grandchild)
//	  └─ 102 node                      6 MB Pss
//	200 claude            (agent B)   70 MB Pss
//	300 firefox           (unrelated) 900 MB Pss
func memFixture(t *testing.T) (*memorySampler, *testsupport.FakeProcTree) {
	t.Helper()
	tree := testsupport.NewFakeProcTree(t)
	add := func(pid, ppid int, comm string, pssKB, swapKB int) {
		tree.AddProcess(t, pid, testsupport.ProcSpec{
			Comm: comm, PPid: ppid, Rollup: &testsupport.RollupKB{Pss: pssKB, SwapPss: swapKB},
		})
	}
	add(100, 1, "claude", 40*kB, 1*kB)
	add(101, 100, "bash", 10*kB, 0)
	add(102, 100, "node", 6*kB, 0)
	add(103, 101, "rg", 4*kB, 0)
	add(200, 1, "claude", 70*kB, 0)
	add(300, 1, "firefox", 900*kB, 0)
	tree.SetMemInfo(t, testsupport.MemInfo(3*1024*kB))
	tree.SetPressureMemory(t, testsupport.PressureMemory(1.25, 556011549))
	return newMemorySamplerAt(tree.Root), tree
}

func TestSampleAttributesTheWholeProcessTreeWhenASessionHasDescendants(t *testing.T) {
	ms, _ := memFixture(t)

	tick := ms.sample([]int{100, 200})

	a, ok := tick.Sessions[100]
	if !ok {
		t.Fatal("no sample for pid 100")
	}
	if want := int64(40 * kB * kB); a.Agent.Pss != want {
		t.Errorf("agent Pss = %d, want %d (the agent process alone)", a.Agent.Pss, want)
	}
	if want := int64(1 * kB * kB); a.Agent.SwapPss != want {
		t.Errorf("agent SwapPss = %d, want %d", a.Agent.SwapPss, want)
	}
	// 40 + 10 + 6 + 4: the agent, both children, and the grandchild — but not the
	// unrelated 900 MB process, and not the other session.
	if want := int64(60 * kB * kB); a.Tree.Pss != want {
		t.Errorf("tree Pss = %d, want %d (agent + 2 children + grandchild)", a.Tree.Pss, want)
	}
	if a.Procs != 4 {
		t.Errorf("tree procs = %d, want 4", a.Procs)
	}
	if !a.Fresh {
		t.Error("a reading taken this tick should be Fresh")
	}

	// The second session is measured independently against the same table scan.
	b, ok := tick.Sessions[200]
	if !ok {
		t.Fatal("no sample for pid 200")
	}
	if want := int64(70 * kB * kB); b.Tree.Pss != want || b.Procs != 1 {
		t.Errorf("pid 200 tree = %d/%d procs, want %d/1", b.Tree.Pss, b.Procs, want)
	}
}

func TestSampleReadsMachinePressureOncePerTick(t *testing.T) {
	ms, _ := memFixture(t)

	tick := ms.sample([]int{100, 200})

	if want := int64(3 * 1024 * kB * kB); tick.Sys.AvailBytes != want {
		t.Errorf("avail = %d, want %d", tick.Sys.AvailBytes, want)
	}
	if !tick.Sys.PSI.Present {
		t.Fatal("PSI should be present when /proc/pressure/memory exists")
	}
	if tick.Sys.PSI.Avg10 != 1.25 || tick.Sys.PSI.TotalUS != 556011549 {
		t.Errorf("PSI = %+v, want avg10=1.25 total=556011549", tick.Sys.PSI)
	}
}

func TestSampleYieldsNoSampleWhenTheProcessIsGone(t *testing.T) {
	ms, tree := memFixture(t)

	// Sampled once while alive, so a last-known value exists to be wrongly reused.
	if _, ok := ms.sample([]int{100}).Sessions[100]; !ok {
		t.Fatal("precondition: pid 100 should sample while alive")
	}
	tree.RemoveProcess(t, 100)

	tick := ms.sample([]int{100})

	// A zero sample would read as "the session freed all its memory" and would
	// corrupt the peak and the time-weighted average, so there must be none at all.
	if got, ok := tick.Sessions[100]; ok {
		t.Errorf("a gone process yielded a sample %+v, want none", got)
	}
	if _, cached := ms.last[100]; cached {
		t.Error("a gone process should drop its cached reading")
	}
}

func TestSampleYieldsNoSampleWhenTheRollupIsEmpty(t *testing.T) {
	ms, tree := memFixture(t)
	// A zombie: the process directory is still there, but smaps_rollup carries no
	// Pss line because the address space is already gone.
	testsupport.WriteFile(t, filepath.Join(tree.PIDDir(100), "smaps_rollup"), "")

	if got, ok := ms.sample([]int{100}).Sessions[100]; ok {
		t.Errorf("an empty rollup yielded a sample %+v, want none", got)
	}
}

func TestSampleKeepsTheLastKnownValueWhenAReadFailsTransiently(t *testing.T) {
	ms, tree := memFixture(t)
	if _, ok := ms.sample([]int{100}).Sessions[100]; !ok {
		t.Fatal("precondition: pid 100 should sample while readable")
	}

	// Unreadable, but emphatically not gone: the process is still in the table and
	// still holds every page it had.
	rollup := filepath.Join(tree.PIDDir(100), "smaps_rollup")
	if err := os.Remove(rollup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(rollup, 0o755); err != nil {
		t.Fatal(err)
	}

	got, ok := ms.sample([]int{100}).Sessions[100]
	if !ok {
		t.Fatal("a transient read failure should keep the last-known reading, not drop it")
	}
	if want := int64(60 * kB * kB); got.Tree.Pss != want {
		t.Errorf("tree Pss = %d, want the last-known %d rather than a flap to zero", got.Tree.Pss, want)
	}
	if got.Fresh {
		t.Error("a repeated reading must not be marked Fresh — the log would take it as measured")
	}
}

func TestSampleDropsCachedReadingsForPidsItNoLongerSamples(t *testing.T) {
	ms, _ := memFixture(t)
	ms.sample([]int{100, 200})

	ms.sample([]int{100})

	if _, cached := ms.last[200]; cached {
		t.Error("a pid that is no longer sampled should not stay cached")
	}
	if _, cached := ms.last[100]; !cached {
		t.Error("a pid still being sampled should stay cached")
	}
}

func TestSampleReadsNothingWhenMemorySamplingIsOff(t *testing.T) {
	var ms *memorySampler // the disabled form

	tick := ms.sample([]int{100})

	if len(tick.Sessions) != 0 || tick.Sys.AvailBytes != 0 {
		t.Errorf("a disabled sampler produced %+v, want an empty tick", tick)
	}
}

// --- the emitted event ---

func memSession(pid int, sessionID string) *state.Session {
	sess := &state.Session{PID: pid, Agent: "claude", CWD: "/home/u/Projects/switchboard"}
	sess.Claude = &state.AgentInfo{SessionID: sessionID}
	return sess
}

func TestEventCarriesTheTreeFiguresAndTheTickPressure(t *testing.T) {
	ms, _ := memFixture(t)
	tick := ms.sample([]int{100})
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.Local)

	ev, ok := tick.event(memSession(100, "ce13c0f2"), now)
	if !ok {
		t.Fatal("a fresh reading should produce an event")
	}
	if ev.Type != history.EventMemorySample {
		t.Errorf("type = %q, want %q", ev.Type, history.EventMemorySample)
	}
	if ev.SessionID != "ce13c0f2" || ev.PID != 100 || ev.Agent != "claude" {
		t.Errorf("identity = %q/%d/%q, want ce13c0f2/100/claude", ev.SessionID, ev.PID, ev.Agent)
	}
	if ev.CWD == "" {
		t.Error("cwd must ride along so the sink can resolve the project before scrubbing it")
	}
	if ev.MemAgentPssBytes != 40*kB*kB || ev.MemAgentSwapBytes != 1*kB*kB {
		t.Errorf("agent = %d/%d bytes, want %d/%d", ev.MemAgentPssBytes, ev.MemAgentSwapBytes, 40*kB*kB, 1*kB*kB)
	}
	if ev.MemTreePssBytes != 60*kB*kB || ev.MemTreeProcs != 4 {
		t.Errorf("tree = %d bytes/%d procs, want %d/4", ev.MemTreePssBytes, ev.MemTreeProcs, 60*kB*kB)
	}
	if ev.SysAvailBytes != 3*1024*kB*kB {
		t.Errorf("sys avail = %d, want %d", ev.SysAvailBytes, 3*1024*kB*kB)
	}
	if ev.SysPsiSomeAvg10 != 1.25 || ev.SysPsiSomeTotalUs != 556011549 {
		t.Errorf("psi = %v/%d, want 1.25/556011549", ev.SysPsiSomeAvg10, ev.SysPsiSomeTotalUs)
	}
}

func TestEventOmitsPSIWhenTheKernelDoesNotMeasureIt(t *testing.T) {
	tree := testsupport.NewFakeProcTree(t)
	tree.AddProcess(t, 100, testsupport.ProcSpec{
		Comm: "claude", PPid: 1, Rollup: &testsupport.RollupKB{Pss: 40 * kB},
	})
	tree.SetMemInfo(t, testsupport.MemInfo(3*1024*kB))
	// No SetPressureMemory: a kernel built without CONFIG_PSI has no such file.
	ms := newMemorySamplerAt(tree.Root)

	ev, ok := ms.sample([]int{100}).event(memSession(100, "x"), time.Now())
	if !ok {
		t.Fatal("want an event")
	}
	// Absent means "not measured"; zero would mean "measured, and no stall".
	if ev.SysPsiSomeTotalUs != 0 || ev.SysPsiSomeAvg10 != 0 {
		t.Errorf("psi = %v/%d, want both zero so omitempty drops them", ev.SysPsiSomeAvg10, ev.SysPsiSomeTotalUs)
	}
	if ev.SysAvailBytes == 0 {
		t.Error("availability should still be reported when PSI is missing")
	}
}

func TestEventIsWithheldForASessionTheTickCouldNotRead(t *testing.T) {
	ms, tree := memFixture(t)
	ms.sample([]int{100})
	rollup := filepath.Join(tree.PIDDir(100), "smaps_rollup")
	os.Remove(rollup)
	os.Mkdir(rollup, 0o755)

	// The repeated last-known value reaches the live state but must not reach the
	// log, where it is indistinguishable from a genuine flat reading.
	if _, ok := ms.sample([]int{100}).event(memSession(100, "x"), time.Now()); ok {
		t.Error("a repeated (non-fresh) reading should produce no event")
	}
	if _, ok := (memoryTick{}).event(memSession(999, "x"), time.Now()); ok {
		t.Error("an unsampled session should produce no event")
	}
}

func TestLivePIDsListsEverySessionInTheSnapshot(t *testing.T) {
	snap := state.Snapshot{Sessions: []state.Session{{PID: 7}, {PID: 11}}}

	if got := livePIDs(snap); len(got) != 2 || got[0] != 7 || got[1] != 11 {
		t.Errorf("livePIDs = %v, want [7 11]", got)
	}
	if got := livePIDs(state.Snapshot{}); len(got) != 0 {
		t.Errorf("livePIDs of an empty snapshot = %v, want none", got)
	}
}
