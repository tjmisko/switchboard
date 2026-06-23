# Switchboard

**See all your Claude Code sessions at a glance, and jump between them.**

You run `claude` in a few different projects and lose track of which is working,
which is waiting on you, and where each one lives. Switchboard is a small daemon
that discovers every `claude` process on your machine — no pre-naming, no
tagging, no registration — and tells you, in real time, how many are running,
their working directories, and what each one is doing.

It installs as a single binary, **detects your environment at runtime**, and
degrades gracefully: it works on any Linux box with zero desktop configuration,
and lights up click-to-focus when it recognizes your window manager and
terminal.

## Capability tiers

Everything hangs on two tiers. Switchboard never hard-fails on a missing
integration — it just offers what the environment supports.

| Tier | What you get | Needs | Availability |
|------|--------------|-------|--------------|
| **Observe** | live count, working directory, and per-session status (working / idle / waiting-on-permission) | nothing but the OS process layer | **always** — the floor |
| **Navigate** | click or keybind to focus a specific session's window + pane | a supported window manager **and** terminal | when both are detected; otherwise degrades to Observe |

`state.json` (the [stable public contract](docs/state-schema.md)) is emitted in
every tier, so any bar or script can render your sessions. The headline is the
Observe tier: **runs anywhere, zero desktop config, any bar can render it.**

### Detected backends

One binary; it probes the environment and picks backends live (build tags are
used only for the OS syscall layer).

| Seam | Backends | Detection |
|------|----------|-----------|
| **OS process** (Observe floor) | Linux `/proc` + `pidfd` · macOS *(planned)* | per-OS at build |
| **Window manager** (Navigate) | Hyprland · sway · i3 · X11/EWMH · `none` | `HYPRLAND_INSTANCE_SIGNATURE` → `SWAYSOCK` → `I3SOCK` → `DISPLAY` |
| **Terminal** (Navigate) | wezterm · tmux · `none` | tmux server socket · wezterm gui sockets (composes when nested) |

The daemon logs its chosen stack at startup and records it in the
`capabilities` block of `state.json`.

## Quickstart (Observe tier, one command after install)

```bash
go install github.com/tjmisko/switchboard/cmd/...@latest

# run the daemon (foreground; see the systemd unit below for a managed service)
switchboard &

# watch your sessions live — in any terminal, even over SSH
claude-tui
```

`claude-tui` is the reference renderer: a zero-dependency live list of every
session with its cwd, status, and (if resolved) workspace. No window manager,
bar, or terminal integration required.

```
switchboard · 3 sessions · navigate · wm=hyprland term=wezterm

  * ● working     ~/Projects/switchboard                   pid 4821  ws 4
    ● idle        ~/Tools/other                            pid 5102  ws 2
    ○ unknown     ~/scratch                                pid 5390
```

Prefer your own UI? Read `~/.cache/switchboard/state.json` directly — see the
[schema](docs/state-schema.md) and [bar recipes](docs/bars/README.md) for
polybar / eww / i3blocks.

## How it works

```
                  ┌────────────────────────────────────────────┐
                  │            switchboard (daemon)            │
  OS process  ───►│  discovery: comm == "claude"  (Observe)    │
   layer          │  death watch: pidfd/kqueue → drop session  │
                  │                                            │
  WM seam     ───►│  window lifecycle + focus  (Navigate)      │
  terminal    ───►│  pane locate + focus       (Navigate)      │
  claude hooks ──►│  RPC "hook": enrich status                 │
                  │                                            │
                  │  → ~/.cache/switchboard/state.json         │
                  │  → $XDG_RUNTIME_DIR/switchboard.sock (RPC) │
                  └────────────────────────────────────────────┘
                        │            │             │
                        ▼            ▼             ▼
                   claude-tui   switchboard-ctl   any bar
                  (reference)   (focus/cycle/…)  (reads state.json)
```

Two load-bearing invariants:

- **Discovery is the source of truth.** Hooks are pure enrichment — if your
  `~/.claude/settings.json` lost its hooks tomorrow, Switchboard would still
  know every session exists and (on a Navigate stack) which window owns it. It
  would only lose the working/idle/permission status.
- **Death is observed, never inferred.** Each tracked PID has a kernel death
  handle (`pidfd_open(2)` on Linux); the session disappears the instant the
  process dies, however it died (Ctrl+C, `/exit`, kill, OOM, shell hangup).

### Mapping (Navigate)

The join from a `claude` PID to a focusable window is anchored on the
**controlling tty**, which the kernel can't lose:

```
claude PID ──/proc──► cwd, tty, pidfd death signal
   tty      ──terminal seam──► pane (mux, pane id, window title)
   mux+title──WM seam──► window address, workspace   (opaque, backend-owned ref)
```

