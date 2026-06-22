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

	"github.com/tjmisko/switchboard/internal/proc"
	"github.com/tjmisko/switchboard/internal/state"
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
}

func New(store *state.Store, socketPath string, term terminal.Locator, manager wm.Manager) *Server {
	return &Server{store: store, socketPath: socketPath, term: term, wm: manager}
}

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
	status := statusFromHookEvent(req.Event)
	if status == "" && req.SessionID == "" && req.Transcript == "" {
		return
	}
	s.store.Apply(func(m map[int]*state.Session) {
		pid := findTrackedAncestor(m, req.PID, proc.Read)
		if pid == 0 {
			return
		}
		sess := m[pid]
		if sess.Claude == nil {
			sess.Claude = &state.ClaudeInfo{}
		}
		// A "permission" chip must stay red until the *prompt itself* resolves —
		// not merely until some tool finishes. PostToolUse fires for EVERY tool
		// that completes, including a sibling tool in the same turn or a background
		// subagent's Task that lands while an interactive prompt (AskUserQuestion /
		// plan / approval) is still waiting on the user. Honoring it flips the red
		// chip green the instant any such tool completes (observed: a
		// PermissionRequest immediately followed by an unrelated PostToolUse one
		// second later). Gate it on the same transcript-resolution signal the
		// reconciler uses (transcript.ResolutionState): only clear when the main
		// conversation thread has actually advanced past the prompt. A bare
		// tool_result from concurrent work is not resolution, so the chip holds
		// red; an unreadable transcript is treated as still-pending too, leaving
		// selfHealStaleAttention's TTL backstop to decay a truly stuck chip.
		if status == "working" && req.Event == "PostToolUse" && sess.Claude.Status == "permission" {
			tpath := sess.Claude.Transcript
			if req.Transcript != "" {
				tpath = req.Transcript
			}
			if st, _ := transcript.ResolutionState(tpath, sess.Claude.StatusSince, transcript.DefaultTailBytes); st != transcript.StateResolved {
				log.Printf("status: pid=%d %s hold permission (event=PostToolUse, prompt still pending)", pid, sessionLabel(sess, req.SessionID))
				status = ""
			}
		}
		// Stamp StatusSince only on a real transition, so repeated same-status
		// hooks (e.g. successive PostToolUse) don't keep resetting the age the
		// reconciler uses to decay a stale "permission" chip.
		if status != "" && status != sess.Claude.Status {
			// Log every chip color change with its cause. This is the forensic
			// trail for state drift: grepping `status: pid=<n>` reconstructs a
			// session's full transition history, and the gap between an idle/
			// permission edge and the next working edge measures how long a chip
			// lagged reality. event= names which Claude Code hook drove it.
			log.Printf("status: pid=%d %s %s->%s (event=%s)", pid, sessionLabel(sess, req.SessionID), sess.Claude.Status, status, req.Event)
			sess.Claude.Status = status
			sess.Claude.StatusSince = time.Now()
		}
		if req.SessionID != "" && sess.Claude.SessionID == "" {
			sess.Claude.SessionID = req.SessionID
		}
		// Transcript path is stable per session; keep it fresh so the reconciler
		// can read the tail to tell a declined prompt from a still-pending one.
		if req.Transcript != "" {
			sess.Claude.Transcript = req.Transcript
		}
	})
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
	if id == "" && sess.Claude != nil {
		id = sess.Claude.SessionID
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

func statusFromHookEvent(event string) string {
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
