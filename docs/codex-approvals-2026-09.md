# Codex questions and approval detection, September 2026

Investigation date: 2026-09-30. Installed CLI: **0.159.2**. This extends the
[August investigation](codex-auto-review-attention.md), which examined 0.149.1.
This is a research report; it does not change runtime behavior.

Implementation follow-up: the user selected red for every outstanding input,
including nonblocking questions. The implementation now detects structured
async question items and hooks, preserves input attention during progress and
same-connection snapshots, and gives fresh Codex input/approval attention precedence
over herdr. Async questions use the provisional next-user-submission dismissal
rule; automatic approval classification remains unchanged. Findings below
describe the pre-implementation state.

## Findings

Robust detection requires two independent facts: **a question needs attention**
and **execution is blocked on a human decision**. Recent Codex can ask a
structured question while continuing work. Switchboard currently misses that
question path, retains a deprecated blocking test, guesses approval ownership
after 30 seconds, and can hide correct provider attention behind herdr's status.

The recommended rule is:

```text
red = outstanding human question OR outstanding human-owned approval
paused_for_human = outstanding blocking human question OR human approval wait
```

An asynchronous question should turn red under the user's requested convention,
while its runtime remains active. Internal Auto-review should carry no human
attention. Unknown ownership should remain unknown.

## What changed recently, and what is directly verified

