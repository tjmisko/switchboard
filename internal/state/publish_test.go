package state_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/testsupport"
)

// TestShouldServeAUsableSnapshotBeforeTheFirstApply pins A1, the cold start.
//
// Snapshot() is a load of a pointer a writer installs. Nothing installs one until
// the first Apply, so New() has to publish an empty snapshot or the very first
// reader dereferences nil — and the first reader is real: rpc.subscribe hands a
// brand-new connection its own full snapshot on connect, and a bar can attach
// before the first reconcile tick fires.
//
// The empty snapshot must also be the SAME empty snapshot the store served
// before, which means `sessions: []` and not `sessions: null` — the wire contract
// in docs/state-schema.md says the array may be empty, never absent.
//
// Verified failing against publish-and-swap without this fix, where it panicked:
// "runtime error: invalid memory address or nil pointer dereference".
func TestShouldServeAUsableSnapshotBeforeTheFirstApply(t *testing.T) {
	store := state.New("")

	snap := store.Snapshot()
	if snap.Sessions == nil {
		t.Error("a fresh store served sessions=nil, which encodes as JSON null; " +
			"the schema promises an array, empty at worst")
	}
	if len(snap.Sessions) != 0 {
		t.Errorf("a fresh store served %d sessions, want none", len(snap.Sessions))
	}
	if snap.Capabilities != nil {
		t.Errorf("a fresh store served capabilities %+v, want none until they are detected", snap.Capabilities)
	}
	if snap.UpdatedAt.IsZero() {
		t.Error("a fresh store served a zero updated_at; consumers read it as a timestamp, not as absence")
	}
}

// TestShouldServeCapabilitiesSetOutsideApply pins A2 for SetCapabilities.
//
// SetCapabilities takes the store lock directly rather than going through Apply,
// so it is exactly the kind of writer publish-and-swap can strand: it mutates
// state every snapshot carries, and once Snapshot() stops reading live state, a
// missed republish means the change is invisible until some unrelated Apply
// happens to run.
//
// It is not a rare path. reconcileOnce calls it EVERY tick, because the terminal
// locator is self-redetecting — a terminal that came up after the daemon flips
// terminal/navigate off their boot-race "none" values with no restart. Strand it
// and the capabilities block of state.json silently freezes at those boot values.
//
// Verified failing against publish-and-swap without this fix, where the snapshot
// still carried the boot-time capabilities: got &{Observe:true Navigate:false
// WM:none Terminal:none}.
func TestShouldServeCapabilitiesSetOutsideApply(t *testing.T) {
	store := state.New("")
	store.SetCapabilities(state.Capabilities{Observe: true, WM: "none", Terminal: "none"})
	store.Apply(func(m map[int]*state.Session) {
		m[1] = &state.Session{PID: 1, StartedAt: time.Unix(1000, 0)}
	})

	// The tick's re-detection: a terminal appeared, so navigate is now possible.
	store.SetCapabilities(state.Capabilities{Observe: true, Navigate: true, WM: "hyprland", Terminal: "wezterm"})

	got := store.Snapshot().Capabilities
	if got == nil {
		t.Fatal("capabilities set outside Apply never reached a reader at all")
	}
	if !got.Navigate || got.Terminal != "wezterm" || got.WM != "hyprland" {
		t.Errorf("capabilities = %+v, want the re-detected stack; a SetCapabilities that does not "+
			"republish freezes this block for as long as no Apply changes anything", got)
	}
}

