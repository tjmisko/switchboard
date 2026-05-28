# Bar recipes

Switchboard's `state.json` is a stable public contract (see
[`../state-schema.md`](../state-schema.md)), so **any** status bar can render
your Claude Code sessions — it just reads the file. The daemon rewrites
`~/.cache/switchboard/state.json` atomically on every change, so a bar can poll
it or watch it for modifications.

These recipes consume the contract directly with `jq`; they need no
Switchboard-specific plugin. They are **reference recipes** — the waybar
integration (see the main README) is the one verified end-to-end on the
author's setup; the polybar/eww/i3blocks snippets below are provided as
starting points and have not been run in CI. Corrections welcome.

The one-line summary most bars want — `<count> <worst-status>`:

```bash
# count + the most-attention-needing status across all sessions
jq -r '
  (.sessions // []) as $s
  | ($s | length) as $n
  | ($s | map(.claude.status // "unknown")) as $st
  | (if   ($st | index("permission")) then "permission"
     elif ($st | index("idle"))       then "idle"
     elif ($st | index("working"))    then "working"
     else "idle" end) as $worst
  | "\($n) \($worst)"
' ~/.cache/switchboard/state.json
```

## polybar

```ini
[module/claude]
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
  | ($s | map(.claude.status // "unknown")) as $st
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
