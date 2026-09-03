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

		// macOS sets p_comm from the RESOLVED binary, so the versioned-symlink
		// install (~/.local/bin/claude -> …/versions/2.1.259) reports the
		// version string. argv[0] is what still carries the name. Without this
		// every macOS session was undiscoverable.
		{"should discover session when comm is a version string", osproc.Info{
			Comm: "2.1.259",
			Exe:  "/Users/u/.local/share/claude/versions/2.1.259",
			Args: []string{"claude"},
		}, true},
		{"should still filter daemon when comm is a version string", osproc.Info{
			Comm: "2.1.259",
			Exe:  "/Users/u/.local/share/claude/versions/2.1.259",
			Args: []string{"/Users/u/.local/bin/claude", "daemon", "run"},
		}, false},
		// Why exe is validation and not identity: a sibling helper shipped in
		// the same versioned payload sits under /claude/ but is not a session.
		{"should reject sibling binary under a claude directory", osproc.Info{
			Comm: "helper",
			Exe:  "/Users/u/.local/share/claude/versions/2.1.259/helper",
			Args: []string{"helper"},
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsClaude(tt.info); got != tt.want {
				t.Errorf("IsClaude(%+v) = %v, want %v", tt.info, got, tt.want)
			}
		})
	}
}

// claudeExeValid is the only OS-dependent part of IsClaude. Testing it with an
// explicit goos exercises the darwin branch on a Linux host and proves the macOS
// broadening does NOT loosen Linux matching: the native-installer / Homebrew /
// npm layouts (…/bin/claude, no /claude/ directory) are accepted on darwin but
// rejected on linux, while the /claude/ dev-build path and a masked exe are
// accepted on both, and claude-impostor is rejected on both.
func TestClaudeExeValid(t *testing.T) {
	tests := []struct {
		name       string
		exe        string
		linux, mac bool
	}{
		{"masked exe", "", true, true},
		{"dev build under /claude/", "/home/u/.local/share/claude/claude", true, true},
		{"impostor", "/usr/bin/claude-impostor", false, false},
		{"native installer ~/.local/bin/claude", "/Users/u/.local/bin/claude", false, true},
		{"~/.claude/bin/claude", "/Users/u/.claude/bin/claude", false, true},
		{"homebrew /opt/homebrew/bin/claude", "/opt/homebrew/bin/claude", false, true},
		{"npm global node_modules", "/usr/local/lib/node_modules/@anthropic-ai/claude-code/cli/claude", false, true},
		{"bare relative exe", "claude", false, true},
		{"node is not claude", "/usr/local/bin/node", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := claudeExeValid(tt.exe, "linux"); got != tt.linux {
				t.Errorf("claudeExeValid(%q, linux) = %v, want %v", tt.exe, got, tt.linux)
			}
			if got := claudeExeValid(tt.exe, "darwin"); got != tt.mac {
				t.Errorf("claudeExeValid(%q, darwin) = %v, want %v", tt.exe, got, tt.mac)
			}
		})
	}
}

// IsClaude on darwin accepts the macOS native-install layout (~/.local/bin/claude,
// no /claude/ directory) while still rejecting non-claude processes. Asserted via
// the GOOS-parametric core so it runs on the Linux host.
func TestIsClaudeDarwinLayout(t *testing.T) {
	macClaude := osproc.Info{Comm: "claude", Exe: "/Users/u/.local/bin/claude"}
	if !(namedClaude(macClaude) && !isBackgroundSubcommand(macClaude.Args) && claudeExeValid(macClaude.Exe, "darwin")) {
		t.Errorf("darwin native-install claude not accepted: %+v", macClaude)
	}
	// A darwin daemon subcommand at the same path is still not a session.
	macDaemon := osproc.Info{Comm: "claude", Exe: "/Users/u/.local/bin/claude", Args: []string{"/Users/u/.local/bin/claude", "daemon", "run"}}
	if isBackgroundSubcommand(macDaemon.Args) == false {
		t.Errorf("darwin daemon subcommand should be filtered: %+v", macDaemon)
	}
	// A non-claude comm is rejected regardless of exe.
	if IsClaude(osproc.Info{Comm: "node", Exe: "/Users/u/.local/bin/claude"}) {
		t.Errorf("comm=node must not classify as claude")
	}
}

