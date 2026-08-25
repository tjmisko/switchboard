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

func TestLocalSubscribeTreatsQueuedBroadcastsAsNotifications(t *testing.T) {
	store := state.New("")
	updates, cancelUpdates := store.Subscribe()
	defer cancelUpdates()

	// Queue A, then advance the authoritative Store to B before the stream reads
	// its initial snapshot. A raw-forwarding subscriber would emit B, A, B.
	store.Apply(func(sessions map[int]*state.Session) {
		sessions[1] = &state.Session{PID: 1, CWD: "/stale-a", StartedAt: time.Unix(1, 0)}
	})
	store.Apply(func(sessions map[int]*state.Session) {
		delete(sessions, 1)
		sessions[2] = &state.Session{PID: 2, CWD: "/current-b", StartedAt: time.Unix(2, 0)}
	})

	server := New(store, "", terminal.NewNone(), wm.NewNone())
	client, daemon := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer daemon.Close()
		server.streamLocalSnapshots(ctx, daemon, json.NewEncoder(daemon), updates)
	}()

	decoder := json.NewDecoder(client)
	for frame := 0; frame < 3; frame++ {
		var response Response
		if err := decoder.Decode(&response); err != nil {
			t.Fatalf("decode frame %d: %v", frame, err)
		}
		if response.Snapshot == nil || len(response.Snapshot.Sessions) != 1 ||
			response.Snapshot.Sessions[0].PID != 2 {
			t.Fatalf("frame %d rolled back from B: %+v", frame, response.Snapshot)
		}
	}
	_ = client.Close()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("local snapshot stream did not stop")
	}
}

// End to end over the real socket: every subscriber must receive the current
// documented wire document, and a brand-new connection must still get a full
// snapshot immediately before any mutation happens. UpdatedAt is stamped by
// each current Snapshot read, so separate subscribers need not receive
// byte-identical timestamps.
func TestSubscribe_deliversCurrentWireFrameToEverySubscriber(t *testing.T) {
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

	for i, r := range readers {
		frame := nextFrame(t, r)
		// Re-encoding through Response must reproduce each frame byte-for-byte:
		// notification/re-read changes ordering, not the public JSON shape.
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
