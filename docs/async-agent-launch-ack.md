# The launch ack: why four live agents rendered idle

## The complaint

A TaskPump session (`6016b3e9…`, 2026-08-14) had four background agents
working — `impl-g46-pump-state`, `impl-g44-adoption-docs`, `impl-review-gates`,
`impl-pump-workspace-root`, all with warm jsonl mtimes. The chip was ORANGE,
the dashboard's AGENTS ALOFT tab was empty, and the delegation metrics counted
the whole window as idle.

Switchboard was not failing to *see* the agents. It saw all four, emitted their
`subagent_spawn` events, and then killed them:

| agent | spawn | stop | actually still running at 00:13 |
|---|---|---|---|
| `ad95ee97` (g46-pump-state) | 00:02:44 | 00:03:10 | yes |
| `a53d910d` (g44-adoption-docs) | 00:03:00 | 00:03:10 | yes |
| `af780d02` (review-gates) | 00:03:09 | 00:03:10 | yes |
| `a8c6b09f` (pump-workspace-root) | 00:07:08 | 00:07:10 | yes |

Each stop landed on the first reconcile tick after the spawn. With every fanout
closed, `InFlightSubagents` was 0, the idle+S>0 ⇒ delegating rule never fired,
and the timeline recorded seconds-long slivers where there should have been
hour-long spans.

## The ack is not a result

The parent transcript answers an `Agent` tool_use ~2s later with a block that
is only an acknowledgement:

```
Async agent launched successfully. (This tool result is internal metadata — …)
agentId: a8c6b09ff2415df8c
The agent is working in the background. You will be notified automatically
when it completes. You know nothing about its results until that notification
arrives …
output_file: /tmp/claude-1000/<proj>/<sess>/tasks/<agentid>.output
```

It says outright that the agent is still running. The real completion never
arrives as a `tool_result` at all — it arrives later as a `<task-notification>`
user entry. `rg -c <tool_use_id>` over the parent transcript returns exactly 2:
the tool_use and this ack, nothing else, for the whole life of the agent.

`fanout.Observer.Reconcile` was recording that ack in `resultDone` and treating
it as the parent-`tool_result` completion.

## Why the existing guard never fired

A guard was already there, and it looked right:

```go
if !done && s.ToolUseID != "" && !ss.background[s.ToolUseID] && ss.resultDone[s.ToolUseID] {
    done = true
}
```

`ss.background` was fed from `input.run_in_background` on the Agent/Task
tool_use. Measured over the 120 most recently modified transcripts on this
machine (2026-08-14):

| Agent `tool_result` shape | `run_in_background` | count |
|---|---|---|
| `Spawned successfully. …` | absent | 49 |
| `Async agent launched successfully. …` | absent | 19 |
| `Spawned successfully. …` | explicit `false` | 1 |

**69 Agent spawns, 69 acks, zero real results, and not one with the flag set.**
The `Agent` tool does not take a `run_in_background` parameter — it is
asynchronous by construction, and its schema never carried the field. So the
`!ss.background[…]` term was constantly true and the guard was dead code from
the day it was written.

The unit test that was supposed to cover this (`TestReconcile_
backgroundIgnoresSpawnAckResult`) synthesized its spawn with
`run_in_background:true` — a shape that does not occur in any real transcript.
It passed while production burned.

Two lessons worth keeping:

- **A flag nothing sets makes a guard that never runs.** The failure is silent:
  no error, no log line, just a branch that is never taken. When a guard keys on
  a field, check the field's actual distribution in the corpus before trusting
  the guard.
- **A fixture that invents its input can only test the fixture.** The synthetic
  `run_in_background:true` spawn was internally consistent and completely
  unlike the thing it stood in for.

## The fix

Classify the ack where the raw facts are read, and let the Observer act on what
it proves:

1. `transcript.TasksSince` returns `[]TaskResult` (`ToolUseID`, `LaunchAck`)
   instead of a bare id list. `block.isLaunchAck()` flattens the result's
   content — both the bare-string and typed-block-array shapes — and prefix-
   matches it against `launchAckPrefixes`.
2. Both wordings are matched. `Spawned successfully` is not historical: it is
   the majority of the live corpus (49 of 69). `Async agent launched
   successfully` is the newer one.
3. The Observer treats an ack as proof the spawn is **asynchronous**: it sets
   `ss.background[toolUseID]` and never touches `resultDone`. Completion falls
   to the signals that work — the agent's own jsonl reaching `end_turn`
   (verified: finished background agents do end that way), or the stale cap.
4. Because the ack is what identifies an async spawn, `subagent_spawn` events
   are finally tagged `Background`. Before this, no ack-answered spawn ever
   was, so the history carried no `background` field at all.

`run_in_background` is retained only as a best-effort tag. It is documented in
`transcript.block` as **not** load-bearing, with the measured distribution
inline, so the next reader does not rebuild a guard on top of it.

## Ordering caveat

The `Background` tag depends on the ack being in the cursor delta by the time
the dir scan emits the spawn. The meta.json appears at spawn and the ack ~2s
later, so a reconcile tick landing inside that window emits the spawn untagged.
This is cosmetic: the completion logic is unaffected, because an ack never
enters `resultDone` no matter when it arrives.

## Blast radius, and repairing the log

Every `subagent_spawn`/`stop` pair switchboard has recorded for an `Agent`
fanout is suspect — the stop is the ack's arrival, not the agent's finish. The
detector fix does nothing for spans already on disk, so the history needs
repair too: [`scripts/repair-launch-ack-spans`](../scripts/repair-launch-ack-spans).

It re-dates each truncated stop against the subagent's own transcript, which
the bug never touched, and classifies by what that transcript shows —
`end_turn` ⇒ finished (re-date to it), quiet past the stale cap ⇒ abandoned
(re-date to last activity), still warm ⇒ **drop the stop entirely** so the
fixed daemon can close it for real. Dropping matters: a stop in the log makes
`history.PriorSubagentState` seed the agent as already closed, and no real stop
would ever follow.

Dry run by default; `--apply` backs up first; re-running is a no-op. Measured
on 2026-08-12 alone: sub-30s spans fell from 13 to 3 and recovered delegated
time rose from 11.97h to 16.01h — four hours of agent work that had been
erased from a single day.

Two things it deliberately does **not** do:

- **Rewrite `transition` events.** Those record what switchboard actually
  believed and displayed, and they are chained by `dur_prev_ms`; synthesizing
  `delegating` states would fabricate a record of what the operator saw.
  `--report-transitions` quantifies the gap instead (the session in the
  complaint above: 92.8 minutes logged idle with an agent working).
- **Touch today's file without `--include-today`.** The live daemon appends
  there and the rewrite is read-all/replace. Note the guard covers the
  *destination* as well as the source: an agent that spawned before midnight
  and finished after it re-dates into today's file even when you are repairing
  last week.

## Not just this repo

The same pairing bug lives wherever a Task/Agent tool_use is matched against
its tool_result:

- `transcript.Tasks` / `InFlightTasks` (legacy, no non-test callers) — fixed
  here by the same rule so a future caller does not inherit it.
- `agent-watcher`'s `in_flight_delegations` — fixed in that repo, which had no
  background handling at all. Note the trap documented there: its `collected`
  set also gates prompt resolution, where an ack genuinely *does* end the main
  thread's wait, so the two questions needed separate sets.
- `switchboard-dashboard` has no independent copy — its arachne provider
  consumes these history events and never parses transcripts — but its
  provider contract now states the obligation (§5.1).

