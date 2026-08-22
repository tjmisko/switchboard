package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/provider"
	claudeprovider "github.com/tjmisko/switchboard/internal/provider/claude"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
)

const testSlotID = "a0f75199-5591-4db6-8468-573b7f1d8ef7"

func seedCodexSlot(t *testing.T, store *state.Store, pid int, started time.Time, threadID string) provider.RootRef {
	t.Helper()
	store.ApplyState(func(sessions map[int]*state.Session, slots map[string]*state.CodexSlot) {
		slot := &state.CodexSlot{SlotID: testSlotID, Endpoint: "unix:///tmp/fake.sock", PID: pid, StartedAt: started}
		slot.BindConversation(threadID, started)
		slot.SetConversationName("retired-name", state.NameOriginUser)
		slots[testSlotID] = slot
		sessions[pid] = &state.Session{
			PID: pid, StartedAt: started, Agent: state.AgentKindCodex, SlotID: testSlotID, CWD: "/work/project",
			Codex: &state.AgentInfo{SessionID: threadID, Status: state.StatusPermission},
			AgentGraph: &state.AgentGraph{RootID: threadID, Summary: state.AgentGraphSummary{Status: state.StatusPermission}, Nodes: []state.AgentNode{
				{ID: threadID, Nickname: "retired-name", Runtime: agentgraph.RuntimeIdle, Attention: agentgraph.AttentionApproval, Lifecycle: agentgraph.LifecycleRunning},
				{ID: "old-child", ParentID: threadID, Runtime: agentgraph.RuntimeActive, Lifecycle: agentgraph.LifecycleRunning},
			}},
		}
	})
	ref, ok := providerRootRef(store.Snapshot().Sessions[0])
	if !ok {
		t.Fatal("seeded slot did not produce a root ref")
	}
	return ref
}

func TestCodexClearRotatesStableSlotAppliesDiscoveringHookAndRejectsStale(t *testing.T) {
	store := state.New("")
	started := time.Unix(100, 0)
	seedCodexSlot(t, store, 77, started, "old-thread")
	observer := newFakeCodexCoordinatorObserver()
	sink := history.NewSink(history.Config{})
	coordinator := newAgentCoordinator(store, sink, nil, observer)
	defer coordinator.Close()

	session := store.Snapshot().Sessions[0]
	coordinator.HandleHook(rpc.Request{Agent: state.AgentKindCodex, SlotID: testSlotID, SessionID: "new-thread", Generation: 2, Event: "UserPromptSubmit"}, session)
	snap := store.Snapshot()
	if len(snap.Sessions) != 1 || snap.Sessions[0].PID != 77 || snap.Sessions[0].SlotID != testSlotID {
		t.Fatalf("visible slot changed across clear: %+v", snap.Sessions)
	}
	slot := snap.Slots[0]
	if slot.Conversation.ThreadID != "new-thread" || slot.Conversation.Generation != 2 {
		t.Fatalf("conversation binding = %+v", slot.Conversation)
	}
	if len(slot.Retired) != 1 || slot.Retired[0].ThreadID != "old-thread" || slot.Retired[0].Name != "retired-name" {
		t.Fatalf("retired history = %+v", slot.Retired)
	}
	sess := snap.Sessions[0]
	if sess.Codex.Status != state.StatusWorking || sess.AgentGraph == nil || sess.AgentGraph.RootID != "new-thread" || len(sess.AgentGraph.Nodes) != 1 {
		t.Fatalf("discovering hook was not applied to cleared state: %+v", sess)
	}

	coordinator.HandleHook(rpc.Request{Agent: state.AgentKindCodex, SlotID: testSlotID, SessionID: "old-thread", Generation: 1, Event: "Stop"}, sess)
	afterStale := store.Snapshot()
	if afterStale.Slots[0].Conversation.ThreadID != "new-thread" || afterStale.Sessions[0].Codex.Status != state.StatusWorking {
		t.Fatalf("retired hook mutated live conversation: %+v", afterStale)
	}

	coordinator.HandleHook(rpc.Request{Agent: state.AgentKindCodex, SlotID: testSlotID, SessionID: "new-thread", Generation: 2, Event: "Stop"}, afterStale.Sessions[0])
	if got := store.Snapshot().Sessions[0].Codex.Status; got != state.StatusIdle {
		t.Fatalf("current-generation stop status = %q", got)
	}
}

