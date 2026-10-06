package state

import (
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/statusexplain"
)

// statusLifetime is the process lifetime the resolver tests' sessions run
// in: a fixed instant before every reading, as discovery stamps one.
var statusLifetime = herdrT0.Add(-time.Hour)

// statusGraph is a one-root graph from source at at reducing to status, fresh
// for lease. Delegating adds one working child.
func statusGraph(t *testing.T, provider agentgraph.ProviderKind, rootID string, source agentgraph.SourceKind, status string, at time.Time, lease time.Duration) *AgentGraph {
	t.Helper()
	root := agentgraph.Node{ID: rootID, UpdatedAt: at}
	nodes := []agentgraph.Node{}
	switch status {
	case StatusWorking:
		root.Runtime = agentgraph.RuntimeActive
	case StatusIdle:
		root.Runtime = agentgraph.RuntimeIdle
	case StatusPermission:
		root.Runtime, root.Attention = agentgraph.RuntimeActive, agentgraph.AttentionApproval
	case StatusDelegating:
		root.Runtime = agentgraph.RuntimeIdle
		nodes = append(nodes, agentgraph.Node{ID: rootID + "-child", ParentID: rootID, Runtime: agentgraph.RuntimeActive, UpdatedAt: at})
	}
	graph, err := ProjectAgentGraph(agentgraph.Observation{
		Provider: provider, RootID: rootID, Source: source, ObservedAt: at, FreshUntil: at.Add(lease),
		Nodes: append([]agentgraph.Node{root}, nodes...),
	}, nil, at)
	if err != nil {
		t.Fatal(err)
	}
	if graph.Summary.Status != status {
		t.Fatalf("graph for %q reduced to %q", status, graph.Summary.Status)
	}
	return graph
}

func claudeStatusGraph(t *testing.T, source agentgraph.SourceKind, status string, at time.Time, lease time.Duration) *AgentGraph {
	return statusGraph(t, agentgraph.ProviderClaude, "sess-1", source, status, at, lease)
}

func codexStatusGraph(t *testing.T, source agentgraph.SourceKind, status string, at time.Time, lease time.Duration) *AgentGraph {
	return statusGraph(t, agentgraph.ProviderCodex, "thread-1", source, status, at, lease)
}

func codexReading(status string, live bool, since time.Time) HerdrReading {
	r := reading(status, live, since)
	r.Agent = AgentKindCodex
	return r
}

func claudeSession() *Session {
	return &Session{PID: 10, StartedAt: statusLifetime, Agent: AgentKindClaude, Claude: &AgentInfo{}}
}

func codexSession() *Session {
	return &Session{PID: 11, StartedAt: statusLifetime, Agent: AgentKindCodex, Codex: &AgentInfo{}}
}

func TestLandAgentGraphShouldKeepTheLatestGraphOfEachKindWhenSeveralLand(t *testing.T) {
	s := claudeSession()
	snapshot := claudeStatusGraph(t, agentgraph.SourceClaudeTranscript, StatusWorking, herdrT0, time.Minute)
	hook := claudeStatusGraph(t, agentgraph.SourceHook, StatusPermission, herdrT0.Add(time.Second), time.Minute)
	s.LandAgentGraph(snapshot, GraphLanding{Kind: GraphSnapshot}, herdrT0)
	s.LandAgentGraph(hook, GraphLanding{Kind: GraphHookEvent}, herdrT0.Add(time.Second))
	if len(s.evidence) != 2 {
		t.Fatalf("evidence holds %d kinds, want the snapshot and the hook", len(s.evidence))
	}
	// The hook's request is newer than the fresh snapshot and nothing
	// authorized resolved it: it decides, and the graph shown is its own.
	if s.Claude.Status != StatusPermission || s.AgentGraph.Source != agentgraph.SourceHook {
		t.Fatalf("published %q from %s, want the hook's permission", s.Claude.Status, s.AgentGraph.Source)
	}
	// A newer snapshot with no request resolves it.
	at := herdrT0.Add(2 * time.Second)
	s.LandAgentGraph(claudeStatusGraph(t, agentgraph.SourceClaudeTranscript, StatusWorking, at, time.Minute), GraphLanding{Kind: GraphSnapshot}, at)
	if s.Claude.Status != StatusWorking || s.AgentGraph.Source != agentgraph.SourceClaudeTranscript {
		t.Fatalf("published %q from %s, want the newer snapshot's working", s.Claude.Status, s.AgentGraph.Source)
	}
}

