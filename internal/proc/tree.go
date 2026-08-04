package proc

import (
	"slices"
	"strconv"
	"strings"
)

// TreeMem is one session's resident cost: the agent process on its own, the
// whole process tree it roots, and how many processes contributed to the tree
// figure.
//
// The two buckets are measured separately and never subtracted from a third.
// What a session's spawned work cost is derived as Tree - Agent by the
// consumer; measuring "the children" as its own sum would double-count the
// pages a child shares with its parent, which for a forked process is most of
// them.
type TreeMem struct {
	Agent Mem
	Tree  Mem
	Procs int
}

// ParentMap reads /proc/<pid>/stat for every visible process and returns the
// pid→ppid map. Processes that vanish between the directory listing and their
// stat read are simply absent from the map.
//
// Deliberately not built on Read: the walk needs exactly one field, while Read
// spends roughly seven more syscalls per process (three fd readlinks, exe,
// cwd, cmdline, status) — multiplied by the several hundred processes on a
// desktop, every reconcile tick.
func ParentMap() (map[int]int, error) { return hostProc.ParentMap() }

func (r *Reader) ParentMap() (map[int]int, error) {
	pids, err := r.AllPIDs()
	if err != nil {
		return nil, err
	}
	parents := make(map[int]int, len(pids))
	for _, pid := range pids {
		stat, err := readSmallFile(r.pidPath(pid, "stat"))
		if err != nil {
			continue
		}
		if ppid, ok := parseStatPPID(stat); ok {
			parents[pid] = ppid
		}
	}
	return parents, nil
}

// Tree returns root followed by every descendant of root, as seen in a single
// snapshot of the process table.
func Tree(root int) ([]int, error) { return hostProc.Tree(root) }

func (r *Reader) Tree(root int) ([]int, error) {
	parents, err := r.ParentMap()
	if err != nil {
		return nil, err
	}
	return treePIDs(parents, root), nil
}

// TreeMemory sums PSS and SwapPss over root and all its descendants, taking
// its own snapshot of the process table.
//
// A tick sampling several sessions should call ParentMap once and use
// TreeMemoryFrom instead: the table scan is ~4 ms of the ~5 ms this call costs
// on a 384-process desktop, so paying for it per session is most of the work.
func TreeMemory(root int) (TreeMem, error) { return hostProc.TreeMemory(root) }

func (r *Reader) TreeMemory(root int) (TreeMem, error) {
	parents, err := r.ParentMap()
	if err != nil {
		return TreeMem{}, err
	}
	return r.TreeMemoryFrom(parents, root)
}

// TreeMemoryFrom sums PSS and SwapPss over root and all its descendants as
// they appear in a parent map the caller already built. Sampling every live
// session against one snapshot also makes the readings mutually consistent:
// each process is attributed to exactly one tree, even if it is reparented
// while the tick runs.
//
// The process tree is the unit of attribution because subagents have no pids
// of their own — they are messages within the agent process, and the work they
// farm out shows up as child processes. Nothing narrower can capture them.
//
// A descendant that vanishes mid-walk, or that we are not permitted to read,
// is skipped rather than failing the sample: a tree of short-lived shell
// commands would otherwise almost never yield a complete reading. Failing to
// read the ROOT is an error, because that is the agent's own figure and a tree
// total without it would be silently short.
func TreeMemoryFrom(parents map[int]int, root int) (TreeMem, error) {
	return hostProc.TreeMemoryFrom(parents, root)
}

func (r *Reader) TreeMemoryFrom(parents map[int]int, root int) (TreeMem, error) {
	agent, err := r.Memory(root)
	if err != nil {
		return TreeMem{}, err
	}

	out := TreeMem{Agent: agent, Tree: agent, Procs: 1}
	for _, pid := range treePIDs(parents, root) {
		if pid == root {
			continue
		}
		mem, err := r.Memory(pid)
		if err != nil {
			continue
		}
		out.Tree.Rss += mem.Rss
		out.Tree.Pss += mem.Pss
		out.Tree.SwapPss += mem.SwapPss
		out.Procs++
	}
	return out, nil
}

// treePIDs returns root followed by every descendant of root in a pid→ppid
// map, breadth-first and pid-sorted within each generation so the result is
// deterministic.
//
// The visited set is what bounds the walk. A process table sampled while the
// kernel is reparenting can present a cycle (and a pid that is its own parent
// is representable), so a walk without one can spin forever on input that is
// merely unlucky rather than corrupt.
func treePIDs(parents map[int]int, root int) []int {
	children := make(map[int][]int, len(parents))
	for pid, ppid := range parents {
		children[ppid] = append(children[ppid], pid)
	}
	for _, kids := range children {
		slices.Sort(kids)
	}

	visited := map[int]bool{root: true}
	tree := []int{root}
	for queue := []int{root}; len(queue) > 0; {
		pid := queue[0]
		queue = queue[1:]
		for _, kid := range children[pid] {
			if visited[kid] {
				continue
			}
			visited[kid] = true
			tree = append(tree, kid)
			queue = append(queue, kid)
		}
	}
	return tree
}

// parseStatPPID pulls the ppid (field 4) out of a /proc/<pid>/stat line.
//
// The scan starts after the LAST ')' rather than splitting the line from the
// left, because field 2 is the comm written raw and unquoted — it may contain
// spaces and parens, and real ones do: "(Web Content)", "((sd-pam))". Counting
// fields from the left lands on part of the comm instead of the ppid, and does
// so only for the processes whose names happen to contain a space, which is
// why it survives casual testing.
func parseStatPPID(stat string) (int, bool) {
	commEnd := strings.LastIndex(stat, ")")
	if commEnd < 0 {
		return 0, false
	}
	fields := strings.Fields(stat[commEnd+1:])
	if len(fields) < 2 { // [0] is the run state, [1] is the ppid
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return ppid, true
}
