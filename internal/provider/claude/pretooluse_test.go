package claude

import (
	"slices"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/provider"
	"github.com/tjmisko/switchboard/internal/statustune"
)

func TestPreToolUseBindsQuestionAtOpenAndClearsRewrittenInputAtHookSpeed(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})

	staged := o.ApplyHook(HookSignal{
		Root: root, Event: "PreToolUse", ToolName: "AskUserQuestion",
		ToolInputHash: "ask-before", ToolUseID: "toolu_ask", At: now.Add(time.Second),
	})
	if !staged.Applied || staged.Changed || len(staged.Projection.Pending) != 0 {
		t.Fatalf("PreToolUse changed attention: %+v", staged)
	}
	assertSummary(t, staged.Observation, now.Add(time.Second), agentgraph.LegacyIdle, agentgraph.AttentionNone)
	assertPromptDiagnostic(t, o, root.Key(), DiagnosticPreToolStaged)

	opened := o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "AskUserQuestion",
		ToolInputHash: "ask-before", At: now.Add(2 * time.Second),
	})
	prompt := opened.Projection.Pending[""]
	if prompt.CallID != "toolu_ask" || prompt.Latch != CallLatchBound {
		t.Fatalf("prompt opened without staged identity: %+v", prompt)
	}
	assertPromptDiagnostic(t, o, root.Key(), DiagnosticPreToolJoinHit)

	cleared := o.ApplyHook(HookSignal{
		Root: root, Event: "PostToolUse", ToolName: "AskUserQuestion",
		ToolInputHash: "ask-after-rewrite", ToolUseID: "toolu_ask", At: now.Add(3 * time.Second),
	})
	if cleared.Rule != statustune.RuleGraphCallMatchCleared || cleared.PromptDepth != 0 {
		t.Fatalf("fast answer = rule %q depth %d, want exact call clear", cleared.Rule, cleared.PromptDepth)
	}
	assertSummary(t, cleared.Observation, now.Add(3*time.Second), agentgraph.LegacyWorking, agentgraph.AttentionNone)
}

func TestPreToolUseNeverOpensRedByItself(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})

	for i, tool := range []string{"AskUserQuestion", "ExitPlanMode"} {
		result := o.ApplyHook(HookSignal{
			Root: root, Event: "PreToolUse", ToolName: tool,
			ToolInputHash: "shape-" + tool, ToolUseID: "call-" + tool,
			At: now.Add(time.Duration(i+1) * time.Second),
		})
		if !result.Applied || result.Changed || len(result.Projection.Pending) != 0 {
			t.Fatalf("%s PreToolUse changed attention: %+v", tool, result)
		}
		assertSummary(t, result.Observation, now.Add(time.Duration(i+1)*time.Second), agentgraph.LegacyIdle, agentgraph.AttentionNone)
	}
}

func TestPreToolUseBindsExitPlanModeWhenItsInputHasIdentity(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
	o.ApplyHook(HookSignal{
		Root: root, Event: "PreToolUse", ToolName: "ExitPlanMode",
		ToolInputHash: "plan-shape", ToolUseID: "toolu_plan", At: now.Add(time.Second),
	})
	opened := o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "ExitPlanMode",
		ToolInputHash: "plan-shape", At: now.Add(2 * time.Second),
	})
	if prompt := opened.Projection.Pending[""]; prompt.CallID != "toolu_plan" || prompt.Latch != CallLatchBound {
		t.Fatalf("ExitPlanMode prompt did not bind at open: %+v", prompt)
	}
	cleared := o.ApplyHook(HookSignal{
		Root: root, Event: "PostToolUse", ToolName: "ExitPlanMode",
		ToolInputHash: "plan-after", ToolUseID: "toolu_plan", At: now.Add(3 * time.Second),
	})
	if cleared.Rule != statustune.RuleGraphCallMatchCleared || cleared.PromptDepth != 0 {
		t.Fatalf("ExitPlanMode completion = rule %q depth %d, want exact clear", cleared.Rule, cleared.PromptDepth)
	}
}

func TestPreToolUseMissingJoinFieldsFallsBackToUnbound(t *testing.T) {
	for _, tc := range []struct {
		name, hash, callID string
	}{
		{name: "missing hash", callID: "toolu_ask"},
		{name: "missing call id", hash: "ask-shape"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, root, now := newTestObserver(t)
			defer o.Close()
			o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
			o.ApplyHook(HookSignal{
				Root: root, Event: "PreToolUse", ToolName: "AskUserQuestion",
				ToolInputHash: tc.hash, ToolUseID: tc.callID, At: now.Add(time.Second),
			})
			opened := o.ApplyHook(HookSignal{
				Root: root, Event: "PermissionRequest", ToolName: "AskUserQuestion",
				ToolInputHash: "ask-shape", At: now.Add(2 * time.Second),
			})
			if prompt := opened.Projection.Pending[""]; prompt.CallID != "" || prompt.Latch != CallLatchUnbound {
				t.Fatalf("incomplete candidate bound: %+v", prompt)
			}
		})
	}
}

