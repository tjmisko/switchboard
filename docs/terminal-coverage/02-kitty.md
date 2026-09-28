# Phase 02 — kitty backend

**Status: ready.** Estimate 1.5–2 days. Verifiable on the dev box (kitty 0.47.4,
`/usr/bin/kitten`).

**Goal.** A `kitty` terminal backend that resolves an agent's tty to its exact
kitty window (kitty's name for a split) and focuses it — selecting the tab and
split and raising the OS window.

## Preconditions

Phase 01 merged (the pty-owner scan and the title-less window join).

## Facts

Verified in kitty 0.47.4 source (`/usr/lib64/kitty/kitty/`) and Omarchy
`quattro` unless marked.

- **Remote control is opt-in.** It needs `allow_remote_control socket-only` (or
  `yes`) plus `listen_on`. If `listen_on` lacks `{kitty_pid}`, kitty appends
  `-<pid>` to the path. Not reloadable at runtime.
- **Omarchy enables it by default:** `etc/xdg/kitty/kitty.conf` sets
  `allow_remote_control socket-only` and
  `listen_on unix:${XDG_RUNTIME_DIR}/omarchy-kitty-{kitty_pid}`. On Omarchy the
  socket for kitty pid P is `$XDG_RUNTIME_DIR/omarchy-kitty-P`.
- **Enumerate:** `kitten @ --to unix:<sock> ls` → JSON: OS windows → tabs →
  windows. Window fields include `id`, `pid` (the child kitty spawned), `cwd`,
  `title`, `is_focused`, `foreground_processes` ({pid, cmdline, cwd}),
  `user_vars`. **No tty field.** OS windows carry `platform_window_id`, which is
  null on Wayland.
- **Focus:** `kitten @ --to unix:<sock> focus-window --match id:N` switches to
  the tab and focuses the split. kitty then raises its own OS window with an
  xdg-activation request (`glfw/wl_window.c`); whether Hyprland focuses or only
  marks it urgent depends on `misc:focus_on_activate` (*unverified* default).
- **Child environment:** `KITTY_WINDOW_ID` (the split id), `KITTY_PID`,
  `KITTY_LISTEN_ON` (only when a socket exists; `fd:N` for `launch
  --allow-remote-control`, useless externally). Env is an exec-time snapshot:
  lost under tmux, `env -i`, sudo, ssh.
- **Process model:** one process per `kitty` invocation, but OS windows created
  from inside kitty (`new_os_window`, `launch --type=os-window`) share its pid;
  `--single-instance` hands later invocations to the first process.
- Omarchy's `bin/omarchy-cmd-terminal-cwd` already special-cases kitty via the
  same socket plus `kitten @ ls` — prior art for the socket convention.

## Design

1. **tty → kitty process:** reuse the pty-owner scan (`processScan`, exe
   `kitty`). Exact, and shares the existing walk.
2. **Socket discovery,** in order: (a) the unix socket the kitty process is
   listening on — its fds `socket:[inode]` matched against `/proc/net/unix`
   (*unverified* approach; exact and convention-free); (b) `KITTY_LISTEN_ON` from
   a child's environ; (c) the Omarchy path. No socket ⇒ fall back to window-level
   behavior (pid-only join, `Activate` = `ErrUnsupported`) — never fail.
3. **tty → kitty window id:** for each `ls` window, read the tty of `pid` (and of
   each `foreground_processes` pid) from `/proc/<pid>/fd/0..2`; match the agent
   tty. Prefer this over `KITTY_WINDOW_ID` (env can be stale or absent).
4. **Window join:** fill `PaneRef` so `matchUniqueClient` works: `Mux` = kitty
   pid, `WindowTitle` = the OS window's active title if it can be matched, else
   empty (pid-only, exact when the process has one OS window — `ls` tells you the
   OS window count). Record `WindowID` = kitty OS window id, `TabID`, `PaneID`.
5. **Activate:** `focus-window --match id:<PaneID>`. Start by shelling out to
   `kitten` (like the wezterm backend shells out); speaking the rc protocol
   directly from Go is a later optimization, not this phase.
6. **Snapshot cost:** one `ls` per kitty process per reconcile. Measure it; the
   wezterm backend's `wezterm cli list` fork is the bar to beat or match.
7. **Focus tracking:** `applyFocus` compares `Wezterm.PaneID` with
   `Hyprland.ActivePaneID` only for wezterm. For kitty, decide whether to mark
   focus from `ls`'s `is_focused` (per split) — without it, every session in a
   focused kitty OS window reads as focused. Pick one; state why in the outcome.

## Tests (fixture-driven, then live)

- should resolve a tty to the kitty split whose child owns it when ls lists it
- should resolve through foreground_processes when the split's shell exec'd the agent
- should fall back to window-level focus when the kitty process has no socket
- should find the socket from the process's listening unix socket when listen_on is custom
- should activate the exact split by id when focusing
- should fail closed on the WM join when one kitty process owns several OS windows and no title matches
- conformance: `RunLocatorContract` for kitty
- live: launch test windows with
  `kitty -o allow_remote_control=socket-only -o listen_on=unix:$XDG_RUNTIME_DIR/sb-kitty-test`,
  two tabs and a split, an agent in the second tab's right split; jump from
  another session; assert the active Hyprland window and `ls` `is_focused`.

## Definition of done

Unit, conformance (fixture and live) and end-to-end checks pass; the jump lands
in the right tab and split in a multi-tab, multi-split kitty; README capability
table and `-terminal` flag updated; deployed with `scripts/deploy`.

## Stop conditions / out of scope

Stop and record if: xdg-activation does not raise the window and the WM join is
ambiguous (that is phase 04/06 territory); `ls` cost exceeds ~20 ms per process.
Out of scope: speaking the rc protocol natively, kitty inside tmux (phase 03).

## Outcome

_Not started._
