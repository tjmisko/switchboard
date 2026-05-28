# Switchboard

A discovery-first session tracker for Claude Code, designed for Hyprland + wezterm.

Tells you, at a glance and in real time, how many Claude Code sessions you have running, where they live, and what each one is doing — without requiring you to pre-name, pre-tag, or otherwise register them. You just run `claude` somewhere; the tracker figures it out.

The bottom waybar strip shows one rounded chip per session, color-coded by state (working / idle / waiting-on-permission), with the focused window's chip outlined. The strip auto-hides when no sessions exist and reappears the moment one does — see [Auto-hiding the bottom bar](#auto-hiding-the-bottom-bar).

## Architecture

```
                  ┌────────────────────────────────────────┐
                  │           switchboard (daemon)       │
                  │                                         │
  /proc scan ───►│  discovery (1 Hz): comm == "claude"     │
                  │                                         │
  pidfd_open ───►│  procwatch: POLLIN → drop session        │
                  │                                         │
  socket2.sock ─►│  hyprland: window lifecycle + focus      │
                  │                                         │
  claude hooks ─►│  RPC "hook" cmd: enrich Claude.Status    │
                  │                                         │
                  │  → ~/.cache/switchboard/state.json    │
                  │  → $XDG_RUNTIME_DIR/switchboard.sock  │
                  └────────────────────────────────────────┘
                              │              │
                              ▼              ▼
                       switchboard-waybar    switchboard-ctl
                       (one process     (one-shot CLI for
                       per slot 0..9)   picker / cycle / focus)
```

**Discovery is the source of truth.** Hooks are pure enrichment — if `~/.claude/settings.json` lost its hooks tomorrow, the tracker would still know every session exists, where its wezterm pane lives, and which Hyprland window owns it. The only thing it would lose is the working/idle/permission status colors.

**Death is observed, never inferred.** Each tracked PID has a `pidfd_open(2)` watching it; `POLLIN` fires the instant the kernel marks the process a zombie, regardless of how it died (Ctrl+C, `/exit`, kill, OOM, parent shell death). The wezterm pane keeps living; the chip just disappears.

## Mapping pipeline

For every discovered claude PID, the daemon assembles:

```
claude PID
  ├── /proc/<pid>/comm           "claude"          (filter)
  ├── /proc/<pid>/cwd            project dir
  ├── /proc/<pid>/fd/0..2        → "/dev/pts/N"
  └── pidfd_open                 death signal

mux_pid + tty_name
  └── wezterm cli --socket=… list  (per-mux walk under $XDG_RUNTIME_DIR/wezterm/)
      → pane_id, window_id, window_title

mux_pid + window_title
  └── hyprctl clients -j          (match by pid AND title)
      → hyprland address, workspace
```