func TestCodexRapidClearRejectsOutOfOrderIntermediateThread(t *testing.T) {
	store := state.New("")
	started := time.Unix(100, 0)
	seedCodexSlot(t, store, 81, started, "thread-a")
	coordinator := newAgentCoordinator(store, history.NewSink(history.Config{}), nil, newFakeCodexCoordinatorObserver())
	defer coordinator.Close()

	coordinator.HandleHook(rpc.Request{
		Agent: state.AgentKindCodex, SlotID: testSlotID, SessionID: "thread-c",
		Event: "UserPromptSubmit", ObservedAt: time.Unix(130, 0),
	}, store.Snapshot().Sessions[0])
	coordinator.HandleHook(rpc.Request{
		Agent: state.AgentKindCodex, SlotID: testSlotID, SessionID: "thread-b",
		Event: "Stop", ObservedAt: time.Unix(120, 0),
	}, store.Snapshot().Sessions[0])

	slot := store.Snapshot().Slots[0]
	if slot.Conversation.ThreadID != "thread-c" || slot.Conversation.Generation != 2 {
		t.Fatalf("out-of-order intermediate clear rotated backwards: %+v", slot)
	}
	if got := store.Snapshot().Sessions[0].Codex.Status; got != state.StatusWorking {
		t.Fatalf("stale intermediate hook changed status to %q", got)
	}
}

type scriptedNamer struct {
	mu      sync.Mutex
	results []string
	errs    []error
	calls   int
	entered chan struct{}
	block   bool
}

func (n *scriptedNamer) Generate(ctx context.Context, _, _, _ string) (string, error) {
	n.mu.Lock()
	i := n.calls
	n.calls++
	entered, block := n.entered, n.block
	var result string
	var err error
	if i < len(n.results) {
		result = n.results[i]
	}
	if i < len(n.errs) {
		err = n.errs[i]
	}
	n.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return result, err
}

func TestCodexAutonameGeneratedFallbackAndExplicitRenameSuppression(t *testing.T) {
	for _, test := range []struct {
		name   string
		namer  *scriptedNamer
		want   string
		origin state.NameOrigin
	}{
		{"generated", &scriptedNamer{results: []string{"Fix The RPC Status!"}}, "fix-the-rpc-status", state.NameOriginGenerated},
		{"fallback after retry", &scriptedNamer{errs: []error{errors.New("transient"), errors.New("transient")}}, "fix-flaky-status-colors", state.NameOriginFallback},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := state.New("")
			seedCodexSlot(t, store, 78, time.Unix(100, 0), "thread")
			store.ApplyState(func(_ map[int]*state.Session, slots map[string]*state.CodexSlot) {
				slots[testSlotID].SetConversationName("", "")
			})
			observer := newFakeCodexCoordinatorObserver()
			coordinator := newAgentCoordinator(store, history.NewSink(history.Config{}), nil, observer)
			coordinator.SetCodexAutonamer(test.namer, "test-model")
			defer coordinator.Close()
			coordinator.HandleHook(rpc.Request{Agent: state.AgentKindCodex, SlotID: testSlotID, SessionID: "thread", Event: "UserPromptSubmit", Prompt: "fix flaky status colors"}, store.Snapshot().Sessions[0])
			waitForSlotName(t, store, test.want)
			slot := store.Snapshot().Slots[0]
			if slot.Conversation.NameOrigin != test.origin {
				t.Fatalf("name origin = %q, want %q", slot.Conversation.NameOrigin, test.origin)
			}
			coordinator.HandleHook(rpc.Request{
				Agent: state.AgentKindCodex, SlotID: testSlotID, SessionID: "thread",
				Event: "UserPromptSubmit", Prompt: "a later prompt",
			}, store.Snapshot().Sessions[0])
			if after := store.Snapshot().Slots[0]; after.Conversation.NameOrigin != test.origin || after.Autoname != slot.Autoname {
				t.Fatalf("later prompt changed settled naming state: before=%+v after=%+v", slot, after)
			}
			call := <-observer.nameCalls
			if call.threadID != "thread" || call.generation != 1 || call.name != test.want {
				t.Fatalf("thread/name/set = %+v", call)
			}
			ref, ok := providerRootRef(store.Snapshot().Sessions[0])
			if !ok {
				t.Fatal("generated slot lost its provider root")
			}
			rename := testCodexObservation(ref, "thread", time.Now(), agentgraph.RuntimeIdle, agentgraph.AttentionNone)
			rename.Nodes[0].Nickname = "explicit-after-generated"
			coordinator.applyObservation(ref, coordinator.begin(ref.Key()), rename, claudeprovider.Compatibility{}, time.Now())
			renamed := store.Snapshot().Slots[0].Conversation
			if renamed.Name != "explicit-after-generated" || renamed.NameOrigin != state.NameOriginUser {
				t.Fatalf("rename after autoname = %+v", renamed)
			}
			coordinator.startAutoname(ref, "thread", "manual retry must not overwrite", true)
			if after := store.Snapshot().Slots[0]; after.Conversation.Name != "explicit-after-generated" || after.Autoname != state.AutonameSuppressed {
				t.Fatalf("manual autoname overwrote explicit name: %+v", after)
			}
		})
	}

	store := state.New("")
	ref := seedCodexSlot(t, store, 79, time.Unix(100, 0), "thread")
	store.ApplyState(func(_ map[int]*state.Session, slots map[string]*state.CodexSlot) {
		slots[testSlotID].SetConversationName("", "")
	})
	observer := newFakeCodexCoordinatorObserver()
	blocking := &scriptedNamer{entered: make(chan struct{}, 1), block: true}
	coordinator := newAgentCoordinator(store, history.NewSink(history.Config{}), nil, observer)
	coordinator.SetCodexAutonamer(blocking, "test-model")
	defer coordinator.Close()
	coordinator.HandleHook(rpc.Request{Agent: state.AgentKindCodex, SlotID: testSlotID, SessionID: "thread", Event: "UserPromptSubmit", Prompt: "generate a title"}, store.Snapshot().Sessions[0])
	<-blocking.entered
	observation := testCodexObservation(ref, "thread", time.Now(), agentgraph.RuntimeIdle, agentgraph.AttentionNone)
	observation.Nodes[0].Nickname = "explicit-user-name"
	coordinator.applyObservation(ref, coordinator.begin(ref.Key()), observation, claudeprovider.Compatibility{}, time.Now())
	slot := store.Snapshot().Slots[0]
	if slot.Conversation.Name != "explicit-user-name" || slot.Conversation.NameOrigin != state.NameOriginUser || slot.Autoname != state.AutonameSuppressed {
		t.Fatalf("explicit rename did not suppress pending autoname: %+v", slot)
	}
}

