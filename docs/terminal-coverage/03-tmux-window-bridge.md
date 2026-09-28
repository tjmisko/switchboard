# Phase 03 — tmux → window bridge

**Status: ready.** Estimate 1–2 days. Verifiable on the dev box (tmux 3.7c).

**Goal.** A session running inside tmux, inside any supported terminal, jumps
fully: the pane is selected in tmux **and** the terminal window showing that
tmux session is raised. Today only the pane is selected (`internal/terminal/tmux.go`,
"Caveat (the tmux↔WM bridge, plan 3.3)"); `PaneRef.Mux` is 0, so the mapping
layer never joins a tmux session to a WM window.

## Preconditions

Phase 01 merged. Phase 02 is not required, but kitty-hosted tmux clients only
raise at window level until it lands.

## Facts

- **Omarchy:** Super+Alt+Return runs `omarchy-launch-terminal bash -c "tmux
  attach || tmux new -s Work"` — every such window is a client of one shared
  session `Work`.
- `tmux list-clients -F '#{client_pid} #{client_tty} #{session_name} #{client_activity}'`
  gives each client's own tty — a pty driven by the **outer** terminal, so the
  existing outer locators (wezterm, foot, alacritty, later kitty) resolve it to a
  window exactly.
- Clients attached to the same session share that session's current window, so
  after `select-window`/`select-pane` **every** client of that session shows the
  pane. Several clients is therefore not an ambiguity for correctness: raising
  any of them lands on the prompt. Choose deterministically (most recent
  `client_activity`, then lowest pid) so repeated jumps are stable.
- **Exception — grouped sessions** (`new-session -t`) share windows but keep
  independent current windows: only clients of the pane's own session are valid.
  A window linked into several sessions (`link-window`) has the same property.
- A pane with **no attached client** (detached session) has no window to raise:
  keep today's behavior (select the pane, report no window).

## Design

1. Extend the tmux listing with `#{session_name}` and `#{window_id}` per pane,
   and add a `list-clients` call to the same Snapshot (one extra fork per tick,
   only while a tmux server is up).
2. Compose in the terminal seam, not the mapping layer: after tmux resolves a
   pane, resolve the chosen client's tty through the **outer** locators and copy
   the outer pane's WM-join fields (`Mux`, `WindowID`, `WindowTitle`) into the
   tmux `PaneRef`, keeping `Backend: "tmux"` and the tmux `Handle`. The natural
   home is `chain`/`auto`, which already own the innermost-first order; keep the
   batch and single paths agreeing (the conformance batch contract checks it).
3. **Activate order:** select the tmux window/pane first, then the WM raise
   happens in `focusLocalTarget` (already WM-then-terminal; confirm the order
   does not matter for a shared session, or swap it and say why).
4. If the outer terminal itself has panes (wezterm, kitty), also activate the
   outer pane hosting the tmux client — otherwise the raise lands on the right
   window but the wrong tab. This is the "composes when nested" promise in the
   README capability table.
5. Focus tracking: a tmux-hosted session is focused when its client's window is
   active **and** tmux's current pane is this pane (`#{pane_active}` on the
   session's current window). Decide and document.

## Tests

- should raise the outer window of the attached client when a tmux pane is focused
- should prefer the most recently active client when several clients share the session
- should use only the pane's own session's clients when sessions are grouped
- should select the pane and report no window when the session is detached
- should activate the outer wezterm pane hosting the client when the terminal has tabs
- should agree between batch and single paths for tmux-hosted ttys (conformance)
- live: two foot windows attached to one session `sbtest` (`tmux -L sbtest` on a
  private socket, never the owner's server), agent in window 2; jump from
  elsewhere; assert active Hyprland window and `tmux display -p '#{pane_id}'`.

## Definition of done

Tests and live check pass for tmux-in-foot and tmux-in-wezterm; the tmux
package doc caveat is removed; README updated; deployed.

## Stop conditions / out of scope

Stop and record if the chain composition forces a `PaneRef` shape change that
leaks into `state.json` (that is a schema decision). Out of scope: remote tmux
over SSH federation (README: "Remote tmux attachment … intentionally outside"),
screen and zellij.

## Outcome

_Not started._
