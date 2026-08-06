package state

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// pendingWriters is a spread of keys chosen so that map iteration order is
// genuinely random across runs (Go randomizes the start for maps of this size)
// and so that "main" does NOT sort first — af5b… precedes it — which is what
// separates "sorted in wire terms" from "the main thread happens to be first".
var pendingWriters = []string{"", "af5bd126402ac16c7", "aa83942381ce15c04", "b1c2d3e4f5061728"}

func redInfoWithPending(writers ...string) *AgentInfo {
	info := &AgentInfo{Status: StatusPermission, StatusSince: time.Unix(1700, 0)}
	for _, w := range writers {
		info.SetPending(w, PendingPrompt{Tool: "Bash", InputHash: "deadbeef", Since: time.Unix(1700, 0)})
	}
	return info
}

// Trap 1, the one that silently costs the most. snapshotChangeKey JSON-encodes
// every tagged field to decide whether to publish, so a projection built by
// ranging the map would differ between snapshots of identical state — and the
// daemon would republish to all ten waybar slots and rewrite state.json on every
// reconcile tick, reintroducing exactly the wake-storm the gate exists to
// suppress.
func TestPendingWritersProjectionIsOrderStable(t *testing.T) {
	t.Run("should produce a byte-identical change key when two equal Pending maps iterate in different orders", func(t *testing.T) {
		snapshotOf := func() Snapshot {
			s := New("")
			s.Apply(func(m map[int]*Session) {
				m[42] = &Session{PID: 42, StartedAt: time.Unix(1000, 0), Agent: AgentKindClaude,
					Claude: redInfoWithPending(pendingWriters...)}
			})
			return s.Snapshot()
		}
		want := snapshotChangeKey(snapshotOf())
		// Many rebuilds: each one re-inserts into a fresh map, so any range-order
		// dependence shows up as a differing key within a handful of iterations.
		for i := 0; i < 64; i++ {
			if got := snapshotChangeKey(snapshotOf()); !bytes.Equal(got, want) {
				t.Fatalf("change key moved on rebuild %d with no state change:\n got %s\nwant %s", i, got, want)
			}
		}
	})

	t.Run("should emit pending_writers sorted ascending with main substituted for the empty key", func(t *testing.T) {
		wire := enrichForWire(redInfoWithPending(pendingWriters...))
		want := []string{"aa83942381ce15c04", "af5bd126402ac16c7", "b1c2d3e4f5061728", PendingWriterMain}
		if !reflect.DeepEqual(wire.PendingWriters, want) {
			t.Errorf("pending_writers = %v, want %v (sorted on the WIRE names, so main is not pinned first)", wire.PendingWriters, want)
		}
	})

	t.Run("should omit pending_writers entirely when no writer is blocked", func(t *testing.T) {
		wire := enrichForWire(&AgentInfo{Status: StatusWorking})
		if wire.PendingWriters != nil {
			t.Errorf("pending_writers = %v, want nil so the field omits", wire.PendingWriters)
		}
		body, err := json.Marshal(wire)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if bytes.Contains(body, []byte("pending_writers")) {
			t.Errorf("pending_writers reached the wire on a block with no prompts: %s", body)
		}
	})

	t.Run("should re-derive pending_writers rather than echo a stale value on the live block", func(t *testing.T) {
		// Load leaves the decoded slice behind until the hydrate consumes it; the
		// projection must never trust it.
		info := redInfoWithPending("af5bd126402ac16c7")
		info.PendingWriters = []string{"a-writer-that-is-no-longer-blocked"}
		if got := enrichForWire(info).PendingWriters; !reflect.DeepEqual(got, []string{"af5bd126402ac16c7"}) {
			t.Errorf("pending_writers = %v, want the map's own key set", got)
		}
	})
}

