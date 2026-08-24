# Investigation — Codex status and agent graphs in Switchboard

> Status: **implemented as a no-wrapper transport plus exact child-edge
> fusion.** A disposable `codex app-server --stdio` process supplies exact
> structural topology for plain `codex` TUIs. Exact lifecycle hooks bind the
> root, retain bounded unavailable root state, and fill child runtime/lifecycle
> only after an exact graph match. No child lifecycle is inferred from topology
> alone. The implementation plan and merge contract are
> in [`docs/codex-session-status/`](codex-session-status/README.md); the captured,
> sanitized 0.149 evidence is in
> [`evidence-report.md`](codex-session-status/evidence-report.md).

> **Known incompatibility (2026-08-21):** enabling the shared Codex app-server
> moved newly created threads under the background daemon and broke the current
> hook-parent-to-visible-TUI identity join. The hooks were configured and
> trusted, but new roots remained unbound and grey. Do not start the shared
> daemon solely for Switchboard until that exact-binding gap is fixed. See the
> [incident report](codex-app-server-hook-attribution-incident.md).

> **Standard-CLI requirement (updated 2026-08-22):** requiring a private
> endpoint launcher is not an acceptable solution. Plain `codex` now detects
> interactive questions through an exact, content-free
> `PreToolUse`/`PostToolUse` latch. The app-server item detector is not evidence
> for the standard path. See the
> [interview-detection retrospective](codex-standard-cli-interview-retrospective.md).

> **Live finding (2026-08-23):** two simultaneous plain Codex TUIs in the same
> cwd bound to different correct process lifetimes. Standalone stdio snapshots
> repeatedly recovered four bound roots plus descendant IDs, immediate
> parentage, and nicknames without erasing later hook-derived root status. The
> recovered children remained `notLoaded` with lifecycle `unknown`, so durable
> child-state fanout required a separate edge channel. August 23–24 hook probes
> found exact graph matches for every logged child lifecycle edge. See the
> [child-lifecycle decision](codex-no-wrapper-child-lifecycle.md).

This document preserves the useful findings from the original investigation,
records which conclusions changed, and describes the shipped observer boundary.

## Current conclusion

Switchboard has separate, field-scoped kinds of truth:

- OS discovery owns the interactive **root process**, its PID/tty/cwd, and its
  navigation target.
- Exact Codex lifecycle hooks bind one root thread to one OS process lifetime
  and own bounded root runtime/attention when app-server has no live value.
- A provider observer owns structural **agent graph** topology: the root thread
  plus nested, non-switchable child threads.
- Exact child hooks own only matched child runtime/lifecycle intervals; they
  retain app-server identity and immediate parentage.

For Codex, `internal/provider/codex` uses a disposable standalone
`codex app-server --stdio` child. It initializes a read-only client, reads the
exact hook-bound root, and lists descendants with an explicit subagent
`sourceKinds` filter and `ancestorThreadId`. The daemon normalizes that evidence
into `internal/agentgraph`, composes unavailable root fields with the exact
hook observation, projects the result into `state.json`, and expires each
authority at its original freshness deadline.

