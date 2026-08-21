# Incident — shared Codex app-server broke hook-to-TUI attribution

> Date diagnosed: 2026-08-21
>
> Affected environment: Linux, Codex CLI 0.149.0
>
> Status: root cause understood; operational workaround available; permanent
> binding fix not implemented

## Summary

After Switchboard's Codex status support was installed, one existing Codex TUI
showed live status colors while every newly launched Codex TUI stayed grey
(`unknown`). The hook definitions were present and their trust hashes had been
recorded. The important difference was process ownership:

- the working thread was still owned by its interactive Codex TUI process;
- threads created after the shared Codex app-server daemon was enabled were
  owned by that daemon, not by their visible TUI processes.

Switchboard's hook adapter identifies the target root from the hook command's
parent-process ancestry. That is exact when an interactive TUI owns the thread,
but it cannot work when a shared daemon runs the thread on behalf of a TUI. The
daemon is deliberately excluded from interactive-session discovery, so the
hook cannot be joined to a visible root and Switchboard fails closed to
`unknown`.

This incident is an incompatibility between shared-daemon thread ownership and
the current exact-binding contract. It is not fixed by increasing observation
freshness, restarting only Switchboard, or binding by cwd.

## User-visible symptom

The failure had a distinctive shape:

- the original session changed between `working`, `permission`, and `idle`;
- new sessions appeared as normal switchable root chips, proving OS discovery
  and terminal navigation still worked;
- every new chip remained `unknown` through prompts, tools, approvals, and
  stops;
- `switchboard-ctl diagnose --observer` reported the original root as
  `bound (graph)`, `source=hook`, and `fresh`, but new roots as `unbound` with
  `exact_binding_unavailable`;
- new roots had neither `agent_graph.root_id` nor a hook-derived status.

That combination means the problem is between lifecycle delivery and exact
root binding, not in Waybar CSS or the status reducer.

## Expected identity path

The hook path was designed around a per-TUI process tree:

```text
interactive Codex TUI
        |
hook shell / switchboard-ctl codex-hook
        |
getppid() plus host /proc ancestry
        |
tracked Switchboard root PID
        |
hook session_id -> exact AgentGraph root
```

The hook payload supplies the exact Codex `session_id`; parent ancestry supplies
the exact visible TUI to which it belongs. Switchboard requires both parts. It
does not guess from cwd, rollout recency, title, or timestamp proximity because
multiple live sessions can share all of those values.

