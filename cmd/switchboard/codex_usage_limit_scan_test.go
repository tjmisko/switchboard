package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/provider"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/tailcache"
)

// rolloutActive is a turn marker that says the thread is still working.
func rolloutActive(at time.Time) string {
	return fmt.Sprintf("{\"timestamp\":%q,\"type\":\"event_msg\",\"payload\":{\"type\":\"agent_message\"}}\n", at.Format(time.RFC3339Nano))
}

// rolloutComplete is a turn that ended normally.
func rolloutComplete(at time.Time) string {
	return fmt.Sprintf("{\"timestamp\":%q,\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\",\"last_agent_message\":null}}\n", at.Format(time.RFC3339Nano))
}

// rolloutCapped is the shape Codex writes when the usage limit ends a turn:
// the exhausted window's telemetry, then task_complete carrying the error.
func rolloutCapped(at, resetsAt time.Time) string {
	return fmt.Sprintf("{\"timestamp\":%q,\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"rate_limits\":{\"limit_id\":\"codex\",\"primary\":{\"used_percent\":100,\"window_minutes\":300,\"resets_at\":%d}}}}\n",
		at.Add(-100*time.Millisecond).Format(time.RFC3339Nano), resetsAt.Unix()) +
		fmt.Sprintf("{\"timestamp\":%q,\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\",\"last_agent_message\":null,\"error\":{\"message\":\"You’ve hit your usage limit.\",\"codex_error_info\":\"usage_limit_exceeded\"}}}\n",
			at.Format(time.RFC3339Nano))
}

