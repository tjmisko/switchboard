# Codex Auto-review attention ownership

> Date: 2026-08-24
>
> Status: findings confirmed; Phase 1 implemented on 2026-08-24
>
> Scope: Codex app-server and hook approval signals, their projection into the
> provider-neutral agent graph, and the red attention color shown by
> Switchboard

## Summary

Switchboard must reserve red for a request that is known to require human
intervention. A Codex approval boundary is not sufficient evidence by itself:
when Auto-review is enabled, Codex routes the boundary to a reviewer agent and
continues without a person if the reviewer approves it.

The pre-change Codex observer implemented this ownership distinction, but its
500 ms classification window assumed that reviewer ownership evidence would
arrive promptly on the passive app-server stream. Live captures disproved that
assumption. Switchboard received an approval request and its later resolution,
but did not receive `thread/settings/updated`,
`item/autoApprovalReview/started`, or guardian evidence before the timer
expired. The timeout classified the unknown request as human-owned, producing
a false red interval that ended automatically.

The immediate mitigation is a 30-second grace period before an ambiguous
approval may become red. Explicit human input remains red immediately. The
long-term invariant is stronger: elapsed time must not be considered human
ownership evidence, and unknown ownership should remain green/reviewing or
neutral until a semantic signal identifies the user as the decision owner.

## User-facing contract

Color is an action semantic:

| Color | Meaning |
|---|---|
| Red | A human-owned request is outstanding; act now |
| Green | Work is happening, including internal permission review; do nothing |
| Orange | Work has stopped or completed; return when convenient |
| Gray | Switchboard cannot determine the state |

The normal-operation invariant is:

```text
red = exists(unresolved request where ownership evidence is human)
```

A red-to-green transition should therefore be preceded by resolution of a
request that was semantically classified as human-owned. Automatic review must
not create that edge. Process death and other terminal cleanup may remove a red
state, but should not fabricate a green working state.

## Codex protocol semantics

The official app-server protocol represents approvals as server-initiated
JSON-RPC requests. A typical command flow contains:

```text
item/started
item/commandExecution/requestApproval
serverRequest/resolved
item/completed
```

The request means that a decision is needed; it does not by itself identify
the decision maker. OpenAI's Auto-review documentation says that setting
`approvals_reviewer = "auto_review"` substitutes a separate reviewer agent for
the human reviewer. An approval-boundary method name is consequently
mechanical-gate evidence, not human-attention evidence.

Relevant official documentation:

