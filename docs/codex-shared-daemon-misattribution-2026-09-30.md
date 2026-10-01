# Codex shared-daemon misattribution — 2026-09-30

Two reported symptoms share one root cause: this working Switchboard Codex
session does not become green, and the Functionary Codex session does not become
red while requesting human approval. Hooks run in a shared managed daemon whose
ancestor is the Functionary TUI. Switchboard mistakes that ancestry for ownership
of every conversation hosted by the daemon.

## Live evidence

Host process inspection showed:

```text
PID     PPID    command
162486  153419  codex                         # Functionary TUI
162518  162486  codex app-server --listen unix:// --managed-daemon
242470  240954  codex                         # Switchboard TUI
240954  47979   -bash
```

The hook capture records both conversations' hooks with ancestor chain
`162518:codex`:

- Switchboard: `01a0f47c-f7da-72c3-af3d-af7401f26428`.
- Functionary: `01a0efa8-9175-7472-87b9-ca8b48b0f1e1`.

At 15:44:46 PDT the service logs `conversation_rotated`. At 15:51, the public
state associates Functionary PID 162486 (`cwd: /home/tjmisko/Projects/Functionary`,
TTY `/dev/pts/7`) with the **Switchboard conversation ID**, graph nickname
`Debug sspi-data-webapp status`, and active/working state. Switchboard PID 242470
(`cwd: /home/tjmisko/Projects/switchboard`, TTY `/dev/pts/8`) has neither Codex
enrichment nor a graph. Its generated slot has classes `unknown, focused` and
`alt: unknown`. Its failure to become green is loss of binding, not an idle
transition. The screenshot's perceived orange should not be taken as proof of
an `idle` status.

At 15:50:36.576 PDT, the captured Functionary `PermissionRequest` contains its
correct conversation ID and CWD, and the daemon ancestor chain above. At
15:50:36 the Switchboard journal reports `rollout_binding_error count=1`.
Functionary's approval never becomes an attention event on its visible row.

Sources: host process identities, `~/.cache/switchboard/state.json`,
`/run/user/1000/switchboard/slot-0.json`, the `switchboard.service` journal,
and `~/.local/state/switchboard/hookcap/2026-09-30/hooks.jsonl`.

## Causal path

1. `cmd/switchboard-ctl/main.go:cmdHook` forwards `os.Getppid()` alongside the
   exact hook conversation ID.
2. `internal/rpc/rpc.go:dispatchAgentHook` calls `findTrackedAncestor`.
   The latter stops at an untracked **discovered agent**, but discovery excludes
   the `app-server` subcommand. It therefore walks through the managed server
   to tracked Functionary PID 162486. Process ancestry proves where the daemon
   was launched, not which client owns its current conversation.
3. Switchboard's new conversation is registered against Functionary's process
   lifetime. The binding registry interprets a different conversation as a
   legitimate `/clear` rotation and retires the actual Functionary ID.
4. Switchboard PID 242470 remains unbound; its repeated diagnostic is
   `exact_binding_unavailable`.
5. Functionary's later permission hook is again attributed to PID 162486.
   `RegisterHookRollout` finds its ID in the retired set and returns
   `codex: stale hook rollout binding`. The coordinator records
   `rollout_binding_error` and returns before reducing approval attention.
   Functionary therefore continues showing this other conversation's green.

This extends the older
[shared-daemon attribution incident](codex-app-server-hook-attribution-incident.md):
when a daemon has no tracked TUI ancestor, hooks fail closed; when it retains a
tracked ancestor, the same walk can instead contaminate that ancestor's identity.
The current agent-boundary guard protects against nested interactive/headless
agents but does not treat an app-server as an ownership boundary.

## Reproduction and scope

A temporary test replayed `findTrackedAncestor` with the observed daemon process
shape and both visible TUI PIDs. The existing implementation returned PID 162486
for the Switchboard conversation's daemon-owned hook. The replay passed and was
removed after use; it did not send hooks or modify live state.

The contemporaneous `rollout_line_too_large` diagnostic concerns rollout usage
ingestion (a 1 MiB line limit). It does not explain the wrong TUI association or
the permission hook's rejection. It is a separate issue.

The SSPI false-green incident is independently reproduced in
[the Claude completion analysis](sspi-status-signalling-2026-09-30.md).
OS discovery and pane mapping continue to work; Claude completion inference and
Codex conversation attribution fail at distinct points.

## Repair requirements

- Treat shared/managed app-server processes as hook ownership boundaries so
  they cannot contaminate a launching TUI's identity. Emit an explicit diagnostic
  instead of interpreting another client's conversation as `/clear`.
- Restore a proven conversation-to-client association for daemon-owned threads,
  or use a supported execution mode in which each TUI owns its hooks. CWD,
  recent rollouts, and the daemon's inherited terminal environment are not
  sufficient proof; several clients share the server.
- Test two simultaneous TUIs sharing a server, approval in one while the other
  works, and genuine `/clear`. Assert both identity isolation and attention.
- Recover already contaminated bindings after the attribution repair. A
  Switchboard-only restart does not remove the shared-server ownership problem.

No running session was changed during the analysis.

## Implemented repair

Codex runtimes now stop the ancestry walk even when they are not navigable
sessions. TUI-owned hooks retain ordinary ancestry attribution. For app-server
hooks, the approved fallback forwards the provider's CWD metadata and requires
exactly one discovered, still-live interactive Codex process with that directory.
Both the stored directory and the current process directory must agree. Missing,
relative, unmatched, or ambiguous directories receive no delivery. Other utilities
such as nested `codex exec` cannot use this fallback. Diagnostic categories report
unique/ambiguous/unmatched/absent directory results without exposing paths.

This is a heuristic rather than a proven thread-to-client identity: multiple
threads can share a directory, and two TUIs in that directory remain unresolved.
The fallback was explicitly approved for shared-daemon mode. It requires neither
changing the Codex execution mode nor trusting the daemon's inherited terminal.

After 90 seconds without an accepted root hook, polling checks the already-bound
rollout for an explicit terminal lifecycle marker newer than that hook. Silence
alone does not imply idle, and pending approvals or user questions are preserved.
Partial writes and unreadable evidence defer a correction. A new hook revokes the
polling correction; a fresh app-server runtime retains precedence over it.

Regression coverage includes two directories sharing a daemon, an approval hook
while the other client works, genuine `/clear`, ambiguous directories, stale or
dead clients, utility boundaries, both hook ownership modes, and polling with
terminal evidence, renewed work, missing evidence, and pending user input.
