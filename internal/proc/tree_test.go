package proc

import (
	"os"
	"slices"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/testsupport"
)

func TestParseStatPPID(t *testing.T) {
	tests := []struct {
		name string
		stat string
		want int
		ok   bool
	}{
		{
			name: "should parse the ppid when the stat line is a live kernel's output",
			stat: "400114 (bat) R 399988 400114 399988 0 -1 4194304 177 0 0 0 0 0 0 0 20 0 2 0 30634763\n",
			want: 399988,
			ok:   true,
		},
		{
			// Counting fields from the left lands on "Content)" here.
			name: "should parse the ppid when the comm contains a space",
			stat: "1234 (Web Content) S 999 1234 999 0 -1 4194304 177\n",
			want: 999,
			ok:   true,
		},
		{
			// systemd's user session helper is literally named "(sd-pam)".
			name: "should parse the ppid when the comm contains parens",
			stat: "1234 ((sd-pam)) S 777 1234 777 0 -1 4194304 177\n",
			want: 777,
			ok:   true,
		},
		{
			name: "should parse the ppid when the comm contains both spaces and parens",
			stat: "1234 (a b) c (d) S 555 1234 555 0 -1 4194304 177\n",
			want: 555,
			ok:   true,
		},
		{
			name: "should report failure when the line has no comm parens",
			stat: "1234 bat R 999\n",
			ok:   false,
		},
		{
			name: "should report failure when the line is truncated after the comm",
			stat: "1234 (bat) R\n",
			ok:   false,
		},
		{
			name: "should report failure when the input is empty",
			stat: "",
			ok:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseStatPPID(tt.stat)
			if ok != tt.ok {
				t.Fatalf("parseStatPPID ok = %v, want %v", ok, tt.ok)
			}
			if ok && got != tt.want {
				t.Errorf("parseStatPPID = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestTreePIDs_ShouldReturnRootAndEveryDescendantWhenTheTreeIsNested(t *testing.T) {
	// 600 ─ 601 ─ 602
	//     └ 603
	parents := map[int]int{500: 1, 600: 500, 601: 600, 602: 601, 603: 600}

	got := treePIDs(parents, 600)
	want := []int{600, 601, 603, 602}
	if !slices.Equal(got, want) {
		t.Errorf("treePIDs = %v, want %v (root first, then breadth-first)", got, want)
	}
}

func TestTreePIDs_ShouldExcludeASiblingSessionsTreeWhenBothShareAParent(t *testing.T) {
	// Two claude sessions under one shell. Attributing 700's pages to 600
	// would make every session on a busy machine look enormous.
	parents := map[int]int{500: 1, 600: 500, 601: 600, 700: 500, 701: 700}

	got := treePIDs(parents, 600)
	for _, unwanted := range []int{500, 700, 701} {
		if slices.Contains(got, unwanted) {
			t.Errorf("treePIDs(600) = %v, must not contain %d", got, unwanted)
		}
	}
	if want := []int{600, 601}; !slices.Equal(got, want) {
		t.Errorf("treePIDs = %v, want %v", got, want)
	}
}

func TestTreePIDs_ShouldTerminateWhenTheParentMapIsCyclic(t *testing.T) {
	// A process table sampled mid-reparent can present a cycle. Without the
	// visited set this walk never returns, so the failure mode is a hung
	// reconcile tick rather than a wrong number.
	for _, tt := range []struct {
		name    string
		parents map[int]int
		root    int
		want    []int
	}{
		{"two-node cycle", map[int]int{10: 11, 11: 10, 12: 10}, 10, []int{10, 11, 12}},
		{"self-parent", map[int]int{5: 5}, 5, []int{5}},
		{"longer cycle", map[int]int{20: 22, 21: 20, 22: 21}, 20, []int{20, 21, 22}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			done := make(chan []int, 1)
			go func() { done <- treePIDs(tt.parents, tt.root) }()
			select {
			case got := <-done:
				slices.Sort(got)
				if !slices.Equal(got, tt.want) {
					t.Errorf("treePIDs = %v, want %v", got, tt.want)
				}
			case <-timeout(t):
				t.Fatalf("treePIDs did not terminate on a cyclic parent map")
			}
		})
	}
}

func TestTreePIDs_ShouldReturnJustTheRootWhenItHasNoChildren(t *testing.T) {
	if got := treePIDs(map[int]int{600: 500}, 600); !slices.Equal(got, []int{600}) {
		t.Errorf("treePIDs = %v, want [600]", got)
	}
}

func TestTreeMemory_ShouldSumExactlyTheRootsTreeWhenASiblingSessionIsRunning(t *testing.T) {
	tree := testsupport.NewFakeProcTree(t)
	// shell 500
	//   claude 600 ─ node 601 ─ rg 602
	//              └ bash 603
	//   claude 700 ─ node 701          (a second session; must not be counted)
	addProc(t, tree, 500, 1, "bash", 1000)
	addProc(t, tree, 600, 500, "claude", 400000)
	addProc(t, tree, 601, 600, "node", 50000)
	addProc(t, tree, 602, 601, "rg", 10000)
	addProc(t, tree, 603, 600, "bash", 5000)
	addProc(t, tree, 700, 500, "claude", 300000)
	addProc(t, tree, 701, 700, "node", 90000)

	got, err := NewReader(tree.Root).TreeMemory(600)
	if err != nil {
		t.Fatalf("TreeMemory(600): %v", err)
	}
	if want := int64(400000) * 1024; got.Agent.Pss != want {
		t.Errorf("Agent.Pss = %d, want %d", got.Agent.Pss, want)
	}
	if want := int64(400000+50000+10000+5000) * 1024; got.Tree.Pss != want {
		t.Errorf("Tree.Pss = %d, want %d", got.Tree.Pss, want)
	}
	if got.Procs != 4 {
		t.Errorf("Procs = %d, want 4", got.Procs)
	}
	// The whole point of the tree unit: spawned work is Tree - Agent, and it
	// is nonzero here because the children are real.
	if got.Tree.Pss <= got.Agent.Pss {
		t.Errorf("Tree.Pss = %d not greater than Agent.Pss = %d", got.Tree.Pss, got.Agent.Pss)
	}
}

func TestTreeMemory_ShouldSkipAChildThatVanishedMidWalkRatherThanFailTheSample(t *testing.T) {
	// A tree of short-lived shell commands loses members constantly; failing
	// the sample on any one of them would mean almost never getting a reading.
	tree := testsupport.NewFakeProcTree(t)
	addProc(t, tree, 600, 500, "claude", 400000)
	addProc(t, tree, 601, 600, "node", 50000)
	// 602 is in the process table but its smaps_rollup is unreadable — the
	// shape a process that exited mid-walk, a kernel thread, and another
	// user's process all present.
	tree.AddProcess(t, 602, testsupport.ProcSpec{Comm: "rg", PPid: 600})

	got, err := NewReader(tree.Root).TreeMemory(600)
	if err != nil {
		t.Fatalf("TreeMemory(600) errored on a vanished child: %v", err)
	}
	if want := int64(450000) * 1024; got.Tree.Pss != want {
		t.Errorf("Tree.Pss = %d, want %d", got.Tree.Pss, want)
	}
	if got.Procs != 2 {
		t.Errorf("Procs = %d, want 2 — only processes that contributed a reading count", got.Procs)
	}
}

func TestTreeMemory_ShouldErrorWhenTheRootItselfCannotBeRead(t *testing.T) {
	// Losing the agent's own figure is not a partial sample; a tree total
	// without the root would be silently short.
	tree := testsupport.NewFakeProcTree(t)
	tree.AddProcess(t, 600, testsupport.ProcSpec{Comm: "claude", PPid: 500})
	addProc(t, tree, 601, 600, "node", 50000)

	if _, err := NewReader(tree.Root).TreeMemory(600); err == nil {
		t.Errorf("TreeMemory err = nil when the root had no rollup, want an error")
	}
}

func TestTreeMemory_ShouldSumSwapAlongsideResidentPagesWhenTheTreeIsSwapping(t *testing.T) {
	tree := testsupport.NewFakeProcTree(t)
	tree.AddProcess(t, 600, testsupport.ProcSpec{
		Comm: "claude", PPid: 500,
		Rollup: &testsupport.RollupKB{Pss: 400000, SwapPss: 20000},
	})
	tree.AddProcess(t, 601, testsupport.ProcSpec{
		Comm: "node", PPid: 600,
		Rollup: &testsupport.RollupKB{Pss: 50000, SwapPss: 3000},
	})

	got, err := NewReader(tree.Root).TreeMemory(600)
	if err != nil {
		t.Fatalf("TreeMemory(600): %v", err)
	}
	if want := int64(20000) * 1024; got.Agent.SwapPss != want {
		t.Errorf("Agent.SwapPss = %d, want %d", got.Agent.SwapPss, want)
	}
	if want := int64(23000) * 1024; got.Tree.SwapPss != want {
		t.Errorf("Tree.SwapPss = %d, want %d", got.Tree.SwapPss, want)
	}
}

func TestTreeMemoryFrom_ShouldAgreeWithTreeMemoryWhenGivenTheSameSnapshot(t *testing.T) {
	// The shared-snapshot path is what the reconcile tick uses, so it must not
	// be a second implementation that can drift from the one-shot call.
	tree := testsupport.NewFakeProcTree(t)
	addProc(t, tree, 600, 500, "claude", 400000)
	addProc(t, tree, 601, 600, "node", 50000)
	addProc(t, tree, 602, 601, "rg", 10000)
	addProc(t, tree, 700, 500, "claude", 300000)
	reader := NewReader(tree.Root)

	oneShot, err := reader.TreeMemory(600)
	if err != nil {
		t.Fatalf("TreeMemory: %v", err)
	}
	parents, err := reader.ParentMap()
	if err != nil {
		t.Fatalf("ParentMap: %v", err)
	}
	shared, err := reader.TreeMemoryFrom(parents, 600)
	if err != nil {
		t.Fatalf("TreeMemoryFrom: %v", err)
	}
	if shared != oneShot {
		t.Errorf("TreeMemoryFrom = %+v, TreeMemory = %+v; want agreement", shared, oneShot)
	}
	if shared.Procs != 3 {
		t.Errorf("Procs = %d, want 3", shared.Procs)
	}
}

func TestParentMap_ShouldReportOurOwnParentWhenReadingTheLiveProc(t *testing.T) {
	parents, err := ParentMap()
	if err != nil {
		t.Fatalf("ParentMap: %v", err)
	}
	self := os.Getpid()
	ppid, ok := parents[self]
	if !ok {
		t.Fatalf("ParentMap has no entry for our own pid %d", self)
	}
	if ppid != os.Getppid() {
		t.Errorf("ParentMap[self] = %d, want %d", ppid, os.Getppid())
	}
}

func TestParentMap_ShouldSkipAProcessWithNoStatWhenBuildingTheMap(t *testing.T) {
	tree := testsupport.NewFakeProcTree(t)
	addProc(t, tree, 600, 500, "claude", 400000)
	if err := os.Remove(tree.PIDDir(600) + "/stat"); err != nil {
		t.Fatalf("remove stat: %v", err)
	}

	parents, err := NewReader(tree.Root).ParentMap()
	if err != nil {
		t.Fatalf("ParentMap: %v", err)
	}
	if _, ok := parents[600]; ok {
		t.Errorf("ParentMap contains 600, whose stat was unreadable")
	}
}

func addProc(t testing.TB, tree *testsupport.FakeProcTree, pid, ppid int, comm string, pssKB int) {
	t.Helper()
	tree.AddProcess(t, pid, testsupport.ProcSpec{
		Comm:   comm,
		PPid:   ppid,
		Rollup: &testsupport.RollupKB{Pss: pssKB},
	})
}

// timeout returns a channel that fires if a walk has not returned promptly,
// turning a nonterminating walk into a test failure instead of a hung run.
func timeout(t testing.TB) <-chan struct{} {
	t.Helper()
	ch := make(chan struct{})
	timer := time.AfterFunc(5*time.Second, func() { close(ch) })
	t.Cleanup(func() { timer.Stop() })
	return ch
}
