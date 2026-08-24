# Phase 3 — Codex app-server observer

> **Historical plan; transport superseded 2026-08-23.** The implementation now
> launches disposable `codex app-server --stdio`, not `app-server proxy`, and
> does not depend on the shared daemon. Live testing proves exact structural
> snapshots, while recovered child runtime/lifecycle remains unknown. Treat the
> notification and child-transition requirements below as unfinished acceptance
> gates; see the
> [canonical no-wrapper result](../codex-no-wrapper-binding-probe.md).

## Mission

Implement a supervised, read-only Codex provider observer whose primary truth is
the shared app-server reached through `codex app-server proxy`. Bind each
discovered Codex root to its exact root thread, build its descendant graph, and
emit coalesced update signals without blocking the state store.

## Ownership and start gate

**Workstream:** C3

**Exclusive write ownership:** `internal/provider/codex/**`, except E0-owned
`internal/provider/codex/testdata/captures/**`.

**Start only after:** W0 contract freeze and E0 fixture merge.

The workstream may read discovery/state/RPC code for context but must not edit
it. It imports the frozen graph/provider contract and reports contract gaps to
the coordinator.

## Architecture

Split the package into narrow components so protocol parsing and lifecycle are
independently testable:

```text
codex app-server proxy subprocess
        |
JSON-RPC codec / request correlator
        |
connection supervisor -- reconnect/backoff/resnapshot
        |
thread cache -- parent graph, status, collab lifecycle
        |
root binding registry -- PID/start -> exact root thread ID
        |
provider.Observer -- immutable observations + update invalidations
```

Suggested files (names are not contractual):

- `client.go`: protocol request/response/notification codec.
- `supervisor.go`: subprocess lifecycle, backoff, initialize, reconnect.
- `cache.go`: normalized thread/collaboration state and ancestry indexing.
- `binding.go`: environment and hook-assisted exact root binding.
- `observer.go`: provider interface implementation.
- `rollout.go`: optional root-only degraded task-boundary evidence.
- `fake_test.go` and fixture-driven tests.

## Transport and safety

Launch:

```text
codex app-server proxy
```

Do not connect directly to the private control-socket path and do not manage the
daemon. The proxy is supervised as a child of Switchboard; killing/restarting the
proxy must not stop the shared app-server or the user's Codex TUI.

Allow only read/initialization operations in v1:

- protocol initialize/initialized handshake;
- thread list/read/search operations required to snapshot a known root and its
  descendants;
- passive notification consumption.

Never send thread start/resume/fork/archive/delete, turn start/steer/interrupt,
approval decisions, or collaborative-agent control commands. Enforce this with
a request-method allowlist test.

Keep stderr separate from protocol stdout. Bound log lines and redact content;
do not log raw notification payloads.

## Exact root binding

Binding has one source: an exact trusted hook-supplied conversation ID,
registered against the `(pid, started_at)` process lifetime. Without that
identity the observer returns no authoritative graph and a content-free
diagnostic.

Never bind by CWD, title, nearest creation time, most recent rollout, or "only
thread in this directory." Those may appear in diagnostics but cannot populate
`RootID`.

Expose a narrow method for C6 to register an exact hook binding. Key it by PID
plus process-start identity and forget it on session death. C3 defines the
method in its own package; C6 wires trusted lifecycle hook input to it later.
`SessionStart` is expected first, but a later hook must be able to self-heal a
binding when startup delivery preceded process discovery.

## Initial snapshot

After binding a root:

1. Read the root thread and verify the returned ID.
2. Query descendants with `ancestorThreadId` when supported. Fall back to
   repeated `parentThreadId` queries only when capability/version evidence says
   it is required.
3. Build nodes using explicit parent IDs.
4. Incorporate thread runtime status and active flags.
5. Incorporate collaborative tool-call state for pending/running/completed/etc.
6. Preserve nickname and role; keep descriptions only when the protocol field is
   explicitly safe and intended for display.
7. Bound graph membership to every live descendant plus terminal descendants
   referenced by the current/most-recent root turn's collaborative items. If the
   server cannot provide that association, use a small documented/tested recent
   terminal cap rather than returning the root's unbounded lifetime history.
8. Mark the observation complete only after the descendant query finishes.

Do not infer a missing parent. Keep the last complete snapshot until its
freshness deadline while a resnapshot is in flight.

## Live notifications and ordering

Consume at least:

- thread started and thread status changed;
- turn started/completed;
- item started/completed for collaborative tool calls;
- thread metadata changes that affect nickname/role/parentage;
- archive/delete events if the protocol reports them.

Notifications update the package-private cache under its own mutex, then send a
non-blocking/coalesced `RootKey` invalidation. Never call state/RPC/renderers from
the client reader goroutine.

Event ordering must be tolerant:

