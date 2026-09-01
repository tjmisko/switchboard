# Bar recipes

Switchboard's `state.json` is a stable public contract (see
[`../state-schema.md`](../state-schema.md)), so **any** status bar can render
your coding-agent sessions — it just reads the file. The daemon rewrites
`~/.cache/switchboard/state.json` atomically on every change, so a bar can poll
it or watch it for modifications.

`switchboard-polybar` and the Waybar integration are native streaming clients.
The eww and i3blocks snippets below consume the contract directly with `jq` and
remain reference recipes.

Renderer activation is machine-local. If the same configuration repository is
used on more than one computer, follow the
[machine-specific configuration](../machine-configuration.md) rules and opt in
to exactly one renderer unit per graphical host.

The one-line summary most bars want — `<count> <worst-status>`:

```bash
# count + the most-attention-needing status across all sessions
jq -r '
  (.sessions // []) as $s
  | ($s | length) as $n
  | ($s | map((.claude // .codex).status // "unknown")) as $st
  | (if   ($st | index("permission")) then "permission"
     elif ($st | index("idle"))       then "idle"
     elif ($st | index("working"))    then "working"
     else "idle" end) as $worst
  | "\($n) \($worst)"
' ~/.cache/switchboard/state.json
```

## Waybar

The Hyprland bottom strip keeps ten real GTK modules (and therefore per-chip
CSS and click targets) without ten resident renderers. `switchboard-ctl
bottombar watch` owns one aggregate subscription, renders all slots, and
atomically replaces `$XDG_RUNTIME_DIR/switchboard/slot-N.json`. Waybar v0.15's
signal mode runs one shell-builtin read at startup and again only after that
slot's bytes change; it does not exec a separate reader binary.

Copy [`../../waybar/claude.jsonc`](../../waybar/claude.jsonc) to
`~/.config/waybar/claude.jsonc` and use
`systemd/switchboard-waybar.service`. Keep `signal: 1..10`, omit `interval`, and
leave `exec-on-event: false`; otherwise clicks cause redundant reads. Slot zero
writes the exact Waybar parent PID to `bottom-waybar.ready`. The broker checks
that acknowledgement before sending RT signals, retaining dirty slots until it
can safely catch up after startup.

Treat the binary and config as one cutover: install the signal-mode config
before restarting `switchboard-waybar.service`. A new watcher paired with the
old `switchboard-waybar --slot` config cannot receive readiness and leaves the
old ten renderers in place. The watcher bounds its fast readiness retries, logs
the mismatch, then probes only every three seconds, but that is a rollout guard,
not a supported steady state.

The example keeps the appliance's right-click picker and middle-click rename
bindings under `$HOME/.config/scripts`, and resolves the control client through
`$HOME/.local/bin`, so it does not depend on the systemd user manager's `PATH`.

This Hyprland extra currently targets glibc Linux (`SIGRTMIN=34`) and uses
pidfds (Linux 5.3+) so a recycled numeric PID can never receive a realtime
update intended for Waybar.

`switchboard-waybar` remains a standalone/debug wrapper, but the live config
must not run it per slot.

## polybar

The supported renderer subscribes to the daemon and emits a single formatted
line containing every session. Status colors use Polybar tags; each navigable
chip has its own left-click action. Right-click opens the picker, and scrolling
cycles sessions. It holds one process open for `tail = true`, emits a muted `✕`
while the daemon is unavailable, and reconnects without causing a respawn loop.

Install the standalone bottom-bar configuration:

```bash
go install ./cmd/switchboard-polybar ./cmd/switchboard-ctl
cp polybar/switchboard.ini ~/.config/polybar/switchboard.ini
polybar -c ~/.config/polybar/switchboard.ini switchboard
```

For a direct i3 launch—and to give the daemon the graphical environment
required for navigation—add:

```i3
exec_always --no-startup-id systemctl --user import-environment DISPLAY XAUTHORITY I3SOCK
exec_always --no-startup-id systemctl --user restart switchboard.service
exec --no-startup-id polybar -c ~/.config/polybar/switchboard.ini switchboard
```

For a systemd-owned renderer with visible status and restart behavior, use
`systemd/switchboard-polybar.service` instead of the last line. The
machine-configuration guide shows the corresponding host activation; do not
use both owners at once.

To embed the module in an existing Polybar instead, copy the
`[module/switchboard]` section from
[`../../polybar/switchboard.ini`](../../polybar/switchboard.ini) and add
`switchboard` to that bar's `modules-*` list.

The renderer accepts `--max-sessions` (`0` means unlimited), `--ctl`, `--socket`,
and one `--*-color` flag per status. Polybar has no tooltip surface, so detailed
session information remains available through `switchboard-ctl pick`,
`switchboard-ctl list`, or `claude-tui`.

### Polling fallback

If the native binary is not installed, this minimal aggregate recipe polls the
public state file:

```ini
[module/switchboard]
type = custom/script
exec = ~/.config/polybar/switchboard.sh
interval = 1
click-left = switchboard-ctl focus active
```

```bash
# ~/.config/polybar/switchboard.sh
f=~/.cache/switchboard/state.json
[ -f "$f" ] || { echo ""; exit 0; }
read -r n worst < <(jq -r '
  (.sessions // []) as $s | ($s|length) as $n
  | ($s | map((.claude // .codex).status // "unknown")) as $st
  | (if ($st|index("permission")) then "permission"
     elif ($st|index("idle")) then "idle"
     elif ($st|index("working")) then "working" else "idle" end) as $w
  | "\($n) \($w)"' "$f")
[ "$n" = 0 ] && { echo ""; exit 0; }
case "$worst" in
  permission) icon="%{F#e06c75}●%{F-}";;
  idle)       icon="%{F#e5c07b}●%{F-}";;
  *)          icon="%{F#98c379}●%{F-}";;
esac
echo "$icon $n"
```

## i3blocks

```ini
[switchboard]
command=~/.config/i3blocks/switchboard.sh
interval=2
markup=pango
```

```bash
# ~/.config/i3blocks/switchboard.sh
f=~/.cache/switchboard/state.json
[ -f "$f" ] || exit 0
jq -r '(.sessions // []) | length as $n
  | if $n == 0 then "" else "claude: \($n)" end' "$f"
```

## eww

`eww` can watch the file with `deflisten` so it updates the instant the daemon
writes (no polling):

```lisp
(deflisten claude :initial "0 idle"
  "while true; do \
     jq -r '(.sessions // []) as $s | ($s|length) as $n \
       | ($s | map(.claude.status // \"unknown\")) as $st \
       | (if ($st|index(\"permission\")) then \"permission\" \
          elif ($st|index(\"idle\")) then \"idle\" \
          elif ($st|index(\"working\")) then \"working\" else \"idle\" end) as $w \
       | \"\\($n) \\($w)\"' ~/.cache/switchboard/state.json; \
     inotifywait -qq -e close_write ~/.cache/switchboard/state.json 2>/dev/null || sleep 1; \
   done")

(defwidget claudechip []
  (label :text {claude}))
```

## TUI

For a no-bar environment (SSH, tmux, a tiling-WM scratchpad), use the bundled
reference renderer instead of a bar:

```bash
claude-tui              # live full-screen list
claude-tui -once        # print one frame and exit (scriptable)
```
