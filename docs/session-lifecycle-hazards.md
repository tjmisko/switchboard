# Session-Lifecycle Hazards — the ghost-lane class

> **Goal:** name the class of bugs where a session's swimlane keeps growing long
> after the session is over — its final status interval stretched all the way to
> `now` — because the daemon never observed the session end and so never wrote
> the `session_end` that closes the lane. Keep a running, executable catalog of
> the specific lifecycle scenarios so each one is pinned as a regression.
>
> The sibling doc `timing-hazards.md` catalogs the *color-of-a-live-chip* class
> (hook-vs-transcript skew). This one catalogs the *is-the-session-still-alive*
> class. Different signals, same shape of defect: two decoupled event streams
> reconciled against a clock that no longer reflects reality.
>
> The catalog lives in `cmd/switchboard/session_lifecycle_test.go`
> (`TestSessionLifecycleHazards` and its siblings); every `Ln-…` id below maps to
> a row or a test there.

---

## 1. The two lifecycle signals

A session's swimlane is bounded by two events, written from two very different
places (`cmd/switchboard/main.go`):

1. **`session_start`** — the process-scanner discovers a new `claude`/`codex`
   pid (`onAgentAppeared`) and records it. It carries only `pid`/`agent`/`cwd`;
   the `session_id` is not known until the first hook fires.
2. **`session_end`** — the **in-memory process death-watch** fires. When
   `onAgentAppeared` registers a session it also calls
   `procSrc.Watch(ctx, pid, cb)`; `cb` records `session_end` and deletes the
   session from the store. This is the **only** writer of `session_end`.

