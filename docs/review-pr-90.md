# Adversarial review: PR #90 (`feat/remote-hysteresis`)

Reviewed 2026-08-26. Branch: `feat/remote-hysteresis`, 5 commits vs `main`.
`go build`, `go vet`, `go test -race` pass. Ten finder angles produced 32
deduped candidates; 31 survived adversarial verification (one refuted); a gap
sweep added one confirmed finding (merged into #3, same root cause). Ranked
most severe first, capped at 15.

## Correctness: the hold machine

### 1. `internal/remotestate/hold.go:173` — Stale-to-fresh flip on transport loss reintroduces the flicker

If the keepalive-silence deadline has already published `Stale=true` and the
SSH link *then* dies, `endContact` (`manager.go:528-530`) sets
`connected=false, lostAt=now`, and `phaseAt`'s disconnected branch derives the
phase from `lostAt` alone, ignoring the existing stale verdict and
`lastContact`. `phaseAt` returns `phaseFresh`, `h.stale` goes true→false,
`publishLocked` emits every row with `Stale=false, LastContact=nil`, and the
timer re-stamps Stale 6s later. Waybar/TUI brighten then dim — the flicker the
hold exists to remove — and the 45s drop countdown restarts from disconnect,
so a host silent for 30s is shown for at least 75s after its last frame.

Scenario: peer advertises keepalive 10s; remote-stream stops writing while
sshd still answers ServerAlive (wedged remote, or client resumes from suspend
and the overdue timer fires) → `onDeadline` publishes Stale. Link then drops.
`endContact`'s only early return is `!h.connected`, which the silence path
never sets.

Fix: skip the quiet phase when `h.stale` is already true, or origin the hold
at `lastContact`.

### 2. `internal/remotestate/manager.go:515` — immediate-drop branch races an in-flight `onDeadline`

The closeout / protocol-error / `HoldFor==0` branch captures `h.epoch` without
bumping it or stopping `h.timer`. An already-dispatched `AfterFunc` callback
carrying the same epoch can then:

- take `m.mu` after `dropHost#1` unlocked to run `OnHostRemoved`; `armLocked`
  bumps to E+1; `dropHost#2` sees `removing` and returns; `dropHost#1` resumes
  at line 580, sees `h.epoch != E`, deletes the tombstone and republishes. The
  host is back in `m.hosts` with `connected=false, stale=true, timer=nil`; no
  code path re-arms it (`!h.connected` early return on every retry), so the
  stale row persists until daemon restart; or
- bump before `dropHost#1`'s first lock, silently losing the closeout/protocol
  drop, leaving the host `connected=true`/stale with no timer until the next
  attempt holds it for another 45s.

The fake clock runs callbacks inline and `Stop` always suppresses, so no test
can show it.

Fix: `h.epoch++` and `h.timer = nil` under the lock before capturing the epoch
(as the transport branch does via `armLocked`), and/or have `onDeadline` treat
`m.removing[host]` as obsolete.

### 3. `internal/remotestate/hold.go:177` — `QuietFor > HoldFor` bypasses `MaxHoldFor`

The fresh branch arms the single timer for `staleAt`, which lies past `dropAt`
when `QuietFor > HoldFor`. `QuietFor` is unbounded (`NewManager` only rejects
`<0`; `main.go` passes `-remote-quiet` through). `switchboard -remote-hold 45s
-remote-quiet 24h` keeps a gone host's rows un-dimmed and navigable for 24h,
contradicting the `ManagerConfig` comment and the 10m `MaxHoldFor` guarantee.
Failed retries take the `!h.connected` early return at `manager.go:520`, so
nothing else drops it.

Fix: return `min(staleAt, dropAt).Sub(now)`, and reject `QuietFor > HoldFor`
in `NewManager` (the flag help already says "how much of -remote-hold").

### 4. `internal/remotestate/hold.go:117` — "no contact" age is wrong for peers without keepalive

Stale rows stamp `LastContact` from `h.lastContact`, the client clock at the
last accepted *frame* (the only write to that field). A remote running a
pre-keepalive `switchboard-ctl` (`KeepaliveSeconds=0`, publishes only on
change) that sits idle 3h on a healthy link and then drops renders "stale (no
contact 3h)" 6s after disconnect — contradicting `state.go:62-66` ("how long
IT has been out of touch"). Even with a new peer the tooltip overstates by up
to ~25s.

Fix: stamp `max(lastContact, lostAt)` on the disconnected branch; keep
`lastContact` only for the connected-but-silent case.

### 5. `internal/remotestate/manager.go:578` — `stopHolds` doesn't fence callbacks already inside `dropHost`

`stopHolds` sets `stopped` and calls `Timer.Stop`, but an `onDeadline`
callback that passed its single `m.stopped` check (`hold.go:217`) and is in
`dropHost`'s unlocked window is neither waited for nor re-checked.
`OnHostRemoved` (`registry.DropLiveHost`, `view.DropRemoteHost` → publish) and
`publishLocked` can run after `Manager.Run` and `federationRuntime.Wait`
return — exactly what the `stopped` field doc (`manager.go:148-149`) claims
cannot happen. Benign today (registry/view are mutex-guarded, subscribers
already unregistered).

