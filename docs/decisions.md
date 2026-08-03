# Decision Register — characterization (⚠) items

Phase 0 pinned twelve cross-layer quirks as **characterization tests** (they
capture *current* behavior, so the suite is green on `main`). This register
records a verdict for each under the **pin-then-fix** policy.

Scope decision (Phase 0.9): implement only the **seam-critical** fix now — the
`StartedAt` PID tie-break — because the WM/terminal refactor's selector contract
depends on it. Every other item keeps its characterization test and is corrected
(or deliberately preserved) when Phase 1 extracts the relevant seam. Each row
links its spec section (`docs/behavior-spec.md`) and the test that pins it.

Verdict legend:
- **FIXED (0.9)** — corrected now; the characterization test was flipped to the intended behavior.
- **FIX @ Phase N** — bug; correct when Phase N touches that seam (test flips then).
- **PRESERVE** — current behavior is intended; the test stays as a guard, behavior unchanged.

| # | Item | Spec | Pinned by | Verdict | Notes |
|---|------|------|-----------|---------|-------|
| 1 | **Address `0x`-prefix normalization** (highest risk) | §7.1, §13.2 | `conformance` normalization round-trip + `TestHyprlandManagerConformance` | **FIXED (1.3)** | The `internal/wm` Hyprland backend now owns normalization: `Subscribe` converts socket2 event addresses into Clients() form via `NormalizeEventAddress`, so the daemon consumes already-comparable refs and no longer reconstructs `"0x"+Data` at the event boundary. The conformance adapter drives the backend's real helper, so the round-trip contract verifies the seam's actual logic. |
| 2 | **`Snapshot` `StartedAt` sort tie-break** | §4.2 | `TestSnapshotEqualStartedAtSortsByPID` | **FIXED (0.9)** | Ascending-PID secondary sort added in `snapshotLocked`. Seam-critical: the positional `focus` selector is now deterministic. |
| 3 | **PID-vs-index `focus` selector collision** | §5.2 | `TestPickSession` | **FIXED (1.5)** | `pickSession` now accepts explicit `pid:<n>` / `idx:<n>` selectors (unambiguous), while the bare-number form keeps the documented PID-first-then-index heuristic for back-compat. The characterization for the bare form stays as a guard; new cases pin the prefix behavior. `switchboard-ctl focus` help lists the prefix forms. |
| 4 | **`Reconcile` keeps a stale WM address on ambiguity** | §3.5 | `TestMatchUniqueClient` (returns nil on ambiguity) | **PRESERVE** | Intentional "retry next tick": an ambiguous `(pid,title)` match returns nil rather than guessing, leaving the prior address until a later tick disambiguates. Now documented; the `internal/wm` seam keeps this contract. |
| 5 | **`Snapshot` shares pointer fields** (Wezterm/Hyprland/Claude) | §4.2 | — (documented invariant) | **PRESERVE** | Read-only by convention; a consumer mutating through a snapshot would race the store. Acceptable while all consumers are read-only. Revisit (deep-copy or value types) only if a future backend needs to mutate a snapshot — flagged for the Phase 1 seam review. |
| 6 | **`HyprlandInfo.Monitor` never populated** | §6.3 | (schema doc marks it reserved) | **FIX @ Phase 2** (deferred from 1.3) | `hyprctl clients` reports `monitor` as an integer index, but the schema field is a monitor *name*; populating it correctly needs an index→name resolution (`hyprctl monitors`) that the neutral `wm.Window` does not yet carry. Deferred to Phase 2, when the WM seam grows monitor metadata across backends. Still cosmetic (always `""`); schema doc flags it reserved. |
| 7 | **`ClaudeInfo.Status` `unknown` never emitted** | §5.1 | `TestStatusFromHookEvent` (asserts never `"unknown"`) | **FIXED (1.5)** | The `ClaudeInfo.Status` doc-comment no longer lists the unreachable `unknown` value (`working\|idle\|permission`). Documentation-only; no runtime change. The schema doc still tells consumers to tolerate unrecognized values defensively. |
| 8 | **`decodeCWD("file://host")` returns the host** | §3.1 | `terminal.TestDecodeCWD` (host-no-path case) | **FIXED (1.2)** | `decodeCWD` moved into the terminal seam (`internal/terminal`, which owns the wezterm `file://` URL) and now returns `""` when the path component is empty, instead of leaking the hostname as a path. The characterization test moved with it and was flipped to the intended behavior. |
| 9 | **`ClaudeInfo.SessionID` write-once** | §5.4 | `handleHook` (set-if-empty) | **PRESERVE** | Intentional: the first hook carrying a session id wins and is never overwritten, so a late/duplicate hook can't clobber it. Documented in the schema. |
| 10 | **Corrupt-JSON `Load` restores no sessions** | §4.5 | `TestLoadCorruptReturnsErrorAndHydratesNothing` | **PRESERVE** | On a corrupt mirror, `Load` returns an error and hydrates nothing; the daemon logs and rebuilds from the live `/proc` scan. The mirror is a cache, not the source of truth, so dropping it is safe. (Possible later nicety: back up the corrupt file for debugging — not required.) |
| 11 | **`procwatch` POLLERR-without-POLLIN can spin** | §9 | (documented gap in `osproc/source_linux_test.go`) | **FIXED (1.1)** | The pidfd poll loop (now in the `internal/osproc` Linux backend, having absorbed `procwatch`) fires `onDeath` on `POLLIN\|POLLERR\|POLLHUP\|POLLNVAL`, so a `POLLERR`/`POLLHUP` without `POLLIN` is treated as death instead of spinning. Hard to trigger from a test (pidfd delivers `POLLIN` on exit), so covered by inspection. |
| 12 | **`Scanner` shadows a recycled PID without `Forget`** | §2.2 | `TestScannerRecycledPIDShadowedWithoutForget`, `TestSessionLifecycleHazards` | **PRESERVE (rationale amended 2026-07-22)** | Correct by design, but the original rationale — "the seen-set relies on `procwatch` calling `Forget` on death, which *always* happens … the death-watch contract prevents [a missed death]" — was **too strong**. The pidfd watch lives in daemon memory, so a restart or SIGKILL orphans it and that death is never observed; a `Watch` that fails to register never observes one either. Both were real: they stranded three lanes as multi-hour ghosts on 2026-07-22 (`session-lifecycle-hazards.md`). The dependency is now backstopped rather than assumed — `sweepDeadSessions` polls liveness every reconcile tick and routes through `endSession`, which records `session_end` **and** calls `Forget`, so the seen-set is repaired within one tick no matter how the watch was lost. |

