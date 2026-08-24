// Package rpc exposes the daemon over a Unix socket. Protocol is one JSON
// request per line, with JSON responses streamed back. Commands:
//
//	{"cmd":"list"}                              -> {"snapshot":{...}}
//	{"cmd":"focus","selector":"active"|"<pid>"|"<index>"|"pid:<n>"|"idx:<n>"} -> {"ok":true}
//	{"cmd":"subscribe"}                          -> stream of {"snapshot":{...}}
package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tjmisko/switchboard/internal/discovery"
	"github.com/tjmisko/switchboard/internal/fanout"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/osproc"
	"github.com/tjmisko/switchboard/internal/proc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statustune"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/transcript"
	"github.com/tjmisko/switchboard/internal/wm"
)

// ErrNavigateUnsupported is returned by focus when the detected stack lacks a
// terminal locator or a WM focus backend — Navigate degrades to Observe, so
// there is nowhere to jump. Distinct from a transient "address not resolved
// yet" so the client can present an actionable message.
var ErrNavigateUnsupported = errors.New("navigate unsupported on this stack (Observe-only)")

// ErrHeadlessSession is returned by focus when the selected session is a
// non-interactive `claude -p` run — it is tracked and visible, but there is no
// window or pane to land on.
var ErrHeadlessSession = errors.New("headless session (claude -p) has no window to focus")

type Request struct {
	Cmd      string `json:"cmd"`
	Selector string `json:"selector,omitempty"`

	// hook fields — set when Cmd == "hook"
	Event      string    `json:"event,omitempty"`
	PID        int       `json:"pid,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	ObservedAt time.Time `json:"observed_at,omitzero"`
	// Prompt is an ephemeral, bounded display-name input. It is never stored or
	// logged by the daemon.
	Prompt string `json:"prompt,omitempty"`
	// LastAssistantMessage is the matching bounded Stop-hook naming input. Like
	// Prompt, it is ephemeral and must never enter state, history, diagnostics,
	// or logs.
	LastAssistantMessage string `json:"last_assistant_message,omitempty"`
	Transcript           string `json:"transcript,omitempty"`
	// HookSource is Codex SessionStart.source (startup, resume, clear, compact).
	// It is lifecycle metadata, not provider content, and lets the daemon avoid
	// treating an immediate post-/clear continuation as a real idle interval.
	HookSource string `json:"hook_source,omitempty"`
	// TurnID and ToolUseID are opaque Codex hook correlation IDs. In particular,
	// tool_use_id joins request_user_input's PreToolUse and PostToolUse edges so
	// an unrelated tool completion cannot clear a waiting-for-user state.
	TurnID    string `json:"turn_id,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	// PermissionMode records the content-free Codex turn mode (default, plan,
	// and so on) at lifecycle boundaries.
	PermissionMode string `json:"permission_mode,omitempty"`
	// Agent names which coding agent fired the hook: "claude" (default when
	// empty) or "codex". It routes the enrichment to the right block and selects
	// the event→status mapping.
	Agent string `json:"agent,omitempty"`
	// ToolName is the hook's tool_name when the event carries one (PreToolUse,
	// PermissionRequest, PostToolUse). It is stashed at red-onset (state.PendingPrompt.Tool, under the
	// writer that raised the prompt) and matched on a later
	// PostToolUse to clear red at hook speed when the approved tool completes —
	// while a non-matching/Task PostToolUse keeps the chip red. Empty for events
	// with no tool (UserPromptSubmit/Stop/SessionStart), which just disables the
	// fast path and falls back to the transcript check.
	ToolName string `json:"tool_name,omitempty"`
	// ToolInputHash is a short, canonicalized sha256 prefix of the hook's
	// tool_input, computed at the ctl edge (see cmdHook's hashToolInput) — the raw
	// input is never forwarded or persisted, since it can be large and can carry
	// sensitive content. tool_input rides both PermissionRequest and PostToolUse,
	// so this is the "which call" correlator that sits between AgentID ("which
	// writer") and ToolName ("which kind" — Bash collides constantly). Empty means
	// NO SIGNAL, not "a hash that failed to match": events with no tool_input, and
	// inputs that are absent/unparseable/null/empty, all send "". Consumers must
	// never treat two empty hashes as a match.
	ToolInputHash string `json:"tool_input_hash,omitempty"`

	// AgentID/AgentType identify the subagent a hook fired from. Claude Code puts
	// agent_id on EVERY hook event (docs/claude-code-hook-schema.md §1), not just
	// SubagentStart/Stop, and populates it ONLY inside a subagent — so empty AgentID
	// means the MAIN THREAD (true even in --agent sessions, where agent_type is set
	// but agent_id is not). Empty is therefore a load-bearing discriminator, never
	// merely "unknown". AgentType names the kind.
	//
	// ⚠ The daemon keys on the BARE id: handleHook runs the incoming value through
	// normalizeAgentID exactly once, stripping one leading "agent-" if the hook
	// happens to send the on-disk file spelling (which shape it sends is still
	// unobserved — plan T1). That bare form is what transcript.Subagent.AgentID and
	// history.Event.AgentID already hold, both derived from the agent-<id>.* file
	// names, so hook-keyed and scan-keyed maps join whichever spelling arrives. Do
	// NOT strip again at a use site: a second pass would eat an "agent-" that is
	// genuinely part of the id.
	//
	// On SubagentStart/Stop both fields are best-effort context — the hook's job
	// there is only to TRIGGER a fanout re-scan, which is keyed off the dir, so
	// neither is required for correctness.
	AgentID   string `json:"agent_id,omitempty"`
	AgentType string `json:"agent_type,omitempty"`

	// Activity carries the global user-activity edge — set when Cmd == "activity".
	// It is session-less: "idle" when an idle daemon (e.g. hypridle) sees no input
	// for its timeout, "active" when input resumes. Exactly those two values are
	// valid; handleActivity rejects anything else.
	Activity string `json:"activity,omitempty"`
}

type Response struct {
	Snapshot    *state.Snapshot   `json:"snapshot,omitempty"`
	Diagnostics []AgentDiagnostic `json:"diagnostics,omitempty"`
	OK          bool              `json:"ok,omitempty"`
	Error       string            `json:"error,omitempty"`
}

// rawResponse mirrors Response field-for-field — same names, same order, same
// omitempty — except the snapshot arrives already encoded. It is the envelope for
// the subscribe stream, where state.Store has marshaled the snapshot once for all
// subscribers (see snapshotFrame).
//
// ⚠ The two structs must be edited together: their encodings are required to be
// byte-identical, which TestSubscribeFrameMatchesResponseEncoding pins.
type rawResponse struct {
	Snapshot    json.RawMessage   `json:"snapshot,omitempty"`
	Diagnostics []AgentDiagnostic `json:"diagnostics,omitempty"`
	OK          bool              `json:"ok,omitempty"`
	Error       string            `json:"error,omitempty"`
}

