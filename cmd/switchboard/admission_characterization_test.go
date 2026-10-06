package main

import (
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statusexplain"
)

// Characterization of graph admission (#95, work unit 6), as the status
// resolver now decides it (#96). Admission no longer ranks one graph against
// another: each landing is kept as the latest evidence of its kind, and the
// resolver selects among them. Each row lands a current graph, then a
// candidate, and asserts whether the candidate decides (and is the graph
// shown). Rows the resolver changed on purpose say so with "#96:" and the
// acceptance criterion that changes them.

var landingLifetime = explainT0.Add(-time.Hour)

func admissionCandidate(provider agentgraph.ProviderKind, rootID string, source agentgraph.SourceKind, at time.Time, lease time.Duration) agentgraph.Observation {
	return agentgraph.Observation{
		Provider: provider, RootID: rootID, Source: source, ObservedAt: at, FreshUntil: at.Add(lease),
		Nodes: []agentgraph.Node{{ID: rootID, Runtime: agentgraph.RuntimeActive, UpdatedAt: at}},
	}
}

type landingSide struct {
	kind      state.GraphKind
	source    agentgraph.SourceKind
	at        time.Time
	lease     time.Duration
	attention agentgraph.AttentionState
	rootID    string
}

// landingSession lands each side in order on a fresh session of provider at
// now, returning the session and the last landing's refusal.
func landingSession(t *testing.T, provider agentgraph.ProviderKind, now time.Time, sides ...landingSide) (*state.Session, statusexplain.Reason) {
	t.Helper()
	agent := state.AgentKindClaude
	if provider == agentgraph.ProviderCodex {
		agent = state.AgentKindCodex
	}
	s := &state.Session{PID: 1, StartedAt: landingLifetime, Agent: agent}
	var refused statusexplain.Reason
	for _, side := range sides {
		rootID := side.rootID
		if rootID == "" {
			rootID = "root"
		}
		o := admissionCandidate(provider, rootID, side.source, side.at, side.lease)
		o.Nodes[0].Attention = side.attention
		graph, err := state.ProjectAgentGraph(o, s.AgentGraph, now)
		if err != nil {
			t.Fatal(err)
		}
		refused = s.LandAgentGraph(graph, state.GraphLanding{Kind: side.kind}, now)
	}
	return s, refused
}

func TestLandingClaudeShouldRankFreshEvidenceBeforeEventTimeWhenOneRootCompetes(t *testing.T) {
	now := explainT0.Add(time.Second)
	transcript, hook, restored := state.GraphSnapshot, state.GraphHookEvent, state.GraphRestored
	for _, tc := range []struct {
		name               string
		current, candidate landingSide
		decides            bool
		refused            statusexplain.Reason
	}{
		{"root changed",
			landingSide{transcript, agentgraph.SourceClaudeTranscript, now, time.Minute, "", ""},
			landingSide{restored, agentgraph.SourceRestoredLastKnown, explainT0, time.Minute, "", "other"}, true, ""},
		{"fresh higher rank holds",
			landingSide{transcript, agentgraph.SourceClaudeTranscript, explainT0, time.Minute, "", ""},
			landingSide{hook, agentgraph.SourceHook, now, time.Minute, "", ""}, false, ""},
		{"fresh higher rank candidate wins",
			landingSide{hook, agentgraph.SourceHook, now, time.Minute, "", ""},
			landingSide{transcript, agentgraph.SourceClaudeTranscript, explainT0, time.Minute, "", ""}, true, ""},
		{"stale higher rank yields",
			landingSide{transcript, agentgraph.SourceClaudeTranscript, explainT0, time.Millisecond, "", ""},
			landingSide{hook, agentgraph.SourceHook, now, time.Minute, "", ""}, true, ""},
		{"fresh current holds against a stale newer graph",
			landingSide{hook, agentgraph.SourceHook, explainT0, time.Minute, "", ""},
			landingSide{transcript, agentgraph.SourceClaudeTranscript, now, -time.Second, "", ""}, false, ""},
		{"same kind newer wins",
			landingSide{hook, agentgraph.SourceHook, explainT0, time.Minute, "", ""},
			landingSide{hook, agentgraph.SourceHook, now, time.Minute, "", ""}, true, ""},
		{"same kind older refused",
			landingSide{hook, agentgraph.SourceHook, now, time.Minute, "", ""},
			landingSide{hook, agentgraph.SourceHook, explainT0, time.Minute, "", ""}, false, statusexplain.ReasonOlderThanCurrent},
		// #96: fresh unresolved provider attention survives until evidence
		// authorized to resolve that request arrives (was refused: a fresh
		// transcript graph outranked the hook).
		{"newer hook request against a fresh transcript graph",
			landingSide{transcript, agentgraph.SourceClaudeTranscript, explainT0, time.Minute, "", ""},
			landingSide{hook, agentgraph.SourceHook, now, time.Minute, agentgraph.AttentionApproval, ""}, true, ""},
	} {
		s, refused := landingSession(t, agentgraph.ProviderClaude, now, tc.current, tc.candidate)
		if refused != tc.refused {
			t.Errorf("%s: refused for %q, want %q", tc.name, refused, tc.refused)
		}
		decided := s.StatusDecision()
		decides := decided.Source == string(tc.candidate.source) && decided.ObservedAt.Equal(tc.candidate.at)
		shown := s.AgentGraph.Source == tc.candidate.source && s.AgentGraph.ObservedAt.Equal(tc.candidate.at)
		if decides != tc.decides || shown != tc.decides {
			t.Errorf("%s: candidate decides=%v shown=%v, want %v (decision %+v)", tc.name, decides, shown, tc.decides, decided.Choice)
		}
	}
}