Fix: re-check `m.stopped` in `dropHost`'s first locked section, or track
in-flight callbacks with a `WaitGroup` that `stopHolds` waits on.

## Correctness: elsewhere in the diff

### 6. `cmd/switchboard-ctl/remote_stream.go:47` — first SIGTERM can be swallowed

`signal.Notify` is installed before `OnAttach`/subscribe/the select loop, so
the first SIGTERM/SIGINT/SIGHUP is consumed into the closeout channel and
`signal.Stop` resets the disposition. If `StreamLocal` is blocked outside its
select (stdout write to a pipe sshd stopped draining — a TCP black hole toward
the client with sshd alive fills the channel window plus the 64KiB pipe and
`w.Write(body)` at `frame.go:180` blocks), a single `kill <pid>` does nothing;
only a second signal kills the process, where `main` killed on the first. Host
shutdown is unaffected (sshd dies from the same SIGTERM, EPIPE → SIGPIPE).

Fix: call `signal.Stop` from `StreamLocal` when it reads the channel, or exit
from the goroutine if the reason isn't consumed within a bound.

### 7. `cmd/switchboard/federation.go:91` — pane binding survives remote restart with PID reuse

Moving `OnHostRemoved` (`registry.DropLiveHost`) from disconnect to end-of-hold
lets a pane binding and `liveHost` entry survive a remote daemon restart. A
same-kind agent that reuses the PID during the downtime inherits the persisted
`started_at` (`main.go:206-207` copies `prior.StartedAt`; not a kernel birth
token), so `ReplaceLive` sees `(N, T_old)` still present and keeps P1.
`OnAttach`'s re-announce only adds/evicts by pane identity; if the announce
fails or the new process has no local pane, a click on that chip focuses P1,
which no longer shows that process. Low probability, but the `DropLiveHost`
doc (`registry.go:208-211`) still promises the re-announce-before-actionable
guarantee that is now 45s late.

Fix: drop live-host bindings at disconnect while keeping rows, or update the
contract.

### 8. `internal/remotestate/stream.go:110` — fractional keepalive advertises a shorter period than sent

`advertised = int(period / time.Second)` truncates, and the range check on
line 105 accepts any duration in `[1s, 300s]`. `StreamOptions{Keepalive:
1500ms}` advertises 1, so the client deadline is `lastContact+3s`; a single
frame delayed to ≥3.0s (`!now.Before(deadline)` is true at equality) marks the
host Stale and the next frame un-stales it — a one-frame flap that violates
`hold.go:66-70`'s "one lost frame must not be enough". Library callers only
(ctl uses the 10s default).

Fix: reject `period % time.Second != 0`, or advertise the ceiling.

### 9. `internal/remotestate/stream.go:183` — `receiveSnapshots` goroutine outlives `StreamLocal`

On Closeout (nil) or a write error, `StreamLocal` returns while the goroutine
is parked in `rpc.Client.Recv` (plain json Decode over `net.Conn`, no ctx) or
on the unbuffered `snapshots` send. `StreamLocal` neither cancels a derived
context nor closes the client. Any caller invoking it more than once in a
long-lived process leaks one goroutine and one daemon subscription per call;
masked in `switchboard-ctl` only because `main`'s deferred `c.Close()` and
process exit follow immediately.

Fix: `ctx, cancel := context.WithCancel(ctx); defer cancel()` at the top of
`StreamLocal`, and document that the caller must Close the client (or add
`Close` to `SubscriptionClient`).

### 10. `internal/remotestate/manager.go:759` — `detachSnapshot` fallback aliases stored rows

The fallback returns the un-detached struct whose `Sessions` backing array is
shared with the stored `hostState.snapshot`; `detachViews` then calls
`stamp()` on it, writing `Stale`/`LastContact` into the stored rows and
breaking the "stored snapshot is only ever replaced, never mutated" invariant
`viewLocked`'s lock-free copy relies on. Latent: needs `json.Marshal` of a
`state.Snapshot` to fail, which no current field can cause; a future
NaN float / non-string map key / erroring custom marshaler turns a stale
host's `Snapshot()` into a data race.

Fix: on fallback, `append([]state.Session(nil), s.Sessions...)` before
returning.

## Docs and design claims

### 11. `README.md:300`, `docs/remote-hysteresis.md` (Keepalive), `frame.go:66-69` — keepalive never beats SSH on a black hole

All three claim the 10s application keepalive "marks rows stale without
waiting for SSH to give up", but the silence deadline is 3×10s = 20–30s after
last contact while `ServerAliveInterval=5 / CountMax=3` declares the link dead
at ~15s. Black hole at T: ssh exits ~T+15s → EOF → `lossTransport` → quiet
window → Stale at ~T+21s from the *hold* path; `phaseAt` evaluates the silence
deadline only while connected (`hold.go:155`), which is already false. The
silence detector only matters when TCP/sshd stay alive but remote-stream or
its daemon subscription wedges (`hold.go:62-64`'s "nominally up" wording is
the accurate one). The tightened ServerAlive also adds an SSH probe every ~10s
per idle remote for a detection gain the 45s hold masks.