// AgentDiagnostic is a bounded, content-free health counter for provider
// observation. Categories are finite implementation labels; messages, paths,
// prompts, commands, and raw provider payloads never cross this RPC surface.
type AgentDiagnostic struct {
	Provider string    `json:"provider"`
	Category string    `json:"category"`
	Count    uint64    `json:"count"`
	LastAt   time.Time `json:"last_at"`
}

// AgentHookHandler receives an already-attributed hook and a detached snapshot
// of its root session. The handler owns provider-specific interpretation. It is
// invoked without the state-store lock held, including while walking /proc to
// attribute wrapper processes.
type AgentHookHandler func(Request, state.Session)

// AgentDiagnosticSource supplies the content-free provider health snapshot.
type AgentDiagnosticSource func() []AgentDiagnostic

type Server struct {
	store       *state.Store
	socketPath  string
	term        terminal.Locator
	wm          wm.Manager
	tun         statustune.Tuning
	hist        *history.Sink
	fanout      *fanout.Observer
	agentHook   AgentHookHandler
	diagnostics AgentDiagnosticSource
	// readProc is the seam findTrackedAncestor walks the ppid chain through.
	// Production is proc.Read; tests substitute a synthetic chain so hook
	// attribution can be exercised against process shapes (a nested `claude -p`,
	// a shell wrapper) that cannot be staged against a live /proc.
	readProc func(int) (proc.Info, error)
}

func New(store *state.Store, socketPath string, term terminal.Locator, manager wm.Manager) *Server {
	return &Server{store: store, socketPath: socketPath, term: term, wm: manager,
		tun: statustune.Default(), readProc: proc.Read}
}

// SetTuning overrides the status-color tuning (defaults from statustune.Default).
// Call once at startup before Serve; the hook handler reads it without a lock,
// which is safe because it is not mutated after startup.
func (s *Server) SetTuning(t statustune.Tuning) { s.tun = t }

// SetHistory wires the activity-log sink the hook handler records transitions to.
// Call once at startup before Serve. A nil sink (the default) records nothing.
func (s *Server) SetHistory(h *history.Sink) { s.hist = h }

// SetFanout wires the subagent fanout Observer that a SubagentStart/Stop hook
// triggers an immediate re-scan on (single source of truth, shared with the
// reconcile loop). Call once at startup before Serve. A nil Observer (the default)
// disables the hook-speed trigger, leaving the reconcile tick to pick fanouts up.
func (s *Server) SetFanout(o *fanout.Observer) { s.fanout = o }

// SetAgentHookHandler installs the graph-aware provider hook path. When set,
// hook attribution and the callback run outside Store.Apply; legacy hook logic
// remains the default for compatibility tests and embedders that do not opt in.
func (s *Server) SetAgentHookHandler(handler AgentHookHandler) { s.agentHook = handler }

// SetAgentDiagnosticSource installs the additive, content-free diagnostics RPC.
func (s *Server) SetAgentDiagnosticSource(source AgentDiagnosticSource) { s.diagnostics = source }

// Serve listens on the socket path and accepts connections until ctx is done.
// The socket file is removed on startup (in case of unclean shutdown) and on
// exit.
func (s *Server) Serve(ctx context.Context) error {
	_ = os.Remove(s.socketPath)
	l, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.socketPath, err)
	}
	defer os.Remove(s.socketPath)

	go func() {
		<-ctx.Done()
		l.Close()
	}()

	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handle(ctx, conn)
	}
}

// ServeConnection serves the ordinary JSONL RPC protocol over one already-open
// connection and owns closing it. The daemon uses Serve's Unix listener;
// in-memory transports use this seam to exercise the identical request and
// subscription paths without creating a filesystem socket.
func (s *Server) ServeConnection(ctx context.Context, conn net.Conn) {
	s.handle(ctx, conn)
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for {
		var req Request
		if err := dec.Decode(&req); err != nil {
			if err != io.EOF {
				_ = enc.Encode(Response{Error: err.Error()})
			}
			return
		}
		switch req.Cmd {
		case "list":
			snap := s.store.Snapshot()
			_ = enc.Encode(Response{Snapshot: &snap})
		case "focus":
			err := s.focus(ctx, req.Selector)
			if err != nil {
				_ = enc.Encode(Response{Error: err.Error()})
			} else {
				_ = enc.Encode(Response{OK: true})
			}
		case "subscribe":
			s.subscribe(ctx, conn, enc)
			return
		case "hook":
			s.handleHook(req)
			_ = enc.Encode(Response{OK: true})
		case "agent-diagnostics":
			var diagnostics []AgentDiagnostic
			if s.diagnostics != nil {
				diagnostics = s.diagnostics()
			}
			_ = enc.Encode(Response{OK: true, Diagnostics: diagnostics})
		case "activity":
			err := s.handleActivity(req)
			if err != nil {
				_ = enc.Encode(Response{Error: err.Error()})
			} else {
				_ = enc.Encode(Response{OK: true})
			}
		default:
			_ = enc.Encode(Response{Error: "unknown cmd: " + req.Cmd})
		}
	}
}

func (s *Server) subscribe(ctx context.Context, conn net.Conn, enc *json.Encoder) {
	ch, cancel := s.store.Subscribe()
	defer cancel()
	snap := s.store.Snapshot()
	if err := enc.Encode(Response{Snapshot: &snap}); err != nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case b, ok := <-ch:
			if !ok {
				return
			}
			if err := enc.Encode(snapshotFrame(b)); err != nil {
				return
			}
		}
	}
}

// snapshotFrame wraps a broadcast in the response envelope WITHOUT re-encoding
// the snapshot. This connection is one of many — the live bar declares ten waybar
// slot modules, each a separate process holding its own subscription — and every
// one of them was serializing the identical snapshot on its own goroutine after
// every mutation. state.Store.broadcast now encodes it once and shares the bytes;
// json.RawMessage carries them through the envelope verbatim.
//
// Byte-identity with enc.Encode(Response{Snapshot:&snap}) holds because
// rawResponse mirrors Response exactly and encoding/json emits a RawMessage
// through compact(), which is a no-op on already-compact bytes and whose HTML
// escaping is idempotent here: state.Broadcast.JSON was produced with escaping
// on, so no '<', '>' or '&' survives to be escaped a second time.
//
// A nil JSON means the encode failed upstream; fall back to encoding the snapshot
// so this subscriber gets a valid frame rather than a truncated one.
func snapshotFrame(b state.Broadcast) any {
	if b.JSON == nil {
		return Response{Snapshot: &b.Snapshot}
	}
	return rawResponse{Snapshot: b.JSON}
}

