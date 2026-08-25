# Bar recipes

Switchboard's `state.json` is a stable public contract (see
[`../state-schema.md`](../state-schema.md)), so **any** status bar can render
your coding-agent sessions — it just reads the file. The daemon rewrites
`~/.cache/switchboard/state.json` atomically on every change, so a bar can poll
it or watch it for modifications.

`switchboard-polybar` and the Waybar integration are native streaming clients.
The eww and i3blocks snippets below consume the contract directly with `jq` and
remain reference recipes.

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

For i3 startup—and to give the daemon the graphical environment required for
navigation—add:

```i3
exec_always --no-startup-id systemctl --user import-environment DISPLAY XAUTHORITY I3SOCK
exec_always --no-startup-id systemctl --user restart switchboard.service
exec --no-startup-id polybar -c ~/.config/polybar/switchboard.ini switchboard
```

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
