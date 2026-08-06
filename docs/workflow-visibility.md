# Workflow visibility: seeing an ultracode run behind the chip

## The complaint

A session running an ultracode Workflow (`Workflow` tool, `/workflows` in the
CLI) showed ORANGE (idle) on the bar while twenty subagents were burning
1M+ tokens. The main thread's Stop hook fires the moment the workflow launches
in the background — from every signal switchboard consumed, the session had
simply gone idle.

## Why every existing signal missed it

Measured live against a 20-agent run (session `705a9077…`, Arachne,
2026-08-05), plus 16 historical runs across 5 projects:

- **Hooks.** Workflow subagents fire NO user-configured hooks — no
  PostToolUse, no SubagentStart/Stop. After the launch turn's Stop at 18:03,
  the only events switchboard received for the session were `focus` and
  `memory_sample`. Hook-driven status can never see a workflow.
- **The flat subagents dir.** `SubagentsForTranscript` scans
  `<session-dir>/subagents/` and skips subdirectories. Workflow agents live one
  level deeper — `subagents/workflows/wf_<runid>/` — so the fanout Observer
  counted zero in-flight subagents and the delegating (green) rule never fired.
- **The parent transcript.** The Workflow tool_use gets an immediate
  "launched in background" tool_result; nothing else lands in the parent file
  until the run completes (a `<task-notification>` user entry). The forward
  cursor sees no spawns, no results.

## What a run writes on disk (the source of truth)

```
<dir>/<session-id>/subagents/workflows/wf_<runid>/
    journal.jsonl            append-only ledger (below)
    agent-<id>.jsonl         each agent's own transcript; mtime = activity
    agent-<id>.meta.json     always {"agentType":"workflow-subagent","spawnDepth":1}
<dir>/<session-id>/workflows/scripts/<name>-wf_<runid>.js
                             the persisted script; filename carries the name
```

The journal has exactly two line shapes (all 17 recorded runs, 800+ lines):

```
{"type":"started","key":"v2:<hash>","agentId":"<id>"}
{"type":"result","key":"v2:<hash>","agentId":"<id>","result":<payload>}
```

Facts that shaped the design:

- **No timestamps in the journal.** Timing comes from file mtimes.
- **`result` is the ONLY per-agent completion signal.** An agent that returns
  via structured output ends its jsonl with a user tool_result line — not an
  assistant `end_turn` — so the flat-dir Done detection does not transfer.
- **Killed runs leave orphans forever.** Several historical journals sit at
  started > resulted permanently (worst: 31/12). "Started and not resulted"
  alone never proves in-flight; it must be bounded by the agent transcript's
  mtime (the Observer's stale cap).
- **No terminal event for the run itself.** Completion shows up only as the
  parent transcript's task-notification and quiescence of the run dir. (The
  `/tmp/claude-1000/<proj>/<sess>/tasks/<taskid>.output` file fills on
  completion, but /tmp is volatile — not load-bearing.)
- **`agents_started` is "so far", not a plan.** The journal records no total;
  the CLI's own "7/17 agents done" denominator also grows as the script fans
  out.

## The fix

`fanout.Observer.Reconcile` now runs a third pass, `reconcileWorkflowsLocked`:

1. `transcript.WorkflowRunsForTranscript` lists `wf_*` run dirs and resolves
   each run's name from its persisted script filename.
2. A per-run byte cursor (`transcript.WorkflowJournalSince`) tails the journal
   for `started`/`result` agent ids. In-flight = started − resulted −
   stale-closed; stale-closed uses the agent jsonl's mtime (journal mtime as
   fallback) against the same `staleCap` as flat fanouts, so a killed run
   drains instead of pinning the count.
3. Workflow agents are spawnDepth-1 children, so they join
   `InFlightSubagents` — the existing idle+S>0 ⇒ delegating rule turns the
   chip GREEN with no state-model change.
4. Wire: `AgentInfo.Workflows []WorkflowStatus` (run id, name, started, done,
   in-flight) summarizes each ACTIVE run; the waybar tooltip renders
   "workflow simplification-audit · 7/17 agents" instead of the bare
   delegating count. A run stays active while agents are in flight or its
   journal is fresher than `DefaultWorkflowQuietGrace` (bridging the
   between-waves instant where everything has resulted).
5. History: `workflow_start`/`workflow_stop` bracket each run (keyed
   `workflow_run_id`; name rides `Label`, scrubbed at the minimal tier), and
   the run's agents emit ordinary `subagent_spawn`/`stop` events tagged with
   `workflow_run_id` and AgentType `workflow-subagent`, so the timeline gets
   per-agent spans for free. Restart idempotence mirrors G1:
   `history.PriorWorkflowState` seeds the announced/ended sets, and the
   journal re-read from offset 0 is deduped by the seeded agent seen-sets.

## Limits / future work

- Per-agent labels (`review:bugs`) are not on disk anywhere; only the prompt
  (first user message of the agent's jsonl) could approximate them.
- Workflow agents' token usage (`message.usage` in each agent jsonl) is still
  invisible to `usage_sample`, which reads only the main transcript. Same
  watch-the-run-dir machinery could feed it.
- A killed run reads as delegating for up to `staleCap` (30 min) before its
  orphans force-close — the same conservative bound as a stalled flat fanout.
  A tighter kill signal would need the parent's task-notification or the /tmp
  task output file.
