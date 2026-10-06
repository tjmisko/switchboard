package state

import (
	"strings"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
)

// piHookGraphFresh reports whether the session holds Pi hook evidence for its
// bound root that is still within its lease at now.
func (s *Session) piHookGraphFresh(now time.Time) bool {
	e, ok := s.evidence[GraphHookEvent]
	return ok && s.Agent == AgentKindPi && s.Pi != nil && s.Pi.SessionID != "" &&
		e.graph.RootID == s.Pi.SessionID && e.graph.Fresh(now)
}

// SetPiHookGraph lands the projection of a Pi extension hook: exact lifecycle
// evidence with complete coverage. It binds the Pi block to the graph's root
// (Pi's session id) and re-resolves the published status, returning it before
// and after so the caller can record the edge.
func (s *Session) SetPiHookGraph(graph *AgentGraph, now time.Time) (before, after string) {
	return s.landPiGraph(graph, GraphHookEvent, now)
}

// SetPiSessionFileGraph lands the projection of Pi's session file, read while
// no hook has reached the daemon: correlated transcript evidence, which a live
// herdr reading outranks.
func (s *Session) SetPiSessionFileGraph(graph *AgentGraph, now time.Time) (before, after string) {
	return s.landPiGraph(graph, GraphTranscriptTail, now)
}

func (s *Session) landPiGraph(graph *AgentGraph, kind GraphKind, now time.Time) (before, after string) {
	before = s.publishedStatus(now)
	graph = graph.Clone()
	s.AgentGraph, s.displayKind = graph, kind
	info := s.AgentBlock(AgentKindPi)
	info.SessionID = graph.RootID
	s.landEvidence(graph, GraphLanding{Kind: kind}, now)
	s.project(info, graph.Summary.Since, now)
	return before, info.Status
}

// ReprojectPi re-resolves a bound Pi session's published status at now. Hook
// evidence lapses without any new reading arriving, so the reconcile tick
// calls this to let the status fall back to herdr, or to unknown.
func (s *Session) ReprojectPi(now time.Time) (before, after string) {
	if s.Agent != AgentKindPi || s.Pi == nil {
		return "", ""
	}
	before = s.Pi.Status
	s.project(s.Pi, now, now)
	return before, s.Pi.Status
}

// RotatePiSession rebinds the Pi block to a new Pi session (/new, /resume,
// /fork, /clone) and drops the display state bound to the old conversation.
// The published status is kept until the new session's first hook graph
// lands, in the same store update, so no transient unknown is published.
func (s *Session) RotatePiSession(sessionID, transcript string) {
	info := s.AgentBlock(AgentKindPi)
	*info = AgentInfo{SessionID: sessionID, Transcript: transcript, Status: info.Status, StatusSince: info.StatusSince}
	s.DisplayName = nil
}

// SetPiNativeName records Pi's /name for the bound Pi session as its display
// name, origin native. An empty name, which is how Pi reports a cleared one,
// drops it, and the label falls back to the ordinary chain. A name for any
// session other than the bound one is ignored: it belongs to a conversation
// this block no longer shows.
func (s *Session) SetPiNativeName(sessionID, name string) {
	if s.Pi == nil || s.Pi.SessionID == "" || s.Pi.SessionID != sessionID {
		return
	}
	name = strings.TrimSpace(name)
	if name == "" {
		s.DisplayName = nil
		return
	}
	s.DisplayName = &DisplayName{Value: name, Origin: DisplayNameNative, ConversationID: sessionID}
}

// SetPiUsage sets the bound Pi root node's cumulative usage and billing
// identity on a graph Pi owns (its hooks', its session file's, or a restored
// one). Status, summary and lease are untouched: usage is no status evidence.
// A herdr graph is left alone, since herdr rebuilds its node on every reading;
// the reducer's own total lands with the next Pi observation instead. It
// reports whether the graph took the usage.
func (s *Session) SetPiUsage(sessionID string, usage agentgraph.Usage, billing agentgraph.BillingIdentity) bool {
	g := s.AgentGraph
	if s.Agent != AgentKindPi || s.Pi == nil || s.Pi.SessionID != sessionID ||
		g == nil || g.RootID != sessionID || g.Source == agentgraph.SourceHerdr {
		return false
	}
	clone := g.Clone()
	for i := range clone.Nodes {
		if clone.Nodes[i].ID != sessionID {
			continue
		}
		clone.Nodes[i].Usage = AgentUsage{
			InputTokens: usage.InputTokens, CachedInputTokens: usage.CachedInputTokens,
			CacheWriteInputTokens: usage.CacheWriteInputTokens, OutputTokens: usage.OutputTokens,
			ReasoningOutputTokens: usage.ReasoningOutputTokens, TotalTokens: usage.TotalTokens,
			ModelContextWindow: usage.ModelContextWindow,
		}
		clone.Nodes[i].Billing = billing
		s.AgentGraph = clone
		return true
	}
	return false
}
