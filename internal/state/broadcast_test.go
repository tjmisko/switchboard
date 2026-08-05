package state_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/state"
)

// recvBroadcast takes the next broadcast off a subscription, failing the test
// rather than hanging when none arrives.
func recvBroadcast(t *testing.T, ch <-chan state.Broadcast) state.Broadcast {
	t.Helper()
	select {
	case b, ok := <-ch:
		if !ok {
			t.Fatal("subscription closed before a broadcast arrived")
		}
		return b
	case <-time.After(time.Second):
		t.Fatal("no broadcast arrived")
	}
	return state.Broadcast{}
}

// requireQuiet asserts nothing was published. Store.broadcast sends synchronously
// inside Apply (a non-blocking send onto a buffered channel), so by the time Apply
// returns any broadcast is already queued — no sleep, no flake.
func requireQuiet(t *testing.T, ch <-chan state.Broadcast, why string) {
	t.Helper()
	if len(ch) == 0 {
		return
	}
	b := <-ch
	t.Fatalf("%s, but a broadcast was published: %s", why, b.JSON)
}

// The whole point of Broadcast: ten waybar slot processes hold ten subscriptions,
// and each state mutation used to serialize the identical snapshot once per
// subscriber. Every subscriber must now receive the SAME buffer — pointer
// identity is the only assertion that can tell "encoded once and shared" apart
// from "encoded N times into equal bytes".
func TestBroadcast_sharesOneEncodingAcrossSubscribers(t *testing.T) {
	store := state.New("")
	const subscribers = 10
	chans := make([]<-chan state.Broadcast, 0, subscribers)
	for i := 0; i < subscribers; i++ {
		ch, cancel := store.Subscribe()
		defer cancel()
		chans = append(chans, ch)
	}

	store.Apply(func(m map[int]*state.Session) {
		m[42] = &state.Session{PID: 42, CWD: "/w", TTY: "/dev/pts/1", StartedAt: time.Unix(1000, 0)}
	})

	var shared []byte
	for i, ch := range chans {
		b := recvBroadcast(t, ch)
		if len(b.JSON) == 0 {
			t.Fatalf("subscriber %d received no encoding", i)
		}
		if i == 0 {
			shared = b.JSON
			continue
		}
		if &b.JSON[0] != &shared[0] {
			t.Errorf("subscriber %d received its own buffer; the snapshot must be marshaled once per broadcast and shared", i)
		}
	}
}