Fix: correct the three claims, or lower `silenceMultiple`/keepalive so the
app path genuinely leads.

### 12. `internal/remotestate/stream.go:25` — "older client's publish gate suppresses the broadcast" is false

The `DefaultKeepalive` comment (and the docs cross-version table) says an
older client suppresses redundant keepalive broadcasts, but on `main`
`Manager.accept` publishes unconditionally (`m.live[host]=...;
m.publishLocked()`), `federation.View.run` publishes on every remote wakeup,
and rpc `subscribeAll` encodes the full aggregate per wakeup with no dedupe.
In the ordinary mixed-version deployment (client on `main`, remote on this
branch) every 10s per remote host the old client does a `cloneSnapshotMap`
JSON round trip, a full `View.Snapshot` merge, and a full-aggregate encode +
write to every subscribe-all client (each waybar slot, claude-tui). No wrong
output, but it is precisely the periodic publish churn
`docs/memory-footprint-plan.md` is chasing, on the machine that was not
upgraded.

Fix: state the constraint honestly (upgrade clients first) or make ctl opt in
to advertising.

### 13. `internal/state/state.go:680` — `ObservablyEqual` inserted inside `snapshotChangeKey`'s doc block

`ObservablyEqual` sits between `snapshotChangeKey`'s 40-line comment (starts
line 640) and the function it documents, with no blank line. `go doc
./internal/state ObservablyEqual` begins with "snapshotChangeKey encodes
everything about a snapshot ..." and `snapshotChangeKey` (line 701) is
undocumented.

Fix: move `ObservablyEqual` (with its comment) above line 640.

## Quality

### 14. `cmd/switchboard-waybar/main.go:353` — three stale wordings, and "stale" now means two things in one tooltip

Waybar (` · stale (no contact 1m)`), claude-tui (`  stale 1m`) and ctl list
(` stale-since=<RFC3339>`) each hand-roll the held-row annotation instead of
sharing a helper next to the existing cross-renderer precedent
`internal/label/label.go:179` (`BlockedWriters`). "stale" now collides with
the same tooltip's agent-row `· stale` (`agentTooltip` 408-413, meaning
AgentGraph not Fresh) — a held remote row almost always also has a non-fresh
AgentGraph, so one hover reads both meanings. `switchboard-ctl list` labels
the value `stale-since` although it is the last-contact time and staleness
begins `QuietFor` later.

Fix: put the vocabulary in `internal/label` (e.g. `label.HeldSince`) and pick
a distinct word for contact loss.

### 15. `internal/remotestate/manager.go:479` — two full-snapshot marshals and a timer realloc per frame

`accept()` pays two full-snapshot `json.Marshal` calls (`state.ObservablyEqual`)
on every frame for an existing non-stale host — including changed frames where
the answer is "changed" regardless — on top of `DecodeFrame`'s three parses of
the same bytes, and `armLocked` (`hold.go:187-202`) stops and re-allocates the
`AfterFunc` timer + closure on every frame for any keepalive-advertising peer.
A busy remote at ~9 frames/s adds ~18 marshals/s and ~9 timer-heap
delete/insert + closure allocs/s per host, under `m.mu`.

Alternatives: a keepalive is a verbatim re-send, so the wire line is
byte-identical to its predecessor — keep the last accepted line per
destination and on `bytes.Equal` skip `DecodeFrame` and `ObservablyEqual`,
just refreshing `lastContact`; cache `snapshotChangeKey` on `hostState` so a
real compare is one marshal; leave the silence timer armed when the host was
already connected and the phase is unchanged (`onDeadline` already recomputes
from the fresh `lastContact` and re-arms), keeping the epoch bump only on the
`wasRemoving` path that `dropHost`'s second check depends on.

## Cut at the cap (all confirmed, lower value)

- `validateCloseoutReason` duplicates `CanonicalHostname`'s byte-class loop (`frame.go:117`).
- `DecodeFrame` re-implements every `validateFrameBody` rule inline (`frame.go:212`).
- polybar ignores `Session.Stale` (pre-existing federation gap).
- `closeouts` map replaceable by a typed error in `readErr`.
- `hostState.owner` duplicates `hostOwners`.
- `lossCategory` collapsible to one `case outcome.Held:` arm.
- `removing` map could be a bool on `hostState`.
- `|| m.stopped` clause in `endContact` is unreachable.
- Tests-alongside-implementation: `23722ee` adds `ObservablyEqual`/`Stale`/`LastContact` with no test in `internal/state`; `31470c3` adds `watchForTeardown` and the `-remote-hold`/`-remote-quiet` flags with zero tests.
- One-logical-change-per-commit: `71ebe80` (1870 insertions) bundles the SSH ServerAlive change, the `StreamLocal` move + keepalive, the `Frame.Snapshot` pointer change, and the hold machine.

## Verdict

Block on 1–3: #1 defeats the PR's stated purpose in a realistic path, #2 can
wedge a host until daemon restart, #3 bypasses the hold ceiling via a flag.
4–5 are cheap fixes in the same file. The rest are follow-ups.
