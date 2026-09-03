// Package discovery scans the OS process source (osproc.Source — /proc on Linux,
// libproc on macOS) for coding-agent sessions (Claude Code and Codex; see
// Classify). We poll once a second rather than subscribing to a kernel process-
// event stream because a process-table scan is cheap (~200-500 entries,
// kernel-side memory) and needs no extra capability. Latency is bounded by the
// tick interval.
package discovery

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"

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
// Code session. Identity comes from namedClaude; background subcommand
// invocations (see backgroundSubcommands) are then rejected — those are
// processes, not sessions — and claudeExeValid is cheap insurance against name
// collisions.
func IsClaude(p osproc.Info) bool {
	if !namedClaude(p) {
		return false
	}
	if isBackgroundSubcommand(p.Args) {
		return false
	}
	return claudeExeValid(p.Exe, runtime.GOOS)
}

// namedClaude reports whether the process was invoked under the name "claude".
//
// comm alone is NOT enough, because comm is a per-platform artifact of how a
// kernel derives a process name rather than a stable fact about the program.
// Linux takes it from the path passed to execve, so a versioned-symlink install
// (~/.local/bin/claude -> ~/.local/share/claude/versions/2.1.259) reports
// "claude". macOS resolves the symlink first and takes the target's basename,
// so the SAME install reports comm "2.1.259" — the version string. Gating on
// comm therefore made every macOS session undiscoverable no matter how correct
// the process backend was.
//
// argv[0] is the portable fallback: it preserves the name the process was
// actually invoked under on both platforms. The exe path is deliberately NOT an
// identity signal here — it is validation (claudeExeValid). Admitting a process
// solely because its executable sits under a /claude/ directory would classify
// sibling helper binaries shipped in the same versioned payload as sessions.
func namedClaude(p osproc.Info) bool {
	if p.Comm == "claude" {
		return true
	}
	return len(p.Args) > 0 && filepath.Base(p.Args[0]) == "claude"
}

