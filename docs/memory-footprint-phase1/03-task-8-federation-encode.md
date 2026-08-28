# #8 — Federation View: one encode per publish, change-gated

**PR 2, with #5/#6/#7. Prereq: #6 must land first** — #8 must reuse its key, and
gating against an un-normalized key suppresses nothing.

---

## Why this is not a federation-only optimization

`memory-footprint-plan.md` §1.2 says "With `-remote` configured,
`rpc.subscribeAll` goes through `federation.View`". **That is wrong.** The View
is constructed unconditionally (`cmd/switchboard/federation.go:80`,
`federation.NewView(store, hostname, remoteManager)` with no `remoteFlags` guard)
and installed unconditionally (`federation.go:108`
`server.SetFederation(f.view, …)`, called from `main.go:261`).

**Every waybar slot and the bottom bar go through `View.publish()` on every box,
`-remote` or not.** And `View.publish()` has no change gate at all.

Consequences:

- **#6 gates `Store.Apply`. #8 gates `View.publish()`. The wire rate — which is
  what #10's < 0.5/s DoD measures — is the View's output.**
- The View publishes when the local store broadcasts (so #6 suppresses it
  indirectly), **but also** on remote updates, focus transitions, and `Refresh`
  from the navigator (`federation.go:129`, `navigator.go:58,164,182`) — all
  ungated.
- **#6 alone cannot reach #10's DoD.** The ablation hinted at this: restricting
  to goosebook-only sessions gave 4.18/s for #6.

Also: `internal/state/golden_test.go:186-188` already *describes* the design this
task builds — "every subscriber sends those same bytes, spliced into the response
envelope verbatim by rpc". That splice does not exist. It is evidence the
omission was an oversight, not a choice.

---

## Corrections to earlier framing (verified)

1. **`View.Snapshot()`'s `UpdatedAt: time.Now()` at `view.go:125` does NOT need
   dropping.** `SnapshotChangeKey` marshals an anonymous struct with only
   `SchemaVersion`, `Sessions`, `Capabilities` — `UpdatedAt` is excluded *by
   construction*. Reusing that key gives the gate a working comparison with
   `view.go:125` untouched, and the published frame keeps a real `updated_at`.
2. **`Refresh` is not called from `routes.go`.** `routes.go` takes a
   `changed func()` callback; `cmd/switchboard/federation.go:129` passes
   `f.view.Refresh` into `RunLiveRoutesReady`. Other callers are
   `navigator.go:58, 164, 182`.

Confirmed as stated: `Broadcast.JSON` has exactly one write (`state.go:849`) and
**zero non-test reads**; `View.Subscribe()` has exactly one non-test consumer
(`rpc.go:467`); `StartViews` (`main.go:265`) runs strictly **before** `ServeReady`
(`main.go:272`), so no client can connect before the first publish.

---

## The crux: what replaces the staleness protection

### The hazard, stated precisely

A subscriber does (1) `Subscribe()`, (2) an independent read of current state for
its initial frame, (3) loop on the channel. Between (1) and (2) a publish
`P_old` can queue. Between that queue and (2) the world moves; (2) reads `S_new`.
The loop then receives `P_old`. Consuming its payload emits `S_new → S_old` — a
visible rollback of liveness and pane routes. Today's fix: discard the payload,
re-read authoritative state, which is monotone because the same goroutine reads
it serially.

### Recommended — payload-free channel plus a cached "latest published frame"

Make the channel `chan struct{}` and add a cache every subscriber reads for
**both** its initial frame and every wakeup. This is literally what the DoD says
("re-reading the latest *shared frame*"). Staleness protection is preserved **by
construction**, not by a check:

- The cache is written only inside `publish()` under `publishMu`, which already
  serializes snapshot+fanout in one order, so it is monotone in publish order.
- There is no payload to replay, so the B→A shape is **unrepresentable**.
- Every subscriber that receives a wakeup reads a cache that is ≥ the frame that
  provoked the wakeup.

