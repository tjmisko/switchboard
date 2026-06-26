# Usage History & Activity Timeline — design plan

> **Goal.** Turn Switchboard from a *live* view of sessions into one that also
> remembers. Four asks, in increasing scope:
>
> 1. **Idle/wait counter on hover** — show how long a session has been idle, and
>    how long it has been *waiting on a permission decision*, in the tooltip.
> 2. **A durable activity store** — "keep this data for users if they want it,"
>    stored *nicely*, so a future dashboard can be built on it.
> 3. **Plan usage over time** — track how much of the Claude plan this machine has
>    burned, over time.
> 4. **Fanout + a summary stat** — capture agent fan-out (subagents) from the log,
>    and compute "hours of agent attention." Ultimately: a **timeline view** with
>    parallel sessions overlapping, colored by status over time.
>
> **The dashboard itself is out of scope here.** This plan designs the *data* —
> the model, the store, the seams that emit it — so the dashboard is a later,
> independent build against a stable format. It also specs the one piece worth
> shipping immediately (the hover counter), because it falls out of the same
> in-memory field (`StatusSince`) the history store needs on the wire.
>
> Style and discipline follow `docs/status-color-state-model.md`: map the real
> state space, justify each decision against its error cost, phase the work
> test-first, and surface the genuine forks as Open Questions (answered via the
> companion interview).

---

## 0. TL;DR — the shape of the answer

- The daemon **already computes every status transition** and logs it
  (`statustune.Decision.Log()` → `status: pid=… FROM->TO rule=… [S=… pending=…
  age=…]`). That stream **is** the activity history; today it only lands in
  journald (un-owned retention, grep-only). The plan is to **tee it to a store
  Switchboard owns**, structured for query.
- A session's color-over-time is a sequence of **intervals**: consecutive
  transitions `(status, t_start, t_end)`. Intervals per session = **swimlanes** =
  the timeline view. Every summary stat (idle time, red-wait time, hours of
  attention) is an aggregation over intervals. So the store's atom is the
  **transition event**; everything else is derived.
- **Storage: append-only JSONL, one file per UTC day, under `$XDG_STATE_HOME`**
  (not cache — this is durable). Pure-Go, zero-dep, portable, greppable, and it
  matches the no-cgo constraint that already governs this repo (SQLite would
  reintroduce the cgo dependency Phase 4/macOS is blocked on).
- **Opt-in.** It records when and where you work. Default **off**, enabled by a
  one-line config flag, with a `switchboard-ctl history purge` escape hatch.
- **Fanout is already derivable** (`transcript.InFlightTasks`, the `[S=…]` tuple
  on every decision line, and the on-disk subagent transcripts). We capture it as
  `subagents` on each event for free, and can enrich it later.
- **Token/plan usage is the one genuinely new data source** — it lives in the
  transcript `usage` blocks, not in anything Switchboard tracks today. It is the
  least coupled piece and can land last (or as a separate sampler).

---

## 1. What already exists (the substrate)

Everything below is in-tree today; the plan reuses it rather than inventing.

### 1.1 `StatusSince` — the idle/wait clock already exists, in memory only
`AgentInfo.StatusSince` (`internal/state/state.go:138`) marks **when the current
status began**. It is stamped on every real transition (`rpc.go:352`,
reconciler `main.go`), carefully anchored to the transcript to dodge clock skew
(`transcript.AnchorSince`). It is exactly "how long has this been idle / waiting
on permission."

**But it is `json:"-"`** — in-memory only, deliberately kept off the wire so the
frozen `state.json` contract is unchanged. The RPC snapshot is JSON-encoded
(`rpc.go:125`, `:160`), so **the renderer never sees it.** The hover counter's
whole cost is getting this one timestamp to the renderer (see F1).

### 1.2 The decision log — a transition time-series that already exists
Every color change *and* every deliberate hold routes through
`statustune.Decision.Log()` (`internal/statustune/statustune.go:122`):

```
status: pid=4821 session=ce13c0f2 permission->working rule=case9-approve-toolmatch reason="tool-name match: AskUserQuestion" [S=0 pending="AskUserQuestion" age=2s]
```

