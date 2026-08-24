# Plain-Codex exact-binding probe

> Status: live no-wrapper root binding, standalone transport, bounded root
> composition, and the exact child-hook/topology join passed on 2026-08-23–24.
> Issue #83 discovery is complete; the fused production rollout remains its
> closing gate. See the
> [child-lifecycle decision](codex-no-wrapper-child-lifecycle.md).

## Decision

Switchboard must work with an ordinary, unmodified `codex` invocation. A
wrapper, alias, replacement launcher, private per-TUI app-server endpoint,
launcher-generated token, or manual slot registration is not an acceptable
dependency or acceptance test.

The architecture separates three concerns that the wrapper had combined:

1. **Observation transport:** one disposable standalone
   `codex app-server --stdio` connection reads thread graphs for Codex roots.
2. **Exact root binding:** a separate supported identity must join a root thread
   to one discovered interactive TUI process lifetime.
3. **Field authority:** app-server owns topology, while an exact hook may own
   bounded root runtime/attention only when app-server reports that field
   unavailable.

Per-TUI app-server endpoints are unnecessary if the second join can be made
exactly. Cwd, title, timestamps, and launch order remain forbidden joins.

## Question this spike answers

When a shared Codex daemon owns a lifecycle hook subprocess, does that subprocess
still retain exact terminal identity from the TUI that caused the event?

`switchboard-ctl codex-hook` now inspects only its own terminal context:

- a TTY attached to stdin, stdout, or stderr;
- `SSH_TTY`;
- `WEZTERM_PANE`;
- `TMUX_PANE`.

Each value is length-bounded and allowlisted before crossing Switchboard's local
RPC socket. The daemon compares TTY and WezTerm pane values with identity it
already discovered for visible Codex roots. It records only finite outcome
counters. `TMUX_PANE` is presence evidence only because the current session
model has no independent tmux pane field to join against.

The possible outcomes are:

- `hook_client_identity_absent`: the hook retained no candidate identity;
- `hook_client_identity_unmatched`: candidates match no visible Codex root;
- `hook_client_identity_ambiguous`: candidates match multiple roots;
- `hook_client_identity_unique`: candidates match exactly one root.

For a unique result, the journal also records the matched root PID, but never the
raw hint. The hook is still dropped. This prevents a plausible-looking probe
from silently weakening the exact-binding invariant.

## Live evidence protocol

Run this only after the spike binary is installed through the normal rollout.
Do not start, stop, or reconfigure the Codex shared daemon for the experiment.

1. Record a counter baseline:

   ```sh
   switchboard-ctl --json agent-diagnostics
   switchboard-ctl --json list
   ```

2. Open two plain `codex` TUIs in the same cwd. Record their visible root PIDs.
3. Trigger a harmless lifecycle event in the first TUI, then the second.
4. Compare counter deltas and the bounded journal records:

   ```sh
   switchboard-ctl --json agent-diagnostics
   journalctl --user -u switchboard.service --since "10 minutes ago" \
     | rg 'codex-hook-attribution-probe'
   ```

5. Repeat across `/clear`, resume, and interleaved events from both TUIs.

Promotion requires every event to produce a unique match to the TUI that
actually caused it. Two same-cwd TUIs must map to different correct PIDs, and no
event may be absent, unmatched, ambiguous, or uniquely matched to the wrong
root. A unique counter without the correct PID comparison is insufficient.

## Architecture after a successful probe

If the identity survives those tests, promote only the proven hint kind into an
exact binder:

```text
plain codex TUI --OS discovery--> RootKey(PID, process start)
       |
standard lifecycle hook --exact terminal identity + session_id--+
                                                               |
standalone stdio app-server --thread graph keyed by root ID-----+--> coordinator
                                                                    |
                                                             agent_state history
```

The standalone observer owns transport and structural graph snapshots. The
binder owns `RootKey <-> root thread ID`, including `/clear` rotation and
process death. Only an exact binding lets the coordinator project graph state
or durable child history. Transport loss, partial snapshots, or binding loss
degrade to unknown.

## If the probe fails

Do not fall back to a wrapper or a heuristic. The remaining production options
are:

1. find passive provider state containing an exact supported
   client/thread-to-terminal relationship;
2. consume a supported shared-app-server client attachment identity if Codex
   exposes one;
3. request an upstream hook field or client-association API.

Until one exists, Switchboard should keep plain-Codex roots navigable but report
their graph/status as degraded or unknown.

## Live result — 2026-08-23

Two simultaneous plain `codex` TUIs in the same cwd were discovered as distinct
process lifetimes and accepted different exact hook thread IDs. Both projected
the expected root lifecycle through `source: hook`. No `hook_client_*` outcome
was emitted because ordinary process ancestry attributed both hooks before the
fallback probe was needed.

