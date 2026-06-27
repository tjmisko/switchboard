package discovery

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/osproc"
)

// fakeProcSource is the injected seam for scanner tests: a fixed pid list and
// per-pid Read result (info or error), with a Read call counter.
type fakeProcSource struct {
	pids  []int
	infos map[int]osproc.Info
	errs  map[int]error
	reads int
}

func (f *fakeProcSource) AllPIDs() ([]int, error) { return f.pids, nil }

func (f *fakeProcSource) Read(pid int) (osproc.Info, error) {
	f.reads++
	if e := f.errs[pid]; e != nil {
		return osproc.Info{}, e
	}
	return f.infos[pid], nil
}

func claudeInfo(pid int) osproc.Info { return osproc.Info{PID: pid, Comm: "claude"} }

// §2.1 IsClaude — seed table (0.3 owns the full coverage). The Scanner
// seen-set state machine (§2.2) gets its harness consumer in §0.5 once the
// injectable procSource lands; this seed exercises the pure predicate only.
func TestIsClaude(t *testing.T) {
	tests := []struct {
		name string
		info osproc.Info
		want bool
	}{
		{"wrong comm", osproc.Info{Comm: "bash", Exe: "/usr/bin/bash"}, false},
		{"comm match, exe masked", osproc.Info{Comm: "claude", Exe: ""}, true},
		{"comm match, exe under /claude/", osproc.Info{Comm: "claude", Exe: "/home/u/.local/share/claude/claude"}, true},
		{"comm match, exe elsewhere", osproc.Info{Comm: "claude", Exe: "/usr/bin/claude-impostor"}, false},
		{"case sensitive comm", osproc.Info{Comm: "Claude", Exe: ""}, false},

		// A session invoked with flags or a positional prompt carries no
		// subcommand verb and stays a session.
		{"interactive --resume", osproc.Info{Comm: "claude", Exe: "/x/claude/claude", Args: []string{"/x/claude/claude", "--resume"}}, true},
		{"interactive positional prompt", osproc.Info{Comm: "claude", Exe: "/x/claude/claude", Args: []string{"claude", "fix the build"}}, true},

		// The detached `claude daemon run` background process shares comm + exe
		// with a real session but is NOT a session — this is the zombie-chip bug.
		// argv is the exact form observed in /proc/<pid>/cmdline.
		{"daemon run is not a session", osproc.Info{
			Comm: "claude",
			Exe:  "/home/u/.local/share/claude/versions/2.1.158",
			Args: []string{"/home/u/.local/bin/claude", "daemon", "run", "--origin", "transient", "--spawned-by", `{"label":"claude","cwd":"/home/u/Projects/x/.worktrees/y","pid":224404}`},
		}, false},
		{"mcp subcommand is not a session", osproc.Info{
			Comm: "claude",
			Exe:  "/x/claude/claude",
			Args: []string{"/x/claude/claude", "mcp", "serve"},
		}, false},
		// Exclusion holds even when the kernel masked the exe.
		{"daemon with masked exe", osproc.Info{Comm: "claude", Exe: "", Args: []string{"claude", "daemon", "run"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsClaude(tt.info); got != tt.want {
				t.Errorf("IsClaude(%+v) = %v, want %v", tt.info, got, tt.want)
			}
		})
	}
}

// IsCodex — codex is a single binary whose subcommand is argv[1]; the bare
// invocation, a leading flag, a positional prompt, resume, and fork are
// interactive sessions, while exec/app-server/mcp/utility verbs are not.
func TestIsCodex(t *testing.T) {
	tests := []struct {
		name string
		info osproc.Info
		want bool
	}{
		{"wrong comm", osproc.Info{Comm: "claude"}, false},
		{"bare codex is a session", osproc.Info{Comm: "codex", Args: []string{"/usr/local/bin/codex"}}, true},
		{"no args at all", osproc.Info{Comm: "codex"}, true},
		{"leading flag is a session", osproc.Info{Comm: "codex", Args: []string{"codex", "--model", "gpt-5-codex"}}, true},
		{"positional prompt is a session", osproc.Info{Comm: "codex", Args: []string{"codex", "fix the build"}}, true},
		{"resume is a session", osproc.Info{Comm: "codex", Args: []string{"codex", "resume"}}, true},
		{"fork is a session", osproc.Info{Comm: "codex", Args: []string{"codex", "fork"}}, true},
		{"exec is not a session", osproc.Info{Comm: "codex", Args: []string{"codex", "exec", "do it"}}, false},
		{"exec alias e is not a session", osproc.Info{Comm: "codex", Args: []string{"codex", "e"}}, false},
		{"app-server is not a session", osproc.Info{Comm: "codex", Args: []string{"codex", "app-server"}}, false},
		{"mcp-server is not a session", osproc.Info{Comm: "codex", Args: []string{"codex", "mcp-server"}}, false},
		{"mcp is not a session", osproc.Info{Comm: "codex", Args: []string{"codex", "mcp"}}, false},
		{"login is not a session", osproc.Info{Comm: "codex", Args: []string{"codex", "login"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsCodex(tt.info); got != tt.want {
				t.Errorf("IsCodex(%+v) = %v, want %v", tt.info, got, tt.want)
			}
		})
	}
}

// Classify is the single predicate the scanner filters on: a claude session, a
// codex session, or neither.
func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		info osproc.Info
		want Agent
	}{
		{"claude session", osproc.Info{Comm: "claude", Exe: "/x/claude/claude"}, AgentClaude},
		{"claude daemon is neither", osproc.Info{Comm: "claude", Exe: "/x/claude/claude", Args: []string{"claude", "daemon", "run"}}, AgentNone},
		{"codex session", osproc.Info{Comm: "codex", Args: []string{"codex"}}, AgentCodex},
		{"codex exec is neither", osproc.Info{Comm: "codex", Args: []string{"codex", "exec"}}, AgentNone},
		{"bash is neither", osproc.Info{Comm: "bash"}, AgentNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.info); got != tt.want {
				t.Errorf("Classify(%+v) = %q, want %q", tt.info, got, tt.want)
			}
		})
	}
}

