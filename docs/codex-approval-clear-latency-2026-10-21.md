# Codex approval-clear latency investigation

Date: 2026-10-21. Switchboard revision: `2faccc17641f1034b4898c069803ca333ebf5e45`.
Installed executable: `codex-cli 0.163.0`. Investigation and proposal only; no
runtime behavior changed.

## Finding

Switchboard has a missing decision-time observation, followed by a deliberately
sticky attention policy. It can detect the beginning of a permission episode
without seeing its resolution until the tool returns. Turning a poll interval
down cannot recover an event absent from the observed source.

The proposed product contract is: after a human submits an answer to the current
blocking request, retire that request's red immediately and show presumptive
green while the turn can continue. Target under 250 ms, acceptance ceiling 3 s
under normal connected operation. An interruption/completion can instead select
idle, and another outstanding human request can keep the aggregate red.

Do not start this three-second clock when red begins. That would hide requests
the user has never answered.

## Evidence and limits

Directly inspected:

- Current Switchboard hook reducers, overlays, provider connector, notification
  handling, polling constants, and central status resolver.
- Experimental TypeScript schemas generated from the installed 0.163.0 binary:
  `codex app-server generate-ts --experimental --out
  /tmp/switchboard-codex-approval-0163`.
- Local Codex source at `/home/tjmisko/Programs/codex`, revision
  `5c013177d8` (abbreviated). This checkout is additional implementation
  evidence, not a demonstrated exact source match for the installed binary.
