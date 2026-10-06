package state

import (
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/statusexplain"
)

// HerdrInfo is the herdr (herdr.dev) pane hosting a session and herdr's own
// reading of the agent in it.
//
// herdr is the status authority for every agent in one of its panes: while
// the daemon is following that pane's server, the session's published status
// (the enrichment block's, and so every renderer's) is herdr's, projected onto
// Switchboard's legacy values by HerdrLegacyStatus. The provider graph keeps
// running underneath it (children, names, usage), and AgentGraph.Summary keeps
// the provider's own verdict, so the two can be compared when they disagree.
// Fresh Codex input/approval attention takes precedence: a screen reporting work
// cannot determine whether an asynchronous question still needs an answer.
type HerdrInfo struct {
	// PaneID is herdr's public pane id ("w1:p2"). It changes when the pane
	// moves to another workspace; the reconciler refreshes it from the terminal
	// backend every tick.
	PaneID string `json:"pane_id"`
	// Socket is the API socket of the herdr server that owns the pane.
	Socket string `json:"socket"`
	// Agent is the agent herdr detected in the pane, "" when none.
	Agent string `json:"agent,omitempty"`
	// Status is herdr's raw status: working|blocked|done|idle|unknown. Empty
	// until the daemon has read it.
	Status string `json:"status,omitempty"`

	// Live is true while the daemon is following the pane's server and Status
	// is current. It is in-memory only: a status hydrated from state.json after a
	// restart carries no authority until the watcher confirms it again.
	Live bool `json:"-"`
	// StatusSince is when herdr's status last changed, for the legacy
	// StatusSince of an edge herdr decides.
	StatusSince time.Time `json:"-"`
	// ActivePaneID is the pane herdr's focus is on, on this pane's server: the
	// herdr counterpart of HyprlandInfo.ActivePaneID. "" means herdr has not
	// said (its server is not followed). This live observation stays off the
	// wire; Focused is its public projection.
	ActivePaneID string `json:"-"`
}

// Raw herdr statuses, as the socket API spells them.
const (
	HerdrWorking = "working"
	HerdrBlocked = "blocked"
	HerdrDone    = "done"
	HerdrIdle    = "idle"
	HerdrUnknown = "unknown"
)

// HerdrLegacyStatus projects herdr's status onto Switchboard's. blocked is
// permission; done (idle that no one has looked at) is idle. An idle agent
// whose provider graph shows working subagents stays delegating: herdr reads
// the screen and cannot see background agents, so that is detail it lacks, not
// a disagreement. unknown means herdr cannot say, so it decides nothing.
func HerdrLegacyStatus(herdr, provider string) (string, bool) {
	switch herdr {
	case HerdrWorking:
		return StatusWorking, true
	case HerdrBlocked:
		return StatusPermission, true
	case HerdrIdle, HerdrDone:
		if provider == StatusDelegating {
			return StatusDelegating, true
		}
		return StatusIdle, true
	default:
		return "", false
	}
}

// herdrAuthority returns the status herdr decides for this session given the
// provider's own, or false when herdr is not the authority right now. reason
// says why: herdr_override when it decides, herdr_yield_attention when it
// yields to Codex attention, coverage_unsupported when its live reading
// classifies nothing, and "" when herdr is not followed.
func (s *Session) herdrAuthority(provider string) (status string, since time.Time, reason statusexplain.Reason, ok bool) {
	if s.Herdr == nil || !s.Herdr.Live {
		return "", time.Time{}, "", false
	}
	if s.Agent == AgentKindCodex && s.AgentGraph != nil && s.AgentGraph.Fresh(time.Now()) &&
		(s.AgentGraph.Summary.Attention == agentgraph.AttentionUserInput || s.AgentGraph.Summary.Attention == agentgraph.AttentionApproval) {
		return "", time.Time{}, statusexplain.ReasonHerdrYieldAttention, false
	}
	status, ok = HerdrLegacyStatus(s.Herdr.Status, provider)
	if !ok {
		return "", time.Time{}, statusexplain.ReasonCoverageUnsupported, false
	}
	return status, s.Herdr.StatusSince, statusexplain.ReasonHerdrOverride, true
}