func (s *Server) focus(ctx context.Context, selector string) error {
	// Navigate needs at least one actuator. With neither a WM nor a terminal
	// backend the stack is Observe-only: fail with the typed error rather than
	// the confusing "session has no hyprland address yet" (decisions.md #3).
	if s.wm.Name() == "none" && s.term.Name() == "none" {
		return ErrNavigateUnsupported
	}
	snap := s.store.Snapshot()
	if len(snap.Sessions) == 0 {
		return fmt.Errorf("no sessions")
	}
	target := pickSession(snap.Sessions, selector)
	if target == nil {
		return fmt.Errorf("no session matches %q", selector)
	}
	if target.Headless {
		return ErrHeadlessSession
	}

	// Best-effort, backend-agnostic: raise the WM window if we have its ref, and
	// focus the terminal pane by re-locating it from the (always-present) tty —
	// so this works for wezterm and tmux without persisting backend-specific
	// pane fields. At least one step must act, else there's nothing to focus.
	acted := false
	if target.Hyprland != nil && target.Hyprland.Address != "" {
		if err := s.wm.Focus(ctx, target.Hyprland.Address); err != nil {
			return fmt.Errorf("wm focus: %w", err)
		}
		acted = true
	}
	if target.TTY != "" {
		if pane, err := s.term.Locate(ctx, target.TTY); err == nil && pane != nil {
			if err := s.term.Activate(ctx, pane); err != nil {
				return fmt.Errorf("terminal activate: %w", err)
			}
			acted = true
		}
	}
	if !acted {
		return fmt.Errorf("session %d has no window or pane to focus yet", target.PID)
	}
	return nil
}

// pickSession resolves a focus selector against the session slice:
//
//	"" / "active"  -> the focused session, else the first
//	"pid:<n>"      -> the session with PID n (explicit; nil if none)
//	"idx:<n>"      -> the session at index n (explicit; nil if out of range)
//	"<n>"          -> back-compat heuristic: PID n if present, else index n
//
// The bare-number form is the Phase-0 ⚠ PID-vs-index collision (decisions.md
// #3): selector "2" means PID 2 when such a session exists, else index 2. It is
// kept for back-compat; the pid:/idx: prefixes are the unambiguous forms.
func pickSession(sessions []state.Session, selector string) *state.Session {
	switch selector {
	case "", "active":
		for i := range sessions {
			if sessions[i].Focused {
				return &sessions[i]
			}
		}
		return &sessions[0]
	}
	if rest, ok := strings.CutPrefix(selector, "pid:"); ok {
		if n, err := strconv.Atoi(rest); err == nil {
			return byPID(sessions, n)
		}
		return nil
	}
	if rest, ok := strings.CutPrefix(selector, "idx:"); ok {
		if n, err := strconv.Atoi(rest); err == nil {
			return byIndex(sessions, n)
		}
		return nil
	}
	if n, err := strconv.Atoi(selector); err == nil {
		if s := byPID(sessions, n); s != nil {
			return s
		}
		return byIndex(sessions, n)
	}
	return nil
}

func byPID(sessions []state.Session, pid int) *state.Session {
	for i := range sessions {
		if sessions[i].PID == pid {
			return &sessions[i]
		}
	}
	return nil
}

func byIndex(sessions []state.Session, idx int) *state.Session {
	if idx >= 0 && idx < len(sessions) {
		return &sessions[idx]
	}
	return nil
}

