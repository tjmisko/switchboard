package discovery

import (
	"errors"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/proc"
)

// fakeProcSource is the injected seam for scanner tests: a fixed pid list and
// per-pid Read result (info or error), with a Read call counter.
type fakeProcSource struct {
	pids  []int
	infos map[int]proc.Info
	errs  map[int]error
	reads int
}

func (f *fakeProcSource) AllPIDs() ([]int, error) { return f.pids, nil }

func (f *fakeProcSource) Read(pid int) (proc.Info, error) {
	f.reads++
	if e := f.errs[pid]; e != nil {
		return proc.Info{}, e
	}
	return f.infos[pid], nil
}

func claudeInfo(pid int) proc.Info { return proc.Info{PID: pid, Comm: "claude"} }

// §2.1 IsClaude — seed table (0.3 owns the full coverage). The Scanner
// seen-set state machine (§2.2) gets its harness consumer in §0.5 once the
// injectable procSource lands; this seed exercises the pure predicate only.
func TestIsClaude(t *testing.T) {
	tests := []struct {
		name string
		info proc.Info
		want bool
	}{
		{"wrong comm", proc.Info{Comm: "bash", Exe: "/usr/bin/bash"}, false},
		{"comm match, exe masked", proc.Info{Comm: "claude", Exe: ""}, true},
		{"comm match, exe under /claude/", proc.Info{Comm: "claude", Exe: "/home/u/.local/share/claude/claude"}, true},
		{"comm match, exe elsewhere", proc.Info{Comm: "claude", Exe: "/usr/bin/claude-impostor"}, false},
		{"case sensitive comm", proc.Info{Comm: "Claude", Exe: ""}, false},

		// A session invoked with flags or a positional prompt carries no
		// subcommand verb and stays a session.
		{"interactive --resume", proc.Info{Comm: "claude", Exe: "/x/claude/claude", Args: []string{"/x/claude/claude", "--resume"}}, true},
		{"interactive positional prompt", proc.Info{Comm: "claude", Exe: "/x/claude/claude", Args: []string{"claude", "fix the build"}}, true},

		// The detached `claude daemon run` background process shares comm + exe
		// with a real session but is NOT a session — this is the zombie-chip bug.
		// argv is the exact form observed in /proc/<pid>/cmdline.
		{"daemon run is not a session", proc.Info{
			Comm: "claude",
			Exe:  "/home/u/.local/share/claude/versions/2.1.158",
			Args: []string{"/home/u/.local/bin/claude", "daemon", "run", "--origin", "transient", "--spawned-by", `{"label":"claude","cwd":"/home/u/Projects/x/.worktrees/y","pid":224404}`},
		}, false},
		{"mcp subcommand is not a session", proc.Info{
			Comm: "claude",
			Exe:  "/x/claude/claude",
			Args: []string{"/x/claude/claude", "mcp", "serve"},
		}, false},
		// Exclusion holds even when the kernel masked the exe.
		{"daemon with masked exe", proc.Info{Comm: "claude", Exe: "", Args: []string{"claude", "daemon", "run"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsClaude(tt.info); got != tt.want {
				t.Errorf("IsClaude(%+v) = %v, want %v", tt.info, got, tt.want)
			}
		})
	}
}

// §2.2 Scanner — fires onAppeared once per newly-seen claude PID; Forget lets
// the next scan re-fire (recycled PID).
func TestScannerFiresOnceAndForgetReFires(t *testing.T) {
	src := &fakeProcSource{pids: []int{100}, infos: map[int]proc.Info{100: claudeInfo(100)}}
	s := newWithSource(src)

	var count int
	fire := func(proc.Info) { count++ }

	s.scanOnce(fire)
	s.scanOnce(fire)
	if count != 1 {
		t.Fatalf("fired %d times across two scans, want 1", count)
	}

	s.Forget(100)
	s.scanOnce(fire)
	if count != 2 {
		t.Fatalf("after Forget, fired total %d, want 2", count)
	}
}

// §2.2 Scanner — a PID whose Read errors is never remembered, so a later
// successful Read still fires.
func TestScannerErroredReadNotRemembered(t *testing.T) {
	src := &fakeProcSource{pids: []int{101}, errs: map[int]error{101: errors.New("gone")}}
	s := newWithSource(src)

	var count int
	fire := func(proc.Info) { count++ }

	s.scanOnce(fire)
	if count != 0 {
		t.Fatalf("errored read fired %d times, want 0", count)
	}

	delete(src.errs, 101)
	src.infos = map[int]proc.Info{101: claudeInfo(101)}
	s.scanOnce(fire)
	if count != 1 {
		t.Fatalf("after read recovered, fired %d, want 1 (was not remembered)", count)
	}
}

// §2.2 Scanner — a non-claude PID is never remembered, so it fires if it later
// becomes claude.
func TestScannerNonClaudeNotRemembered(t *testing.T) {
	src := &fakeProcSource{pids: []int{102}, infos: map[int]proc.Info{102: {PID: 102, Comm: "bash"}}}
	s := newWithSource(src)

	var count int
	fire := func(proc.Info) { count++ }

	s.scanOnce(fire)
	if count != 0 {
		t.Fatalf("non-claude fired %d times, want 0", count)
	}

	src.infos[102] = claudeInfo(102)
	s.scanOnce(fire)
	if count != 1 {
		t.Fatalf("after becoming claude, fired %d, want 1", count)
	}
}

// §2.2 ⚠ characterization: a PID recycled into a fresh claude WITHOUT a Forget
// is shadowed by the seen set and does not re-fire. Relies on procwatch always
// Forget-ing on death.
func TestScannerRecycledPIDShadowedWithoutForget(t *testing.T) {
	src := &fakeProcSource{pids: []int{100}, infos: map[int]proc.Info{100: claudeInfo(100)}}
	s := newWithSource(src)

	var fired int
	fire := func(proc.Info) { fired++ }

	s.scanOnce(fire) // fires for the original PID 100
	s.scanOnce(fire) // recycled claude on PID 100, no Forget → shadowed
	if fired != 1 {
		t.Fatalf("fired %d times, want 1 (recycled PID shadowed)", fired)
	}
}

// §2.2 Scanner — onAppeared runs WITHOUT the scanner lock held, so a callback
// that calls back into the scanner (e.g. Forget) cannot deadlock.
func TestScannerCallbackIsLockFree(t *testing.T) {
	src := &fakeProcSource{pids: []int{100}, infos: map[int]proc.Info{100: claudeInfo(100)}}
	s := newWithSource(src)

	done := make(chan struct{})
	go func() {
		s.scanOnce(func(i proc.Info) { s.Forget(i.PID) })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scanOnce deadlocked — callback ran under the scanner lock")
	}
}