This carries the transition (`FROM->TO`), the **rule** that fired, the
**subagent count** `S`, the **pending tool**, and `age=` (**how long the chip
held the prior status** — i.e. the duration of the interval that just ended).
It already lands in journald as `switchboard.service`, and there is already a
**parser** (`statustune.ParseDecision` → `statustune.Record`,
`internal/statustune/parse.go`) and a **consumer** (`switchboard-ctl diagnose`)
that mines it for wrong-color forensics.

> The history store is, at its core, **the same Record stream written to a place
> Switchboard owns** (durable retention, day-partitioned, query-shaped) instead
> of only to journald (un-owned vacuum policy, line-grep only).

What the decision log is missing for a *complete* history: **session lifecycle**
(start / end / suspend), which today are plain `log.Printf` lines
(`main.go:67`, `:73`) not routed through `Decision`. Those become first-class
events in the store.

### 1.3 Fanout is already derivable
- `transcript.InFlightTasks(path, maxBytes)` (`transcript.go:400`) pairs
  `tool_use`(name∈{Task,Agent}) against `tool_result.tool_use_id` over the main
  transcript tail → **count of subagents in flight**. Recomputed every reconcile
  tick (`main.go:222`) and already on the wire as `in_flight_subagents`.
- The `[S=…]` tuple on every decision line records that count **at each
  transition** — so the history stream already carries fanout depth over time.
- Richer fanout (which agent, doing what) lives on disk: Claude Code writes
  subagent transcripts at
  `~/.claude/projects/<cwd>/<session-id>/subagents/agent-*.jsonl` with a
  `*.meta.json` carrying `{agentType, description, toolUseId}` linking each
  subagent to its spawning `Task`. The `SubagentStop` hook fires on the parent
  session (carries `agent_id`, `agent_transcript_path`) but is **not wired** to
  Switchboard today (status-color-state-model B3, deferred). There is **no**
  `SubagentStart` hook — spawns are inferred from the `Task` tool_use.

### 1.4 Token/plan usage is NOT tracked anywhere
Confirmed repo-wide: Switchboard reads the transcript only for *status* signals
(resolution, interrupts, Task pairing) — never `usage`. Claude Code records
per-assistant-message `usage` (`input_tokens`, `output_tokens`,
`cache_read_input_tokens`, `cache_creation_input_tokens`) in the same `.jsonl`
the daemon already tails. So plan usage is **reachable from a source we already
open**, but is otherwise greenfield (new parser, new aggregation).

### 1.5 Config + path precedents to reuse
- **Writable user config**: `projectname` already owns
  `$XDG_CONFIG_HOME/switchboard/projects.json` (`projectname.go:209`), upserted
  atomically. The history opt-in toggle + retention knobs belong in the same
  config family (`switchboard/history.json` or a shared `config.json`).
- **State path**: `state.json` lives under `$XDG_CACHE_HOME` (ephemeral). History
  is **durable** and belongs under `$XDG_STATE_HOME/switchboard/` (falling back
  to `~/.local/state/switchboard/`) — the XDG-correct home for "data the user
  would be annoyed to lose, but isn't config." No `XDG_STATE_HOME` usage exists
  yet; this introduces it.

---

## 2. The data model — events → intervals → swimlanes → stats

One model serves all four asks. The store records **events**; everything the
dashboard shows is **derived** from them.

### 2.1 The atom: an event
```jsonc
{
  "ts": "2026-06-26T14:32:07.412Z",   // RFC3339, UTC, ms precision
  "type": "transition",                // transition | session_start | session_end | suspend | resume | usage_sample | subagent_spawn | subagent_stop
  "session_id": "ce13c0f2-…",          // stable across PID reuse / restarts; the join key
  "pid": 4821,
  "agent": "claude",                   // claude | codex
  "project": "sb",                     // projectname.CanonicalForDir(cwd) — abbrev, not raw path (privacy tier; see §4)
  "cwd": "/home/u/Projects/switchboard", // omitted at the "minimal" privacy tier
  // type=transition:
  "from": "permission", "to": "working",
  "rule": "case9-approve-toolmatch",
  "subagents": 0,                      // S at the moment of transition (fanout depth)
  "pending": "AskUserQuestion",        // the tool a permission prompt was for
  "dur_prev_ms": 2000,                 // how long `from` was held (= the closed interval; mirrors age=)
  // type=usage_sample (see §6):
  "tokens": { "in": 1234, "out": 567, "cache_read": 89000, "cache_creation": 4096 }
}
```