The official OpenAI hooks documentation confirms that command hooks receive
`session_id`, `transcript_path`, and `cwd`, and that lifecycle events include
`SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PermissionRequest`,
`PostToolUse`, and `Stop`. It also requires non-managed command definitions to
be reviewed and trusted by hash. See
[OpenAI Codex hooks](https://learn.chatgpt.com/docs/hooks).

## What changed

During live app-server troubleshooting, these operations were performed:

```text
codex app-server daemon start
codex app-server daemon enable-remote-control
```

The disposable `codex app-server proxy` did not complete its `initialize`
exchange, so it never became an authoritative Switchboard observer. Enabling
the shared daemon nevertheless changed where subsequently created Codex threads
ran.

The observed ownership split was:

```text
thread created before shared daemon
  -> process_uuid names the interactive TUI PID
  -> hook ancestry reaches a tracked root
  -> colors work

thread created after shared daemon
  -> process_uuid names the shared app-server PID
  -> no exact path from daemon-owned hook work to one visible TUI
  -> colors remain unknown
```

Several new threads from different visible TUIs were associated with the same
shared app-server process UUID. The original working thread remained associated
with its interactive TUI process UUID.

## Evidence and confidence

The investigation used content-free diagnostics and sanitized local metadata:

| Evidence | Result | Meaning |
|---|---|---|
| `~/.codex/hooks.json` | Six absolute-path Switchboard handlers were present | The configuration file and command paths were not missing |
| `~/.codex/config.toml` | Each handler had a persisted `trusted_hash` | Re-running `/hooks` was not the primary remedy |
| Codex feature report | `hooks` was enabled | Hooks were not globally disabled |
| Codex logs | Shared-daemon hook lifecycle activity was present | Codex was processing hook lifecycle work in the daemon-owned runtime |
| Codex log `process_uuid` correlation | New thread IDs belonged to the shared app-server PID | New threads were not owned by their visible TUI roots |
| Root process environments | No usable `CODEX_THREAD_ID` was exported | Hooks were the only available exact fallback binding |
| Switchboard observer diagnostics | New TUI PIDs stayed `unbound` | No hook established an exact root association |
| Working-session control | Existing TUI-owned thread remained hook-bound | Rendering, freshness, and the hook event mapping worked when attribution succeeded |

The confirmed defect is that shared-daemon ownership invalidates the current
hook-parent-to-interactive-root assumption. One detail remains unresolved: the
shared daemon was started before the hook trust hashes were persisted, so it is
not proven whether that already-running daemon dynamically reloaded trust. A
daemon restart might change which handlers execute, but it cannot repair the
missing exact daemon-thread-to-visible-TUI relationship.

## Code path that fails closed

The relevant implementation is intentionally conservative:

1. `cmd/switchboard-ctl/main.go` reads the hook JSON and records
   `os.Getppid()` in the RPC request.
2. `internal/rpc/rpc.go` calls `findTrackedAncestor` against the current
   discovered root snapshot.
3. If no tracked ancestor exists, the hook is ignored.
4. Only after that join succeeds does `cmd/switchboard/agent_observation.go`
   register the payload's `session_id` against `(pid, started_at)`.

The shared app-server is a background service, not an interactive root. Process
discovery correctly rejects `codex app-server` so it does not create a bogus,
non-navigable bar chip. Consequently, a hook attributed to that daemon has no
tracked ancestor and is dropped before its `session_id` can bind anything.

The hook CLI also deliberately suppresses RPC errors so observability cannot
block Codex. Codex can therefore report a completed hook command while
Switchboard has safely declined to mutate state. That behavior is desirable for
agent reliability but made this incident harder to distinguish from “hooks did
not run.”

## Why the original session worked

The working session predated the shared daemon and continued running its thread
inside the interactive TUI process. Its lifecycle hook subprocesses therefore
had a host ancestry chain that reached the already-discovered TUI PID. Once one
trusted hook registered the exact `session_id`, later hooks refreshed its
partial graph normally.

New sessions were not equivalent controls: their visible TUIs were clients of
daemon-owned threads. Their hook work could identify the Codex thread, but not
which interactive root should receive it.

## Operational recovery

Until Switchboard has an exact binding mechanism for daemon-owned threads, do
not start the shared Codex app-server daemon solely to provide Switchboard
status. Prefer the hook fallback with threads owned by their interactive TUIs.

Stopping the shared daemon may affect threads it currently owns. Close or save
active daemon-owned sessions first, then use the Codex CLI to disable remote
control and stop the daemon:

```bash
codex app-server daemon disable-remote-control
codex app-server daemon stop
```

Relaunch the affected Codex TUIs afterward. Restarting Switchboard is optional;
its discovery loop will replace dead process lifetimes and accept hooks from
the new roots.

Verify recovery with:

```bash
switchboard-ctl diagnose --observer
switchboard-ctl --json list
```

For each relaunched session, expect a non-empty graph root, `source=hook`, and a
fresh partial observation after a lifecycle event. Waybar should then emit
`working`, `permission`, or `idle` rather than `unknown`.

## Things that do not solve it

- **Repeated `/hooks` approval:** trust was already persisted. Always inspect
  trust first, but do not assume grey chips mean untrusted hooks.
- **Longer fallback freshness:** freshness only extends an observation that was
  successfully bound. These roots had no observation.
- **Restarting only Switchboard:** the replacement daemon rediscovers the same
  visible roots, but shared ownership still prevents exact hook attribution.
- **Binding by cwd or newest rollout:** two live TUIs can share a directory, and
  retries/resumes can reorder timestamps. This would turn grey into potentially
  wrong colors on the wrong chip.
- **Treating the app-server daemon as a root:** it has no independent terminal
  navigation target and may own threads for multiple visible clients.

## Permanent-fix requirements

Any code fix must preserve exactness under multiple simultaneous TUIs in the
same repository. Acceptable directions include:

1. a supported Codex identity exposed directly on each visible client process;
2. a supported app-server association between a thread and its attached TUI
   client/terminal;
3. an explicit launcher-provided token shared by the TUI and hook command;
4. detecting shared-daemon ownership and reporting a dedicated incompatible
   binding diagnostic instead of generic `exact_binding_unavailable`.

The implementation should also record content-free hook receipt outcomes such
as `accepted`, `no_tracked_ancestor`, and `binding_conflict`. That preserves the
non-blocking hook contract while making “not launched” distinguishable from
“launched but safely rejected.”

No permanent fix should use cwd, title, rollout recency, or timestamp proximity
as identity.

## Regression coverage to add with a code fix

- A TUI-owned hook subprocess binds the matching `(pid, started_at)` root.
- A hook subprocess owned by a shared background daemon cannot silently bind an
  arbitrary same-cwd root.
- Two visible roots with the same cwd remain unambiguous.
- A rejected hook increments a finite-label diagnostic without storing command
  input, prompt text, cwd, or transcript content.
- Recovery after stopping the shared daemon and relaunching a TUI creates a new
  process lifetime with no stale inherited binding.

## Related documentation

- [Codex status and agent-graph investigation](codex-investigation.md)
- [Codex app-server observer plan](codex-session-status/03-codex-app-server-observer.md)
- [Status color state model](status-color-state-model.md)
- [State schema](state-schema.md)
- [OpenAI Codex hooks](https://learn.chatgpt.com/docs/hooks)