// TestShouldServeSessionsHydratedOutsideApply pins A2 for Load, the other writer
// that bypasses Apply.
//
// Load runs once at startup and is the only thing that has ever put a session in
// the map without an Apply. What reads the result is not a distant tick: the very
// next statement in the daemon's startup is dropStaleSessions, whose pre-lock
// hydratePendingVerdicts call reads store.Snapshot() to decide which persisted
// permission prompts survived the downtime. A Load that does not republish hands
// it an empty snapshot, so every hydrated prompt goes unfalsified and every
// hydrated red stays red on evidence nobody looked for.
//
// Verified failing against publish-and-swap without this fix: "a reader saw 0
// hydrated sessions, want 1".
func TestShouldServeSessionsHydratedOutsideApply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	testsupport.WriteFile(t, path, `{"sessions":[{"pid":4242,"cwd":"/home/test","tty":"/dev/pts/2",`+
		`"started_at":"2026-05-28T09:00:00Z","focused":false,`+
		`"claude":{"session_id":"abc","status":"permission","pending_writers":["main"]}}],`+
		`"updated_at":"2026-05-28T09:05:30Z"}`)

	store := state.New(path)
	if err := store.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	snap := store.Snapshot()
	if len(snap.Sessions) != 1 {
		t.Fatalf("a reader saw %d hydrated sessions, want 1 — Load did not publish what it hydrated", len(snap.Sessions))
	}
	if got := snap.Sessions[0].PID; got != 4242 {
		t.Errorf("hydrated session pid = %d, want 4242", got)
	}
	// The hydrate's own inverse projection must survive the publish: Load decodes
	// pending_writers into the in-memory map, and enrichForWire re-derives the
	// slice for the snapshot. A published snapshot taken before that round trip
	// would show the prompt as unowned.
	if got := snap.Sessions[0].Claude.PendingWriters; len(got) != 1 || got[0] != state.PendingWriterMain {
		t.Errorf("hydrated pending_writers = %v, want [%q]", got, state.PendingWriterMain)
	}
}

// TestShouldShareOneBackingArrayAcrossEveryReader is A3, asserted rather than
// implied.
//
// The plan lists this as "should not let one reader's mutation reach another".
// The store cannot promise that, and after publish-and-swap it demonstrably does
// not: every reader is handed the same []Session and the same *AgentInfo
// pointers, because handing out one shared snapshot is the whole mechanism. So
// the test pins the sharing instead — the fact that MAKES Snapshot()'s
// immutability contract load-bearing rather than decorative. A future reader who
// deletes the contract comment finds this test explaining what it was protecting.
//
// This extends a contract the codebase already relies on: Broadcast.JSON has
// carried "Treat it as immutable: every subscriber holds this same backing array"
// since the fan-out encode was shared. A widens it from subscribers to all
// readers.
//
// The second half is the boundary of the damage, and it is the reason a mutating
// reader is a bug and not a catastrophe: the published snapshot's enrichment
// blocks are enrichForWire copies whose reference-typed fields are CLONED, so a
// reader that scribbles on one corrupts its fellow readers and NOT the live
// session the daemon keeps reconciling.
//
// That second half must be asserted through a map or a slice, never through a
// scalar. A shallow copy insulates a scalar for free, so a Status-only assertion
// stays green whether the clones exist or not — which is exactly what it did while
// Pending and Workflows still aliased the daemon's own block.
//
// Verified failing against the pre-change tree, where every Snapshot() allocated
// a fresh slice: "two readers got different backing arrays".
func TestShouldShareOneBackingArrayAcrossEveryReader(t *testing.T) {
	store := state.New("")
	store.Apply(func(m map[int]*state.Session) {
		info := &state.AgentInfo{SessionID: "abc", Status: state.StatusWorking}
		info.SetPending("w1", state.PendingPrompt{Tool: "Bash", InputHash: "h1"})
		info.Workflows = []state.WorkflowStatus{{RunID: "wf_1", AgentsStarted: 3, InFlight: 2}}
		m[1] = &state.Session{PID: 1, StartedAt: time.Unix(1000, 0), Agent: state.AgentKindClaude,
			Claude: info}
	})

	first := store.Snapshot()
	second := store.Snapshot()
	if &first.Sessions[0] != &second.Sessions[0] {
		t.Fatal("two readers got different backing arrays; Snapshot is still copying per call, " +
			"so the immutability contract on it is not yet the thing readers depend on")
	}
	if first.Sessions[0].Claude != second.Sessions[0].Claude {
		t.Fatal("two readers got different *AgentInfo pointers; the enrichment blocks are still per-reader")
	}

	// Therefore: a mutating reader is visible to every other reader. Stated as an
	// assertion so it cannot be rediscovered as a surprise.
	first.Sessions[0].Claude.Status = state.StatusIdle
	if got := second.Sessions[0].Claude.Status; got != state.StatusIdle {
		t.Errorf("second reader's status = %q after the first reader wrote %q; the two are supposed "+
			"to be the same block", got, state.StatusIdle)
	}

	// But the live store is insulated. The published snapshot holds enrichForWire
	// copies whose Pending map and Workflows slice are cloned, so the corruption
	// stops at the wire projection and never reaches the session the next Apply
	// reconciles.
	//
	// Scribble through the two reference-typed fields specifically. Pending is the
	// dangerous one: `len(Pending) > 0 → RED` is the highest-priority rule in the
	// chip fold, so a write that reached the live map would latch a red chip on a
	// session that has no prompt outstanding.
	first.Sessions[0].Claude.Pending["intruder"] = state.PendingPrompt{Tool: "Bash", InputHash: "h2"}
	first.Sessions[0].Claude.Workflows[0].InFlight = 99

	store.Apply(func(m map[int]*state.Session) {
		live := m[1].Claude
		if got := live.Status; got != state.StatusWorking {
			t.Errorf("a reader's mutation reached the LIVE block: status = %q, want %q — "+
				"the snapshot is sharing the daemon's own *AgentInfo, not a wire copy", got, state.StatusWorking)
		}
		if _, intruded := live.Pending["intruder"]; intruded {
			t.Errorf("a reader's write reached the LIVE Pending map, which is the daemon's own: "+
				"enrichForWire's copy is shallow again, so `len(Pending) > 0 → RED` now latches a red "+
				"chip on a session with no prompt outstanding. Clone the map.")
		}
		if got := live.Workflows[0].InFlight; got != 2 {
			t.Errorf("a reader's write reached the LIVE Workflows slice: in_flight = %d, want 2 — "+
				"enrichForWire is aliasing the backing array again", got)
		}
	})
}

