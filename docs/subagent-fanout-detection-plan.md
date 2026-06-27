# Subagent fanout detection — reliability plan

Status: proposal. Goal: make switchboard detect and represent Claude Code
subagent fanouts (the Task/Agent tool spawning sub-agents *inside* one session)
reliably and resiliently, fixing the observed symptom that fanned-out agents
sometimes never appear on the dashboard.

## TL;DR

- **It is a switchboard (detection) problem, not a dashboard problem.** The
  dashboard already has first-class subagent support — it renders
  `lanes[].subagents[]` as sub-bars, shows `intervals[].subagents` counts, and
  computes the fanout (C) metric from `attention_fanout`. If subagents are
  missing on screen, it is because switchboard never emitted the spawn/stop
  events for them.
- **Root cause:** switchboard detects fanouts *only* by tail-parsing the parent
  transcript. `transcript.Tasks()` reads the last **128 KiB** (`statustune.go:92`,
  `TailBytes`) and explicitly drops "a Task whose launching `tool_use` has
  scrolled out of the window" (`transcript.go:417`). The Agent tool stores the
  **full subagent prompt** in `input.prompt`, so each fan-out `tool_use` block
  is large; fanning out several agents at once (or one long-running Explore/Plan
  agent whose spawn and `tool_result` straddle the window) overruns 128 KiB and
  the spawns/stops are silently lost.
- **Fix:** stop relying on a fixed tail window. Add two stronger signals Claude
  Code already provides — the per-session `subagents/` metadata directory
  (authoritative, append-only) and the `SubagentStart`/`SubagentStop` hooks
  (real-time, event-driven) — and convert the transcript fallback from a fixed
  tail to a forward byte cursor so no spawn is ever scrolled past.

## How detection works today

Pipeline (all in `switchboard` the Go daemon):

1. `internal/discovery` polls the OS process table (~1 s) and makes one
   `state.Session` per `claude`/`codex` **OS process** (`discovery.go`).
   **Subagents are not separate processes** — they run in-process — so discovery
   never sees them. This is correct; subagents must be derived some other way.
2. The reconcile loop (`cmd/switchboard/main.go:256`) calls
   `reconcileState.observe` → `observeFanout` (`cmd/switchboard/fanout.go:78`)
   for each Claude session every tick.
3. `observeFanout` calls `transcript.Tasks(c.Transcript, tun.TailBytes)`
   (`transcript.go:419`): tail-reads ≤128 KiB, finds `tool_use` blocks named
   `Task`/`Agent`, pairs them with `tool_result` blocks by `tool_use_id`, and
   returns `[]Task{ID, AgentType, Description, Done}`.
4. It diffs that set against a persistent per-pid `seen` bitmap, emits
   `EventSubagentSpawn` / `EventSubagentStop` history events, and **recomputes**
   `c.InFlightSubagents` as the count of in-window, not-done tasks.
5. Output surfaces consume those events: `internal/history/timeline.go` builds
   `lanes[].subagents[]` (spans) and `intervals[].subagents` (counts) and the
   `attention_fanout` summary; `switchboard-ctl timeline --json` emits them; the
   waybar chip shows `delegating · N agents`; the dashboard renders sub-bars.

So the data model and rendering are sound end-to-end. The weak link is step 3/4:
a **fixed 128 KiB tail** is the only source, and inflight is **recomputed from
the window** rather than tracked durably.

## Why fanouts get missed (failure modes)

1. **Scroll-out on multi-agent fan-out (primary).** Each Agent `tool_use`
   carries the entire subagent prompt in `input.prompt`. A single fan-out of
   several agents writes several large blocks; together with surrounding output
   they exceed 128 KiB, so the earliest spawns are never simultaneously in the
   window → never emitted → never on the dashboard.
2. **Long-running / straddling subagents.** Explore/Plan run for minutes. The
   spawn `tool_use` and its eventual `tool_result` end up far apart in the file;
   by completion the spawn has scrolled out, so the task is no longer in
   `Tasks()` `order` → the **stop** is never emitted and the sub-bar is left
   open-ended, and inflight under-counts mid-flight.
3. **Inflight recomputed-from-window.** Because
   `c.InFlightSubagents = <in-window not-done>`, a genuinely in-flight subagent
   whose spawn scrolled out simply *drops off the count* — the chip loses the
   delegating/green status and the dashboard loses the bar while work continues.
