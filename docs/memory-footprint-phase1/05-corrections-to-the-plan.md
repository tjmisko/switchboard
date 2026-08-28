# Corrections to `docs/memory-footprint-plan.md`

Errors and stale anchors found while designing Phase 1. Several were written by
the same session that wrote §1.4, so they are self-corrections. Apply them as you
go — an executor trusting the uncorrected text will edit the wrong lines or aim
at the wrong number.

---

## 1. ⚠ "#4.5 alone plausibly clears #10's DoD" — WRONG

**Where:** §1.4.2's last ablation row is labelled "i.e. after #4.5", and the
§1.4.3 prose plus task #4.5's Verify note say "#4.5 alone plausibly clears #10's
DoD and no bucket width does".

**The second half is right; the first half is wrong.** The 0.15/s row is
cumulative — it already has #6's rules applied. Isolated on the same 231-frame
capture:

| rule set | rate |
|---|---|
| **#4.5 alone** — flap fixed, `fresh_until` still exactly compared | **5.74/s** |
| **#6 alone** — `fresh_until` neutralized, flap still present | **2.27/s** |
| **both** | **0.15/s** |

**They are complementary; neither is sufficient.** With the flap fixed but
`fresh_until` still in the key exactly, it still advances **once per second** per
active Codex session (`DefaultActiveResnapshot = 1s`,
`internal/provider/codex/observer.go:21`), so the key keeps moving. With
`fresh_until` quantized but the flap present, the ten-minute oscillation defeats
any bucket width.

**Fix:** relabel the row "#4.5 **and** #6 together", replace the prose with the
three-row table, and change #4.5's Verify note to expect **~5.7/s** and to judge
the gate on "the oscillation is gone", not on the rate.

---

## 2. §1.2 — the View is NOT only in the path with `-remote`

**Where:** §1.2, "With `-remote` configured, `rpc.subscribeAll` goes through
`federation.View`".

**Verified wrong.** `federation.NewView` is constructed unconditionally
(`cmd/switchboard/federation.go:80`, no `remoteFlags` guard) and installed
unconditionally (`federation.go:108` `server.SetFederation(f.view, …)`, called
from `main.go:261`). **Every waybar slot and the bottom bar go through
`View.publish()` on every box.**

**Why it matters:** #6 gates `Store.Apply`; #8 gates `View.publish()`; and the
wire rate #10 measures is the View's output. The View is suppressed indirectly
when the store stops broadcasting, but it also publishes on remote updates, focus
transitions, and navigator `Refresh` (`federation.go:129`,
`navigator.go:58,164,182`) — all ungated. **So #6 alone cannot reach #10's DoD,
and #8 is load-bearing rather than a tidy-up.** This retroactively explains the
ablation result that goosebook-only sessions give 4.18/s for #6.

---

## 3. §1.4.4 — the focus-bug evidence conflated two patterns

**Where:** §1.4.4 as originally written quoted "301 A→B→A alternations inside
6 s" as evidence for the `applyFocus` map-pick bug.

That number mixed two mechanisms. Split correctly:

