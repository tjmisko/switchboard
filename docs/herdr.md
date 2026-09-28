# herdr and Switchboard

[herdr](https://herdr.dev) is a terminal workspace manager for coding agents: a
background server owns every pane's pty, TUI clients attach to it from ordinary
terminal windows, and it classifies each pane's agent as working, blocked, done
or idle. Switchboard runs alongside it. This page records how the two divide
the work and where the line is. It was written against herdr 0.9.1 (source
read and behavior checked live, 2026-09-28).

For the implementation, see [terminal-coverage/08-herdr.md](terminal-coverage/08-herdr.md)
(jump-to-focus) and the `herdr` block in [state-schema.md](state-schema.md)
(status authority).

## Who does what

| | herdr | Switchboard |
|---|---|---|
| Pane selection inside herdr | yes (`pane.focus`, `agent.focus`) | asks herdr to do it |
| Raising the OS window showing the pane | no | yes, through the WM |
| Agent status for panes in herdr | yes, the authority | publishes herdr's status |
| Agent status outside herdr | no | yes (hooks, Codex app-server) |
| Agents Switchboard has no adapter for (Pi, OpenCode, Cursor, …) | detects ~20 of them | tracks them through herdr |
| Global (compositor-level) keybinds | no | yes |
| Waybar chips, focus history, cross-machine view | no | yes |

## What herdr cannot do: global keybinds and window focus

herdr has no equivalent of Switchboard's compositor-level keybinds, and cannot
build one on its own.

- **Its keybinds are client-local.** Every herdr binding is a prefix key
  (`ctrl+b …`) read by the herdr TUI client, so it fires only while that
  client's terminal already has keyboard focus. The `prefix+g` Goto picker can
  filter agents by blocked/working/idle/done, but it too lives inside the
  client. herdr registers no global hotkeys.
- **It has no window-manager integration.** The source contains no Hyprland,
  sway, X11 or other WM code. The nearest thing is `client.window_title.set`,
  which sets the title of the terminal hosting the foreground client: a WM can
  read that, but herdr cannot raise, focus or find a window.
- **`herdr agent focus` stops at the tab.** It switches every attached client
  to the pane's tab and marks the agent seen, but leaves the terminal window
  wherever it was.

### Could it be built by hand?

Partly. A Hyprland `bind` can run `herdr agent focus <pane>`, but a script
would still have to find which terminal window hosts a client of that herdr
server and focus it. That is the join Switchboard's herdr backend makes: the
kernel's unix socket table pairs each client with its server, and the client's
tty resolves to its terminal's window. Cycling to the next agent that needs
attention would also need a script over `herdr agent list`, since herdr has no
"next blocked agent" command.

Even then, it would not cover:

- agents outside herdr: plain foot, Alacritty or WezTerm panes, and tmux;
- more than one herdr session at a time (each is a separate server and socket);
- the bars, focus history, and the federated view of other machines.

So Switchboard stays the global layer, with herdr underneath it supplying pane
selection and agent state. Switchboard's existing keybinds (`switchboard-ctl
focus`, attention cycling, chip clicks) already work on sessions inside herdr.

## Status authority, in brief

For every agent in a herdr pane, herdr's status is the one Switchboard
publishes: working→working, blocked→permission, idle/done→idle. idle stays
delegating while the provider graph shows working subagents, since herdr reads
the screen and cannot see background agents. Updates arrive over herdr's event
stream (one subscription per server, re-subscribed whenever panes are created,
closed or moved). A server's statuses keep counting for 10 seconds after its
stream drops, then the provider's status takes over again.

Measured live with Claude Code: herdr's screen detection trails Claude's hooks by
about 0.5–1 s when work starts and when a permission prompt opens. It is faster
the other way: a declined prompt fires no hook, and herdr cleared that red
before Claude's own tracking did.

## Open follow-ups

- A Claude or Codex session publishes no status until its first hook lands its
  provider graph, although herdr already knows the pane's state. Closing this
  needs care in the provider binding (a herdr-sourced graph must never be
  mistaken for a provider session id).
- A Claude `SessionStart` hook that arrives before the 1 s process scan has seen
  the new process is dropped as unattributed. The bug predates herdr support,
  and was seen live while testing it.
- An agent herdr alone observes uses `herdr:<terminal id>` as its history
  session id.
- Not covered: herdr clients attached over SSH (`herdr --remote`), panes on
  saved SSH machines, and macOS/Windows (no sock_diag there).
