package state

import (
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
)

// piHookGraph is a hook graph for the bound Pi session, observed at at with
// the given lease.
func piHookGraph(t *testing.T, runtime agentgraph.RuntimeState, attention agentgraph.AttentionState, at time.Time, lease time.Duration) *AgentGraph {
	t.Helper()
	graph, err := ProjectAgentGraph(agentgraph.Observation{
		Provider: agentgraph.ProviderPi, RootID: piSessionID, Source: agentgraph.SourceHook,
		ObservedAt: at, FreshUntil: at.Add(lease),
		Nodes: []agentgraph.Node{{ID: piSessionID, Runtime: runtime, Attention: attention, UpdatedAt: at}},
	}, nil, at)
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

// boundPi is a Pi session in a followed herdr pane that a hook has bound.
func boundPi(t *testing.T, herdrStatus string) *Session {
	t.Helper()
	s := &Session{PID: 20, Agent: AgentKindPi}
	s.SetHerdr(piReading(herdrStatus, herdrT0), herdrT0)
	s.AgentBlock(AgentKindPi).SessionID = piSessionID
	s.ReprojectPi(herdrT0)
	return s
}

func TestPiStatusAuthorityShouldPreferFreshPiHookEvidenceOverAHerdrReading(t *testing.T) {
	s := boundPi(t, HerdrIdle)
	at := herdrT0.Add(time.Second)
	if before, after := s.SetPiHookGraph(piHookGraph(t, agentgraph.RuntimeActive, agentgraph.AttentionNone, at, time.Minute), at); before != StatusIdle || after != StatusWorking {
		t.Fatalf("hook landing = %q → %q, want idle → working", before, after)
	}
	later := at.Add(time.Second)
	_, after := s.SetHerdr(piReading(HerdrDone, later), later)
	if after != StatusWorking || s.Pi.Status != StatusWorking {
		t.Fatalf("herdr done over a fresh working hook published %q", after)
	}
	if s.AgentGraph.Source != agentgraph.SourceHook {
		t.Fatalf("herdr replaced fresh hook evidence: graph source %q", s.AgentGraph.Source)
	}
	if s.Herdr.Status != HerdrDone {
		t.Fatalf("herdr's reading was not kept for the fallback: %q", s.Herdr.Status)
	}
}

func TestPiStatusAuthorityShouldFallBackToHerdrWhenNoHookHasArrived(t *testing.T) {
	s := boundPi(t, HerdrIdle)
	at := herdrT0.Add(time.Second)
	if _, after := s.SetHerdr(piReading(HerdrWorking, at), at); after != StatusWorking {
		t.Fatalf("bound block with no hook evidence published %q, want herdr's working", after)
	}

	unbound := &Session{PID: 21, Agent: AgentKindPi}
	if _, after := unbound.SetHerdr(piReading(HerdrBlocked, at), at); after != StatusPermission || unbound.Pi != nil {
		t.Fatalf("unbound pi published %q (block %+v), want herdr's permission through its graph", after, unbound.Pi)
	}
}

func TestPiStatusAuthorityShouldFallBackToHerdrWhenTheHookLeaseLapses(t *testing.T) {
	s := boundPi(t, HerdrIdle)
	at := herdrT0.Add(time.Second)
	s.SetPiHookGraph(piHookGraph(t, agentgraph.RuntimeActive, agentgraph.AttentionNone, at, time.Minute), at)
	if before, after := s.ReprojectPi(at.Add(time.Minute - time.Millisecond)); before != after {
		t.Fatalf("reprojected inside the lease: %q → %q", before, after)
	}
	if _, after := s.ReprojectPi(at.Add(time.Minute)); after != StatusIdle {
		t.Fatalf("lapsed hook with live herdr idle published %q", after)
	}
	s.Herdr.Live = false
	if _, after := s.ReprojectPi(at.Add(time.Minute)); after != "" {
		t.Fatalf("lapsed hook with no live herdr published %q, want unknown", after)
	}
}

func TestPiStatusAuthorityShouldNotLetAHerdrWorkingReadingClearAHookHeldRed(t *testing.T) {
	s := boundPi(t, HerdrBlocked)
	at := herdrT0.Add(time.Second)
	s.SetPiHookGraph(piHookGraph(t, agentgraph.RuntimeActive, agentgraph.AttentionUserInput, at, 24*time.Hour), at)
	later := at.Add(time.Minute)
	if _, after := s.SetHerdr(piReading(HerdrWorking, later), later); after != StatusPermission {
		t.Fatalf("herdr working cleared a hook-held red: published %q", after)
	}
}

func TestRotatePiSessionShouldResetConversationDisplayStateAndKeepTheStatus(t *testing.T) {
	s := boundPi(t, HerdrWorking)
	s.Pi.Transcript = "/old.jsonl"
	s.DisplayName = &DisplayName{}
	s.RotatePiSession("next", "/next.jsonl")
	if s.Pi.SessionID != "next" || s.Pi.Transcript != "/next.jsonl" || s.DisplayName != nil {
		t.Fatalf("rotation = %+v name=%+v", s.Pi, s.DisplayName)
	}
	if s.Pi.Status != StatusWorking {
		t.Fatalf("rotation dropped the published status: %q", s.Pi.Status)
	}
}
