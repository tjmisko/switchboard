# Plain-Codex exact-binding probe

> Status: diagnostic spike. A match does not bind a hook or authorize a status
> update.

## Decision

Switchboard must work with an ordinary, unmodified `codex` invocation. A
wrapper, alias, replacement launcher, private per-TUI app-server endpoint,
launcher-generated token, or manual slot registration is not an acceptable
dependency or acceptance test.

The architecture must separate two concerns that the wrapper had combined:

1. **Observation transport:** one read-only shared app-server connection may
   observe thread graphs for every Codex root.
2. **Exact root binding:** a separate supported identity must join a root thread
   to one discovered interactive TUI process lifetime.

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
shared read-only app-server --thread graph keyed by root ID-----+--> coordinator
                                                                    |
                                                             agent_state history
```

The shared observer owns transport and a provider-wide graph cache. The binder
owns `RootKey <-> root thread ID`, including `/clear` rotation and process death.
Only an exact binding lets the coordinator project graph state or durable child
history. Transport loss, partial snapshots, or binding loss degrade to unknown.

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
