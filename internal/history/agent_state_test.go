package history

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
)

func agentObservation(at time.Time) agentgraph.Observation {
	return agentgraph.Observation{
		Provider:   agentgraph.ProviderCodex,
		RootID:     "root-thread",
		Source:     agentgraph.SourceCodexAppServer,
		ObservedAt: at,
		FreshUntil: at.Add(time.Minute),
		Complete:   true,
		Nodes: []agentgraph.Node{
			{
				ID: "child-b", ParentID: "root-thread", Nickname: "documents", Role: "explorer",
				Runtime: agentgraph.RuntimeActive, Attention: agentgraph.AttentionNone,
				Lifecycle: agentgraph.LifecycleRunning, StartedAt: at.Add(-time.Minute), UpdatedAt: at,
			},
			{
				ID: "root-thread", Runtime: agentgraph.RuntimeIdle,
				Attention: agentgraph.AttentionNone, Lifecycle: agentgraph.LifecycleRunning,
				StartedAt: at.Add(-time.Hour), UpdatedAt: at,
			},
		},
	}
}

func TestAgentStateProjectorEmitsCanonicalTransitionsInGraphOrder(t *testing.T) {
	at := time.Date(2026, 8, 21, 16, 0, 0, 0, time.UTC)
	projector := NewAgentStateProjector()
	events, err := projector.Project(AgentStateContext{PID: 42, CWD: "/workspace"}, agentObservation(at), at)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want root + child: %+v", len(events), events)
	}
	if events[0].ThreadID != "root-thread" || events[1].ThreadID != "child-b" {
		t.Fatalf("event order = %q, %q, want graph preorder", events[0].ThreadID, events[1].ThreadID)
	}
	child := events[1]
	if child.Type != EventAgentState || child.SessionID != "root-thread" || child.Agent != "codex" || child.PID != 42 {
		t.Fatalf("canonical identity = %+v", child)
	}
	if child.ParentThreadID != "root-thread" || child.Nickname != "documents" || child.Role != "explorer" {
		t.Fatalf("node metadata = %+v", child)
	}
	if child.FromRuntime != agentgraph.RuntimeUnknown || child.ToRuntime != agentgraph.RuntimeActive ||
		child.FromAttention != agentgraph.AttentionNone || child.ToAttention != agentgraph.AttentionNone ||
		child.FromLifecycle != agentgraph.LifecycleUnknown || child.ToLifecycle != agentgraph.LifecycleRunning {
		t.Fatalf("axis transition = %+v", child)
	}
	if child.Source != agentgraph.SourceCodexAppServer || child.DurPrevMs != 0 {
		t.Fatalf("source/duration = %+v", child)
	}
}

