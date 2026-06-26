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

	"github.com/tjmisko/switchboard/internal/history"
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

type Request struct {
	Cmd      string `json:"cmd"`
	Selector string `json:"selector,omitempty"`

	// hook fields — set when Cmd == "hook"
	Event      string `json:"event,omitempty"`
	PID        int    `json:"pid,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	Transcript string `json:"transcript,omitempty"`
	// Agent names which coding agent fired the hook: "claude" (default when
	// empty) or "codex". It routes the enrichment to the right block and selects
	// the event→status mapping.
	Agent string `json:"agent,omitempty"`
	// ToolName is the hook's tool_name when the event carries one (PermissionRequest,
	// PostToolUse). It is stashed at red-onset (PendingTool) and matched on a later
	// PostToolUse to clear red at hook speed when the approved tool completes —
	// while a non-matching/Task PostToolUse keeps the chip red. Empty for events
	// with no tool (UserPromptSubmit/Stop/SessionStart), which just disables the
	// fast path and falls back to the transcript check.
	ToolName string `json:"tool_name,omitempty"`
}

type Response struct {
	Snapshot *state.Snapshot `json:"snapshot,omitempty"`
	OK       bool            `json:"ok,omitempty"`
	Error    string          `json:"error,omitempty"`
}

type Server struct {
	store      *state.Store
	socketPath string
	term       terminal.Locator
	wm         wm.Manager
	tun        statustune.Tuning
	hist       *history.Sink
}

func New(store *state.Store, socketPath string, term terminal.Locator, manager wm.Manager) *Server {
	return &Server{store: store, socketPath: socketPath, term: term, wm: manager, tun: statustune.Default()}
}

// SetTuning overrides the status-color tuning (defaults from statustune.Default).
// Call once at startup before Serve; the hook handler reads it without a lock,
// which is safe because it is not mutated after startup.
func (s *Server) SetTuning(t statustune.Tuning) { s.tun = t }

// SetHistory wires the activity-log sink the hook handler records transitions to.
// Call once at startup before Serve. A nil sink (the default) records nothing.
func (s *Server) SetHistory(h *history.Sink) { s.hist = h }

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
		case snap, ok := <-ch:
			if !ok {
				return
			}
			if err := enc.Encode(Response{Snapshot: &snap}); err != nil {
				return
			}
		}
	}
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
	agent := req.Agent
	if agent == "" {
		agent = state.AgentKindClaude
	}
	status := statusFromHookEvent(agent, req.Event)
	if status == "" && req.SessionID == "" && req.Transcript == "" {
		return
	}
	s.store.Apply(func(m map[int]*state.Session) {
		pid := findTrackedAncestor(m, req.PID, proc.Read)
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

		// A "permission" chip must stay red until the *prompt itself* resolves —
		// not merely until some tool finishes. PostToolUse fires for EVERY tool
		// that completes, including a sibling tool in the same turn or a background
		// subagent's Task that lands while an interactive prompt (AskUserQuestion /
		// plan / approval) is still waiting on the user. Honoring it blindly flips
		// the red chip green the instant any such tool completes. clearsPermission
		// gates it two ways (see there): the identity-correlated fast path (the
		// approved tool's own PostToolUse, by tool_name) clears at hook speed; else
		// the transcript must show the turn resumed. A bare/Task tool_result is not
		// resolution, so the chip holds red — the reconciler's TTL backstop decays a
		// truly stuck one. Codex is exempt: it records no approvals in its rollout,
		// so a codex PostToolUse advances straight to working without this guard.
		gateLogged := false
		// transitionRule/Reason carry the permission-gate's decision into the
		// history event below, so an approve-cleared edge records WHY it cleared
		// (the plain hook edges leave them empty).
		var transitionRule, transitionReason string
		if agent == state.AgentKindClaude && status == "working" && req.Event == "PostToolUse" && info.Status == "permission" {
			clear, rule, reason := s.clearsPermission(info, req.ToolName)
			d := statustune.Decision{
				PID: pid, Session: shortID(coalesce(req.SessionID, info.SessionID)),
				From: "permission", To: "permission", Rule: rule, Reason: reason,
				Pending: info.PendingTool, Subagents: info.InFlightSubagents,
				Age: time.Since(info.StatusSince),
			}
			if clear {
				d.To = "working"
				transitionRule, transitionReason = rule, reason
			} else {
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
				info.PendingTool = "" // leaving red: forget the captured prompt tool
			}
			info.Status = status
			// Date the transition per the anchoring policy (transcript.AnchorSince):
			// for an edge INTO working/permission, pull StatusSince back to the
			// transcript entry that triggered this hook, because the hook reaches us
			// tens-to-hundreds of ms after Claude recorded that entry and a wall-clock
			// stamp would sit AHEAD of a fast follow-up signal (e.g. an immediate
			// Ctrl+C), hiding it from the reconciler's hookless recovery. For an edge
			// INTO idle (Stop/SessionStart), use wall-clock now instead: the completing
			// turn's own final assistant message is flushed a beat AFTER the Stop hook
			// yet dated before it, so a transcript anchor would let it read as "activity
			// after idle" and falsely re-green the chip. Both skew classes are in
			// docs/timing-hazards.md.
			now := time.Now()
			info.StatusSince = transcript.AnchorSince(info.Transcript, now, status == state.StatusIdle, s.tun.TailBytes)
			if status == state.StatusPermission {
				info.PendingTool = req.ToolName // capture the tool the prompt is for (A2)
			}
		}
		if req.SessionID != "" && info.SessionID == "" {
			info.SessionID = req.SessionID
		}
	})
}

// clearsPermission decides whether a PostToolUse should release a red chip and
// names the rule/reason for the forensic decision log. Two gates, fast first:
//
//   - identity-correlated fast path (A2): the PostToolUse's tool_name matches the
//     tool the prompt was raised for (PendingTool), i.e. the *approved* tool just
//     completed. Clears at hook speed — the fix for the ~26s approve-path lag.
//   - transcript fallback: the main thread produced an assistant message after the
//     prompt (ResolutionResumed), i.e. the turn resumed. Covers the case where the
//     tool_name was not forwarded.
//
// A decline/interrupt deliberately does NOT clear here (it fires no PostToolUse;
// and exiting a hook-driven *working* edge to green on an interrupt would paint
// the wrong color) — the reconciler demotes it to idle/orange instead. Anything
// else holds red (case 12: a bare/sibling/Task tool_result is not resolution).
func (s *Server) clearsPermission(info *state.AgentInfo, toolName string) (clear bool, rule, reason string) {
	if s.tun.EarlyClearApproveByToolName && toolName != "" && toolName == info.PendingTool {
		return true, statustune.RuleApproveToolMatch, "tool-name match: " + toolName
	}
	if k, _ := transcript.ResolveKind(info.Transcript, info.StatusSince, s.tun.TailBytes); k == transcript.ResolutionResumed {
		return true, statustune.RuleApproveTranscript, "transcript: turn resumed"
	}
	return false, statustune.RuleHoldBareResult, "prompt still pending"
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
func findTrackedAncestor(m map[int]*state.Session, pid int, readProc func(int) (proc.Info, error)) int {
	for depth := 0; pid > 1 && depth < 20; depth++ {
		if _, ok := m[pid]; ok {
			return pid
		}
		info, err := readProc(pid)
		if err != nil || info.PPID == 0 {
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

func (c *Client) Close() error              { return c.conn.Close() }
func (c *Client) Send(req Request) error    { return c.enc.Encode(req) }
func (c *Client) Recv(resp *Response) error { return c.dec.Decode(resp) }
