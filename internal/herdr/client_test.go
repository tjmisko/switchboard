package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"testing"
)

func TestDecodeResponseShouldSurfaceTheServersErrorCode(t *testing.T) {
	err := DecodeResponse([]byte(`{"id":"x","error":{"code":"not_found","message":"pane not found"}}`), "pane.focus", nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "not_found" {
		t.Fatalf("err = %v, want an APIError with code not_found", err)
	}
	if err := DecodeResponse([]byte(`{"id":"x"}`), "pane.focus", nil); err == nil {
		t.Fatal("err = nil, want an error for a response with neither result nor error")
	}
}

// The real transport: one newline-terminated request per connection, one line
// back.
func TestCallShouldRoundTripOneRequestWhenTheServerAnswersOnALine(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "herdr.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan map[string]any, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		line, _ := bufio.NewReader(conn).ReadBytes('\n')
		var req map[string]any
		_ = json.Unmarshal(line, &req)
		got <- req
		_, _ = conn.Write([]byte(`{"id":"switchboard","result":{"type":"pane_list","panes":[{"pane_id":"w1:p1","terminal_id":"term_1","agent":"claude","agent_status":"idle"}]}}` + "\n"))
	}()

	panes, err := ListPanes(context.Background(), Call, socket)
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	if len(panes) != 1 || panes[0].PaneID != "w1:p1" || panes[0].AgentName() != "claude" || panes[0].AgentStatus != StatusIdle {
		t.Fatalf("panes = %+v, want w1:p1 running claude, idle", panes)
	}
	if req := <-got; req["method"] != "pane.list" {
		t.Fatalf("request = %v, want method pane.list", req)
	}
}

func TestPaneShouldReportNoAgentWhenHerdrSendsNull(t *testing.T) {
	var p Pane
	if err := json.Unmarshal([]byte(`{"pane_id":"w1:p1","agent":null,"agent_status":"unknown"}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.AgentName() != "" {
		t.Fatalf("AgentName = %q, want empty", p.AgentName())
	}
}