func TestPreToolUseModifiedInputFallsBackWithoutBinding(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
	o.ApplyHook(HookSignal{
		Root: root, Event: "PreToolUse", ToolName: "AskUserQuestion",
		ToolInputHash: "before", ToolUseID: "toolu_modified", At: now.Add(time.Second),
	})
	o.DrainPromptDiagnostics(root.Key())

	opened := o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "AskUserQuestion",
		ToolInputHash: "after", At: now.Add(2 * time.Second),
	})
	prompt := opened.Projection.Pending[""]
	if prompt.CallID != "" || prompt.Latch != CallLatchUnbound {
		t.Fatalf("modified input guessed an identity: %+v", prompt)
	}
	assertPromptDiagnostic(t, o, root.Key(), DiagnosticPreToolJoinMiss)

	held := o.ApplyHook(HookSignal{
		Root: root, Event: "PostToolUse", ToolName: "AskUserQuestion",
		ToolInputHash: "completion", ToolUseID: "toolu_modified", At: now.Add(3 * time.Second),
	})
	if held.PromptDepth != 1 || held.Projection.Status != agentgraph.LegacyPermission {
		t.Fatalf("unbound fallback cleared early: rule=%q projection=%+v", held.Rule, held.Projection)
	}
}

func TestPreToolUseParallelIdenticalCallsAreNeverGuessed(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
	for i, callID := range []string{"toolu_a", "toolu_b"} {
		o.ApplyHook(HookSignal{
			Root: root, Event: "PreToolUse", ToolName: "AskUserQuestion",
			ToolInputHash: "identical", ToolUseID: callID, At: now.Add(time.Duration(i+1) * time.Second),
		})
	}
	o.DrainPromptDiagnostics(root.Key())

	opened := o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "AskUserQuestion",
		ToolInputHash: "identical", At: now.Add(3 * time.Second),
	})
	prompt := opened.Projection.Pending[""]
	if prompt.CallID != "" || prompt.Latch != CallLatchUnbound {
		t.Fatalf("parallel identical calls were guessed: %+v", prompt)
	}
	assertPromptDiagnostic(t, o, root.Key(), DiagnosticPreToolAmbiguous)

	held := o.ApplyHook(HookSignal{
		Root: root, Event: "PostToolUse", ToolName: "AskUserQuestion",
		ToolInputHash: "rewritten", ToolUseID: "toolu_a", At: now.Add(4 * time.Second),
	})
	if held.PromptDepth != 1 || held.Projection.Status != agentgraph.LegacyPermission {
		t.Fatalf("ambiguous identity cleared the prompt: rule=%q projection=%+v", held.Rule, held.Projection)
	}
}

func TestPreToolUseSameShapeCallsStayWithinTheirWriters(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
	for i, tc := range []struct{ writer, callID string }{{"", "toolu_main"}, {"child", "toolu_child"}} {
		o.ApplyHook(HookSignal{
			Root: root, Event: "PreToolUse", AgentID: tc.writer, ToolName: "AskUserQuestion",
			ToolInputHash: "same", ToolUseID: tc.callID, At: now.Add(time.Duration(i+1) * time.Second),
		})
		o.ApplyHook(HookSignal{
			Root: root, Event: "PermissionRequest", AgentID: tc.writer, ToolName: "AskUserQuestion",
			ToolInputHash: "same", At: now.Add(time.Duration(i+3) * time.Second),
		})
	}
	sets := o.Projection(root.Key()).PendingSets
	if sets[""][0].CallID != "toolu_main" || sets["child"][0].CallID != "toolu_child" {
		t.Fatalf("writer-scoped joins = %+v", sets)
	}

	result := o.ApplyHook(HookSignal{
		Root: root, Event: "PostToolUse", AgentID: "child", ToolName: "AskUserQuestion",
		ToolInputHash: "rewritten", ToolUseID: "toolu_child", At: now.Add(6 * time.Second),
	})
	if len(result.Projection.PendingSets["child"]) != 0 || len(result.Projection.PendingSets[""]) != 1 {
		t.Fatalf("child completion crossed writers: %+v", result.Projection.PendingSets)
	}
}

