package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/wm"
)

// wireSnapshot is a deliberately awkward snapshot for the byte-identity checks:
// every optional block populated, and a window title carrying the three
// characters encoding/json escapes ('<', '>', '&'). Those are the ones that would
// expose a double-escape if the pre-encoded body were run through an escaping
// pass a second time on its way into the envelope.
func wireSnapshot() state.Snapshot {
	since := time.Date(2026, 5, 28, 9, 1, 0, 0, time.UTC)
	return state.Snapshot{
		Sessions: []state.Session{{
			PID: 4821, CWD: "/home/u/p", TTY: "/dev/pts/3",
			StartedAt: time.Date(2026, 5, 28, 9, 0, 0, 0, time.UTC),
			Focused:   true, Agent: state.AgentKindClaude, MemTreeBytes: 674234368,
			Wezterm:  &state.WeztermInfo{MuxPID: 4790, PaneID: 12, WindowTitle: "claude <a & b> — switchboard"},
			Hyprland: &state.HyprlandInfo{Address: "0x5640f1a2b3c0", Workspace: "4", WorkspaceID: 4},
			Claude: &state.AgentInfo{
				SessionID: "e0b4b21f", Status: state.StatusWorking,
				StatusSinceWire: &since, InFlightSubagents: 2,
			},
		}},
		UpdatedAt:    time.Date(2026, 5, 28, 9, 5, 30, 0, time.UTC),
		Capabilities: &state.Capabilities{Observe: true, Navigate: true, WM: "hyprland", Terminal: "wezterm"},
	}
}

// encodeFrame renders one response exactly as the server writes it: a single
// json.Encoder line, trailing newline included.
func encodeFrame(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	return buf.Bytes()
}

// The subscribe stream now sends a pre-encoded body through a mirrored envelope
// (rawResponse) instead of re-marshaling the snapshot per subscriber. That is a
// pure performance change, so the frame must stay byte-identical to what
// Response produced — this is the test rpc.snapshotFrame's comment names, and the
// one that fails if rawResponse and Response ever drift apart.
func TestSubscribeFrameMatchesResponseEncoding(t *testing.T) {
	snap := wireSnapshot()
	body, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}

	want := encodeFrame(t, Response{Snapshot: &snap})
	got := encodeFrame(t, snapshotFrame(state.Broadcast{Snapshot: snap, JSON: body}))
	if !bytes.Equal(got, want) {
		t.Errorf("pre-encoded frame differs from Response.\n--- got ---\n%s--- want ---\n%s", got, want)
	}

	// A nil body means the upstream encode failed; the fallback must still produce
	// a valid, identical frame rather than a truncated one.
	fallback := encodeFrame(t, snapshotFrame(state.Broadcast{Snapshot: snap}))
	if !bytes.Equal(fallback, want) {
		t.Errorf("fallback frame differs from Response.\n--- got ---\n%s--- want ---\n%s", fallback, want)
	}
}

// serveTestSocket starts a Server on a throwaway socket and returns its path,
// blocking until the socket accepts connections so a dial cannot lose the race
// with Serve's goroutine.
func serveTestSocket(t *testing.T, store *state.Store) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "s.sock")
	srv := New(store, sock, terminal.NewNone(), wm.NewNone())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.Serve(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			return sock
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("server never started listening")
	return ""
}

// subscriber dials the socket, subscribes, and returns a reader positioned at the
// first streamed frame.
func subscriber(t *testing.T, sock string) *bufio.Reader {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := json.NewEncoder(conn).Encode(Request{Cmd: "subscribe"}); err != nil {
		t.Fatalf("send subscribe: %v", err)
	}
	return bufio.NewReader(conn)
}

// nextFrame reads one newline-delimited response off the stream.
func nextFrame(t *testing.T, r *bufio.Reader) []byte {
	t.Helper()
	line, err := r.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return line
}

// End to end over the real socket: every subscriber must receive the same frame,
// that frame must still be the documented wire document, and — the constraint the
// shared-encoding change must not break — a brand-new connection must still get a
// full snapshot immediately, before any mutation happens.
func TestSubscribe_deliversTheSameWireFrameToEverySubscriber(t *testing.T) {
	store := state.New("")
	store.Apply(func(m map[int]*state.Session) {
		m[4821] = &state.Session{PID: 4821, CWD: "/home/u/p", TTY: "/dev/pts/3", StartedAt: time.Unix(1000, 0)}
	})
	sock := serveTestSocket(t, store)

	const subscribers = 3
	readers := make([]*bufio.Reader, 0, subscribers)
	for i := 0; i < subscribers; i++ {
		r := subscriber(t, sock)
		// The on-connect snapshot: it comes from store.Snapshot(), not the broadcast
		// path, so it must arrive even though nothing has changed since the store was
		// seeded.
		var resp Response
		if err := json.Unmarshal(nextFrame(t, r), &resp); err != nil {
			t.Fatalf("subscriber %d: decode connect frame: %v", i, err)
		}
		if resp.Snapshot == nil || len(resp.Snapshot.Sessions) != 1 {
			t.Fatalf("subscriber %d: connect frame = %+v, want the current snapshot", i, resp.Snapshot)
		}
		readers = append(readers, r)
	}

	store.Apply(func(m map[int]*state.Session) { m[4821].Focused = true })

	var first []byte
	for i, r := range readers {
		frame := nextFrame(t, r)
		if i == 0 {
			first = frame
		} else if !bytes.Equal(frame, first) {
			t.Errorf("subscriber %d received a different frame.\n--- got ---\n%s--- want ---\n%s", i, frame, first)
		}

		// Re-encoding through Response must reproduce the frame byte-for-byte: the
		// shared buffer is the same wire document rpc has always written.
		var resp Response
		if err := json.Unmarshal(frame, &resp); err != nil {
			t.Fatalf("subscriber %d: decode frame: %v", i, err)
		}
		if resp.Snapshot == nil || !resp.Snapshot.Sessions[0].Focused {
			t.Fatalf("subscriber %d: frame did not carry the focus change: %s", i, frame)
		}
		if want := encodeFrame(t, resp); !bytes.Equal(frame, want) {
			t.Errorf("subscriber %d frame is not the canonical encoding.\n--- got ---\n%s--- want ---\n%s", i, frame, want)
		}
	}
}