// The wire codec is a pair: what enrichForWire writes, Load must read back. A
// one-directional change here is how prompt ownership silently stops surviving a
// restart, which is the whole point of the field.
func TestPendingWritersRoundTripThroughTheMirror(t *testing.T) {
	t.Run("should restore the same writer key set when a persisted mirror is loaded", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.json")
		src := New(path)
		src.Apply(func(m map[int]*Session) {
			m[42] = &Session{PID: 42, StartedAt: time.Unix(1000, 0), Agent: AgentKindClaude,
				Claude: redInfoWithPending(pendingWriters...)}
		})

		dst := New(path)
		if err := dst.Load(); err != nil {
			t.Fatalf("Load: %v", err)
		}
		var got *AgentInfo
		dst.Apply(func(m map[int]*Session) { got = m[42].Claude })

		if len(got.Pending) != len(pendingWriters) {
			t.Fatalf("hydrated %d writers, want %d", len(got.Pending), len(pendingWriters))
		}
		for _, w := range pendingWriters {
			if _, ok := got.Pending[w]; !ok {
				t.Errorf("writer %q lost across the mirror", w)
			}
		}
		if got.PendingWriters != nil {
			t.Errorf("PendingWriters = %v on the live block after Load, want nil — the map is the single source of truth", got.PendingWriters)
		}
	})

	t.Run("should drop the correlators when a prompt crosses the mirror", func(t *testing.T) {
		// Deliberate (§9.5): Tool/InputHash/Since are re-earned from the next hook,
		// and a hydrated entry must therefore fail closed on the hook path.
		path := filepath.Join(t.TempDir(), "state.json")
		src := New(path)
		src.Apply(func(m map[int]*Session) {
			m[42] = &Session{PID: 42, StartedAt: time.Unix(1000, 0), Agent: AgentKindClaude,
				Claude: redInfoWithPending("")}
		})

		dst := New(path)
		if err := dst.Load(); err != nil {
			t.Fatalf("Load: %v", err)
		}
		var got *AgentInfo
		dst.Apply(func(m map[int]*Session) { got = m[42].Claude })

		p := got.Pending[""]
		if p.Tool != "" || p.InputHash != "" || !p.Since.IsZero() {
			t.Errorf("hydrated prompt = %+v, want a zero PendingPrompt (ownership only)", p)
		}
		if got.PendingTool != "" {
			t.Errorf("PendingTool = %q after hydrate, want empty so the hook-path fast match cannot fire on it", got.PendingTool)
		}
	})

	t.Run("should hydrate nothing when the mirror predates the field", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.json")
		legacy := `{"sessions":[{"pid":42,"cwd":"/p","tty":"/dev/pts/1","started_at":"2026-05-28T09:00:00Z","agent":"claude","claude":{"status":"permission"}}],"updated_at":"2026-05-28T09:05:00Z"}`
		if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
			t.Fatalf("write mirror: %v", err)
		}
		store := New(path)
		if err := store.Load(); err != nil {
			t.Fatalf("Load: %v", err)
		}
		var got *AgentInfo
		store.Apply(func(m map[int]*Session) { got = m[42].Claude })
		if got.Pending != nil {
			t.Errorf("Pending = %v, want nil — a nil map is what tells the hydrate it is reading a pre-T12 mirror", got.Pending)
		}
	})
}

// PendingTool feeds the hold gate's fast path and the decision log's `pending=`
// field, so "any one of them" must not literally mean a different one each tick.
func TestPendingToolIsDerivedDeterministically(t *testing.T) {
	t.Run("should report the main thread's tool when the main thread is one of the blocked writers", func(t *testing.T) {
		info := &AgentInfo{}
		info.SetPending("af5bd126402ac16c7", PendingPrompt{Tool: "Edit"})
		info.SetPending("", PendingPrompt{Tool: "AskUserQuestion"})
		info.SetPending("aa83942381ce15c04", PendingPrompt{Tool: "Bash"})
		if info.PendingTool != "AskUserQuestion" {
			t.Errorf("PendingTool = %q, want the main thread's AskUserQuestion", info.PendingTool)
		}
	})

	t.Run("should report the lowest-keyed writer's tool when the main thread is not blocked", func(t *testing.T) {
		for i := 0; i < 32; i++ {
			info := &AgentInfo{}
			info.SetPending("b1c2d3e4f5061728", PendingPrompt{Tool: "Edit"})
			info.SetPending("aa83942381ce15c04", PendingPrompt{Tool: "Bash"})
			info.SetPending("af5bd126402ac16c7", PendingPrompt{Tool: "Write"})
			if info.PendingTool != "Bash" {
				t.Fatalf("PendingTool = %q on rebuild %d, want the lowest-keyed writer's Bash", info.PendingTool, i)
			}
		}
	})

	t.Run("should re-derive the reported tool when the writer holding it resolves", func(t *testing.T) {
		info := &AgentInfo{}
		info.SetPending("", PendingPrompt{Tool: "AskUserQuestion"})
		info.SetPending("af5bd126402ac16c7", PendingPrompt{Tool: "Bash"})
		info.DropPending("")
		if info.PendingTool != "Bash" {
			t.Errorf("PendingTool = %q after the main thread resolved, want the surviving writer's Bash", info.PendingTool)
		}
		info.DropPending("af5bd126402ac16c7")
		if info.PendingTool != "" || len(info.Pending) != 0 {
			t.Errorf("PendingTool = %q / Pending = %v after the last writer resolved, want both empty", info.PendingTool, info.Pending)
		}
	})

	t.Run("should count the other blocked writers in the decision log's summary", func(t *testing.T) {
		info := &AgentInfo{}
		info.SetPending("", PendingPrompt{Tool: "AskUserQuestion"})
		if got := info.PendingSummary(); got != "AskUserQuestion" {
			t.Errorf("PendingSummary = %q, want the bare tool so ParseDecision keeps reading it", got)
		}
		info.SetPending("af5bd126402ac16c7", PendingPrompt{Tool: "Bash"})
		if got := info.PendingSummary(); got != "AskUserQuestion+1" {
			t.Errorf("PendingSummary = %q, want AskUserQuestion+1 (a multi-writer red must not report as a single one)", got)
		}
	})
}
