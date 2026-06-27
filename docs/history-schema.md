# Activity-log Schema (`history/*.jsonl`)

> The activity log is Switchboard's **durable, opt-in** record of session
> activity over time — the data a future timeline/usage dashboard reads. It is
> the same information the live `state.json` and the journald decision log carry
> (`statustune.Decision`), written to a place Switchboard owns so retention is
> bounded and the data is query-shaped. **It is OFF by default.**
>
> The Go source of truth is `internal/history` (the `Event` struct in
> `history.go`); this document is the on-disk/consumer contract.

## Where it lives

```
$XDG_STATE_HOME/switchboard/history/2026-06-26.jsonl   (→ ~/.local/state/switchboard/history/…)
```

One **append-only JSON-per-line** file per **local calendar day** (the file is
named for the day in your machine's timezone, so a day-file lines up with your
wall-clock day rather than rolling mid-evening at UTC midnight). State, not
cache: losing a day is permanent (unlike `state.json`, which the daemon rebuilds
from `/proc`). Override the directory with the daemon's `-history-dir` flag.
Readers select files by name but filter on each event's own timestamp, so a
directory holding a mix of local- and legacy UTC-named files reads correctly.

Day-partitioning gives free time-range pruning, trivial retention (delete old
files), and bounded file sizes. A crash mid-append costs at most the final
(torn) line; readers skip any line that does not parse.

## Enabling it (opt-in)

`$XDG_CONFIG_HOME/switchboard/history.json` (sibling of `projects.json`):

```jsonc
{
  "enabled": true,        // default false — nothing is written when off
  "detail": "minimal",    // "minimal" (default) | "full"
  "retain_days": 90,      // delete day-files older than this; 0 = unlimited
  "max_bytes": 104857600  // trim oldest day-files past this total; 0 = unlimited
}
```

The daemon logs `history: enabled=… detail=… dir=…` at startup. Recording is
**local only** — Switchboard never transmits the log.

## Privacy tiers

| Tier | Records | Omits |
|------|---------|-------|
| `minimal` (default) | `ts`, `type`, `session_id`, `pid`, `agent`, `project` (abbrev), `from`/`to`, `rule`, `subagents`, `dur_prev_ms`, `agent_type`, `tool_use_id`, token counts, `model` | `cwd`, `pending`, `reason`, `description`, `label` |
| `full` | everything above **plus** `cwd`, `pending`, `reason`, `description`, `label` | — |

The subagent `agent_type` (e.g. `Explore`), token counts, and the usage-sample
`model` are kept at the minimal tier — they describe *how much*, *what kind*, and
*which model*, not the content of your work; the subagent `description` (the task
text) and the session `label` (the session's name, which can reveal what you are
working on) are scrubbed.

The minimal tier keeps everything a timeline needs while omitting what reveals
*what* you are doing (the raw path, the tool a prompt was for). The project
abbreviation is resolved from the cwd **before** the cwd is scrubbed, so a
minimal log still labels each event by project.

## `Event`

```jsonc
{
  "ts": "2026-06-26T14:32:07.412Z",  // RFC3339; the event instant
  "type": "transition",              // see types below
  "session_id": "ce13c0f2-…",        // agent session UUID; the stable join key (absent until the first hook)
  "pid": 4821,                       // OS pid (always set in practice)
  "agent": "claude",                 // claude | codex
  "project": "sb",                   // project abbreviation (resolved from cwd)

  // transition payload:
  "from": "permission", "to": "working",
  "rule": "case9-approve-toolmatch", // statustune rule id, when a rule decided it
  "subagents": 0,                    // S: subagents in flight at the edge
  "dur_prev_ms": 45000,              // how long `from` was held — the interval this edge closes

  // subagent payload (subagent_spawn / subagent_stop):
  "tool_use_id": "toolu_abc",        // links a spawn to its stop
  "agent_type": "Explore",           // the subagent kind

  // usage payload (usage_sample) — tokens accrued since the previous sample:
  "tok_in": 4200, "tok_out": 1850,
  "tok_cache_read": 920000, "tok_cache_create": 15000,
  "model": "claude-opus-4-8",        // the model the tokens were spent on (priced by CostUSD); minimal-safe

  // session-label payload (session_label):
  "label": "sb-invest",              // the session's current name (full tier — scrubbed at minimal)

  // full tier only:
  "cwd": "/home/u/Projects/switchboard",
  "pending": "AskUserQuestion",      // the tool a permission prompt was for
  "reason": "tool-name match: AskUserQuestion",
  "description": "map the auth code" // a subagent's task description
}
```

