# Phase 0 — two correctness fixes

[Master](README.md). Depends on nothing. Two independent fixes, so use two
branches or two commits.

## 0a. A failed Claude fan-out scan must not renew freshness

**Bug.** `Observer.Observe` in `internal/provider/claude/observer.go` rebuilds
the graph even when the fan-out scan fails (`scanErr != nil`, ~:424-433):

```go
observation, err := o.rebuildLocked(rs, now, agentgraph.SourceClaudeTranscript, scanErr == nil && ...)
...
if scanErr != nil {
    // Preserve the last graph as a partial, newly-dated observation ...
    return observation, scanErr
}
```

`rebuildLocked` stamps `ObservedAt: now, FreshUntil: now.Add(o.freshness)`
(~:1046-1049). The coordinator's `observeAt`
(`cmd/switchboard/agent_observation.go` ~:397-411) records `observe_error` and
then applies the observation anyway. A read that failed therefore extends the
session's authority for another freshness window. That is #98's first
acceptance criterion, fixed early.

**Change.** On `scanErr`, return the root's last applied observation (whatever
`rs` holds from the previous successful rebuild) with its **original**
`ObservedAt` and `FreshUntil`, marked partial, together with the error. If
there is no prior observation, return an empty one so the coordinator takes its
existing `snapshot_pending` → `expireCurrent` path. Do not touch
`reconcileRootRuntime`. Its failed-read branch already keeps the prior runtime
without fabricating idle.

Check what else the rebuild does on that path before removing it: the comment
says the partial graph exists so that "Legacy Reconcile likewise holds its last
count". The count must still be held; only the dates must not move.

**Tests** (`internal/provider/claude/observer_test.go`, plus one coordinator
test):
- should keep the prior FreshUntil when the fan-out scan fails;
- should let the graph expire to unknown at its original deadline when every
  scan after it fails;
- should resume a fresh observation on the first successful scan after
  failures;
- should not fabricate idle or drop children when a scan fails.

Simulate the failure through whatever seam the fan-out scanner already offers
in tests. If there is none, inject the directory reader.

## 0b. Death callbacks fenced by session lifetime

**Bug.** `appear` in `cmd/switchboard/main.go` (~:262-270) registers:

```go
procSrc.Watch(ctx, info.PID, func() {
    store.Apply(func(m map[int]*state.Session) {
        endSession(m, info.PID, sink, forgetRoot, time.Now())
    })
})
```

`endSession` (~:373-385) deletes `m[pid]`, whichever session now holds that
PID. Two concrete hazards follow:

1. **A late callback for an old lifetime ends its replacement.** The liveness
   sweep closes the session first, the scanner re-discovers the reused PID,
   and then the old pidfd's callback runs.
2. **A replacement can be left unwatched.** In `linuxSource.Watch`
   (`internal/osproc/source_linux.go` ~:64-120), a second `Watch(pid)` is a
   no-op while the old watcher is still in `s.watched`. The old goroutine
   deletes its own entry only after `onDeath` returns (deferred, ~:88-92), so a
   re-`appear` that races into that window registers nothing.

`Stop(pid)` has no daemon caller today. Do not widen scope to it beyond
keeping it correct.

**Change.**
- The death closure captures the session's `StartedAt` at registration, and
  ends the session only if `m[pid]` still has that `StartedAt` (and Agent).
  Add an `endSessionIf`-style guard; `endSession`'s once-only contract (L5)
  stays.
- In `linuxSource.Watch`, remove the `watched` entry **before** calling
  `onDeath`, so that a `Watch` made from inside or after the callback registers
  a new watcher. Alternatively, key the watched set by a registration token and
  let a newer registration replace an older one. Pick whichever keeps
  `Watched()` and the conformance tests meaningful.

This is a stopgap. `StartedAt` is not a birth token, and `appear` copies it
onto a reused PID of the same agent (~:240-243), so a same-agent reuse while
the old session is still in the map is not fenced. Phase 4 fixes that
properly. Note it in a code comment that points at #97.

**Tests** (`cmd/switchboard` with a fake `procSrc`, and `internal/osproc`):
- should not end a replacement session when the previous lifetime's death
  callback fires late;
- should still end the session whose lifetime died;
- should register a new watcher when the same PID is watched again from within
  or right after its death callback (Linux; skip where pidfd is unavailable);
- `endSession` stays once-only: two triggers for one death record one
  `session_end`.

## Done when

Both fixes are merged with their tests, and `TZ=UTC go test ./...` passes. Set
the master table's Status to merged. No deploy is needed for Phase 0 on its
own; it rides the next deploy.