| pattern | total | gap < 1 s |
|---|---|---|
| between **two real session ids** (issue #91) | 177 | **84** |
| involving **NULL** (`session_id: ""`) (issue #92) | 353 | 77 |

Only the first row is evidence for #91. The NULL row is a **separate bug** —
`applyFocus` should emit exactly one event when focus leaves to a non-agent
window, because the next tick has `prevID == newID == ""` and returns early. 77
sub-second NULL alternations mean `activeAddr` itself is flapping, which points
at the WM query.

This correction is already applied in §1.4.4. Both bugs are filed (#91, #92) and
are **out of scope** for Phase 1.

---

## 4. §1.4.3 — the nlessfun framing

**Where:** §1.4.3 calls the remote `nlessfun` session "the same defect in its
permanently degraded form".

A 600 s hold with no attachable app-server is the **designed** fallback
(`cmd/switchboard/agent_observation.go:22-30`), which #4.5's DoD explicitly
preserves. That session contributes **zero** churn (0 transitions, 0 round trips
in the capture), and #4.5 cannot reach it — the graph is produced by nlessfun's
own daemon, and that host shows `app_server 0 / hook 231`, so the preserve branch
could never fire there anyway.

**Fix:** reframe as "the designed no-app-server fallback, contributing no churn",
and file a separate issue asking why nlessfun has no attachable Codex app-server.

---

## 5. §1.4.3 / #4.5 — "all 33 rows reach the dashboard" is too strong

`cmd/switchboard-ctl/timeline.go` dedupes on `canonicalTimelineEdge{session,
thread, pid, at, from/to × runtime/attention/lifecycle}` — excluding `source` but
**including** the `from_*` axes. Twins agreeing on `from_*` are collapsed by the
reader itself.

**At most 24 collapse; at least 9 survive** (7 disagreeing on `from_*` + the 2
hook-only rows). That makes the 24/9 grouping the operative one — as §1.4.3
already suspected — and the surviving 9 are exactly the rows that fabricate
transitions the user never made.

---

## 6. §1.4.2 — "drop `fresh_until` entirely" is not a reachable target

The asymptote row (2.27/s) is listed as though it were an option. **It is not:**
dropping `fresh_until` from the key breaks the soundness proof in
`02-tasks-5-6-7-change-key.md` §"The soundness argument" — the key must encode
the same quantized value the wire carries, or suppression makes consumers falsely
stale. It is a bound on what bucketing can achieve, not a design.

Add a line saying so, so nobody reads it as a target.

---

## 7. Stale line anchors

| plan says | actual |
|---|---|
| `drainWMEvents` at `main.go:580` | `cmd/switchboard/main.go:591` |
| `snapshotChangeKey` at `state.go:656` | Exported as `state.SnapshotChangeKey` by task #6; do not rely on a line anchor. |
| `Fresh` at `agent_graph.go:80` | `internal/state/agent_graph.go:79` (`:80` is the nil guard) |
| `rpc.subscribeAll` at `rpc.go:467` | function starts `:462`; `:467` is the `Subscribe()` call |
| `View.publish()` at `view.go:320` | correct **in this worktree**; on `main` it is `:324` |
| `overlayCodexHookObservation` at `:585` | `cmd/switchboard/agent_observation.go:582` |

---

## 8. §1.4.5 — conformance runs gated by default

The note should say the suite normally runs with its live assertions **gated
off**; `SWITCHBOARD_LIVE_CONFORMANCE=1` opens them. Re-run with the gate open and
the only failures are still the same title-only disagreements — but a reader who
does not know about the gate will think a green run proves more than it does.

Also: `conformance.go:430` compares `conformance.Pane` (`Mux`, `PaneID`, `TTY`,
`WindowTitle` — `conformance.go:293-298`), **not** the whole `terminal.PaneRef`.
It does not carry `Title`, which is why #5 can leave `Title` raw.

---

## 9. Task #6's DoD — node `updated_at` amendment

The written DoD says node-level `updated_at` stays in the key. The measured data
says it is pure clock churn (73 of 230 pairs, no sibling field moving on that
node in 72 of 73) costing ~1.76 publishes/s. **Record the amendment as adopted:
drop it**, with the poll-stamp reasoning — for Codex it is a poll stamp, not a
transition stamp, and it re-enters the key indirectly the moment a real axis
moves. `started_at` and `completed_at` stay.

---

## 10. Task #7 — the mechanism changed after an owner decision

The plan's #7 says only that `fresh_until` becomes the bucket ceiling on
published frames. The design work surfaced that there are two ways to do this,
with materially different consequences, and **the owner deferred the in-memory
one on 2026-08-26**. Record the decision and the chosen mechanism (encode-time
`MarshalJSON`), its two costs, and the deferred alternative — all written up in
`02-tasks-5-6-7-change-key.md` §"#7".

---

## 11. Task #10's DoD — two known problems

1. **"publish rate < 0.5/s" is ambiguous.** `sb-mem-baseline` measures the
   **wire** rate (View output, no gate); `publish-stats` counts **`Store.Apply`
   decisions**. The ablation and the target are stated against the wire rate.
   Say which, explicitly.
2. **"'idle · Nm' counters unchanged by eye" will fail, and that means #6
   worked.** `cmd/switchboard-waybar/main.go` has no timer, so counters advance
   only when a frame arrives. Current `main` now renders Waybar ages through
   `durfmt.Coarse`, which changes at most once per minute. Task #8.5 therefore
   uses adaptive display-boundary timers: minute boundaries for Waybar and
   second boundaries only while the TUI visibly displays seconds. A fixed 1 Hz
   Waybar ticker is stale advice and would add avoidable client churn. The fix
   remains client-side, never a daemon heartbeat.

---

## 12. `docs/telemetry.md` — `publishes` thresholds are uncalibrated

Already flagged in that file during Phase 0, repeated here because #10 is where
it gets fixed: the "~560/min baseline, Phase 1 targets < 30/min" figures were
derived from the **wire** rate, which counts View publishes rather than
`Store.Apply` decisions. #10 produces the first real calibration for that
counter — update the table then.

---

## 13. Recovery onto current `main` (2026-08-28)

The salvaged Phase 0 branch was originally based before PRs #90, #93, and #94.
It has now been transplanted onto current `main`. Three old operating assumptions
are invalid:

1. Deploy only with `scripts/deploy`; manual copies into `~/go/bin` or
   `~/.config/switchboard/bin` bypass the immutable-release verification model.
2. `publish-stats store_subscribers` counts in-process `state.Store`
   subscribers. Waybar clients subscribe through the unconditional federation
   View and do not increase it. `sb-mem-baseline.socket_connections` is the
   client-fan-out metric.
3. The 120/60 MB goal is Switchboard-attributable **anonymous** memory excluding
   the GTK Waybar process. Full cgroup `MemoryCurrent` remains reported but
   cannot satisfy those thresholds because it includes GTK, file-backed charges,
   and other fixed components.

Phase 2 is now renderer consolidation and Phase 3 is lazy Codex startup: the
renderer saves memory continuously and is required for the with-Codex target,
whereas lazy startup saves memory only after Codex has been absent for its grace
period and carries the higher history risk.

The FIFO spike must not keep an unread `O_RDWR` descriptor. That design can fill
the pipe while Waybar is absent and block the single renderer. Use reconnecting
nonblocking writers with one cached latest line per slot, and prove prolonged
reader absence cannot block another slot.
