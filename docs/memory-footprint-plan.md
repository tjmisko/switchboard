# Memory Footprint Reduction — orchestration plan

> **Goal.** Cut Switchboard-attributable anonymous resident memory from roughly
> 160 MB to under 120 MB with Codex running and under 60 MB without. The primary
> total is `switchboard.service` cgroup anon + `switchboard-waybar.service` anon
> **excluding the GTK Waybar process** + `switchboard-dashboard.service` anon.
> GTK Waybar, file-backed cgroup charges, and out-of-scope services are reported
> separately; they are not silently folded into this target. Full
> `MemoryCurrent`, per-process RSS, and exclusions remain in every capture. The
> original ~410 MB observation was full cgroup-charged memory and is not
> comparable to the 120/60 MB target. The daemon's live heap is already only
> ~5–10 MB; the work removes allocation churn rather than retained state.
>
> **Non-goal.** Deleting the bench fixtures under `~/.local/state/switchboard`
> (issue #89). That is disk, not RAM, and #89 wants a full-day `MemoryPeak`
> reading first. Do not touch those directories from this plan.
>
> **Hard invariant.** Nothing in this plan may change what lands in the history
> day-files. `switchboard-dashboard` reads *only* those files (via
> `switchboard-ctl timeline --json`), and issue #83 makes Codex child
> `agent_state` history a production invariant. Task #3 guards this with a test
> that must stay green through every phase.

> **Phase 1 executors: start at `docs/memory-footprint-phase1/README.md`.**
> That directory holds the per-task designs, the operating procedure (deploy
> paths, measurement protocol, review gates), and — importantly —
> `05-corrections-to-the-plan.md`, which lists the places THIS document is wrong
> or has stale line anchors. Several corrections are already applied inline
> below; the rest are listed there.

> **Recovery status (2026-08-28).** Phase 0 was transplanted onto current
> `main` after PR #94. The recovered branch uses the immutable release deploy
> (`scripts/deploy`) and distinguishes in-process Store subscribers from socket
> connections. Phase 1 remains blocked until the recovered baseline is merged,
> deployed, and re-measured on the current renderer and remote-state code.

---

## 1. Baseline (2026-08-26 16:20–16:25, goosebook: Asahi arm64, 16K pages, Go 1.26.5, 15.2 GiB RAM)

### 1.1 Where the bytes are

| Unit / process | RSS | Anon | Notes |
|---|---|---|---|
| `switchboard.service` cgroup | 149 MB | 62 MB | `MemoryPeak` 172 MB since 16:15 restart |
| ↳ `switchboard` daemon | 33–50 MB sawtooth | 42 MB | `heap_alloc_mb=5` right after seed; rest is GC headroom |
| ↳ `codex app-server --stdio` (daemon child) | 103 MB | 33 MB | spawned **eagerly** in `NewObserver` regardless of Codex roots |
| ↳ `ssh … remote-stream` | 9 MB | — | federation to `nlessfun` |
| `switchboard-waybar.service` cgroup | 136 MB | 100 MB | |
| ↳ 10 × `switchboard-waybar --slot N` | 12 MB each | 7.6 MB each | Go hello-world floor on this box: 2.8 MB RSS / 1.2 MB anon |
| ↳ `waybar -c claude.jsonc` (GTK) | 47 MB | — | not ours |
| `switchboard-dashboard.service` cgroup | 75 MB | 9.5 MB | peak 1.09 GB earlier today = pre-scrub `ctl timeline` child; today's run is 34 MB / 0.5 s — already fixed |
| `switchboard-ctl bottombar watch` | 12 MB | — | an 11th always-on subscriber |
| `arachne-switchboard-recorder.service` | 38 MB cgroup / 6 MB RSS | — | out of scope |

System pressure at the time (9.5 GiB used, 8.3 GiB swap) was Firefox (~2 GB),
`claude` (422 MB), wezterm (397 MB), two `codex resume` (490 MB), Obsidian —
not switchboard. This plan is about switchboard's own hygiene.

### 1.2 The churn (root cause)

A 15 s `subscribe-all` capture: **141 frames = 9.4 publishes/s, 13.7 KB
each, to 11 subscribers** (≈1.4 MB/s of JSON). Diffing consecutive frames,
the only fields that change:

- `sessions[i].wezterm.window_title` on the two Codex sessions — the Codex TUI
  rotates a braille spinner (`⠹ ⠴ ⠧ ⠋ ⠸ …`) in the pane title
- `sessions[i].agent_graph.observed_at` / `fresh_until` — timestamps that
  advance on every observation with no semantic change
- `updated_at` (already excluded from the change key)

Chain: spinner tick → Hyprland `windowtitlev2` → `reresolveAll`
(`cmd/switchboard/main.go:550`, rate-limited to 200 ms, each one forks
`wezterm cli list` + `hyprctl clients`) → title differs →
`snapshotChangeKey` (`internal/state/state.go:656`) includes the title →
`broadcast` + `persist(state.json)`.

`rpc.subscribeAll` (`internal/rpc/rpc.go:462`) goes through `federation.View`,
and **every subscriber rebuilds the aggregate and re-encodes it itself**
(`s.view.Snapshot()` + `enc.Encode` per wakeup) — 11 deep copies + 11 encodes
per publish, ~100/s total. `View.publish()` (`internal/federation/view.go:320`)
has **no change gate at all**, so this churn fans out ungated.

**Correction (verified 2026-08-26): this is NOT conditional on `-remote`.** An
earlier draft of this paragraph said "with `-remote` configured". The View is
constructed unconditionally (`cmd/switchboard/federation.go:80`, no
`remoteFlags` guard) and installed unconditionally (`federation.go:108`, called
from `main.go:261`), so **every waybar slot and the bottom bar go through
`View.publish()` on every box.** That is why #6 alone cannot reach #10's DoD:
#6 gates `Store.Apply`, #8 gates `View.publish()`, and the wire rate #10
measures is the View's output. The View is suppressed indirectly when the store
stops broadcasting, but it also publishes on remote updates, focus transitions
and navigator `Refresh` (`federation.go:129`, `navigator.go:58,164,182`) — all
ungated. **#8 is load-bearing, not a tidy-up.**

Each waybar slot decodes 14 KB ~10×/s → the ~4.8 MB heap arena per slot.

### 1.3 Measured levers

| Experiment | Result |
|---|---|
| `switchboard-waybar --slot 0`, default | 10.5 MB RSS / 6.4 MB anon |
| `GOGC=25` | 8.6 MB / 4.5 MB |
| `GOMAXPROCS=1 GOGC=25` | 7.9 MB / 3.7 MB |
| `switchboard-ctl timeline --json --plan-window --day 2026-08-26` | 34 MB peak, 0.54 s |

### 1.4 Baseline capture (task #4)

Captured with `scripts/sb-mem-baseline` (task #1) on the build running at the
time — `~/.config/switchboard/bin/switchboard -remote nlessfun`, pid 1097019,
started 16:15:51, so `MemoryPeak` is since that restart and not since boot.

**Census.** 5 live sessions — 2 Claude, 3 Codex — of which 2 Codex panes were
actively spinning; bottom bar up; 11 socket subscribers (10 `switchboard-waybar
--slot N` + `bottombar watch`). Host: goosebook, Asahi arm64, 16K pages,
Go 1.26.5, 15.2 GiB RAM.

**The publish rate does NOT scale with the live-session count** — it is
dominated by how many Codex panes are *actively spinning*; total session count
is a weak proxy. Per-session attribution over this capture (publishes each
session would generate alone, today's key): `[0] claude 10, [1] claude 8,
[2] remote codex 12, [3] codex 153, [4] codex 125`. Two of five sessions produce
92% of the churn, and the union is strongly sublinear (308 summed, 212 together).
Record the census anyway — it is cheap and it is the only way to notice a
workload change between runs — but as "what workload was this", not as a
normalizer.

Raw JSON line, `sb-mem-baseline` default 15 s capture at
**2026-08-26T16:51:05-07:00**:

```json
{"timestamp":"2026-08-26T16:51:05-07:00","host":"goosebook","socket":"/run/user/1000/switchboard.sock","subscribers":11,"units":{"switchboard.service":{"state":"active","memory_current_bytes":123535360,"memory_peak_bytes":208715776,"anon_bytes":72253440,"file_bytes":40173568},"switchboard-waybar.service":{"state":"active","memory_current_bytes":138739712,"memory_peak_bytes":167804928,"anon_bytes":97632256,"file_bytes":21987328},"switchboard-dashboard.service":{"state":"active","memory_current_bytes":73351168,"memory_peak_bytes":1091878912,"anon_bytes":8568832,"file_bytes":58327040}},"processes":{"daemon":{"pid":1097019,"rss_bytes":49299456,"rss_anon_bytes":43794432},"codex_app_server":{"pid":1097049,"rss_bytes":56737792,"rss_anon_bytes":28606464},"remote_stream":{"pid":1097033,"rss_bytes":6979584,"rss_anon_bytes":409600},"waybar_slot_0":{"pid":1407043,"rss_bytes":12337152,"rss_anon_bytes":7929856},"waybar_slot_1":{"pid":1407045,"rss_bytes":11829248,"rss_anon_bytes":7503872},"waybar_slot_2":{"pid":1407047,"rss_bytes":12484608,"rss_anon_bytes":8077312},"waybar_slot_3":{"pid":1407049,"rss_bytes":12320768,"rss_anon_bytes":7913472},"waybar_slot_4":{"pid":1407051,"rss_bytes":12173312,"rss_anon_bytes":7831552},"waybar_slot_5":{"pid":1407057,"rss_bytes":12320768,"rss_anon_bytes":7979008},"waybar_slot_6":{"pid":1407069,"rss_bytes":12107776,"rss_anon_bytes":7766016},"waybar_slot_7":{"pid":1407081,"rss_bytes":12025856,"rss_anon_bytes":7700480},"waybar_slot_8":{"pid":1407088,"rss_bytes":12042240,"rss_anon_bytes":7700480},"waybar_slot_9":{"pid":1407092,"rss_bytes":12058624,"rss_anon_bytes":7716864},"waybar_gtk":{"pid":1407017,"rss_bytes":51970048,"rss_anon_bytes":11911168},"bottombar_watch":{"pid":1098140,"rss_bytes":12402688,"rss_anon_bytes":7487488}},"capture":{"duration_s":15.002,"frames":105,"publishes":104,"publish_rate_hz":6.932,"mean_frame_bytes":19121,"total_frame_bytes":2007725}}
```

**§1.1 confirmed.** Every figure in §1.1 reproduces within its stated range. The
one correction: the `codex app-server` child measured 54–57 MB RSS / 27–31 MB
anon here, not the 103 MB / 33 MB in §1.1 — that process grows with Codex
session count and conversation size, so treat §1.1's figure as an upper
observation rather than a steady state.

**Daemon sawtooth, 56 samples at 10 s, 16:48:42–16:57:52** (a single `VmRSS`
reading is not a baseline — the daemon oscillates by 15 MB between GCs). MiB,
all n=56:

| | min | mean | max | median | sd |
|---|---|---|---|---|---|
| daemon `VmRSS` | 32.4 | **39.3** | 47.9 | 38.9 | 4.3 |
| daemon `RssAnon` | 27.2 | **34.1** | 42.7 | 33.7 | 4.3 |
| `switchboard.service` cgroup anon | 51.5 | **63.3** | 74.7 | 62.4 | 5.3 |
| `switchboard-waybar.service` cgroup anon | 80.7 | **90.1** | 93.9 | 91.9 | 4.1 |

Three caveats on this table, all of which matter for the "after" comparison:

- **This is a BUSY-case baseline, not an idle one.** The window is the middle of
  the multi-agent Phase-0 investigation, with 3–5 subagents live and two captures
  in flight. Phase 1's "after" reading must be taken under comparable load or it
  is measuring the workload, not the fix.
- **A regime change sits inside the window.** `switchboard-waybar` cgroup anon
  drops from ~94 to ~85 MiB at 16:55:52 and stays there, with no unit restart in
  the journal. The mean above averages across two regimes.
- **min/max of 56 draws are inward-biased** estimators of the true trough and
  peak: the real sawtooth almost certainly exceeds 42.7 and dips below 27.2. The
  mean is unbiased (Go's GC is allocation-paced, so a fixed 10 s probe has no
  phase to alias against).

Note for #19: the criterion there is "daemon `RssAnon` after Phase 1 still
> 25 MB". Pre-Phase-1 mean is 34.1 and *minimum* is 27.2, so apply it to the
**mean over a sampled window**, never to a single reading.

**Churn.** Independent captures, all at 11 subscribers, same 5-session census:

| capture | window | publishes/s | mean frame |
|---|---|---|---|
| raw `ncat` capture, 16:45:13 | 19.854 s | 11.58 | 18.3 KiB |
| `sb-mem-baseline`, 16:51:05 | 15 s | 6.93 | 18.7 KiB |
| `sb-mem-baseline`, 16:57 | 3 s | 4.66 | 21.1 KiB |
| `sb-mem-baseline`, 17:00:08 | 15 s | 7.93 | 19.8 KiB |

(All frame sizes here are binary KiB, as `sb-mem-baseline` prints them. The
first row is 230 publishes over the measured 19.854 s span, not over a nominal
20 s.)

**A single capture is a sample, not a level.** The rate spans 4.7–11.5/s on an
unchanged build, tracking how hard the Codex TUIs happen to be spinning. To
claim an effect, use the daemon's own `publish-stats` counters (#2), which
integrate over a minute rather than sampling, cross-checked with ≥3 captures.
#10's < 0.5/s DoD is an order of magnitude below this spread and so survives it;
#19's `RssAnon` criterion does not, which is why it must use the sampled mean.

Frame size is stable at ~18.7 KB (§1.2's 13.7 KB was at a smaller session
count). At 6.9/s this is 130 KB/s encoded per subscriber, ~1.4 MB/s across 11.

#### 1.4.1 Frame-diff — what actually moves between consecutive frames

230 consecutive pairs from the 16:45 capture. **Two denominators, and they are
not interchangeable:** `cells` counts every (pair, session) or (pair, session,
node) slot that moved; `pairs` counts how many of the 230 transitions the field
moved in *at all*. Only `pairs` is comparable to the publish counts in §1.4.2,
because one publish is one pair no matter how many cells changed in it.

| cells | pairs /230 | path | nature |
|---|---|---|---|
| 230 | 230 | `snapshot.updated_at` | clock, already dropped from the change key |
| 190 | **99** | `sessions[i].wezterm.window_title` | Codex braille spinner |
| 111 | 111 | `sessions[i].agent_graph.fresh_until` | clock **and see §1.4.3** |
| 111 | 111 | `sessions[i].agent_graph.observed_at` | clock |
| 73 | 71 | `sessions[i].agent_graph.nodes[i].updated_at` | clock, per node |
| 42 | 42 | `sessions[i].agent_graph.source` | **provenance flap, §1.4.3** |
| 42 | 42 | `sessions[i].agent_graph.complete` | **provenance flap, §1.4.3** |
| 6 | 6 | `focused`, node `runtime`/`lifecycle`, `summary.*` | genuine changes |

`fresh_until` and `observed_at` move in exactly the same 111 cells (set
equality, empty symmetric difference) — they are stamped together, and there is
no transition where the freshness horizon moved without a new observation.

#### 1.4.2 Ablation — what each planned lever actually buys

The planned change-key rules replayed over the real 231-frame capture. Counts
are key *changes* over the 230 consecutive pairs; rates divide by the measured
span, **19.854 s** (from the first and last frame's `updated_at`), not a nominal
20 s. Independently re-derived from scratch by a second reviewer: every count
matched exactly.

| change key | changes | rate | suppressed |
|---|---|---|---|
| today (drops `updated_at` only) | 211 | 10.63/s | 8.3% |
| + #5/#6 title spinner normalize | 114 | 5.74/s | 50.4% |
| + drop `agent_graph.observed_at` | 114 | 5.74/s | 50.4% |
| + #6 `fresh_until` 5 s bucket — **plan's Phase 1 as written** | **94** | **4.73/s** | 59.1% |
| + drop node `updated_at` (not in the plan) | 59 | 2.97/s | 74.3% |
| + `fresh_until` 15 s bucket | 52 | 2.67/s | 77.4% |
| + `fresh_until` 30 s bucket | 49 | 2.47/s | 78.7% |
| + drop `fresh_until` entirely (bucket asymptote) | 45 | 2.27/s | 80.5% |
| + also drop `agent_graph.source` | 45 | 2.27/s | 80.5% |
| + also drop `agent_graph.complete` — **i.e. #4.5 AND #6 together** | **3** | **0.15/s** | 98.7% |

Note the rows are **cumulative**. The `fresh_until`-dropped row is a bound on
what bucketing can achieve, not a reachable design: dropping `fresh_until` from
the key breaks the suppression's soundness (the key must encode the same
quantized value the wire carries, or a consumer that receives no frame goes
falsely stale). See `docs/memory-footprint-phase1/02-tasks-5-6-7-change-key.md`.

**Task #10's DoD is < 0.5/s. Phase 1 as specified reaches 4.73/s, and widening
the bucket asymptotes at 2.27/s — it never gets close.** Three conclusions:

- **#4.5 and #6 are complementary; NEITHER reaches the target alone.** Isolated
  on the same capture: **#4.5 alone 5.74/s** (flap fixed, `fresh_until` still
  compared exactly), **#6 alone 2.27/s** (`fresh_until` neutralized, flap
  present), **both 0.15/s**. With the flap fixed but `fresh_until` still in the
  key, it advances once per second per active Codex session
  (`DefaultActiveResnapshot = 1s`, `internal/provider/codex/observer.go:21`);
  with `fresh_until` quantized but the flap present, the ten-minute oscillation
  defeats any bucket width. `complete` accounts for essentially the whole flap
  residual — `source` contributes nothing once `complete` is gone.
  **#4.5's job is to remove a floor no bucket can collapse**, which is the
  precondition for #6 to work. That is the justification for the reordering.
- **#6 must also normalize node-level `updated_at`.** The plan says to keep it
  in the key. The data says it is pure clock churn — it moves in 73 of 230 pairs
  with no sibling field on that node moving — and costs ~1.76 publishes/s.
- **The ablation is optimistic, which strengthens the conclusion.** Three
  reasons, all pushing the true figure up: (1) these are `federation.View`
  frames, not `state.Store` frames — the daemon ran `-remote`, and
  `View.publish()` (`internal/federation/view.go:320`) has no change gate at all,
  so 19 of the 231 frames are byte-identical to their predecessor under today's
  key and could not have come from the Store gate; the "today" row therefore
  measures View, not `snapshotChangeKey`. (2) Session `[2]` is remote, and #6
  cannot gate it — restricting to goosebook sessions gives 83 changes (4.18/s)
  for Phase 1, so **#8 is load-bearing, not cosmetic.** (3) `View.Subscribe`
  coalesces with a 4-deep drop-oldest channel (`view.go:340`), so frames lost
  there remove potential key changes.

Method note: "differs from the immediately preceding frame" is *exact*, not an
approximation of the daemon's "differs from the last published key". After every
`Apply`, `s.publishedKey` equals the key of the frame just processed — either it
differed and `adoptPublishedLocked` assigned it, or it matched and was already
equal — so by induction the two are identical. Both were implemented; they agree
on every variant. The only case that breaks the induction is the
`invalidatePublished` retraction after a failed persist, which did not fire here.

#### 1.4.3 The `agent_graph` provenance flap — blocks Phase 1's DoD

Two producers write the same Codex session's graph and alternate frame to frame
with different clock conventions *and* different freshness horizons:

```
source=codex_app_server  observed_at 16:45:13-07:00  fresh_until 16:45:28-07:00   15 s horizon, LOCAL tz
source=hook              observed_at 23:45:16Z       fresh_until 23:55:16Z       600 s horizon, UTC
```

Same 31 nodes in both — the only per-node difference is the root's `updated_at`.
Horizons measured at exactly 15.000 s and 600.000 s with zero variance, matching
`codex.DefaultFreshness` (`internal/provider/codex/observer.go:20`) and
`codexHookActiveFreshness` (`cmd/switchboard/agent_observation.go:27`).

Flip counts over the 231-frame capture, per session:

| session | source mix | one-way transitions | round trips |
|---|---|---|---|
| `[3]` goosebook/codex/20805 | app_server 143 / hook 88 | 26 | 13 |
| `[4]` goosebook/codex/23137 | app_server 191 / hook 40 | 16 | 8 |
| `[2]` **nlessfun**/codex/310315 | **hook 231 / app_server 0** | 0 | 0 |

The split is 62/38 and 83/17, not the clean 50/50 the shape suggests.
`|Δfresh_until|` reaches 587 s on both, so `fresh_until` oscillates between two
values **~10 minutes apart**, which no bucket width can collapse — that is the
2.27/s asymptote.

**The remote `nlessfun` session is pinned at `source=hook` with the 600 s
horizon for all 231 frames.** The same defect is live there in its permanently
degraded form rather than flapping: that chip holds "working" for up to 10
minutes after Codex stops, with nothing to correct it.

Mechanism, all in `cmd/switchboard/agent_observation.go:589-593`
(`overlayCodexHookObservation`): a Codex hook produces a *single-node* root
observation (`codex_hook_transitions.go:1176`), which is inflated back to the
app-server's full graph and relabelled wholesale —

```go
overlay.Source     = agentgraph.SourceHook   // :590  unguarded
overlay.FreshUntil = hook.FreshUntil         // :592  imports the 600 s horizon
overlay.Complete   = false                   // :593  unguarded
```

The alternation is not a race: `shouldApplyObservation` returns at
`agent_observation.go:565` on the `ObservedAt` comparison, *before* `sourceRank`
(`:568`, app_server 4 > hook 3) is ever read. Both producers stamp their own
`now`, so the ranks never apply and last-writer-by-wall-clock wins;
`codex_hook_transitions.go:270` then schedules an app-server re-observation
after every hook, closing the loop.

The 600 s horizon is itself deliberate (`agent_observation.go:22-30`) but scoped
to the *no-attachable-app-server* case. Applying it to a graph a live
app-server re-confirms every second is wrong by ~40x, and it means a chip holds
"working" for up to 10 minutes after Codex dies mid-turn instead of greying in
15 s (`agentgraph.Reduce`, `internal/agentgraph/reduce.go:9`).

**This also violates issue #83 in production data.** `:590`/`:593` stamp
`source: hook` onto app-server-derived children. Today's day-file
(`~/.local/state/switchboard/history/2026-08-26.jsonl`, 538 `agent_state` rows):

```
codex hook child rows                                              33
  with a contradictory codex_app_server twin
  (same session_id, thread_id, ts, to_runtime, to_lifecycle)       31
  hook-only child rows (#83 requirement 3 forbids these)            2
```

Grouping sensitivity: on the `to_*` identity above it is 31 twinned / 2
orphaned; including the `from_*` axes it is **24 / 9** — seven more hook child
rows have an app-server row at the same instant that disagrees about the *prior*
state, which is arguably the more damning number. #83 requirement 3 is violated
either way. The two orphans are unambiguous: threads `01a0403b…` and `01a0402c…`,
both `parent_thread_id 01a03f8f…`, both `unknown→not_loaded`, at 23:01:38Z and
23:01:46Z — genuinely hook-manufactured child edges.

**These rows reach the dashboard.** `cmd/switchboard-ctl/timeline.go:306` filters
`agent_state` on `ev.ThreadID == "" || ev.ParentThreadID == ""` and **does not
filter on `source` at all**, so all 33 mislabelled rows are admitted into
`agent_timeline`. §2's phrasing ("child `agent_state` with `parent_thread_id`
*and* `source: codex_app_server`") describes the intended contract, not the
implemented filter. That makes #4.5 a dashboard-correctness fix, not only a churn
fix.

Separately, 89 same-source duplicate facts today on #83's requirement-5 identity
`(provider, root_id, thread_id, changed_axis, target_value, ts)` — the
projector's `seen` dedupe map is in-memory only and is cleared by `Forget`
(`internal/history/agent_state.go:142`). **The trigger is in-lifetime lane churn,
not process restarts:** 73 of the 89 fall inside the single 01:24:48→14:31:18
daemon lifetime, only 7 of 89 land within 120 s of any of the day's 8 starts
(median distance to the nearest start: 47 min), and the three lifetimes after
16:01 produced 106 `agent_state` rows and zero duplicates. #83 requirement 5 is
unmet in production, but whoever picks it up should look at `Forget` call sites,
not at startup replay.

**Consequence for this plan: one fix at `agent_observation.go:589-593` collapses
the source flap, the `complete` flap and the `fresh_until` oscillation, and
removes the #83 mislabelling mechanism. It must land before #6, or Phase 1
cannot meet task #10's DoD.** Filed as task #4.5 in §3.

Latent hazard, worth its own issue: the two `ObservedAt` values are stamped in
different *processes* — the hook in `switchboard-ctl`
(`cmd/switchboard-ctl/main.go:621`, UTC) and the app-server in the daemon
(`internal/provider/codex/observer.go:204`, local). `shouldApplyObservation`
therefore orders a client wall clock against a daemon wall clock. Correct on one
host today; invertible under an NTP step, a suspend/resume, or a remote `ctl`.

#### 1.4.4 Spurious `focus` events from a non-deterministic map pick

Found while extending the task #3 guard to cover `sink.Record` calls inside
`Store.Apply`. Not a memory issue — a history-data issue — but it is recorded
here because it was found by this work and because it inflates one of the event
classes §2 lists as a dashboard input.

`applyFocus` (`cmd/switchboard/main.go:1005`) decides whether focus changed by
comparing two picks taken from two independent `range` statements over the
session map:

```go
prevID := ""
for _, sess := range m {                 // range #1
    if sess.Focused { prevID = enrichmentID(sess); break }   // FIRST focused
}
newID := ""
for _, sess := range m {                 // range #2
    focused := activeAddr != "" && sess.Hyprland != nil && sess.Hyprland.Address == activeAddr
    sess.Focused = focused
    if focused { newID = enrichmentID(sess) }                // LAST focused
}
if newID == prevID { return }
sink.Record(...)                          // else: a focus event
```

Two agent sessions in one Hyprland window — two wezterm splits or tabs — share
an `Hyprland.Address`, so `applyFocus` marks BOTH `Focused`. `prevID` is then
the *first* focused session range #1 happens to reach and `newID` the *last*
range #2 happens to reach. Go randomizes map iteration per range statement, so
with two focused sessions the two picks disagree roughly half the time and a
`focus` event is recorded — while both sessions were already `Focused` and stay
`Focused`, so not one byte of the wire snapshot moves.

Measured over 40 real `reconcileOnce` ticks against a fixture with two sessions
on one address: **34 focus events, 0 broadcast frames, wire unchanged.**

Production evidence, stated carefully — an earlier draft of this section quoted
"301 A→B→A alternations inside 6 s" for this bug, and that number was wrong
because it conflated two different patterns. Today's day-file holds 6272 `focus`
events. Splitting the A→B→A alternations by what they alternate between:

| pattern | total | gap < 1 s |
|---|---|---|
| between **two real session ids** | 177 | **84** |
| involving **NULL** (`session_id: ""`, focus left to a non-agent window) | 353 | 77 |

Only the first row is evidence for THIS bug. The 84 sub-second two-session
alternations are the load-bearing figure: a person cannot alt-tab between two
agent windows and back inside one second, 84 times. The NULL row is a separate
question (see below) and must not be counted here.

The definitive proof of mechanism is the fixture measurement above, not the
day-file: production timestamps cannot show which sessions shared a window
address at the time, so the day-file can only corroborate.

**A second, unexplained pattern in the same data:** 77 sub-second alternations
between a real session and NULL. `applyFocus` should emit at most ONE event when
focus leaves to a non-agent window — the tick after, no session is `Focused`, so
`prevID` and `newID` are both `""` and it returns early. Repeated sub-second
NULL↔real alternation implies `activeAddr` itself is flapping between a match
and a non-match, which would be a WM-query problem rather than a map-pick one.
Not investigated. Worth its own issue.

`switchboard-dashboard` builds focus spans from these events, so spans for any
multi-pane window are contaminated by the first pattern, and spans for every
session may be fragmented by the second.

Fix is small — make range #1's pick deterministic, or compare the focused *set*
rather than a first-vs-last pick. Both loops should agree on what "the focused
session" means when more than one session matches the address, and that
question deserves an answer in `docs/state-schema.md` too: with two panes in one
window, which session is *the* focused one?

**Note for whoever fixes it:** `TestShouldRecordFocusFromInsideApplyWhenApplySuppressesThePublish`
depends on this bug to reach an in-Apply `sink.Record` on a suppressed tick.
Convert it to a `t.Skip` naming this section rather than deleting it; the
coverage it provides then falls back to
`TestShouldRunTheApplyClosureAndItsRecordsWhenTheChangeKeyIsUnchanged`, which
was written to survive exactly this.

#### 1.4.5 Pre-existing test failures on `main` (not caused by this work)

- `internal/conformance` — `TestWeztermLocatorConformance` and
  `TestAutoLocatorConformance` fail whenever a Codex session is running.
  `conformance.go:430` compares the whole `terminal.PaneRef` with `*got != want`,
  `WindowTitle` included, and the batch and single probes read the pane title at
  different instants, so a rotating spinner fails the comparison. **This is issue
  #80 from a third angle, and it bears on where task #5 puts the normalizer:**
  the plan wires it into `internal/mapping` and `internal/label` only, which
  leaves `internal/terminal` (`wezterm.go:79`, `tmux.go:106`) raw and the suite
  still flaky. Normalizing at the `internal/terminal` boundary instead would fix
  the change key, the mapping join, `label` and conformance in one place, at the
  cost of `window_title` going out stripped on the wire and in `state.json`
  (so `docs/state-schema.md` would need updating). Safe for the mapping join,
  which already normalizes both sides at compare time and is idempotent.
  Owner's call — recorded as an open decision on #5.
  Verified over 5 runs: **23 of 23 disagreement pairs are one leading spinner
  rune and nothing else** — `Mux`, `PaneID` and `TTY` are identical in all 23,
  and stripping one rune from mapping's table makes both titles equal in all 23.
  There is no second disagreement hiding behind the title mismatch. Note the
  suite normally runs with its live assertions GATED OFF
  (`SWITCHBOARD_LIVE_CONFORMANCE=1` opens them); re-run with the gate open and
  the only failures are still the same title-only disagreements.
- `internal/projectname` — `TestProjectRoot_noGitReturnsDirItself` fails because
  a stray empty `/tmp/.git` directory exists on this box (created 2026-08-26
  14:49), so `ProjectRoot` walks up and finds `/tmp` as a repo root. Purely
  environmental; `rmdir /tmp/.git` clears it.

Note also that `mapping.spinnerPrefixes` (`◐◑◒◓⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏✳⠂⠐⠁⠈⠠⠄⡀⢀`) is already a
strict superset of `label.spinnerPrefixes` (`✳ ⠂ ⠐ ⠁ ⠈ ⠠ ⠄ ⡀ ⢀`), so #5's shared
table is mapping's, and `label` *gains* coverage of the braille and circle
spinners it currently misses.

---

## 2. Dashboard data contract (what must not move)

`switchboard-dashboard` (`~/Projects/switchboard-dashboard`):

- Reads history **only** through `switchboard-ctl timeline --json --plan-window`
  (`~/.config/switchboard/providers.json`); `cmd/switchboard-ctl/timeline.go`
  never dials the socket or reads `state.json`.
- Second provider `arachne-switchboard-ctl` reads the arachne recorder's own log.
- SessionEnd hook reads `~/.local/share/switchboard/summaries`.
- Consumes: `session_start/end`, `transition`, `session_label`, `usage_sample`,
  `focus`, `activity`, `subagent_spawn/stop`, `workflow_*`, `suspend/resume`,
  and Codex child `agent_state` with `parent_thread_id` and
  `source: codex_app_server` (issue #83; `agent_timeline` is built only from
  those).
- Today's composition: 6686 `usage_sample`, 698 `focus`, 514 `agent_state`
  (296 `codex_app_server`, 137 `hook`), 187 `transition`, 29 `session_label`.

Producers: every `sink.Record` call is in the reconcile tick, a hook handler,
or the provider observation loop (`cmd/switchboard/main.go:209,302,920,981,1024`,
`fanout.go:102,122,152`, `agent_observation.go:397,521`). **None depend on
`Store.Apply` deciding to publish** — `Apply` always runs the mutation; only
`broadcast` and `persist` sit behind the change key (`state.go:554`).
`session_label` already strips spinner glyphs (`internal/label/label.go:32`).

Per-change verdict:

| Change | History impact |
|---|---|
| Change-key normalization (Phase 1) | none — wire only |
| Federation single-encode + gate (Phase 1) | none — wire only |
| Lazy Codex app-server (Phase 3) | **touches** first-snapshot latency and would lose child events if the child were down while a root is live → written as an invariant with tests (#11, #12) |
| Single renderer (Phase 2) | none |
| GOGC (Phase 4) | none |

---

## 3. Task graph

Conventions: `#N` IDs are unique across this file. A task is done only when it
is in `DONE.md` with a date. Phases are sequential; do not start a
`[FUTURE]` phase. Every code task ships its tests in the same commit
(conventional commits, one logical change each). Work in a worktree under
`.worktrees/`, never on `main`.

Verification commands used throughout:

```sh
systemctl --user show switchboard switchboard-waybar -p Id -p MemoryCurrent -p MemoryPeak
cat /sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/switchboard.service/memory.stat | head -3
grep -E 'VmRSS|RssAnon' /proc/$(systemctl --user show switchboard -p MainPID --value)/status
journalctl --user -u switchboard -g 'publish-stats|fanout-seed' --since -1h
```

### Phase 0: Measurement apparatus [RECOVERED — validation pending]

- [x] #1: `scripts/sb-mem-baseline` — one-shot footprint + churn report
  - **Prereqs**: none
  - **DoD**: Script prints (a) `MemoryCurrent/MemoryPeak` and `memory.stat`
    anon/file for `switchboard`, `switchboard-waybar`, `switchboard-dashboard`
    units; (b) `VmRSS`/`RssAnon` for the daemon, its `codex app-server` child,
    each `switchboard-waybar --slot` process, and `bottombar watch`; (c)
    publish rate and mean frame bytes from a 15 s `subscribe-all` capture on
    `$XDG_RUNTIME_DIR/switchboard.sock` (newline-delimited JSON; request is
    `{"cmd":"subscribe-all"}`). Output is one JSON line plus a human table.
    Runs with no arguments on this machine; exits non-zero if the socket is
    absent. Sibling of `scripts/sb-perf` / `scripts/sb-bench-seed` in style.
  - **Phase**: 0
  - **Notes**: The capture logic exists in the analysis scratchpad
    (`subrate.py`, `framediff.py`); port it to Go or POSIX sh — no Python
    dependency in `scripts/`. Bash rules: no `for` loops where `xargs`/`jq`
    will do; `>|` for overwrites.

- [x] #2: Daemon `publish-stats` telemetry line
  - **Prereqs**: none
  - **DoD**: Once per minute the daemon logs one line
    `publish-stats: publishes=<n/min> suppressed=<n/min> store_subscribers=<n>
    frame_bytes=<mean> heap_alloc_mb=<n> heap_sys_mb=<n> vm_hwm_mb=<n>`,
    same shape and placement as `fanout-seed`
    (`internal/fanout/seedtelemetry.go`). `suppressed` counts `Apply` calls
    whose change key matched. Unit test asserts the counters increment
    correctly across a publish and a suppressed publish. Line documented next
    to `fanout-seed` in the seed plan's telemetry section or a new
    `docs/telemetry.md`.
  - **Phase**: 0
  - **Notes**: Counters live on `state.Store` (`adoptPublishedLocked` is the
    one place that decides). Reuse `vmHWMKB` from `seedtelemetry.go` — move it
    to a small shared helper rather than copy it.

- [x] #3: Invariant test — history recording is independent of publish suppression
  - **Prereqs**: none — this is the guard every later phase runs against
  - **DoD**: A test in `cmd/switchboard` (or `internal/state` + a sink fake)
    named `should record history events when Apply suppresses the publish`:
    drive a reconcile/hook path that calls `sink.Record` while the snapshot's
    change key is unchanged, assert the sink received the event and the
    Store's broadcast channel received nothing. Green on `main` before any
    Phase 1 change lands.
  - **Phase**: 0
  - **Notes**: Cheapest seam is `agent_observation.go:397/521` (agent_state
    projection) or `main.go:920` (transition). Pick whichever has an existing
    test harness with an injectable sink.

- [x] #4: Record the baseline in this document
  - **Prereqs**: #1 (the script is the measurement) complete
  - **DoD**: §1 tables above replaced/confirmed by `sb-mem-baseline` output
    captured with two Codex sessions live and the bottom bar showing; the raw
    JSON line pasted under a `### 1.4 Baseline capture` heading with the
    timestamp.
  - **Phase**: 0

- [ ] #4.5: Stop the hook overlay from relabelling the whole Codex graph
  - **Prereqs**: #3 (the invariant guard must exist first — this is the one
    Phase-0/1 change that touches what lands in the history day-files)
  - **Why this is here and not behind #83**: measured in §1.4.3. The five lines
    at `cmd/switchboard/agent_observation.go:589-593` produce BOTH the
    `fresh_until` oscillation that pins the publish rate at a ~2.45/s floor
    (against #10's < 0.5/s DoD) AND the 33 mislabelled Codex child
    `agent_state` rows in today's day-file that violate issue #83
    requirements 3 and 5. One fix addresses both, so it lands here and gets
    noted on #83 rather than the reverse.
  - **DoD**: In `overlayCodexHookObservation`
    (`cmd/switchboard/agent_observation.go:585`), when the current graph is a
    fresh `codex_app_server` graph whose root the app-server actually knows,
    `Source`, `Complete` and `FreshUntil` are carried over from the app-server
    graph and only the root node's `Runtime`/`Attention`/`Lifecycle`/`UpdatedAt`
    are overlaid. Reuse the predicate that already exists —
    `codexAppServerRootUnavailable` (`cmd/switchboard/codex_hook_transitions.go:826`)
    — do not introduce a second one. When there is no app-server graph, the
    600 s `codexHookActiveFreshness` fallback (`agent_observation.go:27`) still
    applies unchanged, and the 24 h / 7 d attention and idle horizons are
    untouched. Tests:
    `should not change agent_graph source when a hook arrives for a live app-server graph`,
    `should not change agent_graph complete when a hook arrives for a live app-server graph`,
    `should keep the app-server freshness horizon when a hook arrives for a live app-server graph`,
    `should still apply the hook fallback horizon when there is no app-server graph`,
    `should still overlay the root node status when the app-server reports the root unavailable`,
    `should emit no child agent_state row carrying source hook after a Forget followed by a hook frame`.
    The last one is the #83 regression test and is the reason #3 is a prereq.
  - **Verify**: re-run the §1.4.2 ablation against a fresh capture and confirm
    `source`, `complete` and `fresh_until` stop oscillating. **Expect ~5.7/s, not
    0.15/s** — this task removes the floor, and #6 does the rest (§1.4.2).
    **Judge this gate on the oscillation being gone, not on the rate.** Only then
    is #6's bucket worth tuning, against these numbers rather than the pre-#4.5
    table. Note the fix is host-local and does NOT reach the remote `nlessfun`
    session — that graph is produced by nlessfun's own daemon, and that host
    shows `app_server 0 / hook 231`, so the preserve branch could never fire
    there. Its 600 s hold is the designed no-app-server fallback and contributes
    zero churn; see `docs/memory-footprint-phase1/01-task-4.5-hook-provenance.md` §8.
  - **Phase**: 0 (lands with Phase 0, ahead of #5/#6)
  - **Notes**: Does NOT fix #83's duplication half — the projector's `seen`
    dedupe map is in-memory and cleared by `Forget`
    (`internal/history/agent_state.go:142`). 89 same-source duplicate facts on
    2026-08-26, of which **73 fall inside a single daemon lifetime** — the
    trigger is in-lifetime lane churn, not process restarts, so look at the
    `Forget` call sites (`agent_observation.go:306,311,508,827`), not at startup
    replay. That stays a #83 sub-item. Also file a separate issue for the
    cross-process clock comparison in `shouldApplyObservation` (§1.4.3).


### Phase 1: Stop the publish storm [FUTURE — do not start until Phase 0 is in DONE.md]

- [ ] #5: Shared title normalization
  - **Prereqs**: none
  - **DECIDED 2026-08-26 (owner): option (b) — normalize at the
    `internal/terminal` boundary**, not only in the change key. §1.4.5 laid out
    (a) change-key-only versus (b) at the boundary; (b) is the one to build.
    This supersedes #6's "the wire snapshot is unchanged" sentence for the title
    specifically: `window_title` now goes out stripped.
  - **DoD**: One package exports the spinner glyph table and a
    `Normalize(title string) string` that strips one leading spinner rune plus
    surrounding whitespace. The table is `internal/mapping.spinnerPrefixes`
    (`◐◑◒◓⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏✳⠂⠐⠁⠈⠠⠄⡀⢀`) — verified a strict superset of
    `internal/label.spinnerPrefixes` (`✳ ⠂ ⠐ ⠁ ⠈ ⠠ ⠄ ⡀ ⢀`), so `label` gains
    coverage of the braille and circle spinners it currently misses.
    Applied where `terminal.PaneRef.WindowTitle` is CONSTRUCTED —
    `internal/terminal/wezterm.go:79` and `internal/terminal/tmux.go:106` — so
    every downstream consumer sees the normalized form.
    `internal/mapping.normalizeTitle` (`mapping.go:300`) and `internal/label`
    call the shared `Normalize`; their existing tests still pass.
    A table test covers every glyph, an empty title, a title that is only a
    glyph, a title whose first rune is a normal letter, and **idempotence**
    (`Normalize(Normalize(x)) == Normalize(x)`) — load-bearing, because the
    mapping join normalizes both sides at compare time and will now be
    normalizing an already-normalized pane title.
    `internal/conformance` (`TestWeztermLocatorConformance`,
    `TestAutoLocatorConformance`) goes GREEN with Codex sessions running — that
    is the acceptance test for this task, and it is currently red on `main`.
    `docs/state-schema.md` updated: `window_title` is the spinner-stripped
    title, and the raw title is not preserved anywhere on the wire.
  - **Phase**: 1
  - **Notes**: Issue #80 is the same race seen from the mapping side, and
    §1.4.5 is it seen from a third. Verified over 5 runs: 23 of 23 conformance
    disagreements are one leading spinner rune, with `Mux`, `PaneID` and `TTY`
    identical in all 23 — so (b) is sufficient to close them, and nothing else
    is hiding behind the title mismatch. Check whether #80 can be closed once
    this lands; if so, note it on the issue with the commit hash.
    Watch for consumers that WANT the raw title: `cmd/switchboard-ctl/main.go:154`
    prints it in `list`, and `internal/rpc/rpc.go:1433` builds a session
    description from it. Both read better stripped, but confirm rather than
    assume.

- [ ] #6: Normalize the publish change key
  - **Prereqs**: #5 (needs the shared normalizer), #3 (the invariant guard
    must exist before the gate gets stricter) complete
  - **DoD**: `snapshotChangeKey` (`internal/state/state.go:656`) is computed
    from a copy of the snapshot in which (a) every `Wezterm.WindowTitle` is
    passed through `Normalize`, (b) `AgentGraph.ObservedAt` is dropped and
    `AgentGraph.FreshUntil` is replaced by its value **rounded up** to a 5 s
    bucket. Node-level `updated_at`, `Summary.Since`, status, pending writers
    and everything else stay in the key. Tests:
    `should not republish when only the title spinner rotates`,
    `should not republish when only observed_at advances within a bucket`,
    `should republish when fresh_until crosses a bucket boundary`,
    `should republish when a node runtime state changes`,
    `should republish when a session's label or status changes`.
    Timestamps still go out raw on the frames that do publish. **The title does
    not** — #5 was decided as option (b), so `window_title` is already
    normalized at the `internal/terminal` boundary before it reaches the
    snapshot, and the change key inherits that rather than re-normalizing.
    Do NOT normalize the title a second time here.
  - **Phase**: 1
  - **Notes**: Keep the "fail open on encode error" behavior. The 5 s bucket
    means the bar can be at most 5 s behind on freshness; #7 makes that
    invisible.

- [ ] #7: Freshness lease on the wire
  - **DECIDED 2026-08-26 (owner): the IN-MEMORY policy change is DEFERRED.**
    Two mechanisms reach this DoD. Applying the ceiling in `ProjectAgentGraph`
    would make every provider's freshness horizon genuinely longer by up to the
    bucket width — one representation everywhere, no divergence, no encode
    overhead, but a real policy change. The owner is not comfortable with that
    yet, so #7 ships the **wire-only** mechanism (`AgentGraph.MarshalJSON`),
    which leaves in-memory `Fresh()` untouched at the cost of a nested encode per
    graph and a ≤1-bucket daemon/consumer divergence. Full write-up, including
    the round-trip asymmetry it introduces for remote and hydrated graphs, in
    `docs/memory-footprint-phase1/02-tasks-5-6-7-change-key.md` §"#7". Revisit
    the deferred alternative with the `heap_sys_mb` measurement taken after C2.
  - **NOTE**: #6 without #7 is a **correctness regression**, not merely an
    incomplete one — if the key quantizes `fresh_until` but the wire carries the
    raw value, suppression makes consumers falsely stale. They ship together.
  - **Prereqs**: #6 complete — only meaningful once republishes are suppressed
  - **DoD**: On the frames that are published, `agent_graph.fresh_until` is
    the bucket **ceiling** (never earlier than the true value), so a consumer
    evaluating `AgentGraph.Fresh(now)` (`internal/state/agent_graph.go:80`,
    used by `cmd/switchboard-waybar/main.go` `agentTooltip`) never reports
    stale while the daemon holds a fresh graph. Test in the waybar package:
    `should not mark the tooltip stale when the daemon suppressed a
    within-bucket republish` (simulate: publish at t, no publish until t+4.9 s,
    assert not stale; at t+5.1 s with no publish, stale is allowed).
    `docs/state-schema.md` notes the ceiling semantics.
  - **Phase**: 1

- [ ] #8: Federation View — one encode per publish, change-gated
  - **Prereqs**: #6 complete — the View must reuse the same normalized key,
    not invent a second one
  - **DoD**: `federation.View.publish()` (`internal/federation/view.go:320`)
    builds the aggregate once, computes the normalized change key, returns
    early when it matches the last published key, and hands subscribers a
    frame carrying the encoded bytes (mirror `state.Broadcast.JSON`).
    `rpc.subscribeAll` (`internal/rpc/rpc.go:467`) writes the shared bytes;
    the "treat the channel as a notification, re-read the latest" ordering
    guarantee is preserved by re-reading the latest *shared frame*, not by
    rebuilding per subscriber. Tests:
    `should encode the aggregate once per publish regardless of subscriber count`
    (count encodes with a counting encoder or by asserting identical backing
    slices), `should not fan out a remote frame that differs only by spinner
    or observed_at`, `should still deliver the final frame on remote
    disconnect` (the existing coalesce comment's guarantee), and the existing
    B→A ordering test stays green.
  - **Phase**: 1
  - **Notes**: The local-only path (`streamLocalSnapshots`) also re-snapshots
    per subscriber despite `Broadcast.JSON` existing; fix it the same way in
    the same commit.

- [ ] #8.5: Renderer-owned clock refresh
  - **Prereqs**: #6 and #7 complete — only needed once clock-only daemon
    publishes are suppressed
  - **DoD**: `switchboard-waybar` and `claude-tui` retain the latest snapshot
    and re-render when the next displayed duration can change, without a daemon
    heartbeat or another socket read. Waybar uses the next minute/coarse-format
    boundary (`durfmt.Coarse`); it must not poll at 1 Hz. The TUI may wake each
    second while `durfmt.Compact` is displaying seconds, then schedules the next
    minute/hour/day boundary. Existing byte/output dedupe remains authoritative.
    Tests use an injected clock/timer and assert that a quiet snapshot advances
    `idle · Nm`, that a Waybar render does not occur more than once per minute,
    and that no refresh opens or reads a socket.
  - **Phase**: 1
  - **Notes**: Current `main` deliberately coarsens Waybar hover fields to minute
    resolution to prevent hover dismissal. A fixed 1 Hz renderer ticker would
    work functionally but would replace daemon churn with avoidable client churn.

- [ ] #9: Separate rate limit for `windowtitlev2`
  - **Prereqs**: #6 complete — with the key normalized, the remaining cost of a
    title event is the fork pair, which is what this task bounds
  - **DoD**: `drainWMEvents` (`cmd/switchboard/main.go:580`) coalesces
    title-only layout events in a 1.5 s window while `openwindow` /
    `movewindowv2` keep the 200 ms `layoutDebounce`; a title event never
    delays a pending open/move flush. `internal/wm` events carry enough to
    tell the raw kinds apart (extend `wm.Event` or split `EventLayoutChanged`
    into `EventTitleChanged`). Tests (no real sleeps — use the injected
    debounce): `should coalesce a burst of title events into one re-resolve
    per window`, `should still re-resolve a new window within the short
    window while titles are bursting`, `should re-arm after a firing`. The
    hazard comment above `layoutDebounce` updated to describe both windows.
    Measured: `wezterm cli list` forks ≤ 1/s with two spinning Codex panes
    (count `wezterm` children via `journalctl` or `strace -f -e execve` for
    15 s).
  - **Phase**: 1

- [ ] #10: Phase 1 verification
  - **Prereqs**: #6, #7, #8, #8.5, #9 complete
  - **DoD**: `sb-mem-baseline` with two Codex sessions spinning and the bottom
    bar up shows wire publish rate < 0.5/s, daemon mean `RssAnon` < 25 MB over
    10 min, `publish-stats` shows `suppressed` ≫ `publishes`; `state.json`
    mtime advances < 1/s. Bar behavior unchanged by eye: chips, tooltips,
    "idle · Nm" counters, stale marking, remote chips. Numbers recorded in
    §5. Issue #80 updated.
  - **Phase**: 1

### Phase 3: Lazy Codex app-server [FUTURE — run after renderer consolidation]

- [ ] #11: Supervisor holds a connection only while a root or hook binding exists
  - **Prereqs**: #3 (invariant guard) and #10 (Phase 1 verified) complete —
    this is the only change that touches dashboard data, so it lands on a
    quiet, measured base
  - **DoD**: `codex.NewObserver` (`internal/provider/codex/observer.go:147`)
    no longer spawns the child. The supervisor loop (`run()`, line 327) is
    entered when the first root is observed or a hook binding is registered
    (`ReconcileHookBinding` already signals `refresh`), and torn down —
    child closed, per-connection ctx cancelled — after `Config.IdleShutdown`
    (default 2 min) with zero roots and no bindings. `IdleShutdown = 0`
    preserves today's always-on behavior. A root arriving during the grace
    cancels the shutdown; a root arriving after it restarts the supervisor
    with `ReconnectMinimum` backoff reset. `Close()` semantics unchanged.
    Tests (fake app-server via the existing `Connector` seam):
    `should not spawn the app-server before the first codex root`,
    `should spawn on the first root and initialize within one connect`,
    `should keep the child alive through the idle grace when a root returns`,
    `should close the child after the grace with no roots`,
    `should restart the child for a root that appears after shutdown`.
  - **Phase**: 3
  - **Notes**: Today `ctx`/`cancel` are observer-lifetime; introduce a
    per-connection context so shutdown does not poison `Close()`. Keep the
    `EphemeralNamer` path working — check whether it opens its own connection
    (`namer_test.go` suggests it uses `Connector` directly) and if so it is
    unaffected.

- [ ] #12: Dashboard invariant under lazy spawn (issue #83)
  - **Prereqs**: #11 complete
  - **DoD**: With the child not yet connected, `Observe` for a bound root
    returns the existing "snapshot pending" diagnostic and never a fabricated
    graph; on the first complete snapshot after spawn the projector emits the
    `unknown/none/unknown → observed` edges exactly once per node with
    `parent_thread_id` set; a resnapshot after reconnect is idempotent.
    Tests: `should emit first-snapshot agent_state edges exactly once after a
    lazy spawn`, `should not lose child agent_state for a root that appears
    during connect backoff`, `should not convert a child to not_found from a
    partial snapshot during spawn`. Live check: start a Codex session on a
    daemon that had none; the day-file shows the root's first
    `agent_state … source=codex_app_server` within 5 s of `session_start`.
  - **Phase**: 3

- [ ] #13: Journal categories + docs for child lifecycle
  - **Prereqs**: #11 complete
  - **DoD**: `agent-observer: provider=codex category=child_started|child_stopped
    count=1` lines (content-free, like the others); `docs/codex-session-status/
    03-codex-app-server-observer.md` describes the lazy lifecycle and the
    `IdleShutdown` knob; `-codex-observer auto|off` unchanged.
  - **Phase**: 3

- [ ] #14: Phase 3 verification
  - **Prereqs**: #12, #13 complete
  - **DoD**: With no Codex process running for > 2 min, `switchboard.service`
    cgroup anon < 40 MB and no `codex app-server` in `cgroup.procs`; with Codex
    running, footprint equals Phase 1's. `publish-stats` unchanged. Numbers
    recorded in §5.
  - **Phase**: 3

### Phase 2: One renderer instead of eleven socket clients [NEXT AFTER PHASE 1]

- [ ] #15: Interim env tuning in the bar config (zero code)
  - **Prereqs**: none — independent, ships the same day as Phase 0 if desired
  - **DoD**: Every `custom/claude-N.exec` in `~/.config/waybar/claude.jsonc`
    is `env GOGC=25 GOMAXPROCS=1 /home/tjmisko/.local/share/switchboard/current/switchboard-waybar --slot N`
    (and `--width-px` if not already set, which also removes the 10 × `hyprctl`
    startup fork). `sb-mem-baseline` shows per-slot RSS ≤ 8 MB. Reverted by #17.
  - **Phase**: 2
  - **Notes**: The dotfiles repo owns that file; note the change there.

- [ ] #16: FIFO renderer spike
  - **Prereqs**: none
  - **DoD**: A throwaway prototype proves, on Waybar v0.15.0: a `custom`
    module with `exec: cat $XDG_RUNTIME_DIR/switchboard/slot-0` and
    `restart-interval: 1` renders JSON lines from a reconnecting nonblocking
    writer. The writer opens `O_WRONLY|O_NONBLOCK` only while a reader exists,
    caches one latest line per slot, and retries after `ENXIO`/`EPIPE`; it must
    not hold an unread `O_RDWR` endpoint. Killing and restarting Waybar
    re-attaches and receives exactly the latest line; killing the writer makes
    `cat` exit and Waybar restart it within `restart-interval`. A sustained
    reader absence long enough to exceed a pipe buffer leaves writer memory
    bounded and never blocks another slot. The `cat` process is ≤ 1.5 MB RSS.
    Findings go under a `### 3.x spike` note, including Waybar quirks such as
    `exec-on-event` re-running `cat` after a click (set it `false`).
  - **Phase**: 2

- [ ] #17: Fold rendering into `switchboard-ctl bottombar watch`
  - **Prereqs**: #16 (design proven), #8 (single shared encode; otherwise the
    renderer's one subscription still costs a per-subscriber rebuild) complete
  - **DoD**: `cmd/switchboard-waybar`'s `renderSlot`, `sessionTooltip`,
    `agentTooltip`, `nameConfig` and `emitter` move to `internal/waybarchip`;
    `switchboard-waybar` keeps aggregate mode and `--slot` for debugging but
    is no longer in `claude.jsonc`. `bottombar watch` — which already owns the
    waybar process and already subscribes — becomes the renderer: one
    subscription, one decode per frame, `renderSlot` for `N` slots
    (`--slots 10` flag, default 10), one FIFO per slot under
    `$XDG_RUNTIME_DIR/switchboard/slot-N`, byte-identical dedupe per slot,
    all slots re-emitted on reconnect. Tests: `should write a slot line only
    when it changes`, `should re-emit every slot after reconnect`,
    `should keep FIFOs open across a waybar restart`, `should render the
    empty class for slots past the session count`, `should stop the bar and
    keep serving FIFOs when the last session ends` (or document that FIFOs
    are torn down with the bar — pick one and test it), and `should not let one
    absent or stalled FIFO reader block another slot`.
  - **Phase**: 2
  - **Notes**: `bottombar` runs before the daemon dial in `ctl main()` on
    purpose (must tolerate the daemon being down) — keep that.

- [ ] #18: Config, unit, docs cutover
  - **Prereqs**: #17 complete
  - **DoD**: `~/.config/waybar/claude.jsonc` modules use `cat` on the FIFOs
    with `restart-interval` and `exec-on-event: false`;
    `systemd/switchboard-waybar.service` runs the new watcher with
    `Environment=GOGC=50`; `docs/bars/*` describe the FIFO contract;
    `DONE.md` entry for #15 notes it is superseded. `sb-mem-baseline`:
    `switchboard-waybar.service` anon < 25 MB excluding the GTK `waybar`
    process; `sb-mem-baseline.socket_connections` shows exactly one persistent
    renderer connection (plus any short-lived RPC present at the sample). The
    `publish-stats store_subscribers` field is expected to remain at its normal
    View floor and is not the Phase 2 metric.
  - **Phase**: 2

### Phase 4: GC headroom [FUTURE — do not start]

- [ ] #19: `GOGC=50` on the daemon unit — only if still worth it
  - **Prereqs**: #10 complete — decide from post-Phase-1 numbers, not the baseline
  - **DoD**: If daemon `RssAnon` after Phase 1 is still > 25 MB:
    `systemd/switchboard.service` gains `Environment=GOGC=50` beside
    `GOMEMLIMIT` with a comment pointing at `publish-stats`; before/after from
    `sb-mem-baseline` recorded in §5; CPU (`systemctl --user show -p CPUUsageNSec`
    delta over 10 min) not worse than +10%. If already < 25 MB: task closed
    as "not needed" in `DONE.md` with the number.
  - **Phase**: 4

### Phase 5: Close-out [FUTURE — do not start]

- [ ] #20: Final numbers and hand-off to #89
  - **Prereqs**: #10, #14, #18, #19 complete
  - **DoD**: §5 table filled: per-unit anon and RSS with and without Codex,
    publish rate, fork rate; one full-day `MemoryPeak` reading for
    `switchboard.service` on the final build posted as a comment on issue #89
    (that is the reading #89 is waiting for — do not delete the fixtures here).
    Memory note `memory-footprint-baseline-2026-08-26` updated with the end
    state.
  - **Phase**: 5

---

## 4. Landing order and session loop

Order: #1 #2 #3 (parallel) → #4 → **#4.5** → #5 → #6+#7 → #8 → #8.5 → #9 →
#10 → #15 (after the control capture, if desired) → #16 → #17 → #18 → #11 →
#12 → #13 → #14 → #19 → #20.

#4.5 was added after the §1.4 baseline: it is a prerequisite for #6, not an
optimization on top of it. Until the hook overlay stops importing a 600 s
freshness horizon onto a live app-server graph, `fresh_until` oscillates
between two values ~10 minutes apart and no change-key bucketing can suppress
it — the measured floor is ~2.45/s against #10's < 0.5/s DoD. Tune #6's bucket
only against a capture taken after #4.5 has landed.

Suggested PRs: recovered Phase 0; #4.5 (its own PR — it touches history content
and needs a #83 cross-reference); stacked review units #5, #6+#7, #8, and #8.5,
rolled out together before Gate 2; #9; #16 findings as a doc commit; #17+#18;
Phase 3; #19.

After completing each task:
1. Tick the box here.
2. Move it to `DONE.md` with today's date and a one-line note (measured
   numbers go in the note).
3. Check whether the next task's prereqs are all in `DONE.md`.
4. If yes and it is in the current phase, begin it.
5. At a phase boundary, run the phase's verification task, then stop and
   summarize — every phase boundary here is a live-deploy decision the owner
   makes.

Deploy with `scripts/deploy`. It builds one immutable release, atomically flips
`~/.local/share/switchboard/current`, restarts the units, and verifies the
running revision from `/proc`. Use `scripts/deploy --status` before and after a
measurement. Do not copy binaries into `~/go/bin` or
`~/.config/switchboard/bin`; those paths predate the current deployment model.
Each daemon restart resets `MemoryPeak`.

---

## 5. Results

| Milestone | Daemon anon | service anon | renderer anon excl. GTK | dashboard anon | attributable anon total | full MemoryCurrent | wire publish/s | Date |
|---|---|---|---|---|---|---|---|---|
| Baseline | 42 MB | 62 MB (+33 MB Codex child) | ~88 MB | 8.6 MB | ~159 MB | ~336 MB across the three units | 9.4 | 2026-08-26 |
| After Phase 1 | _tbd_ | _tbd_ | _tbd_ | _tbd_ | _tbd_ | _tbd_ | _tbd_ | |
| After Phase 2 | — | — | _tbd_ | _tbd_ | _tbd_ | _tbd_ | — | |
| After Phase 3 (no Codex) | _tbd_ | _tbd_ | _tbd_ | _tbd_ | _tbd_ | _tbd_ | — | |
| Final with Codex | _tbd_ | _tbd_ | _tbd_ | _tbd_ | target <120 MB | _reported_ | _tbd_ | |
| Final without Codex | _tbd_ | _tbd_ | _tbd_ | _tbd_ | target <60 MB | _reported_ | _tbd_ | |
