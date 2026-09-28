# Terminal coverage — jump-to-focus on every Omarchy terminal

**Goal.** From any session chip, keybind or `switchboard-ctl focus`, land in the
native terminal window with the agent's prompt focused — the exact tab and split
where the terminal has them — for every terminal Omarchy 4 ships: foot (default),
Alacritty, kitty and Ghostty, with and without tmux. No secondary UI: the
switcher's whole job is putting the user in front of the real prompt.

Each phase below is one file, sized for one agent run. Run them in order; a
phase's preconditions name what it needs from earlier ones.

## Status and order

| # | Phase | Status | Verifiable on the dev box? |
|---|---|---|---|
| 01 | [pty-owner backends: foot + Alacritty](01-pty-owner-backends.md) | **done** (`98ea605`, `85b2f63`, `42eebfb`) | foot yes; Alacritty not installed |
| 02 | [kitty backend](02-kitty.md) | ready | yes (kitty 0.47.4 installed) |
| 03 | [tmux → window bridge](03-tmux-window-bridge.md) | ready | yes (tmux 3.7c) |
| 04 | [Hyprland focus edge cases](04-hyprland-focus-edges.md) | ready | partly (Hyprland 0.56.2; no pre-Lua build) |
| 05 | [Ghostty backend](05-ghostty.md) | **blocked** on a Ghostty release | no (not installed) |
| 06 | [Shared-process disambiguation](06-shared-process-disambiguation.md) | **needs an owner decision** | yes |
| 07 | [Omarchy 4 acceptance](07-omarchy-acceptance.md) | after 02–04 | no — needs an Omarchy 4 x86_64 install |
| 08 | [herdr](08-herdr.md) | **done** (`0810878`, `972cfea`) | yes (herdr 0.9.1) |

Why this order:

- **02 kitty** is the next largest gap: Omarchy ships it as an option and its
  default `kitty.conf` already enables a per-process remote-control socket, so
  full tab/split focus works with no user configuration.
- **03 tmux** is bound to Super+Alt+Return on Omarchy and cuts across every
  terminal; it composes with 01/02, so it follows them.
- **04** hardens the WM step every backend depends on (special workspaces, the
  pre-Lua dispatcher) before acceptance testing.
- **05 Ghostty** waits for upstream: the only usable focus IPC is on `main`.
- **06** covers the configurations where one process hosts several windows
  (foot server, `alacritty msg`, Ghostty single-instance). It writes into the
  agent's terminal, which is a product decision, not an engineering one.
- **07** is the acceptance run on a real Omarchy install, which the dev box
  (Fedora Asahi, aarch64) is not.
- **08 herdr** was taken out of order at the owner's request. It added the
  generic `PaneRef.HostTTY` client bridge that **03 tmux** should reuse.

## Architecture the phases build on

Read `internal/terminal/terminal.go`, `internal/mapping/mapping.go` and
`internal/rpc/rpc.go` (`focusLocalTarget`) before starting any phase.

- **The join chain.** agent pid → controlling tty (kernel, exact) → terminal
  pane (`terminal.Locator`) → WM window (`mapping.matchUniqueClient`). Focus
  raises the WM window, then asks the terminal to `Activate` the pane.
- **pty-owner backends** (`internal/terminal/ptyowner.go`). For a terminal with
  no pane below the window: the process holding a pty's master drives it, and
  `/proc/<pid>/fdinfo/<fd>` names the slave (`tty-index: N`). One `processScan`
  walks `/proc` by exe link (0.34 ms / 364 pids), cached 250 ms, shared by every
  such backend. Reuse it for any new backend that needs the terminal pid.
- **Title-less window join.** A pane with empty `WindowTitle` joins the WM on the
  terminal pid alone: exact when that process owns one window, fail-closed
  (no match, prior address kept) when it owns several.
- **Window-only terminals.** `Activate` returning `terminal.ErrUnsupported`
  means "no step finer than the window"; focus succeeds iff the WM step acted.
- **auto.** `terminal.NewAuto` probes backends every call, innermost first
  (tmux, then outer terminals). Outer backends must never both claim a tty.

## Rules for every phase agent

- **Bound the run.** Stop at the phase's definition of done or its stop
  conditions; do not start the next phase. Write the outcome into the phase
  file's `## Outcome` section (what landed, commits, what was verified live,
  what was not, open questions) — the file is the deliverable, not the reply.
- **Branch.** Work in a worktree: `gh worktree create --branch terminal/<phase>`
  (lives at `.worktrees/terminal/<phase>`). Conventional, atomic commits.
- **Tests alongside code**, named `should <behavior> when <condition>`, over
  fixtures (`newFakeProc` in `internal/terminal/foot_test.go` builds a
  `/proc`-shaped tree). Every new backend also gets a `RunLocatorContract` run
  in `internal/conformance/terminal_test.go`.
- **Live verification is required where the box allows it.** The live
  conformance gate is `SWITCHBOARD_LIVE_CONFORMANCE=1`. Build the test binary
  first (`go test -c`) and open the test window immediately before running it —
  a short-lived window can exit during compilation and the owned-tty assertion
  then skips silently. The end-to-end check is in [01](01-pty-owner-backends.md#end-to-end-check).
- **Never touch the owner's terminal configs.** Test windows take `-o`/`--override`
  flags; scratch files go under `~/.cache/` or the session scratchpad, never
  `/tmp` build caches.
- **Deploy only with `scripts/deploy`** from this checkout. `go install` does not
  deploy, and `~/Tools/switchboard` is a stale clone of the same module —
  building there silently overwrites the real binaries.
- **Uncertainty is stated, not resolved by guessing.** Facts below marked
  *unverified* came from source reading, not a live run; confirm them first.
