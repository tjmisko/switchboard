package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	claudeprovider "github.com/tjmisko/switchboard/internal/provider/claude"
	codexprovider "github.com/tjmisko/switchboard/internal/provider/codex"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
)

type scriptedDisplayNameResult struct {
	name string
	err  error
	wait <-chan struct{}
}

type scriptedDisplayNamer struct {
	mu       sync.Mutex
	results  []scriptedDisplayNameResult
	inputs   []codexprovider.NamingContext
	models   []string
	started  chan int
	finished chan int
}

func (n *scriptedDisplayNamer) Generate(ctx context.Context, input codexprovider.NamingContext, model string) (string, error) {
	n.mu.Lock()
	call := len(n.inputs)
	n.inputs = append(n.inputs, input)
	n.models = append(n.models, model)
	result := scriptedDisplayNameResult{err: errors.New("unexpected naming call")}
	if call < len(n.results) {
		result = n.results[call]
	}
	n.mu.Unlock()
	if n.started != nil {
		select {
		case n.started <- call:
		default:
		}
	}
	defer func() {
		if n.finished != nil {
			select {
			case n.finished <- call:
			default:
			}
		}
	}()
	if result.wait != nil {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-result.wait:
		}
	}
	return result.name, result.err
}

func (n *scriptedDisplayNamer) snapshot() ([]codexprovider.NamingContext, []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]codexprovider.NamingContext(nil), n.inputs...), append([]string(nil), n.models...)
}

func waitForDisplayName(t *testing.T, store *state.Store, value string) *state.DisplayName {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if sessions := store.Snapshot().Sessions; len(sessions) == 1 && sessions[0].DisplayName != nil && sessions[0].DisplayName.Value == value {
			return sessions[0].DisplayName
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("display name did not become %q: %+v", value, store.Snapshot().Sessions)
	return nil
}

func waitForNoDisplayName(t *testing.T, store *state.Store) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		sessions := store.Snapshot().Sessions
		if len(sessions) == 0 || sessions[0].DisplayName == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("display name remained present: %+v", store.Snapshot().Sessions)
}

func waitForNamerCalls(t *testing.T, namer *scriptedDisplayNamer, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		inputs, _ := namer.snapshot()
		if len(inputs) == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	inputs, _ := namer.snapshot()
	t.Fatalf("naming calls = %d, want %d", len(inputs), want)
}

func diagnosticCount(coordinator *agentCoordinator, category string) uint64 {
	for _, diagnostic := range coordinator.Diagnostics() {
		if diagnostic.Provider == state.AgentKindCodex && diagnostic.Category == category {
			return diagnostic.Count
		}
	}
	return 0
}

func TestCodexDisplayNameStartsAfterMatchingCompletedTurnExactlyOnce(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	namer := &scriptedDisplayNamer{results: []scriptedDisplayNameResult{{name: "context-aware-session"}}}
	coordinator.SetCodexDisplayNamer(namer, "test-luna")
	base := time.Now()

	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "thread-1", TurnID: "turn-1",
		Prompt: "Implement completed-turn naming", ObservedAt: base,
	})
	if inputs, _ := namer.snapshot(); len(inputs) != 0 {
		t.Fatalf("naming started at prompt time: %+v", inputs)
	}
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: "thread-1", TurnID: "turn-other",
		LastAssistantMessage: "wrong turn", ObservedAt: base.Add(time.Millisecond),
	})
	if inputs, _ := namer.snapshot(); len(inputs) != 0 {
		t.Fatalf("mismatched stop started naming: %+v", inputs)
	}
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: "thread-1", TurnID: "turn-1",
		LastAssistantMessage: "Implemented and verified the lifecycle", ObservedAt: base.Add(2 * time.Millisecond),
	})
	record := waitForDisplayName(t, store, "context-aware-session")
	if record.Origin != state.DisplayNameGenerated || record.ConversationID != "thread-1" {
		t.Fatalf("display record = %+v", record)
	}
	inputs, models := namer.snapshot()
	if len(inputs) != 1 || inputs[0].CWDBase != "codex" || inputs[0].UserPrompt != "Implement completed-turn naming" ||
		inputs[0].AssistantResponse != "Implemented and verified the lifecycle" {
		t.Fatalf("structured naming context = %+v", inputs)
	}
	if len(models) != 1 || models[0] != "test-luna" {
		t.Fatalf("naming models = %v", models)
	}

	// Replayed and out-of-order Stop hooks cannot regenerate a committed name.
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: "thread-1", TurnID: "turn-1",
		LastAssistantMessage: "duplicate", ObservedAt: base.Add(3 * time.Millisecond),
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: "thread-1", TurnID: "turn-1",
		LastAssistantMessage: "out of order", ObservedAt: base.Add(-time.Millisecond),
	})
	waitForNamerCalls(t, namer, 1)
	if got := diagnosticCount(coordinator, "generated"); got != 1 {
		t.Fatalf("generated diagnostics = %d, want 1", got)
	}
}