// IsCodex accepts only an argv/exe-consistent Codex binary and then parses the
// invocation's global options to find its mode. A positional initial prompt is
// accepted when it is unambiguously prompt-shaped (a quoted argument containing
// whitespace); an unknown single-token verb fails closed so a newly-added Codex
// utility cannot silently become a Switchboard session.
func TestIsCodex(t *testing.T) {
	tests := []struct {
		name string
		info osproc.Info
		want bool
	}{
		{
			"bare installed binary",
			osproc.Info{Comm: "codex", Exe: "/usr/local/bin/codex", Args: []string{"/usr/local/bin/codex"}},
			true,
		},
		{
			"configured TUI with value and repeatable feature options",
			osproc.Info{Comm: "codex", Exe: "/usr/local/bin/codex", Args: []string{
				"/usr/local/bin/codex", "--config", `model_reasoning_effort="high"`,
				"--enable", "shell_tool", "--enable=unified_exec", "--disable", "web_search",
				"--model", "gpt-5-codex", "--profile", "work", "--sandbox", "workspace-write",
				"--ask-for-approval", "on-request", "--cd", "/work/repo", "--add-dir", "/work/shared",
				"--search", "--no-alt-screen",
			}},
			true,
		},
		{
			"short global options before resume",
			osproc.Info{Comm: "codex", Exe: "/opt/codex/bin/codex", Args: []string{
				"/opt/codex/bin/codex", "-c", "model=codex", "-m", "gpt-5-codex", "-p", "default",
				"-s", "read-only", "-a", "never", "-C", "/work/repo", "resume", "thread-id",
			}},
			true,
		},
		{
			"global option equals forms before fork",
			osproc.Info{Comm: "codex", Exe: "/opt/codex/bin/codex", Args: []string{
				"codex", "--config=model=gpt-5", "--model=gpt-5-codex", "--profile=default",
				"--sandbox=read-only", "--ask-for-approval=never", "--cd=/work/repo", "fork", "thread-id",
			}},
			true,
		},
		{
			"image and local provider options before TUI",
			osproc.Info{Comm: "codex", Exe: "/opt/codex/bin/codex", Args: []string{
				"codex", "--image", "/tmp/one.png", "-i", "/tmp/two.png", "--oss", "--local-provider", "ollama",
			}},
			true,
		},
		{
			"full auto convenience flag before TUI",
			osproc.Info{Comm: "codex", Exe: "/opt/codex/bin/codex", Args: []string{"codex", "--full-auto"}},
			true,
		},
		{
			"explicit bypass flag before TUI",
			osproc.Info{Comm: "codex", Exe: "/opt/codex/bin/codex", Args: []string{"codex", "--dangerously-bypass-approvals-and-sandbox"}},
			true,
		},
		{
			"positional quoted initial prompt",
			osproc.Info{Comm: "codex", Exe: "/usr/local/bin/codex", Args: []string{"codex", "fix the build"}},
			true,
		},
		{
			"option terminator with a one-token prompt",
			osproc.Info{Comm: "codex", Exe: "/usr/local/bin/codex", Args: []string{"codex", "--", "exec"}},
			true,
		},
		{
			"option terminator without prompt is bare TUI",
			osproc.Info{Comm: "codex", Exe: "/usr/local/bin/codex", Args: []string{"codex", "--"}},
			true,
		},
		{
			"masked exe with exact argv zero",
			osproc.Info{Comm: "codex", Args: []string{"/usr/local/bin/codex", "resume"}},
			true,
		},
		{
			// comm is no longer consulted: macOS derives it from the resolved
			// symlink target, so a versioned install reports a version string.
			// Identity rests on argv[0] plus exe, which both still say codex.
			// The protection comm used to appear to give (rejecting a mismatched
			// binary) is really the exe check — see the sandbox wrapper case.
			"should discover session when comm is a version string",
			osproc.Info{Comm: "0.42.0", Exe: "/usr/local/bin/codex", Args: []string{"/usr/local/bin/codex"}},
			true,
		},
		{
			"sandbox wrapper regression with codex comm",
			osproc.Info{Comm: "codex", Exe: "/usr/local/libexec/codex-linux-sandbox", Args: []string{
				"/usr/local/libexec/codex-linux-sandbox", "--sandbox-policy-cwd", "/work/repo",
			}},
			false,
		},
		{
			"sandbox wrapper in argv with masked exe",
			osproc.Info{Comm: "codex", Args: []string{"codex-linux-sandbox", "--sandbox-policy-cwd", "/work/repo"}},
			false,
		},
		{
			"codex argv with sandbox executable",
			osproc.Info{Comm: "codex", Exe: "/usr/lib/codex/codex-linux-sandbox", Args: []string{"codex"}},
			false,
		},
		{
			"different argv zero with codex executable",
			osproc.Info{Comm: "codex", Exe: "/usr/local/bin/codex", Args: []string{"/tmp/impostor"}},
			false,
		},
		{
			"different executable merely sets comm",
			osproc.Info{Comm: "codex", Exe: "/tmp/codex-impostor", Args: []string{"/tmp/codex-impostor"}},
			false,
		},
		{
			"masked args with real executable fails closed",
			osproc.Info{Comm: "codex", Exe: "/usr/local/bin/codex"},
			false,
		},
		{
			"masked args and exe fail closed",
			osproc.Info{Comm: "codex"},
			false,
		},
		{
			"empty argv zero fails closed",
			osproc.Info{Comm: "codex", Exe: "/usr/local/bin/codex", Args: []string{""}},
			false,
		},
		{
			"unknown command-like token fails closed",
			osproc.Info{Comm: "codex", Exe: "/usr/local/bin/codex", Args: []string{"codex", "future-command"}},
			false,
		},
		{
			"unknown global option fails closed",
			osproc.Info{Comm: "codex", Exe: "/usr/local/bin/codex", Args: []string{"codex", "--future-mode"}},
			false,
		},
		{
			"missing long option value fails closed",
			osproc.Info{Comm: "codex", Exe: "/usr/local/bin/codex", Args: []string{"codex", "--model"}},
			false,
		},
		{
			"missing short option value before flag fails closed",
			osproc.Info{Comm: "codex", Exe: "/usr/local/bin/codex", Args: []string{"codex", "-m", "--search"}},
			false,
		},
		{
			"empty equals option value fails closed",
			osproc.Info{Comm: "codex", Exe: "/usr/local/bin/codex", Args: []string{"codex", "--profile="}},
			false,
		},
		{
			"unobserved attached short value fails closed",
			osproc.Info{Comm: "codex", Exe: "/usr/local/bin/codex", Args: []string{"codex", "-mgpt-5-codex"}},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsCodex(tt.info); got != tt.want {
				t.Errorf("IsCodex(%+v) = %v, want %v", tt.info, got, tt.want)
			}
		})
	}
}