The [official changelog](https://learn.chatgpt.com/docs/changelog) records:

- September 8: mobile support for answering live questions while Codex continues
  work. This dates a client feature, not the original protocol introduction.
- September 17, CLI 0.155.0: automatic review improvements, including retries and
  separation of review failures from unsafe-action findings.
- September 25, CLI 0.157.0: automatic background-server startup for eligible
  interactive sessions.
- September 29, CLI 0.159.0: opt-in interruption of model responses and code-mode
  calls by new input. The installed 0.159.2 is a subsequent patch release.

I generated experimental TypeScript bindings from the installed executable:

```sh
codex --version
codex app-server generate-ts --experimental --out /tmp/codex-protocol-0.159.2
```

These bindings establish the current contracts below. They do **not** establish
the precise release that introduced each field, nor delivery to a passive
observer. No new live approval/question was triggered in this investigation.

### Blocking questions

`v2/ToolRequestUserInputParams.ts` includes `threadId`, `turnId`, `itemId`,
`questions`, `isBlocking: boolean`, and `autoResolutionMs: number | null`.
Its generated comment explicitly deprecates `autoResolutionMs` and directs
clients to use `isBlocking` for blocking behavior.

Switchboard's `internal/provider/codex/observer.go:1195` currently requires
`isBlocking && autoResolutionMs == nil` to identify a human wait. A current
payload with `isBlocking=true` and a non-null legacy timeout is therefore
suppressed. The existing `wait_ownership_test.go` explicitly expects that
suppression, so passing tests preserve an outdated assumption.

Use an optional boolean in compatibility decoding. An absent field from an
older server must not silently become the current protocol's explicit `false`.
Handle older schemas in a versioned adapter rather than mixing their timeout
semantics into the current rule.

The [app-server documentation](https://learn.chatgpt.com/docs/app-server#approvals)
describes exact request resolution and also notes that app-tool approvals may
use the user-input request channel. Therefore the channel alone does not always
distinguish a clarification question from an approval; either requires human
attention when blocking.

### Asynchronous questions

The installed `v2/ThreadItem.ts` contains this assistant-message variant
(irrelevant fields omitted):

```ts
{ type: "agentMessage", id: string,
  delivery: AgentMessageDelivery | null,
  questions: Array<AsyncUserInputQuestion> | null }
// AgentMessageDelivery = "async"
// AsyncUserInputQuestion = { title: string, options: Array<string> | null }
```

Switchboard's `rpcItem` has neither `delivery` nor `questions`. Item events are
used for collaboration state; assistant-message questions are not detected.
The hook matcher recognizes `request_user_input` and `AskUserQuestion`, but
does not recognize `request_user_input_async`.

This gives a structured way to detect a question's arrival without scanning
prose. It does not, by itself, provide an outstanding-question snapshot or an
answer-correlation contract. The generated async question has no request ID or
answered-state field. Do not mark every historical message with questions as
an unanswered question. Also, an async tool's `PostToolUse` means the tool
returned; it does not establish that the user answered.

A live capture must determine the answer/cancellation signal before adding a
durable asynchronous-question latch. If none is exposed, reliable onset
detection is possible but exact pending/answered state requires upstream
support. Clearing on any user prompt would be a documented heuristic.

An ordinary prose question in a completed assistant response has no equivalent
machine-readable question flag. A question mark, idle turn, or `Stop` message
does not establish a pending human request. Exact detection requires the agent
to use structured questions or a client-provided needs-response signal; prose
classification can only be a separately labeled heuristic.

### Approval ownership

[Auto-review](https://learn.chatgpt.com/docs/sandboxing/auto-review) routes
eligible approval gates to a reviewer agent. A denial can lead to a safer retry
or an assistant question; it does not itself prove a human approval modal is
open. Review failure, timeout, and denial are distinct outcomes. Computer Use
also has direct user-facing app approvals.

The installed schema still has:

- `thread/settings/updated.threadSettings.approvalsReviewer`, with `user`,
  `auto_review`, and `guardian_subagent` values;
- `item/autoApprovalReview/started` and `/completed`, still explicitly unstable;
- review status `inProgress | approved | denied | timedOut | aborted`;
- completion `decisionSource`, currently only `agent`;
- `autoApprovalReview/strictReviewRequired`, carrying thread/turn/timestamp,
  without an explicit human-action flag;
- `serverRequest/resolved`, containing only `threadId` and `requestId`.

Neither a review denial nor `strictReviewRequired` should be interpreted as a
human wait without verifying its runtime meaning. A resolution may also mean
cancellation or turn cleanup; it does not identify who answered.

Current approval requests have timestamps, and command requests may include
`approvalId`. Multiple callbacks can share one `itemId`, including shell bridge
and stdin approvals. Network review events can have a null `targetItemId`.
Retain exact JSON-RPC request IDs as the primary resolution key; use review and
approval IDs for secondary correlation, never collapse requests by item alone.
Switchboard already preserves string versus integer RPC IDs and handles exact
resolution; preserve that work.

## Concrete gaps in Switchboard

| Finding | Location | Consequence |
| --- | --- | --- |
| Deprecated timeout also controls blocking | `internal/provider/codex/observer.go`, user-input branch | Some blocking questions can stay non-red |
| Async question fields and tool name absent | `internal/provider/codex/protocol.go:rpcItem`; `cmd/switchboard/codex_hook_transitions.go:isCodexUserInputTool` | Questions asked during work are missed |
| Timeout promotes unknown owner to human | `protocol.go:expireClassification`; hook approval timer | Slow automatic review can become falsely red; genuine unknown-owner approvals are delayed |
| Broad automatic evidence | `protocol.go:hasAutoEvidence`, `addAutoReview`, `addRequest` | Review activity for one action can classify another action as automatic |
| Interrupted-turn hook not handled | `codexHookObservation`; current hooks registration | Hook-only waits lack a direct interrupt cleanup edge |
| herdr wins displayed status | `internal/state/herdr.go:HerdrLegacyStatus`; `SetAgentGraph` | A correct Codex attention graph can still display green |

The last behavior is explicitly tested by
`TestProviderObservationShouldNotRepaintOrRecordWhenHerdrIsTheAuthority` in
`cmd/switchboard/herdr_status_test.go`: a Codex approval graph arrives while
herdr says working, and the test requires the displayed status to remain working.
Any correction must address this deliberate precedence rule.

Broad reviewer evidence needs special care: app approvals can override the
thread's reviewer through `apps._default.approvals_reviewer` and per-app settings.
See [app-server app configuration](https://learn.chatgpt.com/docs/app-server#apps-connectors).
A thread-wide automatic-review setting or a guardian child cannot prove that
every kind of human request in that thread is automatic.

## Limits of our current observation path

`CommandConnector` starts a disposable `codex app-server --stdio`, with
read-only RPCs. It does not attach to the TUI's owning server.
The official documentation states that `thread/read` does not subscribe or
resume a thread. A standalone server's knowledge of stored threads must not be
confused with access to the owning process's pending callbacks.

The current 0.159.2 `ThreadReadResponse` still contains only `thread`; `Thread`
has neither effective reviewer settings nor a pending-request list.
`ThreadExtra` is empty. `ThreadResumeResponse` includes reviewer settings, but
resume loads/subscribes and is outside our read-only observer contract.
Starting or resuming threads merely to detect attention is not the proposed fix.

The September background-server change makes the actual transport worth
retesting, but it does not prove that our disposable server receives TUI requests.
Earlier [local captures](codex-auto-review-attention.md) found missing ownership
notifications. Their coverage must be remeasured on 0.159.2.

[Hooks](https://learn.chatgpt.com/docs/hooks) provide useful local lifecycle
signals. However, `PermissionRequest` can precede a hook decision that allows or
denies the action without a user prompt. Its documented payload does not identify
the reviewer or say that a modal was displayed. `permission_mode` is not a
reviewer-owner field. A generic permission hook cannot prove a human wait.
`PreToolUse` also precedes the actual tool operation and can itself be blocked.

The local configuration has `approval_policy=on-request` and
`approvals_reviewer=auto_review`. The eight Switchboard lifecycle hook events
are registered, but `Interrupt` and `SessionEnd` are not. Registration alone
does not prove hooks are trusted or delivered on every tool path.

## Recommended work, in order

1. **Expose disagreements before changing classification.** Record content-free
   counts for incoming question/review/approval events, whether the event came
   from the owning transport or hooks, and when herdr overrides provider
   attention. Include effective binary version and connection generation.
2. **Update question decoding.** Follow explicit `isBlocking` in current
   schemas, handle missing metadata explicitly, and detect async
   assistant-message question items. Keep runtime active for async attention.
   Determine their exact answer lifecycle in a controlled capture.
3. **Reconcile display authority.** Give fresh, exact human-attention evidence
   precedence over screen-derived working/idle, or teach herdr the same
   semantic ledger. Either approach must avoid letting timeout-guessed attention
   override a more concrete source.
4. **Scope ownership to the action.** Track requests independently. A matching
   review suppresses that review's action, not every gate in the thread. Preserve
   explicit human questions and app-specific user approvals during concurrent
   automatic review.
5. **Replace timeout-to-human only with measured coverage.** A timer is not
   ownership evidence. Removing it immediately would hide real approvals when
   hooks are the only signal. First establish a reliable human-prompt onset;
   then keep unknown gates gray/reviewing and known automatic review green.
6. **Add interrupt/session cleanup and reconnect recovery.** Use the current
   `Interrupt` hook for its exact root turn and session teardown for cleanup.
   Transport loss should produce uncertainty; replaying history must not revive
   answered questions. A supported pending-request snapshot is needed for exact
   recovery after missed onset events.

The strongest long-term contract is a supported read-only subscription to the
**owning** server, with a reconnect snapshot listing pending requests and each
request's human-action/blocking metadata. For async questions, it also needs
question IDs and answered/cancelled events. Without those contracts, hooks plus
an unrelated standalone server cannot offer perfect recovery and ownership.

## Validation

The existing Codex provider, daemon, and control-client test packages pass:

```sh
go test ./internal/provider/codex ./cmd/switchboard ./cmd/switchboard-ctl
```

They do not validate the new semantics. Before rollout, add replay cases for:
blocking input with a non-null deprecated timeout; absent `isBlocking`; async
question arrival and answer while work continues; an automatic review lasting
over 30 seconds; concurrent automatic and human-owned app gates; multiple
callbacks sharing an item; null network-review target; interrupt; reconnect
mid-question; and confirmed human attention while herdr reports working.

Live evidence should record only event kinds, opaque correlations, blocking and
ownership labels, and timings. Commands, question text, answers, review
rationales, and credentials are unnecessary for these checks.