## Status vs the plan's DoD

The plan's §0.9 DoD ("each ⚠ item has a pin commit + a fix commit before Phase 1
extracts that seam") is adjusted per the agreed scope: **every item has a pin
commit** (the characterization tests landed in 0.3–0.8), and the **seam-critical
fix (#2) had its fix commit in Phase 0**. The remaining fixes are scheduled
against the Phase-1 task that extracts each seam, and the PRESERVE items keep
their pin as a permanent guard.

**Phase 1 status (worked through seam-by-seam):**

- **FIXED in Phase 1:** #8 (1.2 — terminal seam owns cwd decoding), #1 (1.3 — wm
  seam owns address normalization), #11 (1.1 — POLLERR/HUP/NVAL treated as
  death), #3 (1.5 — `pid:`/`idx:` selector prefixes), #7 (1.5 — `Status` doc
  comment). Together with #2 (Phase 0), every actionable bug surfaced by the
  Phase-0 study is now resolved.
- **Deferred:** #6 (Monitor) re-scoped from 1.3 to **Phase 2** — correct
  population needs monitor index→name resolution the neutral `wm.Window` does
  not yet carry.
- **PRESERVE (unchanged guards):** #4, #5, #9, #10, #12 — intended behavior,
  pins remain as permanent regression guards.

## Memory sampling — the lock budget, measured (2026-08-03)

Per-session memory sampling reads `/proc` for every live session on every
reconcile tick. The question that decided its shape was not "is this fast
enough?" but **"where does the time land?"** — `Store.Apply` holds an exclusive
write lock across its whole callback *and* the `snapshotLocked()` that follows,
so a millisecond spent inside it is a millisecond every RPC reader (waybar,
claude-tui, `ctl`) and every inbound hook spends blocked. The rest of the tick's
per-session work is microseconds; this would not have been.