OpenAI's public documentation establishes that app-server is a bidirectional
JSON-RPC interface, uses JSONL on stdio, exposes `thread/read` and `thread/list`,
returns thread runtime status, supports descendant filters, and streams agent
events and approval requests. See the
[official OpenAI app-server documentation](https://learn.chatgpt.com/docs/app-server).
The public documentation search did not establish the standalone CLI transport
behavior used here. `codex app-server --stdio` was verified through installed
0.149.0 CLI help and a live host run and is guarded by a minimum-version
preflight in Switchboard. It should be described as empirical compatibility
evidence, not a general public protocol promise.

OpenAI documents subagents as separate agent threads that supported clients can
inspect. Switchboard preserves that model in the graph, but navigation stays on
the discovered OS root because a child thread does not provide an independent
terminal target. See the
[official OpenAI subagent documentation](https://learn.chatgpt.com/docs/agent-configuration/subagents).

## Exact root binding

A correct graph is useless if attached to the wrong TUI. Binding therefore
accepts only exact identity sources:

1. `CODEX_THREAD_ID` read from the discovered root process environment on
   Linux, before a hook identity has arrived.
2. The root lifecycle hooks' common `session_id`, registered against the same
   `(pid, started_at)` process lifetime. `SessionStart` normally establishes it;
   a later hook self-heals when startup delivery races process discovery. Once
   registered, hook identity wins because it can rotate on `/clear` while the
   process-start environment is immutable.

Switchboard also restores a persisted exact identity for the same process
lifetime after its own daemon restarts. It never carries that binding across a
different PID/start pair.

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

The observer's current path is:

1. Version-check the `codex` CLI (minimum locally verified stdio capability:
   `0.149.0`).
2. Start only `codex app-server --stdio`. Never open a private control socket
   and never start or stop the shared app-server. The
   [2026-08-21 incident](codex-app-server-hook-attribution-incident.md) shows
   that enabling the shared daemon changes new-thread ownership and currently
   breaks hook-to-TUI attribution.
3. Send `initialize` with `experimentalApi`, then `initialized`.
4. Call `thread/read` with `includeTurns: true` for the bound root.
5. Page `thread/list` with `ancestorThreadId`, `useStateDbOnly`, and all accepted
   0.149 subagent source kinds. Omitting `sourceKinds` would select only the
   interactive `cli`/`vscode` defaults and lose descendants.
6. Apply any `thread/*`, `turn/*`, and `item/*` notifications received,
   periodically resnapshot, and fence events/results by connection generation.
   The August 23–24 run found no usable lifecycle signal in these app-server
   channels, so trusted child hooks supply exact edges after topology matching.

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
default. `off` does not construct or run the standalone app-server, but OS
discovery/navigation and configured Codex hook fallback remain active.

## Attention and lifecycle fidelity

App-server status is mapped onto independent neutral axes, but raw wait flags
are classified before they reach the graph:

- runtime: `notLoaded`, `idle`, `active`, `systemError` → `not_loaded`, `idle`,
  `active`, `system_error`;
- mechanical gates: `waitingOnApproval` and `waitingOnUserInput` are retained
  internally but do not by themselves produce attention;
- collaborative lifecycle: `pendingInit`, `running`, `completed`,
  `interrupted`, `errored`, `shutdown`, `notFound` → the corresponding neutral
  snake-case values.

An unresolved server request becomes `approval` only when it is routed to the
user, or when no auto-review evidence arrives during the bounded 500 ms
classification window. A blocking `item/tool/requestUserInput` request with no
auto-resolution becomes `user_input` immediately. Nonblocking and
auto-resolving input remains non-attention. `thread/settings/updated`
`approvalsReviewer`, `item/autoApprovalReview/*`, and an active
`source.subAgent.other = guardian` thread classify automatic ownership; the
auto-review notifications are supplementary because their generated schema is
explicitly unstable. Exact JSON-RPC request IDs and
`serverRequest/resolved.requestId` bound human attention, including concurrent
string and integer IDs.

Publication is held only while an ambiguous wait is classified. Auto evidence
cancels that timer without creating a transient graph or history edge. A
mechanical gate whose owner remains unknown projects runtime `unknown` with no
attention, so uncertainty is gray rather than red. Request state is discarded
on exact resolution, turn/thread completion, deletion, authoritative snapshot
omission, or reconnect generation replacement. Classification diagnostics
contain only a finite source label, duration, and suppressed-false-red boolean.

Confirmed approval and user input deliberately remain distinct. Either makes
the root's legacy summary `permission` (red), but child rows show `approval`
versus `user input` (`question` in the compact Waybar tooltip). Terminal nodes
no longer count as live work or waiting attention even if a partial provider
payload still carries an old runtime/attention value.

Hooks are partial observations, with one independently owned attention latch.
`Stop` maps the root to idle; `UserPromptSubmit` and ordinary
`PreToolUse`/`PostToolUse` map it active; and `PermissionRequest` maps approval.
`request_user_input` is the narrow exception: its `PreToolUse` opens a
user-input wait keyed by `tool_use_id`, and only the matching `PostToolUse`, the
turn's `Stop`, or conversation rotation clears it. Generic app-server snapshots
cannot clear this standard-CLI wait. `SessionStart(clear|startup|resume)` is
briefly coalesced with a same-thread continuation so `/clear` followed by an
accepted plan does not create a synthetic idle interval; a standalone `/clear`
still settles idle. `SessionStart(compact)` stays active because the documented
lifecycle continues the model immediately. The fallback is root-only and
incomplete. Active evidence remains fresh for 10 minutes, approval or user-input
waits for 24 hours, and idle edges for 7 days.

`SubagentStart` and `SubagentStop` are independently ordered child edges. The
coordinator queues at most 256 per root lifetime for ten minutes and applies an
edge only when its exact `agent_id` names a non-root node in a fresh app-server
graph. Start produces active/running and clears completion; stop produces
idle/completed at the hook timestamp. A later start reopens the same child.
Running evidence expires after ten minutes without a later edge, while
completion persists until reactivation, newer concrete provider evidence,
complete omission, root rotation, or process death. Hooks never modify child
attention, create nodes, or change `parent_id`. Canonical graph and history
source remains `codex_app_server`; content-free diagnostics record hook-overlay
provenance. The full evidence and authority table are in the
[no-wrapper child-lifecycle decision](codex-no-wrapper-child-lifecycle.md).

## Terminal titles, spinners, and session names

Codex's terminal title is an output surface, not a hidden primary state feed.
The locally installed 0.149 TUI exposes configurable terminal-title items for:

- a spinner while working / an action-required message while blocked;
- compact run-state text such as ready, working, or thinking;
- the current thread title, falling back to its thread identifier when unnamed;
- project, cwd, model, branch, token, context, usage, and other metadata.

The same installed app-server schema exposes the richer machine-facing sibling
projection: `Thread.status`, `activeFlags`, `thread/status/changed`, turn/item
events, child threads, and approval/user-input requests. It also exposes the
stable naming fields `Thread.name` (optional user-facing title) and
`Thread.preview` (usually the first user message), plus
`thread/name/updated`/`thread/name/set`.

Switchboard therefore does not infer Codex state from the spinner or terminal
title. Watching the title would lose information and add failure modes:

- title items and their order are user-configurable and may be disabled;
- spinner frames repaint at animation frequency and create needless WM/bar
  churn;
- a compact “action required” title cannot preserve approval versus user-input
  waits or identify a blocked child;
- one root pane title cannot represent the descendant graph;
- a frozen/suspended pane or terminal integration failure can leave stale text;
- an unnamed thread's title fallback is a UUID, not a usable task name.

The existing app-server observer consumes Codex's structured topology directly;
it does not need title inference. Titles remain terminal/WM metadata only. For
display naming, Switchboard prefers `Thread.name`; when it is empty, the label
layer uses the first two characters of the stable root thread ID. Codex labels
never fall back to terminal titles, so spinner animation, branch/model suffixes,
and the full UUID cannot appear on Switchboard. This fallback is display-only
and does not mutate the Codex thread via `thread/name/set`.

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
tailing as the primary source for approval state. The protocol can carry
distinct approval and user-input flags, but their live delivery through the
standalone server is not yet proven. Rollout remains degraded evidence only.

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

- Standalone stdio topology and the exact child-id hook join are live-proven.
  The fused production path still needs its final rollout capture before #83
  closes. A recovered `notLoaded`/unknown child without a matched hook remains
  visible but is not counted live.
- The exact `/clear` then “implement plan” sequence in issue #86 still needs a
  content-free live replay proving red input wait, session rotation, and the
  next active/green edge without a stale old-thread repaint.
- The stdio integration depends on the locally verified CLI capability and may
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
- Shared-daemon attribution incident: [`codex-app-server-hook-attribution-incident.md`](codex-app-server-hook-attribution-incident.md)
- Child lifecycle decision: [`codex-no-wrapper-child-lifecycle.md`](codex-no-wrapper-child-lifecycle.md)
- Sanitized local evidence: [`evidence-report.md`](codex-session-status/evidence-report.md)
- Observer: `internal/provider/codex/`
- Neutral contract: `internal/agentgraph/`
- Daemon authority/freshness: `cmd/switchboard/agent_observation.go`
- Wire projection: `internal/state/agent_graph.go`
