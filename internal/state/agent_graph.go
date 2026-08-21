package state

import (
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
)

// AgentGraph is the additive, provider-neutral wire projection attached to one
// switchable root Session. It is a bounded current-session view, not history.
// Nodes are always normalized into deterministic root-first depth-first order.
type AgentGraph struct {
	RootID     string                `json:"root_id"`
	Source     agentgraph.SourceKind `json:"source,omitempty"`
	ObservedAt time.Time             `json:"observed_at,omitzero"`
	FreshUntil time.Time             `json:"fresh_until,omitzero"`
	Complete   bool                  `json:"complete"`
	Summary    AgentGraphSummary     `json:"summary"`
	Nodes      []AgentNode           `json:"nodes"`

	// provider is needed only while applying a freshly projected value to a
	// Session whose discovery kind has not yet landed. The provider is already
	// represented by Session.Agent on the wire, so duplicating it in agent_graph
	// would add two sources of truth.
	provider agentgraph.ProviderKind
}

// AgentGraphSummary is the state-owned wire form of agentgraph.Summary. Status
// is the legacy working|idle|permission|delegating value; an empty status means
// no fresh authoritative summary. The approval/user-input counts retain the
// distinction that the compact Attention field necessarily folds.
type AgentGraphSummary struct {
	Runtime        agentgraph.RuntimeState   `json:"runtime"`
	Attention      agentgraph.AttentionState `json:"attention"`
	Status         string                    `json:"status"`
	LiveChildren   int                       `json:"live_children"`
	WaitingNodes   int                       `json:"waiting_nodes"`
	ApprovalNodes  int                       `json:"approval_nodes"`
	UserInputNodes int                       `json:"user_input_nodes"`
	ErrorNodes     int                       `json:"error_nodes"`
	Since          time.Time                 `json:"since,omitzero"`
}

// AgentNode is the immutable state/wire projection of one provider node. The
// three state axes are deliberately not omitempty: explicit unknown/none values
// make old, partial, and forward-version snapshots unambiguous.
type AgentNode struct {
	ID          string                    `json:"id"`
	ParentID    string                    `json:"parent_id,omitempty"`
	Nickname    string                    `json:"nickname,omitempty"`
	Role        string                    `json:"role,omitempty"`
	Description string                    `json:"description,omitempty"`
	Runtime     agentgraph.RuntimeState   `json:"runtime"`
	Attention   agentgraph.AttentionState `json:"attention"`
	Lifecycle   agentgraph.LifecycleState `json:"lifecycle"`
	StartedAt   time.Time                 `json:"started_at,omitzero"`
	UpdatedAt   time.Time                 `json:"updated_at,omitzero"`
	CompletedAt time.Time                 `json:"completed_at,omitzero"`
	Usage       AgentUsage                `json:"usage,omitzero"`
}

// AgentUsage is optional token accounting. A wholly zero value is omitted; no
// consumer should interpret absence as measured zero usage.
type AgentUsage struct {
	InputTokens           int64 `json:"input_tokens,omitempty"`
	CachedInputTokens     int64 `json:"cached_input_tokens,omitempty"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens,omitempty"`
	OutputTokens          int64 `json:"output_tokens,omitempty"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens,omitempty"`
	TotalTokens           int64 `json:"total_tokens,omitempty"`
	ModelContextWindow    int64 `json:"model_context_window,omitempty"`
}

// IsZero supports encoding/json's omitzero handling on AgentNode.Usage.
func (u AgentUsage) IsZero() bool { return u == (AgentUsage{}) }

// Fresh reports whether the projected observation remains authoritative at
// now. Complete is intentionally independent of freshness.
func (g *AgentGraph) Fresh(now time.Time) bool {
	if g == nil || g.ObservedAt.IsZero() || g.FreshUntil.IsZero() {
		return false
	}
	return !now.Before(g.ObservedAt) && now.Before(g.FreshUntil)
}

