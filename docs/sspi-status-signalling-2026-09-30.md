# SSPI false-green root cause analysis — 2026-09-30

The live `sspi-data-webapp-57` session (PID 149407, Claude session
`8ac0abc0-0e3c-4b8d-ba95-9e82ac1df892`) finished its work but remained green.
Two independent Claude adapter defects explain the result. The renderer and
neutral reducer faithfully reflect the incorrect graph they receive.

## Evidence (local time, PDT)

- 15:23:19.649: captured `SubagentStop` for `a9b20b65e51f7a267`.
- 15:27:25.310: captured `SubagentStop` for `a196cd048262c7888`.
- 15:28:12.518: root final answer, with `message.stop_reason: end_turn`.
- 15:28:12.583: captured root `Stop`, with `background_tasks: []`.
  The transcript's stop-hook summary reports Switchboard's hook ran without
  errors; continuation was not prevented.
- 15:28:12.610: a transcript-only user entry reports the second task completed
  (`queueTranscriptOnly: true`, `queueSkipAttachments: true`).
- 15:28:12: daemon log changes `working -> delegating`, still counting two children.
- 15:28:13: daemon log changes `delegating -> working`, still counting two children.
- 15:29:12.624: captured `idle_prompt` notification; Claude's session file says
  `status: idle`.
- 15:45:18: live Switchboard graph still has root runtime `active` and both
  children runtime `active`, lifecycle `running`; compatibility status is
  `working`, with `status_since` 15:28:13.893.

Sources inspected: the session's root and child transcripts under
`~/.claude/projects/-home-tjmisko-Projects-sspi-data-webapp/`,
`~/.local/state/switchboard/hookcap/2026-09-30/hooks.jsonl`, the
`switchboard.service` journal, and `~/.cache/switchboard/state.json`.
Installed release and repository HEAD both identify release `20614a6`.

## 1. Completion notification falsely resumes the root

`internal/transcript/transcript.go:classify` treats every user message as
`SignalActivity` except interrupt and local-command records. It does not exclude
transcript-only task notifications. It also treats assistant messages as
activity regardless of `end_turn`, and ignores system stop summaries.

`internal/provider/claude/observer.go:reconcileRootRuntime` promotes idle to
active when that activity timestamp is newer than the Stop anchor. The task
notification at 15:28:12.610 follows the final answer and Stop. Consequently the
next observation undoes the Stop. The daemon's 15:28:13 transition matches this
path. Active-to-idle transcript recovery only recognizes an interrupt, so
neither the final-answer terminal reason nor the stop summary heals the latch.

## 2. Handback completion leaves both children running

Both child transcripts end with `SubagentHandback` tool calls and their successful
user `tool_result` responses, without an assistant `end_turn` terminal entry.
`internal/transcript/subagents.go:subagentJSONLState` recognizes completion only
when the last line has `message.stop_reason == end_turn`.

The fanout observer additionally checks parent tool results, but those checks do
not resolve these children. Its forward cursor does not consume the explicit
`<task-notification>` completion records. The Claude adapter's `SubagentStop`
branch only invalidates the snapshot; it records no child terminal transition.
Rescanning therefore reproduces the incorrect running state despite the stops.

The 30-minute quiet cap may eventually retire these children, but cannot clear
the independently latched active root. Correcting only the root would leave the
chip green as `delegating`; correcting only the children would leave it green as
`working`.

## Reproduction

A temporary Go replay called the existing `transcript.NewestSignal` and
`fanout.Observer.Observe` against the live files, with an isolated temporary
history directory and the observation clock set to screenshot time, 15:44:

```text
newest signal=activity at=2026-09-30 22:28:12.61 +0000 UTC err=<nil>
fanout in-flight=2 err=<nil>
child=a196cd048262c7888 lifecycle=running
child=a9b20b65e51f7a267 lifecycle=running
```

The replay did not mutate the daemon, session, or production history.

Replaying the fixed observer against those same live files, restoring the recorded
false-green `working` state and its 15:28:13.893 status anchor, now produces
`status=idle children-in-flight=0` at the screenshot observation time. This second
replay also used isolated history and left the running daemon and session alone.

## Repair requirements

The implementation now excludes transcript-only bookkeeping from conversational
activity, recognizes successful correlated handbacks and task completion
notifications, and records exact child start/stop hook edges. Later child activity
can reopen a completed child. On restart, the parent cursor recovers historical
notifications and launch acknowledgments once while history prevents duplicate
events.

After 90 seconds without a root hook, explicit final-answer or successful Stop
summary evidence can heal a missed stop. Later work or a prevented continuation
blocks the correction; silence alone does not change status. A concurrent newer
hook fences the transcript runtime merge. Regression tests reproduce both finished
children followed by a bookkeeping notification, child resumption, and the quiet
polling cases.

1. Distinguish transcript-only completion bookkeeping from work that actually
   starts a root turn. Keep real subagent messages that wake the root as activity.
2. Consume exact child completion evidence, including task notifications and
   child-stop hooks. Support subsequent resumption of the same agent ID rather
   than treating completion as permanent deletion.
3. Reconcile terminal root evidence so a falsely latched active state can heal.
   Preserve later genuine starts and live child work.
4. Add an incident replay regression: both children complete through handback,
   root stops, a transcript-only completion notification follows, and repeated
   observations must remain `idle`/orange. Cover same-ID child resumption too.

No production fix or daemon restart was performed during this analysis.