func TestCodexDisplayNameAcceptsMissingTurnIDsButRequiresChronologicalStop(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	namer := &scriptedDisplayNamer{results: []scriptedDisplayNameResult{{name: "missing-turn-identity"}}}
	coordinator.SetCodexDisplayNamer(namer, "")
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "thread-1", Prompt: "Name this turn", ObservedAt: base,
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: "thread-1", LastAssistantMessage: "too early", ObservedAt: base,
	})
	if inputs, _ := namer.snapshot(); len(inputs) != 0 {
		t.Fatalf("non-later stop started naming: %+v", inputs)
	}
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: "thread-1", LastAssistantMessage: "completed later", ObservedAt: base.Add(time.Millisecond),
	})
	waitForDisplayName(t, store, "missing-turn-identity")
}

func TestCodexDisplayNameDiscardsEmptyStopAndReplacesIncompletePrompt(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	namer := &scriptedDisplayNamer{results: []scriptedDisplayNameResult{{name: "newer-prompt-wins"}}}
	coordinator.SetCodexDisplayNamer(namer, "")
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "thread-1", TurnID: "turn-empty", Prompt: "Interrupted request", ObservedAt: base,
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: "thread-1", TurnID: "turn-empty", LastAssistantMessage: "   ", ObservedAt: base.Add(time.Millisecond),
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: "thread-1", TurnID: "turn-empty", LastAssistantMessage: "late content", ObservedAt: base.Add(2 * time.Millisecond),
	})
	if inputs, _ := namer.snapshot(); len(inputs) != 0 {
		t.Fatalf("discarded candidate was reused: %+v", inputs)
	}

	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "thread-1", TurnID: "turn-old", Prompt: "Old incomplete prompt", ObservedAt: base.Add(3 * time.Millisecond),
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "thread-1", TurnID: "turn-new", Prompt: "New prompt replaces it", ObservedAt: base.Add(4 * time.Millisecond),
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "thread-1", TurnID: "turn-delayed", Prompt: "Delayed older prompt", ObservedAt: base.Add(3500 * time.Microsecond),
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: "thread-1", TurnID: "turn-old", LastAssistantMessage: "old completion", ObservedAt: base.Add(5 * time.Millisecond),
	})
	if inputs, _ := namer.snapshot(); len(inputs) != 0 {
		t.Fatalf("replaced prompt was completed: %+v", inputs)
	}
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: "thread-1", TurnID: "turn-new", LastAssistantMessage: "new completion", ObservedAt: base.Add(6 * time.Millisecond),
	})
	waitForDisplayName(t, store, "newer-prompt-wins")
	inputs, _ := namer.snapshot()
	if len(inputs) != 1 || inputs[0].UserPrompt != "New prompt replaces it" || inputs[0].AssistantResponse != "new completion" {
		t.Fatalf("replacement context = %+v", inputs)
	}
	if got := diagnosticCount(coordinator, "canceled"); got == 0 {
		t.Fatal("empty completion did not emit a content-free canceled diagnostic")
	}
}