// handleHook updates the Claude.Status of the session whose PID is the hook
// caller — or, if the hook ran inside a shell wrapper, whose PID is the
// nearest claude ancestor. All updates are best-effort enrichment: a missing
// session or an unrecognized event is silently ignored, so a misconfigured
// hook can never corrupt state.
func (s *Server) handleHook(req Request) {
	if s.agentHook != nil {
		s.dispatchAgentHook(req)
		return
	}
	// THE choke point for hook identity: every consumer below (and every future
	// one — the T5 Pending map, the T7 clear rule, the T8/T9 subagent transcript
	// routing) sees the canonical bare id, so none of them can key a map on a
	// spelling the Observer never writes. req is a value copy, so this rewrite is
	// local to the call. Nothing downstream may strip again — see normalizeAgentID.
	req.AgentID = normalizeAgentID(req.AgentID)

	agent := req.Agent
	if agent == "" {
		agent = state.AgentKindClaude
	}
	status := statusFromHookEvent(agent, req.Event)
	if status == "" && req.SessionID == "" && req.Transcript == "" {
		return
	}
	s.store.Apply(func(m map[int]*state.Session) {
		pid := findTrackedAncestor(m, req.PID, s.readProc)
		if pid == 0 {
			return
		}
		sess := m[pid]
		info := sess.AgentBlock(agent)
		// Transcript path is stable per session; refresh it BEFORE the hold gate so
		// its transcript fallback reads the current tail (and so the reconciler can
		// later tell a declined prompt from a still-pending one).
		if req.Transcript != "" {
			info.Transcript = req.Transcript
		}

		// T1 instrumentation (docs/subagent-permission-plan.md): the one empirical
		// gap in the hook-schema work is whether agent_id is actually non-empty
		// end-to-end for a subagent-raised prompt. Log it where it decides
		// something and nowhere else, so the journal stays readable: every
		// PermissionRequest (rare), and a PostToolUse only while the chip is
		// already red (the window in which the id would gate the clear).
		// Deliberately NOT the `status: pid=` shape — switchboard-ctl diagnose
		// parses that format via statustune.ParseDecision.
		if agent == state.AgentKindClaude &&
			(req.Event == "PermissionRequest" || (req.Event == "PostToolUse" && info.Status == state.StatusPermission)) {
			log.Printf("hook-identity: pid=%d %s event=%s agent_id=%q agent_type=%q tool=%q chip=%s pending=%q S=%d",
				pid, sessionLabel(sess, req.SessionID), req.Event, req.AgentID, req.AgentType,
				req.ToolName, info.Status, info.PendingSummary(), info.InFlightSubagents)
		}

		// Session rotation (plan T5, absorbing T15). A /clear or a fork keeps the
		// pid but takes a NEW session_id and a new transcript file, and every prompt
		// recorded under the retired session dies with it: nothing in the new session
		// will ever resolve one, because the writer that raised it no longer exists.
		//
		// Detected here, before the hold gate, because T3 broadened the hold to cover
		// SessionStart — which is exactly the event a rotation announces itself with.
		// Without this clause the retired session's red is held against the new
		// session's own transcript, which of course shows nothing resolving it, and
		// the chip latches red into a session that never had a prompt.
		//
		// The empty guards matter in both directions: an empty req.SessionID is a
		// hook that simply did not carry one (most of them), and an empty
		// info.SessionID is a first assignment, not a rotation.
		rotated := req.SessionID != "" && info.SessionID != "" && req.SessionID != info.SessionID
		if rotated {
			info.ClearPending()
		}

		// T17 — defect 2 (docs/subagent-permission-oscillation.md §2.4/§3.2): a
		// TEAMMATE's hook must not drive the MAIN thread's chip.
		//
		// switchboard-ctl identifies its caller with getppid() and subagents run
		// in-process, so a teammate's hook is indistinguishable from the main
		// thread's by pid — it lands on this very AgentInfo. PostToolUse maps
		// unconditionally to working, so with four teammates the parent's chip was
		// dragged green every ~4s no matter what the main thread was doing,
		// including while it sat parked at a prompt. That is the ENGINE of the limit
		// cycle in §3.4: it hands the reconciler something to demote on every tick.
		// The 12:39:56 restart is the controlled experiment — it removed the stale
		// anchor (defect 3, what T4 fixes) and the flap only slowed, from the 5s tick
		// to the 15s grace, cycling seven more times. Damping it is not removing it;
		// this is.
		//
		// agent_id is the discriminator and the ONLY one: normalized once at the top
		// of handleHook, non-empty means "this hook fired inside a subagent". If it
		// ever arrives empty on some subagent path this whole clause is inert and
		// behavior is exactly what it is today — the correct degradation, and the
		// reason nothing here guesses at teammate-ness from anything else.
		//
		// Two carve-outs, both deliberate:
		//
		//   - status == permission. A subagent-raised prompt MUST still turn the chip
		//     red (status-color-state-model.md §5 case 16): it is blocking the user,
		//     who has exactly one chip per session to look at. Raising red is the one
		//     thing a teammate legitimately says about its parent.
		//   - info.Status == permission. clearsPermission is the SINGLE door out of
		//     red (T2/T3), it already refuses teammate events, and it logs why on a
		//     parseable decision line. Dropping the event silently here would open a
		//     second, unlogged exit and fragment exactly the forensics that diagnosed
		//     this incident. Red stays the hold gate's to hold.
		//
		// Nothing else changes. SubagentStart/Stop already map to "" and still reach
		// the fanout re-scan below, so the S dimension keeps updating at hook speed
		// and the reconciler still paints a delegating parent green via
		// case5-delegating — from S, which is what legitimately means "work is
		// happening", rather than from a teammate's tool traffic. Nor is any
		// main-thread signal lost: a mid-turn main thread already went working on its
		// own UserPromptSubmit and fires its own hooks, and an orchestrator woken by
		// a returning teammate is recovered by resume-activity, which reads the MAIN
		// transcript (cmd/switchboard/main.go:831) — the Task tool_result lands there
		// and classifies as SignalActivity before the main thread emits a token.
		if req.AgentID != "" && status != state.StatusPermission && info.Status != state.StatusPermission {
			status = ""
		}

		// A "permission" chip must stay red until the *prompt itself* resolves —
		// not merely until some tool finishes, and not merely because some other
		// hook fired. PostToolUse fires for EVERY tool that completes, including a
		// sibling tool in the same turn or a teammate subagent's tool that lands
		// while an interactive prompt (AskUserQuestion / plan / approval) is still
		// waiting on the user; and Stop / UserPromptSubmit / SessionStart carry no
		// evidence about the prompt at all. Honoring any of them blindly repaints
		// the red chip the instant unrelated work happens.
		//
		// So the gate covers EVERY event that would move the chip off permission,
		// not just PostToolUse (defect 5 of docs/subagent-permission-oscillation.md
		// §3.5: with a teammate blocked, the main thread merely finishing its turn
		// used to repaint the chip orange and discard the red). clearsPermission is
		// the single door out: the fast path (the approved tool's own PostToolUse,
		// by tool_name) clears at hook speed, else the transcript must show the turn
		// resumed. Anything else holds red — the reconciler's TTL backstop decays a
		// truly stuck one.
		//
		// Three behaviors are preserved deliberately:
		//   - codex is exempt (it records no approvals in its rollout, so a codex
		//     PostToolUse advances straight to working without this guard);
		//   - a fresh PermissionRequest is not gated — it maps to "permission"
		//     itself, so it never moves the chip OFF red;
		//   - SubagentStart/Stop map to "" (status unchanged) and so fall through
		//     to the fanout re-scan below, untouched.
		//
		// UserPromptSubmit is NOT exempt (plan Q6). Typing during a pending prompt
		// is evidence the user is at the keyboard, but queueing a message while a
		// prompt waits is common enough that treating it as an answer would
		// reintroduce the missed RED. Revisit only if it produces a felt stale red.
		//
		// A ROTATED session is exempt: the red the gate would hold belongs to the
		// session that just retired, and nothing in the new one can ever resolve it
		// (see the `rotated` clause above).
		gateLogged := false
		// transitionRule/Reason carry the permission-gate's decision into the
		// history event below, so an approve-cleared edge records WHY it cleared
		// (the plain hook edges leave them empty).
		var transitionRule, transitionReason string
		if agent == state.AgentKindClaude && !rotated && info.Status == state.StatusPermission &&
			status != "" && status != state.StatusPermission {
			clear, writer, rule, reason := s.clearsPermission(info, req)
			d := statustune.Decision{
				PID: pid, Session: shortID(coalesce(req.SessionID, info.SessionID)),
				From: state.StatusPermission, To: state.StatusPermission, Rule: rule, Reason: reason,
				Pending: info.PendingSummary(), Subagents: info.InFlightSubagents,
				Age: time.Since(info.StatusSince),
			}
			if clear {
				// T7/T9: the verdict is now per-WRITER, so it removes ONLY the entry the
				// evidence names (plan §3.3: "P[a] is removed only by evidence from writer
				// a"). Dropping the whole map here — which is what T5 had to do while the
				// verdict was whole-session — is case 18's missed RED: one writer's approved
				// tool would discard a second writer's still-waiting prompt.
				info.DropPending(writer)
			}
			// The fold (plan §3.3): red is owned by Pending, so the chip may leave
			// "permission" only when no writer still holds a prompt. With the removal
			// narrowed above, this conjunct is what actually holds red for the writers the
			// evidence did NOT name — case 18.
			if clear && len(info.Pending) == 0 {
				d.To = status
				transitionRule, transitionReason = rule, reason
			} else {
				if clear {
					// Case 18: this event DID resolve a prompt, and the chip stays red anyway
					// because another writer is still waiting. Re-attribute the line — an
					// approve rule on a permission==permission edge reads as a contradiction to
					// the forensics, and this state has never had a rule of its own. The
					// remainder is named post-drop, so the line says who is still blocking;
					// pending= stays the pre-decision snapshot every other decision line carries.
					d.Rule = statustune.RuleHoldOtherWriter
					d.Reason = fmt.Sprintf("%s; %s still blocked", reason, pendingWritersLabel(info))
				}
				status = "" // hold red
			}
			d.Log()
			gateLogged = true
		}
		// Stamp StatusSince only on a real transition, so repeated same-status
		// hooks (e.g. successive PostToolUse) don't keep resetting the age the
		// reconciler uses to decay a stale "permission" chip.
		if status != "" && status != info.Status {
			// Log every chip color change with its cause. This is the forensic
			// trail for state drift: grepping `status: pid=<n>` reconstructs a
			// session's full transition history, and the gap between an idle/
			// permission edge and the next working edge measures how long a chip
			// lagged reality. agent=/event= name which agent and hook drove it. The
			// permission gate already logged its (richer) decision, so skip the
			// generic line there to avoid a duplicate.
			if !gateLogged {
				log.Printf("status: pid=%d %s %s->%s (agent=%s event=%s)", pid, sessionLabel(sess, req.SessionID), info.Status, status, agent, req.Event)
			}
			// Mirror the edge into the durable activity log (Phase usage-history).
			// Captured BEFORE the mutation below: `from` is the prior status, the age
			// is how long it was held (the closed interval), and pendingForEvent is
			// the tool a permission edge concerns (entering: this prompt's tool;
			// leaving: the tool that was pending, before it is forgotten).
			pendingForEvent := info.PendingTool
			if status == state.StatusPermission {
				pendingForEvent = req.ToolName
			}
			evNow := time.Now()
			s.hist.Record(history.Event{
				Ts: evNow, Type: history.EventTransition,
				SessionID: coalesce(req.SessionID, info.SessionID), PID: pid, Agent: agent, CWD: sess.CWD,
				From: info.Status, To: status, Rule: transitionRule, Reason: transitionReason,
				Subagents: info.InFlightSubagents, Pending: pendingForEvent,
				DurPrevMs: history.HeldMs(info.StatusSince, evNow),
			})
			if info.Status == state.StatusPermission && status != state.StatusPermission {
				// Leaving red: forget who owned the prompt. Redundant for a claude chip
				// (the gate above already emptied the map to get here) but not for codex,
				// which is exempt from the gate entirely and walks out of permission on
				// its own next hook.
				info.ClearPending()
			}
			info.Status = status
			// Date the transition per the anchoring policy (transcript.AnchorSince):
			// for an edge INTO working, pull StatusSince back to the transcript entry
			// that triggered this hook, because the hook reaches us tens-to-hundreds
			// of ms after Claude recorded that entry and a wall-clock stamp would sit
			// AHEAD of a fast follow-up signal (e.g. an immediate Ctrl+C), hiding it
			// from the reconciler's hookless recovery. For an edge INTO idle
			// (Stop/SessionStart) or INTO permission (PermissionRequest), use
			// wall-clock now instead: the turn's own entries flush a beat AFTER the
			// hook yet are dated before it — the final assistant message after its
			// Stop, the pre-prompt thinking/text after its PermissionRequest — so a
			// transcript anchor would let them read as post-transition signals and
			// falsely re-green the chip (or release a still-pending red one). All
			// three skew classes are in docs/timing-hazards.md (H1, H7, H8).
			//
			// The working anchor is floored at the chip's PREVIOUS StatusSince,
			// captured here before the assignment overwrites it. info.Transcript is
			// always the MAIN transcript, but a subagent's hooks are attributed to
			// this session, so a teammate's PostToolUse would otherwise date the edge
			// from whatever the main thread last wrote — minutes ago while it sits
			// blocked — and an arbitrarily stale StatusSince defeats the reconciler's
			// idle-title grace, which is the orange half of the oscillation in
			// docs/subagent-permission-oscillation.md §3.3.
			now := time.Now()
			prevStatusSince := info.StatusSince
			info.StatusSince = transcript.AnchorSince(info.Transcript, now, prevStatusSince, status == state.StatusWorking, s.tun.TailBytes)
		}

		// Entry (plan §3.2): a PermissionRequest records the prompt under the WRITER
		// that raised it — req.AgentID, already normalized at the top of this
		// function, empty meaning the main thread.
		//
		// Deliberately OUTSIDE the transition block above. A second writer blocking
		// while the chip is already red produces status == info.Status, so that block
		// is skipped, and a prompt recorded there would be lost — which is precisely
		// the case the scalar could not represent and the map exists for (case 18: two
		// writers blocked, one resolving must not clear the chip).
		//
		// Claude-only. Codex records no approvals in its rollout and is exempt from
		// the hold gate, so an entry written for it would never be resolved by
		// anything and would latch a hydrated red forever after the next restart.
		if agent == state.AgentKindClaude && req.Event == "PermissionRequest" {
			info.SetPending(req.AgentID, state.PendingPrompt{
				Tool: req.ToolName, InputHash: req.ToolInputHash, Since: time.Now(),
			})
		}
		// Keep the stored identity on the LIVE session, last-hook-wins — the same
		// rule info.Transcript already follows above. A session can rotate its id
		// under a stable pid (a /clear or fork: same process, new session_id, new
		// transcript file); freezing the id at first-seen (the old `info.SessionID
		// == ""` guard) let the transcript field follow the new file while the id
		// stayed pinned to the retired session, so reconciler-derived edges and the
		// fanout Observer keyed off an id that no longer matched the transcript the
		// daemon was reading. Refreshing here keeps id and transcript describing the
		// same session. (--resume is the benign case: new pid, same id — a fresh
		// AgentInfo, so this simply sets it.)
		if req.SessionID != "" {
			info.SessionID = req.SessionID
		}
		// SubagentStart/Stop: trigger an immediate fanout re-scan so the in-flight
		// count and the spawn/stop history update at hook speed instead of waiting for
		// the next reconcile tick. The Observer is the single writer and re-derives
		// everything from the authoritative subagents/ dir — the hook only triggers
		// (it carries no tool_use_id and must not mutate the counter), so a duplicate
		// or out-of-order hook is harmless. statusFromHookEvent returns "" for these,
		// so the main thread's status is untouched; the delegating edge follows from
		// the refreshed InFlightSubagents.
		if s.fanout != nil && (req.Event == "SubagentStart" || req.Event == "SubagentStop") {
			for _, ev := range s.fanout.Reconcile(sess, info, time.Now()) {
				if s.hist != nil {
					s.hist.Record(ev)
				}
			}
		}
	})
}

