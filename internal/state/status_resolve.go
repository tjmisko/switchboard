package state

import (
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/statusexplain"
	"github.com/tjmisko/switchboard/internal/statusresolve"
)

// This file is where a session's published status is decided (#96). Every
// path that can move it (a provider graph landing, a herdr reading, a Pi hook,
// a lease lapsing) gathers the session's current evidence into candidates and
// asks the one pure resolver, statusresolve.Resolve, with the clock it was
// given. The decision it returns is both the published status and the record
// explain reads.

// GraphKind is what a landed provider graph is as evidence. The caller that
// lands a graph states it; it is never read off the graph's source field, which
// for a composed Codex graph names the graph it was composed onto.
type GraphKind uint8

const (
	graphKindNone GraphKind = iota
	// GraphSnapshot is a provider snapshot: the Claude observer's transcript
	// graph, a Codex app-server sample.
	GraphSnapshot
	// GraphHookEvent is an exact lifecycle event: the graph a Claude, Codex or
	// Pi hook landed.
	GraphHookEvent
	// GraphHookEdge is a partial hook edge: Codex child hooks composed onto the
	// published graph before the app-server placed their threads. It reports
	// descendants only.
	GraphHookEdge
	// GraphTranscriptTail is correlated transcript evidence: the Codex rollout
	// tail's idle correction, Pi's session file.
	GraphTranscriptTail
	// GraphRestored is a graph loaded from state.json: presentation until the
	// deadline it was persisted with.
	GraphRestored
)

// graphKinds is every kind, in the order candidates are built.
var graphKinds = []GraphKind{GraphSnapshot, GraphHookEvent, GraphHookEdge, GraphTranscriptTail, GraphRestored}

// GraphLanding says what a landed graph is.
type GraphLanding struct {
	Kind GraphKind
	// HookLatched marks attention the graph carries because it was composed
	// with a request the provider's hooks hold open (Codex's pending-input and
	// approval latches), not attention its own source observed. That request
	// is the hook's to resolve.
	HookLatched bool
}

type evidenceEntry struct {
	graph       *AgentGraph
	hookLatched bool
	// superseded marks event-time-ordered evidence a newer observation of
	// another kind replaced while fresh (statusresolve.Candidate.Superseded).
	superseded bool
}

// graphEvidence is the latest graph of each kind for one conversation. It is
// replaced on every change, never mutated, so copies share it safely.
type graphEvidence map[GraphKind]evidenceEntry

// with returns a copy of e holding entry for kind, keeping only evidence about
// entry's conversation.
func (e graphEvidence) with(kind GraphKind, entry evidenceEntry) graphEvidence {
	next := make(graphEvidence, len(e)+1)
	for k, v := range e {
		if v.graph.RootID == entry.graph.RootID {
			next[k] = v
		}
	}
	next[kind] = entry
	return next
}

// providerOf is the provider kind the session's evidence is about.
func (s *Session) providerOf() agentgraph.ProviderKind {
	if s.AgentGraph != nil && s.AgentGraph.provider != "" && s.Agent == "" {
		return s.AgentGraph.provider
	}
	return agentgraph.ProviderKind(s.Agent)
}

// candidate builds the resolver candidate for one landed graph with the
// builder for its provider and kind.
func candidate(provider agentgraph.ProviderKind, kind GraphKind, startedAt time.Time, e evidenceEntry) (statusresolve.Candidate, bool) {
	o := e.graph.observation(provider)
	var c statusresolve.Candidate
	switch {
	case kind == GraphSnapshot && provider == agentgraph.ProviderClaude:
		c = statusresolve.ClaudeGraph(startedAt, o)
	case kind == GraphSnapshot && provider == agentgraph.ProviderCodex:
		c = statusresolve.CodexAppServer(startedAt, o)
	case kind == GraphHookEvent && provider == agentgraph.ProviderClaude:
		c = statusresolve.ClaudeHook(startedAt, o)
	case kind == GraphHookEvent && provider == agentgraph.ProviderCodex:
		c = statusresolve.CodexHook(startedAt, o)
	case kind == GraphHookEvent && provider == agentgraph.ProviderPi:
		c = statusresolve.PiHook(startedAt, o)
	case kind == GraphHookEdge:
		c = statusresolve.CodexChildHooks(startedAt, o)
	case kind == GraphTranscriptTail && provider == agentgraph.ProviderCodex:
		c = statusresolve.CodexRolloutTail(startedAt, o)
	case kind == GraphTranscriptTail && provider == agentgraph.ProviderPi:
		c = statusresolve.PiSessionTail(startedAt, o)
	case kind == GraphRestored:
		c = statusresolve.RestoredLastKnown(startedAt, o)
	default:
		return statusresolve.Candidate{}, false
	}
	if e.hookLatched {
		c = statusresolve.HookLatched(c)
	}
	c.Superseded = e.superseded
	return c, true
}

