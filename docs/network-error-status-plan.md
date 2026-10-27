# Network-error status (`error`) — plan

Status: planned on `main` (2026-10-26). Nothing implemented yet.

## Problem

On 2026-10-26 21:40–22:00 PDT the laptop lost its network. Every agent was
blocked until a human reconnected, yet no chip went red. Session 09ce4ce6
(Functionary, Claude Code in goal mode) flipped `working` ↔ `delegating` for
twenty minutes:

```
21:40:24 delegating->working   rule=agent_graph_authority
21:43:24 working->delegating   (turn failed; background workflow agents still live)
21:44:28 delegating->working   (goal check-in re-ran the turn)
21:47:29 working->delegating
21:52:50 delegating->working
21:55:57 working->delegating
```

Each provider does report the failure. Switchboard throws the report away:

- **Claude Code** retries for about 3 minutes with backoff (ten `api-request`
  transcript rows at 0.6s … 40s spacing), then writes a synthetic assistant
  row `{error: "server_error", isApiErrorMessage: true, model: "<synthetic>"}`
  and fires `StopFailure` with the same `error` and `last_assistant_message`.
  Observed texts:
  - `API Error: Can't reach the API server — check your internet or DNS (EAI_AGAIN)`
  - `API Error: Connection lost mid-response. The response above may be incomplete.`

  `classifyUsageLimit` (`cmd/switchboard-ctl/main.go`) reads the StopFailure
  but forwards only usage-limit verdicts. A goal-mode session then re-runs the
  turn after 1 min, then 5 min (`Goal still active · the API was unavailable or
  the connection dropped · retrying in 1 min (1/3)`).
- **Pi** ends the run with `stopReason: "error"` and an `errorMessage`. Pi
  session logs contain `Connection error.`, `Network connection lost.`,
  `Request timed out.`, `Upstream idle timeout exceeded`, `terminated` and
  `Stream ended without finish_reason`. It reaches the daemon as
  `pi-hook StopFailure` → `RulePiRunFailed` → idle.
- **Codex** retries for about 2.5 minutes, then writes a terminal
  `task_complete` with an `error` and fires no `Stop` hook. Details are under
  "Codex evidence" (captured under `unshare -rn`).

## Decisions (owner, 2026-10-26)

- Red, because reconnecting is an action only a human can take.
- **No connectivity probe and no detection during the provider's own retry
  loop.** While the provider retries, the chip stays green. A session parks red
  only when the provider gives up and ends the turn. Up to ~3 minutes of
  undetected outage is accepted, because it rules out false positives from
  short network drops.
- Scope: the session whose turn failed. No host-wide or account-wide
  propagation. Every session that was working during an outage fails on its own
  evidence. Idle sessions are not blocked and keep their colour.
- Work on `main`.

## Model

`Session.TurnFailure` (`turn_failure` on the wire) sits beside
`Session.UsageLimit` and follows the same contract: hooks and the provider
graph still decide the underlying status, and only publication reads the
overlay.

```go
type TurnFailure struct {
	ObservedAt time.Time `json:"observed_at"`
	Kind       string    `json:"kind"`   // "unreachable" | "auth"
	Source     string    `json:"source"` // claude_stop_failure | codex_rollout | codex_app_server | pi_hook
}
```

- Published status: `error` (`state.StatusError`). `sessionview.ColorClass`
  maps `error` to `permission`, the existing red class, and `Prepare` adds
  `error` as a secondary class. This mirrors how `limited` maps to `unknown`,
  so no stylesheet or bar adapter has to change. A tooltip helper beside
  `LimitDetail` renders "can't reach API" or "auth failed".
- No deadline. A parked turn stays parked until the session acts again. The
  record holds even after the network is back, because the failed turn still
  needs a nudge.
- Clearing reuses the usage-limit activity rule: `UserPromptSubmit`,
  `PreToolUse`, `PostToolUse` and `PermissionRequest` newer than the evidence
  drop it. The two records share one activity watermark (rename
  `usageLimitActivityAt` → `turnActivityAt`), so a late StopFailure that loses
  a race with the next prompt cannot paint a working session red.
- **Limited vs error:** a session holds both records only if two turns failed
  differently. The newer `ObservedAt` wins. Recording one record drops the
  older one.
- Permission red stays above both records, because a live dialog is newer
  evidence than a failed turn.
- `ProjectPublished`, `hydrate*` (Load maps a persisted `error` back to idle)
  and `statusexplain` (`ReasonTurnFailureOverlay`, `EvidenceTurnFailure`) all
  extend the usage-limit paths.

## Classification

Rename `internal/usagelimit` to `internal/stopfailure`. It returns a single
verdict: `UsageLimit{ResetsAt}`, `Unreachable` or `Auth`. It runs at the same
privacy boundary as today: message text never crosses RPC, only the verdict.
`rpc.Request` gains `TurnFailure string` beside `UsageLimit bool`.