func TestPreToolUseDuplicateDeliveryIsIdempotentButCallIDCollisionIsQuarantined(t *testing.T) {
	t.Run("duplicate", func(t *testing.T) {
		o, root, now := newTestObserver(t)
		defer o.Close()
		o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
		signal := HookSignal{
			Root: root, Event: "PreToolUse", ToolName: "AskUserQuestion",
			ToolInputHash: "same", ToolUseID: "toolu_same", At: now.Add(time.Second),
		}
		o.ApplyHook(signal)
		o.ApplyHook(signal)
		diagnostics := o.DrainPromptDiagnostics(root.Key())
		if countDiagnostic(diagnostics, DiagnosticPreToolStaged) != 1 {
			t.Fatalf("duplicate staging diagnostics = %v, want one staged edge", diagnostics)
		}
		opened := o.ApplyHook(HookSignal{
			Root: root, Event: "PermissionRequest", ToolName: "AskUserQuestion",
			ToolInputHash: "same", At: now.Add(2 * time.Second),
		})
		if prompt := opened.Projection.Pending[""]; prompt.CallID != "toolu_same" || prompt.Latch != CallLatchBound {
			t.Fatalf("duplicate delivery did not remain uniquely joinable: %+v", prompt)
		}
	})

	t.Run("collision", func(t *testing.T) {
		o, root, now := newTestObserver(t)
		defer o.Close()
		o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
		o.ApplyHook(HookSignal{
			Root: root, Event: "PreToolUse", ToolName: "AskUserQuestion",
			ToolInputHash: "main", ToolUseID: "toolu_collision", At: now.Add(time.Second),
		})
		o.ApplyHook(HookSignal{
			Root: root, Event: "PreToolUse", AgentID: "child", ToolName: "AskUserQuestion",
			ToolInputHash: "child", ToolUseID: "toolu_collision", At: now.Add(2 * time.Second),
		})
		assertPromptDiagnostic(t, o, root.Key(), DiagnosticPreToolCollision)

		opened := o.ApplyHook(HookSignal{
			Root: root, Event: "PermissionRequest", ToolName: "AskUserQuestion",
			ToolInputHash: "main", At: now.Add(3 * time.Second),
		})
		if prompt := opened.Projection.Pending[""]; prompt.CallID != "" || prompt.Latch != CallLatchUnbound {
			t.Fatalf("quarantined call id was rebound: %+v", prompt)
		}
	})

	t.Run("collision invalidates an existing claim", func(t *testing.T) {
		o, root, now := newTestObserver(t)
		defer o.Close()
		o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
		o.ApplyHook(HookSignal{
			Root: root, Event: "PreToolUse", ToolName: "AskUserQuestion",
			ToolInputHash: "main", ToolUseID: "toolu_collision", At: now.Add(time.Second),
		})
		o.ApplyHook(HookSignal{
			Root: root, Event: "PermissionRequest", ToolName: "AskUserQuestion",
			ToolInputHash: "main", At: now.Add(2 * time.Second),
		})
		o.ApplyHook(HookSignal{
			Root: root, Event: "PreToolUse", AgentID: "child", ToolName: "AskUserQuestion",
			ToolInputHash: "child", ToolUseID: "toolu_collision", At: now.Add(3 * time.Second),
		})
		prompt := o.Projection(root.Key()).Pending[""]
		if prompt.CallID != "" || prompt.Latch != CallLatchContested {
			t.Fatalf("existing colliding claim stayed usable: %+v", prompt)
		}
		result := o.ApplyHook(HookSignal{
			Root: root, Event: "PostToolUse", ToolName: "AskUserQuestion",
			ToolInputHash: "rewritten", ToolUseID: "toolu_collision", At: now.Add(4 * time.Second),
		})
		if result.PromptDepth != 1 || result.Projection.Status != agentgraph.LegacyPermission {
			t.Fatalf("quarantined id cleared its former prompt: rule=%q projection=%+v", result.Rule, result.Projection)
		}
	})
}