func TestIsCodexRejectsTopLevelCommands(t *testing.T) {
	commands := []string{
		"agents", "exec", "e", "review", "login", "logout", "mcp", "mcp-server",
		"plugin", "app-server", "remote-control", "completion", "update", "doctor",
		"sandbox", "debug", "apply", "queue", "archive", "delete", "migrate-rollouts",
		"unarchive", "cloud", "exec-server", "features", "help",
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			info := osproc.Info{
				Comm: "codex",
				Exe:  "/usr/local/bin/codex",
				Args: []string{"codex", "--model", "gpt-5-codex", command, "subcommand-arg"},
			}
			if IsCodex(info) {
				t.Fatalf("IsCodex(%q after globals) = true, want false", command)
			}
		})
	}
}

func TestIsCodexRejectsAppServerForms(t *testing.T) {
	forms := [][]string{
		{"app-server", "daemon"},
		{"app-server", "proxy"},
		{"app-server", "generate-json-schema", "--out", "/tmp/schema"},
		{"app-server", "listen", "--socket", "/tmp/codex.sock"},
	}
	for _, form := range forms {
		t.Run(form[1], func(t *testing.T) {
			args := append([]string{"codex"}, form...)
			if IsCodex(osproc.Info{Comm: "codex", Exe: "/usr/local/bin/codex", Args: args}) {
				t.Fatalf("IsCodex(%v) = true, want false", args)
			}
		})
	}
}

