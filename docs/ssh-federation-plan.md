# SSH federation: minimal live-state and WezTerm binding plan

> Status: implemented in code; real two-host/WezTerm acceptance pending
> Date: 2026-08-24
> Scope: show live Switchboard sessions from SSH hosts on the client and focus
> the exact local WezTerm pane/window that displays a remote session.

## 1. Decision

Keep the design deliberately small:

1. The Switchboard daemon on each host remains the sole authority for that
   host's roots, status, and child-agent graph.
2. The client runs one long-lived SSH subscription per configured host and
   retains only the latest complete snapshot from each live connection.
3. A session is namespaced by `(hostname, pid)`. The existing `started_at` is
   carried on focus and binding operations as a best-effort stale-action fence.
4. A remote daemon identifies a session's terminal by writing one bounded
   WezTerm user variable into that session's own TTY.
5. A small WezTerm Lua integration receives that variable in the exact local
   pane and reports the local GUI, window, and pane IDs to the client daemon.
6. Focus is then an ordinary local WezTerm-pane activation plus the existing
   local window-manager focus operation.

```text
remote Store -- full snapshots --> ssh stdout --> client read-only host map

remote root TTY -- OSC user var --> exact local WezTerm pane
                                         |
                                         v
client focus request --> local pane ID + exact local WM window
```

This plan does **not** introduce a general federation protocol, a distributed
store, or a multi-hop action planner. It adds one state stream and one exact
terminal binding.

## 2. Why this fits the current architecture

Switchboard already does the difficult observation work on each host:

- `internal/state.Store` owns live discovered roots and provider-derived state.
- Store broadcasts and RPC `subscribe` use full replacement snapshots. Dropped
  intermediate frames repair themselves at the next frame.
- `updated_at` changes only when observable state changes, so it is not a
  liveness clock.
- A session already carries `(pid, started_at)`, which the public schema tells
  consumers to use across PID reuse.
- Navigation already re-locates a terminal pane at action time and asks the
  local WM backend to raise the window.

The missing fact is only this cross-host join:

```text
remote root's /dev/pts/N  ---- exact SSH terminal stream ----> local WezTerm pane
```

OpenSSH does not expose that join as metadata. The terminal stream itself does,
because an escape written to the remote root's TTY can only arrive in the pane
that is displaying that TTY.

## 3. Identity and duplicate prevention

### 3.1 Live row identity

For the unified live view, hostname plus PID is sufficient:

```go
type SessionKey struct {
    Hostname string
    PID      int
}
```

PIDs collide between hosts, so PID alone is not sufficient. Within one running
host, there is only one live process for a PID.

### 3.2 Exact action identity

Bindings and focus requests also carry the already-existing `started_at`:

```go
type ExactSessionKey struct {
    Hostname  string
    PID       int
    StartedAt time.Time
}
```

`started_at` is Switchboard's discovery timestamp and a stale-action guard, not
a new identity system or kernel process-birth token. It rejects a PID
replacement observed during daemon continuity. A hydrated same-kind process
that reuses a PID while the daemon was down may retain the timestamp; reconnect
still drops all old pane candidates and requires a fresh TTY announcement.

Do not add daemon instance IDs, random process incarnations, stream revisions,
or request generations for the first version.

### 3.3 Hostname rules

- The SSH destination remains local configuration, such as `build` from
  OpenSSH config.
- The remote stream reports `os.Hostname()`; remote session rows and pane
  signals use that same value.
- Allow at most one active source for a hostname. If two SSH destinations
  report the same hostname, reject the second with a clear diagnostic rather
  than render duplicate rows.
- Remote data never chooses an SSH destination.

This assumes hostnames are unique in the user's actual fleet. If containers or
cloned machines with duplicate hostnames become a real topology, add an
explicit configured host ID then. Do not solve that hypothetical now.

## 4. Live observation over SSH

### 4.1 One high-level stream

Add a CLI command intended for SSH execution:

```bash
ssh -T build switchboard-ctl remote-stream
```

`remote-stream` connects to the normal remote daemon socket, requests the
current snapshot plus subsequent `subscribe` frames, and writes newline-delimited
frames such as:

