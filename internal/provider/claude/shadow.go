package claude

import (
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
)

// ShadowCase is a deterministic, content-free hook sequence with the legacy
// summary it must reproduce. C6 can replay these fixtures before flipping graph
// authority without importing RPC or daemon implementation details.
type ShadowCase struct {
	Name         string
	Hooks        []HookSignal
	LegacyStatus string
	LiveChildren int
	WaitingNodes int
}

// ShadowComparison is the rate-limit-friendly result C6 may log. Rule and
// statuses contain no user content.
type ShadowComparison struct {
	Match        bool
	Rule         string
	LegacyStatus string
	GraphStatus  string
	LiveChildren int
	WaitingNodes int
}

// CanonicalShadowCases returns detached compatibility-oracle fixtures.
func CanonicalShadowCases() []ShadowCase {
	cases := []ShadowCase{
		{Name: "root_idle", Hooks: []HookSignal{{Event: "SessionStart"}}, LegacyStatus: agentgraph.LegacyIdle},
		{Name: "root_working", Hooks: []HookSignal{{Event: "SessionStart"}, {Event: "UserPromptSubmit"}}, LegacyStatus: agentgraph.LegacyWorking},
		{Name: "main_approval", Hooks: []HookSignal{{Event: "SessionStart"}, {Event: "PermissionRequest", ToolName: "Bash"}}, LegacyStatus: agentgraph.LegacyPermission, WaitingNodes: 1},
		{Name: "child_question", Hooks: []HookSignal{{Event: "SessionStart"}, {Event: "PermissionRequest", AgentID: "child", ToolName: "AskUserQuestion"}}, LegacyStatus: agentgraph.LegacyPermission, LiveChildren: 1, WaitingNodes: 1},
		{Name: "simultaneous_writers", Hooks: []HookSignal{{Event: "SessionStart"}, {Event: "PermissionRequest", ToolName: "Edit"}, {Event: "PermissionRequest", AgentID: "child", ToolName: "Bash"}}, LegacyStatus: agentgraph.LegacyPermission, LiveChildren: 1, WaitingNodes: 2},
		{Name: "approved_resume", Hooks: []HookSignal{{Event: "SessionStart"}, {Event: "PermissionRequest", ToolName: "Bash"}, {Event: "PostToolUse", ToolName: "Bash"}}, LegacyStatus: agentgraph.LegacyWorking},
	}
	out := make([]ShadowCase, len(cases))
	for i, fixture := range cases {
		out[i] = fixture
		out[i].Hooks = append([]HookSignal(nil), fixture.Hooks...)
	}
	return out
}

// CompareShadow compares the neutral reducer with the simultaneously-produced
// legacy status. It never includes prompts, tools, paths, or descriptions.
func CompareShadow(legacyStatus string, observation agentgraph.Observation, prior agentgraph.Summary, now time.Time) ShadowComparison {
	summary := agentgraph.Reduce(observation, prior, now)
	comparison := ShadowComparison{
		Match:        legacyStatus == summary.LegacyStatus,
		LegacyStatus: legacyStatus, GraphStatus: summary.LegacyStatus,
		LiveChildren: summary.LiveChildren, WaitingNodes: summary.WaitingNodes,
		Rule: "claude_shadow_match",
	}
	if !comparison.Match {
		comparison.Rule = "claude_shadow_status_mismatch"
	}
	return comparison
}
