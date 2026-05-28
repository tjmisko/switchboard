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
| 1 | **Address `0x`-prefix normalization** (highest risk) | §7.1, §13.2 | `conformance` normalization round-trip + `TestHyprlandManagerConformance`; `handleHyprlandEvent` | **FIX @ Phase 1.3** | Normalization is already centralized at the daemon event boundary (`"0x"+Data`) and pinned by the conformance round-trip. The *fix* is structural: the `internal/wm` seam (`Manager`) must own it so every backend yields already-comparable refs. No behavior change needed now — only relocation when the seam is created. |
| 2 | **`Snapshot` `StartedAt` sort tie-break** | §4.2 | `TestSnapshotEqualStartedAtSortsByPID` | **FIXED (0.9)** | Ascending-PID secondary sort added in `snapshotLocked`. Seam-critical: the positional `focus` selector is now deterministic. |
| 3 | **PID-vs-index `focus` selector collision** | §5.2 | `TestPickSession` | **FIX @ Phase 1.5** | Selector `"2"` means PID 2 if present, else index 2 — same input, different session by state. Resolve when `rpc.focus` is reworked for graceful Navigate degradation (1.5): prefer an explicit `pid:`/`idx:` prefix, keep the bare-number heuristic for back-compat. Low harm today (interactive use). |
| 4 | **`Reconcile` keeps a stale WM address on ambiguity** | §3.5 | `TestMatchUniqueClient` (returns nil on ambiguity) | **PRESERVE** | Intentional "retry next tick": an ambiguous `(pid,title)` match returns nil rather than guessing, leaving the prior address until a later tick disambiguates. Now documented; the `internal/wm` seam keeps this contract. |
| 5 | **`Snapshot` shares pointer fields** (Wezterm/Hyprland/Claude) | §4.2 | — (documented invariant) | **PRESERVE** | Read-only by convention; a consumer mutating through a snapshot would race the store. Acceptable while all consumers are read-only. Revisit (deep-copy or value types) only if a future backend needs to mutate a snapshot — flagged for the Phase 1 seam review. |
| 6 | **`HyprlandInfo.Monitor` never populated** | §6.3 | (schema doc marks it reserved) | **FIX @ Phase 1.3 / 2** | Declared but never written (always `""`). Populate from `hyprctl clients` `monitor` field when the `internal/wm` seam maps clients, or generalize into the neutral window block. Cosmetic until then; schema doc flags it reserved. |
| 7 | **`ClaudeInfo.Status` `unknown` never emitted** | §5.1 | `TestStatusFromHookEvent` (asserts never `"unknown"`) | **FIX @ Phase 1 (doc)** | The doc-comment lists `unknown` but it is unreachable. Fix is documentation-only: drop `unknown` from the comment (the schema doc already lists the real set and tells consumers to tolerate unknown values). No runtime change. |
| 8 | **`decodeCWD("file://host")` returns the host** | §3.1 | `TestDecodeCWD` (host-no-path case) | **FIX @ Phase 1.2** | A `file://host` URL with no path decodes the hostname as if it were a path. Harmless today (wezterm emits `file:///…` with an empty host), but wrong. Correct when the terminal seam (`internal/terminal`) owns cwd decoding: return `""` when the path component is empty. |
| 9 | **`ClaudeInfo.SessionID` write-once** | §5.4 | `handleHook` (set-if-empty) | **PRESERVE** | Intentional: the first hook carrying a session id wins and is never overwritten, so a late/duplicate hook can't clobber it. Documented in the schema. |
| 10 | **Corrupt-JSON `Load` restores no sessions** | §4.5 | `TestLoadCorruptReturnsErrorAndHydratesNothing` | **PRESERVE** | On a corrupt mirror, `Load` returns an error and hydrates nothing; the daemon logs and rebuilds from the live `/proc` scan. The mirror is a cache, not the source of truth, so dropping it is safe. (Possible later nicety: back up the corrupt file for debugging — not required.) |
| 11 | **`procwatch` POLLERR-without-POLLIN can spin** | §9 | (documented gap in `procwatch_test.go`) | **FIX @ Phase 1.1** | The poll loop only fires `onDeath` on `POLLIN`; a `POLLERR`/`POLLHUP` revent without `POLLIN` is ignored and the loop re-polls. Correct when the `internal/osproc` Linux backend is built: treat `POLLERR`/`POLLHUP`/`POLLNVAL` as death (or bail) too. Not observed in practice (pidfd delivers `POLLIN` on exit). |
| 12 | **`Scanner` shadows a recycled PID without `Forget`** | §2.2 | `TestScannerRecycledPIDShadowedWithoutForget` | **PRESERVE** | Correct by design: the seen-set relies on `procwatch` calling `Forget` on death, which always happens (death observed via pidfd, exactly once). A recycled PID is only shadowed if a death was missed — which the death-watch contract prevents. Documented dependency between the two packages. |

## Status vs the plan's DoD

The plan's §0.9 DoD ("each ⚠ item has a pin commit + a fix commit before Phase 1
extracts that seam") is adjusted per the agreed scope: **every item has a pin
commit** (the characterization tests landed in 0.3–0.8), and the **seam-critical
fix (#2) has its fix commit now**. The remaining fixes are scheduled against the
Phase-1 task that extracts each seam (the test flips in that commit), and the
PRESERVE items keep their pin as a permanent guard. This register is the
checklist Phase 1 works through seam-by-seam.
