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

One **append-only JSON-per-line** file per **UTC day**. State, not cache:
losing a day is permanent (unlike `state.json`, which the daemon rebuilds from
`/proc`). Override the directory with the daemon's `-history-dir` flag.

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
| `minimal` (default) | `ts`, `type`, `session_id`, `pid`, `agent`, `project` (abbrev), `from`/`to`, `rule`, `subagents`, `dur_prev_ms`, `agent_type`, `tool_use_id`, token counts | `cwd`, `pending`, `reason`, `description` |
| `full` | everything above **plus** `cwd`, `pending`, `reason`, `description` | — |

The subagent `agent_type` (e.g. `Explore`) and token counts are kept at the
minimal tier — they describe *how much* and *what kind*, not the content of your
work; the subagent `description` (the task text) is scrubbed.

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
`session_start` (which has no id yet).

### Event types

| `type` | When | Key payload |
|--------|------|-------------|
| `session_start` | a `claude`/`codex` process is discovered | `pid`, `agent` (no `session_id` yet) |
| `transition` | any status edge — hook-driven *or* reconciler self-heal | `from`, `to`, `dur_prev_ms`, `rule`, `subagents` |
| `suspend` / `resume` | the process is Ctrl-Z'd / resumed | bounds a greyed-out span |
| `session_end` | the process dies | closes the last interval |
| `subagent_spawn` | a `Task`/`Agent` subagent is launched (seen in the main transcript) | `tool_use_id`, `agent_type`, `description` |
| `subagent_stop` | that subagent's result lands | `tool_use_id`, `agent_type` |
| `usage_sample` | tokens accrued since the last sample (each reconcile tick) | `tok_in`, `tok_out`, `tok_cache_read`, `tok_cache_create` |

Fanout (`subagent_*`) is derived by diffing the main transcript's `Task` tool_use
↔ `tool_result` pairing across reconcile ticks; no extra hook is required.
Usage is sampled incrementally from the transcript's per-message `usage` blocks,
primed at discovery so a pre-existing transcript's backlog is not double-counted.

**Every** status transition is captured: the frequent hook edges
(`UserPromptSubmit`→working, `Stop`→idle, `PermissionRequest`→permission) and the
hookless reconciler edges (permission self-heal, delegating promotion,
interrupt/resume recovery) all funnel one `transition` event apiece.

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

`--json` emits `{window, lanes, summary, totals}` — the stable contract a GUI
dashboard would consume. Durations in the JSON are **nanoseconds** (Go
`time.Duration`); token fields are raw counts.

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