// Clone returns a fully detached graph projection. The current nested structs
// carry no maps, pointers, or slices, so copying Nodes detaches the full value.
func (g *AgentGraph) Clone() *AgentGraph {
	if g == nil {
		return nil
	}
	clone := *g
	clone.Nodes = append([]AgentNode(nil), g.Nodes...)
	return &clone
}

// ProjectAgentGraph validates and deterministically orders a provider
// observation, reduces its summary, and returns a state-owned immutable value.
// prior may be nil; when it belongs to this root, its structured summary carries
// Summary.Since across identical observations.
func ProjectAgentGraph(observation agentgraph.Observation, prior *AgentGraph, now time.Time) (*AgentGraph, error) {
	normalized, err := agentgraph.Normalize(observation)
	if err != nil {
		return nil, err
	}
	var priorSummary agentgraph.Summary
	if prior != nil && prior.RootID == normalized.RootID {
		priorSummary = prior.domainSummary()
	}
	summary := agentgraph.Reduce(normalized, priorSummary, now)
	projection := &AgentGraph{
		RootID:     normalized.RootID,
		Source:     normalized.Source,
		ObservedAt: normalized.ObservedAt,
		FreshUntil: normalized.FreshUntil,
		Complete:   normalized.Complete,
		Summary:    projectSummary(summary),
		Nodes:      make([]AgentNode, len(normalized.Nodes)),
		provider:   normalized.Provider,
	}
	for i, node := range normalized.Nodes {
		projection.Nodes[i] = projectNode(node)
	}
	return projection, nil
}

// SetAgentGraph attaches a detached projection and updates the existing legacy
// enrichment view. Root ID and summary status are the only compatibility fields
// it changes: Claude in-flight/workflow/pending-writer data remains owned by the
// Claude adapter. Legacy StatusSince moves only when the legacy status changes,
// independently of structured count-only summary transitions.
func (s *Session) SetAgentGraph(graph *AgentGraph) {
	s.AgentGraph = graph.Clone()
	if s.AgentGraph == nil {
		return
	}
	kind := s.Agent
	if kind == "" {
		switch s.AgentGraph.provider {
		case agentgraph.ProviderClaude:
			kind = AgentKindClaude
		case agentgraph.ProviderCodex:
			kind = AgentKindCodex
		case agentgraph.ProviderUnknown:
			switch {
			case s.Codex != nil && s.Claude == nil:
				kind = AgentKindCodex
			case s.Claude != nil && s.Codex == nil:
				kind = AgentKindClaude
			}
		}
	}
	if kind != AgentKindClaude && kind != AgentKindCodex {
		return
	}
	info := s.AgentBlock(kind)
	info.SessionID = s.AgentGraph.RootID
	if info.Status != s.AgentGraph.Summary.Status {
		info.Status = s.AgentGraph.Summary.Status
		info.StatusSince = s.AgentGraph.Summary.Since
	}
}

func (g *AgentGraph) domainSummary() agentgraph.Summary {
	if g == nil {
		return agentgraph.Summary{}
	}
	return agentgraph.Summary{
		Runtime:        g.Summary.Runtime,
		Attention:      g.Summary.Attention,
		LegacyStatus:   g.Summary.Status,
		LiveChildren:   g.Summary.LiveChildren,
		WaitingNodes:   g.Summary.WaitingNodes,
		ApprovalNodes:  g.Summary.ApprovalNodes,
		UserInputNodes: g.Summary.UserInputNodes,
		ErrorNodes:     g.Summary.ErrorNodes,
		Since:          g.Summary.Since,
	}
}

func projectSummary(summary agentgraph.Summary) AgentGraphSummary {
	return AgentGraphSummary{
		Runtime:        summary.Runtime,
		Attention:      summary.Attention,
		Status:         summary.LegacyStatus,
		LiveChildren:   summary.LiveChildren,
		WaitingNodes:   summary.WaitingNodes,
		ApprovalNodes:  summary.ApprovalNodes,
		UserInputNodes: summary.UserInputNodes,
		ErrorNodes:     summary.ErrorNodes,
		Since:          summary.Since,
	}
}