func TestIsCodexRejectsHelpAndVersion(t *testing.T) {
	for _, flag := range []string{"--help", "-h", "--version", "-V"} {
		t.Run(flag, func(t *testing.T) {
			info := osproc.Info{Comm: "codex", Exe: "/usr/local/bin/codex", Args: []string{"codex", "--profile", "work", flag}}
			if IsCodex(info) {
				t.Fatalf("IsCodex(%q) = true, want false", flag)
			}
		})
	}
	for _, command := range []string{"resume", "fork"} {
		t.Run(command+" subcommand help", func(t *testing.T) {
			info := osproc.Info{Comm: "codex", Exe: "/usr/local/bin/codex", Args: []string{"codex", command, "--help"}}
			if IsCodex(info) {
				t.Fatalf("IsCodex(%q --help) = true, want false", command)
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

// A rejected sandbox wrapper is not inserted into the scanner's seen set. If
// the source later returns a real Codex TUI for that PID (the strongest form of
// the regression; a distinct later PID follows trivially), discovery still
// emits the real session.
func TestScannerRejectedCodexSandboxDoesNotHideLaterTUI(t *testing.T) {
	const pid = 103
	src := &fakeProcSource{
		pids: []int{pid},
		infos: map[int]osproc.Info{pid: {
			PID:  pid,
			Comm: "codex",
			Exe:  "/usr/local/libexec/codex-linux-sandbox",
			Args: []string{"codex-linux-sandbox", "--sandbox-policy-cwd", "/work/repo"},
		}},
	}
	s := newWithSource(src)

	var fired []int
	s.scanOnce(func(info osproc.Info) { fired = append(fired, info.PID) })
	if len(fired) != 0 {
		t.Fatalf("sandbox scan fired %v, want none", fired)
	}

	src.infos[pid] = osproc.Info{
		PID:  pid,
		Comm: "codex",
		Exe:  "/usr/local/bin/codex",
		Args: []string{"/usr/local/bin/codex"},
	}
	s.scanOnce(func(info osproc.Info) { fired = append(fired, info.PID) })
	if len(fired) != 1 || fired[0] != pid {
		t.Fatalf("later real Codex scan fired %v, want [%d]", fired, pid)
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

func TestIsHeadless(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"should mark claude -p as headless", []string{"claude", "-p", "--model", "haiku"}, true},
		{"should mark claude --print as headless", []string{"claude", "--print"}, true},
		{"should mark -p headless anywhere in argv", []string{"claude", "--model", "haiku", "-p"}, true},
		{"should keep a bare claude interactive", []string{"claude"}, false},
		{"should keep claude --resume interactive", []string{"claude", "--resume"}, false},
		{"should keep a positional prompt interactive", []string{"claude", "fix the tests"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := osproc.Info{Comm: "claude", Args: tt.args}
			if got := IsHeadless(info); got != tt.want {
				t.Fatalf("IsHeadless(%v) = %t, want %t", tt.args, got, tt.want)
			}
		})
	}
}