| Agent  | Unreachable when | Auth when |
|--------|------------------|-----------|
| Claude | `error=server_error` and text matches `can't reach the api server`, `connection lost mid-response`, `connection error`, `eai_again`, `enotfound`, `econnrefused`, `econnreset`, `etimedout` | `error=authentication_failed` |
| Pi     | `errorMessage` matches `connection error`, `network connection lost`, `fetch failed`, `request timed out`, `upstream idle timeout`, the errno names above | `401`, `invalid api key`, `unauthorized` |
| Codex  | rollout or app-server `task_complete.error`; see "Codex evidence" | same |

These must **not** park:

- plain `server_error` 5xx, 529 overload, or a bare `rate_limit`: transient,
  and the provider retries them.
- `invalid_request`, AUP/safeguard refusals, `max_output_tokens`: the turn
  ended, but reconnecting won't help. These may become a follow-up `auth`-like
  kind.
- Pi `Operation aborted` / `This operation was aborted`: a user cancel, which
  `Aborted` already models.
- Pi `terminated` / `Stream ended without finish_reason`: ambiguous. Leave
  them out until there is evidence.
- `ENOSPC` and other local errors.

Matching is case-insensitive substring matching on the provider's synthetic
error text only. It never reads user content: Claude's
`last_assistant_message` on StopFailure is the synthetic row, and Pi's
`errorMessage` is not model output.

## Codex evidence

Captured 2026-10-26 22:10 PDT with `unshare -rn codex exec "say hi"` (codex-cli
0.163.0, ChatGPT login). Rollout:
`~/.codex/sessions/2026/10/26/rollout-2026-10-26T22-10-22-01a17bc4-443e-7ba3-af9d-cde364bc1de6.jsonl`.

- The retry loop (`Reconnecting... 1/5` … `5/5`, 150s) goes to stderr only.
  The rollout shows nothing until the turn ends, so the chip stays green
  during retries, as decided.
- Then a terminal row:
  `task_complete {error: {message: "workspace routing discovery failed", codex_error_info: "other"}, duration_ms: 150376}`.
- No `Stop` hook fired (only `UserPromptSubmit`), which matches the usage-limit
  case. Detection therefore rides the existing rollout and app-server
  turn-error readers (`runtime_rollout.go`, `codex_usage_limit_scan.go`), not a
  hook.
- With a ChatGPT login, routing discovery runs before the model request. When
  the network is down it fails first and hides the transport error as `other`.
  In upstream `workspace_routing.rs`, `DiscoveryFailed` is the generic
  transport failure, separate from `DiscoveryTimeout`, `DiscoveryUnauthorized`
  and the config variants, so its text is a safe witness.
- With an API key, or once routing is cached, the model request fails directly.
  Upstream `CodexErrorInfo` then has structured variants, which serialize as
  single-key objects: `http_connection_failed`,
  `response_stream_connection_failed`, `response_stream_disconnected`,
  `response_too_many_failed_attempts`. None of these is captured locally yet.
  Classify them by tag, and add a fixture when one shows up.

Codex row for the classification table:

| Unreachable when | Auth when |
|------------------|-----------|
| `codex_error_info` tag is `http_connection_failed`, `response_stream_connection_failed`, `response_stream_disconnected` or `response_too_many_failed_attempts`; or it is `other` and the message is `workspace routing discovery failed` / `… timed out` | tag `unauthorized`, or message `workspace routing discovery unauthorized (401)` |

`server_overloaded`, `internal_server_error` and `rate_limit_exceeded` do not
park.

## Open verification

1. **Does a goal-mode check-in fire `UserPromptSubmit`?** If it does, the
   retry loop reads green and the next give-up parks red again, which is what
   the owner wants. If it does not, the record would hold through the retry.
   In that case, also clear on a provider-graph `working` edge whose turn
   started after `ObservedAt`. Verify with hookcap before choosing.
2. **Subagent failures:** workflow agents each hit EAI_AGAIN too. The root's
   own StopFailure is the witness, and child failures add nothing unless the
   root is still waiting on them. Confirm the root fires StopFailure while
   children are live (09ce4ce6 suggests it does: `working->delegating` at the
   failure).

## Tests

- `internal/stopfailure`: a table with every observed text (Claude, Pi, Codex)
  mapped to its verdict, plus the must-not-park list above and a user message
  that merely contains "connection error".
- `state`: should publish `error` when a turn fails unreachable; should keep
  `error` across the underlying status changing to delegating; should clear
  when a newer prompt arrives; should ignore a StopFailure older than the
  newest activity; should let the newer of usage-limit and turn-failure win;
  should read permission over error; should hydrate a persisted `error` back to
  idle.
- `sessionview`: should paint `error` with the red class plus an `error`
  secondary class.
- `switchboard-ctl`: should forward only the verdict, never the message, for a
  StopFailure carrying EAI_AGAIN.
- Fixtures: real payloads captured by running each agent under `unshare -rn`
  (a user network namespace with no network), not hand-written JSON.