All fields except `ts`/`type` are `omitempty`, so a line stays small and a reader
tolerates older/newer shapes. **Group a session's events by `session_id`** when
present (stable across PID reuse), falling back to `pid` for the pre-hook
`session_start` (which has no id yet). A `session_start` for a pid whose lane is
still open (no intervening `session_end`) is a daemon-restart rediscovery — the
process never died — and continues the existing lane rather than starting a new
one; genuine pid reuse is preceded by a `session_end`.

### Event types

| `type` | When | Key payload |
|--------|------|-------------|
| `session_start` | a `claude`/`codex` process is discovered | `pid`, `agent` (no `session_id` yet) |
| `transition` | any status edge — hook-driven *or* reconciler self-heal | `from`, `to`, `dur_prev_ms`, `rule`, `subagents` |
| `suspend` / `resume` | the process is Ctrl-Z'd / resumed | bounds a greyed-out span |
| `session_end` | the process dies | closes the last interval |
| `subagent_spawn` | a `Task`/`Agent` subagent is launched (seen in the main transcript) | `tool_use_id`, `agent_type`, `description` |
| `subagent_stop` | that subagent's result lands | `tool_use_id`, `agent_type` |
| `usage_sample` | tokens accrued since the last sample (each reconcile tick), one per model | `tok_in`, `tok_out`, `tok_cache_read`, `tok_cache_create`, `model` |
| `session_label` | the session's name/label changed | `session_id`, `label` (full tier) |
| `focus` | window focus moved to/away from an agent session (Hyprland) | `session_id` = focused agent session, empty = focus left all agent windows |
| `activity` | the user went idle / active (global, session-less; from an idle daemon) | `to` = `idle` \| `active` |

Fanout (`subagent_*`) is derived by diffing the main transcript's `Task` tool_use
↔ `tool_result` pairing across reconcile ticks; no extra hook is required.
Usage is sampled incrementally from the transcript's per-message `usage` blocks,
primed at discovery so a pre-existing transcript's backlog is not double-counted.

**Every** status transition is captured: the frequent hook edges
(`UserPromptSubmit`→working, `Stop`→idle, `PermissionRequest`→permission) and the
hookless reconciler edges (permission self-heal, delegating promotion,
interrupt/resume recovery) all funnel one `transition` event apiece.

## Cost (pricing)

There is **no native source for dollar cost** on a solo Pro/Max subscription, so
cost is recomputed from `tokens × per-model price`, keyed on the `usage_sample`'s
`model`. The single price table and the recompute function live in
`internal/history/pricing.go`:

```go
// internal/history/pricing.go — the exact exported contract consumers depend on.
func CostUSD(model string, tokIn, tokOut, cacheRead, cacheCreate int64) float64
```

- **Token params are `int64`** (matching `transcript.Usage` / `Event.Tok*`).
- **Returns dollars** (a float): Σ over the four token buckets of
  `tokens × perMTokRate / 1e6`.
- **Unknown / unpriced model → `0`** (never panics), so a foreign model
  contributes no cost rather than a wrong one.
- **Model matching is robust** to real transcript model ids
  (`claude-opus-4-8`, `claude-opus-4-8[1m]`, `claude-sonnet-4-6`,
  `claude-haiku-4-5-20251001`, `claude-fable-5`): the id is normalized (a `[…]`
  context-window suffix and a trailing `-YYYYMMDD` date are stripped) and matched
  on the family substring.

The `prices` table — dollars **per million tokens** (per MTok), confirmed against
the `claude-api` reference (Anthropic public pricing); `cacheCreate` is the
5-minute cache-write rate (1.25× input — small drift vs the 1h rate is accepted):

| family (key) | model ids | input | output | cacheRead | cacheCreate |
|------|-----------|------:|-------:|----------:|------------:|
| `opus` | `claude-opus-4-8` / `-4-7` / `-4-6` | 5 | 25 | 0.5 | 6.25 |
| `sonnet` | `claude-sonnet-4-6` | 3 | 15 | 0.3 | 3.75 |
| `haiku` | `claude-haiku-4-5` | 1 | 5 | 0.1 | 1.25 |
| `fable` | `claude-fable-5` | 10 | 50 | 1 | 12.5 |

Consumers (timeline derivation, plan-window, dashboard) call `CostUSD` rather
than duplicating the table; `cost_usd` fields in `timeline --json` are **float
dollars**.

## Deriving the timeline (what a consumer does)

A session's consecutive `transition` events bound **intervals**
`(status, t_start, t_end)` — a colored **swimlane** over time. Stack swimlanes by
session for the parallel-sessions timeline. `dur_prev_ms` pre-computes each
closed interval's length (also recoverable from adjacent `ts`). Every summary
stat — idle time, permission-wait time, hours of agent attention — is an
aggregation over these intervals. See `internal/history` (`BuildSwimlanes`,
`Summarize`, `AggregateTotals`) and `switchboard-ctl timeline`.