4. **Background agents (`run_in_background: true`).** Completion arrives async
   (a later task-notification), so the in-window `tool_result` correlation is
   unreliable → they tend to show as perpetually in-flight or never stop.
5. **No daemon-start backfill.** Unlike usage (which primes an offset),
   `observeFanout` has no authoritative seed; fanouts already scrolled out of
   the tail when the daemon starts are invisible.

## What Claude Code actually exposes (indicators, ranked)

Verified against the local `~/.claude` tree and the Claude Code docs:

| # | Indicator | Where | Reliability | Notes |
|---|-----------|-------|-------------|-------|
| A | `subagents/agent-<id>.meta.json` | `~/.claude/projects/<proj>/<session-id>/subagents/` | **Authoritative, persistent** | One file per spawn. Fields: `agentType`, `description`, `toolUseId`, `spawnDepth`. Append-only, never scrolls. ~624 already on disk. |
| B | `SubagentStart` / `SubagentStop` hooks | `~/.claude/settings.json` | **Real-time, event-driven** | Documented in hooks-guide. Matcher = agent type. Fires reliably; full stdin schema partly undocumented → must capture a real payload. Plugs into the existing `switchboard-ctl hook <event>` → RPC channel. |
| C | `subagents/agent-<id>.jsonl` | same dir | Good | Subagent's own transcript; existence + mtime/last-entry hints running-vs-done. |
| D | Parent transcript `tool_use`(`Agent`/`Task`)+`tool_result` | `<session-id>.jsonl` | Fragile (tail-bounded) | What we use today. Keep as cross-check, but cursor-based. |
| E | Process tree (`pgrep -P <pid>`) | OS | Unreliable | Subagent child PID is transient; no history. Ignore. |

`spawnDepth` (0/1/2 observed) gives nesting — depth-2 = a subagent that itself
fanned out. Background agents create their *own* top-level session IDs and are
already tracked by discovery as separate sessions — distinct from in-session
fanouts; do not conflate.

Path derivation: the stored `c.Transcript` is
`~/.claude/projects/<proj>/<session-id>.jsonl`; the subagents dir is the sibling
`~/.claude/projects/<proj>/<session-id>/subagents/`. Derivable with no new state.

## Proposed design — defense in depth

Three layers, each independently an improvement; together they make detection
both reliable (nothing missed) and resilient (no single fragile source):

### Layer 1 — Authoritative source: scan the `subagents/` directory
Replace (or front) the tail parse with a scan of the per-session `subagents/`
dir. Each `agent-<id>.meta.json` is a definitive spawn with stable metadata,
immune to scroll-out. This becomes the source of truth for *which* subagents
exist and their `agentType`/`description`/`toolUseId`/`spawnDepth`, and gives
free daemon-start backfill (enumerate the dir on first sight of a session).

Completion per subagent: correlate `toolUseId` with (a) a `SubagentStop` hook
(Layer 2), else (b) the parent `tool_result` (Layer 3 cursor), else (c) the
subagent's own `.jsonl` going quiescent (final entry / mtime older than a
threshold). Inflight = spawned − completed, tracked durably — never recomputed
from a window.

### Layer 2 — Real-time: `SubagentStart`/`SubagentStop` hooks → daemon
Install two hooks calling `switchboard-ctl hook SubagentStart|SubagentStop`,
mirroring the five hooks already wired (`SessionStart`, `UserPromptSubmit`,
`Stop`, `PermissionRequest`, `PostToolUse`). Extend the RPC `Request` + the
daemon's `handleHook`/`statusFromHookEvent` (`internal/rpc/rpc.go`) to:
- increment/decrement an authoritative per-session inflight counter,
- emit `EventSubagentSpawn`/`EventSubagentStop` at hook speed (carrying
  `agentType`, and `toolUseId`/`description` if present in the payload),
- drive the delegating (green) status edge directly instead of waiting for the
  next reconcile tick.

This is the most reliable real-time signal and removes all tail-window latency.
**Spike first:** add a `SubagentStart` hook that logs raw stdin to confirm the
payload fields (agent type, session id, tool_use id) before wiring behavior.