### Rejected — a generation counter on the frame

Mirrors `Store.broadcastGen`, but does not close the hazard on its own: the
subscriber's *initial* frame comes from an independent `Snapshot()` read carrying
**no generation** — precisely the unnumbered thing the queued frame must be
compared against. To give it a generation you must serve it from the same
numbered sequence, i.e. from a cache — at which point the counter is dead weight,
because reading the cache always yields the newest.

### ⚠ Rejected — cache only the re-reads, keep a fresh initial read

**This is the trap, and it looks like the minimal diff.** Initial read yields
`S_new` (fresh, uncached); the first wakeup then yields `F_old` (the last
*published* frame, older than `S_new`). Same rollback, reintroduced. Call it out
in review.

### Why the wakeup cannot be lost

With `chan struct{}` the fanout becomes a plain non-blocking send:

```go
select { case ch <- struct{}{}: default: }
```

Invariant: *the frame swap happens-before the send attempt; a failed send implies
≥1 unconsumed wakeup already in the buffer; the subscriber will receive that
wakeup and its subsequent cache read is ordered after the swap.* Channel
operations are atomic, so "buffer full" at instant T means an element existed at
T that the subscriber had not yet received; that receive and the following
`CurrentFrame()` read both happen after the swap.

This is **strictly stronger than today's coalescing**, which has a real hole:
`view.go:334-344` drains one slot then re-sends, and if another publisher refills
the slot in the gap the *newest* frame is dropped with nothing to repair it.

### What could still go wrong

| risk | assessment | mitigation |
|---|---|---|
| A new connection's initial frame is "last published", not "strictly current" | Real but bounded. The gate means the cache differs from current state only in fields the key ignores — **except** `routeReady`/`routeWorkspace`, which read the pane registry and workspace index and can move with **no publish** (`ObserveWindows` runs every reconcile tick silently). A reconnecting slot can get chip ordering up to one publish stale. | The *stream* already lags these by one publish today, so only the connect frame changes. Bounded by the reconcile tick (≤5 s) and self-heals. **Accept; document in `CurrentFrame`'s comment.** Do NOT force a publish on connect — that reintroduces churn and races. |
| The cache is empty before `run()`'s first publish | Unreachable in the shipped daemon (`StartViews` precedes `ServeReady`); reachable in tests and future rewiring | Double-checked lazy build in `CurrentFrame()` that **also adopts the key** |
| The lazy build adopting the key strands a waiting subscriber | Only if some subscriber already holds an *earlier* frame — impossible, since the lazy build runs only when the cache is `nil` | Guard on `v.frame == nil` exactly, under `publishMu`, and say so in the comment. **If it did not adopt**, `run()`'s first publish could fan out a snapshot taken *earlier* than the one already handed out — the same rollback, at startup |
| Zero subscribers + skipping the encode leaves the cache stale | Real | The View skips only the fanout, never the cache/encode. `Store` **invalidates** instead |
| Duplicate frames on the wire | A subscriber can be woken for a frame it already sent | **Do not dedupe** — `TestSubscribeAllTreatsQueuedValuesAsNotifications` decodes exactly 2 frames and would hang. Duplicates are now nearly free (shared bytes, one socket write). Possible follow-up, not #8 |

---

## Design

### Reuse `state.Broadcast` — do not invent a second frame type