This is a **superset of `statustune.Record`** plus lifecycle and usage. A
`transition` event is literally a `Record` with `ts`, `project`, and `cwd`
attached — so the daemon can emit it at the exact same chokepoint that already
calls `Decision.Log()`.

### 2.2 The derived view: intervals (swimlanes)
Consecutive transitions for a session bound an interval:
`(session_id, status, t_start, t_end)`. A session's intervals, laid end to end,
are its **swimlane** — a colored bar over time (green/orange/red/grey). Stack
swimlanes by session and you have the **timeline view with parallel sessions
overlapping** the user asked for. No extra data is needed beyond the transition
stream — `dur_prev_ms` even pre-computes each interval's length, but it is
recoverable from adjacent `ts` alone, so it is belt-and-suspenders.

### 2.3 The summary stats (all aggregations over intervals)
- **Idle time / red-wait time** per session/day: sum of `idle` / `permission`
  interval lengths. (Red-wait directly answers "how long have I been blocking an
  agent.")
- **Hours of agent attention**: a configurable aggregation — see §7, it has
  three defensible definitions and is a real fork.
- **Fan-out over time**: `subagents` sampled at each transition → a step function;
  area under it ≈ subagent-seconds.
- **Plan usage over time**: sum of `usage_sample` token deltas, bucketed by the
  plan's rate-limit windows (§6).

The point: **pick the right atom (the transition event), and every stat and the
whole timeline are queries, not new pipelines.**

---

## 3. Storage design

### 3.1 Format — append-only JSONL, day-partitioned (recommended)
```
$XDG_STATE_HOME/switchboard/history/2026-06-26.jsonl
$XDG_STATE_HOME/switchboard/history/2026-06-27.jsonl
```
One JSON event per line, appended (`O_APPEND`, line-atomic for the small records
we write). Day partitioning gives free time-range pruning (a timeline query for
"last 3 days" opens 3 files), trivial retention (delete files older than N days),
and bounded file sizes.

**Why JSONL over SQLite:**
- **Portability / no-cgo.** This repo deliberately avoids cgo — it is the exact
  thing blocking Phase 4 (macOS). `mattn/go-sqlite3` is cgo; the pure-Go
  `modernc.org/sqlite` is a heavy dependency for a tool whose headline is "single
  static binary, runs anywhere." JSONL keeps the zero-dependency promise.
- **Append is the access pattern.** We only ever append events and scan ranges.
  That is JSONL's sweet spot and SQLite's least-needed feature.
- **Inspectable + recoverable.** `jq`, `grep`, `tail -f` work directly; a torn
  final line (crash mid-write) costs one event, not the DB.
- **It is the format we already parse.** `ParseDecision` already reads this
  stream from journald; the store is the same records, structured.

**When SQLite would win** (note for the interview): heavy *interactive* dashboard
queries over months of history (indexed range scans, joins). If the dashboard
grows into that, the JSONL is the durable log of record and a **derived** SQLite
(or DuckDB-over-Parquet) index can be rebuilt from it — never the other way
round. Designing JSONL-first keeps that door open; designing SQLite-first slams
the portability door now for a dashboard we are not building yet.

### 3.2 Location — `$XDG_STATE_HOME`, not cache
`state.json` is cache (regenerable from `/proc`, wiped freely). History is
**not regenerable** — once a day passes unrecorded it is gone — so it is *state*,
not cache. `$XDG_STATE_HOME/switchboard/` (→ `~/.local/state/switchboard/`) is
the XDG-correct home. A `-history-dir` daemon flag overrides it (mirrors
`-state`).

### 3.3 Retention & rotation
- Default cap by **age** (e.g. keep 90 days) and/or **total size** (e.g. 100 MB),
  whichever first; both configurable, `0` = unlimited. Pruning runs at daemon
  start and once a day — just `os.Remove` on day-files past the window.
- Volume sanity check: a heavy-dev day in the measured window had ~43 red
  episodes + the working/idle churn; call it low-hundreds of transitions/day.
  At ~250 B/event that is well under 100 KB/day — **months fit in a few MB**.
  Token usage_samples (if enabled, §6) are the only thing that could inflate
  this; bound them by sampling cadence.

### 3.4 Who writes it — a `history.Sink` at the existing chokepoint
A small `internal/history` package with a `Sink` the daemon owns:
- `Sink.Record(ev Event)` — append one event (best-effort; a write failure logs
  and is dropped, never blocks or crashes the daemon, exactly like
  `state.persist`'s error handling).
- **One wiring point for transitions**: `statustune.Decision` already funnels
  every status edge. Give the daemon a hook so that wherever `Decision.Log()` is
  called we *also* `sink.Record(...)`. (Cleanest: have the daemon construct
  Decisions through a tiny helper that does both, or add an optional
  `Decision.Emit(sink)` — keeps `statustune` a dependency-free leaf by passing
  the sink in.)
- **Lifecycle events**: `session_start`/`session_end` in `onAgentAppeared` /
  the death callback (`main.go:67`/`:73`); `suspend`/`resume` where the
  reconciler reads `/proc` run-state.
- **No new goroutine, no socket.** It is a synchronous append on the reconcile /
  hook path, which already does file I/O (`state.persist`).

---

## 4. Privacy & opt-in (a first-class concern)

This data is a **log of when, where, and how hard you work.** It must be the
user's choice, local, and easy to destroy.

- **Default off.** History recording is enabled by a single config flag
  (`history.enabled: true` in the switchboard config) or a `-history` daemon
  flag. When off, **zero** files are written.
- **Privacy tiers** (config `history.detail`): `minimal` records only
  `session_id`, `agent`, `project` (the abbrev), status, and timing — no raw
  `cwd`, no task names. `full` adds `cwd` and pending-tool detail. Default
  `minimal`.
- **Local only.** Switchboard never transmits anything; the file stays on the
  machine. (Worth stating loudly in README so "telemetry" doesn't scare anyone.)
- **Escape hatches**: `switchboard-ctl history purge [--before DATE]` deletes
  files; `switchboard-ctl history path` prints the directory; the day-file layout
  means a user can `rm` a single day by hand.

---

## 5. Feature F1 — the hover idle/wait counter (ship now)

The only piece worth building immediately; it is small and self-contained, and
it forces the one wire decision (`StatusSince` → renderer) that F2 also wants.

### 5.1 Get `StatusSince` to the renderer
`StatusSince` is `json:"-"`. Add it to the wire **additively** (the schema doc's
own rules class additive optional fields as non-breaking):
```jsonc
"claude": { "status": "idle", "status_since": "2026-06-26T14:30:00Z" }
```
- Add `StatusSince` back as a real JSON field **or** mirror it into a new
  exported `StatusSince time.Time json:"status_since,omitempty"`. (It is already
  populated in memory; we only flip its visibility.) This regenerates the golden
  fixture (`UPDATE_GOLDEN=1`) and is a deliberate, reviewed schema bump per
  `state-schema.md` §versioning. Update `docs/state-schema.md` in the same PR.
- The renderer computes `now - status_since` itself, so the duration is correct
  regardless of snapshot age.

### 5.2 Render it in the tooltip
`sessionTooltip` (`cmd/switchboard-waybar/main.go:178`) already prints the status
word; append a humanized duration:
```
sb   ● idle · 3m12s            ← turn-ended; your turn
sb   ● permission · 0m45s      ← waiting on YOU for 45s (the explicit ask)
sb   ● delegating · 2 agents · working 1m4s
```
Humanize with a compact formatter (`45s`, `3m`, `1h04m`). The reference TUI
(`cmd/claude-tui`) gets the same treatment so both renderers agree.

### 5.3 Granularity — snapshot cadence vs live tick (Open Q)
Waybar tooltips are static per emitted line; the renderer re-emits on each
snapshot, i.e. **every reconcile tick (~5 s) or any mutation**. So a tooltip
counter advances in ~5 s steps unless the renderer also self-emits on a 1 s
timer. Recommendation: **accept snapshot cadence** for v1 (a hover counter that
updates every few seconds is fine; it is not a stopwatch), and leave a
renderer-side 1 s ticker as a later polish. (See Q-F1.)

### 5.4 DoD
- `status_since` present on the wire; golden + `state-schema.md` updated.
- Tooltip shows idle duration and permission-wait duration, humanized.
- Unit test on the humanizer; a renderer test asserting the tooltip string for an
  idle and a permission session at a known `now - status_since`.

---

## 6. Feature F3 — plan / token usage over time

The one new data source. Keep it **decoupled** so it can land independently.

- **Source**: per-assistant-message `usage` in the transcript `.jsonl` (already
  opened by `transcript`). A new `transcript.UsageTail(path, sinceOffset)` reads
  new `usage` blocks since the last read and returns token deltas. (Codex
  rollouts carry analogous usage; same shape.)
- **Sampling**: cheapest correct approach is to read **cumulative** usage on the
  `Stop` edge (turn boundary) and on the reconcile tick for live sessions, and
  emit a `usage_sample` event with the delta since the last sample. Avoid
  re-reading the whole transcript — track a byte offset per session.
- **Metric — what does "plan usage" mean?** (Open Q-F3.) Options:
  1. **Raw tokens** (in/out/cache split). Faithful, model-agnostic, but cache
     tokens dominate and don't map linearly to "plan consumed."
  2. **Cost estimate** (tokens × per-model price). Human-meaningful but needs a
     price table and is wrong the moment prices change.
  3. **Plan-window consumption** — Claude plans rate-limit on rolling windows
     (e.g. 5-hour + weekly). We can't read the actual quota from outside, but we
     *can* bucket token usage into those windows and show "this much used since
     the window opened." Most useful for "am I about to hit my limit," which is
     the spirit of "track usage of my plan."
- **Aggregation**: per session, per project, per day, and per plan-window. All
  are sums over `usage_sample` events — same interval-aggregation machinery.
- **Privacy**: token counts are low-sensitivity (no content), but they still
  reveal activity; gate behind the same `history.enabled`.

This is the **most speculative** piece (depends on transcript usage-block shape,
which is Claude-Code-internal and may drift) — so it is sequenced **last** and
written behind its own small parser with golden fixtures, so a format drift
breaks one test, not the daemon.

---

## 7. Feature F4 — fanout capture & "hours of agent attention"

### 7.1 Capturing fanout (cheap → rich)
1. **Free, today**: `subagents` (the `S` count) is already on every transition
   event. That alone gives fan-out *depth* over time — enough to draw the count
   as a step function and weight an attention stat.
2. **Spawn/stop events** (`subagent_spawn`/`subagent_stop`): derive spawns from
   the `Task` tool_use the daemon already pairs in `InFlightTasks`; derive stops
   from the pairing's completion **or** by finally wiring the `SubagentStop` hook
   (status-color-state-model B3) for hook-speed precision. Each spawn can carry
   `{agentType, description}` read from the subagent `*.meta.json`. This yields a
   true **fan-out tree per turn**: which agents, doing what, when.
3. **Decision**: ship (1) with the history store immediately (zero new code —
   the field is already computed); offer (2) as a follow-up once the store
   exists. (See Q-F4.)

### 7.2 "Hours of agent attention" — define it (Open Q-F4)
Genuinely ambiguous; the model supports any, but the headline number depends on
the choice:
- **A. Active wall-clock (union).** Time during which *at least one* session was
  working/delegating. "How many hours was something happening for me." ≤ real
  time. Most intuitive headline.
- **B. Per-session sum.** Σ over sessions of working-time. Rewards parallelism
  (3 sessions working for 1 h = 3 attention-hours). "How much agent-work did I
  orchestrate."
- **C. Fan-out-weighted.** Σ over *all agents incl. subagents* of busy-time
  (integrate the `subagents+1` count over working intervals). Captures the full
  compute you directed — a 5-way fan-out for 10 min counts as ~50 agent-minutes.
  The truest "agent attention," and the reason capturing fan-out matters.

Recommendation: compute and store the intervals; let the dashboard/CLI **show
all three** (they answer different questions) with **C as the headline** since it
is the one fan-out unlocks and the one the user gestured at ("how well you've
used your agents").

---

## 8. Phased, test-first plan

Each phase is independently shippable and verifiable. Phases 2–4 are gated on the
interview answers (storage format, opt-in default, attention definition).

### Phase 1 — Hover counter (F1) — *no store needed; ship first*
- 1.1 Expose `status_since` on the wire; regen golden; update `state-schema.md`.
- 1.2 Humanizer + tooltip duration in waybar and claude-tui renderers.
- **DoD**: §5.4. Independently useful, zero new persistence, answers asks #1 & the
  permission-wait ask immediately.

### Phase 2 — The history store (F2 core)
- 2.1 `internal/history`: `Event` type, `Sink` (append JSONL, day-partitioned,
  best-effort), path resolution under `$XDG_STATE_HOME`, retention/prune.
- 2.2 Config: `history.enabled` / `history.detail` / retention knobs (extend the
  `projectname`-style writable config, or a shared `config` package).
- 2.3 Wire transition events at the `Decision` chokepoint; lifecycle events at
  discovery/death/suspend.
- 2.4 `switchboard-ctl history` subcommands: `path`, `purge`, and a `tail`/`stat`
  for sanity.
- **DoD**: with `history.enabled`, a day-file accrues one well-formed event per
  transition and per lifecycle change; retention prunes old files; `purge` works;
  golden-fixture test on the event encoder; an integration test driving a fake
  session through transitions and asserting the JSONL.

### Phase 3 — Derivation + summary CLI (F4 stats, no GUI)
- 3.1 `internal/history` reader: stream events → intervals → per-session
  swimlanes.
- 3.2 `switchboard-ctl timeline` (text/JSON): print swimlanes for a day/range and
  the summary stats (idle, red-wait, attention A/B/C). JSON output is the stable
  contract the future GUI consumes.
- **DoD**: reader reconstructs intervals from a fixture stream; stats match
  hand-computed expectations; `--json` shape is documented and golden-tested.

### Phase 4 — Plan/token usage (F3)
- 4.1 `transcript.UsageTail` + per-session byte-offset tracking.
- 4.2 `usage_sample` events; per-day/window aggregation in the reader; surface in
  `switchboard-ctl timeline --usage`.
- **DoD**: usage parser golden-tested against captured transcript fixtures;
  aggregation matches; gracefully no-ops when usage blocks are absent/changed.

### Phase 5 (later, out of scope now) — the dashboard
A GUI/web/TUI timeline over the Phase 3 JSON contract. Explicitly **not built
here**; the data model above exists to make it a clean, independent project.

---

## 9. Open questions (answered in the companion interview)

- **Q-STORE-1 — format**: JSONL day-files (recommended) vs pure-Go SQLite vs
  both. Trades portability/simplicity against future interactive-query speed.
- **Q-STORE-2 — opt-in**: default **off** (recommended, privacy-first) vs
  default **on, local-only, easy purge**.
- **Q-STORE-3 — detail/privacy tier**: `minimal` (no raw cwd/task) vs `full`, and
  what the default is.
- **Q-STORE-4 — retention**: keep-N-days and/or size cap, and the defaults.
- **Q-F1-1 — counter scope**: which statuses show a duration (idle + permission
  only, or also working/delegating)?
- **Q-F1-2 — granularity**: snapshot-cadence counter (recommended v1) vs
  renderer-side 1 s live tick.
- **Q-F1-3 — wire change**: confirm we may add `status_since` to the frozen
  `state.json` (additive, golden regen) — or keep `state.json` frozen and expose
  the counter via a side channel.
- **Q-F3-1 — usage metric**: raw tokens vs cost estimate vs plan-window
  consumption (which is the headline?).
- **Q-F3-2 — scope now**: build F3 in this arc, or design-only and defer?
- **Q-F4-1 — attention definition**: A (union wall-clock) / B (per-session sum) /
  C (fan-out-weighted) — which is the headline stat?
- **Q-F4-2 — fanout richness**: count-only (free, now) vs spawn/stop events with
  `{agentType, description}` (wire `SubagentStop`, read meta.json).
- **Q-SCOPE — sequencing**: confirm Phase 1 ships now; confirm Phases 2–4 are
  design-locked here and built later; confirm the dashboard stays out of scope.

---

## 9a. Implementation status (shipped)

The interview (`.claude/interviews/usage-history-timeline-2026-06-26.md`) expanded
scope from "ship F1, design the rest" to **build the whole data-collection
substrate (F1–F4)**, deferring only the GUI dashboard. All of it landed; decisions
taken:

| Q | Decision |
|---|----------|
| Q-STORE-1 | **JSONL day-files** under `$XDG_STATE_HOME` (refactor to SQLite later only if needed). |
| Q-STORE-2 | **Default OFF**, enabled via `history.json` (`{"enabled":true}`). |
| Q-STORE-3 | **Default `minimal`** (no raw cwd / task description / pending tool). |
| Q-STORE-4 | **90 days / 100 MB**, pruned at startup + on day-rotation. |
| Q-F1-1 | Duration shown for **all statuses** in the hover tooltip + TUI. |
| Q-F1-2 | **Snapshot cadence** (every transition is captured as an event regardless). |
| Q-F1-3 | **`status_since` added to `state.json`** (additive; golden + schema updated). |
| Q-F3-2 | Token usage **built now** (`usage_sample` events). |
| Q-F4-1 | **All three** attention stats (A union / B per-session / C fanout); C is the headline. |
| Q-F4-2 | **Both**: subagent count on transitions **and** rich `subagent_spawn/stop` events with `agent_type`/`description`. |

What shipped, by seam:

| Feature | Where | Status |
|---|---|---|
| F1 hover idle/wait counter | `state.StatusSinceWire` (derived in `snapshotLocked`), `internal/durfmt`, waybar + claude-tui tooltips | ✅ |
| F2 durable store | `internal/history` (`Event`/`Sink`/`Config`/reader), wired at the `Decision` chokepoint + lifecycle/suspend; `switchboard-ctl history path\|tail\|stat\|purge` | ✅ |
| F3 token/plan usage | `transcript.UsageSince` + per-session offset (primed), `usage_sample` events, `AggregateTotals` | ✅ |
| F4 fanout | `transcript.Tasks` (rich metadata), `reconcileState` diff → `subagent_spawn/stop`, count on transitions | ✅ |
| F4 stats + timeline CLI | `history.BuildSwimlanes`/`Summarize`/`AggregateTotals`, `switchboard-ctl timeline [--json]` | ✅ |
| Phase 5 dashboard | — | ⏭ out of scope (the `timeline --json` contract is its input) |

Contract docs: `docs/history-schema.md` (the store) and `docs/state-schema.md`
(`status_since`). Optional future precision: wire the `SubagentStop` hook for
hook-speed drains (today the tick-based transcript diff suffices); a cost-table
mapping for tokens→dollars.

## 10. Risks & non-goals

- **Frozen contract**: `state.json` is a public contract. The only change F1
  needs is the *additive* `status_since`; it must go through the golden-regen +
  doc-update ritual (`state-schema.md` §versioning). No other field changes.
- **Transcript-format coupling**: F3 (and richer F4) lean on Claude-Code-internal
  `.jsonl` shapes that can drift. Isolate every such read behind a parser with
  golden fixtures so drift fails a test, never the daemon — the same discipline
  `transcript` already follows.
- **Don't reinvent the decision log**: the temptation is a parallel telemetry
  pipeline. Resist — tee the *existing* `Decision` stream; one source of truth
  for "what color, when, why."
- **Non-goal: the dashboard.** Not built here. **Non-goal: remote/cloud
  anything.** Local only, always.
</content>
</invoke>