// The shared buffer is the wire body rpc splices into the response envelope
// verbatim, so it has to be exactly the encoding of the snapshot it arrived with
// — not of some earlier or later one.
func TestBroadcast_encodingMatchesTheSnapshotItArrivedWith(t *testing.T) {
	store := state.New("")
	ch, cancel := store.Subscribe()
	defer cancel()

	store.SetCapabilities(state.Capabilities{Observe: true, Navigate: true, WM: "hyprland", Terminal: "wezterm"})
	store.Apply(func(m map[int]*state.Session) {
		m[7] = &state.Session{
			PID: 7, CWD: "/home/u/p", TTY: "/dev/pts/3", StartedAt: time.Unix(2000, 0), Focused: true,
			Agent:  state.AgentKindClaude,
			Claude: &state.AgentInfo{Status: state.StatusWorking, StatusSince: time.Unix(2100, 0), SessionID: "abc"},
		}
	})

	b := recvBroadcast(t, ch)
	want, err := json.Marshal(b.Snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if !bytes.Equal(b.JSON, want) {
		t.Errorf("shared encoding does not match its snapshot.\n--- got ---\n%s\n--- want ---\n%s", b.JSON, want)
	}
}

// The reconciler calls Apply every 5 s whether or not the world moved, so an idle
// machine was waking ten waybar processes and rewriting state.json forever to
// republish byte-identical state. Re-applying the same world must publish nothing.
func TestApply_publishesNothingWhenNothingChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := state.New(path)
	ch, cancel := store.Subscribe()
	defer cancel()

	// A fresh *Session carrying identical values each time — the reconciler
	// rewrites the same fields every tick, so equality must be by value, not by
	// pointer.
	seed := func(m map[int]*state.Session) {
		m[7] = &state.Session{
			PID: 7, CWD: "/home/u/p", TTY: "/dev/pts/3", StartedAt: time.Unix(2000, 0),
			Agent:  state.AgentKindClaude,
			Claude: &state.AgentInfo{Status: state.StatusIdle, StatusSince: time.Unix(2100, 0)},
		}
	}
	caps := state.Capabilities{Observe: true, Navigate: true, WM: "hyprland", Terminal: "wezterm"}
	store.SetCapabilities(caps)
	store.Apply(seed)
	recvBroadcast(t, ch) // the first Apply is a genuine change

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted state: %v", err)
	}

	// Exactly what reconcileOnce does every tick: re-publish the detected stack,
	// rewrite every session's fields, and Apply.
	store.SetCapabilities(caps)
	store.Apply(seed)                              // identical world, rewritten
	store.Apply(func(m map[int]*state.Session) {}) // a tick that touched nothing
	requireQuiet(t, ch, "the world did not change")

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted state: %v", err)
	}
	// Byte equality also proves updated_at did not advance, which is the only
	// field that would have moved.
	if !bytes.Equal(before, after) {
		t.Errorf("an unchanged Apply rewrote state.json.\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

// A persist that fails must not leave the change key adopted. It is adopted
// before the write is attempted, so without the retraction a single transient
// failure would suppress every later identical Apply and freeze state.json at its
// last good content — for an hour, if nothing about the session set happens to
// move. Before the publish gate existed every Apply rewrote the file, so such a
// failure healed on the next tick; that self-healing has to survive.
func TestApply_republishesWhenTheLastPersistFailed(t *testing.T) {
	dir := t.TempDir()
	// A regular file standing where persist needs a directory, so MkdirAll fails
	// with ENOTDIR on every attempt. Preferred over chmod-ing a directory
	// read-only, which does not stop a root-owned CI run from writing anyway.
	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocked, nil, 0o644); err != nil {
		t.Fatalf("seed the blocker file: %v", err)
	}
	store := state.New(filepath.Join(blocked, "state.json"))
	ch, cancel := store.Subscribe()
	defer cancel()

	seed := func(m map[int]*state.Session) {
		m[7] = &state.Session{PID: 7, CWD: "/home/u/p", TTY: "/dev/pts/3", StartedAt: time.Unix(2000, 0)}
	}
	store.Apply(seed)
	recvBroadcast(t, ch) // broadcast happened; the persist behind it did not

	// The very next tick, with the world unchanged. It must publish anyway, because
	// the on-disk mirror is still missing the state the wire already carries.
	store.Apply(seed)
	if len(ch) == 0 {
		t.Error("a failed persist left the change key adopted; state.json would stay stale until something unrelated changed")
	}

	// Once the path becomes writable again the retry lands, which is the whole
	// point of not latching: recovery needs no further change to the session set.
	if err := os.Remove(blocked); err != nil {
		t.Fatalf("unblock the state dir: %v", err)
	}
	store.Apply(seed)
	if _, err := os.ReadFile(filepath.Join(blocked, "state.json")); err != nil {
		t.Errorf("state.json was not rewritten once the path became writable: %v", err)
	}
}

// The suppression must not swallow real edges. Each of these is a field a bar
// renders, and every one has to reach the subscriber and the on-disk mirror.
func TestApply_publishesWhenAnObservableFieldChanges(t *testing.T) {
	cases := map[string]func(*state.Session){
		"a focus change":     func(s *state.Session) { s.Focused = true },
		"a suspend":          func(s *state.Session) { s.Suspended = true },
		"a status edge":      func(s *state.Session) { s.Claude.Status = state.StatusWorking },
		"a memory reading":   func(s *state.Session) { s.MemTreeBytes = 1 << 20 },
		"a subagent landing": func(s *state.Session) { s.Claude.InFlightSubagents = 2 },
		"a workspace move":   func(s *state.Session) { s.Hyprland = &state.HyprlandInfo{WorkspaceID: 3} },
		"a resolved cwd":     func(s *state.Session) { s.CWD = "/home/u/other" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			store := state.New(path)
			ch, cancel := store.Subscribe()
			defer cancel()

			store.Apply(func(m map[int]*state.Session) {
				m[7] = &state.Session{
					PID: 7, CWD: "/home/u/p", TTY: "/dev/pts/3", StartedAt: time.Unix(2000, 0),
					Agent:  state.AgentKindClaude,
					Claude: &state.AgentInfo{Status: state.StatusIdle, StatusSince: time.Unix(2100, 0)},
				}
			})
			recvBroadcast(t, ch)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read persisted state: %v", err)
			}

			store.Apply(func(m map[int]*state.Session) { mutate(m[7]) })

			if len(ch) == 0 {
				t.Fatalf("%s was not broadcast", name)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read persisted state: %v", err)
			}
			if bytes.Equal(before, after) {
				t.Errorf("%s was not persisted", name)
			}
		})
	}
}