`state.Broadcast` is already `{Snapshot; JSON []byte}` with the right doc
contract ("Treat it as immutable: every subscriber holds this same backing
array"), and `federation` already imports `state`. Add:

```go
// NewBroadcast builds one fan-out unit: the snapshot plus the single encoding
// every subscriber for that publish shares. JSON is nil when the encode failed;
// the caller must then encode Snapshot itself rather than send a truncated frame.
func NewBroadcast(snap Snapshot) Broadcast
```

It wraps `marshalSnapshot` (leave that unexported — `golden_test.go:203` anchors
on it) and owns the stderr write on failure that `state.go:857` does today.
`Store.broadcast` is refactored to call it, then `s.countFrame(len(b.JSON))`.

### `View.publish()` — the gate

```go
func (v *View) publish() {
    v.publishMu.Lock()
    defer v.publishMu.Unlock()
    snapshot := v.Snapshot()                       // built ONCE per publish
    key := state.SnapshotChangeKey(snapshot)
    if key != nil && bytes.Equal(key, v.publishedKey) {
        return                                     // nothing observable moved
    }
    v.publishedKey = key                           // nil key fails OPEN, as in state
    frame := state.NewBroadcast(snapshot)          // encoded ONCE per publish
    v.frameMu.Lock()
    v.frame = &frame                               // swap BEFORE the fanout
    v.frameMu.Unlock()
    v.mu.RLock()
    defer v.mu.RUnlock()
    for ch := range v.subscribers {
        select { case ch <- struct{}{}: default: }
    }
}
```

New fields: `publishedKey []byte` (touched only under `publishMu`, so no extra
mutex), `frame *state.Broadcast`, `frameMu sync.RWMutex`. `subscribers` becomes
`map[chan struct{}]struct{}`.

`&frame` is stored as a **pointer** so "no frame yet" (`nil`) is distinguishable
from "frame whose encode failed" (`JSON == nil`); otherwise a marshal failure
would make `CurrentFrame()` rebuild forever.

### `View.CurrentFrame()`

```go
// CurrentFrame returns the latest published aggregate frame: the snapshot and the
// one encoding every subscriber shares for it. Subscribers use it for BOTH their
// initial frame and every wakeup re-read, and that is what preserves the ordering
// guarantee: a wakeup queued before a subscriber's first read can no longer replay
// older state, because there is no payload to replay and this cache only moves
// forward, under publishMu, in publish order.
//
// It is the LAST PUBLISHED frame, not a fresh rebuild. It can differ from a live
// Snapshot() only in fields the change key ignores, plus route/workspace metadata
// that moves without a publish (WorkspaceIndex.ObserveWindows) — bounded by the
// next publish, which every source drives.
func (v *View) CurrentFrame() state.Broadcast {
    v.frameMu.RLock()
    frame := v.frame
    v.frameMu.RUnlock()
    if frame != nil { return *frame }

    // Nothing published yet — only reachable before run()'s first publish
    // (StartViews closes viewReady before ServeReady, so no real client gets
    // here). Build under publishMu and ADOPT the key, so the first real publish
    // cannot fan out a snapshot taken EARLIER than the one already handed out.
    // Adopting is safe only because the cache is empty: no subscriber can be
    // holding an earlier frame to be stranded on.
    v.publishMu.Lock()
    defer v.publishMu.Unlock()
    v.frameMu.RLock(); frame = v.frame; v.frameMu.RUnlock()
    if frame != nil { return *frame }
    snapshot := v.Snapshot()
    v.publishedKey = state.SnapshotChangeKey(snapshot)
    built := state.NewBroadcast(snapshot)
    v.frameMu.Lock(); v.frame = &built; v.frameMu.Unlock()
    return built
}
```

**`View.Snapshot()` stays exactly as it is** — authoritative rebuild — because
`list-all` (`rpc.go:456-460`) and five `view_test`/`navigator_test` assertions
depend on it being current. **Only the subscribe stream uses the cache.** Smaller
blast radius; the asymmetry is worth one sentence in the doc comment.

Lock order everywhere: `publishMu → frameMu`, `publishMu → v.mu`. `frameMu` is
never held while taking `v.mu` (the swap precedes the `v.mu.RLock()`). No caller
holds `v.mu` while entering `publish()` — verify this stays true for
`SetRemoteFocusFrom` / `ClearRemoteFocusFrom` / `ClearRemoteFocusKey` /
`DropRemoteHost`, which all unlock before publishing today. No cycle.

### `View.Subscribe()` → `<-chan struct{}`

Payload-free over `chan *state.Broadcast` because the whole design says the
payload must not be consumed — making it payload-free renders the bug
unrepresentable rather than merely documented, and lets the fanout be a plain
non-blocking send. **Buffer: cap 1.** For a pure wakeup channel a deeper buffer
only causes redundant reads, and `struct{}` is zero-sized so it is not a memory
question. Low stakes; pick deliberately and note it.

### Two deliberate divergences from `Store.broadcast`

1. **No zero-subscriber early-out before the encode in the View.** The cache
   backs the initial read, so it must stay valid with nobody attached. And the
   cost is bounded: `SnapshotChangeKey` is *itself* a full marshal paid
   unconditionally on every publish, so skipping the wire marshal saves 2x, not
   ∞. At <0.5 publishes/s × ~19 KB that is ~19 KB/s into the void — not worth a
   lazy-encode state machine that would also require mutating `frame.JSON` after
   the frame is reachable.
2. **`Store.broadcast` must *invalidate* its cache on the zero-subscriber path**
   (`lastBroadcast = nil`), not leave it stale — otherwise a connection arriving
   mid-bar-restart gets pre-mutation state.

### `state.Store` — the same shape

Add `lastBroadcast *Broadcast` under a dedicated `frameMu sync.RWMutex` (**not**
`broadcastMu`, which is held across the encode+fanout).

- `broadcast()`: invalidate on the zero-subscriber path; store the frame before
  the fanout on the normal path. The generation-guard early-out
  (`gen <= s.broadcastGen`) needs no cache change — a newer broadcast already ran.
- `Store.CurrentBroadcast() Broadcast`: RLock fast path; on `nil`, double-checked
  build under `broadcastMu` (which serializes it against a concurrent fanout and
  enforces the existing `broadcastMu → s.mu.RLock` order), then cache.
  **Never adopts `publishedKey`** — `Apply` adopts it under the write lock
  independently of subscriber count, so the read path has nothing to adopt and
  adopting *would* strand. Serialization matters: two unsynchronized lazy builds
  could cache the older of the two and walk the cache backward.

**`Store.Subscribe()`'s element type stays `Broadcast`.** Do not touch it:
`broadcast_test.go:71/104`, `broadcast_order_test.go`, `publishstats_test.go` and
three tests in `publish_suppression_invariant_test.go` read the payload; the
payload is only a slice header; and `TestBroadcast_sharesOneEncodingAcrossSubscribers`
is the template this task wants preserved.

### The wire path for pre-encoded bytes

```go
// rawSnapshotResponse is Response with an already-encoded snapshot. It exists so a
// subscription can splice the shared per-publish encoding into the envelope instead
// of re-marshaling the same snapshot once per subscriber. json.RawMessage is written
// verbatim (compacted and HTML-escape-checked, both idempotent for bytes produced by
// state.NewBroadcast), so the frame is byte-for-byte what Response would have
// produced — which TestSubscribe_deliversCurrentWireFrameToEverySubscriber pins.
type rawSnapshotResponse struct {
    Snapshot json.RawMessage `json:"snapshot,omitempty"`
}

func writeSnapshotFrame(enc *json.Encoder, b state.Broadcast) error {
    if len(b.JSON) == 0 {
        return enc.Encode(Response{Snapshot: &b.Snapshot})   // fail open
    }
    return enc.Encode(rawSnapshotResponse{Snapshot: b.JSON})
}
```

Byte-identity holds because `omitempty` on a slice omits at `len == 0` exactly as
`omitempty` on `*state.Snapshot` omits at nil; `snapshot` is the first field in
both; and no other `Response` field is ever set on this path. Everything still
goes through the connection's single `json.Encoder`, so the stream cannot
desynchronize.

**Do not change `Response.Snapshot`'s type.** Seven `cmd/` decoders read
`resp.Snapshot.Sessions`: `switchboard-waybar/main.go:115`,
`switchboard-ctl/bottombar.go:191,237`, `claude-tui/main.go:64,119`,
`switchboard-polybar/main.go:92`, `switchboard-ctl/remote_stream.go:30`,
`internal/remotestate/frame.go`.

Both stream functions collapse to the same shape:

```go
// subscribeAll
if err := writeSnapshotFrame(enc, s.view.CurrentFrame()); err != nil { return }
// ... case <-ch:
if err := writeSnapshotFrame(enc, s.view.CurrentFrame()); err != nil { return }

// streamLocalSnapshots — SIGNATURE UNCHANGED, pinned by subscribe_test.go:99
if err := writeSnapshotFrame(enc, s.store.CurrentBroadcast()); err != nil { return }
// ... case _, ok := <-ch:
if err := writeSnapshotFrame(enc, s.store.CurrentBroadcast()); err != nil { return }
```

### The interface change and its blast radius

```go
type SnapshotView interface {
    Snapshot() state.Snapshot              // unchanged — list-all
    CurrentFrame() state.Broadcast         // new
    Subscribe() (<-chan struct{}, func())  // changed element type
}
```

Fully enumerated: `internal/rpc/rpc.go:203-206` (definition), `rpc.go:462-496`
(only consumer), `internal/rpc/federation_test.go:16-35` (`fakeSnapshotView`),
`internal/federation/view.go` (`*View` is the only implementation),
`internal/federation/view_test.go:183, 226`. `cmd/switchboard/federation.go:108`
passes `f.view` and needs **no edit**.

### The #6 dependency: one key function, not two

**Already complete in #6 (`eeaf7de`):** `SnapshotChangeKey` is exported from
`internal/state/state.go`, and its detached-copy implementation carries the
full soundness/aliasing comment. Use it directly.

**Do not** create a second key in `federation`, and **do not** create a new
package. `state` owns `Snapshot`; the key is a property of `Snapshot`.

**#6 must land first.** It rewrites this function's body; if #8's export lands
first, #6 rebases onto the new name — trivial but pointless — and #8 would be
committing a gate against an un-normalized key that cannot suppress the storm it
exists to suppress.

---

## Each constrained test, and how it is handled

**`subscribe_test.go:77` `TestLocalSubscribeTreatsQueuedBroadcastsAsNotifications`
— unchanged, stays green.** Traced: `Apply(A)` broadcasts with 1 subscriber →
caches A; `Apply(B)` → caches B; both wakeups queued. Initial
`CurrentBroadcast()` = **B**; wakeup 1 → **B**; wakeup 2 → **B**. Three frames,
all PID 2. The signature `(ctx, conn, enc, <-chan state.Broadcast)` is untouched,
so the direct call at `:99` still compiles.

**`subscribe_test.go:127` `TestSubscribe_deliversCurrentWireFrameToEverySubscriber`
— unchanged, stays green.** The seeding `Apply` happens with zero subscribers, so
the cache is invalidated and each connect frame comes from the lazy build (one
session ✅). The focus `Apply` runs with 3 subscribers → one encode → three
spliced frames. The `encodeFrame(t, resp)` round-trip at `:164-166` is exactly
the byte-identity assertion the splice needs, so **this test is the guard on
`rawSnapshotResponse`**.

*Optional strengthening:* add `bytes.Equal(frames[0], frames[i])` across the
three post-mutation frames and update the `:122-126` header comment, which now
understates the guarantee. Do **not** extend it to the connect frames — those are
legitimately rebuilt. Flagged as an owner call: it pins the shared-bytes property
end-to-end but would also break under a future legitimate per-connection filter.

**`view_test.go:179` `TestViewSaturationKeepsFinalReplacement` — rewritten, not
weakened.** It pins "the newest replacement survives a saturated subscriber". The
newest replacement is no longer *in* the channel, so asserting on the channel is
asserting on the wrong object. Same 9-publish body, then:

```go
if len(updates) == 0 { t.Fatal("saturated subscriber lost its wakeup") }
frame := view.CurrentFrame()
if len(frame.Snapshot.Sessions) != 1 || frame.Snapshot.Sessions[0].PID != 9 {
    t.Fatalf("current frame = %+v", frame.Snapshot.Sessions)
}
want, _ := json.Marshal(frame.Snapshot)
if !bytes.Equal(frame.JSON, want) { t.Error("shared encoding is not the encoding of the frame it arrived with") }
```

This pins *more* than the original: final state survives, a wakeup survives
saturation, and the shared bytes match the frame. All 9 publishes fire (each
`remote.replace` uses a distinct PID → distinct key).

**`view_test.go:220` `TestRunReadyPublishesQuietStateThatPredatedSubscription` —
rewritten, not weakened.** Replace `snapshot := <-updates` with `<-updates` then
`frame := view.CurrentFrame()` and the same PID-7 assertion, plus
`frame.JSON != nil`. It pins the same two things **and gains a new one**: the
gate must not swallow `run()`'s first publish. That new coverage is exactly what
kills the tempting "prime the cache in `NewView`" shortcut — priming would make
the first publish's key match and suppress it.

**`view_test.go:153` `TestViewRunPublishesSourceReplacement` — unchanged.** It
already does a bare `case <-updates:` and re-reads `view.Snapshot()`. Leave it
alone.

**`federation_test.go:12-35` `fakeSnapshotView` — updated with the interface.**
`updates chan struct{}`; `replace()` sends `struct{}{}`; add `CurrentFrame()`
returning `state.Broadcast{Snapshot: v.snapshot, JSON: mustMarshal(v.snapshot)}`
under the same RLock.

**`federation_test.go:106` `TestSubscribeAllTreatsQueuedValuesAsNotifications` —
keep, rename, and be honest about the loss.** The pre-queued *stale payload* at
`:113` becomes unrepresentable once the channel is `chan struct{}`, so this test
degenerates into "a queued wakeup causes a re-read that yields the current
frame". Still worth keeping (rename to
`TestSubscribeAllReReadsTheCurrentFrameOnEveryWakeup`, keep the 2-frame count,
which is what stops anyone adding dedupe), but **it no longer pins the hazard it
was written for.** Replace that coverage at the `federation` layer — see new
test 4.

