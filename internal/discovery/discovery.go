// Package discovery scans the OS process source (osproc.Source — /proc on Linux,
// libproc on macOS) for coding-agent sessions (Claude Code and Codex; see
// Classify). We poll once a second rather than subscribing to a kernel process-
// event stream because a process-table scan is cheap (~200-500 entries,
// kernel-side memory) and needs no extra capability. Latency is bounded by the
// tick interval.
package discovery

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/tjmisko/switchboard/internal/osproc"
)

// backgroundSubcommands are `claude <verb> …` invocations that are NOT
// interactive TUI sessions. The load-bearing one is `daemon`: Claude Code spawns
// a detached `claude daemon run …` process, reparents it to init, and lets it
// outlive the session that started it. It shares the claude binary — same comm,
// same exe under /claude/ — and has no controlling tty, so without this filter
// it surfaces as an un-navigable "zombie" chip that lingers after its session
// dies. `mcp` (the MCP server/management verb) is excluded for the same reason.
// Interactive sessions never carry a verb here: they start with a flag
// (--resume), a positional prompt, or nothing.
var backgroundSubcommands = map[string]struct{}{
	"daemon": {},
	"mcp":    {},
}

// IsClaude returns true if the given process snapshot is an interactive Claude
// Code session. We match on comm == "claude" AND exe under
// ~/.local/share/claude/ (handles both the released binary and dev builds
// installed elsewhere; the exe check is cheap insurance against name
// collisions), AND reject background subcommand invocations (see
// backgroundSubcommands) — those are processes, not sessions.
func IsClaude(p osproc.Info) bool {
	if p.Comm != "claude" {
		return false
	}
	if isBackgroundSubcommand(p.Args) {
		return false
	}
	if p.Exe == "" {
		return true // benefit of the doubt — kernel masked exe (rare)
	}
	return strings.Contains(p.Exe, "/claude/")
}

// isBackgroundSubcommand reports whether argv is a `claude <verb> …` invocation
// of a non-interactive subcommand. args[0] is the program path; args[1] is the
// subcommand verb when present.
func isBackgroundSubcommand(args []string) bool {
	if len(args) < 2 {
		return false
	}
	_, ok := backgroundSubcommands[args[1]]
	return ok
}

// Agent identifies a supported coding-agent CLI discovered in the process table.
// AgentNone (the empty value) means "not a tracked interactive session".
type Agent string

const (
	AgentNone   Agent = ""
	AgentClaude Agent = "claude"
	AgentCodex  Agent = "codex"
)

// Classify reports which interactive coding-agent session a process snapshot is,
// or AgentNone when it is neither Claude Code nor Codex. It is the single
// predicate the scanner filters on, so adding an agent is a matter of extending
// this switch. The returned value's string matches state.AgentKind*.
func Classify(p osproc.Info) Agent {
	switch {
	case IsClaude(p):
		return AgentClaude
	case IsCodex(p):
		return AgentCodex
	default:
		return AgentNone
	}
}

// codexNonInteractiveSubcommands are `codex <verb> …` invocations that are NOT
// interactive TUI sessions. Codex is a single `codex` binary whose subcommand is
// argv[1]; unlike claude, most of its surface is non-session — a headless
// `exec`, the `app-server`/`mcp-server`/`mcp` servers, and `login`/`doctor`/
// `update`/… utilities — so we blocklist those and treat everything else (the
// bare `codex`, a leading flag, a positional prompt, `resume`, `fork`) as a
// session. Mirrors claude's verb filter; a future non-interactive verb must be
// added here.
var codexNonInteractiveSubcommands = map[string]struct{}{
	"exec": {}, "e": {},
	"app-server": {}, "mcp-server": {}, "mcp": {}, "remote-control": {}, "sandbox": {},
	"login": {}, "logout": {}, "doctor": {}, "completion": {}, "update": {},
	"plugin": {}, "features": {}, "cloud": {}, "apply": {},
	"archive": {}, "unarchive": {}, "delete": {}, "execpolicy": {}, "app": {},
}

// IsCodex returns true if the process snapshot is an interactive Codex CLI
// session: comm == "codex" with an argv[1] that is not a non-interactive
// subcommand (see codexNonInteractiveSubcommands). No exe-path check — codex is
// not installed under a distinctive directory the way claude is.
func IsCodex(p osproc.Info) bool {
	if p.Comm != "codex" {
		return false
	}
	return codexIsInteractive(p.Args)
}

