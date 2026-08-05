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

	"github.com/tjmisko/switchboard/internal/fanout"
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

// ErrHeadlessSession is returned by focus when the selected session is a
// non-interactive `claude -p` run — it is tracked and visible, but there is no
// window or pane to land on.
var ErrHeadlessSession = errors.New("headless session (claude -p) has no window to focus")

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
	Snapshot *state.Snapshot `json:"snapshot,omitempty"`
	OK       bool            `json:"ok,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// rawResponse mirrors Response field-for-field — same names, same order, same
// omitempty — except the snapshot arrives already encoded. It is the envelope for
// the subscribe stream, where state.Store has marshaled the snapshot once for all
// subscribers (see snapshotFrame).
//
// ⚠ The two structs must be edited together: their encodings are required to be
// byte-identical, which TestSubscribeFrameMatchesResponseEncoding pins.
type rawResponse struct {
	Snapshot json.RawMessage `json:"snapshot,omitempty"`
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
	fanout     *fanout.Observer
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

// SetFanout wires the subagent fanout Observer that a SubagentStart/Stop hook
// triggers an immediate re-scan on (single source of truth, shared with the
// reconcile loop). Call once at startup before Serve. A nil Observer (the default)
// disables the hook-speed trigger, leaving the reconcile tick to pick fanouts up.
func (s *Server) SetFanout(o *fanout.Observer) { s.fanout = o }

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
				req.ToolName, info.Status, info.PendingTool, info.InFlightSubagents)
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
		gateLogged := false
		// transitionRule/Reason carry the permission-gate's decision into the
		// history event below, so an approve-cleared edge records WHY it cleared
		// (the plain hook edges leave them empty).
		var transitionRule, transitionReason string
		if agent == state.AgentKindClaude && info.Status == state.StatusPermission &&
			status != "" && status != state.StatusPermission {
			clear, rule, reason := s.clearsPermission(info, req.Event, req.ToolName)
			d := statustune.Decision{
				PID: pid, Session: shortID(coalesce(req.SessionID, info.SessionID)),
				From: state.StatusPermission, To: state.StatusPermission, Rule: rule, Reason: reason,
				Pending: info.PendingTool, Subagents: info.InFlightSubagents,
				Age: time.Since(info.StatusSince),
			}
			if clear {
				d.To = status
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
			if status == state.StatusPermission {
				info.PendingTool = req.ToolName // capture the tool the prompt is for (A2)
			}
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

// clearsPermission decides whether a hook event should release a red chip and
// names the rule/reason for the forensic decision log. Two gates, fast first:
//
//   - tool-name fast path (A2): the PostToolUse's tool_name matches the tool the
//     prompt was raised for (PendingTool), i.e. the *approved* tool just
//     completed. Clears at hook speed — the fix for the ~26s approve-path lag.
//     Refused while subagents are in flight, see below.
//   - transcript fallback: the main thread produced an assistant message after the
//     prompt (ResolutionResumed), i.e. the turn resumed. Covers the case where the
//     tool_name was not forwarded, and every non-tool event (Stop /
//     UserPromptSubmit / SessionStart), which carries no tool_name at all.
//
// ⚠ The fast path is a comparison on a tool *kind*, not a tool *identity*. With
// any subagent in flight, a teammate running the same kind of tool satisfies it —
// and the tool in question is usually Bash, the most frequently executed tool
// there is. That is the 2026-08-05 lost RED verbatim: at 12:38:20 this guard
// correctly held, and one second later a teammate's Bash cleared the same pending
// Bash prompt (docs/subagent-permission-oscillation.md §2.2/§3.1). So with
// InFlightSubagents > 0 the fast path is refused outright and the transcript
// fallback decides. This deliberately trades back some of the approve-path
// latency the fast path bought, but only in sessions with teammates in flight,
// and only until (agent_id, tool_name, tool_input) matching lands (plan T7): a
// missed RED is the worst error and a slow-but-correct clear is the cheapest.
//
// A decline/interrupt deliberately does NOT clear here (it fires no PostToolUse;
// and exiting a hook-driven *working* edge to green on an interrupt would paint
// the wrong color) — the reconciler demotes it to idle/orange instead. Anything
// else holds red (case 12: a bare/sibling/teammate tool_result is not resolution,
// and neither is an unrelated lifecycle event).
func (s *Server) clearsPermission(info *state.AgentInfo, event, toolName string) (clear bool, rule, reason string) {
	nameMatch := s.tun.EarlyClearApproveByToolName && toolName != "" && toolName == info.PendingTool
	teammateCollision := nameMatch && info.InFlightSubagents > 0
	if nameMatch && !teammateCollision {
		return true, statustune.RuleApproveToolMatch, "tool-name match: " + toolName
	}
	if k, _ := transcript.ResolveKind(info.Transcript, info.StatusSince, s.tun.TailBytes); k == transcript.ResolutionResumed {
		return true, statustune.RuleApproveTranscript, "transcript: turn resumed"
	}
	if teammateCollision {
		return false, statustune.RuleHoldTeammateCollision,
			fmt.Sprintf("tool-name match on %s but %d subagent(s) in flight — kind, not identity", toolName, info.InFlightSubagents)
	}
	if event != "PostToolUse" {
		return false, statustune.RuleHoldNonToolEvent, "prompt still pending; " + event + " is not evidence"
	}
	return false, statustune.RuleHoldBareResult, "prompt still pending"
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