// TestShouldRepublishEvenWhenNothingObservableChanged pins the plan's "publish
// unconditionally" decision, which until now was carried by a comment and by
// nothing else: gating publishLocked on `changed` — the exact regression the
// decision exists to forbid — left the entire suite green.
//
// Apply gates its broadcast and its persist on `changed`, and that is right:
// re-encoding an unchanged snapshot for every subscriber and rewriting an
// identical state.json every tick are both pure waste. The publish is different,
// and the difference is UpdatedAt. It is the one field a reader cannot recompute
// and the one field whose entire job is to say when the state in hand was
// current. Gate the publish and a reader's updated_at freezes at the last
// OBSERVABLE change, so an idle box looks like a daemon that died — which is the
// precise misreading docs/state-schema.md warns against.
//
// An idle reconcile tick is the common case, not a corner: it re-derives the same
// state and suppresses the write, so on a quiet machine nearly every Apply lands
// here.
func TestShouldRepublishEvenWhenNothingObservableChanged(t *testing.T) {
	store := state.New("")
	store.Apply(func(m map[int]*state.Session) {
		m[1] = &state.Session{PID: 1, StartedAt: time.Unix(1000, 0), Agent: state.AgentKindClaude,
			Claude: &state.AgentInfo{SessionID: "abc", Status: state.StatusWorking}}
	})
	before := store.Snapshot()

	// Long enough that the two stamps cannot collide on a coarse clock, short
	// enough to be free. The assertion is on ordering, not on the interval.
	time.Sleep(2 * time.Millisecond)

	// A no-op Apply: the tick that finds nothing to do. `changed` is false here,
	// so this is exactly the path a `changed` gate would skip.
	store.Apply(func(m map[int]*state.Session) {})
	after := store.Snapshot()

	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Errorf("updated_at did not advance across an idle Apply: before=%s after=%s — the publish "+
			"is gated on `changed` again, so a reader's updated_at now freezes at the last observable "+
			"change and an idle daemon is indistinguishable from a dead one",
			before.UpdatedAt.Format(time.RFC3339Nano), after.UpdatedAt.Format(time.RFC3339Nano))
	}
}
