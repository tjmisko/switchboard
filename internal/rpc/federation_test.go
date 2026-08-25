package rpc

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/wm"
)

type fakeSnapshotView struct {
	mu       sync.RWMutex
	snapshot state.Snapshot
	updates  chan state.Snapshot
}

func (v *fakeSnapshotView) Snapshot() state.Snapshot {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.snapshot
}
func (v *fakeSnapshotView) replace(snapshot state.Snapshot) {
	v.mu.Lock()
	v.snapshot = snapshot
	v.mu.Unlock()
	v.updates <- snapshot
}
func (v *fakeSnapshotView) Subscribe() (<-chan state.Snapshot, func()) {
	return v.updates, func() {}
}

func pipeServer(t *testing.T, server *Server) (net.Conn, *json.Encoder, *json.Decoder) {
	t.Helper()
	client, daemon := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = client.Close()
	})
	go server.ServeConnection(ctx, daemon)
	return client, json.NewEncoder(client), json.NewDecoder(client)
}

func TestAggregateRPCStaysSeparateFromLocalStream(t *testing.T) {
	store := state.New("")
	store.Apply(func(sessions map[int]*state.Session) {
		sessions[1] = &state.Session{PID: 1, StartedAt: time.Unix(1, 0)}
	})
	view := &fakeSnapshotView{snapshot: state.Snapshot{Sessions: []state.Session{{
		PID: 2, Hostname: "remote", StartedAt: time.Unix(2, 0),
	}}}, updates: make(chan state.Snapshot, 1)}
	server := New(store, "", terminal.NewNone(), wm.NewNone())
	server.SetFederation(view, nil)
	_, enc, dec := pipeServer(t, server)

	for _, test := range []struct {
		cmd string
		pid int
	}{{"list", 1}, {"list-all", 2}} {
		if err := enc.Encode(Request{Cmd: test.cmd}); err != nil {
			t.Fatal(err)
		}
		var response Response
		if err := dec.Decode(&response); err != nil {
			t.Fatal(err)
		}
		if response.Snapshot == nil || len(response.Snapshot.Sessions) != 1 || response.Snapshot.Sessions[0].PID != test.pid {
			t.Fatalf("%s response = %+v", test.cmd, response.Snapshot)
		}
	}
}

func TestSubscribeAllUsesAggregateUpdates(t *testing.T) {
	view := &fakeSnapshotView{
		snapshot: state.Snapshot{Sessions: []state.Session{{PID: 2, Hostname: "remote", StartedAt: time.Unix(2, 0)}}},
		updates:  make(chan state.Snapshot, 1),
	}
	server := New(state.New(""), "", terminal.NewNone(), wm.NewNone())
	server.SetFederation(view, nil)
	_, enc, dec := pipeServer(t, server)
	if err := enc.Encode(Request{Cmd: "subscribe-all"}); err != nil {
		t.Fatal(err)
	}
	var first Response
	if err := dec.Decode(&first); err != nil {
		t.Fatal(err)
	}
	if first.Snapshot == nil || first.Snapshot.Sessions[0].PID != 2 {
		t.Fatalf("initial response = %+v", first.Snapshot)
	}
	view.replace(state.Snapshot{Sessions: []state.Session{{PID: 3, Hostname: "remote", StartedAt: time.Unix(3, 0)}}})
	var next Response
	if err := dec.Decode(&next); err != nil {
		t.Fatal(err)
	}
	if next.Snapshot == nil || next.Snapshot.Sessions[0].PID != 3 {
		t.Fatalf("update = %+v", next.Snapshot)
	}
}

func TestSubscribeAllTreatsQueuedValuesAsNotifications(t *testing.T) {
	stale := state.Snapshot{Sessions: []state.Session{{
		PID: 2, Hostname: "remote", StartedAt: time.Unix(2, 0),
	}}}
	view := &fakeSnapshotView{
		// Model a remote disconnect landing after an old live publication was
		// queued but before subscribe-all reads its initial current snapshot.
		snapshot: state.Snapshot{Sessions: []state.Session{}},
		updates:  make(chan state.Snapshot, 1),
	}
	view.updates <- stale

	server := New(state.New(""), "", terminal.NewNone(), wm.NewNone())
	server.SetFederation(view, nil)
	_, enc, dec := pipeServer(t, server)
	if err := enc.Encode(Request{Cmd: "subscribe-all"}); err != nil {
		t.Fatal(err)
	}
	for frame := 0; frame < 2; frame++ {
		var response Response
		if err := dec.Decode(&response); err != nil {
			t.Fatal(err)
		}
		if response.Snapshot == nil || len(response.Snapshot.Sessions) != 0 {
			t.Fatalf("frame %d replayed stale queued live state: %+v", frame, response.Snapshot)
		}
	}
}

func TestFederatedActionHandlersReceiveExactInputs(t *testing.T) {
	server := New(state.New(""), "", terminal.NewNone(), wm.NewNone())
	startedAt := time.Unix(20, 0).UTC()
	var gotFocus Request
	var gotBind Request
	var gotActive Request
	server.SetFederation(nil, func(_ context.Context, host string, pid int, started time.Time) error {
		gotFocus = Request{Hostname: host, PID: pid, StartedAt: started}
		return nil
	})
	server.SetPaneBinding(
		func(_ context.Context, binding string, guiPID, windowID, paneID int) error {
			gotBind = Request{Binding: binding, GUIPID: guiPID, WindowID: windowID, PaneID: paneID}
			return nil
		},
		func(_ context.Context, guiPID, windowID, paneID int, active bool) error {
			gotActive = Request{GUIPID: guiPID, WindowID: windowID, PaneID: paneID, WindowFocused: active}
			return nil
		},
		func(context.Context) error { return nil },
	)
	_, enc, dec := pipeServer(t, server)
	requests := []Request{
		{Cmd: "focus-session", Hostname: "remote", PID: 9, StartedAt: startedAt},
		{Cmd: "pane-bind", Binding: `{"v":1}`, GUIPID: 10, WindowID: 11, PaneID: 12},
		{Cmd: "pane-state", GUIPID: 10, WindowID: 11, PaneID: 12, WindowFocused: true},
		{Cmd: "announce-bindings"},
	}
	for _, request := range requests {
		if err := enc.Encode(request); err != nil {
			t.Fatal(err)
		}
		var response Response
		if err := dec.Decode(&response); err != nil {
			t.Fatal(err)
		}
		if !response.OK || response.Error != "" {
			t.Fatalf("%s response = %+v", request.Cmd, response)
		}
	}
	if gotFocus.Hostname != "remote" || gotFocus.PID != 9 || !gotFocus.StartedAt.Equal(startedAt) {
		t.Fatalf("focus inputs = %+v", gotFocus)
	}
	if gotBind.Binding != `{"v":1}` || gotBind.GUIPID != 10 || gotBind.WindowID != 11 || gotBind.PaneID != 12 {
		t.Fatalf("bind inputs = %+v", gotBind)
	}
	if !gotActive.WindowFocused || gotActive.PaneID != 12 {
		t.Fatalf("active inputs = %+v", gotActive)
	}
}