func TestAgentStateProjectorPinsDurationAndReconnectDedupe(t *testing.T) {
	at := time.Date(2026, 8, 21, 16, 0, 0, 0, time.UTC)
	projector := NewAgentStateProjector()
	ctx := AgentStateContext{PID: 42, CWD: "/workspace"}
	obs := agentObservation(at)
	if events, err := projector.Project(ctx, obs, at); err != nil || len(events) != 2 {
		t.Fatalf("initial Project = (%+v, %v)", events, err)
	}

	// A reconnect/resnapshot can carry a new observation envelope while the
	// provider's node transition timestamps and states are unchanged.
	resnapshot := obs.Clone()
	resnapshot.ObservedAt = at.Add(10 * time.Second)
	resnapshot.FreshUntil = resnapshot.ObservedAt.Add(time.Minute)
	if events, err := projector.Project(ctx, resnapshot, resnapshot.ObservedAt); err != nil || len(events) != 0 {
		t.Fatalf("resnapshot Project = (%+v, %v), want no duplicate", events, err)
	}

	changedAt := at.Add(30 * time.Second)
	changed := resnapshot.Clone()
	changed.ObservedAt = changedAt
	changed.FreshUntil = changedAt.Add(time.Minute)
	changed.Nodes[0].Runtime = agentgraph.RuntimeIdle
	changed.Nodes[0].Attention = agentgraph.AttentionUserInput
	changed.Nodes[0].UpdatedAt = changedAt
	events, err := projector.Project(ctx, changed, changedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("changed events = %+v, want one child transition", events)
	}
	ev := events[0]
	if ev.ThreadID != "child-b" || ev.FromRuntime != agentgraph.RuntimeActive || ev.ToRuntime != agentgraph.RuntimeIdle ||
		ev.FromAttention != agentgraph.AttentionNone || ev.ToAttention != agentgraph.AttentionUserInput {
		t.Fatalf("changed axes = %+v", ev)
	}
	if ev.DurPrevMs != 30_000 || !ev.Ts.Equal(changedAt) {
		t.Fatalf("duration/ts = %d/%v, want 30000/%v", ev.DurPrevMs, ev.Ts, changedAt)
	}

	// An authoritative disappearance produces one not_found edge. Repeated
	// snapshots remain deduped, and replaying the original child transition after
	// reconnect is recognized by root/node/axis/target/transition-time identity.
	missingAt := changedAt.Add(10 * time.Second)
	missing := changed.Clone()
	missing.ObservedAt = missingAt
	missing.FreshUntil = missingAt.Add(time.Minute)
	missing.Nodes = missing.Nodes[1:] // normalized input root remains; child absent
	events, err = projector.Project(ctx, missing, missingAt)
	if err != nil || len(events) != 1 || events[0].ToLifecycle != agentgraph.LifecycleNotFound {
		t.Fatalf("missing Project = (%+v, %v), want one not_found", events, err)
	}
	missingAgain := missing.Clone()
	missingAgain.ObservedAt = missingAt.Add(time.Second)
	missingAgain.FreshUntil = missingAgain.ObservedAt.Add(time.Minute)
	if events, err := projector.Project(ctx, missingAgain, missingAgain.ObservedAt); err != nil || len(events) != 0 {
		t.Fatalf("repeated missing Project = (%+v, %v), want no duplicate", events, err)
	}

	replayed := obs.Clone()
	replayed.ObservedAt = missingAt.Add(2 * time.Second)
	replayed.FreshUntil = replayed.ObservedAt.Add(time.Minute)
	if events, err := projector.Project(ctx, replayed, replayed.ObservedAt); err != nil || len(events) != 0 {
		t.Fatalf("replayed transition Project = (%+v, %v), want dedupe", events, err)
	}
}

func TestAgentStateProjectorPreservesNodesMissingFromPartialObservation(t *testing.T) {
	at := time.Date(2026, 8, 21, 16, 0, 0, 0, time.UTC)
	projector := NewAgentStateProjector()
	ctx := AgentStateContext{PID: 42}
	obs := agentObservation(at)
	if _, err := projector.Project(ctx, obs, at); err != nil {
		t.Fatal(err)
	}
	partial := obs.Clone()
	partial.Complete = false
	partial.ObservedAt = at.Add(time.Second)
	partial.FreshUntil = partial.ObservedAt.Add(time.Minute)
	partial.Nodes = partial.Nodes[1:]
	if events, err := projector.Project(ctx, partial, partial.ObservedAt); err != nil || len(events) != 0 {
		t.Fatalf("partial omission = (%+v, %v), want no transition", events, err)
	}
}

func TestAgentStateProjectorRejectsInvalidAndIgnoresExpiredObservation(t *testing.T) {
	at := time.Date(2026, 8, 21, 16, 0, 0, 0, time.UTC)
	projector := NewAgentStateProjector()
	invalid := agentObservation(at)
	invalid.Nodes = invalid.Nodes[:1]
	if events, err := projector.Project(AgentStateContext{}, invalid, at); err == nil || len(events) != 0 {
		t.Fatalf("invalid Project = (%+v, %v), want error and no events", events, err)
	}
	expired := agentObservation(at)
	if events, err := projector.Project(AgentStateContext{}, expired, expired.FreshUntil); err != nil || len(events) != 0 {
		t.Fatalf("expired Project = (%+v, %v), want no events", events, err)
	}
}