func TestLandingCodexShouldDecideByEventTimeFirstWhenOneConversationCompetes(t *testing.T) {
	now := explainT0.Add(time.Second)
	appServer, hook, rollout := state.GraphSnapshot, state.GraphHookEvent, state.GraphTranscriptTail
	for _, tc := range []struct {
		name               string
		current, candidate landingSide
		decides            bool
	}{
		{"newer hook over fresh app-server",
			landingSide{appServer, agentgraph.SourceCodexAppServer, explainT0, time.Minute, "", ""},
			landingSide{hook, agentgraph.SourceHook, now, time.Minute, "", ""}, true},
		{"older app-server under newer hook",
			landingSide{hook, agentgraph.SourceHook, now, time.Minute, "", ""},
			landingSide{appServer, agentgraph.SourceCodexAppServer, explainT0, time.Minute, "", ""}, false},
		{"newer stale rollout under fresh app-server",
			landingSide{appServer, agentgraph.SourceCodexAppServer, explainT0, time.Minute, "", ""},
			landingSide{rollout, agentgraph.SourceCodexRollout, now, -time.Millisecond, "", ""}, false},
		{"newer rollout idle correction over an older hook",
			landingSide{hook, agentgraph.SourceHook, explainT0, time.Hour, "", ""},
			landingSide{rollout, agentgraph.SourceCodexRollout, now, time.Minute, "", ""}, true},
		{"same instant falls back to rank",
			landingSide{appServer, agentgraph.SourceCodexAppServer, now, time.Minute, "", ""},
			landingSide{hook, agentgraph.SourceHook, now, time.Minute, "", ""}, false},
		// #96 (coordinator decision 1): a Codex hook does not clear the
		// app-server's own request; only a snapshot or its deadline does (was
		// admitted by event time).
		{"newer hook working against app-server input",
			landingSide{appServer, agentgraph.SourceCodexAppServer, explainT0, time.Minute, agentgraph.AttentionUserInput, ""},
			landingSide{hook, agentgraph.SourceHook, now, time.Minute, "", ""}, false},
	} {
		s, refused := landingSession(t, agentgraph.ProviderCodex, now, tc.current, tc.candidate)
		if refused != "" {
			t.Errorf("%s: refused for %q", tc.name, refused)
		}
		decided := s.StatusDecision()
		decides := decided.ObservedAt.Equal(tc.candidate.at) && decided.EvidenceKind == evidenceKindOfLanding(tc.candidate.kind)
		shown := s.AgentGraph.Source == tc.candidate.source && s.AgentGraph.ObservedAt.Equal(tc.candidate.at)
		if decides != tc.decides || shown != tc.decides {
			t.Errorf("%s: candidate decides=%v shown=%v, want %v (decision %+v)", tc.name, decides, shown, tc.decides, decided.Choice)
		}
	}
}

func evidenceKindOfLanding(kind state.GraphKind) statusexplain.EvidenceKind {
	switch kind {
	case state.GraphSnapshot:
		return statusexplain.EvidenceProviderSnapshot
	case state.GraphHookEvent:
		return statusexplain.EvidenceHook
	case state.GraphTranscriptTail:
		return statusexplain.EvidenceTranscript
	case state.GraphRestored:
		return statusexplain.EvidenceRestored
	default:
		return statusexplain.EvidenceHookEdge
	}
}

func TestObservedKindShouldKeepAHeldObservationsProvenanceWhenAnObserverReturnsIt(t *testing.T) {
	for source, want := range map[agentgraph.SourceKind]state.GraphKind{
		agentgraph.SourceClaudeTranscript:  state.GraphSnapshot,
		agentgraph.SourceCodexAppServer:    state.GraphSnapshot,
		agentgraph.SourceRestoredLastKnown: state.GraphRestored,
		agentgraph.SourceHook:              state.GraphHookEvent,
	} {
		if got := observedKind(agentgraph.Observation{Source: source}); got != want {
			t.Errorf("observed %s lands as kind %d, want %d", source, got, want)
		}
	}
}

func TestAdmitRootShouldKeepTheResolversEvidenceWhenDiscoveryReannouncesASession(t *testing.T) {
	m := map[int]*state.Session{}
	prior := &state.Session{PID: 77, StartedAt: landingLifetime, Agent: state.AgentKindClaude, TTY: "/dev/pts/7"}
	o := admissionCandidate(agentgraph.ProviderClaude, "root", agentgraph.SourceHook, explainT0, time.Hour)
	o.Nodes[0].Attention = agentgraph.AttentionApproval
	graph, err := state.ProjectAgentGraph(o, nil, explainT0)
	if err != nil {
		t.Fatal(err)
	}
	prior.LandAgentGraph(graph, state.GraphLanding{Kind: state.GraphHookEvent}, explainT0)
	m[prior.PID] = prior

	admitted := admitRoot(m, state.Session{PID: 77, StartedAt: explainT0, Agent: state.AgentKindClaude, TTY: "/dev/pts/7"}, nil, newFakeHerdrSource(), nil, explainT0)
	at := explainT0.Add(time.Second)
	admitted.SetHerdr(state.HerdrReading{PaneID: "w1:p1", Socket: "/h.sock", TerminalID: "t1", Agent: "claude",
		Status: state.HerdrWorking, Live: true, Since: at}, at)
	if admitted.Claude.Status != state.StatusPermission {
		t.Fatalf("re-announced session published %q under herdr working, want the hook's request held", admitted.Claude.Status)
	}
}