// dispatchAgentHook attributes against a detached snapshot and performs all
// process reads before invoking the provider-aware handler. In particular, no
// /proc read, transcript read, or provider callback runs under Store.Apply.
func (s *Server) dispatchAgentHook(req Request) {
	snap := s.store.Snapshot()
	tracked := make(map[int]*state.Session, len(snap.Sessions))
	for i := range snap.Sessions {
		tracked[snap.Sessions[i].PID] = &snap.Sessions[i]
	}
	pid := findTrackedAncestor(tracked, req.PID, s.readProc)
	if pid == 0 {
		return
	}
	s.agentHook(req, *tracked[pid])
}

// agentIDPrefix is the "agent-" that brackets a subagent id in its on-disk file
// names (agent-<id>.meta.json / agent-<id>.jsonl). transcript.Subagent.AgentID
// and history.Event.AgentID both store the id with this prefix already removed.
const agentIDPrefix = "agent-"

// normalizeAgentID canonicalizes a hook's agent_id to the bare form the rest of
// the daemon keys on, stripping one leading "agent-" if present.
//
// The join it protects: the fanout Observer writes the PREFIX-STRIPPED id (it
// derives it from the agent-<id>.* file names), while a hook sends whatever
// Claude Code puts in agent_id — a shape nobody has yet read off a live payload
// (plan T1). If those two spellings disagree, nothing crashes: a map keyed by the
// hook value simply never joins the Observer's seen-set, and every correlation
// between a pending prompt and fanout state fails in silence. Normalizing here
// makes T1's eventual answer stop mattering.
//
// Three properties the callers depend on:
//
//   - AT MOST ONE strip. Subagent ids are themselves `a`-initial (file
//     agent-a158b13da3d13b0ea ⇒ id a158b13da3d13b0ea), so a doubled prefix is
//     plausible on the wire; an id that legitimately reads "agent-…" after the
//     strip keeps it. This is why the daemon must normalize in exactly one place —
//     a second call at a use site would eat that second prefix.
//   - EMPTY STAYS EMPTY. Empty agent_id means main thread, so this must never
//     manufacture a non-empty id from one.
//   - NON-EMPTY STAYS NON-EMPTY, for the same reason read the other way. A
//     degenerate bare "agent-" is left intact rather than stripped to "", which
//     would silently re-attribute a subagent's hook to the main thread — exactly
//     the class of invisible failure this function exists to close.
func normalizeAgentID(id string) string {
	rest, ok := strings.CutPrefix(id, agentIDPrefix)
	if !ok || rest == "" {
		return id
	}
	return rest
}