// claudeExeValid reports whether exe is a plausible claude binary path for the
// given GOOS. A masked (empty) exe is accepted on both platforms — the comm gate
// already matched and the kernel sometimes hides exe.
//
// Linux keeps the original, tighter rule: the exe must sit under a /claude/
// directory (the dev build at ~/.local/share/claude/claude and the released
// versioned payload both do). This rejects /usr/bin/claude-impostor and a stray
// /usr/local/bin/claude, so Linux precision is unchanged by the macOS broadening.
//
// macOS additionally accepts a /claude basename, because the native installer
// puts the binary at ~/.local/bin/claude — NOT under a /claude/ directory — and
// the Homebrew (/opt/homebrew/bin/claude) and npm (…/claude-code/…/claude)
// launchers also resolve to a …/bin/claude file. A bare "claude" (relative exec)
// is accepted too. The basename rule still rejects claude-impostor, which does
// not end in /claude.
func claudeExeValid(exe, goos string) bool {
	if exe == "" {
		return true // kernel masked the exe (rare); comm already matched
	}
	if strings.Contains(exe, "/claude/") {
		return true
	}
	if goos == "darwin" {
		return exe == "claude" || strings.HasSuffix(exe, "/claude")
	}
	return false
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

// IsHeadless reports whether a discovered claude process is a non-interactive
// print/SDK run: `claude -p …` / `claude --print …`. Such processes are real
// sessions — they write transcripts and registry files (whose entrypoint reads
// "sdk-cli" instead of "cli") — so they stay discovered and visible, but there
// is no TUI window or pane behind them. The daemon tags them Headless so
// renderers can style them inert and navigation (focus/cycle/pick) skips them.
// Interactive sessions never pass -p/--print, so argv is a reliable
// discriminator at discovery time.
func IsHeadless(p osproc.Info) bool {
	if len(p.Args) < 2 {
		return false
	}
	for _, arg := range p.Args[1:] {
		if arg == "-p" || arg == "--print" {
			return true
		}
	}
	return false
}

// codexNonInteractiveSubcommands is the installed Codex CLI's non-TUI command
// surface. It is deliberately paired with a fail-closed default in
// codexIsInteractive: this table documents known commands, but a newly-added
// command-like token is rejected even before the table catches up.
var codexNonInteractiveSubcommands = map[string]struct{}{
	"agents": {}, "exec": {}, "e": {}, "review": {},
	"login": {}, "logout": {}, "mcp": {}, "mcp-server": {}, "plugin": {},
	"app-server": {}, "remote-control": {}, "completion": {}, "update": {},
	"doctor": {}, "sandbox": {}, "debug": {}, "apply": {}, "queue": {},
	"archive": {}, "delete": {}, "migrate-rollouts": {}, "unarchive": {},
	"cloud": {}, "exec-server": {}, "features": {}, "help": {},

	// Older/private surfaces remain non-interactive if encountered.
	"execpolicy": {}, "app": {},
}

// codexGlobalValueOptions are recognized global options that consume exactly
// one following argv element. Feature and directory options may be repeated;
// walking them rather than inspecting Args[1] is what lets a configured TUI be
// distinguished from `codex --model … app-server`, for example.
var codexGlobalValueOptions = map[string]struct{}{
	"-c": {}, "--config": {},
	"--enable": {}, "--disable": {},
	"-i": {}, "--image": {},
	"-m": {}, "--model": {},
	"--local-provider": {},
	"-p":               {}, "--profile": {},
	"-s": {}, "--sandbox": {},
	"-a": {}, "--ask-for-approval": {},
	"-C": {}, "--cd": {},
	"--add-dir": {},
}

// codexGlobalFlags are recognized global switches that do not consume a value.
var codexGlobalFlags = map[string]struct{}{
	"--oss":       {},
	"--full-auto": {},
	"--dangerously-bypass-approvals-and-sandbox": {},
	"--search":        {},
	"--no-alt-screen": {},
}

var codexHelpVersionFlags = map[string]struct{}{
	"--help": {}, "-h": {}, "--version": {}, "-V": {},
}

// IsCodex returns true only when the process snapshot proves both that it is the
// real Codex executable and that the invocation enters an interactive TUI.
//
// Comm is neither sufficient nor necessary, so it is not consulted at all.
// Not sufficient: codex-linux-sandbox has been observed with comm=codex. Not
// necessary: macOS derives comm from the resolved symlink target, so a
// versioned install reports a version string rather than "codex" (see
// namedClaude). Args[0], when readable, and Exe, when readable, must therefore
// both have the exact basename "codex" — which was already the real identity
// test here, with the comm gate contributing nothing but a macOS outage.
// A masked Exe is safe when Args[0] proves identity; masked Args are not
// accepted because no remaining process field can distinguish a bare TUI from
// `codex exec` or another utility invocation.
func IsCodex(p osproc.Info) bool {
	if len(p.Args) == 0 || filepath.Base(p.Args[0]) != "codex" {
		return false
	}
	if p.Exe != "" && filepath.Base(p.Exe) != "codex" {
		return false
	}
	return codexIsInteractive(p.Args)
}

// codexIsInteractive parses the global option prefix of an identity-validated
// `codex …` argv and decides its mode. Bare, resume, fork, and an unambiguous
// positional prompt enter the TUI. Known utilities and unknown command-like
// tokens fail closed.
//
// The process table preserves shell argv boundaries. To avoid guessing that an
// unknown one-word utility is a prompt, an unterminated positional prompt is
// considered unambiguous only when its first argv element contains whitespace
// (for example, `codex "fix the build"`). `--` explicitly chooses positional
// interpretation, so any token following it is a prompt; `codex --` is the bare
// TUI. This conservative rule cannot admit codex-linux-sandbox because identity
// validation happens first.
func codexIsInteractive(args []string) bool {
	for i := 1; i < len(args); {
		arg := args[i]
		if arg == "--" {
			return true
		}
		if _, exits := codexHelpVersionFlags[arg]; exits {
			return false
		}
		if _, flag := codexGlobalFlags[arg]; flag {
			i++
			continue
		}
		if _, takesValue := codexGlobalValueOptions[arg]; takesValue {
			if i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
				return false
			}
			i += 2
			continue
		}
		if strings.HasPrefix(arg, "--") {
			name, value, hasEquals := strings.Cut(arg, "=")
			if _, takesValue := codexGlobalValueOptions[name]; !hasEquals || !takesValue || value == "" {
				return false
			}
			i++
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return false
		}

		switch arg {
		case "resume", "fork":
			return codexInteractiveSubcommand(args[i:])
		default:
			if _, nonInteractive := codexNonInteractiveSubcommands[arg]; nonInteractive {
				return false
			}
			return strings.IndexFunc(arg, unicode.IsSpace) >= 0
		}
	}
	return true
}

// codexInteractiveSubcommand rejects help/version exits on the two interactive
// command surfaces. `--` ends their option parsing and makes later tokens
// positional (for example, a session selector literally named "--help").
func codexInteractiveSubcommand(args []string) bool {
	for _, arg := range args[1:] {
		if arg == "--" {
			break
		}
		if _, exits := codexHelpVersionFlags[arg]; exits {
			return false
		}
	}
	return true
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