func TestLandAgentGraphShouldRefuseAGraphWhenItIsOlderThanItsKindsHeldGraph(t *testing.T) {
	s := codexSession()
	newer := codexStatusGraph(t, agentgraph.SourceCodexAppServer, StatusWorking, herdrT0.Add(time.Second), time.Minute)
	older := codexStatusGraph(t, agentgraph.SourceCodexAppServer, StatusIdle, herdrT0, time.Minute)
	s.LandAgentGraph(newer, GraphLanding{Kind: GraphSnapshot}, herdrT0.Add(time.Second))
	if reason := s.LandAgentGraph(older, GraphLanding{Kind: GraphSnapshot}, herdrT0.Add(time.Second)); reason != statusexplain.ReasonOlderThanCurrent {
		t.Fatalf("older snapshot refused for %q, want older_than_current", reason)
	}
	if s.Codex.Status != StatusWorking {
		t.Fatalf("older snapshot repainted %q", s.Codex.Status)
	}
	// Another kind's older graph is evidence of its own: it lands, and the
	// resolver weighs it (Codex orders its evidence by event time).
	if reason := s.LandAgentGraph(codexStatusGraph(t, agentgraph.SourceHook, StatusIdle, herdrT0, time.Minute), GraphLanding{Kind: GraphHookEvent}, herdrT0.Add(time.Second)); reason != "" {
		t.Fatalf("older hook refused for %q", reason)
	}
	if s.Codex.Status != StatusWorking || s.AgentGraph.Source != agentgraph.SourceCodexAppServer {
		t.Fatalf("older hook decided %q from %s", s.Codex.Status, s.AgentGraph.Source)
	}
}

func TestLandAgentGraphShouldForgetAnotherConversationsEvidenceWhenTheRootRotates(t *testing.T) {
	s := codexSession()
	s.LandAgentGraph(codexStatusGraph(t, agentgraph.SourceCodexAppServer, StatusPermission, herdrT0, time.Hour), GraphLanding{Kind: GraphSnapshot}, herdrT0)
	rotated := statusGraph(t, agentgraph.ProviderCodex, "thread-2", agentgraph.SourceHook, StatusWorking, herdrT0.Add(time.Second), time.Hour)
	s.LandAgentGraph(rotated, GraphLanding{Kind: GraphHookEvent}, herdrT0.Add(time.Second))
	if len(s.evidence) != 1 || s.Codex.SessionID != "thread-2" {
		t.Fatalf("evidence %v for %q, want only the new conversation's", s.evidence, s.Codex.SessionID)
	}
	if s.Codex.Status != StatusWorking {
		t.Fatalf("the old conversation's request held the new one at %q", s.Codex.Status)
	}
}

func TestLandAgentGraphShouldLetTheHookResolveALatchedRequestWhenASnapshotCarriedIt(t *testing.T) {
	s := codexSession()
	s.SetHerdr(codexReading(HerdrWorking, true, herdrT0), herdrT0)
	// The app-server sample carries the question the hooks hold open.
	at := herdrT0.Add(time.Second)
	s.LandAgentGraph(codexStatusGraph(t, agentgraph.SourceCodexAppServer, StatusPermission, at, time.Minute), GraphLanding{Kind: GraphSnapshot, HookLatched: true}, at)
	if s.Codex.Status != StatusPermission {
		t.Fatalf("latched request under herdr working published %q", s.Codex.Status)
	}
	// The hook that answered it lands newer, with no request.
	answered := at.Add(time.Second)
	s.LandAgentGraph(codexStatusGraph(t, agentgraph.SourceHook, StatusWorking, answered, time.Hour), GraphLanding{Kind: GraphHookEvent}, answered)
	if s.Codex.Status != StatusWorking {
		t.Fatalf("the hook's answer left %q", s.Codex.Status)
	}
}