func TestCodexDisplayNameRetriesThenUsesDeterministicFallback(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	namer := &scriptedDisplayNamer{results: []scriptedDisplayNameResult{
		{err: errors.New("first failure")}, {name: "one", err: nil},
	}}
	coordinator.SetCodexDisplayNamer(namer, "")
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "thread-1", TurnID: "turn-1",
		Prompt: "Please repair the observer reconnect path", ObservedAt: base,
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: "thread-1", TurnID: "turn-1",
		LastAssistantMessage: "Tests pass", ObservedAt: base.Add(time.Millisecond),
	})
	record := waitForDisplayName(t, store, "repair-observer-reconnect-path")
	if record.Origin != state.DisplayNameFallback {
		t.Fatalf("origin = %q, want fallback", record.Origin)
	}
	waitForNamerCalls(t, namer, 2)
	if got := diagnosticCount(coordinator, "fallback"); got != 1 {
		t.Fatalf("fallback diagnostics = %d, want 1", got)
	}
}

func TestCodexDisplayNameTimeoutsFallBackAfterTwoAttempts(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	coordinator.namingTimeout = 5 * time.Millisecond
	namer := &scriptedDisplayNamer{results: []scriptedDisplayNameResult{
		{wait: make(chan struct{})}, {wait: make(chan struct{})},
	}}
	coordinator.SetCodexDisplayNamer(namer, "")
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "thread-1", TurnID: "turn-1",
		Prompt: "Bound naming timeouts", ObservedAt: base,
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: "thread-1", TurnID: "turn-1",
		LastAssistantMessage: "Completed", ObservedAt: base.Add(time.Millisecond),
	})
	record := waitForDisplayName(t, store, "bound-naming-timeouts")
	if record.Origin != state.DisplayNameFallback {
		t.Fatalf("timeout origin = %q, want fallback", record.Origin)
	}
	waitForNamerCalls(t, namer, 2)
}

func TestCodexDisplayNameNewerCompletedAttemptCancelsOlderWork(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	blocked := make(chan struct{})
	namer := &scriptedDisplayNamer{
		results: []scriptedDisplayNameResult{{name: "stale-first-result", wait: blocked}, {name: "newer-completed-turn"}},
		started: make(chan int, 2), finished: make(chan int, 2),
	}
	coordinator.SetCodexDisplayNamer(namer, "")
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "thread-1", TurnID: "turn-1", Prompt: "First request", ObservedAt: base,
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: "thread-1", TurnID: "turn-1", LastAssistantMessage: "First response", ObservedAt: base.Add(time.Millisecond),
	})
	if call := <-namer.started; call != 0 {
		t.Fatalf("first started call = %d", call)
	}
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "thread-1", TurnID: "turn-2", Prompt: "Second request", ObservedAt: base.Add(2 * time.Millisecond),
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: "thread-1", TurnID: "turn-2", LastAssistantMessage: "Second response", ObservedAt: base.Add(3 * time.Millisecond),
	})
	waitForDisplayName(t, store, "newer-completed-turn")
	close(blocked)
	waitForNamerCalls(t, namer, 2)
	if got := store.Snapshot().Sessions[0].DisplayName.Value; got != "newer-completed-turn" {
		t.Fatalf("stale result replaced newer name: %q", got)
	}
	if got := diagnosticCount(coordinator, "canceled"); got == 0 {
		t.Fatal("superseded attempt did not emit canceled diagnostic")
	}
}

func TestCodexDisplayNameRejectsStaleResultWithoutReplacingCommittedRecord(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	release := make(chan struct{})
	namer := &scriptedDisplayNamer{
		results: []scriptedDisplayNameResult{{name: "late-generated-result", wait: release}},
		started: make(chan int, 1),
	}
	coordinator.SetCodexDisplayNamer(namer, "")
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "thread-1", TurnID: "turn-1", Prompt: "Race a result", ObservedAt: base,
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: "thread-1", TurnID: "turn-1", LastAssistantMessage: "Done", ObservedAt: base.Add(time.Millisecond),
	})
	<-namer.started
	store.Apply(func(sessions map[int]*state.Session) {
		sessions[8123].DisplayName = &state.DisplayName{
			Value: "already-committed-name", Origin: state.DisplayNameGenerated, ConversationID: "thread-1",
		}
	})
	close(release)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && diagnosticCount(coordinator, "stale-result") == 0 {
		time.Sleep(time.Millisecond)
	}
	if got := store.Snapshot().Sessions[0].DisplayName.Value; got != "already-committed-name" {
		t.Fatalf("stale result replaced committed name: %q", got)
	}
	if got := diagnosticCount(coordinator, "stale-result"); got != 1 {
		t.Fatalf("stale-result diagnostics = %d, want 1", got)
	}
}