func TestAgentStatePrivacyTiers(t *testing.T) {
	ts := time.Date(2026, 8, 21, 12, 0, 0, 0, time.Local)
	ev := Event{
		Ts: ts, Type: EventAgentState, SessionID: "root", Agent: "codex",
		ThreadID: "child", ParentThreadID: "root", Nickname: "secret nickname", Role: "custom role",
		FromRuntime: agentgraph.RuntimeIdle, ToRuntime: agentgraph.RuntimeActive,
		FromAttention: agentgraph.AttentionNone, ToAttention: agentgraph.AttentionNone,
		FromLifecycle: agentgraph.LifecyclePending, ToLifecycle: agentgraph.LifecycleRunning,
		Source: agentgraph.SourceCodexAppServer,
	}

	minimalDir := t.TempDir()
	minimal := NewSink(Config{Enabled: true, Detail: DetailMinimal, Dir: minimalDir})
	minimal.Record(ev)
	minimal.Close()
	gotMinimal := readDay(t, minimalDir, dayKey(ts))[0]
	if gotMinimal.Nickname != "" || gotMinimal.Role != "" {
		t.Fatalf("minimal history leaked display names: %+v", gotMinimal)
	}
	if gotMinimal.ThreadID != "child" || gotMinimal.ToRuntime != agentgraph.RuntimeActive || gotMinimal.Source != agentgraph.SourceCodexAppServer {
		t.Fatalf("minimal history lost canonical safe fields: %+v", gotMinimal)
	}

	fullDir := t.TempDir()
	full := NewSink(Config{Enabled: true, Detail: DetailFull, Dir: fullDir})
	full.Record(ev)
	full.Close()
	gotFull := readDay(t, fullDir, dayKey(ts))[0]
	if gotFull.Nickname != ev.Nickname || gotFull.Role != ev.Role {
		t.Fatalf("full history dropped permitted display names: %+v", gotFull)
	}
}

func TestAgentStateMinimalJSONGolden(t *testing.T) {
	ts := time.Date(2026, 8, 21, 16, 0, 30, 0, time.UTC)
	ev := Event{
		Ts: ts, Type: EventAgentState, SessionID: "root-thread", PID: 42,
		Agent: "codex", Project: "switchboard", DurPrevMs: 30_000,
		ThreadID: "child-thread", ParentThreadID: "root-thread",
		Nickname: "private nickname", Role: "private role",
		FromRuntime: agentgraph.RuntimeActive, ToRuntime: agentgraph.RuntimeIdle,
		FromAttention: agentgraph.AttentionNone, ToAttention: agentgraph.AttentionUserInput,
		FromLifecycle: agentgraph.LifecycleRunning, ToLifecycle: agentgraph.LifecycleRunning,
		Source: agentgraph.SourceCodexAppServer,
	}
	(&Sink{cfg: Config{Detail: DetailMinimal}}).scrub(&ev)
	got, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	want, err := os.ReadFile(filepath.Join("testdata", "agent-state-minimal.golden.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("agent_state golden mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestAgentStateProjectorForgetScopesDedupeToRootLifetime(t *testing.T) {
	at := time.Date(2026, 8, 21, 16, 0, 0, 0, time.UTC)
	projector := NewAgentStateProjector()
	obs := agentObservation(at)
	if events, err := projector.Project(AgentStateContext{}, obs, at); err != nil || len(events) != 2 {
		t.Fatalf("initial Project = (%+v, %v)", events, err)
	}
	projector.Forget(agentgraph.ProviderCodex, obs.RootID)
	if events, err := projector.Project(AgentStateContext{}, obs, at); err != nil || len(events) != 2 {
		t.Fatalf("Project after Forget = (%+v, %v), want fresh lifetime", events, err)
	}
}
