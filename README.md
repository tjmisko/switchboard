# Switchboard

**See all your coding-agent sessions at a glance, including their child agents,
and jump between the roots.**

You run Claude Code or Codex in a few different projects and lose track of which
root is working, which child is waiting on you, and where each terminal lives.
Switchboard is a small daemon that discovers the interactive root processes on
your machine — no pre-naming or registration — and tells you, in real time,
what each root and its nested agent threads are doing.

It installs as a single binary, **detects your environment at runtime**, and
degrades gracefully: it works on any Linux box with zero desktop configuration,
and lights up click-to-focus when it recognizes your window manager and
terminal.

> **Provenance.** Switchboard's design and architecture are mine; the Go implementation was built by AI coding agents under my direction, growing out of a shell prototype I hand-wrote.

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
root session with its cwd, status, nested agent rows, and (if resolved)
workspace. No window manager, bar, or terminal integration required.

```
switchboard · 3 sessions · navigate · wm=hyprland term=wezterm

  * ● permission  ~/Projects/switchboard                   pid 4821  ws 4
      ├─ ● reviewer          active · 18s
      └─ ● metadata          approval · 3m
    ● idle        ~/Tools/other                            pid 5102  ws 2
    ○ unknown     ~/scratch                                pid 5390
```

Only the root lines are navigation targets. Child agents do not own independent
terminal targets or Switchboard sessions; they appear as indented, non-focusable
rows in the TUI and as bounded detail in Waybar tooltips.

Prefer your own UI? Read `~/.cache/switchboard/state.json` directly — see the
[schema](docs/state-schema.md) and [bar recipes](docs/bars/README.md) for
polybar / eww / i3blocks. Every chip's tooltip also shows how long the session
has held its current status (`idle · 3m`, `permission · 45s`), from the
additive `status_since` field.

## Activity history & timeline (opt-in)

Switchboard can also *remember*: an append-only log of every status transition,
session lifecycle, subagent fan-out, and token-usage sample, so you can see — over
time — when and how hard your agents worked.

It is **off by default** (it records when and where you work). Turn it on with one
file, `$XDG_CONFIG_HOME/switchboard/history.json`:

```json
{ "enabled": true, "detail": "minimal", "retain_days": 90, "max_bytes": 104857600 }
```

The log is one JSON-per-line file per local calendar day under
`$XDG_STATE_HOME/switchboard/history/`, local-only, with a privacy tier
(`minimal` omits cwd / task descriptions). Inspect and render it — no daemon
needed:

```bash
switchboard-ctl history tail            # recent events
switchboard-ctl timeline                # per-session swimlanes + attention stats
switchboard-ctl timeline --json         # the stable contract for a future dashboard
```

`timeline` draws each session as a colored bar over time (parallel sessions
overlap) and reports the three "hours of agent attention" figures — union
wall-clock, per-session sum, and fan-out-weighted — plus subagents launched and
tokens used. See the [activity-log schema](docs/history-schema.md) and the
[design plan](docs/usage-history-plan.md).

## How it works

```
                  ┌────────────────────────────────────────────┐
                  │            switchboard (daemon)            │
  OS process  ───►│  discovery: interactive roots   (Observe)  │
   layer          │  death watch: pidfd/kqueue → drop session  │
                  │                                            │
  WM seam     ───►│  window lifecycle + focus  (Navigate)      │
  terminal    ───►│  pane locate + focus       (Navigate)      │
  providers   ───►│  root + nested agent graph                 │
  hooks       ───►│  exact identity / fallback enrichment      │
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

- **Discovery is the source of truth for roots.** Provider observers and hooks
  enrich a discovered root with status and nested threads; they never create a
  second switchable session for a child. If an enrichment source fails,
  Switchboard still knows the root exists and, on a Navigate stack, which
  window owns it.
- **Death is observed, never inferred.** Each tracked PID has a kernel death
  handle (`pidfd_open(2)` on Linux); the session disappears the instant the
  process dies, however it died (Ctrl+C, `/exit`, kill, OOM, shell hangup).

### Mapping (Navigate)

The join from a `claude` PID to a focusable window is anchored on the
**controlling tty**, which the kernel can't lose:

```
root PID   ──/proc──► cwd, tty, pidfd death signal
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
  discovery/   1 Hz interactive Claude/Codex root classifier
  hyprland/    Hyprland IPC client (wrapped by wm/hyprland)
  wezterm/     wezterm multi-mux cli client (wrapped by terminal/wezterm)
  mapping/     orchestrates proc → pane → window
  state/       in-memory store + atomic state.json mirror
  rpc/         Unix socket: list / focus / subscribe / hook
  agentgraph/  provider-neutral root/child graph + status reducer
  provider/    Claude and Codex graph observers
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
switchboard-ctl attention           # first permission, else first idle, else cycle green if all green (repeat to cycle the tier)
switchboard-ctl pick                # pid<TAB>label<TAB>ws<TAB>cwd lines (for fzf)
switchboard-ctl diagnose --observer # content-free binding/freshness/graph health
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
The Codex observer separately accepts `-codex-observer auto|off` (default
`auto`).

