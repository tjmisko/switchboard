# Usage-limit status (`limited`) — plan and queue

Status: in progress on `feat/usage-limit-status` (2026-10-05).

## Problem

A session whose provider account has hit its subscription usage limit cannot do
anything until the limit resets, yet no part of Switchboard detects it. Worse,
both providers end the capped turn without the hook that normally ends a turn:

- **Claude Code** fires `StopFailure {error: "rate_limit", last_assistant_message:
  "You've hit your session limit · resets 1:20pm (America/Los_Angeles)"}` and no
  `Stop` (verified in hookcap, 2026-09-29). The daemon ignores `StopFailure`, so
  the chip stays green.
- **Codex** writes `task_complete {error: {codex_error_info:
  "usage_limit_exceeded", message: "… try again at 5:15 PM."}}` to the rollout
  and fires no `Stop` hook (functionary session, 2026-10-05 12:28).
  `token_count.rate_limits.primary.resets_at` holds the same reset instant as an
  epoch.
- **Pi** ends the run with an assistant message `stopReason: "error"` and an
  `errorMessage` such as "You have hit your ChatGPT usage limit (plus plan). Try
  again in ~47 min." herdr's extension reports this as plain `idle`.

## Decisions (owner, 2026-10-05)

- A new wire status value, `limited`, rendered grey.
- It clears at the reset time, or earlier on any new activity from that session.
- Scope is the session that hit the limit only, with no account-wide propagation.
- All three agents: Claude Code, Codex, Pi. Pi reports through `switchboard-ctl
  pi-hook`, independent of herdr's managed extension.

## Model

`Session.UsageLimit` (`usage_limit` on the wire) records the evidence:
`observed_at`, optional `resets_at`, and `source`. It is orthogonal to every
status writer (hook FSM, provider graph, herdr authority, reconciler heals).
The snapshot projects it: while the limit is active (`resets_at` unset or in
the future), the wire copy's enrichment status and graph summary status read
`limited`, and `status_since` is `observed_at`. An expired record is dropped
from the wire copy, which also republishes the chip on the first reconcile tick
past the reset.

Projecting at the snapshot, instead of writing `limited` into the FSM, is
deliberate. The FSM's rules (hold gate, idle-title, delegating) never see a
value they don't model, and herdr cannot override it. When the limit lapses,
the chip shows whatever the provider says now, and since the capped turn ended,
that is idle. `Load` maps a persisted `limited` back to `idle`.

Clearing on activity: `UserPromptSubmit`, `PreToolUse`, `PostToolUse` and
`PermissionRequest` from any agent, and Pi `agent_start`, drop the record.

## Detection

| Agent  | Evidence                                    | Reset time                                      |
|--------|---------------------------------------------|-------------------------------------------------|
| Claude | `StopFailure` hook, `error=rate_limit` + "hit your … limit" text | "resets 1:20pm (Area/City)" parsed in that zone |
| Codex  | rollout terminal `task_complete` with `usage_limit_exceeded` | latest `rate_limits` window with max `used_percent`; else "try again at 5:15 PM" |
| Pi     | `pi-hook StopFailure` from `integrations/pi/switchboard.ts` | "Try again in ~N min" / "try again at H:MM PM"  |

A bare `rate_limit` (transient 429, overload) never sets `limited`. Message
text is classified at the ctl edge. Only `{usage_limit, resets_at}` crosses
RPC, which keeps the hook privacy boundary.

## Queue

1. [x] Evidence survey (this doc).
2. [x] `state`: `StatusLimited`, `UsageLimit`, snapshot projection, `Load`
   normalization.
3. [x] ctl/rpc: limit classification at the edge, record and clear in
   `dispatchAgentHook`. Claude observer treats `StopFailure` as `Stop`.
4. [x] Codex: the rollout reader reports the limit on its terminal marker, and
   `pollCodexStoppedRoot` records it. Latency is the 90 s quiet window that
   read already waits for; the app-server path (item 9) would cut it.
5. [x] Pi: `switchboard-ctl pi-hook`, `integrations/pi/switchboard.ts`.
6. [x] Renderers: tooltip "usage limit · resets 5:15 PM"; `limited` maps to
   the `unknown` colour class plus a `limited` secondary class, so every
   existing stylesheet (chips, circles, polybar, claude-tui) greys it.
7. [ ] Wire config: Claude `StopFailure` → `switchboard-ctl hook StopFailure`;
   install the Pi extension; deploy.
8. [ ] **Pi full hook lifecycle.** Pi becomes a provider agent with its own
   enrichment block, like Claude and Codex, instead of a herdr-only graph.
   The extension forwards `session_start`, `agent_start`, `tool_call`/
   `tool_result`, approval prompts, `agent_end`/`agent_settled` and
   `session_shutdown` through `switchboard-ctl pi-hook`. The daemon adds a Pi
   observer (status FSM, transcript path from `ctx.sessionManager`, session
   rotation) and herdr drops back to a fallback. It needs its own design doc.
9. [ ] Follow-ups: Codex app-server path (`turn/completed` with
   `turn.error.codexErrorInfo == "usageLimitExceeded"`, and
   `account/rateLimits/updated`); Claude transcript fallback (`isApiErrorMessage`
   + `error: "rate_limit"`) for sessions without the hook; Claude statusline
   `rate_limits.*.used_percentage` as a proactive tooltip warning.