func writeRollout(t *testing.T, dir, name string, rows ...string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	text := ""
	for _, row := range rows {
		text += row
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func codexRootRef(t *testing.T, store *state.Store) provider.RootRef {
	t.Helper()
	ref, ok := providerRootRef(store.Snapshot().Sessions[0])
	if !ok {
		t.Fatal("missing provider root")
	}
	return ref
}

func requireLimited(t *testing.T, store *state.Store, resetsAt time.Time) {
	t.Helper()
	published := store.PublishedSnapshot().Sessions[0]
	if published.Codex == nil || published.Codex.Status != state.StatusLimited {
		t.Fatalf("published codex block = %+v, want limited", published.Codex)
	}
	if published.UsageLimit == nil || published.UsageLimit.ResetsAt == nil || !published.UsageLimit.ResetsAt.Equal(resetsAt) {
		t.Fatalf("usage_limit = %+v, want resets_at %v", published.UsageLimit, resetsAt)
	}
}

func requireNotLimited(t *testing.T, store *state.Store) {
	t.Helper()
	if published := store.PublishedSnapshot().Sessions[0]; published.UsageLimit != nil {
		t.Fatalf("usage_limit = %+v, want none", published.UsageLimit)
	}
}

func TestCodexUsageLimitScanShouldLimitTheSessionWhenASubagentIsCappedWhileTheRootWaits(t *testing.T) {
	c, store := newStandardCodexHookTestCoordinator(t, "root")
	now := time.Now()
	resetsAt := now.Add(3 * time.Hour).Truncate(time.Second)
	dir := t.TempDir()
	// The root spawned the child and parked in wait_agent: its newest marker
	// predates the child's cap, and it writes no turn end of its own.
	rootPath := writeRollout(t, dir, "root.jsonl", rolloutActive(now))
	childPath := writeRollout(t, dir, "child.jsonl", rolloutActive(now.Add(time.Second)), rolloutCapped(now.Add(2*time.Second), resetsAt))
	sendCodexHook(c, store, rpc.Request{Event: "PostToolUse", SessionID: "root", ToolName: "spawn_agent", Transcript: rootPath, ObservedAt: now})
	sendCodexHook(c, store, rpc.Request{Event: "PostToolUse", SessionID: "root", AgentID: "child", ToolName: "exec_command", Transcript: childPath, ObservedAt: now.Add(time.Second)})

	c.observeAt(context.Background(), codexRootRef(t, store), now.Add(95*time.Second))

	requireLimited(t, store, resetsAt)
}

func TestCodexUsageLimitScanShouldIgnoreASubagentCapWhenTheRootMovedOnAfterIt(t *testing.T) {
	c, store := newStandardCodexHookTestCoordinator(t, "root")
	now := time.Now()
	dir := t.TempDir()
	rootPath := writeRollout(t, dir, "root.jsonl", rolloutActive(now), rolloutActive(now.Add(5*time.Second)))
	childPath := writeRollout(t, dir, "child.jsonl", rolloutCapped(now.Add(2*time.Second), now.Add(3*time.Hour)))
	sendCodexHook(c, store, rpc.Request{Event: "PostToolUse", SessionID: "root", ToolName: "spawn_agent", Transcript: rootPath, ObservedAt: now})
	sendCodexHook(c, store, rpc.Request{Event: "PostToolUse", SessionID: "root", AgentID: "child", ToolName: "exec_command", Transcript: childPath, ObservedAt: now.Add(time.Second)})

	c.observeAt(context.Background(), codexRootRef(t, store), now.Add(95*time.Second))

	requireNotLimited(t, store)
}

// A hook fired inside a subagent carries the root's session_id and the child's
// transcript_path (captured 2026-10-05). It must not replace the root's
// rollout, or the root's own cap is read from the wrong file.
func TestCodexUsageLimitScanShouldReadTheRootRolloutWhenTheLastHookCameFromASubagent(t *testing.T) {
	c, store := newStandardCodexHookTestCoordinator(t, "root")
	now := time.Now()
	resetsAt := now.Add(3 * time.Hour).Truncate(time.Second)
	dir := t.TempDir()
	rootPath := writeRollout(t, dir, "root.jsonl", rolloutActive(now), rolloutCapped(now.Add(4*time.Second), resetsAt))
	childPath := writeRollout(t, dir, "child.jsonl", rolloutComplete(now.Add(2*time.Second)))
	sendCodexHook(c, store, rpc.Request{Event: "PostToolUse", SessionID: "root", ToolName: "spawn_agent", Transcript: rootPath, ObservedAt: now})
	sendCodexHook(c, store, rpc.Request{Event: "PostToolUse", SessionID: "root", AgentID: "child", ToolName: "exec_command", Transcript: childPath, ObservedAt: now.Add(time.Second)})

	c.codexHookMu.Lock()
	root := c.codexHookRoots[codexRootRef(t, store).Key()]
	gotRoot, gotChild := root.transcript, root.childTranscripts["child"]
	c.codexHookMu.Unlock()
	if gotRoot != rootPath || gotChild != childPath {
		t.Fatalf("root transcript = %q, child = %q; want %q, %q", gotRoot, gotChild, rootPath, childPath)
	}

	c.observeAt(context.Background(), codexRootRef(t, store), now.Add(95*time.Second))

	requireLimited(t, store, resetsAt)
}

func TestCodexUsageLimitScanShouldWaitWhenHooksAreInsideTheQuietWindow(t *testing.T) {
	c, store := newStandardCodexHookTestCoordinator(t, "root")
	now := time.Now()
	dir := t.TempDir()
	rootPath := writeRollout(t, dir, "root.jsonl", rolloutActive(now))
	childPath := writeRollout(t, dir, "child.jsonl", rolloutCapped(now.Add(2*time.Second), now.Add(3*time.Hour)))
	sendCodexHook(c, store, rpc.Request{Event: "PostToolUse", SessionID: "root", ToolName: "spawn_agent", Transcript: rootPath, ObservedAt: now})
	sendCodexHook(c, store, rpc.Request{Event: "PostToolUse", SessionID: "root", AgentID: "child", ToolName: "exec_command", Transcript: childPath, ObservedAt: now.Add(time.Second)})

	c.observeAt(context.Background(), codexRootRef(t, store), now.Add(30*time.Second))

	requireNotLimited(t, store)
}

func TestCodexUsageLimitScanShouldIgnoreACapOlderThanTheNewestHook(t *testing.T) {
	c, store := newStandardCodexHookTestCoordinator(t, "root")
	now := time.Now()
	dir := t.TempDir()
	rootPath := writeRollout(t, dir, "root.jsonl", rolloutActive(now))
	childPath := writeRollout(t, dir, "child.jsonl", rolloutCapped(now.Add(2*time.Second), now.Add(3*time.Hour)))
	sendCodexHook(c, store, rpc.Request{Event: "PostToolUse", SessionID: "root", AgentID: "child", ToolName: "exec_command", Transcript: childPath, ObservedAt: now.Add(time.Second)})
	// The root kept firing hooks after the child's cap, so the cap is not why
	// it went quiet.
	sendCodexHook(c, store, rpc.Request{Event: "PostToolUse", SessionID: "root", ToolName: "exec_command", Transcript: rootPath, ObservedAt: now.Add(10 * time.Second)})

	c.observeAt(context.Background(), codexRootRef(t, store), now.Add(105*time.Second))

	requireNotLimited(t, store)
}

// After a daemon restart no hook has bound a rollout, so the app-server's
// thread.path is the only way to find a cap already on disk.
func TestCodexUsageLimitScanShouldFindACapFromAppServerPathsWhenNoHookArrivedSinceRestart(t *testing.T) {
	now := time.Now()
	resetsAt := now.Add(2 * time.Hour).Truncate(time.Second)
	for _, tt := range []struct {
		name        string
		root, child []string
	}{
		{name: "root capped", root: []string{rolloutActive(now.Add(-time.Hour)), rolloutCapped(now.Add(-30*time.Minute), resetsAt)}, child: []string{rolloutComplete(now.Add(-40 * time.Minute))}},
		{name: "child capped under a waiting root", root: []string{rolloutActive(now.Add(-time.Hour))}, child: []string{rolloutCapped(now.Add(-30*time.Minute), resetsAt)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := state.New("")
			ref := seedCoordinatorSession(store, 4077, now.Add(-2*time.Hour), state.AgentKindCodex, "root", "/codex")
			fake := newFakeCodexCoordinatorObserver()
			dir := t.TempDir()
			fake.threadPaths = map[string]string{
				"root":  writeRollout(t, dir, "root.jsonl", tt.root...),
				"child": writeRollout(t, dir, "child.jsonl", tt.child...),
			}
			fake.observations[ref.Key()] = testCodexObservation(ref, "root", now, agentgraph.RuntimeNotLoaded, agentgraph.AttentionNone)
			c := newAgentCoordinator(store, nil, nil, fake)
			c.refreshTrackedRoots()
			defer c.Close()

			// The first tick lands the app-server graph the scan keys on; the
			// second, past the scan interval, reads the rollouts.
			c.observeAt(context.Background(), codexRootRef(t, store), now)
			c.observeAt(context.Background(), codexRootRef(t, store), now.Add(codexUsageLimitScanInterval))

			requireLimited(t, store, resetsAt)
		})
	}
}

func TestCodexUsageLimitScanShouldReuseATailWhenTheRolloutIsUnchanged(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}
	now := time.Now()
	path := writeRollout(t, t.TempDir(), "child.jsonl", rolloutCapped(now, now.Add(time.Hour)))
	t.Cleanup(tailcache.SetDefault(tailcache.New(tailcache.OS{}, 0)))
	if got := readCodexRolloutState(path); got.UsageLimit == nil {
		t.Fatalf("first read = %+v, want the cap", got)
	}
	// Unreadable but unchanged: only a cached verdict can still name the cap.
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	if got := readCodexRolloutState(path); got.UsageLimit == nil {
		t.Fatalf("second read = %+v, want the cached cap without re-reading", got)
	}
	if stats := tailcache.Default().Stats(); stats.Reads != 1 || stats.Hits != 1 {
		t.Fatalf("cache work = %+v, want one read and one hit", stats)
	}
}
