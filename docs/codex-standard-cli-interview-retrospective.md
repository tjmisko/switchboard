# Retrospective — interview detection targeted the wrong Codex launch contract

> Date: 2026-08-21
>
> Decision: Switchboard must work when the user launches the standard `codex`
> command. Requiring `switchboard-codex`, a private per-TUI app-server, a shell
> alias, or another launcher is not an acceptable solution.
>
> Status (updated 2026-08-22): the item-based app-server change is not a fix for
> the reported standard-Codex symptom. The standard path is now fixed separately
> with content-free Codex hooks: `request_user_input` opens on `PreToolUse` and
> resolves by exact `tool_use_id` on `PostToolUse` (or by the turn's `Stop`).
> Restart recovery for an already-open hook-only interview remains unknown.

## Reported symptom

The Codex TUI was displaying an interview with unanswered questions, but
Switchboard showed the session as active/green. The desired behavior is red for
the full interval in which Codex is waiting for an interview response.

The important deployment fact is that the TUI was launched as ordinary
`codex`. The eventual recommendation to relaunch it through
`switchboard-codex` changed that requirement instead of solving it.

## What was implemented

The attempted change augmented the Codex app-server graph with three attention
signals:

1. the existing `waitingOnUserInput` thread flag;
2. an in-progress `dynamicToolCall` named `request_user_input` in a
   `thread/read(includeTurns=true)` snapshot or item notification;
3. an `item/tool/requestUserInput` server request, correlated with
   `serverRequest/resolved` and bounded by item/turn completion.

That is a coherent reducer inside an attached app-server observer. Its tests
show that empty active flags cannot clear a separately observed pending input
item. Those tests do not show that an observer exists or receives those events
for a standard `codex` process.

## The end-to-end path that was missed

In the architecture being edited, the new signals are reachable only after a
launcher registers a private endpoint:

```text
switchboard-codex
  -> SWITCHBOARD_SLOT_ID
  -> codex_slot_register(endpoint)
  -> SlotObserver creates app-server proxy
  -> thread/item/request events reach graphState
  -> item-based interview detector can produce RED
```

The required launch path does not have those edges:

```text
plain codex
  -> no slot id
  -> no registered private endpoint
  -> SlotObserver returns no app-server graph
  -> status is hook-only (when hooks bind successfully)
  -> item-based interview detector is never called
```

The repository already said this explicitly: Codex processes not started
through `switchboard-codex` remain hook-only. That statement should have been
treated as an architecture stop, not as a deployment footnote.

## Where the reasoning failed

### 1. The operating constraint was not established first

The first question should have been: “Must this work with unmodified, standard
`codex`?” Instead, the investigation selected the richest available protocol
signal and only later revealed that consuming it required a different launcher.

A solution that requires changing the user's launch command is a product
decision, not an implementation detail. It needed explicit agreement before
code was changed.

### 2. Protocol expressiveness was confused with transport reachability

The generated 0.149 schema proves that Codex defines
`waitingOnUserInput`, `dynamicToolCall`, `item/tool/requestUserInput`, and
`serverRequest/resolved`. It does not prove that Switchboard can subscribe to
those messages for a standard TUI.

The implementation answered “can this payload be reduced correctly?” while
the blocking question was “can this payload reach the process we are building
for?”

### 3. Schema-derived fixtures were treated as live behavioral evidence

The existing evidence report explicitly says that no live app-server capture
was obtained and that the wait fixtures are schema-derived. During this task,
an attempted connection through the default app-server proxy returned no
events. That should have lowered confidence and stopped implementation until a
reachable standard-Codex signal was characterized.

Instead, synthetic snapshots were added showing an in-progress
`request_user_input` item. They validate parser behavior for that hypothetical
input, not the presence, ordering, or retention of such an item in the reported
session.

### 4. Unit coverage was mistaken for product coverage

The new tests construct `rpcThread`, `rpcItem`, and RPC notification values
directly. They prove the in-memory latch and clear rules. No test launches or
models the required path:

```text
standard codex -> hook/protocol transport -> Switchboard root -> red chip
```

The missing acceptance test made it possible for every new test to pass while
the user's launch mode could never execute the code.

### 5. The handoff quietly changed the solution

The rebuild instructions ended with “new Codex terminals should then be
launched with `switchboard-codex`.” That was the clearest evidence of the scope
error: the proposed operational step worked around the mismatch by replacing
the user's launcher.

The correct handoff should have said that the implementation was not reachable
from plain `codex` and was therefore not ready to deploy.

## What should have been investigated instead

The next investigation must begin on a standard `codex` process and retain only
content-free metadata.

### Gate 1 — characterize hooks during an interview

Capture the ordered hook metadata for one interview without retaining question
text, answers, or tool arguments:

- event name;
- normalized tool name;
- opaque session id;
- whether the hook subprocess ancestry reaches the visible Codex PID;
- monotonic timestamp.

The key questions are:

1. Does `PreToolUse` fire before the interview becomes visible?
2. Is its tool name `request_user_input`, `functions.request_user_input`, or
   something else?
3. Does `PostToolUse` fire after an answer?
4. What fires when the interview is interrupted or dismissed?
5. Are those hooks still owned by the interactive TUI when no shared app-server
   daemon is enabled?

The verified hook contract provides the required onset and resolution edges.
The implemented design is a hook-owned pending-input latch for plain Codex:
`request_user_input` is the narrow exception to generic `PreToolUse` activity,
and opaque `tool_use_id` correlation prevents another call's completion from
clearing the wait. Turn `Stop` handles interruption, and conversation rotation
clears the retired thread without relying on cwd or content.

### Gate 2 — find a recovery source

Hooks are edge-triggered, so they cannot alone reconstruct an interview that
was already open when Switchboard restarted or missed the onset event. Before
calling a hook latch complete, determine whether the exact rollout identified
by the hook exposes a content-free pending tool-call state while the interview
is open. Do not infer ownership from cwd, newest-file order, or timestamps.

If rollout state cannot recover it, the limitation must be explicit: hook-speed
detection works only after an observed onset edge, and restart recovery remains
unknown rather than confidently green.

### Gate 3 — only then consider terminal UI evidence

Terminal title or pane-content inspection is a last-resort degraded signal. It
may be useful if standard Codex exposes no structured passive source, but it
must be characterized for configurability, localization, stale frames,
suspension, and multiple same-cwd sessions. A visual string match must never be
quietly described as authoritative protocol state.

## Acceptance criteria for a workable fix

A replacement is not complete until all of these pass without a launcher
wrapper:

- launching `codex` directly creates the observed root;
- opening an interview changes that exact root to user-input/red promptly;
- an empty or generic active event cannot clear the wait;
- answering clears red promptly to the actual subsequent runtime state;
- interrupting or dismissing the interview does not leave a long-lived false
  red;
- two standard Codex TUIs in the same cwd cannot affect each other's status;
- a missed event or Switchboard restart fails unknown unless pending state can
  be reconstructed exactly;
- no prompt, answer, tool arguments, cwd, or transcript content is persisted in
  diagnostics or fixtures.

At least one test must exercise the standard-Codex ingestion boundary rather
than constructing an app-server graph directly.

## Disposition of the attempted change

The item/request reducer may be useful if private per-TUI endpoints ever become
an accepted optional mode. It should not be merged or advertised as resolving
standard-Codex interview detection on the strength of its current tests.

This retrospective does not itself revert the experimental worktree changes.
Their disposition should be an explicit follow-up: revert them, or retain them
only with clear wrapper-only scope and separate tests. The standard-Codex fix
must be developed from the evidence gates above.

## Related documentation

- [Shared app-server attribution incident](codex-app-server-hook-attribution-incident.md)
- [Codex status investigation](codex-investigation.md)
- [Codex evidence report](codex-session-status/evidence-report.md)
- [Status color state model](status-color-state-model.md)
