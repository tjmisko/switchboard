# Phase 07 — Omarchy 4 acceptance

**Status: after 02–04** (05 and 06 join the matrix when they land). Estimate
1 day of testing plus fixes. **Needs an Omarchy 4 install on x86_64** — a VM is
fine. The dev box is Fedora Asahi on aarch64 and is not Omarchy.

**Goal.** Prove the promise on the real target: on a stock Omarchy 4, every
supported terminal, launched the ways Omarchy launches it, jumps to the exact
prompt from a keypress.

## Setup

- Omarchy 4 (`quattro`), recording the exact version (`omarchy version` or the
  repo's `version` file) and Hyprland version.
- Install Switchboard with `scripts/deploy` from a checkout; add the Claude and
  Codex hooks from the main README. Use the stock `kitty.conf` (it enables
  remote control) — do not hand-edit terminal configs.
- Install the optional terminals through Omarchy's own menu (Install →
  Terminal) and switch with `omarchy-default-terminal <name>`.

## Matrix

Rows: foot · Alacritty · kitty · Ghostty (if phase 05 landed). For each row:

| Launch | How | Expected |
|---|---|---|
| plain | Super+Return, run `claude` | window focus (kitty/Ghostty: tab + split) |
| two windows | two Super+Return windows, agents in both | each jump lands in its own window |
| tabs/splits | kitty/Ghostty only: agent in a non-active tab's split | right tab and split |
| tmux | Super+Alt+Return twice (shared `Work`), agent in a pane | pane selected, a client window raised |
| agent launcher | Super+Shift+Ctrl+A (`omarchy-agent`, app-id `org.omarchy.agent`) | window focus |
| scratchpad | first agent from the Quake console (`special:scratchpad`) | special workspace shown, agent focused |
| other workspace | target on another workspace, source fullscreen | workspace switch, target raised |

For each cell drive the jump three ways: `switchboard-ctl focus pid:N`,
`switchboard-ctl attention` (with the target waiting on a permission prompt),
and `switchboard-ctl cycle next`. Pass = the active Hyprland window is the
target's and the target's prompt accepts keystrokes immediately.

## Also record

- Capabilities block per row (`switchboard-ctl --json list | jq .capabilities`).
- `misc:focus_on_activate` as Omarchy sets it.
- Daemon CPU while idle with each terminal open (`systemctl --user status
  switchboard` CPU time over 10 minutes) — the per-tick `/proc` walk and any
  `kitten @ ls` forks are the new costs.
- Anything Omarchy-specific that broke and the phase it belongs to.

## Definition of done

Every matrix cell passes or has a filed follow-up naming its phase; results
table recorded below with versions.

## Out of scope

The bar renderer. Omarchy 4's bar is a Quickshell (QML) process, not Waybar; an
Omarchy Shell plugin renderer is a separate plan.

## Outcome

_Not started._