**`broadcast_test.go:71` `TestBroadcast_sharesOneEncodingAcrossSubscribers` —
unchanged**, and reused verbatim as the shape for the new View test.

---

## New tests

All in `internal/federation/view_test.go` unless noted. Fanout is synchronous
inside `publish()` (non-blocking sends on a buffered channel), so `len(updates)`
immediately after `publish()` returns is deterministic — **no sleeps, no
flakes**. Same argument as `requireQuiet` at `broadcast_test.go:55-58`; cite it.

1. **`TestViewEncodesTheAggregateOncePerPublishRegardlessOfSubscriberCount`** —
   the DoD test. Direct transcription of `broadcast_test.go:71`, including its
   comment about why **pointer identity** is the only assertion that
   distinguishes "encoded once and shared" from "encoded N times into equal
   bytes". Ten `Subscribe()`s, one `publish()`, drain each channel, each
   subscriber calls `CurrentFrame()`, assert `&b.JSON[0] == &shared[0]` for all
   ten.
2. **`TestViewDoesNotFanOutARemoteFrameThatDiffersOnlyBySpinnerOrObservedAt`** —
   the DoD test. Publish a baseline remote session carrying
   `Wezterm.WindowTitle: "⠹ codex"` and an `AgentGraph`; drain. Replace with a
   spun title and `ObservedAt + 1s` (with `FreshUntil` **inside the same 5 s
   bucket**); `publish()`; assert `len(updates) == 0`. Then make a genuine change
   (a node runtime state, or a session status) and assert it **does** fan out —
   otherwise the test passes for a gate stuck shut.
   **Be honest in the comment about which leg does the work.** For a *remote* row
   `view.go` sets `session.Wezterm = nil`, so the spinner is discarded by the
   aggregate projection before the key is computed — a genuine and stronger
   property ("an update whose only delta is inside a field the aggregate discards
   must not fan out"), but **not** #6's normalizer being exercised. The
   `observed_at` leg is what proves the View inherits #6's key. Local-session
   spinner suppression is #6's test, not #8's.
3. **`TestViewStillDeliversTheFinalFrameOnRemoteDisconnect`** — the DoD test.
   Saturate a never-draining subscriber with 8–9 distinct publishes, then
   `remote.replace(map[string]state.Snapshot{})` (or `DropRemoteHost`) and
   `publish()`. Assert `len(updates) > 0` and that
   `CurrentFrame().Snapshot.Sessions` is empty. This is the guarantee the
   coalesce comment at `view.go:337-341` claimed and did not quite deliver.
4. **`TestViewCurrentFrameNeverPrecedesAnEarlierRead`** — **not in the DoD; add it
   anyway.** It replaces the coverage the `chan struct{}` change makes
   unrepresentable at the rpc layer, and is the direct View-level analogue of the
   pinned B→A test. Subscribe, `publish(A)`, `publish(B)` (both wakeups queued),
   read `CurrentFrame()` and assert it is B, then drain each queued wakeup and
   assert every subsequent `CurrentFrame()` is still B. Comment it as the
   View-side twin of `TestLocalSubscribeTreatsQueuedBroadcastsAsNotifications`.

In `internal/state/broadcast_test.go`:

5. **`TestCurrentBroadcastReturnsThePublishedEncoding`** — after an `Apply` with a
   live subscriber, `store.CurrentBroadcast().JSON` shares a backing array with
   the value the subscriber received (pointer identity again).
6. **`TestCurrentBroadcastRebuildsAfterAZeroSubscriberPublish`** — subscribe,
   `Apply`, cancel the subscription, `Apply` again (zero subscribers →
   invalidate), assert `CurrentBroadcast()` reflects the **second** mutation.
   **This catches the stale-cache trap directly.**

In `internal/rpc/subscribe_test.go`:

7. **`TestWriteSnapshotFrameSplicesTheSharedEncodingVerbatim`** —
   `writeSnapshotFrame(enc, state.NewBroadcast(snap))` must produce bytes equal
   to `encodeFrame(t, Response{Snapshot: &snap})`, and `writeSnapshotFrame` with
   `JSON == nil` must produce the same thing. Pins both the splice and the
   fail-open fallback against a future field added to `Response`.

---

## Implementation order (tree builds and tests green at every step)

1. **`internal/state`, additive only.** Add `NewBroadcast` and refactor
   `Store.broadcast` to use it; `SnapshotChangeKey` already exists from #6.
   `go build ./... && go test ./internal/state/...` green. No behaviour change.
2. **`internal/state`, the cache.** `lastBroadcast` + `frameMu`;
   cache-or-invalidate in `broadcast()`; `CurrentBroadcast()`; tests 5 and 6.
   Green, still no consumer.
3. **`internal/rpc`, local path only.** `rawSnapshotResponse` +
   `writeSnapshotFrame`; rewrite `streamLocalSnapshots`'s body; test 7.
   `TestLocalSubscribeTreatsQueuedBroadcastsAsNotifications` and
   `TestSubscribe_deliversCurrentWireFrameToEverySubscriber` must both stay green
   here — **this step is the cheap proof that the splice is byte-exact before
   touching the interface.**
4. **`internal/federation` + the rpc federation path, ATOMIC.** These cannot be
   split: the moment `View.Subscribe()` changes element type, `*View` stops
   satisfying `SnapshotView` and `cmd/switchboard` stops building. One commit:
   the View rewrite, the `SnapshotView` widening, `subscribeAll`,
   `fakeSnapshotView`, and the two `view_test` rewrites.
5. **The DoD tests** (1–4). Adding them last means each is written against
   finished behaviour rather than co-designed with it; **if any needs the
   implementation bent to pass, that is a design signal worth stopping on.**
6. **Comments and docs.** `view.go:18-21`, `view.go:302-304`, `rpc.go:484-489`,
   `rpc.go:504-509`, `state.Broadcast`'s doc, `golden_test.go:186`. These are the
   load-bearing explanations of an invariant whose *mechanism* just changed. Do
   not skip them.
7. **Verify.** `go test -race ./...`, expecting only the pre-existing
   `internal/conformance` failure (which #5 should have fixed by this point).

---

## Scope calls

**`remotestate.cloneSnapshotMap` (`manager.go:475-494`) — leave it alone; file a
follow-up.** Today each publish costs `(1 + N)` `Manager.Snapshot()` calls — one
from `View.publish()` and one per subscriber from `subscribeAll`'s
`s.view.Snapshot()` — each doing a JSON marshal **and** unmarshal per remote
host. At 11 subscribers × ~10 publishes/s that is ~130 round trips/s and is
plausibly the largest single allocator in the whole storm. **After #8 it is 1 per
publish**; after #10's gate, <0.5/s. A ~250x reduction with zero risk.

Optimizing further would mean replacing a deliberately-chosen detachment boundary
(its comment justifies the round trip as "the smallest reliable deep copy of
nested provider graph slices/pointers") with hand-written deep-copy code that
must be kept in sync with every future field — real correctness risk for a cost
#8 has already made negligible. Worth including in the follow-up:
`runLiveRoutes` (`routes.go:61`) calls `source.Snapshot()` on every remote
notification and only reads keys — it never needs a detached copy at all, and a
`SnapshotShallow()` there is a smaller, safer win.

**No `publish-stats` counters for the View.** #10 reads `publishes`/`suppressed`
from the Store's counters and the aggregate rate from `sb-mem-baseline`'s socket
capture, which measures the View's output directly. A second counter surface
would make the journal line ambiguous about which gate it describes.

---

## Genuine uncertainties

1. **Whether the lazy `CurrentFrame()` build should adopt the key.**
   Recommended yes — not adopting leaves a real startup rollback, and adopting is
   safe *only* under the empty-cache guard. But it is unreachable in the shipped
   daemon, so it is subtle machinery defending a window that cannot occur today.
   An executor uncomfortable with it could make `CurrentFrame()` panic before the
   first publish instead — not recommended, but the tradeoff is real and either
   choice needs its reasoning in the comment.
2. **Wakeup channel buffer: 1 vs keeping 4.** Cap 1 is the honest expression; cap
   4 minimizes the behavioural delta. Zero-sized element either way. Pick
   deliberately and note it.
3. **Whether to strengthen `TestSubscribe_deliversCurrentWireFrameToEverySubscriber`**
   with cross-subscriber byte equality — see above.
4. **HTML escaping idempotence under `json.RawMessage`.** `json.Marshal` and
   `json.Encoder` both escape HTML by default, and re-compacting already-escaped
   bytes with `escapeHTML=true` is idempotent (`<` contains no `<`). Test 7
   and `subscribe_test.go:164-166` both check it — but if anyone ever calls
   `enc.SetEscapeHTML(false)` on a subscription encoder, the splice silently
   diverges from a fresh encode. **Worth a sentence in `writeSnapshotFrame`'s
   comment.**
5. **Whether to move `SnapshotChangeKey` to its own file** — purely about merge
   ergonomics against #6's diff. Decide when you can see #6's actual patch.
