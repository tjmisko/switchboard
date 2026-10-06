package state

import (
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/statusexplain"
)

func TestResetAgentEvidenceShouldDropTheOldConversationsGraphBeforeItsDeadlineWhenTheBindingMoves(t *testing.T) {
	s := codexSession()
	s.LandAgentGraph(codexStatusGraph(t, agentgraph.SourceCodexAppServer, StatusWorking, herdrT0, time.Hour), GraphLanding{Kind: GraphSnapshot}, herdrT0)
	at := herdrT0.Add(time.Second)
	before, after, dropped := s.ResetAgentEvidence("thread-2", at)
	if !dropped || before != StatusWorking || after != "" {
		t.Fatalf("reset = %q -> %q dropped=%t, want working -> unknown with the old graph dropped", before, after, dropped)
	}
	if s.AgentGraph != nil || len(s.evidence) != 0 || s.Codex.SessionID != "thread-2" {
		t.Fatalf("after reset graph=%v evidence=%d session=%q, want nothing held and the new binding", s.AgentGraph, len(s.evidence), s.Codex.SessionID)
	}
	d := s.ExplainStatus(at)
	if d.Root.SessionID != "thread-2" || d.Status != "" || d.Reason != statusexplain.ReasonObservationPending {
		t.Fatalf("decision after reset = %+v, want unknown for the new binding with no prior held", d.Choice)
	}
}

func TestResetAgentEvidenceShouldLetHerdrDecideWhenTheOldConversationsRequestIsDropped(t *testing.T) {
	s := codexSession()
	s.SetHerdr(codexReading(HerdrWorking, true, herdrT0), herdrT0)
	s.LandAgentGraph(codexStatusGraph(t, agentgraph.SourceCodexAppServer, StatusPermission, herdrT0, time.Hour), GraphLanding{Kind: GraphSnapshot}, herdrT0)
	if s.Codex.Status != StatusPermission {
		t.Fatalf("setup: published %q, want the app-server's open request", s.Codex.Status)
	}
	if _, after, _ := s.ResetAgentEvidence("thread-2", herdrT0.Add(time.Second)); after != StatusWorking {
		t.Fatalf("after reset = %q, want herdr's working through the resolver", after)
	}
}

func TestResetAgentEvidenceShouldChangeNothingWhenEveryGraphIsAlreadyAboutTheNewBinding(t *testing.T) {
	s := codexSession()
	rotated := statusGraph(t, agentgraph.ProviderCodex, "thread-2", agentgraph.SourceHook, StatusWorking, herdrT0, time.Hour)
	s.LandAgentGraph(rotated, GraphLanding{Kind: GraphHookEvent}, herdrT0)
	before, after, dropped := s.ResetAgentEvidence("thread-2", herdrT0.Add(time.Second))
	if dropped || before != StatusWorking || after != StatusWorking || s.AgentGraph == nil || len(s.evidence) != 1 {
		t.Fatalf("reset = %q -> %q dropped=%t graph=%v, want the new conversation's graph untouched", before, after, dropped, s.AgentGraph)
	}
}

func TestHeldEvidenceShouldReportTheLandedWindowOnlyForTheSameConversationAndKind(t *testing.T) {
	s := claudeSession()
	graph := claudeStatusGraph(t, agentgraph.SourceClaudeTranscript, StatusWorking, herdrT0, time.Minute)
	s.LandAgentGraph(graph, GraphLanding{Kind: GraphSnapshot}, herdrT0)
	observedAt, freshUntil, ok := s.HeldEvidence("sess-1", GraphSnapshot)
	if !ok || !observedAt.Equal(herdrT0) || !freshUntil.Equal(herdrT0.Add(time.Minute)) {
		t.Fatalf("held = [%v, %v) %t, want the landed snapshot's window", observedAt, freshUntil, ok)
	}
	if _, _, ok := s.HeldEvidence("sess-1", GraphHookEvent); ok {
		t.Fatal("a kind never landed reported held evidence")
	}
	if _, _, ok := s.HeldEvidence("sess-2", GraphSnapshot); ok {
		t.Fatal("another conversation reported held evidence")
	}
}