// HerdrReading is one reading of a session's herdr pane.
type HerdrReading struct {
	PaneID     string
	Socket     string
	TerminalID string // herdr's id for the pane's terminal; stable across moves
	Agent      string
	Status     string
	// Live is whether the pane's server is followed now; false withdraws
	// herdr's authority.
	Live bool
	// Since is when herdr's status began.
	Since time.Time
	// ActivePaneID is the pane herdr's focus is on, "" when unknown.
	ActivePaneID string
}

// SetHerdr records a reading of the session's herdr pane and re-projects the
// published status, returning it before and after so the caller can record a
// transition when they differ.
//
// A Claude or Codex session publishes herdr's status over its provider graph
// (see projectStatus); one with no graph yet has nothing to replace and is left
// unchanged. Any other agent has no provider adapter at all, so herdr is its
// only source until a Pi hook binds the session: the reading becomes its whole
// agent graph, rooted at Pi's session id once bound. A bound Pi session takes
// its status from piStatusAuthority, which prefers fresh hook evidence.
//
// A reading that is not live, for a block that was not live either, carries no
// news: it is what the daemon sees at startup before the watcher reconnects.
// The pane identity is still recorded, but the status (a hydrated one
// included) waits for herdr to confirm or replace it rather than flashing to
// unknown and back.
func (s *Session) SetHerdr(r HerdrReading, now time.Time) (before, after string) {
	if !r.Live && (s.Herdr == nil || !s.Herdr.Live) {
		if s.Herdr == nil {
			s.Herdr = &HerdrInfo{}
		}
		s.Herdr.PaneID, s.Herdr.Socket, s.Herdr.ActivePaneID = r.PaneID, r.Socket, r.ActivePaneID
		status := s.publishedStatus(now)
		return status, status
	}
	if s.Herdr == nil {
		s.Herdr = &HerdrInfo{}
	}
	h := s.Herdr
	if h.Status != r.Status {
		h.StatusSince = r.Since
	}
	h.PaneID, h.Socket, h.Agent, h.Status, h.Live = r.PaneID, r.Socket, r.Agent, r.Status, r.Live
	h.ActivePaneID = r.ActivePaneID
	if !IsProviderAgent(s.Agent) {
		before = s.publishedStatus(now)
		// Fresh Pi hook evidence owns the graph; herdr's reading waits in the
		// block until that evidence lapses (see piStatusAuthority).
		if !s.piHookGraphFresh(now) {
			s.AgentGraph = herdrAgentGraph(s.Agent, r, s.AgentGraph, s.boundRootID(), now)
		}
		// A Pi session a hook has bound publishes through its own block; until
		// then it has none and publishes its herdr graph.
		if s.Agent == AgentKindPi && s.Pi != nil {
			s.projectPiStatus(s.Pi, r.Since, now)
		} else {
			s.recordHerdrOnly(now)
		}
		return before, s.publishedStatus(now)
	}
	info := s.graphEnrichment()
	if info == nil {
		return "", ""
	}
	before = info.Status
	s.projectStatus(info, r.Since, now)
	return before, info.Status
}

// publishedStatus is the session's status as renderers read it: the
// enrichment block's, else its graph's.
func (s *Session) publishedStatus(now time.Time) string {
	if info := s.Enrichment(); info != nil {
		return info.Status
	}
	return s.graphStatus(now)
}

// IsProviderAgent reports whether Switchboard has a provider adapter for the
// agent kind (Claude Code, Codex). Every other kind is discovered and observed
// through herdr alone.
func IsProviderAgent(kind string) bool {
	return kind == AgentKindClaude || kind == AgentKindCodex
}

// graphStatus is the published status of a session with no enrichment block:
// its graph's summary, "" while the graph is absent or expired.
func (s *Session) graphStatus(now time.Time) string {
	if s.AgentGraph == nil || !s.AgentGraph.Fresh(now) {
		return ""
	}
	return s.AgentGraph.Summary.Status
}

// herdrGraphLease is how long a herdr-sourced graph stays fresh without a new
// reading. herdr streams every change and the daemon re-reads each pane every
// reconcile tick, but a reading that does not change is not re-applied, so the
// lease must outlast any quiet stretch. Losing the server is what expires it:
// that reading carries Live=false and a lease ending now.
const herdrGraphLease = 30 * 24 * time.Hour