## Claude Code hooks (optional status enrichment)

Status colors come from Claude Code hooks. Without them, sessions still appear
(Observe) but show `unknown` status. In `~/.claude/settings.json`:

```json
{
  "hooks": {
    "SessionStart":      [{ "hooks": [{ "type": "command", "command": "switchboard-ctl hook SessionStart",      "timeout": 2 }] }],
    "UserPromptSubmit":  [{ "hooks": [{ "type": "command", "command": "switchboard-ctl hook UserPromptSubmit",  "timeout": 2 }] }],
    "PostToolUse":       [{ "hooks": [{ "type": "command", "command": "switchboard-ctl hook PostToolUse",       "timeout": 2 }] }],
    "PermissionRequest": [{ "hooks": [{ "type": "command", "command": "switchboard-ctl hook PermissionRequest", "timeout": 2 }] }],
    "Stop":              [{ "hooks": [{ "type": "command", "command": "switchboard-ctl hook Stop",              "timeout": 2 }] }],
    "SubagentStart":     [{ "hooks": [{ "type": "command", "command": "switchboard-ctl hook SubagentStart",     "timeout": 2 }] }],
    "SubagentStop":      [{ "hooks": [{ "type": "command", "command": "switchboard-ctl hook SubagentStop",      "timeout": 2 }] }]
  }
}
```

The forwarder is fire-and-forget; a broken hook can never corrupt state or
block Claude Code.

`SubagentStart`/`SubagentStop` are **optional** — they make subagent-fanout
detection real-time by triggering an immediate re-scan instead of waiting for the
next reconcile tick. Fanouts are still detected without them (the daemon scans
the authoritative `subagents/` metadata directory every tick), so these two only
reduce the latency on the `delegating` chip and the "N agents" count. The hook is
a pure trigger; the daemon's Observer remains the single source of truth, so a
duplicated or dropped subagent hook can never miscount.

## Codex hooks, display names, and read-only observation

Switchboard discovers ordinary interactive **OpenAI Codex** TUI processes while
excluding non-interactive subcommands such as `exec`, `mcp`, and
`app-server`. Run Codex normally with `codex`; navigation remains rooted in
the OS process and its controlling tty. Codex child agents are nested graph
nodes and never become focus/cycle/pick targets of their own.

Trusted Codex hooks provide the exact conversation ID, immediate root status,
and the bounded context used for display naming. Configure
`~/.codex/hooks.json` with the standard command:

```json
{
  "description": "Switchboard Codex identity, naming, and fallback status",
  "hooks": {
    "SessionStart": [{"hooks": [{"type": "command", "command": "switchboard-ctl codex-hook SessionStart", "timeout": 2}]}],
    "UserPromptSubmit": [{"hooks": [{"type": "command", "command": "switchboard-ctl codex-hook UserPromptSubmit", "timeout": 2}]}],
    "PreToolUse": [{"hooks": [{"type": "command", "command": "switchboard-ctl codex-hook PreToolUse", "timeout": 2}]}],
    "PostToolUse": [{"hooks": [{"type": "command", "command": "switchboard-ctl codex-hook PostToolUse", "timeout": 2}]}],
    "PermissionRequest": [{"hooks": [{"type": "command", "command": "switchboard-ctl codex-hook PermissionRequest", "timeout": 2}]}],
    "Stop": [{"hooks": [{"type": "command", "command": "switchboard-ctl codex-hook Stop", "timeout": 2}]}],
    "SubagentStart": [{"hooks": [{"type": "command", "command": "switchboard-ctl codex-hook SubagentStart", "timeout": 2}]}],
    "SubagentStop": [{"hooks": [{"type": "command", "command": "switchboard-ctl codex-hook SubagentStop", "timeout": 2}]}]
  }
}
```