// pendingEntryFor resolves the prompt entry a hook's evidence is allowed to speak
// to: the one owned by `writer` (bare, normalized agent_id; "" = main thread).
//
// A writer that owns no entry gets ok=false, and that is the hard part of the T7
// rule — evidence from a writer with no prompt of its own says nothing about
// anybody else's (plan §3.3). It is what rules teammates out.
//
// The one degradation: an EMPTY map beside a red chip. Post-T5 every live claude
// red has an entry (PermissionRequest writes one, hydrate rebuilds one), so this is
// a pre-map or hand-seeded block. There the derived scalar PendingTool stands in for
// a main-thread entry with no input correlator, which reproduces exactly the
// pre-T7 behavior for the only writer such a block can be describing. Restricted to
// writer == "": with a NON-empty map, an unattributable event must not borrow some
// other writer's prompt (PendingTool falls back to the lowest-keyed writer, so
// reading it here would resurrect defect 1 — the flattening this whole change
// removes).
func pendingEntryFor(info *state.AgentInfo, writer string) (state.PendingPrompt, bool) {
	if p, ok := info.Pending[writer]; ok {
		return p, true
	}
	if len(info.Pending) == 0 && writer == "" {
		return state.PendingPrompt{Tool: info.PendingTool}, true
	}
	return state.PendingPrompt{}, false
}

