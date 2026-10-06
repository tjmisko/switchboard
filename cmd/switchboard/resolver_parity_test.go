package main

import (
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statusexplain"
	"github.com/tjmisko/switchboard/internal/statusresolve"
)

// Parity between today's graph admission (admission_characterization_test.go)
// and the pure resolver (#96), which is not wired in yet. Admission decides
// whether a candidate observation replaces the current graph; the resolver
// sees both at once. They agree when the resolver selects the candidate
// exactly where admission admits it. A row may differ only on purpose: it names
// the #96 criterion that changes it.

var parityLifetime = explainT0.Add(-time.Hour)

// parityCandidate builds the resolver candidate for o as its source's
// builder does.
func parityCandidate(o agentgraph.Observation) statusresolve.Candidate {
	switch {
	case o.Source == agentgraph.SourceRestoredLastKnown:
		return statusresolve.RestoredLastKnown(parityLifetime, o)
	case o.Provider == agentgraph.ProviderCodex && o.Source == agentgraph.SourceHook:
		return statusresolve.CodexHook(parityLifetime, o)
	case o.Provider == agentgraph.ProviderCodex && o.Source == agentgraph.SourceCodexRollout:
		return statusresolve.CodexRolloutTail(parityLifetime, o)
	case o.Source == agentgraph.SourceHook:
		return statusresolve.ClaudeHook(parityLifetime, o)
	case o.Source == agentgraph.SourceCodexAppServer:
		return statusresolve.CodexAppServer(parityLifetime, o)
	default:
		return statusresolve.ClaudeGraph(parityLifetime, o)
	}
}

func parityObservation(provider agentgraph.ProviderKind, rootID string, source agentgraph.SourceKind, at time.Time, lease time.Duration, attention agentgraph.AttentionState) agentgraph.Observation {
	o := admissionCandidate(provider, rootID, source, at, lease)
	o.Nodes[0].Attention = attention
	return o
}

func TestResolverParityShouldMatchGraphAdmissionExceptWhereIssue96ChangesIt(t *testing.T) {
	now := explainT0.Add(time.Second)
	const attention = "fresh unresolved provider attention survives until evidence authorized to resolve that request arrives"
	type side struct {
		source    agentgraph.SourceKind
		at        time.Time
		lease     time.Duration
		attention agentgraph.AttentionState
	}
	for _, tc := range []struct {
		name               string
		provider           agentgraph.ProviderKind
		current, candidate side
		candidateRoot      string
		criterion          string // why the resolver differs from admission; "" when it must agree
	}{
		{name: "claude: root changed", provider: agentgraph.ProviderClaude, candidateRoot: "other",
			current:   side{agentgraph.SourceClaudeTranscript, now, time.Minute, ""},
			candidate: side{agentgraph.SourceRestoredLastKnown, explainT0, time.Minute, ""}},
		{name: "claude: fresh higher rank holds", provider: agentgraph.ProviderClaude,
			current:   side{agentgraph.SourceClaudeTranscript, explainT0, time.Minute, ""},
			candidate: side{agentgraph.SourceHook, now, time.Minute, ""}},
		{name: "claude: fresh higher rank candidate wins", provider: agentgraph.ProviderClaude,
			current:   side{agentgraph.SourceHook, now, time.Minute, ""},
			candidate: side{agentgraph.SourceClaudeTranscript, explainT0, time.Minute, ""}},
		{name: "claude: stale higher rank yields", provider: agentgraph.ProviderClaude,
			current:   side{agentgraph.SourceClaudeTranscript, explainT0, time.Millisecond, ""},
			candidate: side{agentgraph.SourceHook, now, time.Minute, ""}},
		{name: "claude: fresh current holds against stale other source", provider: agentgraph.ProviderClaude,
			current:   side{agentgraph.SourceClaudeTranscript, explainT0, time.Minute, ""},
			candidate: side{agentgraph.SourceCodexAppServer, now, -time.Second, ""}},
		{name: "claude: same rank newer wins", provider: agentgraph.ProviderClaude,
			current:   side{agentgraph.SourceHook, explainT0, time.Minute, ""},
			candidate: side{agentgraph.SourceHook, now, time.Minute, ""}},
		{name: "claude: same rank older refused", provider: agentgraph.ProviderClaude,
			current:   side{agentgraph.SourceHook, now, time.Minute, ""},
			candidate: side{agentgraph.SourceHook, explainT0, time.Minute, ""}},
		{name: "claude: newer hook request against a fresh transcript graph", provider: agentgraph.ProviderClaude,
			current:   side{agentgraph.SourceClaudeTranscript, explainT0, time.Minute, ""},
			candidate: side{agentgraph.SourceHook, now, time.Minute, agentgraph.AttentionApproval}, criterion: attention},
		{name: "codex: newer hook over fresh app-server", provider: agentgraph.ProviderCodex,
			current:   side{agentgraph.SourceCodexAppServer, explainT0, time.Minute, ""},
			candidate: side{agentgraph.SourceHook, now, time.Minute, ""}},
		{name: "codex: older app-server under newer hook", provider: agentgraph.ProviderCodex,
			current:   side{agentgraph.SourceHook, now, time.Minute, ""},
			candidate: side{agentgraph.SourceCodexAppServer, explainT0, time.Minute, ""}},
		{name: "codex: newer stale rollout under fresh app-server", provider: agentgraph.ProviderCodex,
			current:   side{agentgraph.SourceCodexAppServer, explainT0, time.Minute, ""},
			candidate: side{agentgraph.SourceCodexRollout, now, -time.Millisecond, ""}},
		{name: "codex: same instant falls back to rank", provider: agentgraph.ProviderCodex,
			current:   side{agentgraph.SourceCodexAppServer, now, time.Minute, ""},
			candidate: side{agentgraph.SourceHook, now, time.Minute, ""}},
		{name: "codex: newer hook working against app-server input", provider: agentgraph.ProviderCodex,
			current:   side{agentgraph.SourceCodexAppServer, explainT0, time.Minute, agentgraph.AttentionUserInput},
			candidate: side{agentgraph.SourceHook, now, time.Minute, ""}, criterion: attention},
	} {
		candidateRoot := "root"
		if tc.candidateRoot != "" {
			candidateRoot = tc.candidateRoot
		}
		current := parityObservation(tc.provider, "root", tc.current.source, tc.current.at, tc.current.lease, tc.current.attention)
		candidate := parityObservation(tc.provider, candidateRoot, tc.candidate.source, tc.candidate.at, tc.candidate.lease, tc.candidate.attention)
		currentGraph, err := state.ProjectAgentGraph(current, nil, tc.current.at)
		if err != nil {
			t.Fatal(err)
		}
		admitted, _ := admitObservation(candidate, currentGraph, now)

		target := statusresolve.Target{Root: statusexplain.Root{PID: 1, StartedAt: parityLifetime, Provider: string(tc.provider), SessionID: candidateRoot}}
		d := statusresolve.Resolve(target, []statusresolve.Candidate{parityCandidate(current), parityCandidate(candidate)}, statusexplain.Decision{}, now)
		selected := d.Source == string(tc.candidate.source) && d.ObservedAt.Equal(tc.candidate.at)

		if (admitted != selected) != (tc.criterion != "") {
			t.Errorf("%s: admission admits=%v, resolver selects candidate=%v (%s for %s): a difference must name its criterion, and only a difference may",
				tc.name, admitted, selected, d.Status, d.Reason)
		}
	}
}