func TestLandAgentGraphShouldHoldTheAppServersOwnRequestWhenOnlyAHookSaysOtherwise(t *testing.T) {
	s := codexSession()
	at := herdrT0.Add(time.Second)
	s.LandAgentGraph(codexStatusGraph(t, agentgraph.SourceCodexAppServer, StatusPermission, at, time.Minute), GraphLanding{Kind: GraphSnapshot}, at)
	hookAt := at.Add(time.Second)
	s.LandAgentGraph(codexStatusGraph(t, agentgraph.SourceHook, StatusWorking, hookAt, time.Hour), GraphLanding{Kind: GraphHookEvent}, hookAt)
	if s.Codex.Status != StatusPermission || s.AgentGraph.Source != agentgraph.SourceCodexAppServer {
		t.Fatalf("hook over the app-server's request published %q from %s, want it held", s.Codex.Status, s.AgentGraph.Source)
	}
	if got := s.ExplainStatus(hookAt).Reason; got != statusexplain.ReasonAttentionHeld {
		t.Fatalf("explained as %q, want attention_held", got)
	}
	// Past the sample's deadline the request is no longer held.
	expired := at.Add(time.Minute)
	s.ReprojectStatus(expired)
	if s.Codex.Status != StatusWorking {
		t.Fatalf("expired request still published %q", s.Codex.Status)
	}
}

func TestLandAgentGraphShouldAmendThePublishedGraphWhenAChildHookEdgeLands(t *testing.T) {
	s := codexSession()
	base := codexStatusGraph(t, agentgraph.SourceCodexAppServer, StatusIdle, herdrT0, time.Minute)
	s.LandAgentGraph(base, GraphLanding{Kind: GraphSnapshot}, herdrT0)
	edge := statusGraph(t, agentgraph.ProviderCodex, "thread-1", agentgraph.SourceCodexAppServer, StatusDelegating, herdrT0, time.Minute)
	s.LandAgentGraph(edge, GraphLanding{Kind: GraphHookEdge}, herdrT0.Add(time.Second))
	if s.Codex.Status != StatusDelegating || len(s.AgentGraph.Nodes) != 2 {
		t.Fatalf("child edge published %q with %d nodes, want delegating with the child shown", s.Codex.Status, len(s.AgentGraph.Nodes))
	}
	if s.DisplayGraphKind() != GraphSnapshot {
		t.Fatalf("published graph kind %d, want the snapshot it amended", s.DisplayGraphKind())
	}
}

func TestInheritStatusEvidenceShouldKeepTheResolversEvidenceWhenDiscoveryRebuildsTheSession(t *testing.T) {
	prior := claudeSession()
	prior.LandAgentGraph(claudeStatusGraph(t, agentgraph.SourceHook, StatusPermission, herdrT0, time.Hour), GraphLanding{Kind: GraphHookEvent}, herdrT0)
	next := &Session{PID: prior.PID, StartedAt: prior.StartedAt, Agent: prior.Agent, Claude: prior.Claude, AgentGraph: prior.AgentGraph}
	next.InheritStatusEvidence(prior)
	at := herdrT0.Add(time.Second)
	next.SetHerdr(reading(HerdrWorking, true, at), at)
	if next.Claude.Status != StatusPermission {
		t.Fatalf("rebuilt session published %q under herdr working, want the hook's request held", next.Claude.Status)
	}
}

