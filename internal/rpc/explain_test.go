package rpc

import (
	"errors"
	"testing"

	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statusexplain"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/wm"
)

func explainRoundTrip(t *testing.T, server *Server, req Request) Response {
	t.Helper()
	_, enc, dec := pipeServer(t, server)
	if err := enc.Encode(req); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := dec.Decode(&response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestExplainShouldReturnTheExplainersDecisionWhenAPidIsGiven(t *testing.T) {
	server := New(state.New(""), "", terminal.NewNone(), wm.NewNone())
	var gotHost string
	var gotPID int
	server.SetStatusExplainer(func(hostname string, pid int) (statusexplain.Decision, error) {
		gotHost, gotPID = hostname, pid
		return statusexplain.Decision{
			Root:   statusexplain.Root{PID: pid, Provider: "claude"},
			Choice: statusexplain.Choice{Status: "idle", EvidenceKind: statusexplain.EvidenceHook, Reason: statusexplain.ReasonGraphAuthority},
		}, nil
	})
	response := explainRoundTrip(t, server, Request{Cmd: "explain", PID: 77, Hostname: "box-a"})
	if response.Error != "" || !response.OK || response.Explanation == nil {
		t.Fatalf("response = %+v", response)
	}
	if gotHost != "box-a" || gotPID != 77 {
		t.Fatalf("explainer got host %q pid %d", gotHost, gotPID)
	}
	if response.Explanation.Root.PID != 77 || response.Explanation.Reason != statusexplain.ReasonGraphAuthority {
		t.Fatalf("explanation = %+v", *response.Explanation)
	}
}

func TestExplainShouldReportAnErrorWhenUnconfiguredPidlessOrRefused(t *testing.T) {
	unconfigured := New(state.New(""), "", terminal.NewNone(), wm.NewNone())
	if response := explainRoundTrip(t, unconfigured, Request{Cmd: "explain", PID: 1}); response.Error == "" || response.Explanation != nil {
		t.Fatalf("unconfigured response = %+v", response)
	}

	server := New(state.New(""), "", terminal.NewNone(), wm.NewNone())
	called := false
	server.SetStatusExplainer(func(string, int) (statusexplain.Decision, error) {
		called = true
		return statusexplain.Decision{}, errors.New("explain is local only")
	})
	if response := explainRoundTrip(t, server, Request{Cmd: "explain"}); response.Error == "" || called {
		t.Fatalf("pidless response = %+v called = %v", response, called)
	}
	if response := explainRoundTrip(t, server, Request{Cmd: "explain", PID: 1, Hostname: "elsewhere"}); response.Error != "explain is local only" || response.Explanation != nil {
		t.Fatalf("refused response = %+v", response)
	}
}