```json
{"host":"buildbox","snapshot":{"schema_version":3,"sessions":[]}}
```

Every frame is a complete replacement for that host. Repeating the hostname is
small and keeps every line independently meaningful.

This is preferable to a raw stdio RPC bridge for the first version:

- there is only one SSH child and one direction of application data;
- no remote runtime socket path is exposed;
- there is no second command channel or RPC multiplexing;
- the child has one stable, inspectable output contract.

### 4.2 Client worker

Each configured SSH destination has one sequential worker:

1. ensure the local RPC listener is already accepting pane-binding callbacks;
2. start `ssh -T <destination> switchboard-ctl remote-stream`;
3. decode complete frames;
4. after the first valid frame, claim its hostname;
5. atomically replace that hostname's latest snapshot on every frame;
6. on EOF or error, remove the hostname's sessions from the live view;
7. wait briefly and reconnect only after the old child has exited.

Because a worker never overlaps two children, an old stream cannot race a new
one. No connection generation is needed.

Use OpenSSH's ordinary host-key checking and noninteractive authentication.
Bound half-open connections with `ServerAliveInterval`/`ServerAliveCountMax`
through the user's SSH config or narrowly scoped process arguments.

### 4.3 No stale remote rows initially

When the SSH stream dies, remove that host's sessions immediately. This is the
simplest correct behavior: the UI never presents an old snapshot as live, and
no application heartbeat or stale TTL is needed. The source may show one
bounded disconnected diagnostic outside the session list.

Reconnect supplies a fresh initial snapshot and restores the rows.

### 4.4 Read-only aggregation

Do not insert remote records into the host-local `state.Store`. Keep:

```text
local store snapshot
remote latest[hostname]snapshot
```

The unified view flattens detached copies for rendering and selection. Each row
retains its hostname. Local discovery, liveness, provider observation, history,
terminal reconciliation, and persistence continue to operate only on local
sessions.

Keep existing local `list` and `subscribe` behavior. Add an explicit aggregate
surface (`list-all`/`subscribe-all`) until there is a deliberate schema-version
decision to make aggregation the default.

## 5. Exact remote-session to local-pane binding

### 5.1 Let the terminal stream carry the join

For each interactive, non-headless session, the origin daemon can write this
WezTerm user variable to the session's controlling TTY:

```text
OSC 1337 ; SetUserVar=SWITCHBOARD_SESSION=<base64(v1 payload)> BEL
```

The decoded, bounded v1 payload contains only:

```json
{"host":"buildbox","pid":1234,"started_at":"2026-08-24T20:00:00Z"}
```

The payload is a correlation label, not a credential. It contains no command,
socket path, SSH destination, cwd, transcript path, or content.

Emit it:

- when a live root is discovered;
- again when `remote-stream` attaches, so a restarted client daemon can rebuild
  its in-memory routes;
- outside the store lock, with a bounded/nonblocking TTY write;
- only after validating that `(pid, started_at, tty)` is still the live session.

Failure to write is non-fatal and leaves the row observe-only.

WezTerm intentionally associates user variables with panes, emits a
`user-var-changed` event, and propagates the change across multiplexer
connections. This is the exact correlation primitive needed here:

- [Passing data from a pane to Lua](https://wezterm.org/recipes/passing-data.html)
- [`user-var-changed`](https://wezterm.org/config/lua/window-events/user-var-changed.html)

The current `wezterm cli list --format json` contract does **not** expose user
variables or the domain name, so polling the CLI cannot replace the Lua event:

- [current `cli list` output fields](https://github.com/wezterm/wezterm/blob/main/wezterm/src/cli/list.rs#L108-L136)

### 5.2 Small local WezTerm integration

The Lua integration has three jobs.

First, append a stable local marker to every WezTerm OS-window title:

```text
[sbw:<wezterm-gui-pid>:<mux-window-id>]
```

The marker composes with the user's existing title rather than replacing it.
It makes the final WezTerm-window-to-WM join exact even when two windows show
the same host, cwd, or agent title.

Second, on `SWITCHBOARD_SESSION` `user-var-changed`, launch a fixed argument
vector—never a shell string—equivalent to:

```text
switchboard-ctl pane-bind <session-payload> <gui-pid> <window-id> <pane-id>
```

The local values come from WezTerm itself:

- `wezterm.procinfo.pid()` for the local GUI process;
- `window:window_id()` for the local mux window;
- `pane:pane_id()` for the local, possibly remapped pane.

Third, report active-pane/window changes for the remote `Focused` projection.
Use `window-focus-changed` and a change-deduplicated `update-status` handler;
do not launch a helper on every periodic status tick. Binding and state helpers
must share a per-pane serialized lane using synchronous `run_child_process`:
background children can reach the daemon out of observation order.

Relevant WezTerm surfaces:

- [`window:window_id()`](https://wezterm.org/config/lua/window/window_id.html)
- [`pane:activate()`](https://wezterm.org/config/lua/pane/activate.html)
- [`wezterm.procinfo.pid()`](https://wezterm.org/config/lua/wezterm.procinfo/pid.html)
- [`wezterm.run_child_process`](https://wezterm.org/config/lua/wezterm/run_child_process.html)
- [`window-focus-changed`](https://wezterm.org/config/lua/window-events/window-focus-changed.html)
- [`update-status`](https://wezterm.org/config/lua/window-events/update-status.html)

The title hook must be composed into an existing `format-window-title` handler,
because WezTerm executes only the first handler for that event.

### 5.3 Minimal route table

The client daemon keeps an in-memory, bidirectional binding table:

```go
type LocalPaneRef struct {
    GUIPID   int
    WindowID int
    PaneID   int
}

ExactSessionKey -> set[LocalPaneRef]
LocalPaneRef     -> ExactSessionKey
```

Rules:

- A new binding for a local pane replaces that pane's previous binding.
- A binding is navigable only while its exact session is present in the latest
  live snapshot for the same hostname.
- A binding may arrive before the first snapshot and remain a candidate; it
  authorizes nothing until the matching live row exists.
- If one exact session is bound to more than one live local pane, navigation is
  ambiguous and fails closed. A multi-client policy can come later.
- Routes are in memory. A new `remote-stream` connection re-announces current
  bindings after a client-daemon restart.

The emitter clears and then sets the value on every announcement. Live
acceptance must verify that this produces a fresh set event on the installed
WezTerm build; do not add route persistence merely to work around that seam.

### 5.4 Solid pane-to-window validation

A bind validates only the bounded payload and locally reported ID tuple, then
records an unauthorizing candidate. For a focused pane-state report and again at
action time immediately before focus, the client validates the whole local route:

1. the live `gui-sock-<gui-pid>` enumeration contains `pane_id` in `window_id`;
2. exactly one WM client has PID `gui-pid` and title marker
   `[sbw:<gui-pid>:<window-id>]`;
3. the exact remote session key is still live.

Only after all three facts resolve does focus act:

1. activate the local WezTerm pane through its local GUI socket;
2. raise the exact WM window through the existing WM backend.

Do not store a WM address as durable routing truth. Resolve it at action time,
as current local focus already re-resolves terminal state.

WezTerm's own `window:focus()` cannot perform the final hop on Wayland, so the
Hyprland/sway backend remains necessary:

- [`window:focus()` platform support](https://wezterm.org/config/lua/window/focus.html)

## 6. Focused state

Ignore a remote daemon's `Focused` bit in the unified view. It describes the
remote desktop, if any, not the client's outer pane.

For a remote row, derive `Focused` only when:

- WezTerm reports that exact bound pane as active in its window; and
- the local WM reports that exact marked WezTerm window as focused.

This keeps `cycle` and `attention` correct whether the user reached a pane
through Switchboard, clicked it, or changed tabs manually.

Selection and focus requests carry `ExactSessionKey`; display and sorting may
use the smaller `(hostname, pid)` namespace.

## 7. Supported first topologies

### Ordinary OpenSSH pane

The remote TTY escape traverses the ordinary SSH channel and lands in its one
local WezTerm pane. Focus is entirely local after binding.

This is the first and most important topology. Two panes connected to the same
host and cwd must remain distinguishable because the join uses the terminal
stream, not host/cwd heuristics.

### WezTerm SSHMUX domain

WezTerm user variables propagate across multiplexer connections. The Lua event
sees the client-remapped local pane ID, so Switchboard does not need to translate
remote pane IDs or query a domain name.

Validate this after ordinary SSH, using the same binding path rather than a
second SSHMUX-specific navigation design.

### Remote tmux

Defer remote tmux. Passing OSC through tmux requires its passthrough rules, and
one remote pane can be visible through multiple attached clients. Those are
real extra semantics, not requirements for proving SSH federation.

If the same binding path works through tmux with a properly wrapped escape and
one attached client, add it as a small extension. Do not build a tmux client
router in the first implementation.

## 8. Security and failure behavior

The OSC signal registers a route; it never focuses a window by itself. A remote
process therefore cannot steal focus merely by printing the variable. Focus
still requires an explicit local click/key action.

Additional rules:

- accept only bounded, versioned binding payloads;
- require the payload to match a currently imported exact session before use;
- derive all local GUI/window/pane values in Lua, never from remote data;
- invoke `switchboard-ctl` from Lua with a fixed argument vector, never a shell;
- never forward a WezTerm control socket to a server;
- never let remote fields select a command, executable, socket, or SSH target;
- log finite failure categories, not snapshot or terminal content.

| Condition | First-version behavior |
|---|---|
| SSH stream unavailable | remove that host's rows; retry |
| Remote daemon unavailable | same; local state remains healthy |
| Malformed/schema-incompatible frame | reject the source |
| Duplicate hostname | reject the second source |
| Missing binding | show row as observe-only |
| Stale PID binding | `started_at` mismatch; reject |
| Pane closed or moved | re-resolve; update or fail without guessing |
| Missing/duplicate window marker | navigation unavailable |
| More than one client pane for a session | ambiguous; navigation unavailable |
| Local pane activates but WM focus fails | report partial failure |
| One remote source fails | no effect on local or other remote sources |

## 9. Machinery explicitly deferred

Do not implement these until a demonstrated requirement needs them:

- raw Unix-socket forwarding as the production transport;
- a byte-for-byte RPC stdio bridge;
- two persistent SSH command channels;
- request IDs or RPC multiplexing;
- daemon instance IDs, random incarnations, revisions, or generations;
- application heartbeats or stale snapshot retention;
- delta snapshots, replay, or resynchronization protocols;
- SSH wrappers, `AcceptEnv`, forwarded `WEZTERM_PANE`, or route tokens;
- a generic typed multi-hop focus planner;
- automatic remote tmux attach/spawn;
- aggregate history persistence;
- dynamic remote-source configuration or discovery.

The existing schema version plus full-snapshot replacement, exact session key,
sequential SSH worker, and action-time pane validation cover the first version's
modeled races without adding a general coordination protocol.

## 10. Implementation and acceptance phases

### Phase 0 — binding path implemented; live acceptance pending

Build the smallest experimental path:

1. append the stable `[sbw:gui-pid:window-id]` title marker in WezTerm;
2. install a temporary `user-var-changed` logger/callback;
3. with two ordinary SSH panes connected to the same host and cwd, write two
   distinct v1 binding values into the two remote TTYs;
4. verify the callback reports the correct local GUI/window/pane tuple for each;
5. use those tuples to activate and raise each exact pane/window repeatedly;
6. move panes between tabs/windows, close one, and repeat;
7. repeat the same-value announcement and restart the client daemon to verify
   route reconstruction.

Definition of done for one live WezTerm GUI/socket lifetime: the wrong
pane/window is never focused, and every stale or ambiguous case fails rather
than guesses. The route currently uses numeric GUI/pane/window IDs plus socket
peer credentials, not a separate GUI-process birth token; the live test must not
claim protection against a complete numeric-ID collision after GUI PID reuse.

The OSC emitter, Lua callback, exact pane/socket lookup, marked-window lookup,
and ordering tests are implemented. The real-terminal experiment still tests
the only novel, environment-dependent seam; unit tests cannot establish that
an installed WezTerm and an ordinary SSH PTY pass the escape end to end.

### Phase 1 — implemented; live SSH acceptance pending

- Add `switchboard-ctl remote-stream`.
- Start one sequential SSH worker from the client daemon.
- Keep one detached latest snapshot keyed by returned hostname.
- Add a minimal `list-all`/`subscribe-all` view and host labels.
- Remove remote rows on disconnect and reconnect with a small bounded delay.

Definition of done: one remote host appears and changes at the same latency as
its local Switchboard subscription, while stopping SSH removes only those rows.

### Phase 2 — implemented; live focus acceptance pending

- Emit the v1 binding on remote discovery and remote-stream attach.
- Add bounded local `pane-bind` and active-pane RPCs.
- Add the in-memory bidirectional route table.
- Resolve the exact local pane and marked WM window at focus time.
- Derive remote `Focused` from local WezTerm state plus current WM validation.
- Generalize pick/cycle/attention to `ExactSessionKey`.

Definition of done: two ordinary SSH panes on the same host and cwd remain
exactly navigable, including after PID death/reuse and a client-daemon restart.

### Phase 3 — multi-host code implemented; SSHMUX remains unverified

- Allow repeated configured SSH destinations.
- Reject duplicate returned hostnames.
- Exercise same PID values on different hosts.
- Validate the identical OSC/Lua binding through a WezTerm SSHMUX domain.

Definition of done: local, ordinary-SSH, and SSHMUX rows share one view and one
focus path, with no duplicate or cross-host selection.

### Later, only if wanted

- tmux passthrough and multi-client policy;
- grey stale rows instead of immediate removal;
- explicit host IDs for duplicate-hostname topologies;
- persistent source configuration;
- transport optimization if one SSH process per host measures poorly.

## 11. Validation matrix

| Case | Required result |
|---|---|
| Same PID on two hosts | distinct rows by hostname |
| Same host configured through two SSH aliases | second stream rejected |
| PID reused while daemon remains up | old `started_at` binding cannot act |
| Same-kind PID reused while daemon was down | fresh TTY binding is required; do not claim `started_at` is a birth token |
| Two SSH panes, same host and cwd | exact distinct pane/window routes |
| Pane moved to another tab/window | next bind/reconcile targets new location |
| Pane closed | focus fails; no fallback guess |
| Local daemon restart | remote stream re-announces live bindings |
| Remote daemon restart | fresh snapshot and bindings replace prior state |
| SSH EOF/half-open timeout | remote rows disappear; local rows remain |
| Slow consumer | later full snapshot repairs skipped state |
| Duplicate title without marker | not navigable |
| WezTerm SSHMUX | same binding path yields local remapped pane |
| Multiple SSHMUX clients | ambiguity fails closed |
| Wayland/Hyprland | existing WM backend raises exact marked window |

## 12. Manual acceptance checks still required

These are empirical checks that automated tests cannot answer. They are not
reasons to add architecture up front:

1. Does a bounded write to the discovered remote TTY reliably deliver OSC 1337
   while Claude/Codex owns the alternate screen?
2. Does the implemented clear-then-set sequence reliably produce a fresh
   `user-var-changed` set event for the same value?
3. Does `wezterm.procinfo.pid()` match the GUI socket owner and Hyprland client,
   and does `[sbw:gui-pid:window-id]` remain present and unique while tabs, pane
   titles, and windows change?
4. Does the exact same variable arrive at the client callback through the
   configured SSHMUX topology?

If ordinary SSH fails question 1, stop and inspect the terminal path. Do not
fall back to hostname/CWD/process heuristics. If it succeeds, the rest is
ordinary snapshot subscription and existing local navigation machinery.

## 13. Recommended next step

Run the live Phase 0 acceptance next, specifically with two ordinary SSH panes
on the same host and cwd. Confirm PTY OSC delivery, clear-then-set behavior,
GUI/socket/WM PID agreement, marker uniqueness, pane moves, and stale pane
closure before declaring end-to-end remote focus proven. Then repeat the same
path through SSHMUX if that topology matters.

The code deliberately still has no socket forwarding, general federation state
model, or command channel. If a live check fails, fix the exact TTY-to-pane or
pane-to-WM-window seam instead of adding a second routing design.
