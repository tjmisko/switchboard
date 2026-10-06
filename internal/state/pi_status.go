package state

import (
	"strings"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
)

// piHookGraphFresh reports whether the session's graph is Pi hook evidence for
// its bound root that is still within its lease at now.
func (s *Session) piHookGraphFresh(now time.Time) bool {
	return s.piGraphFresh(now, agentgraph.SourceHook)
}

// piGraphFresh reports whether the session's graph comes from one of sources,
// is rooted at the bound Pi session, and is still within its lease at now.
func (s *Session) piGraphFresh(now time.Time, sources ...agentgraph.SourceKind) bool {
	g := s.AgentGraph
	if s.Agent != AgentKindPi || s.Pi == nil || s.Pi.SessionID == "" ||
		g == nil || g.RootID != s.Pi.SessionID || !g.Fresh(now) {
		return false
	}
	for _, source := range sources {
		if g.Source == source {
			return true
		}
	}
	return false
}

// piStatusAuthority is the interim precedence between a bound Pi session's
// status sources (phase-1 plan, 1.5 and 1.7):
//
//  1. Pi hook evidence within its lease wins. It is exact: the extension
//     reports Pi's own lifecycle and dialog count.
//  2. Otherwise a live herdr reading decides, mapped as HerdrLegacyStatus
//     maps it for every other agent.
//  3. Otherwise the tail of Pi's session file, read while no hook has reached
//     this daemon, or the graph a restart restored, within its lease. Neither
//     is live authority, so herdr outranks both; a restored graph keeps only
//     the deadline it was persisted with.
//  4. Otherwise the status is unknown ("").
//
// herdr's blocked state and the hook's dialog count both mean red, so they
// cannot disagree on red; and because fresh hook evidence outranks herdr
// outright, a herdr working reading cannot clear a hook-held red.
//
// #96 replaces this function with the one pure status resolver; keep Pi's
// precedence here and nowhere else so that it can.
func (s *Session) piStatusAuthority(now time.Time) (status string, since time.Time) {
	if s.piHookGraphFresh(now) {
		return s.AgentGraph.Summary.Status, s.AgentGraph.Summary.Since
	}
	if s.Herdr != nil && s.Herdr.Live {
		if status, ok := HerdrLegacyStatus(s.Herdr.Status, ""); ok {
			return status, s.Herdr.StatusSince
		}
	}
	if s.piGraphFresh(now, agentgraph.SourcePiSessionFile, agentgraph.SourceRestoredLastKnown) {
		return s.AgentGraph.Summary.Status, s.AgentGraph.Summary.Since
	}
	return "", time.Time{}
}

// projectPiStatus sets a bound Pi block's published status from
// piStatusAuthority. StatusSince moves only when the status changes; fallback
// dates an edge whose source carries no start.
func (s *Session) projectPiStatus(info *AgentInfo, fallback, now time.Time) {
	status, since := s.piStatusAuthority(now)
	if info.Status == status {
		return
	}
	if since.IsZero() {
		since = fallback
	}
	info.Status, info.StatusSince = status, since
}

// SetPiHookGraph lands the projection of one Pi observation, a hook's or the
// session file's: it binds the Pi block to the graph's root (Pi's session id)
// and re-projects the published status, returning it before and after so the
// caller can record the edge.
func (s *Session) SetPiHookGraph(graph *AgentGraph, now time.Time) (before, after string) {
	before = s.publishedStatus(now)
	s.AgentGraph = graph.Clone()
	info := s.AgentBlock(AgentKindPi)
	info.SessionID = graph.RootID
	s.projectPiStatus(info, graph.Summary.Since, now)
	return before, info.Status
}

// ReprojectPi re-derives a bound Pi session's published status at now. Hook
// evidence lapses without any new reading arriving, so the reconcile tick
// calls this to let the status fall back to herdr, or to unknown.
func (s *Session) ReprojectPi(now time.Time) (before, after string) {
	if s.Agent != AgentKindPi || s.Pi == nil {
		return "", ""
	}
	before = s.Pi.Status
	s.projectPiStatus(s.Pi, now, now)
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
