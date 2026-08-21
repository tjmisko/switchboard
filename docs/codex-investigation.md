# Investigation — Codex status and agent graphs in Switchboard

> Status: **implemented; the earlier hook-primary recommendation is
> superseded.** Switchboard now treats Codex app-server as the primary source
> for thread runtime, attention, lifecycle, and parentage. Hooks provide an
> exact root binding when process-environment identity is unavailable and a
> short-lived partial fallback. The implementation plan and merge contract are
> in [`docs/codex-session-status/`](codex-session-status/README.md); the captured,
> sanitized 0.149 evidence is in
> [`evidence-report.md`](codex-session-status/evidence-report.md).

This document preserves the useful findings from the original investigation,
records which conclusions changed, and describes the shipped observer boundary.

## Current conclusion

Switchboard has two different kinds of truth:

- OS discovery owns the interactive **root process**, its PID/tty/cwd, and its
  navigation target.
- A provider observer owns the root's **agent graph**: the root thread plus
  nested, non-switchable child threads.

For Codex, `internal/provider/codex` uses the app-server protocol through a
disposable `codex app-server proxy` child. It initializes a read-only client,
reads the exact root, lists all descendants with an explicit subagent
`sourceKinds` filter and `ancestorThreadId`, then consumes thread/turn/item
notifications. The daemon normalizes that evidence into `internal/agentgraph`,
projects it into `state.json`, and expires authority when the observation is no
longer fresh.

