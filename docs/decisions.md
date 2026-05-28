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
| 12 | **`Scanner` shadows a recycled PID without `Forget`** | §2.2 | `TestScannerRecycledPIDShadowedWithoutForget` | **PRESERVE** | Correct by design: the seen-set relies on `procwatch` calling `Forget` on death, which always happens (death observed via pidfd, exactly once). A recycled PID is only shadowed if a death was missed — which the death-watch contract prevents. Documented dependency between the two packages. |

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