// clearsPermission decides whether a hook event releases a red chip, WHICH writer's
// prompt it releases, and the rule/reason for the forensic decision log. The
// returned writer is the key handleHook drops from Pending — never the whole map,
// or one writer's approved tool discards another's still-waiting prompt (case 18).
//
// req.AgentID must already be normalized (handleHook does it once, at entry).
//
// The rule, in one line: evidence resolves a prompt only when it comes from the
// writer that raised it (plan §3.3, docs/claude-code-hook-schema.md §2).
//
//	writer mismatch (agent_id owns no prompt)  → HOLD, no transcript read
//	unidentified writer, teammates in flight   → never at hook speed; the transcript decides
//	writer + tool_name + input hash all match  → CLEAR at hook speed
//	writer + tool_name, hashes differ          → fall through to the transcript
//	writer + tool_name, no hash on either side → CLEAR
//	anything else                              → the writer's OWN transcript decides
//
// ⚠ Why a hash MISMATCH does not hold red on its own. PostToolUse reports
// `updatedInput` — the input AFTER the permission decision — and the approval paths
// rewrite it: `{...e,command:g}` on "path layer approved", `{...e,path:r}` on a
// permission-root relocation, injected keys, plus a `userModified` flag when the
// user edits the call in the dialog. The rewrites include `command:` on Bash, the
// tool in the 2026-08-05 incident. So a mismatch is genuinely ambiguous between
// "same call, input rewritten on approval" (must clear) and "sibling same-named call
// by the same writer" (must hold), and requiring a hash match would have held red
// forever on exactly the approve path this fast path exists to serve. The transcript
// asks the question that is actually decisive: did the blocked writer resume?
//
// ⚠ An EMPTY hash is NO SIGNAL, not a mismatch (rpc.Request.ToolInputHash: absent,
// unparseable, null and empty inputs all send ""). Two empties must never read as a
// match, so the decision falls back to (agent_id, tool_name) — T2's rule narrowed to
// one writer — rather than to the transcript, which would strand every no-arg tool
// call and every pre-T6 ctl on the slow path.
//
// ⚠ T2's guard survives as the FLOOR for the unidentifiable writer, and it OUTRANKS
// the correlator (T21). An empty agent_id is "main thread" AND "a hook that carried
// no id", so with subagents in flight NO hook-speed match may clear it — not tool
// name, and not an input hash either.
//
// T7 exempted a hash match here, reasoning that a name AND a hash collision needs
// "two independent failures to coincide". They are not independent. The hash is taken
// over tool_input ALONE (cmd/switchboard-ctl.hashToolInput — no cwd, no session, no
// writer), so it says WHICH CALL, never WHO RAN IT: it is a second reading of the
// same axis agent_id already covers, and it becomes load-bearing precisely when
// agent_id has told us nothing. That degraded case — agent_id absent on tool events —
// is exactly what plan T1 has not yet ruled out (hook-schema §4, confidence medium).
// And a byte-identical same-tool call from a teammate is ordinary under fanout, not
// exotic: five agents in five worktrees run one `go build ./...`, and a command
// pre-approved under one worktree's permission root while it prompts under another
// collides with no human having answered anything. That is the 12:38:21 lost RED with
// one extra step.
//
// The price is one reconcile tick — ≤5s, §4's own P2 budget, and only when the writer
// is unidentifiable AND teammates are live; an identified writer's fast path is
// untouched. A missed RED is the worst error there is (§4.1) and a slow-but-correct
// clear the cheapest. Fail closed, never open.
//
// ⚠ Do NOT relax matching to agent_id alone (plan §9.6 trap 2). A hydrated entry has
// Tool == "", so the `req.ToolName != ""` guard makes it unmatchable and it resolves
// by transcript — which is correct and load-bearing: Claude Code emits parallel
// tool_use blocks in one assistant message, so a writer can complete an
// auto-approved sibling while its own prompt still waits. That is this incident's
// bug at a narrower radius.
//
// The transcript fallback is ROUTED (T9): entry P[a] is resolved against
// transcript.SubagentPath(info.Transcript, a) — the main .jsonl when a == "", the
// teammate's own sibling file otherwise. info.Transcript is ALWAYS the parent's
// (hook-schema §3), so reading it for a subagent-raised prompt asks about the wrong
// writer: while the main thread works, its file shows resume the whole time a
// teammate sits blocked (defect 4).
//
// A decline/interrupt deliberately does NOT clear here (it fires no PostToolUse; and
// exiting a hook-driven *working* edge to green on an interrupt would paint the
// wrong color) — the reconciler demotes it to idle/orange instead. Anything else
// holds red (case 12), and the reconciler's per-prompt backstop decays a stuck one.
func (s *Server) clearsPermission(info *state.AgentInfo, req Request) (clear bool, writer, rule, reason string) {
	writer = req.AgentID
	entry, owns := pendingEntryFor(info, writer)

	// The fast path, in three parts. `owns` is the writer gate; ToolName != "" is
	// trap 2's guard (a hydrated entry, Tool == "", can never satisfy it).
	nameMatch := s.tun.EarlyClearApproveByToolName && owns &&
		req.ToolName != "" && req.ToolName == entry.Tool
	hashKnown := entry.InputHash != "" && req.ToolInputHash != ""
	hashMatch := hashKnown && entry.InputHash == req.ToolInputHash
	hashMismatch := hashKnown && entry.InputHash != req.ToolInputHash
	// The T2 floor: nothing identifies this writer, so ANY correlator match — tool
	// KIND or input hash, neither of which names a writer — is a teammate collision
	// waiting to happen while any teammate can produce one.
	unidentified := writer == "" && info.InFlightSubagents > 0

	switch {
	case nameMatch && unidentified, nameMatch && hashMismatch:
		// Unattributable with teammates live (the floor OUTRANKS the correlator —
		// T21), or ambiguous (input rewritten on approval vs. a sibling call). Either
		// way the transcript below decides.
	case nameMatch && hashMatch:
		return true, writer, statustune.RuleApproveToolMatch,
			fmt.Sprintf("%s completed %s, input %s — the approved call", writerLabel(writer), req.ToolName, req.ToolInputHash)
	case nameMatch:
		return true, writer, statustune.RuleApproveToolMatch,
			fmt.Sprintf("%s completed %s (no input signal on either event)", writerLabel(writer), req.ToolName)
	}

	// Transcript fallback, routed to the prompt's own writer. Skipped outright when
	// the event's writer owns no prompt: there is no entry it could resolve, and
	// reading the main transcript for somebody else's prompt is defect 4.
	if owns {
		since := entry.Since
		if since.IsZero() {
			since = info.StatusSince // pre-map/hand-seeded block: the chip's own onset
		}
		path := transcript.SubagentPath(info.Transcript, writer)
		if k, _ := transcript.ResolveKind(path, since, s.tun.TailBytes); k == transcript.ResolutionResumed {
			return true, writer, statustune.RuleApproveTranscript,
				"transcript: " + writerLabel(writer) + " resumed"
		}
	}

	// Holds, safety-critical attribution first: an unattributable or wrong-writer
	// tool-KIND collision with the tool the chip's red is reported under is the
	// 12:38:21 edge, and it must read as that even when a hash also disagrees.
	if req.ToolName != "" && req.ToolName == info.PendingTool && (!owns || unidentified) {
		return false, writer, statustune.RuleHoldTeammateCollision,
			fmt.Sprintf("%s completed %s; the prompt belongs to %s — tool kind, not tool identity (S=%d)",
				writerLabel(writer), req.ToolName, pendingWritersLabel(info), info.InFlightSubagents)
	}
	if nameMatch && hashMismatch {
		return false, writer, statustune.RuleHoldInputMismatch,
			fmt.Sprintf("%s completed %s but input %s != the pending %s, and its transcript shows no resume",
				writerLabel(writer), req.ToolName, req.ToolInputHash, entry.InputHash)
	}
	if req.Event != "PostToolUse" {
		return false, writer, statustune.RuleHoldNonToolEvent, "prompt still pending; " + req.Event + " is not evidence"
	}
	return false, writer, statustune.RuleHoldBareResult, "prompt still pending"
}

// writerLabel renders a Pending key for a human-read decision line, spelling the
// empty (main-thread) key out. Log text only — never a map key.
func writerLabel(writer string) string {
	if writer == "" {
		return state.PendingWriterMain
	}
	return writer
}

// pendingWritersLabel names every writer currently holding a prompt, for the
// decision log's reason= field — so a hold says WHO is still waiting, not just that
// somebody is. Ordered by PendingWriterKeys (sorted), because a reason string that
// permutes between ticks is unreadable and undiffable.
func pendingWritersLabel(info *state.AgentInfo) string {
	keys := info.PendingWriterKeys() // freshly allocated and sorted; safe to relabel
	if len(keys) == 0 {
		return "no recorded writer" // a pre-map/hand-seeded block: PendingTool with no owner
	}
	for i, k := range keys {
		keys[i] = writerLabel(k)
	}
	return strings.Join(keys, ",")
}

