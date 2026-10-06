package main

import (
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statusexplain"
)

// Characterization of today's graph admission (#95, work unit 6). Each test is
// named for the rule it pins, and asserts the verdict and the reason explain
// files for a refusal. These are #96's regression suite.

func admissionGraph(source agentgraph.SourceKind, at time.Time, lease time.Duration) *state.AgentGraph {
	return &state.AgentGraph{RootID: "root", Source: source, ObservedAt: at, FreshUntil: at.Add(lease)}
}

func admissionCandidate(provider agentgraph.ProviderKind, rootID string, source agentgraph.SourceKind, at time.Time, lease time.Duration) agentgraph.Observation {
	return agentgraph.Observation{
		Provider: provider, RootID: rootID, Source: source, ObservedAt: at, FreshUntil: at.Add(lease),
		Nodes: []agentgraph.Node{{ID: rootID, Runtime: agentgraph.RuntimeActive, UpdatedAt: at}},
	}
}

// Rule source rank: app-server = transcript > hook > rollout = Pi session
// file > restored > anything else.
func TestAdmissionSourceRankShouldOrderSourcesWhenTheyCompete(t *testing.T) {
	order := [][]agentgraph.SourceKind{
		{agentgraph.SourceCodexAppServer, agentgraph.SourceClaudeTranscript},
		{agentgraph.SourceHook},
		{agentgraph.SourceCodexRollout, agentgraph.SourcePiSessionFile},
		{agentgraph.SourceRestoredLastKnown},
		{agentgraph.SourceHerdr, agentgraph.SourceUnknown},
	}
	for tier, sources := range order {
		for _, source := range sources {
			if got, want := sourceRank(source), sourceRank(order[tier][0]); got != want {
				t.Errorf("rank(%s) = %d, want %d like %s", source, got, want, order[tier][0])
			}
			if tier > 0 && sourceRank(source) >= sourceRank(order[tier-1][0]) {
				t.Errorf("rank(%s) = %d does not sit below %s", source, sourceRank(source), order[tier-1][0])
			}
		}
	}
}

// Rules for one Claude conversation: rank first among fresh evidence, then
// freshness, then event time.
func TestAdmissionClaudeShouldRankFreshSourcesBeforeEventTimeWhenOneRootCompetes(t *testing.T) {
	now := explainT0.Add(time.Second)
	for _, tc := range []struct {
		name      string
		current   *state.AgentGraph
		candidate agentgraph.Observation
		admit     bool
		reason    statusexplain.Reason
	}{
		{"no current graph", nil,
			admissionCandidate(agentgraph.ProviderClaude, "root", agentgraph.SourceHook, now, time.Minute), true, ""},
		{"root changed", admissionGraph(agentgraph.SourceClaudeTranscript, now, time.Minute),
			admissionCandidate(agentgraph.ProviderClaude, "other", agentgraph.SourceRestoredLastKnown, explainT0, time.Minute), true, ""},
		{"fresh higher rank holds", admissionGraph(agentgraph.SourceClaudeTranscript, explainT0, time.Minute),
			admissionCandidate(agentgraph.ProviderClaude, "root", agentgraph.SourceHook, now, time.Minute), false, statusexplain.ReasonSourceOutranked},
		{"fresh higher rank candidate wins", admissionGraph(agentgraph.SourceHook, now, time.Minute),
			admissionCandidate(agentgraph.ProviderClaude, "root", agentgraph.SourceClaudeTranscript, explainT0, time.Minute), true, ""},
		{"stale higher rank yields", admissionGraph(agentgraph.SourceClaudeTranscript, explainT0, time.Millisecond),
			admissionCandidate(agentgraph.ProviderClaude, "root", agentgraph.SourceHook, now, time.Minute), true, ""},
		{"fresh current holds against stale other source", admissionGraph(agentgraph.SourceClaudeTranscript, explainT0, time.Minute),
			admissionCandidate(agentgraph.ProviderClaude, "root", agentgraph.SourceCodexAppServer, now, -time.Second), false, statusexplain.ReasonStaleVsFresh},
		{"same rank newer wins", admissionGraph(agentgraph.SourceHook, explainT0, time.Minute),
			admissionCandidate(agentgraph.ProviderClaude, "root", agentgraph.SourceHook, now, time.Minute), true, ""},
		{"same rank older refused", admissionGraph(agentgraph.SourceHook, now, time.Minute),
			admissionCandidate(agentgraph.ProviderClaude, "root", agentgraph.SourceHook, explainT0, time.Minute), false, statusexplain.ReasonOlderThanCurrent},
		{"empty observation", nil,
			agentgraph.Observation{Provider: agentgraph.ProviderClaude}, false, ""},
	} {
		admit, reason := admitObservation(tc.candidate, tc.current, now)
		if admit != tc.admit || reason != tc.reason {
			t.Errorf("%s: admit = %v reason = %q, want %v %q", tc.name, admit, reason, tc.admit, tc.reason)
		}
		if got := shouldApplyObservation(tc.candidate, tc.current, now); got != tc.admit {
			t.Errorf("%s: shouldApplyObservation = %v, want %v", tc.name, got, tc.admit)
		}
	}
}

// Rules for one Codex conversation: exact event time first, so a newer hook
// beats a fresh app-server sample and an older sample never repaints a newer
// hook; a newer stale reading still cannot displace fresh evidence from
// another source.
func TestAdmissionCodexShouldDecideByEventTimeFirstWhenOneConversationCompetes(t *testing.T) {
	now := explainT0.Add(time.Second)
	for _, tc := range []struct {
		name      string
		current   *state.AgentGraph
		candidate agentgraph.Observation
		admit     bool
		reason    statusexplain.Reason
	}{
		{"newer hook over fresh app-server", admissionGraph(agentgraph.SourceCodexAppServer, explainT0, time.Minute),
			admissionCandidate(agentgraph.ProviderCodex, "root", agentgraph.SourceHook, now, time.Minute), true, ""},
		{"older app-server under newer hook", admissionGraph(agentgraph.SourceHook, now, time.Minute),
			admissionCandidate(agentgraph.ProviderCodex, "root", agentgraph.SourceCodexAppServer, explainT0, time.Minute), false, statusexplain.ReasonOlderThanCurrent},
		{"newer stale rollout under fresh app-server", admissionGraph(agentgraph.SourceCodexAppServer, explainT0, time.Minute),
			admissionCandidate(agentgraph.ProviderCodex, "root", agentgraph.SourceCodexRollout, now, -time.Millisecond), false, statusexplain.ReasonStaleVsFresh},
		{"same instant falls back to rank", admissionGraph(agentgraph.SourceCodexAppServer, now, time.Minute),
			admissionCandidate(agentgraph.ProviderCodex, "root", agentgraph.SourceHook, now, time.Minute), false, statusexplain.ReasonSourceOutranked},
	} {
		admit, reason := admitObservation(tc.candidate, tc.current, now)
		if admit != tc.admit || reason != tc.reason {
			t.Errorf("%s: admit = %v reason = %q, want %v %q", tc.name, admit, reason, tc.admit, tc.reason)
		}
	}
}