// §2.2 Scanner — fires onAppeared once per newly-seen claude PID; Forget lets
// the next scan re-fire (recycled PID).
func TestScannerFiresOnceAndForgetReFires(t *testing.T) {
	src := &fakeProcSource{pids: []int{100}, infos: map[int]osproc.Info{100: claudeInfo(100)}}
	s := newWithSource(src)

	var count int
	fire := func(osproc.Info) { count++ }

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
	fire := func(osproc.Info) { count++ }

	s.scanOnce(fire)
	if count != 0 {
		t.Fatalf("errored read fired %d times, want 0", count)
	}

	delete(src.errs, 101)
	src.infos = map[int]osproc.Info{101: claudeInfo(101)}
	s.scanOnce(fire)
	if count != 1 {
		t.Fatalf("after read recovered, fired %d, want 1 (was not remembered)", count)
	}
}

// §2.2 Scanner — a non-claude PID is never remembered, so it fires if it later
// becomes claude.
func TestScannerNonClaudeNotRemembered(t *testing.T) {
	src := &fakeProcSource{pids: []int{102}, infos: map[int]osproc.Info{102: {PID: 102, Comm: "bash"}}}
	s := newWithSource(src)

	var count int
	fire := func(osproc.Info) { count++ }

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
	src := &fakeProcSource{pids: []int{100}, infos: map[int]osproc.Info{100: claudeInfo(100)}}
	s := newWithSource(src)

	var fired int
	fire := func(osproc.Info) { fired++ }

	s.scanOnce(fire) // fires for the original PID 100
	s.scanOnce(fire) // recycled claude on PID 100, no Forget → shadowed
	if fired != 1 {
		t.Fatalf("fired %d times, want 1 (recycled PID shadowed)", fired)
	}
}