func TestPreToolUseCandidateExpiresAndSessionStartClearsIt(t *testing.T) {
	t.Run("expiry", func(t *testing.T) {
		o, root, now := newTestObserver(t)
		defer o.Close()
		o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
		o.ApplyHook(HookSignal{
			Root: root, Event: "PreToolUse", ToolName: "AskUserQuestion",
			ToolInputHash: "old", ToolUseID: "toolu_old", At: now.Add(time.Second),
		})
		o.DrainPromptDiagnostics(root.Key())
		opened := o.ApplyHook(HookSignal{
			Root: root, Event: "PermissionRequest", ToolName: "AskUserQuestion",
			ToolInputHash: "old", At: now.Add(62 * time.Second),
		})
		if prompt := opened.Projection.Pending[""]; prompt.CallID != "" || prompt.Latch != CallLatchUnbound {
			t.Fatalf("expired candidate bound: %+v", prompt)
		}
		diagnostics := o.DrainPromptDiagnostics(root.Key())
		if !slices.Contains(diagnostics, DiagnosticPreToolExpired) {
			t.Fatalf("expiry diagnostics = %v", diagnostics)
		}
	})

	t.Run("session start", func(t *testing.T) {
		o, root, now := newTestObserver(t)
		defer o.Close()
		o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
		o.ApplyHook(HookSignal{
			Root: root, Event: "PreToolUse", ToolName: "ExitPlanMode",
			ToolInputHash: "plan", ToolUseID: "toolu_plan", At: now.Add(time.Second),
		})
		o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now.Add(2 * time.Second)})
		opened := o.ApplyHook(HookSignal{
			Root: root, Event: "PermissionRequest", ToolName: "ExitPlanMode",
			ToolInputHash: "plan", At: now.Add(3 * time.Second),
		})
		if prompt := opened.Projection.Pending[""]; prompt.CallID != "" || prompt.Latch != CallLatchUnbound {
			t.Fatalf("candidate survived SessionStart: %+v", prompt)
		}
	})

	t.Run("session rotation", func(t *testing.T) {
		o, root, now := newTestObserver(t)
		defer o.Close()
		o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
		o.ApplyHook(HookSignal{
			Root: root, Event: "PreToolUse", ToolName: "AskUserQuestion",
			ToolInputHash: "old-session", ToolUseID: "toolu_old_session", At: now.Add(time.Second),
		})
		rotated := root
		rotated.ProviderSessionID = "rotated-session"
		opened := o.ApplyHook(HookSignal{
			Root: rotated, Event: "PermissionRequest", ToolName: "AskUserQuestion",
			ToolInputHash: "old-session", At: now.Add(2 * time.Second),
		})
		if prompt := opened.Projection.Pending[""]; prompt.CallID != "" || prompt.Latch != CallLatchUnbound {
			t.Fatalf("candidate crossed session rotation: %+v", prompt)
		}
	})
}

func TestPreToolUseAutoApprovedCompletionConsumesCandidate(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
	o.ApplyHook(HookSignal{
		Root: root, Event: "PreToolUse", ToolName: "AskUserQuestion",
		ToolInputHash: "shape", ToolUseID: "toolu_auto", At: now.Add(time.Second),
	})
	o.ApplyHook(HookSignal{
		Root: root, Event: "PostToolUse", ToolName: "AskUserQuestion",
		ToolInputHash: "shape", ToolUseID: "toolu_auto", At: now.Add(2 * time.Second),
	})
	opened := o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "AskUserQuestion",
		ToolInputHash: "shape", At: now.Add(3 * time.Second),
	})
	if prompt := opened.Projection.Pending[""]; prompt.CallID != "" || prompt.Latch != CallLatchUnbound {
		t.Fatalf("completed candidate was reused: %+v", prompt)
	}
}

func TestPreToolUseCandidatesAreBoundedAndIgnoreUnscopedTools(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})

	unscoped := o.ApplyHook(HookSignal{
		Root: root, Event: "PreToolUse", ToolName: "Bash",
		ToolInputHash: "bash", ToolUseID: "toolu_bash", At: now.Add(time.Second),
	})
	if !unscoped.Applied || unscoped.Changed || len(unscoped.Projection.Pending) != 0 {
		t.Fatalf("unscoped PreToolUse affected attention: %+v", unscoped)
	}

	for i := 0; i < maxPreToolCandidatesPerWriter+1; i++ {
		o.ApplyHook(HookSignal{
			Root: root, Event: "PreToolUse", ToolName: "AskUserQuestion",
			ToolInputHash: "shape-" + time.Duration(i).String(),
			ToolUseID:     "call-" + time.Duration(i).String(), At: now.Add(time.Duration(i+2) * time.Second),
		})
	}
	o.mu.Lock()
	candidates := append([]preToolCandidate(nil), o.roots[root.Key()].preToolCandidates[""]...)
	o.mu.Unlock()
	if len(candidates) != maxPreToolCandidatesPerWriter {
		t.Fatalf("candidate count = %d, want cap %d", len(candidates), maxPreToolCandidatesPerWriter)
	}
	if slices.ContainsFunc(candidates, func(candidate preToolCandidate) bool { return candidate.CallID == "call-0s" }) {
		t.Fatalf("oldest candidate survived cap: %+v", candidates[0])
	}
}

func assertPromptDiagnostic(t *testing.T, o *Observer, key provider.RootKey, want string) {
	t.Helper()
	diagnostics := o.DrainPromptDiagnostics(key)
	if !slices.Contains(diagnostics, want) {
		t.Fatalf("prompt diagnostics = %v, want %q", diagnostics, want)
	}
}

func countDiagnostic(diagnostics []string, want string) int {
	count := 0
	for _, diagnostic := range diagnostics {
		if diagnostic == want {
			count++
		}
	}
	return count
}
