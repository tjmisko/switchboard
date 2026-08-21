package barlayout

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/state"
)

// AgentRow is presentation metadata for one non-root graph node. Graph nodes
// remain provider-neutral and non-switchable; renderers use Depth and
// TreePrefix only to show their place beneath the owning root session.
type AgentRow struct {
	Node       state.AgentNode
	Depth      int
	TreePrefix string
}

// AgentRows returns non-root nodes in the deterministic order carried by the
// state projection. It is defensive around malformed hand-written/older wire
// values: missing or cyclic parents get a shallow, bounded indentation rather
// than hanging or producing an unbounded line.
func AgentRows(graph *state.AgentGraph) []AgentRow {
	if graph == nil || len(graph.Nodes) == 0 {
		return nil
	}
	byID := make(map[string]state.AgentNode, len(graph.Nodes))
	lastChild := make(map[string]string, len(graph.Nodes))
	for _, node := range graph.Nodes {
		byID[node.ID] = node
		if node.ID != graph.RootID {
			lastChild[node.ParentID] = node.ID
		}
	}

	rows := make([]AgentRow, 0, len(graph.Nodes)-1)
	for _, node := range graph.Nodes {
		if node.ID == graph.RootID {
			continue
		}
		ancestors := make([]string, 0, 4)
		seen := map[string]struct{}{node.ID: {}}
		parentID := node.ParentID
		for parentID != "" && parentID != graph.RootID && len(ancestors) < len(graph.Nodes) {
			if _, duplicate := seen[parentID]; duplicate {
				break
			}
			seen[parentID] = struct{}{}
			parent, ok := byID[parentID]
			if !ok {
				break
			}
			ancestors = append(ancestors, parentID)
			parentID = parent.ParentID
		}
		for left, right := 0, len(ancestors)-1; left < right; left, right = left+1, right-1 {
			ancestors[left], ancestors[right] = ancestors[right], ancestors[left]
		}

		var prefix strings.Builder
		prefix.WriteString("  ")
		for _, ancestorID := range ancestors {
			ancestor := byID[ancestorID]
			if lastChild[ancestor.ParentID] == ancestorID {
				prefix.WriteString("   ")
			} else {
				prefix.WriteString("│  ")
			}
		}
		if lastChild[node.ParentID] == node.ID {
			prefix.WriteString("└─ ")
		} else {
			prefix.WriteString("├─ ")
		}

		depth := len(ancestors) + 1
		if parentID != graph.RootID {
			depth = 1
		}
		rows = append(rows, AgentRow{Node: node, Depth: depth, TreePrefix: prefix.String()})
	}
	return rows
}

// PrioritizeAgentRows returns a stable Waybar view: waits first, then active
// work, idle/unknown/error states, and terminal nodes. Relative graph order is
// retained inside each bucket.
func PrioritizeAgentRows(rows []AgentRow) []AgentRow {
	out := append([]AgentRow(nil), rows...)
	sort.SliceStable(out, func(i, j int) bool {
		return agentPriority(out[i].Node) < agentPriority(out[j].Node)
	})
	return out
}

// LimitAgentRows bounds a renderer's current-state output and reports how many
// rows were folded. A non-positive limit intentionally shows no rows.
func LimitAgentRows(rows []AgentRow, limit int) ([]AgentRow, int) {
	if limit < 0 {
		limit = 0
	}
	if len(rows) <= limit {
		return rows, 0
	}
	return rows[:limit], len(rows) - limit
}

// AgentName prefers user-facing identity and falls back to a short stable ID.
// It also caps provider-supplied names so a malformed value cannot defeat a
// narrow terminal or balloon a hover.
func AgentName(node state.AgentNode) string {
	name := node.Nickname
	if name == "" {
		name = node.Role
	}
	if name == "" {
		name = shortID(node.ID)
	}
	return truncateRunes(name, 36)
}