- status may arrive before thread metadata;
- spawn item may expose `newThreadId` before the child thread notification;
- completion may race a final status change;
- a reconnect snapshot may supersede queued pre-disconnect notifications;
- notifications for unrelated roots may appear on the shared connection.

Use protocol IDs and monotonic connection generations to reject stale responses.
The initial/resync list result is authoritative for that generation; delayed
responses from an older generation cannot overwrite it.

## Mapping to neutral axes

Thread runtime:

- `notLoaded` -> `not_loaded`
- `idle` -> `idle`
- `active` -> `active`
- `systemError` -> `system_error`

Active flags are mechanical gates, not attention ownership:

- `waitingOnApproval` starts or refreshes approval classification;
- `waitingOnUserInput` starts or refreshes input classification;
- neither flag alone maps to `approval` or `user_input`.

The adapter preserves inbound JSON-RPC request IDs and correlates them with
`serverRequest/resolved`. A user-routed approval request becomes `approval`; a
blocking, non-auto-resolving input request becomes `user_input`. Reviewer mode
`auto_review`/`guardian_subagent`, auto-review events, or an active guardian
source keeps runtime active with no attention. Ambiguous approval ownership is
held for 30 seconds during the instrumented Phase-1 policy: a still-unresolved
request then uses a timeout-to-human fallback, while a flag with no request
becomes runtime `unknown`/gray. The delay is before publication, not a renderer
debounce, so reviews that resolve within the grace create no red graph or
history transition. Timeout fallback is labeled separately from semantic human
evidence; see [Codex Auto-review attention ownership](../codex-auto-review-attention.md).

Collaborative-agent status:

- `pendingInit` -> `pending`
- `running` -> `running`
- `completed` -> `completed`
- `interrupted` -> `interrupted`
- `errored` -> `errored`
- `shutdown` -> `shutdown`
- `notFound` -> `not_found`

Unknown future enum values map to the neutral unknown value and produce a
rate-limited diagnostic; they do not crash or drop the entire graph.

## Disconnect, freshness, and degradation

- On EOF/proxy failure, retain the last complete observation only until its
  freshness deadline.
- Emit an invalidation immediately on disconnect and again when freshness
  expires so the daemon cannot leave a stale green/red chip indefinitely.
- Reconnect with capped exponential backoff plus jitter.
- After reconnect, initialize and fully resnapshot every bound live root before
  treating new notifications as complete.
- Periodically resnapshot as a backstop for dropped notifications.
- If the app-server remains unavailable, root discovery/navigation continues.
  Hook status may supply a legacy root summary, but child graph status is
  unknown rather than guessed.

Rollout parsing may provide root task-start/task-complete evidence as a degraded
fallback after primary behavior is green. Direct SQLite is not a v1 runtime
dependency: its schema is internal and adding a SQLite driver has portability
and dependency consequences. E0 may use SQLite read-only to validate app-server
relationships.

## Concurrency requirements

- One reader goroutine owns protocol stdout.
- Request IDs and response waiters are synchronized and cancelled on generation
  loss.
- Observer returns deep copies; cache mutation cannot race reducers/renderers.
- Update sends never block the protocol reader.
- `Close` is idempotent, terminates the standalone app-server, releases waiters, and stops retry
  timers without leaking goroutines.
- No method waits on the app-server while holding a cache mutex.

## Tests

All tests use E0 fixtures or a fake standalone app-server transport. They must cover:

- initialization and allowed-method enforcement;
- exact hook binding, conversation rotation, and retired-identity fencing;
- same-CWD roots never cross-bind;
- nested descendants and stable parentage;
- spawn-before-thread and status-before-metadata ordering;
- root idle while child active;
- child approval and child user-input waits;
- both wait-before-auto-review and auto-review-before-wait ordering;
- auto-review allow, deny, timeout, and abort with zero attention edges;
- exact string/integer request resolution and mixed automatic/human waits;
- nonblocking and auto-resolving user input remaining non-attention;
- reconnect guardian evidence and unknown-owner timer expiry;
- simultaneous waiting children;
- completion/drain;
- a new root turn pruning older terminal children without losing live ones;
- unknown enum forward compatibility;
- disconnect, freshness expiry, reconnect, generation fencing, and resnapshot;
- unrelated-root filtering;
- slow consumer/update coalescing;
- repeated `Forget` and `Close`;
- `go test -race` and goroutine cleanup.

Tests must not invoke the installed Codex binary or live socket.

## Acceptance

- Every E0 fixture maps to the expected neutral graph.
- The fake-server end-to-end scenario survives disconnect/reconnect without a
  stale authoritative status.
- Request allowlist proves the observer cannot mutate Codex threads.
- No CWD heuristic exists in the binding path.
- Package tests pass under `-race`.
- Only owned files changed.

## Handoff to C6

Return the constructor, exact-binding method, observer lifecycle contract,
freshness defaults, and required startup/shutdown ordering. C6 owns actually
wiring these into the daemon and hook RPC path.
