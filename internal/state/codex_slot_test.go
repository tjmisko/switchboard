package state

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func TestCodexSlotConversationRotationAndStaleFence(t *testing.T) {
	slot := &CodexSlot{SlotID: "slot"}
	now := time.Unix(100, 0)
	rotated, stale := slot.BindConversation("one", now)
	if !rotated || stale || slot.Conversation.Generation != 1 {
		t.Fatalf("first bind = rotated %t stale %t slot %+v", rotated, stale, slot)
	}
	slot.SetConversationName("first-name", NameOriginUser)
	rotated, stale = slot.BindConversation("two", now.Add(time.Second))
	if !rotated || stale || slot.Conversation.Generation != 2 || slot.Conversation.Name != "" {
		t.Fatalf("rotation = rotated %t stale %t slot %+v", rotated, stale, slot)
	}
	if len(slot.Retired) != 1 || slot.Retired[0].Name != "first-name" || slot.Retired[0].NameOrigin != NameOriginUser {
		t.Fatalf("retired history = %+v", slot.Retired)
	}
	rotated, stale = slot.BindConversation("one", now.Add(2*time.Second))
	if rotated || !stale || slot.Conversation.ThreadID != "two" || slot.Conversation.Generation != 2 {
		t.Fatalf("stale old thread rotated slot backwards: rotated %t stale %t slot %+v", rotated, stale, slot)
	}
}

func TestCodexSlotFencesEveryRetiredConversationForSlotLifetime(t *testing.T) {
	slot := &CodexSlot{SlotID: "slot"}
	now := time.Unix(100, 0)
	for i := 0; i < 24; i++ {
		rotated, stale := slot.BindConversation(fmt.Sprintf("thread-%d", i), now.Add(time.Duration(i)*time.Second))
		if !rotated || stale {
			t.Fatalf("bind %d = rotated %t stale %t", i, rotated, stale)
		}
	}
	if got := len(slot.Retired); got != 23 {
		t.Fatalf("retired conversations = %d, want 23", got)
	}
	rotated, stale := slot.BindConversation("thread-0", now.Add(time.Minute))
	if rotated || !stale || slot.Conversation.ThreadID != "thread-23" {
		t.Fatalf("oldest retired thread escaped fence: rotated %t stale %t current %+v", rotated, stale, slot.Conversation)
	}
}

func TestCodexSlotRejectsOutOfOrderNeverBoundThread(t *testing.T) {
	slot := &CodexSlot{SlotID: "slot"}
	if rotated, stale := slot.BindConversation("thread-a", time.Unix(10, 0)); !rotated || stale {
		t.Fatalf("first bind = rotated %t stale %t", rotated, stale)
	}
	if rotated, stale := slot.BindConversation("thread-c", time.Unix(30, 0)); !rotated || stale {
		t.Fatalf("newest bind = rotated %t stale %t", rotated, stale)
	}
	rotated, stale := slot.BindConversation("thread-b", time.Unix(20, 0))
	if rotated || !stale || slot.Conversation.ThreadID != "thread-c" || slot.Conversation.Generation != 2 {
		t.Fatalf("out-of-order unseen thread rotated backwards: rotated %t stale %t current %+v", rotated, stale, slot.Conversation)
	}
}

func TestStoreRemovesSlotWhenVisibleSessionDies(t *testing.T) {
	store := New("")
	started := time.Unix(100, 0)
	store.ApplyState(func(sessions map[int]*Session, slots map[string]*CodexSlot) {
		sessions[42] = &Session{PID: 42, StartedAt: started, Agent: AgentKindCodex, SlotID: "slot"}
		slots["slot"] = &CodexSlot{SlotID: "slot", PID: 42, StartedAt: started, Endpoint: "unix:///tmp/slot.sock"}
	})
	if len(store.Snapshot().Slots) != 1 {
		t.Fatal("registered slot was pruned while its session was live")
	}
	store.Apply(func(sessions map[int]*Session) { delete(sessions, 42) })
	if slots := store.Snapshot().Slots; len(slots) != 0 {
		t.Fatalf("dead TUI retained stale slot: %+v", slots)
	}
}

func TestLoadCleanlyResetsUnversionedCodexState(t *testing.T) {
	path := t.TempDir() + "/state.json"
	if err := os.WriteFile(path, []byte(`{"sessions":[{"pid":7,"started_at":"2026-01-01T00:00:00Z","agent":"codex","codex":{"session_id":"old","status":"working"}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := New(path)
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	if len(store.Snapshot().Sessions) != 0 {
		t.Fatal("incompatible process-bound Codex mirror was migrated")
	}
}
