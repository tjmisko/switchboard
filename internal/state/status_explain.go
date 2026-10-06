package state

import (
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/statusexplain"
)

// This file records why a session's published status is what it is (#95).
// The projections (projectStatus, projectPiStatus, SetHerdr's herdr-only path)
// file their decision here as they make it; ExplainStatus reads it back. None
// of it decides a status: it only observes the decisions already made.

// StatusDecision returns the decision the session's projection last recorded.
func (s Session) StatusDecision() statusexplain.Projection { return s.statusDecision }

func (s *Session) recordDecision(choice statusexplain.Choice, rejected statusexplain.Candidate) {
	s.statusDecision = statusexplain.Projection{Choice: choice, StartedAt: s.StartedAt, Rejected: rejected}
}

func graphChoice(g *AgentGraph, status string, reason statusexplain.Reason, now time.Time) statusexplain.Choice {
	return statusexplain.Choice{
		Status: status, Source: string(g.Source), EvidenceKind: statusexplain.EvidenceKindOf(g.Source),
		Reason: reason, ObservedAt: g.ObservedAt, FreshUntil: g.FreshUntil, DecidedAt: now,
	}
}

func graphCandidate(g *AgentGraph, reject statusexplain.Reason) statusexplain.Candidate {
	return statusexplain.Candidate{
		Source: string(g.Source), EvidenceKind: statusexplain.EvidenceKindOf(g.Source),
		Status: g.Summary.Status, ObservedAt: g.ObservedAt, FreshUntil: g.FreshUntil, RejectReason: reject,
	}
}

// herdrChoice is herdr's live reading deciding. A live reading has no
// deadline: it holds until the next one.
func (s *Session) herdrChoice(status string, reason statusexplain.Reason, now time.Time) statusexplain.Choice {
	return statusexplain.Choice{
		Status: status, Source: string(agentgraph.SourceHerdr), EvidenceKind: statusexplain.EvidenceTerminal,
		Reason: reason, ObservedAt: s.Herdr.StatusSince, DecidedAt: now,
	}
}

// herdrCandidate is herdr's live reading, mapped as HerdrLegacyStatus maps it
// against provider, losing for reject.
func (s *Session) herdrCandidate(provider string, reject statusexplain.Reason) statusexplain.Candidate {
	status, _ := HerdrLegacyStatus(s.Herdr.Status, provider)
	return statusexplain.Candidate{
		Source: string(agentgraph.SourceHerdr), EvidenceKind: statusexplain.EvidenceTerminal,
		Status: status, ObservedAt: s.Herdr.StatusSince, RejectReason: reject,
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

// recordGraphProjection files projectStatus's decision. status is the one it
// published; herdrReason is herdrAuthority's.
func (s *Session) recordGraphProjection(status string, herdrReason statusexplain.Reason, now time.Time) {
	g := s.AgentGraph
	switch herdrReason {
	case statusexplain.ReasonHerdrOverride:
		s.recordDecision(s.herdrChoice(status, herdrReason, now), graphCandidate(g, herdrReason))
	case statusexplain.ReasonHerdrYieldAttention:
		s.recordDecision(graphChoice(g, status, herdrReason, now), s.herdrCandidate(g.Summary.Status, herdrReason))
	case statusexplain.ReasonCoverageUnsupported:
		// herdr is live but its reading classifies nothing; the graph decides.
		s.recordDecision(graphChoice(g, status, graphReason(g, status, now), now), s.herdrCandidate(g.Summary.Status, herdrReason))
	default:
		s.recordDecision(graphChoice(g, status, graphReason(g, status, now), now), statusexplain.Candidate{})
	}
}

// recordPiProjection files projectPiStatus's decision: status as
// piStatusAuthority decided it, for reason.
func (s *Session) recordPiProjection(status string, reason statusexplain.Reason, now time.Time) {
	bound := s.piBoundGraph()
	herdrLive := s.Herdr != nil && s.Herdr.Live
	var rejected statusexplain.Candidate
	var choice statusexplain.Choice
	switch reason {
	case statusexplain.ReasonPiHookAuthority:
		choice = graphChoice(bound, status, reason, now)
		if herdrLive {
			rejected = s.herdrCandidate("", reason)
		}
	case statusexplain.ReasonHerdrFallback:
		choice = s.herdrChoice(status, reason, now)
		switch {
		case bound == nil:
		case bound.Fresh(now):
			// Session-file or restored evidence: herdr outranks both.
			rejected = graphCandidate(bound, statusexplain.ReasonSourceOutranked)
		default:
			rejected = graphCandidate(bound, statusexplain.ReasonObservationExpired)
		}
	case statusexplain.ReasonGraphAuthority:
		choice = graphChoice(bound, status, reason, now)
		if herdrLive {
			rejected = s.herdrCandidate("", statusexplain.ReasonCoverageUnsupported)
		}
	default:
		switch {
		case bound != nil:
			choice = graphChoice(bound, status, reason, now)
		case herdrLive:
			choice = s.herdrChoice(status, reason, now)
		default:
			choice = statusexplain.Choice{Status: status, EvidenceKind: statusexplain.EvidenceNone, Reason: reason, DecidedAt: now}
		}
	}
	s.recordDecision(choice, rejected)
}

// recordHerdrOnly files the decision for a session herdr alone observes: an
// agent with no provider adapter, or a Pi session no hook has bound. Its
// published status is its herdr graph's.
func (s *Session) recordHerdrOnly(now time.Time) {
	g := s.AgentGraph
	if g == nil {
		s.recordDecision(statusexplain.Choice{EvidenceKind: statusexplain.EvidenceNone,
			Reason: statusexplain.ReasonCoverageUnsupported, DecidedAt: now}, statusexplain.Candidate{})
		return
	}
	status := s.graphStatus(now)
	reason := statusexplain.ReasonHerdrOnly
	if status == "" {
		reason = graphReason(g, status, now)
	}
	s.recordDecision(graphChoice(g, status, reason, now), statusexplain.Candidate{})
}

// ExplainStatus explains the session's status at now: the decision its
// projection recorded for this process lifetime, or why there is none; with
// publication's usage-limit overlay computed on demand by the rule
// ProjectPublished applies. A recorded decision that no longer matches the
// status the session shows is not offered as its cause: some path that records
// nothing moved the status since, and the explanation says so. Admission-layer
// candidates are the coordinator's to add.
func (s Session) ExplainStatus(now time.Time) statusexplain.Decision {
	d := statusexplain.Decision{Root: s.explainRoot()}
	recorded := s.statusDecision
	if recorded.Reason != "" && recorded.StartedAt.Equal(s.StartedAt) {
		d.Choice = recorded.Choice
		d.AddRejected(recorded.Rejected)
	} else {
		d.Choice = s.undecidedChoice(now)
	}
	if published := s.publishedStatus(now); d.Status != published {
		d.Choice = statusexplain.Choice{Status: published, EvidenceKind: statusexplain.EvidenceNone,
			Reason: statusexplain.ReasonUnrecorded}
		d.Rejected = nil
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

// undecidedChoice explains a session no projection has decided for in this
// lifetime: one restored across a restart, or one nothing observes yet. Its
// DecidedAt stays zero, because nothing decided.
func (s Session) undecidedChoice(now time.Time) statusexplain.Choice {
	g := s.AgentGraph
	switch {
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