// status_since is stamped from the wall clock, which makes it look like the
// updated_at trap — but per docs/state-schema.md it only moves on a status edge,
// so it is a real change a tooltip's duration counter depends on. Excluding it
// would freeze "idle 3m" at whatever it read when the chip last changed color.
func TestApply_publishesWhenStatusSinceMoves(t *testing.T) {
	store := state.New("")
	ch, cancel := store.Subscribe()
	defer cancel()

	store.Apply(func(m map[int]*state.Session) {
		m[7] = &state.Session{PID: 7, Agent: state.AgentKindClaude,
			Claude: &state.AgentInfo{Status: state.StatusIdle, StatusSince: time.Unix(2100, 0)}}
	})
	recvBroadcast(t, ch)

	store.Apply(func(m map[int]*state.Session) {
		m[7].Claude.Status = state.StatusWorking
		m[7].Claude.StatusSince = time.Unix(2200, 0)
	})

	b := recvBroadcast(t, ch)
	got := b.Snapshot.Sessions[0].Claude.StatusSinceWire
	if got == nil || !got.Equal(time.Unix(2200, 0)) {
		t.Errorf("status_since = %v, want the new edge time", got)
	}
}

// The mirror image of the trap: fields the daemon re-stamps every tick but never
// puts on the wire must NOT force a publish. TitleAt (and the pane Title it
// dates) are re-sampled by the resolver on every reconcile tick, and PendingTool
// is transient red-onset state — none of them is visible to a consumer, so
// republishing for them is exactly the wasted work this gate exists to stop.
func TestApply_publishesNothingWhenOnlyInMemoryFieldsChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := state.New(path)
	ch, cancel := store.Subscribe()
	defer cancel()

	store.Apply(func(m map[int]*state.Session) {
		m[7] = &state.Session{PID: 7, Agent: state.AgentKindClaude,
			Wezterm: &state.WeztermInfo{PaneID: 1, Title: "· idle", TitleAt: time.Unix(3000, 0)},
			Claude:  &state.AgentInfo{Status: state.StatusPermission, StatusSince: time.Unix(2100, 0), PendingTool: "Bash"}}
	})
	recvBroadcast(t, ch)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted state: %v", err)
	}

	store.Apply(func(m map[int]*state.Session) {
		m[7].Wezterm.Title = "✳ working" // the animated spinner glyph, repainted constantly
		m[7].Wezterm.TitleAt = time.Unix(3005, 0)
		m[7].Claude.PendingTool = "Edit"
	})

	requireQuiet(t, ch, "only in-memory (json:\"-\") fields changed")
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted state: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("an in-memory-only change rewrote state.json.\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

// Capabilities are re-published every reconcile tick (the terminal locator
// self-redetects), so they sit on the suppressed path — but a backend coming up
// flips navigate and must reach the bar, which decides whether to offer "jump to".
func TestApply_publishesWhenCapabilitiesChange(t *testing.T) {
	store := state.New("")
	ch, cancel := store.Subscribe()
	defer cancel()

	store.SetCapabilities(state.Capabilities{Observe: true, WM: "none", Terminal: "none"})
	store.Apply(func(m map[int]*state.Session) { m[7] = &state.Session{PID: 7} })
	recvBroadcast(t, ch)

	store.SetCapabilities(state.Capabilities{Observe: true, Navigate: true, WM: "hyprland", Terminal: "wezterm"})
	store.Apply(func(m map[int]*state.Session) {}) // the next tick, no session change

	b := recvBroadcast(t, ch)
	if b.Snapshot.Capabilities == nil || !b.Snapshot.Capabilities.Navigate {
		t.Errorf("capabilities = %+v, want the newly detected Navigate tier", b.Snapshot.Capabilities)
	}
}
