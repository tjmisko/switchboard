# Phase 1 — Stop the publish storm

Working order for the six tasks of `docs/memory-footprint-plan.md` Phase 1,
written for an executor with no prior context. The plan document is the
specification; these files are the design and the operating procedure on top of
it.

| file | covers |
|---|---|
| `README.md` (this file) | orientation, environment, measurement, sequencing |
| `01-task-4.5-hook-provenance.md` | #4.5 — stop the hook overlay relabelling the graph |
| `02-tasks-5-6-7-change-key.md` | #5 title normalizer, #6 change key, #7 freshness lease |
| `03-task-8-federation-encode.md` | #8 — one encode per publish, change-gated |
| `04-task-9-title-rate-limit.md` | #9 — separate rate limit for `windowtitlev2` |
| `05-corrections-to-the-plan.md` | errors found in `memory-footprint-plan.md` itself |

---

## Read these five first

Each is a trap a careful reader still walks into.

1. **#4.5 and #6 are complementary; neither reaches the target alone.** #4.5 ≈
   5.7/s, #6 ≈ 2.27/s, together 0.15/s. `memory-footprint-plan.md` says
   otherwise in two places and is wrong — see `05-corrections`.
2. **Deploy with `scripts/deploy`.** It stages one immutable release, flips the
   `current` symlink, restarts, and verifies the running revision from `/proc`.
   Manual copies into `~/go/bin` or `~/.config/switchboard/bin` are obsolete.
3. **Other sessions deploy to this machine.** One restarted the daemon during
   planning with a different branch. Verify the running build is yours before
   quoting any number.
4. **In #5, normalize `WindowTitle` only — never `PaneRef.Title`.** H9's
   stuck-chip recovery reads `Title`'s first rune; stripping it silently
   disables the rule.
5. **#10 needs task #8.5's renderer-owned clock refresh.** Waybar has no render
   timer, but current `main` coarsens its hover ages to minute resolution. Use
   adaptive display-boundary timers, not a fixed 1 Hz Waybar poll.

---

## Context — why this work exists

The original full-cgroup observation was ~410 MB across the related units. The
optimization target is narrower and actionable: Switchboard-attributable
anonymous memory, excluding GTK Waybar, under 120 MB with Codex and 60 MB
without. Full `MemoryCurrent` remains a reported secondary metric. The daemon's
*live heap* is only 5–10 MB, so this is not a leak: it is allocation **churn**.
A Codex TUI rotates a braille spinner in its pane title; that title
reaches the publish change key; every rotation republishes a ~19 KB JSON
snapshot to 11 subscribers and rewrites `state.json`. Measured 2026-08-26:
**6.9–11.5 publishes/s, ~1.4 MB/s of JSON**, for a machine where nothing
semantically changed.

The causal chain, verified end to end:

```
Codex TUI rotates a braille spinner in its pane title
  → Hyprland windowtitlev2
  → wm.EventLayoutChanged (indistinguishable from openwindow/movewindowv2 today)
  → drainWMEvents, 200 ms rate limit                cmd/switchboard/main.go:591
  → reresolveAll → forks `wezterm cli list` + `hyprctl clients`     main.go:730
  → title differs → SnapshotChangeKey includes it   internal/state/state.go:670
  → Store.broadcast + persist(state.json)                          state.go:826
  → federation.View.publish() — NO change gate   internal/federation/view.go:320
  → 11 subscribers each rebuild AND re-encode the aggregate      rpc.go:490-491
```

Separately, and the larger effect: a Codex hook arrives and
`overlayCodexHookObservation` (`cmd/switchboard/agent_observation.go:582`)
relabels the whole app-server graph — `Source`, `Complete`, and a `FreshUntil`
imported from a **600 s** hook horizon onto a graph a live app-server reconfirms
every **15 s**. `fresh_until` then oscillates between two values ten minutes
apart, which no bucket can collapse. `codex_hook_transitions.go:270` schedules an
app-server re-observation after every hook, closing the loop.

### What Phase 0 already gives you

| | |
|---|---|
| `scripts/sb-mem-baseline` | One-shot footprint + churn report. Human table + one JSON line. Read its header before using it. |
| `publish-stats` journal line | Daemon-internal publish/suppress counters, once a minute. Documented in `docs/telemetry.md`. |
| `cmd/switchboard/publish_suppression_invariant_test.go` | **The guard.** History recording must stay independent of publish suppression. Must be green at every commit. |
| `docs/memory-footprint-plan.md` §1.4 | The measured baseline, the ablation, and the amendments. |

### The hard invariant