// codexIsInteractive reports whether a `codex …` argv launches an interactive
// session. args[0] is the program path; args[1] is the subcommand verb when
// present. A bare invocation, a leading flag, or a non-blocklisted verb counts.
func codexIsInteractive(args []string) bool {
	if len(args) < 2 {
		return true // bare `codex` → TUI
	}
	verb := args[1]
	if strings.HasPrefix(verb, "-") {
		return true // `codex --model … ` etc. → still the TUI
	}
	_, nonInteractive := codexNonInteractiveSubcommands[verb]
	return !nonInteractive
}

// procSource is the narrow seam between the scanner and the OS process layer:
// list pids cheaply, then Read only the unseen ones. The runtime implementation
// adapts an osproc.Source (osprocSource); tests inject a fake so the seen-set
// state machine can be exercised without a live process table.
type procSource interface {
	AllPIDs() ([]int, error)
	Read(pid int) (osproc.Info, error)
}

// pidLister is the optional fast-path an osproc.Source may provide to list pids
// cheaply (no per-pid exe/cwd/tty reads). The Linux source implements it; a
// Source that does not is driven from Enumerate. It is deliberately NOT part of
// the neutral osproc.Source contract — discovery upgrades to it when present and
// degrades gracefully when absent, so a new backend drops in either way.
type pidLister interface {
	AllPIDs() ([]int, error)
}

// osprocSource adapts an osproc.Source to the scanner's narrow procSource seam.
// AllPIDs uses the cheap pidLister fast-path when the underlying Source provides
// it, and otherwise derives the pid list from a full Enumerate — preserving the
// "enumerate cheaply, Read only the unseen" hot path on Linux while keeping
// discovery functional over any Source.
type osprocSource struct{ src osproc.Source }

func (a osprocSource) AllPIDs() ([]int, error) {
	if l, ok := a.src.(pidLister); ok {
		return l.AllPIDs()
	}
	infos, err := a.src.Enumerate()
	if err != nil {
		return nil, err
	}
	pids := make([]int, len(infos))
	for i := range infos {
		pids[i] = infos[i].PID
	}
	return pids, nil
}

func (a osprocSource) Read(pid int) (osproc.Info, error) { return a.src.Read(pid) }

type Scanner struct {
	mu   sync.Mutex
	seen map[int]struct{}
	src  procSource
}

// New builds a Scanner over the given OS process source. The darwin backend
// drops in here unchanged — discovery only ever touches osproc.Source.
func New(src osproc.Source) *Scanner {
	return &Scanner{seen: make(map[int]struct{}), src: osprocSource{src: src}}
}

// newWithSource builds a Scanner over an injected procSource. Test-only seam;
// runtime callers use New, which wires the osproc-backed adapter.
func newWithSource(src procSource) *Scanner {
	return &Scanner{seen: make(map[int]struct{}), src: src}
}

// Run polls the process source every interval and invokes onAppeared for any
// new agent PID. Returns when ctx is cancelled. Death is *not* reported here —
// that is the osproc.Source watcher's job, fed by pidfds (Linux) / kqueue (macOS).
func (s *Scanner) Run(ctx context.Context, interval time.Duration, onAppeared func(osproc.Info)) error {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	s.scanOnce(onAppeared)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			s.scanOnce(onAppeared)
		}
	}
}

// Forget drops a PID from the seen set so the next scan can re-fire if the
// kernel ever recycled the same PID for a fresh claude process. Call this
// from procwatch's death callback.
func (s *Scanner) Forget(pid int) {
	s.mu.Lock()
	delete(s.seen, pid)
	s.mu.Unlock()
}

func (s *Scanner) scanOnce(onAppeared func(osproc.Info)) {
	pids, err := s.src.AllPIDs()
	if err != nil {
		return
	}
	for _, pid := range pids {
		s.mu.Lock()
		_, known := s.seen[pid]
		s.mu.Unlock()
		if known {
			continue
		}
		info, err := s.src.Read(pid)
		if err != nil {
			continue
		}
		if Classify(info) == AgentNone {
			continue
		}
		s.mu.Lock()
		s.seen[pid] = struct{}{}
		s.mu.Unlock()
		onAppeared(info)
	}
}