The tty match is load-bearing (the kernel can't lose it). Window-title match is the weakest link — relies on wezterm pushing its title to the WM, which it does — and falls back gracefully on collision.

## Components

```
cmd/
  switchboard/       daemon — goroutines fan signal sources into one store
  switchboard-ctl/   CLI — list / focus / cycle / pick / hook / bottombar
  switchboard-waybar/        waybar exec module — one process per slot, emits JSON

internal/
  proc/                 /proc reader (comm, cwd, ppid, tty)
  discovery/            1 Hz scan filter
  procwatch/            pidfd_open + POLLIN per PID
  hyprland/             clients + dispatch + socket2 stream
  wezterm/              multi-mux cli list + activate-pane
  mapping/              orchestrates proc → pane → addr
  state/                in-memory store + atomic state.json mirror
  rpc/                  Unix socket: list / focus / cycle / pick / subscribe / hook

systemd/
  switchboard.service  user service, Restart=always
```

## Install

```bash
go install ./...
mkdir -p ~/.config/systemd/user
cp systemd/switchboard.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now switchboard.service
```

Then in `~/.config/hypr/hyprland.conf`, before any `exec-once` that needs Hyprland env in systemd-user-land:

```
exec-once = systemctl --user import-environment HYPRLAND_INSTANCE_SIGNATURE WAYLAND_DISPLAY XDG_CURRENT_DESKTOP DISPLAY
exec-once = systemctl --user start --no-block switchboard.service
```

## Integration points (this machine)

### Waybar — two bars, two processes

The top bar and the bottom claude strip run as **separate waybar processes** so the bottom one can be shown/hidden without touching the top. waybar's `--bar` flag does not filter a single array config (it loads every bar regardless), so the split is done with two config files:

- `~/.config/waybar/config.jsonc` — the top bar only. Launched at boot by `exec-once = waybar` (the default config path).
- `~/.config/waybar/claude.jsonc` — the bottom strip only. **Not** launched directly; its lifecycle is owned by `switchboard-ctl bottombar` (see below).

`claude.jsonc` declares 10 `custom/claude-N` modules so each chip is a real GTK widget with its own CSS (border, border-radius, hover). Each runs `switchboard-waybar --slot N` and emits a JSON line per snapshot update; `class` carries the status + `focused` so `~/.config/waybar/style.css` paints the chip. Empty slots collapse to zero width.

Click semantics:
- left  — focus that slot's session (`switchboard-ctl focus N`)
- right — open the rofi picker (`~/.config/scripts/claude-picker`)
- scroll up/down — cycle prev/next

### Auto-hiding the bottom bar

The bottom strip obeys one invariant:

```
bottom bar runs  ⟺  (top bar visible)  AND  (≥1 claude session)
```

Visibility is **process existence**, not a SIGUSR1 toggle: `switchboard-ctl bottombar` literally starts and kills the `waybar -c claude.jsonc` process. There is no toggle state to drift, so the two bars never alternate or desync.

Two inputs drive the invariant, each owned by a different actor:

- **session count** — the daemon. `switchboard-ctl bottombar watch` subscribes to the daemon stream (plus a 3 s safety/self-heal ticker) and shows/kills the bottom bar as sessions come and go. Run once at startup:

  ```
  exec-once = switchboard-ctl bottombar watch
  ```

- **top-bar visibility** — the F8 master toggle in `~/.config/scripts/hypr-float-center`. That script touches/removes a marker file to hide/show the top bar, then calls `switchboard-ctl bottombar reconcile` so the bottom bar follows in lockstep. The script also excludes the bottom bar's pid (recorded at `$XDG_RUNTIME_DIR/switchboard/bottom-waybar.pid`) when it SIGUSR1s the top bar, so toggling the top never touches the bottom.

The watcher kills the bottom bar by **process group**, so the 10 `switchboard-waybar` slot subprocesses die with it (no orphans), and reaps the resulting children so they never linger as zombies.

Overridable via env: `SWITCHBOARD_WAYBAR_MARKER` (default `/tmp/hypr-float-center/waybar-hidden`) and `SWITCHBOARD_BOTTOM_CONFIG` (default `~/.config/waybar/claude.jsonc`).

### Hyprland keybindings

```
bind = $mainMod, A, exec, ~/.config/scripts/claude-picker
bind = $mainMod $altMod, Up,    exec, switchboard-ctl cycle next
bind = $mainMod $altMod, Down,  exec, switchboard-ctl cycle prev
bind = $mainMod $altMod, Right, exec, switchboard-ctl cycle next
bind = $mainMod $altMod, Left,  exec, switchboard-ctl cycle prev
```

### Claude Code hooks (optional enrichment)

In `~/.claude/settings.json`:

```json
"hooks": {
  "SessionStart":      [{ "hooks": [{ "type": "command", "command": "switchboard-ctl hook SessionStart",      "timeout": 2 }] }],
  "UserPromptSubmit":  [{ "hooks": [{ "type": "command", "command": "switchboard-ctl hook UserPromptSubmit",  "timeout": 2 }] }],
  "PostToolUse":       [{ "hooks": [{ "type": "command", "command": "switchboard-ctl hook PostToolUse",       "timeout": 2 }] }],
  "PermissionRequest": [{ "hooks": [{ "type": "command", "command": "switchboard-ctl hook PermissionRequest", "timeout": 2 }] }],
  "Stop":              [{ "hooks": [{ "type": "command", "command": "switchboard-ctl hook Stop",              "timeout": 2 }] }]
}
```

The forwarder is fire-and-forget. Hook failures cannot corrupt state — they just leave a session at its previous status.

## Useful commands

```bash
switchboard-ctl list                # human-friendly snapshot
switchboard-ctl --json list         # raw JSON
switchboard-ctl status              # one-line count
switchboard-ctl focus active        # jump to currently-focused session
switchboard-ctl focus <pid>         # jump to specific session
switchboard-ctl focus <N>           # jump to Nth session (by start time)
switchboard-ctl cycle next|prev     # focus next/prev session, wrapping
switchboard-ctl pick                # pid<TAB>label<TAB>ws<TAB>cwd lines
```

Live state mirror at `~/.cache/switchboard/state.json` (atomic-rename writes); useful for ad-hoc scripts.

## Requirements

- Linux with `pidfd_open(2)` (kernel 5.3+)
- `wezterm` and `hyprctl` on PATH
- Go 1.25 for build
- `rofi` (for picker), `jq` (for picker)
- A Nerd Font is *not* required (CSS chips replaced the powerline-glyph approach)

## Status / roadmap

Working:
- `/proc` discovery, pidfd death detection
- wezterm multi-mux + Hyprland socket2 mapping
- waybar 10-slot strip with CSS borders
- rofi picker, cycle keybindings
- Claude Code hooks for status colors
- systemd user service
- bottom-bar auto-hide (`bottombar watch`/`reconcile`), split top/bottom waybar processes, F8 lockstep

Not yet:
- i3 port (would swap `internal/hyprland` for `internal/i3`; same RPC + same waybar binary)
- PID-pinned click selectors (today a click after a session-end race can target a neighbor; rare in practice)
