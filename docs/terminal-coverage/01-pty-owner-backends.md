# Phase 01 — pty-owner backends: foot + Alacritty

**Status: done.** Commits `98ea605` (seam), `85b2f63` (foot), `42eebfb`
(Alacritty on a shared scan). Deployed and verified end to end with foot.

This file records what was built and the procedures later phases reuse.

## What landed

- `internal/terminal/ptyowner.go` — `ptyOwnerLocator` (tty → terminal pid via the
  pty master's fdinfo) and `processScan` (shared, TTL-cached `/proc` walk by exe
  basename, with a per-use exe re-check against pid reuse).
- `internal/terminal/foot.go`, `alacritty.go` — the two backends. `Activate`
  returns `ErrUnsupported`: the window is the pane.
- `internal/mapping/mapping.go` — title-less panes join the WM on pid alone.
- `internal/rpc/rpc.go` — `ErrUnsupported` from `Activate` means no finer step.
- `auto` probes tmux → wezterm → foot → alacritty; `-terminal foot|alacritty`.

## Coverage

| Configuration | Result |
|---|---|
| plain `foot` / `alacritty` (Omarchy's launch) | exact window focus — full jump, no tabs to select |
| `foot --server` + footclient, `alacritty msg create-window` | exact while the process hosts one window; Observe-only when it hosts several (phase 06) |
| tmux inside either | pane selected, window not raised (phase 03) |

Omarchy facts (from `omacom/omarchy` branch `quattro` @ b18ab49): Super+Return →
`omarchy-launch-terminal` → `setsid uwsm-app -- xdg-terminal-exec`; the default
desktop entry is `foot.desktop` with `Exec=foot`; Alacritty's is `Exec=alacritty`.
No footclient or foot-server reference exists in the repo.

## Remaining in this phase

- **Alacritty live check.** Not installed on the dev box (installing needs sudo).
  Run the live conformance and the end-to-end check below once it is. The code
  path is shared with foot, which passed both.

## End-to-end check

Reusable by every phase; substitute the terminal's launch command.

```bash
mkdir -p ~/.cache/sb-e2e
(setsid foot --title sb-e2e -D ~/.cache/sb-e2e claude >/dev/null 2>&1 &); sleep 7
term=$(pgrep -x foot | tail -1)
agent=$(cat /proc/$term/task/*/children | tr ' ' '\n' | head -1)
switchboard-ctl --json list | jq -c ".sessions[] | select(.pid==$agent) | .hyprland.address"
switchboard-ctl focus pid:<another session>; switchboard-ctl focus pid:$agent
hyprctl activewindow -j | jq -r .address        # must equal the address above
kill $term; rm -r ~/.cache/sb-e2e               # then focus back where you were
```

The idle `claude` makes no API call until prompted, but it does record a short
session in history. Never `pkill -f` a pattern that also appears in your own
command line — it kills the calling shell (exit 144).

## Outcome

Foot: live conformance passed (owned-tty assertion ran); end-to-end jump passed
twice (before and after the shared-scan refactor). Snapshot cost 0.6 ms on 364
pids. Alacritty: unit and fixture tests only.
