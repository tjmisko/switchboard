package state

import (
	"time"

	"github.com/tjmisko/switchboard/internal/statusexplain"
)

// This file explains why a session's published status is what it is (#95).
// The status resolver (status_resolve.go) records its decision as it makes
// it; ExplainStatus reads it back. Nothing here decides a status.

// StatusDecision returns the resolver's last decision for the session.
func (s Session) StatusDecision() statusexplain.Decision { return s.statusDecision }

func graphChoice(g *AgentGraph, status string, reason statusexplain.Reason, now time.Time) statusexplain.Choice {
	return statusexplain.Choice{
		Status: status, Source: string(g.Source), EvidenceKind: statusexplain.EvidenceKindOf(g.Source),
		Reason: reason, ObservedAt: g.ObservedAt, FreshUntil: g.FreshUntil, DecidedAt: now,
	}
}

// graphReason is why a graph that decided gave status: its own summary, or,
// for an unknown one, expiry or a source that cannot classify the root.
func graphReason(g *AgentGraph, status string, now time.Time) statusexplain.Reason {
	switch {
	case status != "":
		return statusexplain.ReasonGraphAuthority
	case !g.Fresh(now):
		return statusexplain.ReasonObservationExpired
	default:
		return statusexplain.ReasonCoverageUnsupported
	}
}

// ExplainStatus explains the session's status at now: the decision the
// resolver recorded for this process lifetime, or why there is none; with
// publication's usage-limit overlay computed on demand by the rule
// ProjectPublished applies. A recorded decision that no longer matches the
// status the session shows is not offered as its cause: some path that records
// nothing moved the status since, and the explanation says so (unrecorded).
// Every path that moves a published status now resolves it, so that is rare;
// see the Phase 3 plan for where it remains.
func (s Session) ExplainStatus(now time.Time) statusexplain.Decision {
	d := statusexplain.Decision{Root: s.explainRoot()}
	recorded := s.statusDecision
	if recorded.Reason != "" && recorded.Root.StartedAt.Equal(s.StartedAt) {
		d.Choice = recorded.Choice
		d.Rejected = append([]statusexplain.Candidate(nil), recorded.Rejected...)
		d.RejectedOmitted = recorded.RejectedOmitted
	} else {
		d.Choice = s.undecidedChoice(now)
	}
	if published := s.publishedStatus(now); d.Status != published {
		d.Choice = statusexplain.Choice{Status: published, EvidenceKind: statusexplain.EvidenceNone,
			Reason: statusexplain.ReasonUnrecorded}
		d.Rejected, d.RejectedOmitted = nil, 0
	}
	if usageLimitOverlays(&s, now) {
		underlying := d.Choice
		d.Underlying = &underlying
		d.Choice = statusexplain.Choice{
			Status: StatusLimited, Source: s.UsageLimit.Source, EvidenceKind: statusexplain.EvidenceUsageLimit,
			Reason: statusexplain.ReasonUsageLimitOverlay, ObservedAt: s.UsageLimit.ObservedAt,
			FreshUntil: s.UsageLimit.deadline(), DecidedAt: now,
		}
	}
	return d
}

func (s Session) explainRoot() statusexplain.Root {
	root := statusexplain.Root{PID: s.PID, StartedAt: s.StartedAt, Provider: s.Agent}
	if info := s.Enrichment(); info != nil {
		root.SessionID = info.SessionID
	}
	if root.SessionID == "" && s.AgentGraph != nil {
		root.SessionID = s.AgentGraph.RootID
	}
	return root
}

// undecidedChoice explains a session the resolver has not decided for in this
// lifetime: one restored across a restart, or one nothing observes yet. Its
// DecidedAt stays zero, because nothing decided.
func (s Session) undecidedChoice(now time.Time) statusexplain.Choice {
	g := s.AgentGraph
	switch {
	case g != nil && s.displayKind == GraphRestored:
		status := s.graphStatus(now)
		reason := statusexplain.ReasonLastKnown
		if status == "" {
			reason = graphReason(g, status, now)
		}
		choice := graphChoice(g, status, reason, time.Time{})
		choice.EvidenceKind = statusexplain.EvidenceRestored
		return choice
	case g != nil:
		status := s.graphStatus(now)
		return graphChoice(g, status, graphReason(g, status, now), time.Time{})
	case !IsProviderAgent(s.Agent) && s.Agent != AgentKindPi:
		return statusexplain.Choice{EvidenceKind: statusexplain.EvidenceNone, Reason: statusexplain.ReasonCoverageUnsupported}
	case s.explainRoot().SessionID == "":
		return statusexplain.Choice{EvidenceKind: statusexplain.EvidenceNone, Reason: statusexplain.ReasonBindingMissing}
	default:
		return statusexplain.Choice{EvidenceKind: statusexplain.EvidenceNone, Reason: statusexplain.ReasonObservationPending}
	}
}