The reader (`history.BuildSwimlanes`) groups events **by pid** and closes each
lane at the first of: its `session_end`, or — for a lane still open when the
event stream ends — the caller's `end` bound. For today's live day the ctl
clamps that bound to **`now`** (`cmd/switchboard-ctl/timeline.go`, "extend a
running session to the present"). So a lane with no `session_end` does not merely
stop at its last event — its **final interval is stretched to `now`**, whatever
color that interval happened to be.

That stretch is correct for a genuinely-live idle session (it *is* idle, up to
now). It is a **ghost** when the session is actually dead and the death was
simply never seen.

---

## 2. The skew (root cause): the death-watch is not durable

`procSrc.Watch` is a **pidfd/kqueue watcher living in daemon memory**. It has no
on-disk state and no replay. Three ways its `session_end` is lost, all rooted in
the same fact:

- **Daemon restart / SIGKILL.** When the daemon exits, every registered watch
  dies with it. Any process that dies **while the daemon is down**, or in the
  restart window, fires no `session_end` — the watcher that would have caught it
  no longer exists, and the replacement daemon never registered one for a pid it
  discovered *before* the restart. The only startup reconciliation,
  `dropStaleSessions`, **silently `delete`s** a hydrated-but-dead session instead
  of recording its end — so the lane is never closed.
- **Watch-registration failure.** `procSrc.Watch` returning an error is only
  logged (`main.go`, "watch pid=%d: %v"); that session then has **no death-watch
  for its entire life**, and its lane can only ever close by the reader's
  end-bound → a ghost to `now`.
- **The reconciler defers to a watch that may never fire.** Every tick the
  reconciler already reads `proc.State(sess.PID)` and, on `ErrGone`, comments
  *"the procwatch death callback will drop the session shortly"* and does
  nothing. If that callback was orphaned by a restart, "shortly" is **never**.

```
T0      scanner discovers pid P → session_start,  Watch(P) registered (daemon A)
T0+…    P does real work, then goes quiet (idle / dormant / mid-turn working)
T1      daemon A is stopped/​SIGKILLed          → Watch(P) is GONE, no session_end
T1+ε    daemon B starts, dropStaleSessions runs → if P already dead: silent delete
                                                   if P alive:        re-Watch(P)
T2      P finally dies                          → nobody is watching → no session_end
now     BuildSwimlanes closes P's open lane at `end = now`
        → P's final interval (whatever color) spans T0+… → now
```

This is a **liveness-observation** bug, not a clock bug: the lane's end is dated
correctly (to `now`) *given* the daemon's belief that the session is still
running. The defect is that the belief is stale — the one signal that would
correct it (`session_end`) is carried on a channel (an in-memory watch) that does
not survive the daemon.

### The invalidated contract

`decisions.md` row 12 records an explicit assumption this bug violates:

> the seen-set relies on `procwatch` calling `Forget` on death, which **always
> happens** (death observed via pidfd, exactly once). A recycled PID is only
> shadowed if a death was missed — which **the death-watch contract prevents**.

The death-watch contract does **not** hold across a daemon restart. Everything
that leans on "death is observed exactly once" — the scanner's recycled-pid
seen-set, the fanout Observer's per-session prune, and `session_end` itself —
inherits this hole. Restoring the contract at its source therefore fixes more
than the timeline.

---

## 2.1 The 2026-07-22 episode (three 4½-hour ghosts)

The dashboard showed three subagent sessions "running" for 4½ hours each,
inflating the day's active-time and force-multiplier numbers. All three were
long dead. The daemon had restarted three times that day —
`10:35:26`/`10:35:27` (a rapid double restart) and, decisively,
`14:09:23` **`code=killed, status=9/KILL`** — each one wiping the live watch set.

| lane (name) | pid | session_id | last real event | final interval | rendered end |
|---|---|---|---|---|---|
| `debug-paused-agent-pump` | 3407477 | `4a9af989` | 10:28:09 (`→working`, then dormant) | **dormant** 10:28→now | 15:08:42 (`now`) |
| `transcript-review-pipeline` | 3503954 | `a23b64b5` | 10:32:46 | **working** 10:32→now | 15:08:42 (`now`) |
| `obsidian-map-view-6f` | 3662298 | `9520ecfe` | 10:32:29 | **idle** 10:32→now | 15:08:42 (`now`) |

All three were discovered in the **first** 10:14–10:32 window, *before* the 10:35
restart. Their watches were registered by the daemon instance that the 10:35
restart replaced; the replacement never re-watched a pid it had not itself
discovered as still-alive, and by the time they died no watcher remained. No
`session_end` was ever written, so `BuildSwimlanes` stretched each lane's last
interval — `dormant`, `working`, `idle` respectively — to `now`.

The telltale is structural, and visible without any liveness probe: **each
ghost's `session_id` reappears on a *second, later, properly-closed* lane** — the
session was resumed in a fresh pane (a new pid) that carried the real work and
died cleanly with a `session_end`:

| ghost lane | its bounded twin (same session_id) |
|---|---|
| pid 3407477 `4a9af989` $8.78 (dormant→now) | pid 11569, ended 14:48:54 via `session_end`, $111.88 |
| pid 3503954 `a23b64b5` $7.85 (working→now) | pid 13899, ended 11:58:12 via `session_end`, $43.35 |
| pid 3662298 `9520ecfe` $0.61 (idle→now) | pid 3149, ended 12:10:12 via `session_end`, $41.30 |

The low cost of each ghost ($0.61–$8.78) is the giveaway that it did only a few
minutes of real work before the daemon lost it — the 4½ hours are entirely the
stretched final interval, not activity. Because two of the three end in an
*active* color (`working`, `dormant` counts toward delegated-active), the ghosts
inflate `effective time gained`, the `force multiplier`, `attention_union`, and
`by_status` totals — every duration-derived number for the day.

A secondary amplifier: `BuildSwimlanes` groups **by pid**, while
`history-schema.md` states the reader should *"group a session's events by
`session_id` when present."* Had the two same-`session_id` lanes been merged, the
resumed twin's `session_end` would have capped the ghost. Pid-grouping keeps them
split, so the dead pid ghosts on its own. Grouping by `session_id` would mask
*this* (resume) shape, but not the general case — a session that dies during
downtime with no later twin still ghosts. The durable fix is at the source.

---

## 3. The fix: make `session_end` delivery robust at the source

Restore the "death observed exactly once" contract by not depending on an
in-memory watch surviving. Defense in depth, source-first (mirrors
`timing-hazards.md` §2, "closing the race at its source"). All of the following
is implemented in `cmd/switchboard/main.go`.

**One writer, three triggers.** `endSession` is now the **single** writer of
`session_end`. It records the event, drops the session from the store map, and
calls the scanner's `Forget` (so a recycled pid is re-discovered — the
`decisions.md` §12 dependency). Its three triggers are the pidfd death-watch
(fast path), the liveness sweep (backstop), and the startup stale-drop.

**F1 — `sweepDeadSessions`: poll-based liveness reconciliation (the durable
backstop).** It runs first in every reconcile tick, before any per-session work,
and closes the lane of any session whose process is definitively gone. It reads
through the same `osproc.Source` the scanner and death-watch use and depends on
**no prior state at all**, so it self-heals a lost, failed, or orphaned watch
within one `reconcile-interval` (5 s) — across restart, SIGKILL, and
registration failure alike. The reconciler no longer defers to the death
callback on `ErrGone`.

**F2 — `dropStaleSessions` records `session_end` (closes the startup gap).** For
each hydrated session whose pid is already gone at startup, it records the
`session_end` *before* deleting, instead of silently dropping it. This bounds
lanes for deaths that happened while the daemon was down, before F1's first tick
runs. Dated at startup-`now` — the same clock the death-watch would have used.

**`sessionDead` judges only positive evidence of death.** A pid counts as dead
on `osproc.ErrGone`, or on a successful read that classifies as `AgentNone` (the
kernel recycled it). **Any other read error reports "not dead"** and is retried
next tick. This matters twice over: it keeps a transient failure from
fabricating an end, and it keeps the darwin backend — whose `Read` returns
`ErrUnsupported` for *every* pid — from ending every session on the first tick.
Liveness is never inferred from inactivity, which would falsely end a live idle
session (L4). A false `session_end` is strictly worse than a late one: it splits
a running session into two lanes and permanently under-counts the second.