// herdrAgentGraph turns a herdr reading into the one-node graph of an agent
// herdr alone observes. The root id is boundRootID when a provider hook has
// bound one (see PiRootID), else herdr's terminal id, stable for the life of
// the terminal the agent runs in; a reading without one keeps the prior root.
func herdrAgentGraph(agent string, r HerdrReading, prior *AgentGraph, boundRootID string, now time.Time) *AgentGraph {
	rootID := "herdr:" + r.TerminalID
	switch {
	case r.TerminalID != "":
	case prior != nil && prior.RootID != "":
		rootID = prior.RootID
	default:
		rootID = "herdr:" + r.Socket + "#" + r.PaneID
	}
	rootID = PiRootID(boundRootID, rootID)
	observedAt := r.Since
	if observedAt.IsZero() || observedAt.After(now) {
		observedAt = now
	}
	freshUntil := observedAt.Add(herdrGraphLease)
	if !r.Live {
		freshUntil = now // not followed: the reading reduces to unknown
	}
	runtime, attention := herdrNodeState(r.Status)
	observation := agentgraph.Observation{
		Provider: agentgraph.ProviderKind(agent), RootID: rootID, Source: agentgraph.SourceHerdr,
		ObservedAt: observedAt, FreshUntil: freshUntil,
		Nodes: []agentgraph.Node{{ID: rootID, Nickname: r.Agent, Runtime: runtime, Attention: attention, UpdatedAt: observedAt}},
	}
	graph, err := ProjectAgentGraph(observation, prior, now)
	if err != nil {
		return prior
	}
	return graph
}

// herdrNodeState maps herdr's status onto a graph node, so the reducer derives
// the same legacy status HerdrLegacyStatus does.
func herdrNodeState(status string) (agentgraph.RuntimeState, agentgraph.AttentionState) {
	switch status {
	case HerdrWorking:
		return agentgraph.RuntimeActive, agentgraph.AttentionNone
	case HerdrBlocked:
		return agentgraph.RuntimeActive, agentgraph.AttentionApproval
	case HerdrIdle, HerdrDone:
		return agentgraph.RuntimeIdle, agentgraph.AttentionNone
	default:
		return agentgraph.RuntimeUnknown, agentgraph.AttentionNone
	}
}

// PiRootID chooses a Pi session's graph root: Pi's own session id once a hook
// has bound it, else the herdr root (herdr:<terminal id>) it has until then.
func PiRootID(boundSessionID, herdrRootID string) string {
	if boundSessionID != "" {
		return boundSessionID
	}
	return herdrRootID
}

// boundRootID is the provider session id a hook has bound to a herdr-observed
// session, "" while none has. Only Pi binds one.
func (s *Session) boundRootID() string {
	if s.Agent != AgentKindPi || s.Pi == nil {
		return ""
	}
	return s.Pi.SessionID
}

// graphEnrichment returns the enrichment block the provider graph projects
// into, or nil when the session has no graph (and so no status to replace).
// A Pi block is not one: it projects through projectPiStatus.
func (s *Session) graphEnrichment() *AgentInfo {
	if s.AgentGraph == nil {
		return nil
	}
	switch s.Agent {
	case AgentKindClaude:
		return s.Claude
	case AgentKindCodex:
		return s.Codex
	}
	return nil
}

// projectStatus sets the published status from the provider graph's summary,
// overridden by herdr while it is the authority. StatusSince moves only when
// the published status changes. fallback dates a provider-decided edge whose
// summary carries no Since. now dates the decision record, changed or not; it
// decides nothing.
func (s *Session) projectStatus(info *AgentInfo, fallback, now time.Time) {
	status, since := s.AgentGraph.Summary.Status, s.AgentGraph.Summary.Since
	herdrStatus, herdrSince, herdrReason, herdrDecides := s.herdrAuthority(status)
	if herdrDecides {
		status, since = herdrStatus, herdrSince
	}
	s.recordGraphProjection(status, herdrReason, now)
	if info.Status == status {
		return
	}
	if since.IsZero() {
		since = fallback
	}
	info.Status, info.StatusSince = status, since
}

func cloneHerdr(h *HerdrInfo) *HerdrInfo {
	if h == nil {
		return nil
	}
	value := *h
	return &value
}
