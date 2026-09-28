# Phase 08 — herdr

**Status: done** (`0810878`, `972cfea`, branch `terminal/herdr`, deployed).
Verified live on the dev box (herdr 0.9.1).

For how herdr and Switchboard divide the work overall, see
[../herdr.md](../herdr.md).

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

- **Focus flag granularity** (fixed on `fix/herdr-pane-focus`). Every agent in
  one herdr client window used to read as focused while that window was
  active. The watcher now follows herdr's focused pane (`pane.list`'s
  `focused`, then `pane.focused` events) into `HerdrInfo.ActivePaneID`, the
  counterpart of WezTerm's `HyprlandInfo.ActivePaneID`, and `showsInWindow`
  narrows by it. A tab switch inside herdr moves no OS window, so the herdr
  status loop re-derives focus itself when the focused pane moves. herdr's
  focus is per server, not per client: with two client windows on one server,
  whichever window is active highlights herdr's focused pane.
- **Trusted focus requests.** herdr's event stream polls every 100 ms
  (`CONNECTION_POLL_INTERVAL`), so `pane.focused` trails a `pane.focus` ack by
  up to that much, while the window raise before it fires a WM focus event
  within a few milliseconds. Waiting for herdr lit the previous pane's chip
  first, then moved it. A focus Switchboard requests is now trusted before any
  of it runs (`claimHerdrFocus`, through `rpc.SetFocusIntent`): the watcher
  records the target pane, and focus is re-derived against the active window,
  so the raise's WM event already lights the right chip. herdr moves its focus
  before it acks, so its later event confirms the claim and changes nothing. A
  request herdr refuses is corrected by re-listing its panes.
- **herdr's own latency.** A tab switch made with herdr's keys still reaches
  the chips up to 100 ms late, through that stream poll. herdr's request reader
  polls the same way: 19 of 100 of Switchboard's `pane.list` calls took about
  101 ms against 1.3 ms at the median (measured 2026-09-28). Both are herdr's to
  fix, by waking on an event or a readable socket instead of sleeping.
- **herdr inside a WezTerm tab.** A herdr session has no `Wezterm` block (its
  own pane is herdr's), so the WezTerm tab layer does not narrow it: a herdr
  client in a background WezTerm tab still reads as shown while that WezTerm
  window is active. Closing this needs the host pane id carried through the
  chain's window join.
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