func TestLandAgentGraphShouldNotLetASupersededHookDecideWhenTheNewerSampleLapses(t *testing.T) {
	s := codexSession()
	s.LandAgentGraph(codexStatusGraph(t, agentgraph.SourceHook, StatusIdle, herdrT0, 7*24*time.Hour), GraphLanding{Kind: GraphHookEvent}, herdrT0)
	at := herdrT0.Add(time.Second)
	s.LandAgentGraph(codexStatusGraph(t, agentgraph.SourceCodexAppServer, StatusDelegating, at, time.Second), GraphLanding{Kind: GraphSnapshot}, at)
	if s.Codex.Status != StatusDelegating {
		t.Fatalf("newer sample published %q", s.Codex.Status)
	}
	if _, after := s.ReprojectStatus(at.Add(time.Minute)); after != "" {
		t.Fatalf("after the sample lapsed the superseded hook published %q, want unknown", after)
	}
	// A newer hook is current evidence again.
	hookAt := at.Add(2 * time.Minute)
	s.LandAgentGraph(codexStatusGraph(t, agentgraph.SourceHook, StatusWorking, hookAt, time.Hour), GraphLanding{Kind: GraphHookEvent}, hookAt)
	if s.Codex.Status != StatusWorking {
		t.Fatalf("a newer hook published %q, want working", s.Codex.Status)
	}
}

func TestLandAgentGraphShouldStayDelegatingWhenTheRolloutIdleCorrectionLandsOverAGraphWithWorkingChildren(t *testing.T) {
	s := codexSession()
	s.LandAgentGraph(codexStatusGraph(t, agentgraph.SourceHook, StatusDelegating, herdrT0, time.Hour), GraphLanding{Kind: GraphHookEvent}, herdrT0)
	// The transcript poll corrects the published graph's root to idle and
	// keeps its children, as codex_transcript_poll builds it.
	at := herdrT0.Add(95 * time.Second)
	s.LandAgentGraph(codexStatusGraph(t, agentgraph.SourceCodexRollout, StatusDelegating, at, 30*time.Second), GraphLanding{Kind: GraphTranscriptTail}, at)
	if s.Codex.Status != StatusDelegating {
		t.Fatalf("idle root with a working child published %q, want delegating", s.Codex.Status)
	}
	if s.AgentGraph.Summary.Status != s.Codex.Status {
		t.Fatalf("graph summary %q disagrees with published %q", s.AgentGraph.Summary.Status, s.Codex.Status)
	}
}

func TestLandAgentGraphShouldPublishIdleWhenTheRolloutIdleCorrectionCarriesNoWorkingChildren(t *testing.T) {
	s := codexSession()
	s.LandAgentGraph(codexStatusGraph(t, agentgraph.SourceHook, StatusDelegating, herdrT0, time.Hour), GraphLanding{Kind: GraphHookEvent}, herdrT0)
	// A newer correction whose graph no longer holds a working child is the
	// newest report of the root's descendants: the older hook's child does not
	// come back.
	at := herdrT0.Add(95 * time.Second)
	s.LandAgentGraph(codexStatusGraph(t, agentgraph.SourceCodexRollout, StatusIdle, at, 30*time.Second), GraphLanding{Kind: GraphTranscriptTail}, at)
	if s.Codex.Status != StatusIdle {
		t.Fatalf("idle correction with no working child published %q, want idle", s.Codex.Status)
	}
}

func TestProjectShouldDateAStatusHerdrDecidesFromHerdrsOwnStartWhenARequestResolvesUnderIt(t *testing.T) {
	s := claudeSession()
	s.SetHerdr(reading(HerdrWorking, true, herdrT0), herdrT0)
	asked := herdrT0.Add(time.Minute)
	s.SetAgentGraph(claudeStatusGraph(t, agentgraph.SourceClaudeTranscript, StatusPermission, asked, time.Hour), asked)
	if s.Claude.Status != StatusPermission {
		t.Fatalf("request under herdr working published %q", s.Claude.Status)
	}
	// The request resolves; herdr, working since herdrT0, decides again.
	answered := asked.Add(time.Minute)
	s.SetAgentGraph(claudeStatusGraph(t, agentgraph.SourceClaudeTranscript, StatusIdle, answered, time.Hour), answered)
	if s.Claude.Status != StatusWorking || !s.Claude.StatusSince.Equal(herdrT0) {
		t.Fatalf("published %q since %v, want herdr's working since %v", s.Claude.Status, s.Claude.StatusSince, herdrT0)
	}
}