// §2.2 Scanner — onAppeared runs WITHOUT the scanner lock held, so a callback
// that calls back into the scanner (e.g. Forget) cannot deadlock.
func TestScannerCallbackIsLockFree(t *testing.T) {
	src := &fakeProcSource{pids: []int{100}, infos: map[int]osproc.Info{100: claudeInfo(100)}}
	s := newWithSource(src)

	done := make(chan struct{})
	go func() {
		s.scanOnce(func(i osproc.Info) { s.Forget(i.PID) })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scanOnce deadlocked — callback ran under the scanner lock")
	}
}

// fakeOSSource is a stand-in osproc.Source for the runtime adapter path (the
// path New() wires, as opposed to the narrow procSource fake above). It serves
// a fixed process table; Watch/Stop are no-ops because the scanner never calls
// them (death-watch lives in the daemon, not discovery). It deliberately does
// NOT implement AllPIDs, so the osprocSource adapter drives it from Enumerate —
// exercising the fallback path a Source without the cheap pid-lister takes.
type fakeOSSource struct {
	infos map[int]osproc.Info
}

func (s fakeOSSource) Enumerate() ([]osproc.Info, error) {
	out := make([]osproc.Info, 0, len(s.infos))
	for _, info := range s.infos {
		out = append(out, info)
	}
	return out, nil
}

func (s fakeOSSource) Read(pid int) (osproc.Info, error) {
	info, ok := s.infos[pid]
	if !ok {
		return osproc.Info{PID: pid}, osproc.ErrGone
	}
	return info, nil
}

func (fakeOSSource) Watch(context.Context, int, func()) error { return nil }
func (fakeOSSource) Stop(int)                                 {}

// fakeOSSourceWithPIDs adds the optional AllPIDs fast-path, so the adapter uses
// the cheap pid-lister upgrade (the Linux source's hot path) instead of deriving
// pids from a full Enumerate.
type fakeOSSourceWithPIDs struct {
	fakeOSSource
	pids []int
}

func (s fakeOSSourceWithPIDs) AllPIDs() ([]int, error) { return s.pids, nil }

// New(osproc.Source) drives the scanner through the osprocSource adapter, using
// the cheap AllPIDs fast-path when the Source provides it. Only the claude
// process is classified and fired; the bash sibling is ignored.
func TestScannerOverOsprocSourceFastPath(t *testing.T) {
	src := fakeOSSourceWithPIDs{
		fakeOSSource: fakeOSSource{infos: map[int]osproc.Info{
			100: {PID: 100, Comm: "claude", Exe: "/x/claude/claude"},
			101: {PID: 101, Comm: "bash", Exe: "/usr/bin/bash"},
		}},
		pids: []int{100, 101},
	}
	s := New(src)

	var fired []int
	s.scanOnce(func(i osproc.Info) { fired = append(fired, i.PID) })

	if len(fired) != 1 || fired[0] != 100 {
		t.Fatalf("fast-path scan fired %v, want [100]", fired)
	}
}

// When the Source does not provide the AllPIDs fast-path, the adapter derives
// the pid list from Enumerate and still classifies/fires correctly — so a
// backend (e.g. a future one) drops in without implementing the cheap lister.
func TestScannerOverOsprocSourceEnumerateFallback(t *testing.T) {
	src := fakeOSSource{infos: map[int]osproc.Info{
		200: {PID: 200, Comm: "claude", Exe: "/home/u/.local/share/claude/claude"},
		201: {PID: 201, Comm: "node", Exe: "/usr/bin/node"},
	}}
	s := New(src)

	var fired []int
	s.scanOnce(func(i osproc.Info) { fired = append(fired, i.PID) })

	if len(fired) != 1 || fired[0] != 200 {
		t.Fatalf("enumerate-fallback scan fired %v, want [200]", fired)
	}
}