### Layer 3 — Harden the fallback: forward cursor, not fixed tail
For sessions actively watched, replace the fixed-tail read in the fanout path
with a per-session byte-offset cursor (mirror `usageOffset` in
`reconcileState`): each tick read only new bytes since the last offset and fold
spawns/results into the durable `seen` map. Guarantees every `tool_use` is
observed exactly once for a live session — eliminating scroll-out — while
bounding per-tick I/O. The fixed tail is kept only for cheap cold-start probing,
with the `subagents/` dir as the real backfill.

### Layer 4 (optional) — dashboard polish
Only if desired, and **in a worktree** (`gh worktree create`): surface
`spawnDepth` nesting (indent/stack depth-2 grandchildren) and a small badge
distinguishing background agents from foreground fanouts. The schema already
carries `subagents[]`, so this is additive rendering, not a contract change.

## Recommended sequencing

Highest value first. Layer 1 alone fixes the reported symptom (missed spawns);
Layer 2 makes it real-time; Layer 3 hardens the cross-check.

1. Reproduce + lock the bug with a failing test (scroll-out fixture).
2. Layer 1 (subagents/ dir scan) — fixes missed spawns + backfill.
3. Layer 2 (hooks) — real-time inflight + green edge.
4. Layer 3 (forward cursor) — resilient fallback / cross-check.
5. Layer 4 (dashboard polish) — optional.

## Definition of done

- Fanning out N agents at once (long prompts) shows all N sub-bars on the
  dashboard and `InFlightSubagents == N` on the chip while they run.
- A long-running Explore/Plan agent shows a sub-bar that *closes* when it
  finishes (stop event emitted), with no open-ended bars.
- Background (`run_in_background`) agents are detected and their completion is
  recorded.
- Daemon restart mid-fanout backfills the in-flight set from the `subagents/`
  dir rather than losing it.
- Detection survives a transcript far larger than `TailBytes` (regression test).

## Tasklist

- [ ] **T0 — Repro & pin.** Build a transcript fixture with many large
      `Agent` `tool_use` blocks (full prompts) exceeding 128 KiB and a
      long-straddling spawn→result pair; assert current `transcript.Tasks` /
      `observeFanout` drop spawns/stops. This is the regression guard.
- [ ] **T1 — subagents/ dir reader.** New `transcript.Subagents(sessionDir)` (or
      similar) that derives the dir from the transcript path and parses every
      `agent-*.meta.json` → `{toolUseID, agentType, description, spawnDepth}`.
      Unit test against a fixture dir.
- [ ] **T2 — Wire dir scan into `observeFanout`.** Use the dir as the spawn
      source of truth; keep emitting `EventSubagentSpawn` with metadata; backfill
      on first sight of a session. Make `InFlightSubagents` durable
      (spawned − completed), not window-recomputed.
- [ ] **T3 — Completion correlation.** Decide done per `toolUseId` via hook
      (T5) → parent `tool_result` → subagent `.jsonl` quiescence, in that order;
      emit `EventSubagentStop` once.
- [ ] **T4 — Hook payload spike.** Add a temporary `SubagentStart` hook that logs
      raw stdin; capture and document the real field names (agent type, session
      id, tool_use id, background flag).
- [ ] **T5 — Hook ingest.** Extend RPC `Request` + `handleHook` +
      `statusFromHookEvent` for `SubagentStart`/`SubagentStop`: authoritative
      inflight counter, spawn/stop events at hook speed, drive delegating edge.
      Install the two hooks in `~/.claude/settings.json` (and the installer/docs).
- [ ] **T6 — Forward-cursor fallback.** Add `fanoutOffset` to `reconcileState`;
      read new bytes since last offset instead of fixed tail; fold into `seen`.
      Prove the T0 scroll-out fixture now passes.
- [ ] **T7 — Background agents.** Detect `run_in_background: true` fanouts; rely
      on hook/`.jsonl` completion, tag them so the timeline can render distinctly.
- [ ] **T8 — Reconcile sources.** When dir + hook + transcript disagree, define
      precedence (hook > dir > transcript) and de-dup by `toolUseId` so one
      subagent yields exactly one spawn and one stop.
- [ ] **T9 — Tests + DoD.** Cover all DoD bullets; live-verify by fanning out
      several Explore agents and watching the chip + dashboard.
- [ ] **T10 — (optional, worktree) Dashboard.** `spawnDepth` nesting +
      background-agent badge in `switchboard-dashboard`.

## Empirical findings & corrected design (Wave 1, 2026-06-27)

