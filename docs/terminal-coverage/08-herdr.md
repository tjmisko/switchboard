# Phase 08 — herdr

**Status: done** (`0810878`, `972cfea`, branch `terminal/herdr`, deployed).
Verified live on the dev box (herdr 0.9.1).

**Goal.** An agent in a [herdr](https://herdr.dev) pane jumps fully: herdr
selects the pane, the WM raises the window of a terminal running an attached
herdr client, and an outer terminal with tabs shows the client's tab.

## Facts (verified live unless marked)

- herdr is client/server: `herdr server` owns every pane's pty; TUI clients
  (`herdr`, `herdr --session NAME`, `herdr session attach NAME`) attach through
  `<session dir>/herdr-client.sock`, and the JSON API listens on `herdr.sock`
  next to it. herdr's own rule: client socket = `<dir>/<api stem>-client.sock`.
- The API serves one newline-delimited JSON request per connection.
  `pane.list` has no tty. `pane.process_info` names the pane's `shell_pid`,
  whose controlling tty is the pane's pty.
- A successful external `pane.focus` / `agent.focus` moves **every** attached
  client to the pane's tab (`focus_all_shell_clients_on_default_target` in
  `src/server/headless/client_views.rs`), and marks the agent seen. So any client
  window is a correct raise target. herdr tracks the last-used client
  (`foreground_client_id`) but does not expose it, or any client pid or tty.
- The kernel's unix socket table (NETLINK_SOCK_DIAG, what `ss -x` reads; no
  privilege needed) pairs each client's socket with the server's accepted end,
  which carries the client-socket path. That joins a client to its server
  exactly, whatever session name or `HERDR_SOCKET_PATH` override is in play.
- herdr strips `WEZTERM_PANE`, `TMUX`, `KITTY_*` and similar from pane env, so
  the tty is the only join key, as for every other backend.

## Design (as built)

- `PaneRef.HostTTY` (new, not persisted): the tty a separate client process
  draws this pane on. The chain resolves it through its other backends and
  copies the outer pane's `Mux`/`WindowID`/`WindowTitle`, on both the Locate and
  Snapshot paths. `Activate` focuses the pane, then each outer pane hosting its
  client, skipping `ErrUnsupported` hosts. Hops are bounded at 4. Phase 03
  (tmux) can set `HostTTY` from `list-clients` and reuse this unchanged.
- `internal/terminal/herdr.go`: servers and clients from the herdr processes'
  fd tables joined with the socket table; `pane.list` per server; a pane's tty
  from its shell's `stat` tty_nr, cached by (socket, terminal_id), accepted only
  while the server holds that pty's master. Shares the pty-owner `/proc` scan.
- With several clients attached, the most recently started one hosts the jump.
- auto order: tmux, herdr, wezterm, foot, alacritty. `-terminal herdr` forces it.

## Verified

- Unit tests over fixtures (topology, caching, recycled pid, detached server,
  side-by-side sessions, API callers not mistaken for clients, transport) and a
  real-kernel test of the socket-table join. Full suite passes.
- Live conformance (`SWITCHBOARD_LIVE_CONFORMANCE=1`) for herdr and auto.
- Snapshot cost with two live servers: ~0.5–0.9 ms warm, ~3 ms cold.
- End to end, private session `sbtest`, idle `claude` in tab 1, tab 2 focused:
  - client in foot: jump raised the foot window, herdr switched to the pane.
  - second client in a WezTerm window, covered by another WezTerm tab: mapping
    moved to the newer client, the jump raised the window, activated the tab
    hosting the client (`[sbp:22]` → `[sbp:21]`) and herdr selected the pane.

## Not covered / open

- **Focus flag granularity.** Every agent in one herdr client window reads as
  focused while that window is active: `applyFocus` refines by pane only for
  WezTerm. herdr reports `focused` per pane, but using it needs a state field,
  which is a schema decision.
- **Client choice** is "most recently started", a stand-in for herdr's
  last-used client. An upstream `client.list` API (pid, tty, last activity)
  would make it exact.
- **Remote machines:** panes on saved SSH machines, and `herdr --remote`
  clients, are out of scope. Unverified: a local client showing a remote
  machine's view when a local pane is focused.
- **macOS/Windows:** no sock_diag, so herdr reports no panes there.
- **Agent state:** herdr's own working/blocked/done states are not read, and
  Switchboard reports nothing back to herdr (`pane.report_metadata`).

## Outcome

Landed as above. Branch `terminal/herdr` is deployed but not merged to main.