func TestCodexDisplayNameCommitDropsAConcurrentPromptCandidate(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	release := make(chan struct{})
	namer := &scriptedDisplayNamer{
		results: []scriptedDisplayNameResult{{name: "first-completed-turn", wait: release}},
		started: make(chan int, 1),
	}
	coordinator.SetCodexDisplayNamer(namer, "")
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "thread-1", TurnID: "turn-1", Prompt: "First prompt", ObservedAt: base,
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: "thread-1", TurnID: "turn-1", LastAssistantMessage: "First response", ObservedAt: base.Add(time.Millisecond),
	})
	<-namer.started
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "thread-1", TurnID: "turn-2", Prompt: "Must not remain in memory", ObservedAt: base.Add(2 * time.Millisecond),
	})
	close(release)
	waitForDisplayName(t, store, "first-completed-turn")
	key := providerRootKey(store.Snapshot().Sessions[0])
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		coordinator.namingMu.Lock()
		_, retained := coordinator.naming[key]
		coordinator.namingMu.Unlock()
		if !retained {
			break
		}
		time.Sleep(time.Millisecond)
	}
	coordinator.namingMu.Lock()
	retained := len(coordinator.naming)
	coordinator.namingMu.Unlock()
	if retained != 0 {
		t.Fatalf("committed name retained %d transient naming candidate(s)", retained)
	}
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: "thread-1", TurnID: "turn-2", LastAssistantMessage: "Second response", ObservedAt: base.Add(3 * time.Millisecond),
	})
	waitForNamerCalls(t, namer, 1)
}