func TestCodexClearCancelsPendingAutonameByGeneration(t *testing.T) {
	store := state.New("")
	seedCodexSlot(t, store, 80, time.Unix(100, 0), "old-thread")
	store.ApplyState(func(_ map[int]*state.Session, slots map[string]*state.CodexSlot) {
		slots[testSlotID].SetConversationName("", "")
	})
	observer := newFakeCodexCoordinatorObserver()
	blocking := &scriptedNamer{entered: make(chan struct{}, 1), block: true}
	coordinator := newAgentCoordinator(store, history.NewSink(history.Config{}), nil, observer)
	coordinator.SetCodexAutonamer(blocking, "test-model")
	defer coordinator.Close()

	coordinator.HandleHook(rpc.Request{Agent: state.AgentKindCodex, SlotID: testSlotID, SessionID: "old-thread", Event: "UserPromptSubmit", Prompt: "name the old conversation"}, store.Snapshot().Sessions[0])
	select {
	case <-blocking.entered:
	case <-time.After(time.Second):
		t.Fatal("autonamer did not start")
	}
	coordinator.HandleHook(rpc.Request{Agent: state.AgentKindCodex, SlotID: testSlotID, SessionID: "new-thread", Event: "SessionStart"}, store.Snapshot().Sessions[0])

	deadline := time.Now().Add(time.Second)
	for {
		slot := store.Snapshot().Slots[0]
		if slot.Conversation.ThreadID == "new-thread" && slot.Conversation.Generation == 2 && slot.Autoname == state.AutonameNone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("clear did not reset naming generation: %+v", slot)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case call := <-observer.nameCalls:
		t.Fatalf("retired generation wrote a name: %+v", call)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestCodexAutonameFailureWaitsForManualRetry(t *testing.T) {
	store := state.New("")
	seedCodexSlot(t, store, 82, time.Unix(100, 0), "thread")
	store.ApplyState(func(_ map[int]*state.Session, slots map[string]*state.CodexSlot) {
		slots[testSlotID].SetConversationName("", "")
	})
	observer := newFakeCodexCoordinatorObserver()
	observer.nameErrors = make(chan error, 2)
	observer.nameErrors <- errors.New("endpoint unavailable")
	observer.nameErrors <- nil
	namer := &scriptedNamer{results: []string{"first-name", "manual-name"}}
	coordinator := newAgentCoordinator(store, history.NewSink(history.Config{}), nil, observer)
	coordinator.SetCodexAutonamer(namer, "test-model")
	defer coordinator.Close()

	coordinator.HandleHook(rpc.Request{
		Agent: state.AgentKindCodex, SlotID: testSlotID, SessionID: "thread",
		Event: "UserPromptSubmit", Prompt: "first prompt",
	}, store.Snapshot().Sessions[0])
	waitForAutonameDiagnostic(t, store, "autoname name update failed")
	coordinator.HandleHook(rpc.Request{
		Agent: state.AgentKindCodex, SlotID: testSlotID, SessionID: "thread",
		Event: "UserPromptSubmit", Prompt: "later prompt",
	}, store.Snapshot().Sessions[0])
	time.Sleep(20 * time.Millisecond)
	if calls := autonameCallCount(namer); calls != 1 {
		t.Fatalf("later prompt retried autoname automatically: calls=%d", calls)
	}
	if err := coordinator.HandleCodexSlot(rpc.Request{Cmd: "autoname", SlotID: testSlotID}); err != nil {
		t.Fatal(err)
	}
	waitForSlotName(t, store, "manual-name")
}

func autonameCallCount(namer *scriptedNamer) int {
	namer.mu.Lock()
	defer namer.mu.Unlock()
	return namer.calls
}

func waitForAutonameDiagnostic(t *testing.T, store *state.Store, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		slots := store.Snapshot().Slots
		if len(slots) > 0 && slots[0].Diagnostic == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot never reached diagnostic %q: %+v", want, slots)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCodexSlotRegistrationAndUnregistration(t *testing.T) {
	store := state.New("")
	coordinator := newAgentCoordinator(store, history.NewSink(history.Config{}), nil, newFakeCodexCoordinatorObserver())
	defer coordinator.Close()
	started := time.Unix(200, 0).UTC()
	if err := coordinator.HandleCodexSlot(rpc.Request{
		Cmd: "codex_slot_register", SlotID: testSlotID, Endpoint: "unix:///tmp/slot.sock", PID: 901, StartedAt: started,
	}); err != nil {
		t.Fatal(err)
	}
	snap := store.Snapshot()
	if len(snap.Slots) != 1 || len(snap.Sessions) != 1 || snap.Slots[0].PID != 901 || snap.Sessions[0].SlotID != testSlotID {
		t.Fatalf("registration snapshot = %+v", snap)
	}
	if snap.Slots[0].Conversation != nil || snap.Slots[0].Diagnostic != "slot alive but no thread bound" {
		t.Fatalf("new slot binding = %+v", snap.Slots[0])
	}
	store.ApplyState(func(_ map[int]*state.Session, slots map[string]*state.CodexSlot) {
		slots[testSlotID].BindConversation("thread", started.Add(time.Second))
		slots[testSlotID].SetConversationName("kept-name", state.NameOriginUser)
	})
	if err := coordinator.HandleCodexSlot(rpc.Request{
		Cmd: "codex_slot_register", SlotID: testSlotID, Endpoint: "unix:///tmp/slot.sock", PID: 901, StartedAt: started,
	}); err != nil {
		t.Fatalf("idempotent registration retry: %v", err)
	}
	if got := store.Snapshot().Slots[0].Conversation; got == nil || got.ThreadID != "thread" || got.Name != "kept-name" {
		t.Fatalf("registration retry erased binding: %+v", got)
	}
	if err := coordinator.HandleCodexSlot(rpc.Request{
		Cmd: "codex_slot_register", SlotID: testSlotID, Endpoint: "unix:///tmp/other.sock", PID: 902, StartedAt: started,
	}); err == nil {
		t.Fatal("conflicting registration was accepted")
	}
	if err := coordinator.HandleCodexSlot(rpc.Request{Cmd: "codex_slot_unregister", SlotID: testSlotID}); err != nil {
		t.Fatal(err)
	}
	snap = store.Snapshot()
	if len(snap.Slots) != 0 || snap.Sessions[0].SlotID != "" {
		t.Fatalf("unregistration snapshot = %+v", snap)
	}
}

func waitForSlotName(t *testing.T, store *state.Store, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		snap := store.Snapshot()
		if len(snap.Slots) > 0 && snap.Slots[0].Conversation != nil && snap.Slots[0].Conversation.Name == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot never reached name %q: %+v", want, snap.Slots)
		}
		time.Sleep(time.Millisecond)
	}
}