This validates the no-wrapper root-binding path for the tested runtime; it does
not validate terminal hints under shared-daemon-owned hook ancestry. The
remaining blocker was the observation transport: both graphs stayed
`complete: false`, hook-sourced, while `snapshot_pending` increased. The spike
therefore emits bounded snapshot-stage categories for `thread/read`,
`thread/list`, root mismatch, and graph validation. Raw protocol errors, thread
IDs, and payloads remain excluded from diagnostics.

## Standalone stdio transport experiment — 2026-08-23

The corrected live probe showed that `codex app-server proxy` repeatedly opened
its subprocess pipes but failed the first `initialize` request. The installed
CLI describes that command as a byte proxy to an already-running app-server
control socket; no usable socket was present.

The spike now launches `codex app-server --stdio` instead. This standalone
transport is exposed by the installed unmodified Codex binary; public
documentation did not establish its compatibility contract. It does not
require a wrapper, alias, replacement launcher, private per-TUI endpoint, or
any change to the shared Codex daemon. The experiment must establish both
halves of the feature before promotion:

1. the standalone server can read the hook-bound root thread and its descendants;
2. its snapshots or notifications remain sufficiently live to retain every
   child lifecycle transition required by history fanout.

Successful initialization alone is transport progress, not proof of either
invariant. `observer_initialized` must be followed by snapshot target/read/list
success, complete structural graphs, and independent live transition evidence.

### First live stdio result

The standalone transport initialized and repeatedly installed valid snapshots
for all four hook-bound roots. It also recovered descendant thread IDs,
parentage, and nicknames. However, the standalone server reported every
interactive root and recovered child as `notLoaded` with unknown lifecycle.
That structural snapshot erased the previously useful hook-derived root status
and cannot yet prove child lifecycle transitions.

The spike therefore composes authorities by field: app-server snapshots retain
topology, while the last exact root hook retains working/idle/attention only
until its original state-specific freshness deadline. A later app-server value
other than `unknown`/`notLoaded` wins immediately. Unknown child lifecycle stays
unknown; the spike does not infer liveness from topology alone.

### Final live validation

After installing commit `17af1aa`, the observer initialized immediately and
continued installing snapshots for the exact hook-bound roots without read or
list errors. Later snapshots preserved bounded hook transitions, including
idle-to-working and working-to-idle edges, instead of repainting those roots as
not loaded. The original hook freshness deadline remained authoritative.

The recovered descendants still had runtime `not_loaded` and lifecycle
`unknown`. Their IDs, immediate parentage, and nicknames prove topology only;
they do not prove that the children are currently live or that lifecycle edges
will reach durable `agent_state` history.

| Invariant | Result |
|---|---|
| Two same-cwd plain Codex TUIs bind to distinct correct process lifetimes | Pass |
| Unmodified `codex app-server --stdio` initializes and snapshots | Pass |
| Exact root and descendant topology is recovered | Pass |
| Bounded exact-hook root status survives later not-loaded snapshots | Pass |
| Exact child hook id joins app-server topology | Pass; every logged outcome matched |
| Fused production child runtime/lifecycle and durable fanout | Implementation complete; live rollout still gates #83 |
| `/clear` then implement-plan red/rotation/green sequence | Unproved; blocks #86 |

## Child-lifecycle probe result and production decision

The #83 experiment separated three possible no-wrapper lifecycle channels:

1. lifecycle-relevant notifications received by the standalone app-server;
2. persisted `collabAgentToolCall.agentsStates` returned by either
   `thread/read(includeTurns=true)` or the explicit full-item
   `thread/turns/list`; and
3. standard `SubagentStart`/`SubagentStop` hooks whose `agent_id` exactly matches
   a child already present in app-server topology.

[`scripts/run-codex-child-lifecycle-probe`](../scripts/run-codex-child-lifecycle-probe)
installs a diagnostic build, adds the two standard hook handlers, and watches a
single deliberately long-lived child from start through completion. It emits
only finite evidence categories, opaque root/child IDs, neutral state axes, and
canonical child-history fields. Prompts, messages, tool input, commands, cwd,
nicknames, roles, and raw provider payloads are excluded.

Across approximately 800 snapshots, the app-server notification channel
produced no lifecycle evidence. `thread/read` and `thread/turns/list` exposed
completed `wait` items but no receiver ids or useful agent states. Child
snapshots stayed `not_loaded`/`unknown`. In contrast, all logged lifecycle hooks
matched exact non-root graph ids: nine unique children on August 23, three on
August 24, and a stop counter of at least 19, with no absent-id, graph-missing,
or unmatched result.

That evidence promotes only the proven join. Production queues child hooks
until fresh topology matches, preserves app-server parentage/source, expires
unclosed running evidence after ten minutes, and never creates a child or
completion heuristically. The full evidence record, authority matrix, state
machine, diagnostics, and rollout gate are in
[`codex-no-wrapper-child-lifecycle.md`](codex-no-wrapper-child-lifecycle.md).