func TestCodexDisplayNameClearAndPIDReuseCancelPendingWork(t *testing.T) {
	t.Run("clear rotation", func(t *testing.T) {
		coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-old")
		namer := &scriptedDisplayNamer{
			results: []scriptedDisplayNameResult{{name: "must-not-commit", wait: make(chan struct{})}},
			started: make(chan int, 1), finished: make(chan int, 1),
		}
		coordinator.SetCodexDisplayNamer(namer, "")
		coordinator.codexStartSettle = time.Millisecond
		base := time.Now()
		sendCodexHook(coordinator, store, rpc.Request{
			Event: "UserPromptSubmit", SessionID: "thread-old", TurnID: "turn-1", Prompt: "Old request", ObservedAt: base,
		})
		sendCodexHook(coordinator, store, rpc.Request{
			Event: "Stop", SessionID: "thread-old", TurnID: "turn-1", LastAssistantMessage: "Old response", ObservedAt: base.Add(time.Millisecond),
		})
		<-namer.started
		sendCodexHook(coordinator, store, rpc.Request{
			Event: "SessionStart", HookSource: "clear", SessionID: "thread-new", ObservedAt: base.Add(2 * time.Millisecond),
		})
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) && conversationIDForSession(store.Snapshot().Sessions[0]) != "thread-new" {
			time.Sleep(time.Millisecond)
		}
		if got := conversationIDForSession(store.Snapshot().Sessions[0]); got != "thread-new" {
			t.Fatalf("conversation after clear = %q", got)
		}
		select {
		case <-namer.finished:
		case <-time.After(time.Second):
			t.Fatal("clear did not cancel pending namer")
		}
		waitForNoDisplayName(t, store)
	})

	t.Run("process death", func(t *testing.T) {
		coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-old")
		namer := &scriptedDisplayNamer{
			results:  []scriptedDisplayNameResult{{name: "must-not-commit", wait: make(chan struct{})}},
			started:  make(chan int, 1),
			finished: make(chan int, 1),
		}
		coordinator.SetCodexDisplayNamer(namer, "")
		base := time.Now()
		sendCodexHook(coordinator, store, rpc.Request{
			Event: "UserPromptSubmit", SessionID: "thread-old", TurnID: "turn-1", Prompt: "Dying process", ObservedAt: base,
		})
		sendCodexHook(coordinator, store, rpc.Request{
			Event: "Stop", SessionID: "thread-old", TurnID: "turn-1", LastAssistantMessage: "Done", ObservedAt: base.Add(time.Millisecond),
		})
		<-namer.started
		store.Apply(func(sessions map[int]*state.Session) { delete(sessions, 8123) })
		coordinator.refreshTrackedRoots()
		select {
		case <-namer.finished:
		case <-time.After(time.Second):
			t.Fatal("process death did not cancel pending namer")
		}
		if sessions := store.Snapshot().Sessions; len(sessions) != 0 {
			t.Fatalf("dead process remained in state: %+v", sessions)
		}
	})

	t.Run("pid reuse", func(t *testing.T) {
		coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-old")
		namer := &scriptedDisplayNamer{
			results: []scriptedDisplayNameResult{{name: "must-not-commit", wait: make(chan struct{})}},
			started: make(chan int, 1), finished: make(chan int, 1),
		}
		coordinator.SetCodexDisplayNamer(namer, "")
		base := time.Now()
		sendCodexHook(coordinator, store, rpc.Request{
			Event: "UserPromptSubmit", SessionID: "thread-old", TurnID: "turn-1", Prompt: "Old process", ObservedAt: base,
		})
		sendCodexHook(coordinator, store, rpc.Request{
			Event: "Stop", SessionID: "thread-old", TurnID: "turn-1", LastAssistantMessage: "Old result", ObservedAt: base.Add(time.Millisecond),
		})
		<-namer.started
		store.Apply(func(sessions map[int]*state.Session) {
			sessions[8123] = &state.Session{
				PID: 8123, StartedAt: base.Add(time.Hour), Agent: state.AgentKindCodex, CWD: "/new-process",
				Codex: &state.AgentInfo{SessionID: "thread-new"},
			}
		})
		coordinator.refreshTrackedRoots()
		select {
		case <-namer.finished:
		case <-time.After(time.Second):
			t.Fatal("PID reuse did not cancel old process lifetime")
		}
		waitForNoDisplayName(t, store)
	})
}

func TestCodexDisplayNameDaemonShutdownCancelsPendingWork(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	namer := &scriptedDisplayNamer{
		results:  []scriptedDisplayNameResult{{name: "must-not-commit", wait: make(chan struct{})}},
		started:  make(chan int, 1),
		finished: make(chan int, 1),
	}
	coordinator.SetCodexDisplayNamer(namer, "")
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "thread-1", TurnID: "turn-1", Prompt: "Pending at shutdown", ObservedAt: base,
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: "thread-1", TurnID: "turn-1", LastAssistantMessage: "Complete", ObservedAt: base.Add(time.Millisecond),
	})
	<-namer.started
	coordinator.Close()
	select {
	case <-namer.finished:
	case <-time.After(time.Second):
		t.Fatal("daemon shutdown did not cancel pending namer")
	}
	waitForNoDisplayName(t, store)
}