// statusTarget is the tracked agent the session's candidates must be about.
func (s *Session) statusTarget() statusresolve.Target {
	t := statusresolve.Target{Root: s.explainRoot()}
	if s.Herdr != nil {
		t.PaneID, t.TerminalID = s.Herdr.PaneID, s.Herdr.TerminalID
	}
	return t
}

// source is where a candidate came from: a landed graph of kind, or (kind
// none) the herdr reading.
type candidateSource struct {
	kind  GraphKind
	graph *AgentGraph
}

// graphCandidates builds a candidate from each landed graph, in graphKinds
// order.
func (s *Session) graphCandidates() ([]statusresolve.Candidate, []candidateSource) {
	provider := s.providerOf()
	var candidates []statusresolve.Candidate
	var sources []candidateSource
	for _, kind := range graphKinds {
		e, ok := s.evidence[kind]
		if !ok {
			continue
		}
		if c, ok := candidate(provider, kind, s.StartedAt, e); ok {
			candidates = append(candidates, c)
			sources = append(sources, candidateSource{kind: kind, graph: e.graph})
		}
	}
	return candidates, sources
}

// herdrCandidate is herdr's reading of the session's pane, if herdr has given
// one. A reading that is not followed is withdrawn: it expires at now.
func (s *Session) herdrCandidate(now time.Time) (statusresolve.Candidate, bool) {
	h := s.Herdr
	if h == nil || h.Status == "" {
		return statusresolve.Candidate{}, false
	}
	return statusresolve.Herdr(statusresolve.HerdrReading{
		PaneID: h.PaneID, TerminalID: h.TerminalID, Agent: h.Agent, Status: h.Status, Live: h.Live, Since: h.StatusSince,
	}, now), true
}

// resolveStatus decides the session's status at now from every landed graph
// and herdr's reading, with its previous decision as the prior, and records
// the decision. It returns the decision and the source it rests on (kind
// none with a nil graph for herdr, the prior, or nothing).
func (s *Session) resolveStatus(now time.Time) (statusexplain.Decision, candidateSource) {
	candidates, sources := s.graphCandidates()
	if c, ok := s.herdrCandidate(now); ok {
		candidates = append(candidates, c)
		sources = append(sources, candidateSource{})
	}
	d, chosen := statusresolve.ResolveIndex(s.statusTarget(), candidates, s.statusDecision, now)
	s.statusDecision = d
	if chosen < 0 {
		return d, candidateSource{}
	}
	return d, sources[chosen]
}

// project resolves the session's status at now and publishes it on info.
// StatusSince moves only when the published status changes: to the start the
// deciding evidence carries for that status, else fallback.
func (s *Session) project(info *AgentInfo, fallback, now time.Time) {
	d, source := s.resolveStatus(now)
	if info.Status == d.Status {
		return
	}
	since := time.Time{}
	switch {
	case source.graph != nil:
		if source.graph.Summary.Status == d.Status {
			since = source.graph.Summary.Since
		}
	case d.EvidenceKind == statusexplain.EvidenceTerminal && s.Herdr != nil:
		since = s.Herdr.StatusSince
	}
	if since.IsZero() {
		since = fallback
	}
	info.Status, info.StatusSince = d.Status, since
}