`switchboard-dashboard` reads history day-files **only** (via `switchboard-ctl
timeline --json`), never the socket and never `state.json`. Nothing in Phase 1
may change what lands in those files — except #4.5, which fixes what lands
there. Issue #83 makes Codex child `agent_state` history a production invariant.

---

## Measured facts you must not re-derive

Ablation over the real 231-frame capture, 230 consecutive pairs, 19.854 s span:

| change key | rate |
|---|---|
| today | 10.63/s |
| + title spinner normalize | 5.74/s |
| + drop `observed_at`, `fresh_until` 5 s bucket (**#6 as written**) | 4.73/s |
| + drop node `updated_at` | 2.97/s |
| + `fresh_until` 30 s bucket | 2.47/s |
| + drop `fresh_until` entirely (bucket asymptote) | 2.27/s |
| + drop `source`/`complete` (**#4.5 and #6 together**) | **0.15/s** |

**Isolated, so the complementarity is unambiguous:**

| rule set | rate |
|---|---|
| **#4.5 alone** — flap fixed, `fresh_until` still exactly compared | **5.74/s** |
| **#6 alone** — `fresh_until` neutralized, flap still present | **2.27/s** |
| **both** | **0.15/s** |

With the flap fixed but `fresh_until` still in the key exactly, it still advances
**once per second** per active Codex session (`DefaultActiveResnapshot = 1s`,
`internal/provider/codex/observer.go:21`). With `fresh_until` quantized but the
flap present, the ten-minute oscillation defeats any bucket width.

**Three reasons the ablation is optimistic** (all push the true figure up):

1. The captured frames are `federation.View` frames, not `state.Store` frames —
   `View.publish()` has no change gate, and 19 of 231 frames were byte-identical
   to their predecessor.
2. One of five sessions is remote; #6 cannot gate it. Goosebook-only gives
   4.18/s for #6. **#8 is load-bearing.**
3. `View.Subscribe` coalesces with a 4-deep drop-oldest channel.

**Measurement is noisy.** The publish rate spans 4.7–11.5/s and daemon RSS spans
35.9–47.0 MB on an *unchanged* build. Baseline of record is the 56-sample
sawtooth (daemon `RssAnon` min 27.2 / **mean 34.1** / max 42.7 MiB, sd 4.3) plus
the `publish-stats` counters, which integrate over a minute instead of sampling.

**The publish rate does NOT scale with session count** — it scales with
*actively spinning Codex panes*. Two of five sessions produced 92% of the churn,
and the union is strongly sublinear (308 summed, 212 together). Record a census
with every capture so workload changes are visible; never use it as a
normalizer.

### A trap in #10's DoD

#10 says "publish rate < 0.5/s" and `publish-stats` reports `publishes`. **These
are different quantities.** `sb-mem-baseline`'s rate is the **wire** rate from a
`subscribe-all` capture, which on any box comes from `federation.View` (no
change gate). `publish-stats` counts **`Store.Apply` decisions**. The < 0.5/s
figure and the whole ablation are stated against the **wire** rate — judge #10 on
that, and use `publish-stats` as the lower-variance corroborating signal and to
prove `suppressed ≫ publishes`. `docs/telemetry.md`'s thresholds for `publishes`
are flagged uncalibrated; you will produce the first real calibration.

---

## The environment

### You do not have this machine to yourself

The original planning session observed another worktree replacing the running
daemon mid-measurement. The current deploy model prevents ambiguous in-place
copies, but another session can still deploy a different immutable revision.
Always verify the release before quoting a number.

```sh
scripts/deploy --status
pid=$(systemctl --user show switchboard -p MainPID --value)
readlink /proc/$pid/exe
/proc/$pid/exe -version
journalctl --user -u switchboard -g 'Started switchboard' --since today
```

A different running revision means the control is no longer comparable.
`MemoryPeak` resets on every restart, including someone else's, so always pair
it with the running revision and unit start time.

### Deploy one verified release

```sh
scripts/deploy --status
scripts/deploy
scripts/deploy --status
```

The deploy builds all commands from one revision, stages them under
`~/.local/share/switchboard/releases/`, atomically flips `current`, restarts the
units, verifies the running daemon, and rolls back on failure. Use
`--allow-dirty` only for an explicitly recorded measurement build. Do not
manually restart one binary from a mixed revision.

`switchboard-waybar.service` runs `switchboard-ctl bottombar watch`, which spawns
the 10 `switchboard-waybar --slot N` processes. `switchboard-dashboard` invokes
`switchboard-ctl timeline` as a short-lived child per request, so a new ctl takes
effect there without a restart. There is a backup convention in that directory
(`.switchboard-backup-<date>-<tag>/`) — make one before the first deploy.

---

## Measurement protocol

**A single capture is a sample, not a level.** Take your own control; do not
compare against §1.4's numbers, which are from a build that is no longer running.

1. Deploy the recovered Phase 0 HEAD **unchanged** (behaviour matches
   `main` apart from the telemetry line). Restart. Confirm it is your build.
2. Capture the control: **≥3** `sb-mem-baseline` runs at the 15 s default a few
   minutes apart, plus **≥10 minutes** of `publish-stats`. Record the census —
   you need at least one, ideally two, live Codex sessions actually working.
3. Implement, deploy, restart, capture the same set under a comparable census.
4. Report before/after as ranges with the census, never two point values.

```sh
scripts/sb-mem-baseline | tail -n 1 >> ~/mem-baseline.jsonl
journalctl --user -u switchboard -g 'publish-stats' --since -15m --no-pager
```

`sb-mem-baseline` exits 2 and sets `capture_status` to `no_frames` or
`truncated` when a capture is broken. **Never quote a rate without checking
`capture_status` is `ok`** — a daemon restart mid-capture yields `truncated`.

### Reproducing the ablation

Needed at GATE 1 and GATE 2. The original scratchpad artifacts are gone; this is
self-contained.

```sh
{ printf '{"cmd":"subscribe-all"}\n'; sleep 20; } \
  | timeout 25 ncat -U "$XDG_RUNTIME_DIR/switchboard.sock" > capture.jsonl
wc -l capture.jsonl     # ~200+ with Codex spinning; 1 means the capture broke
```

`printf ... | ncat -U sock` **without** the `sleep` captures exactly ONE frame
and exits 0 — ncat closes on stdin EOF and the daemon treats that as a
disconnect. It looks like a successful capture of a perfectly idle daemon.

Then flatten each frame to leaf paths, apply a candidate rule, count consecutive
pairs that differ, and divide by the **measured** span (first and last frame's
`updated_at`), not a nominal 20 s. Python is fine for analysis — it just must not
end up in `scripts/`.

Two things to get right, both of which caught errors in Phase 0:

- **"Differs from the previous frame" is exact, not an approximation** of the
  daemon's "differs from the last *published* key". After every `Apply`,
  `publishedKey` equals the key of the frame just processed, so by induction the
  two are identical. The only case that breaks it is `invalidatePublished` after
  a failed persist.
- **Count cells and pairs separately.** A field can move in many
  `(pair, session)` slots within one pair; only the **pairs** count is comparable
  to a publish rate. Conflating them overstated the title lever ~2x in a draft.

---

## Working agreement

**Branch.** Work in a worktree under `.worktrees/`, never on `main`. Branch Phase
1 work from `feat/mem-phase0` — #4.5's tests depend on Phase 0's guard.

**Commits.** Conventional (`feat:`/`fix:`/`test:`/`docs:`/`refactor:`), one
logical change each, tests in the same commit as the code they cover. No emoji.
End with `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.

**When a measurement contradicts the plan — stop and report.** Do not adjust the
target to match what you got, and do not quietly amend a DoD. Phase 0 did this
three times and it produced task #4.5 and issues #91/#92. Specifically expect to
stop if: the post-#4.5 re-measure does not collapse the `fresh_until`
oscillation; #6 cannot reach the target at any sane bucket width; or
`internal/conformance` does not go green after #5.

**The guard.** `cmd/switchboard/publish_suppression_invariant_test.go` must be
green at every commit. If it goes red you have broken the dashboard's data feed —
stop; do not "fix the test".

**Do not touch.** Issues #91 and #92 (focus bugs) are filed with full evidence
and are out of scope. The bench fixtures under `~/.local/state/switchboard` are
issue #89's. Phase 2+ is `[FUTURE]`.

---

## Sequence and review gates

```
control measurement → #4.5 → [GATE 1] → #5 #6+#7 #8 #8.5 → [GATE 2] → #9 → [GATE 3] → #10
```

### PR 1 — #4.5

The critical path and the only Phase-1 change that touches history content. It
does **not** reach the target alone — expect ~5.7/s. Its job is to remove the
ten-minute `fresh_until` oscillation, the floor no bucket in #6 can collapse.

**GATE 1:**
1. `go test -race ./...` green (except `internal/conformance`, red until #5).
2. Deploy, restart, re-run the ablation on a **fresh capture**: `source`,
   `complete` and `fresh_until` must stop oscillating. **This is what everything
   downstream is calibrated against.**
3. No **new** Codex child `agent_state` row carries `source: hook`:
   ```sh
   jq -r 'select(.type=="agent_state" and .agent=="codex" and (.parent_thread_id//"")!="" and .source=="hook") | .ts' \
     ~/.local/state/switchboard/history/$(date +%F).jsonl | tail
   ```
   Count rows *after* your deploy timestamp; pre-existing rows stay.
4. Adversarial review of the diff.
5. **Report the re-measured numbers before starting PR 2.** #6's bucket width is
   chosen from them, not from the pre-#4.5 table.

Judge this gate on **the oscillation being gone**, not on the rate.

### Stacked rollout — #5 + #6/#7 + #8 + #8.5

Review these as small stacked units but deploy them together for Gate 2. #6
needs #5's normalizer, **#6 without #7 is a correctness regression**, #8 must
reuse #6's key, and #8.5 preserves renderer clocks after suppression.

**GATE 2:**
1. `go test -race ./...` **fully green, `internal/conformance` included** — that
   suite going green with Codex spinning is #5's acceptance test.
2. Wire rate re-measured (≥3 captures, live Codex, census recorded).
3. Bar behaviour by eye: chips, tooltips, `idle · Nm` counters, stale marking,
   remote chips. **See the predicted failure below.**
4. Adversarial review.

### PR 3 — #9

**GATE 3:** `wezterm cli list` forks ≤ 1/s with two spinning Codex panes,
measured. Tests use the injected debounce. Adversarial review.

### #10 — Phase 1 verification

See "Definition of done" below.

---

## Renderer clock dependency — implemented at `0e531b8`

#10 requires "'idle · Nm' counters unchanged by eye". **That will not hold, and
it is a consequence of #6 working correctly.**

Verified: `cmd/switchboard-waybar/main.go` contains **no timer of any kind** —
the loop is a blocking `Recv` → `emit`. `renderSlot` calls `time.Now()`
internally (`:229`), so every `now.Sub(at)` counter — the chip's `idle · Nm` and
the tooltip's age rows — advances **only when a frame arrives**. Today the
`observed_at`/`fresh_until` churn this phase removes is what keeps them ticking.
After #6 a quiet Claude session publishes nothing and its counter **freezes
indefinitely**. `cmd/claude-tui` has the same shape.

Task #8.5 implements the fix **client-side, not as a daemon heartbeat**. Waybar
uses `durfmt.Coarse` and schedules its next visible boundary instead of polling
at 1 Hz. The TUI schedules second boundaries only while `durfmt.Compact`
displays seconds, then coarsens. Both retain the latest snapshot, so timer edges
perform no socket read; existing output dedupe remains the last guard against
unnecessary writes.

---

## Definition of done for the phase

- Wire publish rate **< 0.5/s** with two Codex sessions spinning, from ≥3
  captures with `capture_status: ok` at a recorded census.
- Daemon `RssAnon` **mean < 25 MB** over a 10-minute sampled window (not a point
  reading — the pre-fix spread was 27.2–42.7 MiB).
- `publish-stats` showing `suppressed ≫ publishes`; update `docs/telemetry.md`'s
  thresholds with this first real calibration.
- `state.json` mtime advancing < 1/s.
- `go test -race ./...` green including `internal/conformance`.
- `publish_suppression_invariant_test.go` green and **unmodified**.
- No new Codex child `agent_state` rows carrying `source: hook`.
- Numbers recorded in `memory-footprint-plan.md` §5; issue #80 updated with
  whether #5 closed it.

---

## Follow-ups to file (found during planning, not in scope)

1. **Separate agent-graph provenance from hook-composition routing** — `Source`
   does both jobs; #4.5 works around it with a `Diagnostic` marker. The clean fix
   is an unexported `composed bool` on `state.AgentGraph` (precedent: `provider
   agentgraph.ProviderKind`, `internal/state/agent_graph.go:25`).
2. **Note on #83** — #4.5 fixes requirement 3's producer; requirement 5's
   `Forget`-driven re-emission is untouched. Include the corrected evidence: at
   most 24 twins collapse in the reader, **at least 9 survive**.
3. **The cross-process clock** in `shouldApplyObservation` — hook `ObservedAt` is
   stamped in `switchboard-ctl`, the app-server's in the daemon. Same host today
   (unix socket), so exposure is an NTP step; #4.5 narrows rather than widens it.
4. **`remotestate.cloneSnapshotMap`** JSON round-trip per host per call — #8 takes
   it from `1+N` per publish to 1. Do not also rewrite the clone;
   `routes.go:61` only reads keys and never needs a detached copy.
5. **Why `nlessfun` has no attachable Codex app-server** — the 600 s hold there is
   the *designed* fallback and contributes zero churn.
6. **Waybar/tui counters freeze between publishes** — see above. File before PR 2.
7. **`AgentGraph.FreshnessClass()`** — `cmd/switchboard-ctl/diagnose.go:220-227`
   reimplements freshness inline with a finer four-way split.