func projectNode(node agentgraph.Node) AgentNode {
	return AgentNode{
		ID:          node.ID,
		ParentID:    node.ParentID,
		Nickname:    node.Nickname,
		Role:        node.Role,
		Description: node.Description,
		Runtime:     node.Runtime,
		Attention:   node.Attention,
		Lifecycle:   node.Lifecycle,
		StartedAt:   node.StartedAt,
		UpdatedAt:   node.UpdatedAt,
		CompletedAt: node.CompletedAt,
		Usage: AgentUsage{
			InputTokens:           node.Usage.InputTokens,
			CachedInputTokens:     node.Usage.CachedInputTokens,
			CacheWriteInputTokens: node.Usage.CacheWriteInputTokens,
			OutputTokens:          node.Usage.OutputTokens,
			ReasoningOutputTokens: node.Usage.ReasoningOutputTokens,
			TotalTokens:           node.Usage.TotalTokens,
			ModelContextWindow:    node.Usage.ModelContextWindow,
		},
	}
}

func (g *AgentGraph) observation(provider agentgraph.ProviderKind) agentgraph.Observation {
	observation := agentgraph.Observation{
		Provider:   provider,
		RootID:     g.RootID,
		Source:     g.Source,
		ObservedAt: g.ObservedAt,
		FreshUntil: g.FreshUntil,
		Complete:   g.Complete,
		Nodes:      make([]agentgraph.Node, len(g.Nodes)),
	}
	for i, node := range g.Nodes {
		observation.Nodes[i] = agentgraph.Node{
			ID:          node.ID,
			ParentID:    node.ParentID,
			Nickname:    node.Nickname,
			Role:        node.Role,
			Description: node.Description,
			Runtime:     node.Runtime,
			Attention:   node.Attention,
			Lifecycle:   node.Lifecycle,
			StartedAt:   node.StartedAt,
			UpdatedAt:   node.UpdatedAt,
			CompletedAt: node.CompletedAt,
			Usage: agentgraph.Usage{
				InputTokens:           node.Usage.InputTokens,
				CachedInputTokens:     node.Usage.CachedInputTokens,
				CacheWriteInputTokens: node.Usage.CacheWriteInputTokens,
				OutputTokens:          node.Usage.OutputTokens,
				ReasoningOutputTokens: node.Usage.ReasoningOutputTokens,
				TotalTokens:           node.Usage.TotalTokens,
				ModelContextWindow:    node.Usage.ModelContextWindow,
			},
		}
	}
	return observation
}

// hydrateAgentGraph validates an optional persisted graph independently of the
// legacy snapshot. A malformed additive graph is dropped rather than poisoning
// hydration of the root session. A valid expired graph retains its bounded node
// structure but reduces to unknown immediately, so restored red/green state can
// never outlive its freshness deadline.
func hydrateAgentGraph(sess *Session, now time.Time) {
	if sess.AgentGraph == nil {
		return
	}
	provider := agentgraph.ProviderKind(sess.Agent)
	projection, err := ProjectAgentGraph(sess.AgentGraph.observation(provider), sess.AgentGraph, now)
	if err != nil {
		sess.AgentGraph = nil
		return
	}
	sess.AgentGraph = projection
	var info *AgentInfo
	switch sess.Agent {
	case AgentKindClaude:
		info = sess.Claude
	case AgentKindCodex:
		info = sess.Codex
	}
	if info == nil {
		return
	}
	info.SessionID = projection.RootID
	info.Status = projection.Summary.Status
	// Preserve the pre-graph hydration contract: status_since is re-earned by
	// daemon reconciliation, never trusted across a restart.
	info.StatusSince = time.Time{}
	info.StatusSinceWire = nil
}