OpenAI's public documentation establishes that app-server is a bidirectional
JSON-RPC interface, uses JSONL on stdio, exposes `thread/read` and `thread/list`,
returns thread runtime status, supports descendant filters, and streams agent
events and approval requests. See the
[official OpenAI app-server documentation](https://learn.chatgpt.com/docs/app-server).
The public page does **not** currently document `app-server proxy`. That command
was verified against the locally installed Codex 0.149.0 CLI and is guarded by a
minimum-version preflight in Switchboard. It should not be described as a
general public protocol promise.

OpenAI documents subagents as separate agent threads that supported clients can
inspect. Switchboard preserves that model in the graph, but navigation stays on
the discovered OS root because a child thread does not provide an independent
terminal target. See the
[official OpenAI subagent documentation](https://learn.chatgpt.com/docs/agent-configuration/subagents).

## Exact root binding

A correct graph is useless if attached to the wrong TUI. Binding therefore
accepts only exact identity sources, in this order:

1. `CODEX_THREAD_ID` read from the discovered root process environment on
   Linux.
2. The root `SessionStart` hook's `session_id`, registered against the same
   `(pid, started_at)` process lifetime.

The observer never binds by cwd, timestamp proximity, rollout recency, title,
or a same-directory heuristic. `CODEX_SESSION_ID` is intentionally ignored:
sanitized 0.149 capture showed `CODEX_THREAD_ID` and `CODEX_SESSION_ID` can
differ, with the latter naming a parent session.

The official hooks documentation defines `SessionStart`, the common
`session_id` field, and the three-level event → matcher group → command-handler
configuration. It also states that subagent hooks use the parent session id.
See the [official OpenAI hooks documentation](https://learn.chatgpt.com/docs/hooks).
The use of `CODEX_THREAD_ID` is a locally verified Switchboard binding, not a
claim made by that public hooks page.

## Primary observation and degradation

The observer's normal path is:

1. Version-check the `codex` CLI (minimum locally verified proxy capability:
   `0.149.0`).
2. Start only the disposable stdio proxy. Never open the private control socket
   directly and never start or stop the shared app-server.
3. Send `initialize` with `experimentalApi`, then `initialized`.
4. Call `thread/read` with `includeTurns: true` for the bound root.
5. Page `thread/list` with `ancestorThreadId`, `useStateDbOnly`, and all accepted
   0.149 subagent source kinds. Omitting `sourceKinds` would select only the
   interactive `cli`/`vscode` defaults and lose descendants.
6. Apply live `thread/*`, `turn/*`, and `item/*` notifications, periodically
   resnapshot, and fence events/results by connection generation.

Production defaults are a 10-second resnapshot interval, 15-second observation
freshness, 5-second request timeout, reconnect backoff from 100 ms to 5 seconds
with jitter, and retention of at most 32 recent out-of-cohort terminal nodes
plus all ancestors required by retained/live nodes.

On disconnect, the last graph is not erased immediately. Its existing
`fresh_until` remains the authority boundary; once expired, the shared reducer
produces unknown rather than a frozen confident status. The supervisor keeps
reconnecting and performs a complete resnapshot before queued notifications
become authoritative. Unknown protocol enums degrade the affected axis to
`unknown` and emit only a rate-limited, content-free diagnostic.

The daemon flag `-codex-observer auto|off` controls rollout. `auto` is the
default. `off` does not construct or run the proxy, but OS discovery/navigation
and configured Codex hook fallback remain active.

## Attention and lifecycle fidelity

App-server status is mapped onto independent neutral axes:

- runtime: `notLoaded`, `idle`, `active`, `systemError` → `not_loaded`, `idle`,
  `active`, `system_error`;
- attention flags: `waitingOnApproval` → `approval`,
  `waitingOnUserInput` → `user_input`;
- collaborative lifecycle: `pendingInit`, `running`, `completed`,
  `interrupted`, `errored`, `shutdown`, `notFound` → the corresponding neutral
  snake-case values.

Approval and user input deliberately remain distinct. Either makes the root's
legacy summary `permission` (red), but child rows show `approval` versus `user
input` (`question` in the compact Waybar tooltip). Terminal nodes no longer
count as live work or waiting attention even if a partial provider payload
still carries an old runtime/attention value.

Hooks are lower-authority, partial observations. `SessionStart`/`Stop` map the
root to idle; `UserPromptSubmit`/`PreToolUse`/`PostToolUse` map it active; and
`PermissionRequest` maps approval, except `AskUserQuestion`, which maps user
input. The fallback is root-only, incomplete, and fresh for 15 seconds. A fresh
app-server observation outranks it.

## Historical findings retained

### Process discovery

Codex invocations share the `codex` executable name, so `comm` alone is not a
session classifier. Switchboard uses exact executable/argv basenames and an
interactive allowlist, accepting the TUI forms while rejecting wrappers,
servers, and one-shot utilities such as `exec`, `app-server`, `mcp-server`,
`mcp`, `remote-control`, and `sandbox`. Ambiguous evidence fails closed. This
prevents helper processes from becoming duplicate root chips.

The whole Navigate join remains provider-neutral:

```text
root PID -> controlling tty -> terminal pane -> window target
```

Child threads do not participate in that join.

### Rollout files

Codex rollouts remain useful degraded evidence and forensic input. The observed
layout is `~/.codex/sessions/YYYY/MM/DD/rollout-<timestamp>-<uuid>.jsonl`, with
records shaped as `timestamp`, `type`, and `payload`. Explicit turn boundaries
can support working/idle recovery.

The important limitation also remains true: the rollout does not provide a
reliable passive distinction between a command still executing and one blocked
on approval. The original investigation therefore correctly rejected rollout
tailing as the primary source for approval state. In the shipped design this is
no longer a product limitation because live app-server status carries distinct
approval and user-input flags; rollout is degraded evidence only.

### Hooks and legacy `notify`

The original investigation correctly found that hooks deliver lifecycle events
as stdin JSON and that legacy `notify` is too narrow for a full state machine.
It was wrong to promote hooks to primary truth. Hooks are edge-triggered,
root-biased, and cannot snapshot already-running descendants after a missed
event or daemon restart. They now serve exact identity and fallback enrichment.

Legacy `notify` remains unimplemented. Its turn-complete signal cannot provide
the graph, parentage, or wait-reason fidelity that app-server provides.

### State schema choice

The earlier A/B choice between only parallel `claude`/`codex` blocks and a
replacement neutral block has been resolved additively:

- `agent` plus the existing `claude`/`codex` `AgentInfo` blocks preserve legacy
  consumers and summary status.
- `agent_graph` adds the provider-neutral structured root/child view.

No legacy wire key was renamed. Renderers prefer existing enrichment status
when present and otherwise the graph summary; child detail always comes from
the graph.

## Diagnostics and privacy boundary

`switchboard-ctl diagnose --observer` combines the persisted snapshot with
finite-label observer journal records. It reports binding presence, graph
source, freshness, complete/partial state, live/wait/error counts, observer
mode, and Claude shadow agreement. It intentionally excludes cwd, transcript
paths, names, roles, descriptions, prompts, commands, tool input, raw protocol
payloads, and OS error text.

The history sink has a separate opt-in privacy contract. Canonical
`agent_state` events retain opaque thread ids and all three state axes at the
minimal tier, while nickname/role/cwd/description are scrubbed.

## Remaining boundaries

- The proxy integration depends on the locally verified CLI capability and may
  need revision if Codex changes or publicly specifies that surface.
- Non-Linux process-environment binding needs an injected platform reader or a
  trusted `SessionStart` hook.
- Rollout/SQLite captures are characterization evidence, not stable OpenAI
  APIs. Implementation protocol fixtures are generated from version-specific
  app-server schemas and are preferred over guessing undocumented fields.
- Child threads remain display/history detail. Making one independently
  focusable would require a separate product and terminal-target contract.

## Source map

- Public protocol: [OpenAI Codex app-server](https://learn.chatgpt.com/docs/app-server)
- Public lifecycle configuration: [OpenAI Codex hooks](https://learn.chatgpt.com/docs/hooks)
- Public child-thread model: [OpenAI Codex subagents](https://learn.chatgpt.com/docs/agent-configuration/subagents)
- Sanitized local evidence: [`evidence-report.md`](codex-session-status/evidence-report.md)
- Observer: `internal/provider/codex/`
- Neutral contract: `internal/agentgraph/`
- Daemon authority/freshness: `cmd/switchboard/agent_observation.go`
- Wire projection: `internal/state/agent_graph.go`