**Idempotency is automatic.** All three triggers act only on sessions *present in
the store map*, and each removes the session under the store lock. Whichever
fires first deletes it; the others find nothing and record nothing. Map
membership is the dedup, exactly as it is for the recycled-pid seen-set (L5).

**F3 — Reader grouping, by `session_id` (implemented).** `BuildSwimlanes` now
groups by session id, the identity `history-schema.md` already defined as
canonical, falling back to pid only for the pre-hook `session_start` lead-in.
This does not fix the ghost — F1/F2 do — but it fixes the *attribution* bugs the
ghost investigation surfaced, which were live in the same day's log: six pids
hosting up to four sequential sessions each were being merged into one lane with
their costs summed and only the last one's name and focus spans kept, while four
resumed sessions were split across two lanes. On 2026-07-22 that was 20 pid-lanes
standing in for 28 real sessions; regrouping yields 38 lanes over the full day
with total cost and tokens exactly conserved.

Two rules make it work, and both are load-bearing:

- the provisional pid-keyed lead-in is **adopted** by the first session id seen
  on that pid, so a lane still starts at its `session_start`;
- the **global** focus and activity streams are excluded from lane routing
  entirely. A focus event is keyed by the session that *gained* focus, not by
  the session its emitting pid is running, so letting it route would read as
  "a different session took this pid" and close a live lane early.

A remaining option, not implemented: have the ctl pass the live-pid set into
`BuildSwimlanes` so a lane whose pid is provably dead caps at last-observed
activity rather than `now`. That would bound the blast radius of any *future*
missed end, but F1/F2 remove the cause.

### Accuracy note — dating the late `session_end`

F1 dates the end at the tick that first notices the death: up to one
`reconcile-interval` late (≤5 s — negligible). F2's startup case is dated at
`now`, which over-counts by however long the daemon was down (e.g. the 10:35
restart would have capped the ghosts at ~10:35 instead of ~10:32 — minutes, not
hours). A refinement dates the reconstructed `session_end` at the session's
last-known activity (`StatusSince` / last transition) rather than `now`, trading
a small under-count for immunity to long downtime. Pick one policy and pin it in
the test.

---

## 4. The catalog

| id | scenario | the OS says | daemon/reader must |
|----|----------|-------------|--------------------|
| **L1-restart-orphans-watch** | a discovered session goes quiet, the daemon restarts/is SIGKILLed, the session later dies with no watch alive to see it | `ErrGone` at a later reconcile tick | `sweepDeadSessions` records `session_end` and drops it; the lane closes at that tick, not at `now`. |
| **L2-death-during-downtime** | the session dies while the daemon is down; hydrated state still holds it | `ErrGone` at startup | `dropStaleSessions` records `session_end` **before** deleting. A live sibling in the same map is kept and re-watched. |
| **L3-watch-registration-failed** | `procSrc.Watch` errored at discovery, so this session never had a death-watch at all | `ErrGone` at a later tick | the sweep backstops it identically to L1 — the lane still closes. |
| **L4-live-idle-not-a-ghost** *(contrast)* | a genuinely-live session sits idle for hours | a valid `claude` snapshot | **do nothing** — no `session_end`, session stays tracked, nothing forgotten. The lane legitimately extends to `now`. Liveness keys on pid-death, never on a no-activity TTL. |
| **L4b-recycled-pid-is-a-death** | the pid resolves, but the kernel handed it to a non-agent process | a `bash` snapshot → `AgentNone` | treat as a definitive death: close the lane. |
| **L4c-unreadable-is-not-a-death** *(contrast)* | a transient read failure, or the darwin backend that cannot answer for any pid | `ErrUnsupported` | **do nothing** — a fabricated `session_end` would split a running session into two lanes. Re-check next tick. |
| **L5-no-double-end** | both the sweep and a late pidfd callback observe the same death | `ErrGone` to both | exactly **one** `session_end`; whichever deletes the session first wins, every later trigger no-ops. |
| **L6-ghost-lane-bounded** *(the 2026-07-22 shape)* | a session stops at T, but nothing closes its lane | — | with no `session_end` `BuildSwimlanes` stretches the lane to `now` (the ghost, pinned as the before-state); with the `session_end` the fix emits, both the lane **and its final interval** end at T. |

L6 also pins the *resume* shape that produced the three observed ghosts: one
`session_id` running on pid A, then resumed on pid B, where only B's death was
ever observed. F1/F2 close A at its own death, so it no longer ghosts to `now`
regardless of the pid-vs-`session_id` grouping discussed in §2.1.

### Adding a hazard

When a new lifecycle scenario surfaces:

1. Add a row to `TestSessionLifecycleHazards` with a new `Ln-…` id, modeling the
   three phases (events at discovery → what happens to the process/daemon →
   expected lane end).
2. Add the matching row to the table above with a one-line "why it's tricky".
3. If it exposes a new way the death signal is lost (not just a new shape of an
   existing one), extend §2.

The test is the source of truth; this document is its index.