Four parallel agents (binary decompile of CC 2.1.195 + 633 real meta.json + 641
subagent jsonl + adversarial corpus review) corrected three load-bearing
assumptions above. Authoritative facts now:

- **meta.json is heterogeneous.** Only `agentType` is present in 100% of files;
  `toolUseId` is **absent in ~36%** (in-process teammates, grandchildren, bare
  variants); `spawnDepth` absent in ~65%. Fields are camelCase. **The only
  universal key is the `agent-<id>` filename stem** — which also equals the
  hook's `agent_id` and the sibling `agent-<id>.jsonl`. ⇒ Key everything by
  `agent_id`; treat `toolUseId` as a best-effort transcript cross-check only.
  Add an `AgentID` field to `history.Event` so spans/dedup key on it.
- **Hooks (binary-verified, 2.1.195).** `SubagentStart` and `SubagentStop` both
  exist; payloads are snake_case and carry `agent_id`, `agent_type`,
  `session_id` (parent), `cwd`, and on Stop `agent_transcript_path`. **No
  `tool_use_id` in the payload** — correlate by `agent_id`. The hook must be a
  pure **trigger** for a re-scan, never a second writer of the counter (single
  writer = the Observer). See `[[cc-subagent-hook-schema]]` memory.
- **Done marker.** Last jsonl line `.message.stop_reason == "end_turn"` ⇒ a
  subagent's final turn ended. Guard with mtime quiescence (a between-turns
  `end_turn` is possible) and a hard age cap that force-closes as
  completion=unknown so inflight can never leak. Absent jsonl ⇒ running.
- **Background** = parent tool_use `input.run_in_background == true` (only count
  it for `name` ∈ {Agent,Task}; it also appears on Bash etc.). `SubagentStop`
  may NOT fire for background agents ⇒ dir/jsonl-quiescence stays authoritative.
- **spawnDepth semantics (verified on the corpus):** `1` = direct child of main
  (toolUseId in main transcript 79/81); `2` = grandchild (toolUseId in main
  transcript **0/8**); absent = mostly direct. ⇒ **Exclude `spawnDepth>=2` from
  the main thread's inflight/fanout count** (render nested as decoration only);
  count depth-1/absent. (The earlier "filter to depth 0" idea was backwards.)
- **The flat `subagents/` dir mixes depths 0/1/2 of one session.** Grandchildren
  cannot be completion-correlated via the main transcript (their tool_use isn't
  there) — close them via child-jsonl quiescence/cap only.
- **Orphans:** ~8 `agent-<id>.jsonl` exist with no sibling meta; metas are NEVER
  deleted. ⇒ spawn set = {meta files} ∪ {jsonl files} keyed by `agent_id`.

### Hardening the Observer (from the adversarial review)

- **G1 restart/resume double-count (CRITICAL):** metas are immortal and state is
  pid-keyed, so a restart or `claude --resume` (new pid, same session-id, same
  dir) would re-emit every historical spawn. ⇒ Key durable state by
  **session-id**, and on first sight **seed the seen-set by replaying already-
  emitted `subagent_spawn`/`stop` events for that session-id from the history
  log** (mirror how `observeUsage` primes its offset to filesize).
- **G5 cursor reset:** on `/clear`/compaction the file shrinks (`offset>size`);
  clamp `offset ≤ filesize` and, on shrink, re-derive `seen` from history, not
  from zero. Idempotency lives in the `agent_id`-keyed seen-set, not the cursor.
- **G7 race:** a `SubagentStop` trigger can arrive before the meta flushes;
  buffer a stop for an as-yet-unseen `agent_id` so a meta arriving a tick later
  doesn't resurrect it. Preserve "spawn+stop in one pass" for rapid lifecycles.
- **G9 clock skew:** date a dir-discovered spawn from the meta/jsonl mtime (not
  reconcile-now) and clamp `stop ≥ spawn` so spans aren't dropped as negative.
- **G10:** deriving the subagents dir as the sibling of `c.Transcript` is robust
  to worktrees/`/name`/XDG — do not re-derive from cwd or a project slug.

### Resolved open questions
- Hook stdin schema: resolved (binary). Done marker: resolved (stop_reason +
  quiescence + cap). Background: parent tool_use input; Stop may not fire → keep
  jsonl path. spawnDepth≥2: exclude from count, render nested.