The tty match is bulletproof; the `(mux, title)` join to the WM window is
best-effort and returns nothing rather than guessing on a collision (it retries
next tick). A session that can't be mapped stays in the Observe tier.

## Layout

```
cmd/
  switchboard/        daemon — fans the signal sources into one store
  switchboard-ctl/    CLI — list / focus / cycle / pick / hook / bottombar
  claude-tui/         reference TUI renderer (subscribe → live list)
  switchboard-waybar/ waybar exec module — one process per slot (Hyprland extra)

internal/
  osproc/      Seam 1 — OS process layer (enumerate + death watch; per-OS)
  terminal/    Seam 2 — terminal locator (wezterm, tmux, auto, none, chain)
  wm/          Seam 3 — window manager (hyprland, sway/i3, x11, none)
  detect/      runtime backend selection + capability reporting
  proc/        Linux /proc reader — pid metadata (cwd, tty, comm, state)
  discovery/   1 Hz claude scan filter
  hyprland/    Hyprland IPC client (wrapped by wm/hyprland)
  wezterm/     wezterm multi-mux cli client (wrapped by terminal/wezterm)
  mapping/     orchestrates proc → pane → window
  state/       in-memory store + atomic state.json mirror
  rpc/         Unix socket: list / focus / subscribe / hook
  conformance/ backend-agnostic contract suites reused by every backend
  testsupport/ fixtures (fake conn, fake /proc, real-child death helpers)
```

The portability design and phase plan live in
[`docs/portability-plan.md`](docs/portability-plan.md).

## `switchboard-ctl`

```bash
switchboard-ctl list                # human-friendly snapshot
switchboard-ctl --json list         # raw JSON
switchboard-ctl status              # one-line count
switchboard-ctl focus active        # jump to the focused session
switchboard-ctl focus pid:<n>       # jump to a specific PID (unambiguous)
switchboard-ctl focus idx:<n>       # jump to the Nth session (unambiguous)
switchboard-ctl focus <n>           # PID n if present, else index n (back-compat)
switchboard-ctl cycle next|prev     # focus next/prev session, wrapping
switchboard-ctl attention           # first permission, else first idle (repeat to cycle the tier)
switchboard-ctl pick                # pid<TAB>label<TAB>ws<TAB>cwd lines (for fzf)
```

On an Observe-only stack, `focus` returns a clean "navigate unsupported"
message instead of failing obscurely.

## Run as a service

```bash
mkdir -p ~/.config/systemd/user
cp systemd/switchboard.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now switchboard.service
```

Force a backend (e.g. to test degradation) with the daemon flags
`-wm auto|hyprland|sway|i3|x11|none` and `-terminal auto|wezterm|tmux|none`.

## Claude Code hooks (optional status enrichment)

Status colors come from Claude Code hooks. Without them, sessions still appear
(Observe) but show `unknown` status. In `~/.claude/settings.json`:

```json
"hooks": {
  "SessionStart":      [{ "hooks": [{ "type": "command", "command": "switchboard-ctl hook SessionStart",      "timeout": 2 }] }],
  "UserPromptSubmit":  [{ "hooks": [{ "type": "command", "command": "switchboard-ctl hook UserPromptSubmit",  "timeout": 2 }] }],
  "PostToolUse":       [{ "hooks": [{ "type": "command", "command": "switchboard-ctl hook PostToolUse",       "timeout": 2 }] }],
  "PermissionRequest": [{ "hooks": [{ "type": "command", "command": "switchboard-ctl hook PermissionRequest", "timeout": 2 }] }],
  "Stop":              [{ "hooks": [{ "type": "command", "command": "switchboard-ctl hook Stop",              "timeout": 2 }] }]
}
```

The forwarder is fire-and-forget; a broken hook can never corrupt state or
block Claude Code.

## Codex hooks (optional status enrichment)

Switchboard also discovers **OpenAI Codex** TUI sessions (`comm == "codex"`,
excluding `exec`/`mcp`/`app-server`/… subcommands) and tracks their status the
same way. They reach the Navigate tier identically — the focus join keys on the
tty, which is agent-agnostic. Status comes from Codex's hooks via
`switchboard-ctl codex-hook <Event>`, which reads the hook's stdin JSON exactly
like the Claude forwarder.

Recent Codex ships a Claude-Code-style hooks system. In `~/.codex/hooks.json`
(or an inline `[hooks]` table in `~/.codex/config.toml`), wire each lifecycle
event to the codex forwarder:

```jsonc
// representative — confirm the exact file shape against the Codex hooks docs
{
  "hooks": {
    "SessionStart":      [{ "command": "switchboard-ctl codex-hook SessionStart" }],
    "UserPromptSubmit":  [{ "command": "switchboard-ctl codex-hook UserPromptSubmit" }],
    "PreToolUse":        [{ "command": "switchboard-ctl codex-hook PreToolUse" }],
    "PostToolUse":       [{ "command": "switchboard-ctl codex-hook PostToolUse" }],
    "PermissionRequest": [{ "command": "switchboard-ctl codex-hook PermissionRequest" }],
    "Stop":              [{ "command": "switchboard-ctl codex-hook Stop" }]
  }
}
```

The status mapping mirrors Claude's (`UserPromptSubmit`/`PreToolUse`/
`PostToolUse` → working, `PermissionRequest` → permission, `Stop`/`SessionStart`
→ idle). Two honest limitations today:

- **`permission` needs the live hook.** Codex does not record approval requests
  in its on-disk rollout, so a "blocked on approval" state is observable only via
  the `PermissionRequest` hook — there is no transcript-tail self-heal for it as
  there is for Claude. A Codex session with no hooks configured shows only
  `working`/`idle`.
- **Self-heal is Claude-only** for now. The idle↔working / stale-permission
  reconcilers read the Claude transcript format; the analogous Codex rollout
  parser is future work (see [`docs/codex-investigation.md`](docs/codex-investigation.md),
  phase C-4). Codex status therefore tracks the hooks directly.

Without any hooks, Codex sessions still appear (Observe) with `unknown` status,
exactly like an un-hooked Claude session.

## Requirements

- **Observe:** Linux with `pidfd_open(2)` (kernel 5.3+). Go 1.25 to build.
- **Navigate:** a supported WM (Hyprland / sway / i3 / X11) **and** terminal
  (wezterm / tmux) on `PATH`.
- macOS support (Observe tier) is planned (see the plan).

## Status / roadmap

Done: runtime-detecting `osproc` / `terminal` / `wm` seams behind a reusable
conformance contract; Hyprland + sway/i3 + X11/EWMH WM backends; wezterm + tmux
terminal backends with per-session locator chaining; capability reporting;
`claude-tui` reference renderer; the Hyprland + waybar two-bar appliance
(appendix).

Next: macOS OS backend (`libproc` + `kqueue`); verified polybar/eww recipes;
the tmux→WM-window focus bridge.

---

## Appendix — the Hyprland + waybar appliance

The original Switchboard was a Hyprland + wezterm + waybar appliance. That
integration still ships as a Hyprland-specific extra; the portable core above
does not depend on it.

### Waybar — two bars, two processes

The top bar and the bottom claude strip run as **separate waybar processes** so
the bottom one can be shown/hidden without touching the top. The split is done
with two config files:

- `~/.config/waybar/config.jsonc` — the top bar only (launched by `exec-once = waybar`).
- `~/.config/waybar/claude.jsonc` — the bottom strip only; **not** launched
  directly. Its lifecycle is owned by `switchboard-ctl bottombar`.

`claude.jsonc` declares 10 `custom/claude-N` modules so each chip is a real GTK
widget with its own CSS. Each runs `switchboard-waybar --slot N` and emits a
JSON line per snapshot; `class` carries status + `focused` + `suspended` so
`style.css` paints the chip. Click = focus that slot; right-click = rofi picker;
scroll = cycle.

A chip whose `claude` process is job-control-stopped (Ctrl-Z) gains the
`suspended` class on top of its status class. Grey it out in `style.css`:

```css
#custom-claude-0.suspended, #custom-claude-1.suspended, /* … through -9 */ {
  opacity: 0.4;
}
```

Hyprland startup wiring:

```
exec-once = systemctl --user import-environment HYPRLAND_INSTANCE_SIGNATURE WAYLAND_DISPLAY XDG_CURRENT_DESKTOP DISPLAY
exec-once = systemctl --user start --no-block switchboard.service
exec-once = switchboard-ctl bottombar watch
```

### Auto-hiding the bottom bar

```
bottom bar runs  ⟺  (top bar visible)  AND  (≥1 claude session)
```

Visibility is **process existence**, not a toggle: `switchboard-ctl bottombar`
literally starts and kills the `waybar -c claude.jsonc` process, so the two bars
never desync. The session-count input comes from the daemon stream (`bottombar
watch`, plus a 3 s self-heal ticker); the top-bar-visibility input comes from
the F8 master toggle, which touches a marker file and calls `bottombar
reconcile` so the bottom bar follows in lockstep. The watcher kills by process
group (no orphan slot subprocesses) and reaps them (no zombies).

Overridable via `SWITCHBOARD_WAYBAR_MARKER` and `SWITCHBOARD_BOTTOM_CONFIG`.
This auto-hide logic is deeply Hyprland-specific and stays an opt-in extra, not
part of the portable core.