// AgentStateText returns the canonical human label for the node's current
// state. Terminal lifecycle wins over stale runtime/attention carried by a
// partial provider payload; live waits remain distinct.
func AgentStateText(node state.AgentNode) string {
	switch node.Lifecycle {
	case agentgraph.LifecycleCompleted:
		return "completed"
	case agentgraph.LifecycleInterrupted:
		return "interrupted"
	case agentgraph.LifecycleErrored:
		return "errored"
	case agentgraph.LifecycleShutdown:
		return "shutdown"
	case agentgraph.LifecycleNotFound:
		return "not found"
	}
	switch node.Attention {
	case agentgraph.AttentionApproval:
		return "approval"
	case agentgraph.AttentionUserInput:
		return "user input"
	}
	if node.Runtime == agentgraph.RuntimeSystemError {
		return "system error"
	}
	if node.Runtime == agentgraph.RuntimeNotLoaded {
		return "not loaded"
	}
	switch node.Lifecycle {
	case agentgraph.LifecyclePending:
		return "pending"
	case agentgraph.LifecycleRunning:
		if node.Runtime == agentgraph.RuntimeIdle {
			return "idle"
		}
		return "active"
	}
	switch node.Runtime {
	case agentgraph.RuntimeActive:
		return "active"
	case agentgraph.RuntimeIdle:
		return "idle"
	case agentgraph.RuntimeNotLoaded:
		return "not loaded"
	default:
		return "unknown"
	}
}

// AgentStateKind groups canonical state text into the legacy renderer palette.
// It deliberately does not derive or override the root chip summary.
func AgentStateKind(node state.AgentNode) string {
	if node.Lifecycle.Terminal() {
		if node.Lifecycle == agentgraph.LifecycleErrored {
			return "error"
		}
		return "terminal"
	}
	if node.Attention == agentgraph.AttentionApproval || node.Attention == agentgraph.AttentionUserInput {
		return "waiting"
	}
	if node.Runtime == agentgraph.RuntimeSystemError {
		return "error"
	}
	if node.Runtime == agentgraph.RuntimeNotLoaded {
		return "unknown"
	}
	if node.Lifecycle == agentgraph.LifecyclePending {
		return "active"
	}
	if node.Runtime == agentgraph.RuntimeIdle {
		return "idle"
	}
	if node.Runtime == agentgraph.RuntimeActive || node.Lifecycle == agentgraph.LifecycleRunning {
		return "active"
	}
	return "unknown"
}

// AgentStateAt returns the best available transition timestamp for the visible
// state. Terminal completion is most specific; UpdatedAt and StartedAt are the
// provider-neutral fallbacks.
func AgentStateAt(node state.AgentNode) time.Time {
	if node.Lifecycle.Terminal() && !node.CompletedAt.IsZero() {
		return node.CompletedAt
	}
	if !node.UpdatedAt.IsZero() {
		return node.UpdatedAt
	}
	return node.StartedAt
}

// AgentUsageText renders usage only when at least one measured value exists.
// TotalTokens is preferred; otherwise the available input/output components
// are shown without manufacturing zero-valued measurements.
func AgentUsageText(node state.AgentNode) string {
	u := node.Usage
	if u.TotalTokens > 0 {
		return fmt.Sprintf("%d tok", u.TotalTokens)
	}
	parts := make([]string, 0, 3)
	if u.InputTokens > 0 {
		parts = append(parts, fmt.Sprintf("%d in", u.InputTokens))
	}
	if u.CachedInputTokens > 0 {
		parts = append(parts, fmt.Sprintf("%d cached", u.CachedInputTokens))
	}
	if u.CacheWriteInputTokens > 0 {
		parts = append(parts, fmt.Sprintf("%d cache-write", u.CacheWriteInputTokens))
	}
	if u.OutputTokens > 0 {
		parts = append(parts, fmt.Sprintf("%d out", u.OutputTokens))
	}
	if u.ReasoningOutputTokens > 0 {
		parts = append(parts, fmt.Sprintf("%d reasoning", u.ReasoningOutputTokens))
	}
	if u.ModelContextWindow > 0 {
		parts = append(parts, fmt.Sprintf("ctx %d", u.ModelContextWindow))
	}
	return strings.Join(parts, "/")
}

func agentPriority(node state.AgentNode) int {
	if node.Lifecycle.Terminal() {
		return 3
	}
	if node.Attention == agentgraph.AttentionApproval || node.Attention == agentgraph.AttentionUserInput {
		return 0
	}
	if node.Runtime == agentgraph.RuntimeNotLoaded {
		return 2
	}
	if node.Lifecycle == agentgraph.LifecyclePending || node.Runtime == agentgraph.RuntimeActive || (node.Lifecycle == agentgraph.LifecycleRunning && node.Runtime != agentgraph.RuntimeIdle) {
		return 1
	}
	return 2
}

func shortID(id string) string {
	const max = 8
	if utf8.RuneCountInString(id) <= max {
		if id == "" {
			return "unknown"
		}
		return id
	}
	return string([]rune(id)[:max])
}

func truncateRunes(value string, max int) string {
	if utf8.RuneCountInString(value) <= max {
		return value
	}
	return string([]rune(value)[:max-1]) + "…"
}