// selectDisplay chooses the published graph among the landed ones: the one
// the provider's own evidence decides by (the resolver over the landed graphs
// alone, so a herdr reading never hides the provider's graph), else landed,
// the graph that just arrived.
func (s *Session) selectDisplay(landed *AgentGraph, kind GraphKind, now time.Time) {
	candidates, sources := s.graphCandidates()
	_, chosen := statusresolve.ResolveIndex(s.statusTarget(), candidates, statusexplain.Decision{}, now)
	if chosen >= 0 && sources[chosen].kind != GraphHookEdge {
		s.AgentGraph, s.displayKind = sources[chosen].graph, sources[chosen].kind
		return
	}
	s.AgentGraph, s.displayKind = landed, kind
}

// DisplayGraphKind is the kind of evidence the published AgentGraph came from,
// so a caller re-landing it (an expiry) can say what it is.
func (s Session) DisplayGraphKind() GraphKind { return s.displayKind }

// LandingRefusal says why a graph of landing's kind would not be kept, "" when
// it would: a graph older than the one its kind already holds for the same
// conversation is refused. A partial edge amends the published graph and is
// never refused.
func (s Session) LandingRefusal(rootID string, observedAt time.Time, kind GraphKind) statusexplain.Reason {
	if kind == GraphHookEdge {
		return ""
	}
	held, ok := s.evidence[kind]
	if ok && held.graph.RootID == rootID && observedAt.Before(held.graph.ObservedAt) {
		return statusexplain.ReasonOlderThanCurrent
	}
	return ""
}

// landEvidence stores graph as the latest evidence of its kind. A partial edge
// was composed onto the published graph; it also amends the evidence that
// graph came from, since it is that same evidence with child detail added.
//
// Provider evidence ordered by event time (Codex) keeps that order across
// kinds: a fresh landing supersedes the older evidence of the other kinds, so
// an older hook does not decide again once a newer app-server sample lapses,
// as it could not when one graph replaced the other. Superseded evidence still
// holds its own open request until its deadline or an authorized resolution.
func (s *Session) landEvidence(graph *AgentGraph, landing GraphLanding, now time.Time) {
	entry := evidenceEntry{graph: graph, hookLatched: landing.HookLatched}
	provider := s.providerOf()
	if landed, ok := candidate(provider, landing.Kind, s.StartedAt, entry); ok && landed.EventTimeOrder && graph.Fresh(now) {
		for kind, held := range s.evidence {
			if kind == landing.Kind || held.superseded || held.graph.RootID != graph.RootID ||
				!held.graph.ObservedAt.Before(graph.ObservedAt) {
				continue
			}
			if c, ok := candidate(provider, kind, s.StartedAt, held); ok && c.EventTimeOrder {
				held.superseded = true
				s.evidence = s.evidence.with(kind, held)
			}
		}
	}
	if landing.Kind == GraphHookEdge {
		if d := s.AgentGraph; d != nil && s.displayKind != graphKindNone && s.displayKind != GraphHookEdge &&
			d.RootID == graph.RootID && d.Source == graph.Source && d.ObservedAt.Equal(graph.ObservedAt) {
			base := s.evidence[s.displayKind]
			base.graph = graph
			s.evidence = s.evidence.with(s.displayKind, base)
		}
	}
	s.evidence = s.evidence.with(landing.Kind, entry)
}

// InheritStatusEvidence carries prior's in-memory status state (its landed
// evidence, the kind its published graph came from, and the resolver's last
// decision) onto s, which replaces prior for the same agent process lifetime.
// Discovery rebuilds a root's Session when it re-announces one; without this
// the resolver would forget the evidence behind the graph it carries over.
func (s *Session) InheritStatusEvidence(prior *Session) {
	s.evidence, s.displayKind, s.statusDecision = prior.evidence, prior.displayKind, prior.statusDecision
}

// ReprojectStatus re-resolves a Claude or Codex session's published status at
// now with no new evidence: a deadline alone can move it (an open request
// expiring, a snapshot lapsing onto a hook). It returns the status before and
// after.
func (s *Session) ReprojectStatus(now time.Time) (before, after string) {
	info := s.graphEnrichment()
	if info == nil || len(s.evidence) == 0 {
		return "", ""
	}
	before = info.Status
	s.selectDisplay(s.AgentGraph, s.displayKind, now)
	s.project(info, now, now)
	return before, info.Status
}
