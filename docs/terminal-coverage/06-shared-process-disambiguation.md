# Phase 06 — Shared-process disambiguation

**Status: needs an owner decision** before any code. Estimate 1–2 days once
decided. Verifiable on the dev box with foot server mode.

**Goal.** Jump to a session whose terminal process hosts several windows, where
the pid-only window join is ambiguous and the session is Observe-only today:

| Configuration | Why ambiguous |
|---|---|
| `foot --server` + footclient | server owns every window's pty and Wayland connection |
| `alacritty msg create-window` / `alacritty --daemon` | same, one process |
| Ghostty 1.3.x single-instance | one process, no usable focus IPC |
| kitty with several OS windows and no socket | one process, no rc to ask |

None of these is Omarchy's default launch; this phase is for users who opted in.

## The decision needed

The only exact disambiguator available is to **write an escape sequence into the
session's own terminal**: bytes written by a same-uid process to `/dev/pts/N`
reach the terminal as output, so a unique tag appears on exactly one window,
which the WM then reports. The owner must decide whether Switchboard may do
this, and if so whether it is on by default or opt-in.

Risks to weigh:

- **Interleaving.** A write can land between two writes of the agent's own
  output. If the agent split an escape sequence across writes, ours aborts it and
  one frame renders wrong until the agent's next repaint. Tagging only when
  ambiguous, once per window lifetime, bounds the exposure.
- **Visible change.** A title tag shows in the title bar until restored.
- **Window rules.** Changing foot's app-id can re-trigger Hyprland rules keyed on
  class (*unverified*).
- **tmux swallows it** unless wrapped in passthrough, which reaches only the
  attached client's outer terminal. Out of scope here; phase 03 handles tmux.

## Facts

- **foot:** OSC 176 sets the Wayland app-id at runtime (`\e]176;<id>\e\\`; empty
  resets to default; since 1.17.0; debounced). OSC 0/2 set the title. The
  `toplevel-tag` option (Hyprland `xdgTag`) is creation-time only. Source:
  `osc.c`, `foot-ctlseqs.7.scd`.
- **Alacritty:** OSC 0/2 set the title when `window.dynamic_title` is on (the
  default); no escape changes the class.
- **Ghostty:** OSC 0/2 set the surface title; the window title follows the
  selected tab.
- Hyprland reports `class`, `title` and `xdgTag` per client in `hyprctl clients -j`.

## Design (if approved)

1. Trigger only when the join is ambiguous for a session with no bound address.
2. Tag with a nonce (`sb-<random>`): foot via OSC 176 (the title is untouched),
   others via OSC 2. Poll `Clients()` briefly for the one window carrying it.
3. Bind (tty → window address) for the window's lifetime; drop the binding when
   the address disappears or the tty's owner changes.
4. Restore: foot `\e]176;\e\\`; title terminals — restore the prior title read
   from `hyprctl` before tagging, knowing the agent may repaint it anyway.
5. Put the writer behind one interface so tests use a fake tty file.

## Tests

- should tag only when the pid-only join is ambiguous
- should bind the one window whose class carries the nonce
- should restore the default app-id after binding
- should never tag a tty that tmux owns
- should keep the binding when a later window opens on the same server
- live: `foot --server` with three footclient windows; jump to each session.

## Definition of done

Decision recorded here; implementation behind the chosen default; live foot
server check passes; README's "stays Observe-only" notes updated.

## Outcome

_Not started — awaiting decision._
