package barlayout

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/state"
)

func TestAgentRowsPreserveWireOrderAndDepth(t *testing.T) {
	graph := &state.AgentGraph{RootID: "root", Nodes: []state.AgentNode{
		{ID: "root"},
		{ID: "alpha", ParentID: "root", Nickname: "alpha"},
		{ID: "nested", ParentID: "alpha", Role: "reviewer"},
		{ID: "omega", ParentID: "root", Nickname: "omega"},
	}}
	rows := AgentRows(graph)
	if len(rows) != 3 {
		t.Fatalf("AgentRows returned %d rows, want 3", len(rows))
	}
	if rows[0].Node.ID != "alpha" || rows[1].Node.ID != "nested" || rows[2].Node.ID != "omega" {
		t.Fatalf("AgentRows changed wire order: %#v", rows)
	}
	if rows[0].Depth != 1 || rows[1].Depth != 2 || rows[2].Depth != 1 {
		t.Fatalf("depths = %d,%d,%d, want 1,2,1", rows[0].Depth, rows[1].Depth, rows[2].Depth)
	}
	if rows[0].TreePrefix != "  ├─ " || rows[1].TreePrefix != "  │  └─ " || rows[2].TreePrefix != "  └─ " {
		t.Fatalf("tree prefixes = %q, %q, %q", rows[0].TreePrefix, rows[1].TreePrefix, rows[2].TreePrefix)
	}
}

func TestPrioritizeAgentRowsIsStableWithinStatusBuckets(t *testing.T) {
	rows := []AgentRow{
		{Node: state.AgentNode{ID: "idle-a", Runtime: agentgraph.RuntimeIdle}},
		{Node: state.AgentNode{ID: "active-a", Runtime: agentgraph.RuntimeActive}},
		{Node: state.AgentNode{ID: "wait", Attention: agentgraph.AttentionUserInput}},
		{Node: state.AgentNode{ID: "active-b", Lifecycle: agentgraph.LifecycleRunning}},
		{Node: state.AgentNode{ID: "done", Lifecycle: agentgraph.LifecycleCompleted}},
	}
	got := PrioritizeAgentRows(rows)
	want := []string{"wait", "active-a", "active-b", "idle-a", "done"}
	for i := range want {
		if got[i].Node.ID != want[i] {
			t.Fatalf("priority order[%d] = %q, want %q", i, got[i].Node.ID, want[i])
		}
	}
}

func TestAgentPresentationFallbacksStateTimeAndUsage(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	node := state.AgentNode{
		ID: "1234567890abcdef", Role: "reviewer", Runtime: agentgraph.RuntimeActive,
		UpdatedAt: now, Usage: state.AgentUsage{InputTokens: 20, OutputTokens: 4},
	}
	if got := AgentName(node); got != "reviewer" {
		t.Fatalf("AgentName(role) = %q", got)
	}
	if got := AgentStateText(node); got != "active" {
		t.Fatalf("AgentStateText = %q", got)
	}
	if got := AgentStateAt(node); !got.Equal(now) {
		t.Fatalf("AgentStateAt = %v", got)
	}
	if got := AgentUsageText(node); got != "20 in/4 out" {
		t.Fatalf("AgentUsageText = %q", got)
	}
	node.Role = ""
	if got := AgentName(node); got != "12345678" {
		t.Fatalf("AgentName(ID) = %q", got)
	}
	if got := AgentUsageText(state.AgentNode{}); got != "" {
		t.Fatalf("missing usage rendered as %q", got)
	}
	node.Usage = state.AgentUsage{CachedInputTokens: 10}
	if got := AgentUsageText(node); got != "10 cached" {
		t.Fatalf("cache-only usage rendered as %q", got)
	}
	node.Nickname = strings.Repeat("x", 50)
	if got := AgentName(node); utf8.RuneCountInString(got) != 36 || !strings.HasSuffix(got, "…") {
		t.Fatalf("long narrow-terminal name = %q (%d runes)", got, utf8.RuneCountInString(got))
	}
}

func TestAgentStateTextKeepsWaitsErrorsAndTerminalDistinct(t *testing.T) {
	tests := []struct {
		node state.AgentNode
		want string
	}{
		{state.AgentNode{Attention: agentgraph.AttentionApproval}, "approval"},
		{state.AgentNode{Attention: agentgraph.AttentionUserInput}, "user input"},
		{state.AgentNode{Runtime: agentgraph.RuntimeSystemError}, "system error"},
		{state.AgentNode{Runtime: agentgraph.RuntimeNotLoaded}, "not loaded"},
		{state.AgentNode{Lifecycle: agentgraph.LifecycleCompleted}, "completed"},
	}
	for _, tt := range tests {
		if got := AgentStateText(tt.node); got != tt.want {
			t.Errorf("AgentStateText(%+v) = %q, want %q", tt.node, got, tt.want)
		}
	}
	if got := AgentStateKind(state.AgentNode{Runtime: agentgraph.RuntimeIdle, Lifecycle: agentgraph.LifecycleRunning}); got != "idle" {
		t.Errorf("idle running-lifecycle node kind = %q, want idle", got)
	}
}

func TestLimitAgentRowsReportsFoldedCount(t *testing.T) {
	rows := []AgentRow{{}, {}, {}, {}}
	got, folded := LimitAgentRows(rows, 2)
	if len(got) != 2 || folded != 2 {
		t.Fatalf("LimitAgentRows = %d rows, %d folded", len(got), folded)
	}
}