- [Codex app-server](https://learn.chatgpt.com/docs/app-server)
- [Codex Auto-review](https://learn.chatgpt.com/docs/sandboxing/auto-review)
- [Codex hooks](https://learn.chatgpt.com/docs/hooks)

The locally generated Codex 0.149.1 app-server schema contains additional
ownership signals:

- `thread/settings/updated.threadSettings.approvalsReviewer`, with `user`,
  `auto_review`, and `guardian_subagent` routes;
- `item/autoApprovalReview/started` and
  `item/autoApprovalReview/completed` lifecycle notifications;
- a `decisionSource` field on review completion;
- `item/tool/requestUserInput.isBlocking` and `autoResolutionMs`.

The Auto-review lifecycle notifications are explicitly unstable. They are
useful corroborating evidence but cannot be the sole correctness boundary.
They also were not present in the passive stream for the incidents below.

The documented `PermissionRequest` hook payload has no reviewer-owner field.
A hook receipt can therefore open an ambiguous permission episode, but cannot
independently prove that the user must resolve it.

## Live evidence

The screenshot incident was correlated with the exact Switchboard session and
app-server thread. Its history contained:

```text
2026-08-24T20:16:53.282680609Z  active/none -> idle/approval
2026-08-24T20:17:19.141770396Z  idle/approval -> active/none
```

The false red interval lasted 25.859 seconds. Both observations came from the
Codex app-server source.

A controlled, harmless approval-boundary reproduction in another live Codex
session produced:

```text
2026-08-24T20:23:20.723Z  active/none -> idle/approval
2026-08-24T20:23:24.487Z  idle/approval -> active/none
```

That false red interval lasted 3.764 seconds. The sanitized rollout metadata
contained the tool call and tool result but no reviewer or Auto-review record.

Approval/red episodes observed in Switchboard history on 2026-08-24:

| Statistic | Value |
|---|---:|
| Episodes | 53 |
| Total visible red time | 373.069 s |
| Median | 3.839 s |
| p90 | 13.237 s |
| p95 | 15.603 s |
| Maximum | 45.945 s |
| At or below 30 seconds | 52 of 53 |

These episodes occurred on Auto-review-configured sessions and have the
automatic-resolution shape, but the missing resolution-actor field prevents
every historical episode from being treated as perfectly labeled ground
truth. The screenshot incident and controlled reproduction are confirmed
examples.

With the existing 500 ms window accounted for, a 30-second window would have
fully suppressed approximately 52 of 53 episodes and about 96% of visible red
time. It would have fully suppressed the screenshot incident.

Eight structured user-input waits observed on the same day lasted between
6.677 seconds and 621.358 seconds. Those are consistent with genuine human
waits and support keeping structured blocking input immediately red rather
than applying a global delay.

## Pre-change failure mechanism

`internal/provider/codex` retains mechanical `waitingOnApproval` and
`waitingOnUserInput` flags separately from graph attention. An approval request
starts as pending ownership. The observer attempts to classify it with:

1. the effective per-thread reviewer route;
2. Auto-review lifecycle events;
3. an active guardian-source child;
4. structured blocking and auto-resolution input metadata;
5. exact string or integer JSON-RPC request resolution.

Publication was suppressed while ownership was pending. The problem occurred
when the timer expired: every pending request was converted to `requestHuman`.
`deriveNode` then correctly projected that synthetic human owner as approval
attention. The graph reducer and renderer were behaving as designed; the
classification timeout supplied false ownership evidence.

The pre-change test suite validated review-first and request-first ordering,
review outcomes, exact resolution IDs, guardian snapshots, reconnect cleanup,
and mixed automatic/human requests. It did not model the observed production
stream:

```text
approval request -> no ownership notification -> automatic resolution
```

The hook fallback also mapped every generic `PermissionRequest` directly to
approval attention. Because its payload cannot distinguish reviewer ownership,
this path could recreate the same false red-to-green edge even after the
app-server classifier was corrected.

## Accepted implementation

Implementation status: the app-server observer and standard-CLI hook fallback
now apply the 30-second grace described below. Both paths emit content-free,
ephemerally correlated `wait_episode` records. Tests cover exact resolution
inside the grace, timeout fallback, late automatic evidence, immediate
structured user input, hook/app-server reconciliation, and timer races. The
timeout-to-human fallback remains intentionally enabled pending rollout data;
Phase 2 is not implemented.

### Phase 1: bounded ambiguous-approval grace

1. Increase the default ownership-classification window from 500 ms to
   30 seconds.
2. Apply the delay only to mechanically ambiguous approval requests and raw
   unowned gates.
3. Keep these events immediate:
   - blocking `item/tool/requestUserInput` with no auto-resolution;
   - `mcpServer/elicitation/request`;
   - approval requests whose effective reviewer is explicitly `user`;
   - exact standard-CLI user-input hooks.
4. Keep known Auto-review and guardian-owned requests non-red for their entire
   duration.
5. Retain the timeout-to-human fallback during the instrumented first rollout,
   so missing reviewer settings cannot hide a real approval indefinitely.

This is asymmetric temporal hysteresis: red entry for uncertain ownership is
delayed, while resolution and red exit remain immediate. It delays only the
Switchboard alert, not Codex execution.

### Phase 2: evidence-gated red

Once event coverage is measured, stop treating timeout as human evidence.
When ownership remains unknown after 30 seconds, project a nonurgent
`review_pending` or unknown state. If no dedicated visual state is introduced,
retain the prior green state while work/review activity is known and otherwise
use gray. Never use red without explicit human evidence.

Seed reviewer ownership at observer attachment when a narrow, supported source
becomes available. A full configuration payload must not be logged. A
`thread/read` snapshot that includes current `approvalsReviewer` would be the
preferred upstream contract because a settings notification can precede
Switchboard's attachment.

## Content-free diagnostics

Diagnostics must describe complete wait episodes rather than only the final
classification callback. They must not record commands, prompts, rationales,
tool inputs, cwd, transcript text, or configuration content.

Each episode has an ephemeral correlation ID and emits finite-label lifecycle
events in the following shape:

```text
category=wait_episode event=started
category=wait_episode event=evidence
category=wait_episode event=red_published
category=wait_episode event=red_cleared
category=wait_episode event=resolved
```

Implemented fields:

- observer-local episode ID and provider source;
- request kind: command, file, permissions, legacy approval, MCP, user input,
  raw gate, or hook permission;
- ownership: unknown, human, automatic, or ignored;
- ownership evidence: reviewer setting, Auto-review event, guardian,
  structured input, timeout fallback, exact resolution, or hook progress;
- classification or episode duration;
- whether red was published and its visible duration;
- whether the former 500 ms policy would already have published red;
- whether a red state cleared without explicit human-ownership evidence.

Recommended follow-up fields and aggregates:

- connection generation;
- resolution source: exact request, turn/thread completion, snapshot omission,
  reconnect, or process cleanup;
- approval requests seen;
- reviewer-setting and Auto-review events seen;
- requests resolved before classification;
- requests promoted by timeout;
- red publications caused by timeout;
- red clears without semantic human evidence;
- unmatched request/review IDs and target item IDs;
- classification and episode duration histograms with 0.5, 1, 2, 5, 10, 30,
  and 60 second buckets.

The most important rollout invariant is:

```text
red_cleared_without_human_evidence = 0
```

During Phase 1, timeout-promoted episodes are reported separately so the
remaining uncertainty is visible rather than mislabeled as semantic human
ownership.

## Regression and replay coverage

Add deterministic fake-clock or bounded-time tests for:

1. `requestApproval -> serverRequest/resolved` without Auto-review events,
   resolving before 30 seconds and never publishing red;
2. ownership evidence arriving at 100 ms, 5 seconds, 29.9 seconds, 30.1
   seconds, and 60 seconds;
3. explicit `approvalsReviewer=user` producing immediate red;
4. hook-only `PermissionRequest` entering ambiguous classification rather than
   immediate approval attention;
5. blocking non-auto-resolving user input producing immediate red;
6. auto-resolving or nonblocking input never producing red;
7. reconnect, authoritative snapshot replacement, and turn completion during
   classification;
8. concurrent automatic approval and human user input;
9. exact string and integer request resolution IDs;
10. multiple simultaneous approval requests with independent resolution.

Add a property-style invariant around graph publication:

```text
every published approval attention node has semantic human evidence
or is explicitly labeled timeout_fallback during Phase 1
```

The live end-to-end probe is a harmless approval-boundary request under
Auto-review while a passive observer records method counts and episode labels.
Success means the request resolves without a red history edge and without
capturing request content.

## Rollout

1. Ship episode diagnostics and record `old_would_publish_red` beside the new
   projection, preserving a shadow comparison with the former 500 ms policy.
2. Enable the 30-second ambiguous-approval grace while leaving structured
   human input immediate.
3. Compare old/new red decisions for at least one normal workday.
4. Review delayed genuine prompts, timeout promotions, missing event classes,
   and red-clear invariant violations.
5. Run the instrumented policy for one week before removing timeout-to-human.
6. Move to evidence-gated red once reviewer ownership is available reliably or
   the measured cost of unknown ownership justifies the nonurgent fallback.

## Desired upstream protocol support

The clean protocol contract would provide:

- stable `requiresUserAction` or `decisionOwner` on every approval request;
- current `approvalsReviewer` in `thread/read` snapshots;
- stable Auto-review lifecycle events delivered to passive subscribed clients;
- a stable resolution actor/source on `serverRequest/resolved`;
- documentation of which server requests are client-scoped versus broadcast.

Until then, Switchboard should combine all available ownership evidence, keep
unstable signals supplementary, and treat missing evidence as uncertainty
rather than proof that the user is needed.
