package state

import (
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
)

// piHookGraphFresh reports whether the session's graph is Pi hook evidence for
// its bound root that is still within its lease at now.
func (s *Session) piHookGraphFresh(now time.Time) bool {
	g := s.AgentGraph
	return s.Agent == AgentKindPi && s.Pi != nil && s.Pi.SessionID != "" &&
		g != nil && g.Source == agentgraph.SourceHook && g.RootID == s.Pi.SessionID && g.Fresh(now)
}

// piStatusAuthority is the interim precedence between a bound Pi session's two
// status sources (phase-1 plan, 1.5):
//
//  1. Pi hook evidence within its lease wins. It is exact: the extension
//     reports Pi's own lifecycle and dialog count.
//  2. Otherwise a live herdr reading decides, mapped as HerdrLegacyStatus
//     maps it for every other agent.
//  3. Otherwise the status is unknown ("").
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

// SetPiHookGraph lands the projection of one Pi hook observation: it binds the
// Pi block to the graph's root (Pi's session id) and re-projects the published
// status, returning it before and after so the caller can record the edge.
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
