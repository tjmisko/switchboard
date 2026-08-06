# Store lock: making the rule enforceable

Follow-on plan to #57. Two changes, A then B, plus one deferred to an issue.

## Why, in one paragraph

The rule "no I/O inside `store.Apply`" is correct, load-bearing, and enforced by
nothing. #57 hoisted every read out from under the lock; six weeks later the
workflow-visibility feature (#61) put two of them back — a full-archive decode
in the Observer's seed, and a per-session directory scan every tick — because
the rule lives in prose several call frames from where it must be obeyed. The
contributor did nothing wrong by the code's own lights: no failing test, no
lint, no type error. That is the defect to fix, and it is not a performance
defect.

Measured, so the effort goes to the right place:

| | value | where |
|---|---|---|
| `Apply` with an empty closure, 5 sessions | **5.6 µs** | `BenchmarkApplyFloor` |
| `Snapshot`, 5 sessions | **0.8 µs** | `BenchmarkSnapshot` |
| Observer seed on `main`, per newly-seen session | **1.81 s** | live archive, 62.6 MB / 37 days |
| Observer seed after #57's merge resolution | **54.8 ms** | same archive |

The lock's own machinery is five orders of magnitude below the thing that hurt.
**There is no contention problem.** A is therefore justified as a change of
failure mode, not of throughput, and the actor/single-writer rewrite is rejected
outright (see "Not doing").

## Sequencing

1. **#57 merge lands first.** A and B both touch code the merge rewrites
   (`internal/state/state.go`, `internal/fanout/observer.go`). Landing them
   first guarantees a third conflict.
2. **A before B.** A is behaviour-preserving and small; B's tick-budget test
   (B2) wants A's contract in place so "reader blocked" and "writer slow" are
   distinguishable failures rather than one number.
3. **Re-measure after A, not between A and B.** B adds no runtime cost.

---

# A — publish and swap

## Mechanism

`Apply` already builds a full `Snapshot` inside the lock; it needs one for the
change-key comparison. Store that snapshot in an `atomic.Pointer` on the way
out, and make `Snapshot()` a pointer load instead of an `RLock`.

```go
type Store struct {
	path      string
	mu        sync.Mutex               // WRITERS ONLY, now
	sessions  map[int]*Session
	published atomic.Pointer[Snapshot] // what every reader sees
	// … subscribers, caps, publishedKey, publishedGen unchanged
}

// publishLocked installs the snapshot readers will see. Caller holds s.mu.
func (s *Store) publishLocked(snap Snapshot) {
	s.published.Store(&snap)
}

func (s *Store) Snapshot() Snapshot {
	return *s.published.Load()   // never blocks, whatever a writer is doing
}
```

What this buys is **not speed**. It is that a slow write stops being able to
freeze a reader. Today a 1.81 s `Apply` blocks every waybar subscriber, every
hook RPC and every chip click for 1.81 s. After A, it delays only the next
*write*. The entire class of bug this plan exists for stops reaching the user,
whether or not B ever catches it.

## Four things that break, and their fixes

Each of these is a real behaviour change, not a detail. They are why A is a
plan and not a patch.

**A1 — the cold start.** Before the first `Apply`, `published` is nil and
`Snapshot()` panics. Fix: publish an empty snapshot in `New()`. An empty store's
snapshot is `Snapshot{Sessions: nil, Capabilities: nil}` plus a timestamp, which
is what `Snapshot()` returns today for the same state.

**A2 — `SetCapabilities` and `Load` bypass `Apply`.** Both mutate state that
appears in snapshots, and today are visible to the very next `Snapshot()`
because it reads live state. Under A they must republish or the change is
invisible until the next tick. `SetCapabilities` in particular runs every tick
(the terminal locator self-redetects), so a missed republish would silently
freeze the `capabilities` block of `state.json`. Fix: both call
`publishLocked(s.snapshotLocked())` before unlocking.

**A3 — readers now share one backing array.** This is the load-bearing risk.
Today every `Snapshot()` call allocates a fresh `[]Session` and deep-copies each
enrichment block via `enrichForWire`, so a caller that mutates its result
affects nobody. After A, all readers hold the same slice and the same
`*AgentInfo` pointers. A mutating reader corrupts every other reader.

The precedent is already in the codebase and already documented — `Broadcast.JSON`
carries "Treat it as immutable: every subscriber holds this same backing array."
A extends that existing contract from subscribers to all readers, which is the
argument for it being a small change rather than a new hazard. But the contract
must be stated on `Snapshot()` and the callers audited once:

| call site | verdict |
|---|---|
| `cmd/switchboard/main.go` — `hydratePendingVerdicts(store.Snapshot(), …)` | read-only; its doc already says "mutates nothing" |
| `cmd/switchboard/main.go` — `snap := store.Snapshot()` in `reconcileOnce` | read-only; feeds the samplers |
| `cmd/switchboard/memory.go` — `livePIDs(store.Snapshot())` | read-only |
| `internal/rpc/rpc.go` ×3 — `list`, `subscribe`, `focus` | read-only: encode, encode, pick-and-read |

All six read-only on inspection. The audit is a plan step, not a formality:
it must be re-run against the post-merge tree, and the result recorded in the
commit message.

**A4 — `UpdatedAt` moves from read time to publish time.** Today
`snapshotLocked` stamps `time.Now()` on every call, so two readers a second
apart see different `updated_at`. After A they see the publish instant, which is
more truthful ("when this state was current") but is a wire-visible change for
RPC `list` and `subscribe`. `state.json` is unaffected — it is already written
from the snapshot built inside `Apply`. Check `internal/state/golden_test.go`
and `docs/state-schema.md`; if the schema documents read-time semantics, the
doc changes with the code.

## Publish unconditionally

`Apply` gates `broadcast` and `persist` on `changed`. Publishing is **not**
gated: the snapshot is already built, the store is one atomic pointer write, and
gating it would mean a reader's `UpdatedAt` freezes on an idle box. Cost is one
`atomic.Store` per `Apply` on a path that already does a JSON encode.

## Tests

Written before the change, each verified failing against the current code.

- `should not block a reader while a writer holds the lock` — reuse
  `measureWorstReaderWait` from `cmd/switchboard/reconcile_lock_test.go` with an
  `Apply` whose closure sleeps 200 ms. Today the reader waits ~200 ms; after A
  it waits ~0. This is the whole point of the change and it is directly
  measurable.
- `should serve capabilities set outside Apply` — pins A2.
- `should serve a usable snapshot before the first Apply` — pins A1.
- `should not let one reader's mutation reach another` — documents A3 by
  asserting the shared-backing-array contract explicitly rather than leaving it
  implied.
- Existing golden tests pin A4; update the golden only if the diff is
  `updated_at` alone.

Run the full suite under `-race`. A publishes a pointer that readers dereference
concurrently with writers building the next one; the race detector is the
primary check that the handoff is clean.

## Risks

- **Low, behaviour-preserving, except A3 and A4**, both of which are contract
  changes rather than bugs — and A3 is a contract this codebase already relies
  on for subscribers.
- Staleness is bounded by the inter-write interval, not the tick: hooks write
  too. Every sampler in the tick already tolerates a one-`Apply`-stale view —
  that is what `usableFor` and `freshFor` exist for — so A hands them exactly
  the guarantee they were already written against.

---

# B — make the rule enforceable

A removes the *consequence*. B removes the *recurrence*. Both are wanted; B is
the one that addresses the root.

## What B cannot be

Worth stating first, because the obvious framing does not survive contact.

"The apply phase has no filesystem" is **not achievable** as a type-level
guarantee, because the inline fallback in `Observer.reconcile` is load-bearing
for correctness: when a sample is rejected (a hook reconciled the session
mid-tick), the apply phase *must* re-read, under the lock, exactly as it did
before the split. Removing that would trade a rare short lock hold for a dropped
tick. So the invariant B enforces is narrower and true:

> **The fast path does no I/O.** A sampler that has a usable sample reads
> nothing while the lock is held.

Go offers no effect system to express that, and a static "does this closure
transitively call `os`?" check would trip on the fallback it must permit. So B
is tests, not types.

## B1 — a sampler contract suite

The project already has the right machinery: `internal/conformance` holds
"backend-agnostic contract suites reused by every backend". B1 adds a sampler
contract to it.

The proof technique is already in use ad hoc — `#57`'s own tests take a sample,
delete what it read, and require the answer to come out anyway:

```go
s := obs.Sample(e.c.SessionID, e.c.Transcript)

// Delete the subagents dir the sample already read. If ReconcileFrom went back
// to disk, it would now find nothing and emit no spawn.
if err := os.RemoveAll(filepath.Join(e.base, e.sid)); err != nil {
	t.Fatal(err)
}

if ev := obs.ReconcileFrom(s, e.sess, e.c, time.Now()); !hasEvent(ev, history.EventSubagentSpawn, "n1") {
	t.Fatalf("n1's spawn should come from the sample taken before the dir was removed; got %+v", ev)
}
```

That works, needs no seam, and is proven in this codebase. B1 promotes it from
a per-test idiom to a contract every sampler is registered in:

```go
// conformance.SamplerContract asserts the fast path reads nothing.
type Sampler struct {
	Name    string
	Fixture func(t *testing.T) (dir string, apply func() any)  // lays out disk
	Sample  func() any                                          // pre-lock phase
	Apply   func(sample any) any                                // under-lock phase
}

// For each sampler: run Apply with the fixture intact, then again with the
// fixture renamed away, and require identical results.
func SamplerContract(t *testing.T, s Sampler)
```

Registered samplers, one per pre-lock reader in the tick: `sampleFanout`,
`sampleSignals`, `sampleLabels`, `sampleMemory`, `sampleProc`, `sampleUsage`.

The honest limit: **a new sampler that is never registered is not caught.**
B1 makes the contract cheap and conventional, not automatic. B2 is what covers
the unregistered case.

## B2 — a tick budget test

The one that would have caught #61 directly, and it needs no new machinery
either — `cmd/switchboard/reconcile_lock_test.go` already has both halves:

- `reconcileFixture(t, sessionCount)` builds a store, resolver and tick;
- `measureWorstReaderWait(store, work)` hammers `Snapshot()` while `work` runs
  and reports the longest a reader was blocked.

and it already uses them exactly this way, with a comment that reads as a
prophecy of #61:

```go
func TestShouldNotHoldTheStoreLockAcrossTerminalEnumeration(t *testing.T) {
	store, _, _, tick := reconcileFixture(t, 8)
	got := measureWorstReaderWait(store, tick)
	limit := enumDelay / 3
	if got > limit {
		t.Errorf("a store reader blocked for %v during a tick whose enumeration takes %v; "+
			"want under %v — the enumeration is being held under the lock again", got, enumDelay, limit)
	}
}
```

The gap is that the fixture makes only the *terminal enumeration* slow. B2
extends it so the *history archive* is slow too:

- give `reconcileFixture` an option that writes N day-files into the fixture's
  history dir, sized so `PriorFanoutState` takes a measurable tens of ms;
- run a tick that discovers a **new** session, which is what triggers the seed;
- assert worst reader wait stays under budget.

Against `main` today this fails by roughly the seed duration. Against the merged
tree it passes, because `Prime` runs before the lock. Against a future
`reconcileWorkflowsLocked` that reads under the lock, it fails again — which is
the entire point.

Two design notes so the test is not flaky:

- **Budget as a fraction of injected delay, not a wall-clock constant.** The
  existing tests use `enumDelay / 3`; B2 uses `seedDelay / 3`. A constant would
  fail on a loaded CI box and pass on a fast one.
- **After A, the reader-wait number collapses to ~0 by construction**, so B2
  would pass vacuously. Keep it meaningful by measuring the *writer* side too:
  time the `Apply` itself (via `lockWarnOut`, which #57 already made a variable
  "purely so the test can read it") and budget that. Post-A, B2's assertion
  becomes "the tick's Apply is short", which is still exactly the invariant.

## B3 — deferred design note, not scheduled

The type-level version does exist: `ReconcileFrom` returns `ErrStaleSample`
instead of re-reading, and the caller re-samples **outside** the lock and
retries, with a bounded attempt count falling back to inline reads to guarantee
progress. That genuinely removes I/O from the fast path by construction.

It is not proposed, because it restructures the tick's single `Apply` into a
sample → try-apply → re-sample → apply loop, and the thing it buys over B1+B2
is the elimination of a rare, short, correct inline read. Recorded here so the
next person does not have to re-derive it.

## Tests for B

B is tests. Its own verification is that each new test **fails against the
pre-merge tree and against `main`**, checked explicitly, in the style #57
already uses ("the test written before the fix and verified failing against the
previous code first").

---

# Not doing: the actor rewrite

Give the store to one goroutine; every mutation becomes a channel message. It is
the design the narrative most suggests and it is rejected on the measurement:
the quantity it optimizes is 5.6 µs at this box's session count.

It would also import failure modes the current design does not have — mailbox
depth and backpressure policy, the ordering relationship between a hook message
and a tick message, what happens to a queued mutation when its session dies, and
a debugging story where a stack trace no longer names who asked for the write.
Real costs, paid for a quantity that is already negligible. If a future
measurement shows writer-vs-writer contention actually mattering, this decision
gets revisited with that number in hand.

---

# Deferred to issues

**#63 — watch, do not only poll.** The reconcile tick is 5 s. Subagent hooks make
hand-launched fanouts near-instant, but workflow runs fire no hooks at all, so
their entire visibility latency is the tick interval — a run can start, fan out
and drain inside one tick. An `inotify` watch on each session's transcript,
`subagents/` and `workflows/` dirs takes that to sub-100 ms. It must stay a
*trigger*, exactly as the `SubagentStart`/`SubagentStop` hooks are, with the
tick remaining the source of truth; otherwise it breaks the invariant that
discovery is authoritative and everything else is enrichment.

**#64 — the workflow run-dir scan grows without bound**, same class as the
seed. Run dirs are never deleted, so `WorkflowRunsForTranscript` does
two `ReadDir`s plus one journal open and 2+N stats *per session per tick*, over
a directory that accumulates every run the session has ever made. Cost
proportional to accumulated history rather than to what is live — the same shape
as the seed. A run already bracketed by `workflow_stop` and quiet past the grace
should cost one stat, not an open plus two.

---

# Harness

The re-measure is blocked on setup, not on work. State and fixes:

- **The running daemon emits nothing.** `SWITCHBOARD_DEBUG_LOCK` is unset, so
  there is no baseline at all. Fixed by a systemd drop-in (below), picked up on
  the next restart — which `scripts/lock-hold-arm` does anyway.
- **`main` cannot attribute a hold.** The `caller=` field arrives with `28ced0d`,
  which is on the #57 branch. Without it every warning on a baseline arm folds
  into one `(unattributed)` row that cannot separate `reconcileOnce` from
  `handleHook` — which is the question. **Verified: `28ced0d` cherry-picks onto
  `main` cleanly and `internal/state` passes.** The baseline arm is therefore
  `main` + that one commit, built from a throwaway branch.

## Arms

| arm | tree | answers |
|---|---|---|
| `baseline` | `main` + cherry-picked `28ced0d` | what ships today, attributed |
| `merged` | #57 merge resolution | did this help |
| `merged+A` | after A lands | did publish-and-swap move the reader-side number |

Run `baseline` and `merged` first. `merged+A` only after A lands — and note in
advance that A is expected to move the *reader wait*, not the hold duration, so
the lock-hold report may barely change while the thing users feel does. That is
a reason to add a reader-wait measurement to the harness, not a reason to
discount A.

## Baseline result — 2026-08-06, 30 min, `a3ee08c`

Arm `baseline` (`main` + cherry-picked `28ced0d`), binary `d5899d99`, window
10:48:05–11:18:05 on an otherwise-idle box carrying 3–6 sessions.

| caller | count | p50 | p90 | p99 | max |
|---|---|---|---|---|---|
| `main.reconcileOnce:700` | 261 | 10.0 ms | 23.0 ms | 2117 ms | **5270 ms** |
| `rpc.(*Server).handleHook:397` | 1 | 0.0 ms | 0.0 ms | 0.0 ms | 6.1 ms |

Three findings, none of which the pre-measurement framing predicted:

**The tail is recurring, not a cold-start artifact.** Nine holds exceeded one
second, and they are spread across the whole window — three of them at
11:05–11:07, nineteen minutes in, on an idle box. The seed is per *newly-seen*
session, and sessions churn continuously on a machine whose whole purpose is
watching sessions come and go. So the 1.81 s is not paid once at startup; it is
paid again every time a session appears. The startup case is merely the worst
(5.27 s, five sessions seeded in a single tick).

**Production agrees with the bench.** Observed per-seed holds run 1.57–2.36 s
against a bench figure of 1.81 s. The archive decode is the cost, as diagnosed.

**`handleHook` is not currently a problem.** One warning, 6.1 ms, in thirty
minutes. Issue #56 (the hook handler reading the transcript inside the lock) is
real but is not what users are feeling; it should not be prioritized off this
number. Attribution is what makes that claim sayable at all, which is the
justification for cherry-picking `28ced0d` onto the baseline arm.

The distribution is bimodal — a ~10 ms body that is the terminal enumeration,
and a multi-second tail that is the seed. #57 hoists both. A prediction worth
writing down before the `merged` arm runs: if the merge is correct, the
nine-hold tail goes to **zero** and p50 drops toward the sub-millisecond floor.
A tail that survives the merge means a read was missed.

## Rules, from the script headers

Both cost this work a wrong number already, so they are rules and not advice:

- **Do not run builds, `go test`, or a multi-agent review while measuring.**
  They hit the same disk and CPU. A window overlapping them is not comparable to
  one that does not — 35 minutes of one showed 449 warnings at p99 866 ms, which
  is real and is not a baseline.
- **Leave the box alone ≥ 30 minutes per arm.** A six-minute window read as a
  clean zero once; thirty minutes showed four post-startup trips.
- The marker file guards the rest: `lock-hold-report` refuses to print if the
  daemon restarted or the binary changed mid-window, because another session ran
  `go install` fourteen minutes into a thirty-minute window and the result looked
  like a clean 206-warning number for a binary nobody meant to measure.