`switchboard-ctl timeline [--day D | --since D --until D] [--json]` renders the
swimlanes as colored bars plus a summary: per-status totals, the three
**attention** figures, subagents launched, and tokens used. The three attention
definitions (all reported) are:

- **A — union wall-clock**: time ≥1 session was active (working/delegating).
- **B — per-session sum**: Σ over sessions of active time (rewards parallelism).
- **C — fanout-weighted**: Σ active time × (1 + subagents) — the total agent
  compute directed, counting teammates (approximate; subagents sampled at each
  opening transition).

### `--json` contract (v2)

`--json` emits the stable envelope a GUI dashboard consumes. **Durations are
nanoseconds** (Go `time.Duration`), token counts are raw, and `cost_usd` /
`delegation_effectiveness` are floats. Every v2 field is `omitempty` — purely
additive to the original `{window, lanes, summary, totals}`:

```jsonc
{
  "window": "2026-06-26",
  "lanes": [{
    "session_id": "…", "pid": 4821, "agent": "claude", "project": "sb",
    "name": "sb-invest",                                                     // one canonical display name: the /name slug wins over the auto title
    "start": "…", "end": "…",
    "intervals": [{ "status": "working", "start": "…", "end": "…", "subagents": 0 }],
    "labels":    [{ "label": "sb-invest", "start": "…", "end": "…" }],       // name over time (A1)
    "subagents": [{ "agent_type": "Explore", "tool_use_id": "…",
                    "description": "…", "start": "…", "end": "…" }],         // launched subagents (A3)
    "focus":     [{ "start": "…", "end": "…" }],                            // this session held OS focus (C1)
    "cost_usd": 10.5, "tok_in": 2000000, "tok_out": 100000,                 // per-lane usage + recomputed cost (A2)
    "tok_cache_read": 0, "tok_cache_create": 0
  }],
  "summary": {
    "from": "…", "to": "…", "sessions": 1,
    "by_status": { "working": 600000000000 },
    "attention_union": 0, "attention_per_session": 0, "attention_fanout": 0,
    "prompt_active": 240000000000,     // focused-on-an-agent ∧ user-active
    "attended_active": 240000000000,   // agent-active ∧ (focused ∧ user-active) — supervising
    "delegated_active": 360000000000,  // agent-active ∧ ¬(focused ∧ user-active) — true delegation
    "delegation_effectiveness": 0.6    // delegated / (delegated + attended), in [0,1]
  },
  "totals": { "tok_in": 2000000, "tok_out": 100000, "tok_cache_read": 0,
              "tok_cache_create": 0, "subagents": 1, "cost_usd": 10.5 },
  "activity": [{ "state": "active", "start": "…", "end": "…" },             // global idle/active timeline (C2)
               { "state": "idle",   "start": "…", "end": "…" }],
  "plan_window": { "hours": 5, "from": "…", "to": "…", "cost_usd": 10.5,    // only with --plan-window (A4)
                   "tok_in": 0, "tok_out": 0, "tok_cache_read": 0, "tok_cache_create": 0 }
}
```

**Delegation (C3)** splits agent-active time by whether you were *attending* it —
focused on that session while active at the keyboard. It needs the `focus` stream
(Hyprland) **and** the `activity` stream (an idle daemon, e.g. hypridle); with
neither, all agent-active time reads as delegated and effectiveness is 1 (or 0
when there is no agent-active time — never a divide by zero).

**`activity[]`** is the top-level global idle/active timeline (both states,
alternating, tiling the window), surfaced for the dashboard's idle-dimming and
focus∧active overlay. Absent when there are no `activity` events in range.

**`plan_window`** (only with `--plan-window`) is a rolling `[now-5h, now]`
cost/token total — the self-computed dollar half of the dashboard's plan gauge;
the official utilization **%** comes from a separate cached file the dashboard
reads, never from this producer.

## Inspecting / managing it

```
switchboard-ctl history path                       # print the directory
switchboard-ctl history tail [--day D] [-n N]       # most recent events (--json for raw)
switchboard-ctl history stat                        # event counts, size, date range
switchboard-ctl history purge --before YYYY-MM-DD   # delete old day-files
switchboard-ctl history purge --all                 # delete everything
```

All read the files directly — no daemon connection — so they work whether or not
the daemon is running, and `jq`/`grep`/`tail -f` work on the raw `.jsonl` too.

## Stability

The format is **additive**: new event types and new optional fields may appear;
consumers must ignore unknown fields and tolerate missing optional ones. `ts` and
`type` are the only guaranteed-present fields.