This three-level event → matcher group → handler shape follows the
[official OpenAI hooks documentation](https://learn.chatgpt.com/docs/hooks).
Codex requires non-managed command hooks to be reviewed and trusted; use
`/hooks` after adding or changing the file. A PID is always paired with its
process-lifetime `started_at`, and hook identities retired by `/clear` cannot
rotate a chip back to an older conversation. The fallback maps
`UserPromptSubmit` and ordinary `PreToolUse`/`PostToolUse` to active,
`PermissionRequest` to an approval wait, and `Stop` to idle. A
`request_user_input` `PreToolUse` instead opens a user-input wait keyed by its
opaque `tool_use_id`; only the matching `PostToolUse`, its turn's `Stop`, or a
new conversation clears that wait. Unrelated tool hooks and generic app-server
snapshots cannot repaint it green. `SessionStart(clear|startup|resume)` is held
for 250 ms so an immediate same-thread continuation replaces the provisional
idle edge, while a standalone `/clear` still settles idle.
`SessionStart(compact)` remains active because Codex continues the model
immediately after compaction. Only content-free lifecycle metadata is used to
correlate the wait; its question, answer, and raw tool input stay out of the
daemon. The root-status hook fallback is partial and root-only. Active evidence
remains fresh for 10 minutes, approval or user-input waits for 24 hours, and idle edges
for 7 days. A daemon restart during an already-open hook-only interview cannot
reconstruct that wait and therefore remains unknown rather than confidently
green. The two child hooks are required for live child state on the tested
standard-CLI path, but not for discovery or topology. Matched running child
edges expire after 10 minutes without a later edge; matched completions persist
within the exact root lifetime and can be reopened by a later start. See the
[no-wrapper child-lifecycle decision](docs/codex-no-wrapper-child-lifecycle.md).

After the first usable prompt/`Stop` pair in a conversation, Switchboard asks
an isolated ephemeral naming turn for a 2–5-word lowercase kebab-case label (at
most 40 characters). Both the prompt and final assistant message are trimmed to
1,000 Unicode characters, used only in memory, and never written to state,
history, diagnostics, or logs. The default model is `gpt-5.6-luna`; select a
different one with `-codex-autoname-model`. Invalid or failed output is retried
once, then replaced by a deterministic fallback.

Only the resulting `display_name` record is persisted. It is bound to the
exact Codex conversation and shown before the native graph nickname.
Switchboard never changes Codex's native thread name. When a later
authoritative app-server observation reports a native name different from the
baseline captured for the display record, the generated record is cleared, so
`/rename` wins.

In `-codex-observer auto` mode, the generic observer binds exclusively from an
exact hook-supplied thread ID. It may read that thread and its descendants and
consume native-name notifications, but exposes no visible-thread write path. If
observation is disabled or temporarily unavailable, hooks still drive status
and display-name generation; native-rename precedence takes effect when an
authoritative name observation next becomes available.

Automatic degradation is fail-open for the root and fail-closed for status:

- app-server capability/start/connection failure leaves discovery and navigation working;
- the last complete graph remains visible only until its explicit freshness
  deadline, then its summary becomes unknown/stale rather than freezing a
  confident color;
- the observer reconnects with bounded exponential backoff and resnapshots;
- `-codex-observer off` never constructs the standalone app-server. Codex hooks
  can still supply the partial root view, but child hooks cannot synthesize
  topology; without hooks the root remains visible with unknown status and no
  proven live children.

Use `switchboard-ctl diagnose --observer` to inspect content-free binding,
snapshot freshness, completeness, node counts, observer mode, display-name
origin, and finite error categories. It never prints cwd, transcripts, thread
labels, prompts, assistant content, commands, or raw provider payloads.

## Requirements

- **Observe:** Linux with `pidfd_open(2)` (kernel 5.3+). Go 1.25 to build.
- **Codex graph observer:** a locally verified Codex CLI 0.149.0+ installation
  exposing `codex app-server --stdio`; otherwise use `-codex-observer off` or
  the automatic degraded root-only behavior above.
- **Navigate:** a supported WM (Hyprland / sway / i3 / X11) **and** terminal
  (wezterm / tmux) on `PATH`.
- macOS support (Observe tier) is planned (see the plan).

## Status / roadmap

Done: runtime-detecting `osproc` / `terminal` / `wm` seams behind a reusable
conformance contract; Hyprland + sway/i3 + X11/EWMH WM backends; wezterm + tmux
terminal backends with per-session locator chaining; capability reporting;
`claude-tui` reference renderer; provider-neutral root/child agent graphs;
Codex app-server observation with bounded degradation; Claude compatibility
projection; canonical agent history/timeline output; the Hyprland + waybar
two-bar appliance (appendix).

Next: macOS OS backend (`libproc` + `kqueue`); verified polybar/eww recipes;
the tmux→WM-window focus bridge.

---

## Appendix — the Hyprland + waybar appliance

The original Switchboard was a Hyprland + wezterm + waybar appliance. That
integration still ships as a Hyprland-specific extra; the portable core above
does not depend on it.

### Waybar — two bars, two processes

The top bar and the bottom agent strip run as **separate waybar processes** so
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

Waybar's row does not wrap, so each slot abbreviates its label to fit the
monitor (`internal/barlayout`). The fit shares out *glyph cells*, not pixels —
every chip pays the same fixed overhead, so what remains is one pool the row
divides max-min fair, with the remainder handed back a cell at a time rather
than rounded off into the bar's margin. That overhead is the chip's CSS box, so
`barlayout.DefaultMetrics` has to mirror `style.css` and `claude.jsonc`:

```
ChipFixedPx = 2×padding + 2×border + 2×margin + spacing
            =    2×7    +   2×1    +   2×2    +    2     = 22
```

Change the chip padding, margin, or the bar's `spacing` and you must update
`DefaultMetrics` to match, or the labels will be cut short (too high) or spill
off the edge of the bar (too low).

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
bottom bar runs  ⟺  (top bar visible)  AND  (≥1 agent session)
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