// handleActivity records a global, session-less user-activity edge: "idle" when
// the user stepped away from the keyboard, "active" when input resumed. The signal
// is fed by an idle daemon (hypridle) via `switchboard-ctl activity <idle|active>`;
// any other value is rejected so a misconfigured timer can never pollute the
// stream. Unlike a per-session transition it carries no PID/session — a dashboard
// derives global active/idle intervals from the To values alone. Recording is
// best-effort: a nil/disabled sink drops it (Sink.Record is a no-op), but the
// value is still validated so the client learns it sent garbage.
func (s *Server) handleActivity(req Request) error {
	switch req.Activity {
	case "idle", "active":
	default:
		return fmt.Errorf("activity must be idle|active (got %q)", req.Activity)
	}
	s.hist.Record(history.Event{
		Ts:   time.Now(),
		Type: history.EventActivity,
		To:   req.Activity,
	})
	return nil
}

// shortID trims a session id to its first segment for compact decision logs.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// coalesce returns the first non-empty string.
func coalesce(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// findTrackedAncestor walks up the ppid chain starting at pid, returning the
// first PID that's a tracked session. Bounded depth keeps us out of trouble on
// weird process states. readProc is injected (defaults to proc.Read at the call
// site) so the walk is testable without a live /proc.
//
// The walk exists for ONE reason: a hook that ran inside a shell wrapper, where
// getppid() is the wrapper rather than the agent. It stops at the nearest AGENT
// process for the mirror reason — that process owns the hook, and if it is not
// tracked the hook belongs to nobody the daemon knows about. Handing it to the
// grandparent is not a fallback; it is a misattribution onto a live session.
//
// # A2 — the nested-headless race (docs/claude-code-hook-schema.md §6)
//
// A `claude -p` spawned from inside an interactive session (the session-digest
// summarizer, a flag investigation, any nested SDK run) fires SessionStart within
// milliseconds of exec, while discovery registers it only on the next /proc scan
// tick (--scan-interval, default 1s). In that sub-second window its pid is not in
// m, so the old walk sailed past it and landed on the interactive parent, which
// then took the helper's identity: Transcript repointed at the helper's file (so
// the reconciler derived the PARENT's status from the HELPER's transcript), a
// false rotation clearing the parent's pending prompts, and SessionID overwritten
// last-hook-wins and never restored.
//
// Measured on 2026-07-31: 35 of 102 session ids were carried by two pids, and one
// interactive session cycled through 41 ids in 10.5 hours. In 32 of those 35 the
// parent took the id 1-700ms BEFORE the helper's own session_start — the whole
// distribution sits inside one scan tick, which is the signature of this race and
// the reason it reproduces roughly three runs in four.
//
// This is the process-level counterpart to T17 (rpc.go, handleHook), which stops
// a TEAMMATE's hook driving the MAIN thread's chip. T17 discriminates by agent_id
// and suppresses only status; it cannot see this case, which is a separate
// process and corrupts identity rather than status.
//
// Returning 0 drops the hook. The cost is bounded and self-correcting: the scan
// picks the helper up within the tick and its later hooks self-match, so only the
// hooks fired inside the window are lost. A `claude -p` shorter than one tick
// gets no session id at all — an unnamed single-interval lane, the shape
// switchboard already produces for a process that dies before transitioning.
func findTrackedAncestor(m map[int]*state.Session, pid int, readProc func(int) (proc.Info, error)) int {
	for depth := 0; pid > 1 && depth < 20; depth++ {
		if _, ok := m[pid]; ok {
			return pid
		}
		info, err := readProc(pid)
		if err != nil || info.PPID == 0 {
			return 0
		}
		if discovery.Classify(osproc.FromProc(info)) != discovery.AgentNone {
			return 0
		}
		pid = info.PPID
	}
	return 0
}

// sessionLabel builds a stable, human-recognizable identifier for status log
// lines. The Claude session id survives PID reuse (the same chip across a
// daemon or session restart), so it anchors the timeline; the terminal window
// title is what actually names the chip on the bar, so it makes a line readable
// at a glance. Both are best-effort: a hook can arrive before either resolves,
// hence the "?" / cwd fallbacks. preferID lets the caller pass req.SessionID,
// which a hook carries before it has been copied onto the session.
func sessionLabel(sess *state.Session, preferID string) string {
	id := preferID
	if id == "" {
		if info := sess.Enrichment(); info != nil {
			id = info.SessionID
		}
	}
	if id == "" {
		id = "?"
	} else if len(id) > 8 {
		id = id[:8]
	}
	if sess.Wezterm != nil && sess.Wezterm.WindowTitle != "" {
		return fmt.Sprintf("session=%s %q", id, strings.TrimSpace(sess.Wezterm.WindowTitle))
	}
	return fmt.Sprintf("session=%s cwd=%s", id, sess.CWD)
}

// statusFromHookEvent maps a hook event to a chip status for the given agent.
// The two agents share most of the vocabulary; Codex additionally emits
// PreToolUse (Claude Code does not wire it here). Any unmapped event returns ""
// (status unchanged) and "unknown" is never emitted.
//
// It answers "what would this event mean for the thread that fired it" and
// nothing more — WHICH thread fired it is not an input here. handleHook narrows
// the answer afterwards: a hook carrying an agent_id came from a subagent, and a
// subagent does not speak for its parent's chip (T17, defect 2). Keep that
// narrowing at the call site; this stays the plain vocabulary table its test
// enumerates.
func statusFromHookEvent(agent, event string) string {
	if agent == state.AgentKindCodex {
		switch event {
		case "UserPromptSubmit", "PreToolUse", "PostToolUse":
			return "working"
		case "PermissionRequest":
			return "permission"
		case "Stop", "SessionStart":
			return "idle"
		}
		return ""
	}
	switch event {
	case "UserPromptSubmit", "PostToolUse":
		return "working"
	case "PermissionRequest":
		return "permission"
	case "Stop", "SessionStart":
		return "idle"
	}
	return ""
}

// Client is a tiny convenience for ctl tooling.
type Client struct {
	conn net.Conn
	enc  *json.Encoder
	dec  *json.Decoder
}

func Dial(socketPath string) (*Client, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, err
	}
	return &Client{
		conn: conn,
		enc:  json.NewEncoder(conn),
		dec:  json.NewDecoder(bufio.NewReader(conn)),
	}, nil
}

func (c *Client) Close() error                   { return c.conn.Close() }
func (c *Client) SetDeadline(at time.Time) error { return c.conn.SetDeadline(at) }
func (c *Client) Send(req Request) error         { return c.enc.Encode(req) }
func (c *Client) Recv(resp *Response) error      { return c.dec.Decode(resp) }
