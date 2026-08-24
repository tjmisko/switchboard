package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDisplayNameValidForRequiresBoundGeneratedRecord(t *testing.T) {
	for _, test := range []struct {
		name   string
		record *DisplayName
		thread string
		want   bool
	}{
		{name: "generated", record: &DisplayName{Value: "fix-hook-binding", Origin: DisplayNameGenerated, ConversationID: "thread-1"}, thread: "thread-1", want: true},
		{name: "fallback", record: &DisplayName{Value: "fix-hook-task", Origin: DisplayNameFallback, ConversationID: "thread-1"}, thread: "thread-1", want: true},
		{name: "wrong conversation", record: &DisplayName{Value: "fix-hook-binding", Origin: DisplayNameGenerated, ConversationID: "thread-1"}, thread: "thread-2"},
		{name: "unknown origin", record: &DisplayName{Value: "fix-hook-binding", Origin: "native", ConversationID: "thread-1"}, thread: "thread-1"},
		{name: "empty value", record: &DisplayName{Origin: DisplayNameGenerated, ConversationID: "thread-1"}, thread: "thread-1"},
		{name: "nil", thread: "thread-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.record.ValidFor(test.thread); got != test.want {
				t.Fatalf("ValidFor(%q) = %t, want %t", test.thread, got, test.want)
			}
		})
	}
}

func TestDisplayNameRoundTripsAndSnapshotsDeepCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := New(path)
	started := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	baseline := "initial native title"
	store.Apply(func(sessions map[int]*Session) {
		sessions[42] = &Session{
			PID: 42, StartedAt: started, Agent: AgentKindCodex,
			Codex: &AgentInfo{SessionID: "thread-1"},
			DisplayName: &DisplayName{
				Value: "context-aware-names", Origin: DisplayNameGenerated,
				ConversationID: "thread-1", NativeBaseline: &baseline,
			},
		}
	})

	snapshot := store.Snapshot()
	snapshot.Sessions[0].DisplayName.Value = "mutated-copy"
	*snapshot.Sessions[0].DisplayName.NativeBaseline = "mutated-baseline"
	unchanged := store.Snapshot().Sessions[0].DisplayName
	if unchanged.Value != "context-aware-names" || unchanged.NativeBaseline == nil || *unchanged.NativeBaseline != baseline {
		t.Fatalf("snapshot alias mutated live display name: %+v", unchanged)
	}

	reloaded := New(path)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	got := reloaded.Snapshot()
	if got.SchemaVersion != CurrentSchemaVersion || len(got.Sessions) != 1 {
		t.Fatalf("loaded snapshot = %+v", got)
	}
	record := got.Sessions[0].DisplayName
	if !record.ValidFor("thread-1") || record.NativeBaseline == nil || *record.NativeBaseline != baseline {
		t.Fatalf("loaded display name = %+v", record)
	}
}

func TestLoadIgnoresSchemaV2Mirror(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	body := `{"schema_version":2,"sessions":[{"pid":42,"cwd":"/old","started_at":"2026-08-24T12:00:00Z","focused":false,"agent":"codex","codex":{"session_id":"thread-old","status":"idle"}}],"updated_at":"2026-08-24T12:00:00Z"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	store := New(path)
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	if got := store.Snapshot().Sessions; len(got) != 0 {
		t.Fatalf("schema v2 sessions were hydrated: %+v", got)
	}
}

func TestDisplayNameWireContainsNoTransientNamingContext(t *testing.T) {
	baseline := "native title"
	snapshot := Snapshot{
		SchemaVersion: CurrentSchemaVersion,
		Sessions: []Session{{
			PID: 7, Agent: AgentKindCodex,
			DisplayName: &DisplayName{
				Value: "safe-display-name", Origin: DisplayNameFallback,
				ConversationID: "thread-7", NativeBaseline: &baseline,
			},
		}},
	}
	wire, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"user_prompt", "prompt", "assistant_response", "last_assistant_message", "turn_id"} {
		if strings.Contains(string(wire), forbidden) {
			t.Fatalf("persisted display record contains transient key %q: %s", forbidden, wire)
		}
	}
}
