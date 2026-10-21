**The main problem is a missing answer-time signal, compounded by a sticky red latch.** Switchboard can learn that Codex needs approval, then keep that approval pending until the approved tool finishes. Faster polling alone will not fix that.

I saved the full investigation and implementation proposal in [codex-approval-clear-latency-2026-10-21.md](/home/tjmisko/Projects/switchboard/docs/codex-approval-clear-latency-2026-10-21.md). No runtime behavior changed.

I inspected Switchboard’s current implementation, generated protocol schemas from installed **Codex 0.163.0**, checked the local Codex source, and consulted current official documentation. I confirmed the failure mechanism in code; I did not capture timestamps for your specific earlier incident.

**Why red can persist after you approve**

The relevant lifecycle can be:

```text
PreToolUse
PermissionRequest
User approves
Approved command runs for 20 seconds
PostToolUse
```

Switchboard opens an approval record on `PermissionRequest`. A matching `PreToolUse` or `PostToolUse` can clear it, alongside later turn/session boundaries.

But Codex normally runs `PreToolUse` **before** the permission decision. There is no guaranteed second pre-hook immediately after approval. The post-hook comes after the tool produces its result—and unified execution can deliver it through a later `write_stdin` poll when the original command finishes. That makes command duration part of the stale-red interval. [Official hook lifecycle and coverage](https://learn.chatgpt.com/docs/hooks)

Two Switchboard behaviors then preserve that stale state:

- Its pending-input and approval overlays reapply attention to subsequent graphs.
- Its central resolver deliberately prevents a green terminal reading from clearing an outstanding semantic request.

Consequently, the terminal can correctly look green while Switchboard continues honoring the old approval record. Updating one green observation is insufficient if the old request remains pending.

There are additional possible scheduling delays: active snapshots default to one second, idle snapshots to ten seconds, RPC timeouts to five seconds, and reconnect backoff to five seconds. Those can worsen latency, but they are secondary to missing the resolution edge.

The **30-second approval grace** is separate: it delays classifying an ambiguous permission boundary as human-owned. Reducing it would not improve answer-to-green latency.

**What signals can answer your three questions?**

| Source | Has the user answered? | What did they answer? | Is the agent moving again? |
|---|---|---|---|
| Client’s outbound approval/question response | Exact submitted response | Yes | No; ideal presumptive-green trigger |
| Owning server accepts that response | Exact accepted response | Yes | No; strongest general decision signal |
| `serverRequest/resolved` | Request answered **or cleared** | No | No |
| Owning-server status changes | Waiting flags can clear | No | Supports continuation |
| Fresh execution/output/message activity | Indirectly | No | Yes, for the correlated activity |
| `PostToolUse` / completed item | Matched tool reached a result | Sometimes question output | Yes, but often much too late |
| `UserPromptSubmit` | A prompt was submitted | Prompt content | Intent to continue |
| Terminal modal disappearance | Heuristic | Usually unknown | Needs additional activity evidence |
| Terminal input | Input was forwarded | Ambiguous without modal context | No |
| Transcript/log tail | Sometimes eventually | Sometimes | Eventually; unsuitable for a hard latency bound |

The important distinctions:

1. **Approval replies are not necessarily user prompts.** Clicking Yes typically sends a structured protocol response. Waiting for `UserPromptSubmit` misses that path.
2. **`serverRequest/resolved` is useful but incomplete.** Its installed schema contains only `threadId` and `requestId`. Cancellation and turn cleanup can produce the same event as an answer. It cannot tell us Yes versus No or prove resumed execution. [Official resolution semantics](https://learn.chatgpt.com/docs/app-server)
3. **`item/started` is not sufficient proof of resumption.** Some items start before their approval request.
4. **Concurrent activity is not resolution.** A sibling agent producing output must not clear another agent’s unanswered question.

“What they answered” is richer than y/n. Command approvals distinguish acceptance, session acceptance, policy amendments, decline, and cancel. Permission grants specify granted permissions; structured questions carry answer maps; MCP elicitations carry an action and optional content. The response handler can normalize these without recording private answer text.

**Why the current app-server observer cannot simply supply this**

Switchboard starts a separate `codex app-server --stdio` and issues read requests. It does not intercept the running client’s approval responses.

`thread/read` explicitly does not subscribe to the thread’s events. Reading stored thread data through another server therefore does not establish access to the owning server’s live callbacks. The generated installed protocol also exposes no standalone read-only subscription or pending-request query that solves this directly. [Official thread-read contract](https://learn.chatgpt.com/docs/app-server)

The inspected Codex implementation already has the right internal boundary: it accepts a response for a specific connection/request, wakes the waiting handler, and emits resolution to subscribed connections before the approved tool needs to finish. The missing piece is getting that information reliably to Switchboard.

**My recommended solution**

Add a small **Codex-side resolution event bridge**, then make Switchboard retire the corresponding request immediately.

The best general bridge point is the owning app server’s accepted response handler. It covers terminal, desktop, and remote answers reaching that server. A client submission event can additionally provide the earliest optimistic transition.

This would be a Codex change or upstream feature request: I found no existing dedicated “approval answered” hook in the inspected hook registry. `hook/completed` means a hook handler finished, not that the subsequent human approval was answered.

On receiving the resolution event, Switchboard should:

1. Resolve that exact request and cancel its approval timer.
2. Retire its corresponding hook latch and resolver attention claim together.
3. Publish presumptive green immediately, without waiting for polling or tool completion.
4. Preserve any other outstanding human requests.
5. Confirm activity later, or transition to idle on cancellation/completion.
6. Fence delayed hooks and snapshots so they cannot resurrect the answered request.

This directly matches your intended behavior: **once your action removes the block, green can represent presumed continuation before execution is independently confirmed.** Declining an action can also return control to the agent, so “No” should not automatically leave it red. Cancellation should follow the resulting turn state.

For stock Codex without a bridge, a terminal integration could observe submission within a recognized approval modal and its subsequent dismissal, then grant the same presumptive-green transition. That is a useful fallback, but arbitrary Enter/y/n interception is too ambiguous, and terminal observation cannot cover desktop or remote answers reliably. It also requires changing the current resolver policy so old red evidence cannot immediately override it.

**The three-second requirement needs an answer signal.** A timer started when red appears would hide requests you have never answered. With a decision-time bridge, I would target **under 250 ms**, with **three seconds as the normal connected-operation acceptance ceiling**, measured from answer submission through actual bar rendering.

The decisive acceptance test is approving a command that runs for 60 seconds: the bar must become green promptly while the command is still running. Denial, cancellation, concurrent questions, child approvals, stale snapshots, and reconnects also need coverage.

The existing Codex provider, status resolver, and daemon tests all passed. They validate the current baseline, not the proposed latency guarantee.
