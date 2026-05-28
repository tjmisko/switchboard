// Package rpc exposes the daemon over a Unix socket. Protocol is one JSON
// request per line, with JSON responses streamed back. Commands:
//
//	{"cmd":"list"}                              -> {"snapshot":{...}}
//	{"cmd":"focus","selector":"active"|"<pid>"|"<index>"} -> {"ok":true}
//	{"cmd":"subscribe"}                          -> stream of {"snapshot":{...}}
package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"

	"github.com/tjmisko/switchboard/internal/hyprland"
	"github.com/tjmisko/switchboard/internal/proc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/wezterm"
)

type Request struct {
	Cmd      string `json:"cmd"`
	Selector string `json:"selector,omitempty"`

	// hook fields — set when Cmd == "hook"
	Event     string `json:"event,omitempty"`
	PID       int    `json:"pid,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

type Response struct {
	Snapshot *state.Snapshot `json:"snapshot,omitempty"`
	OK       bool            `json:"ok,omitempty"`
	Error    string          `json:"error,omitempty"`
}

type Server struct {
	store      *state.Store
	socketPath string
}

func New(store *state.Store, socketPath string) *Server {
	return &Server{store: store, socketPath: socketPath}
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
	snap := s.store.Snapshot()
	if len(snap.Sessions) == 0 {
		return fmt.Errorf("no sessions")
	}
	target := pickSession(snap.Sessions, selector)
	if target == nil {
		return fmt.Errorf("no session matches %q", selector)
	}
	if target.Hyprland == nil || target.Hyprland.Address == "" {
		return fmt.Errorf("session has no hyprland address yet")
	}
	if err := hyprland.FocusWindow(ctx, target.Hyprland.Address); err != nil {
		return fmt.Errorf("hyprland focus: %w", err)
	}
	if target.Wezterm != nil {
		if err := wezterm.ActivatePane(ctx, target.Wezterm.MuxSocket, target.Wezterm.PaneID); err != nil {
			return fmt.Errorf("wezterm activate: %w", err)
		}
	}
	return nil
}

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
	if n, err := strconv.Atoi(selector); err == nil {
		for i := range sessions {
			if sessions[i].PID == n {
				return &sessions[i]
			}
		}
		if n >= 0 && n < len(sessions) {
			return &sessions[n]
		}
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
	if status == "" && req.SessionID == "" {
		return
	}
	s.store.Apply(func(m map[int]*state.Session) {
		pid := findTrackedAncestor(m, req.PID)
		if pid == 0 {
			return
		}
		sess := m[pid]
		if sess.Claude == nil {
			sess.Claude = &state.ClaudeInfo{}
		}
		if status != "" {
			sess.Claude.Status = status
		}
		if req.SessionID != "" && sess.Claude.SessionID == "" {
			sess.Claude.SessionID = req.SessionID
		}
	})
}

// findTrackedAncestor walks up the /proc ppid chain starting at pid, returning
// the first PID that's a tracked session. Bounded depth keeps us out of
// trouble on weird /proc states.
func findTrackedAncestor(m map[int]*state.Session, pid int) int {
	for depth := 0; pid > 1 && depth < 20; depth++ {
		if _, ok := m[pid]; ok {
			return pid
		}
		info, err := proc.Read(pid)
		if err != nil || info.PPID == 0 {
			return 0
		}
		pid = info.PPID
	}
	return 0
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

func (c *Client) Close() error                 { return c.conn.Close() }
func (c *Client) Send(req Request) error       { return c.enc.Encode(req) }
func (c *Client) Recv(resp *Response) error    { return c.dec.Decode(resp) }