Measured on this machine (`SWITCHBOARD_LIVE_MEMORY=1 go test ./cmd/switchboard
-run LiveCost -v`), 4 live claude sessions throughout, in two regimes that turned
out to differ by far more than expected:

| | quiet machine | under memory pressure |
|---|---|---|
| conditions | 400 procs, PSI avg10 0.63, 4.5 GB avail | 434–443 procs, PSI avg10 7.7–8.9, 2.7–3.1 GB avail |
| `ParentMap` (the process-table scan) | 4.5 ms | 10.3 – 16.5 ms |
| **full sampling pass, outside the lock** | **14.0 ms** | **54.5 – 96.5 ms** |
| per-session `TreeMemory` (the shape *not* used) | 24.8 ms | 121 – 140 ms |
| **added lock-hold, inside `store.Apply`** | **833 ns** | **491 – 837 ns** |

**The sampling cost is not stable — it degrades ~7× under memory pressure**,
which is precisely when the reading is most wanted. `smaps_rollup` is answered by
walking the target's VMA list, so it gets slower exactly as the system starts
thrashing. The quiet-machine figures match the estimates this work was planned
against (4–6 ms / 10–20 ms); the loaded figures are 3–5× them. Anything sizing
this feature should use the loaded column. The lock-hold, by contrast, is
**invariant** across both regimes — it does no I/O — which is the point.

Three things follow.

**The structure is the whole result.** Sampling runs before the lock is taken,
against the pid set of the last published snapshot; only a map lookup, two field
assignments, and a non-blocking `sink.Record` happen inside `Apply`. That moves
milliseconds per session out of the critical section and leaves sub-microsecond
behind — the added lock-hold is four to five orders of magnitude smaller than the
work it schedules. Had this followed the `proc.State` precedent of reading inside
the loop, a 5-second tick would have held the exclusive write lock for 14 ms at
rest and up to a tenth of a second under pressure, blocking every bar repaint,
every `ctl` call, and every inbound hook for the duration.

**One `ParentMap` per tick, not per session.** The scan is roughly a third of a
pass, so paying it per session is most of the bill: the naive shape measured
24.8 ms against 14.0 ms quiet, and 121–140 ms against 54.5–96.5 ms loaded. It is
also more *correct* — every process is attributed to exactly one tree even if the
kernel reparents it mid-tick.

**Full PSS was affordable; the RSS fallback was NOT needed.** The interview's
plan B (PSS for the agent, cheap RSS for the tree walk) is unnecessary and was
not implemented. `smaps_rollup` costs ~0.25 ms per process at rest and
~1.1–2.0 ms under pressure, and it scales with the *target's* VMA count rather
than with the size of the machine — a 3.5 GB 30-process tree dominates a
1-process 190 MB one — but all of it is outside the lock, so the accurate measure
is the one that ships. PSS is what makes the figures summable across sessions
without double-counting shared pages, which is the property the whole surface
rests on; trading it away to save time in a section that is not contended would
have been a bad trade. Keeping `SwapPss` separate earned its keep too: a live
session was observed at 486 MB PSS with a further 192 MB in swap, which an
RSS-only reading would have reported as simply gone.

The unlocked cost is still not free — ~0.3% of one core at rest and ~1–2% under
pressure, at a 5 s tick. If that ever needs to come down, the lever is sampling
**cadence** (every Nth tick), not precision: the per-read cost is the kernel's
VMA walk and cannot be reduced without giving up PSS.

### Retention follows the volume

A sample per live session per tick is ~12× the line volume of everything else
the log records — ~11.8 MB/day against the 1.0 MB actually written on the busiest
recorded day (2026-07-22: 66 lanes, 54.8 live session-hours). Retention is
size-bounded *before* it is age-bounded (`pruneDir` trims oldest-first on total
size, and `retain_days` only rules on what survives), so the old 100 MB default
would have quietly turned a configured 90-day retention into about ten days,
with nothing logged and nothing to notice. That corpus is load-bearing — the
suspect caps in `suspect.go` were calibrated by replaying a month of it — so the
default cap now scales with what is being recorded: 2 GB with memory sampling on,
100 MB without, an explicitly configured `max_bytes` always winning either way.
The effective cap is logged at startup beside `history: enabled=…`, because a
multi-gigabyte disk commitment should never be made silently.
