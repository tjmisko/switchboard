package state

import "time"

// HerdrInfo is the herdr (herdr.dev) pane hosting a session and herdr's own
// reading of the agent in it.
//
// herdr is the status authority for every agent in one of its panes: while
// the daemon is following that pane's server, the session's published status
// (the enrichment block's, and so every renderer's) is herdr's, projected onto
// Switchboard's legacy values by HerdrLegacyStatus. The provider graph keeps
// running underneath it (children, names, usage), and AgentGraph.Summary keeps
// the provider's own verdict, so the two can be compared when they disagree.
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
// provider's own, or false when herdr is not the authority right now.
func (s *Session) herdrAuthority(provider string) (string, time.Time, bool) {
	if s.Herdr == nil || !s.Herdr.Live {
		return "", time.Time{}, false
	}
	status, ok := HerdrLegacyStatus(s.Herdr.Status, provider)
	return status, s.Herdr.StatusSince, ok
}

// SetHerdr records herdr's reading of the session's pane and re-projects the
// published status. live=false withdraws herdr's authority (the pane's server
// is no longer followed), handing the status back to the provider graph.
//
// since is when herdr's status began. It returns the published status before
// and after, so the caller can record a transition when they differ. A session
// with no provider graph has no published status for herdr to replace yet and
// is left unchanged.
func (s *Session) SetHerdr(paneID, socket, agent, status string, live bool, since time.Time) (before, after string) {
	if s.Herdr == nil {
		s.Herdr = &HerdrInfo{}
	}
	h := s.Herdr
	if h.Status != status {
		h.StatusSince = since
	}
	h.PaneID, h.Socket, h.Agent, h.Status, h.Live = paneID, socket, agent, status, live
	info := s.graphEnrichment()
	if info == nil {
		return "", ""
	}
	before = info.Status
	s.projectStatus(info, since)
	return before, info.Status
}

// graphEnrichment returns the enrichment block the provider graph projects
// into, or nil when the session has no graph (and so no status to replace).
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
// summary carries no Since.
func (s *Session) projectStatus(info *AgentInfo, fallback time.Time) {
	status, since := s.AgentGraph.Summary.Status, s.AgentGraph.Summary.Since
	if herdrStatus, herdrSince, ok := s.herdrAuthority(status); ok {
		status, since = herdrStatus, herdrSince
	}
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
