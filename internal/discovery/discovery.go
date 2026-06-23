// Package discovery scans /proc for coding-agent sessions (Claude Code and
// Codex; see Classify). We poll once a second
// rather than subscribing to netlink CN_PROC because /proc scans are cheap
// (~200-500 procfs entries, kernel-side memory) and avoid needing
// CAP_NET_ADMIN. Latency is bounded by the tick interval.
package discovery

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/tjmisko/switchboard/internal/proc"
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

// IsClaude returns true if the given /proc snapshot is an interactive Claude
// Code session. We match on comm == "claude" AND exe under
// ~/.local/share/claude/ (handles both the released binary and dev builds
// installed elsewhere; the exe check is cheap insurance against name
// collisions), AND reject background subcommand invocations (see
// backgroundSubcommands) — those are processes, not sessions.
func IsClaude(p proc.Info) bool {
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

// Agent identifies a supported coding-agent CLI discovered in /proc. AgentNone
// (the empty value) means "not a tracked interactive session".
type Agent string

const (
	AgentNone   Agent = ""
	AgentClaude Agent = "claude"
	AgentCodex  Agent = "codex"
)

// Classify reports which interactive coding-agent session a /proc snapshot is,
// or AgentNone when it is neither Claude Code nor Codex. It is the single
// predicate the scanner filters on, so adding an agent is a matter of extending
// this switch. The returned value's string matches state.AgentKind*.
func Classify(p proc.Info) Agent {
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

// IsCodex returns true if the /proc snapshot is an interactive Codex CLI
// session: comm == "codex" with an argv[1] that is not a non-interactive
// subcommand (see codexNonInteractiveSubcommands). No exe-path check — codex is
// not installed under a distinctive directory the way claude is.
func IsCodex(p proc.Info) bool {
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

// procSource is the seam between the scanner and /proc. The default
// implementation calls the proc package directly; tests inject a fake so the
// seen-set state machine can be exercised without a live /proc.
type procSource interface {
	AllPIDs() ([]int, error)
	Read(pid int) (proc.Info, error)
}

type realProcSource struct{}

func (realProcSource) AllPIDs() ([]int, error)         { return proc.AllPIDs() }
func (realProcSource) Read(pid int) (proc.Info, error) { return proc.Read(pid) }

type Scanner struct {
	mu   sync.Mutex
	seen map[int]struct{}
	src  procSource
}

func New() *Scanner {
	return &Scanner{seen: make(map[int]struct{}), src: realProcSource{}}
}

// newWithSource builds a Scanner over an injected procSource. Test-only seam;
// runtime callers use New, which wires the real /proc-backed source.
func newWithSource(src procSource) *Scanner {
	return &Scanner{seen: make(map[int]struct{}), src: src}
}

// Run polls /proc every interval and invokes onAppeared for any new claude
// PID. Returns when ctx is cancelled. Death is *not* reported here — that's
// the procwatch package's job, fed by pidfds.
func (s *Scanner) Run(ctx context.Context, interval time.Duration, onAppeared func(proc.Info)) error {
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

func (s *Scanner) scanOnce(onAppeared func(proc.Info)) {
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