- Current official [hooks](https://learn.chatgpt.com/docs/hooks) and
  [app-server](https://learn.chatgpt.com/docs/app-server) documentation.
- A live daemon `explain` for this investigation's Codex process: working,
  `provider_snapshot`/`codex_app_server`, with a recent working hook rejected
  as older. This confirms current arbitration is active; it does not establish
  which evidence held the user's earlier incident red.

No timestamped answer-to-render capture of the reported incident was available.
The code establishes a concrete failure mechanism and unbounded dependence on
tool duration, but does not establish the number of seconds attributable to each
stage in that particular incident. No new model turn was launched to reproduce
an approval, and no approval was answered automatically.

## Where latency comes from

### 1. The release hook is too late

`cmd/switchboard/codex_hook_transitions.go`:

- `reduceCodexPendingApprovalLocked` opens a generic permission episode on
  `PermissionRequest`.
- Matching `PreToolUse` or `PostToolUse` clears it. Root `Stop`, session/prompt
  boundaries, and lifecycle cleanup provide additional release paths.
- A generic approval still unresolved after `codexHookApprovalGrace` (30 s)
  may be published red. This is an ownership-classification delay, not an
  answer-to-green delay.

Codex's `core/src/tools/registry.rs` runs `run_pre_tool_use_hooks` before invoking
the tool handler, and `run_post_tool_use_hooks` after the handler returns a
successful tool result with a post-hook payload. Permission checks occur within
tool execution/orchestration. A normal approval sequence can therefore be:

```text
PreToolUse -> PermissionRequest -> user waits -> user accepts
           -> approved tool runs for N seconds -> PostToolUse
```

There is no second `PreToolUse` guaranteed immediately after the answer. If
Switchboard holds the permission episode until `PostToolUse`, its stale red can
last the approved tool's entire duration. Official hook documentation explicitly
notes unified-exec's post hook may be delivered by a later `write_stdin` poll
when the original command finishes. Denial, cancellation, or an error without a
post-hook payload can delay release until a later boundary.

Blocking structured questions have a related latch in
`reduceCodexPendingInput`: exact `PostToolUse` or root turn stop releases them.
Async question post hooks intentionally do not release attention, because the
tool returns before the user answers; the next `UserPromptSubmit` currently
dismisses them heuristically.

### 2. Green evidence is deliberately unable to retire some red evidence

`overlayCodexPendingObservation` reapplies pending question attention;
`overlayCodexApprovalObservation` reapplies approval attention once published.
A later snapshot may therefore carry old hook-owned attention even if its own
runtime evidence says active.

`internal/statusresolve/resolve.go` holds provider/hook attention against a green
base, including a green terminal reading. Terminal and transcript observations
cannot resolve a semantic request. `internal/state/status_resolve.go` also retains
evidence across kinds; superseding an observation does not by itself erase its
open request.

Thus both the request ledger and the resolver must learn about a resolution.
Painting one green graph while leaving the old latch alive is insufficient.
Hook attention freshness is 24 hours (`cmd/switchboard/agent_observation.go`),
which is a retention bound, not a suitable interaction timeout.

### 3. The observer does not own the interactive transport

`internal/provider/codex/proxy.go` starts `codex app-server --stdio`.
`client.go` permits read RPCs including `thread/read`, `thread/list`, and
`thread/turns/list`; it does not attach to the TUI's existing connection.

Official documentation says `thread/read` neither resumes a thread nor subscribes
to its events. A disposable server reading stored thread data is not a tap into
the running client's callback channel. Existing investigation reports also
describe `notLoaded` roots on this path. The current live working graph is not
proof of receiving the owning server's approval responses: graphs can include
composed fallback evidence.

The installed schema has `thread/unsubscribe`, but no standalone `thread/subscribe`
or pending-requests query in its client request union. `thread/resume` is a
different operation with subscription/runtime effects; do not quietly add it to
the read-only observer as though it were a harmless query.

### 4. Scheduling adds latency after evidence becomes available

Provider defaults: active resnapshot 1 s, idle resnapshot 10 s, request timeout
5 s, reconnect backoff up to 5 s. The active/idle polling decision tests root
runtime `active`; a blocking/idle root can fall into the slower path if no other
root is active. These are additional possible delays, not proof of the incident's
exact timing. Notifications do have an immediate processing/invalidation path.

The rollout correction waits for 90 s of hook quiet and expressly refuses to
clear pending input/approval records. It is not an answer detector.

## Available sources for answered, answer content, and resumed work

| Source | Request answered/cleared? | Actual answer? | Work resumed? | Availability/limits |
|---|---|---|---|---|
| Owning client's outbound JSON-RPC response | Exact submitted response, correlated by request ID | Yes: approval decision, question answers, elicitation action/content | No; earliest point for presumptive green | Requires client instrumentation or access to its actual transport |
| Owning app-server callback acceptance | Exact accepted response for a pending connection/request | Yes | No; server has accepted the decision | Strong semantic bridge point; no public post-answer hook established |
| `serverRequest/resolved` | Exact request is no longer pending | No | No | Includes cancellation/turn cleanup; delivered to subscribed connections on owning server |
| `thread/status/changed` and `activeFlags` | Coarse waiting-state transition | No | Active with wait flags cleared supports continuation | Same owning-server/subscription requirement; inspect remaining waits |
| Fresh output deltas or new reasoning/message deltas | Indirect evidence of activity | No | Yes, for that stream | Correlate thread/turn and time; concurrent work can coexist with an unanswered request |
| Command execution start/core event | Evidence execution reached start | No | Yes for that command | `item/started` alone may precede approval and is not proof of post-approval execution |
| `PostToolUse` / `item/completed` | Tool result can close a matched request | Sometimes question tool output; not generally the approval choice | Tool reached result/completion | Often too late for a 3 s target; inspect result/status rather than assume success |
| `UserPromptSubmit` | User submitted a prompt | Prompt content, when delivered | Intent to run; not proof of model activity | Approval selections are protocol replies, not necessarily new user prompts; async dismissal is currently heuristic |
| `PermissionRequest` / `PreToolUse` | No: onset/pre-execution evidence | No | No | Permission hooks can themselves allow/deny; onset is not proof a human modal appeared |
| Terminal modal disappears / screen changes | Heuristic: visible request left the screen | Usually not exact; UI may echo selection | Spinner/output supports continuation | Useful fallback; distinguish modal dismissal from scroll, resize, obscuring, or terminal loss |
| Terminal key/input forwarding | Input was forwarded, not necessarily accepted | Raw key is ambiguous without current modal and selection | No | Candidate optimistic signal only; typing, navigation, paste and Escape differ |
| Rollout/transcript tail | Eventual result or submitted user message | Sometimes eventual question response; no universal decision receipt | Eventual persisted progress | Buffering, history format and persistence policy prevent a decision-time guarantee |
| Internal tracing/analytics | Implementation may record received response | Potentially yes | Separate activity evidence required | Diagnostic source; unstable, may expose payloads, may not be enabled or promptly readable |

The installed `CommandExecutionApprovalDecision` distinguishes `accept`,
`acceptForSession`, exec-policy amendments, network-policy amendments, `decline`,
and `cancel`. It is richer than y/n. Network amendments contain an allow/deny
action; normalize that action rather than treating every amendment as acceptance.
File changes and permission grants have different response shapes. Structured
input uses a question-ID-to-answer map; MCP elicitation uses
`accept`/`decline`/`cancel` with optional content.

`ServerRequestResolvedNotification` in the installed schema contains only
`threadId` and `requestId`. It does not distinguish human from automatic decision,
accept from reject, or a real answer from lifecycle cleanup.

In the inspected Codex source, `outgoing_message.rs::notify_client_response`
consumes the callback for the specific connection/request, decodes a typed
response, and wakes its receiver. `bespoke_event_handling.rs` response handlers
then request `serverRequest/resolved`, release waiting guards, decode/forward
the decision into core, and continue. `thread_lifecycle.rs` sends resolution to
the thread's subscribed connections. This is an early signal, before the tool
has to finish, and can even precede core processing of the answer.

The hook registry lists 12 lifecycle events and no `PermissionResolved` or
`UserInputAnswered` event. `hook/completed` means a hook handler completed; a
PermissionRequest hook completing does not mean the later human modal was answered.

The inspected rollout persistence policy marks execution-start, approval-request,
user-input-request, elicitation-request and hook lifecycle events transient.
Do not assume tailing JSONL exposes every event visible to the client. The
installed async-question schema still has title/options without a durable
question ID or answered flag, so exact answer correlation remains a separate gap.

## Recommended design

### A. Add a decision-time event source

Preferred reliable approach: a small Codex-side event bridge at accepted
app-server responses, or at the UI submission boundary plus server confirmation.
If maintaining a local Codex build is acceptable, this is a bounded change with
well-defined source locations. For upstream, request an informational,
nonblocking `ServerRequestResolved`/`UserInputAnswered` hook or read-only observer
API, with response kind and resolution cause. These are proposed names/contracts,
not existing hooks.

For desktop, CLI and remote answers, the owning-server acceptance point covers
more surfaces than terminal keystrokes. A TUI-only submission bridge provides
the fastest optimistic edge for local interactions, but does not cover answers
submitted elsewhere. A transport proxy works only if deliberately placed on
the actual owning client/server link; starting another stdio app-server is not
such a proxy. Attaching another client needs explicit routing, subscription,
reconnect and approval-ownership validation, so it is not the smallest first fix.

Emit only metadata required for status: provider session/thread, turn, exact
request ID including JSON type, item/approval correlation when available,
connection generation, monotonic sequence, timestamp, normalized outcome and
resolution cause. Include identity sufficient to match hooks to the same tool
call. Answer text is unnecessary for the bar; keep it out of routine status logs.
The event adapter can know the full answer without persisting it.

### B. Resolve a request, then project presumptive green immediately

Maintain a shared request ledger rather than independent lasting hook and
app-server red claims. Key protocol requests by connection generation, thread,
and exact request ID; correlate hooks by tool-use ID/turn/writer and available
input identity. Never collapse multiple callbacks by item ID alone.

On an accepted answer:

1. Mark that request resolved and cancel its grace timer.
2. Retire the corresponding hook latch and resolver attention claim together.
3. Preserve other outstanding human requests, including sibling/child requests.
4. Immediately publish active/presumed and invalidate the bar snapshot, without
   waiting for the next provider poll or tool result.
5. Confirm with later active status/output; replace with idle on cancellation or
   terminal completion and red on a distinct new human request.

Decline normally hands control back to the agent to choose another action, so it
can also be presumptive green. Cancel/interruption should follow turn state,
often idle. An unknown-cause resolution clears only the request; runtime remains
an independent observation, with green explicitly presumptive if selected.

Keep a resolution tombstone/sequence fence so delayed PermissionRequest hooks,
queued snapshots or reconnect replays cannot revive an answered episode. Ensure
stale terminal `blocked` evidence cannot outrank a newer acknowledged answer;
require a new modal/request generation to restore red for that episode.

### C. Provide a fallback without promising exact answers

For stock Codex with no bridge, a terminal observer can identify a visible input
modal, then observe a commit action and modal dismissal. This can offer prompt
presumptive green, with answer outcome unknown unless the UI state establishes
the selected option. It must update the request/resolver policy, not merely
add a green terminal candidate that existing attention rejects.

In the current WezTerm setup this requires a scoped input callback or terminal
integration; a herdr-based equivalent belongs at its terminal input/screen
observer. Merely observing focus, arbitrary keys, or shrinking the permission
grace is insufficient. A disappearance without any input/semantic resolution
should be weaker than an actual answer and should not clear unrelated requests.

This fallback cannot guarantee the target for desktop/remote responses or a
terminal modal it cannot observe. A hard normal-operation 3 s target requires
an answer signal from a surface that actually sees the answer. A universal bound
during process stalls/disconnection cannot be guaranteed by this architecture.

## Validation and implementation order

First capture one accepted long command and one declined command with a common
clock: UI commit, server response receipt, resolution notification, hook delivery,
ledger retirement, resolver decision, snapshot publication, rendered bar. This
separates Codex delay, observer delay, state-policy delay and rendering delay.

Then implement the normalized resolution event and ledger retirement, bridge it
from Codex acceptance, and add the terminal fallback only where needed. Keep
normal polling for recovery and topology, not as the critical interaction path.

Acceptance cases: accept a 60 s command (green before completion); deny and retry;
cancel; blocking/free-text/async input; two concurrent requests; child waits;
same item with multiple callbacks; late onset after resolution; reconnect and
request-ID reuse; automatic review without human input; process death; stale
terminal blocked state; immediate new approval after the previous answer.

Measure answer-to-render latency, targeting <250 ms and requiring <=3 s for each
connected acceptance case. Do not validate only hook-to-store latency.

Existing relevant tests passed during this investigation:

```text
go test ./internal/provider/codex ./internal/statusresolve ./cmd/switchboard
```

Those tests establish the current baseline, not the proposed latency guarantee.