func TestCodexDisplayNameNativeBaselineAndRenamePrecedence(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	namer := &scriptedDisplayNamer{results: []scriptedDisplayNameResult{{name: "generated-display-label"}}}
	coordinator.SetCodexDisplayNamer(namer, "")
	ref, _ := providerRootRef(store.Snapshot().Sessions[0])
	base := time.Now()

	initial := testCodexObservation(ref, "thread-1", base, agentgraph.RuntimeIdle, agentgraph.AttentionNone)
	initial.Nodes[0].Nickname = "session-naming"
	if !coordinator.applyObservation(ref, coordinator.begin(ref.Key()), initial, claudeprovider.Compatibility{}, base) {
		t.Fatal("initial authoritative native observation was not applied")
	}
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "thread-1", TurnID: "turn-1", Prompt: "Implement naming", ObservedAt: base.Add(time.Millisecond),
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: "thread-1", TurnID: "turn-1", LastAssistantMessage: "Implemented", ObservedAt: base.Add(2 * time.Millisecond),
	})
	waitForDisplayName(t, store, "generated-display-label")

	// The Stop hook is a partial overlay. A complete snapshot with the same
	// native title captures the baseline and keeps the generated label visible.
	same := testCodexObservation(ref, "thread-1", base.Add(3*time.Millisecond), agentgraph.RuntimeIdle, agentgraph.AttentionNone)
	same.Nodes[0].Nickname = "session-naming"
	if !coordinator.applyObservation(ref, coordinator.begin(ref.Key()), same, claudeprovider.Compatibility{}, same.ObservedAt) {
		t.Fatal("same-name authoritative observation was not applied")
	}
	record := store.Snapshot().Sessions[0].DisplayName
	if record == nil || record.NativeBaseline == nil || *record.NativeBaseline != "session-naming" {
		t.Fatalf("native baseline = %+v", record)
	}

	partial := testCodexObservation(ref, "thread-1", base.Add(4*time.Millisecond), agentgraph.RuntimeActive, agentgraph.AttentionNone)
	partial.Source = agentgraph.SourceHook
	partial.Complete = false
	partial.Nodes[0].Nickname = "partial-fabrication"
	if !coordinator.applyObservationWithHookOwnership(ref, coordinator.begin(ref.Key()), partial, claudeprovider.Compatibility{}, partial.ObservedAt, true) {
		t.Fatal("partial hook observation was not applied")
	}
	if got := store.Snapshot().Sessions[0].DisplayName; got == nil {
		t.Fatal("partial hook graph cleared the display name")
	}

	renamed := testCodexObservation(ref, "thread-1", base.Add(5*time.Millisecond), agentgraph.RuntimeIdle, agentgraph.AttentionNone)
	renamed.Nodes[0].Nickname = "manual-native-name"
	if !coordinator.applyObservation(ref, coordinator.begin(ref.Key()), renamed, claudeprovider.Compatibility{}, renamed.ObservedAt) {
		t.Fatal("native rename observation was not applied")
	}
	waitForNoDisplayName(t, store)
	if got := diagnosticCount(coordinator, "native-override"); got != 1 {
		t.Fatalf("native-override diagnostics = %d, want 1", got)
	}
}

func TestCodexDisplayNameBoundsContextAndNeverLeaksRawContent(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	namer := &scriptedDisplayNamer{results: []scriptedDisplayNameResult{{name: "bounded-private-context"}}}
	coordinator.SetCodexDisplayNamer(namer, "")
	secretPrompt := "PROMPT_SECRET_" + strings.Repeat("界", 1200)
	secretResponse := "RESPONSE_SECRET_" + strings.Repeat("🙂", 1200)
	var logs bytes.Buffer
	priorWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(priorWriter) })
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "thread-1", TurnID: "turn-1", Prompt: secretPrompt, ObservedAt: base,
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: "thread-1", TurnID: "turn-1", LastAssistantMessage: secretResponse, ObservedAt: base.Add(time.Millisecond),
	})
	waitForDisplayName(t, store, "bounded-private-context")
	inputs, _ := namer.snapshot()
	if len(inputs) != 1 || len([]rune(inputs[0].UserPrompt)) != 1000 || len([]rune(inputs[0].AssistantResponse)) != 1000 {
		t.Fatalf("bounded context lengths = prompt %d response %d", len([]rune(inputs[0].UserPrompt)), len([]rune(inputs[0].AssistantResponse)))
	}
	wire, err := json.Marshal(struct {
		State       state.Snapshot        `json:"state"`
		Diagnostics []rpc.AgentDiagnostic `json:"diagnostics"`
	}{store.Snapshot(), coordinator.Diagnostics()})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"PROMPT_SECRET_", "RESPONSE_SECRET_"} {
		if bytes.Contains(wire, []byte(forbidden)) || strings.Contains(logs.String(), forbidden) {
			t.Fatalf("raw naming context %q leaked: wire=%s logs=%s", forbidden, wire, logs.String())
		}
	}
}
