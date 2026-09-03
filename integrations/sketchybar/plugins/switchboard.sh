#!/usr/bin/env bash
# Render switchboard sessions as SketchyBar chips.
#
# SketchyBar is the macOS answer to Seam 4 (see docs/portability-plan.md). Like
# the waybar renderer it consumes ONLY the public contract -- the snapshot that
# `switchboard-ctl --json list` prints -- so it stays ignorant of every platform
# detail below it.
#
# Visual target is the Linux bottom bar: outlined pills floating on a
# transparent bar, where the status colour is the BORDER and the TEXT rather
# than a fill. Chips are centred and carry the full project name.
#
# Invoked two ways:
#   switchboard.sh update   rebuild the chip set from the current snapshot
#   switchboard.sh focus N  jump to the session with pid N (chip click_script)
set -uo pipefail

SB_CTL="${SWITCHBOARD_CTL:-$HOME/.local/bin/switchboard-ctl}"
PREFIX="sb.session"

# Gruvbox-material, matching the Linux bar. Status semantics come from the
# daemon, not from taste: red is "needs you now", amber "waiting on you", aqua
# "busy".
COLOR_PERMISSION=0xffea6962 # red    -- blocked on a permission prompt
COLOR_IDLE=0xffd8a657       # amber  -- finished, awaiting input
COLOR_WORKING=0xff89b482    # aqua   -- actively working
COLOR_DELEGATING=0xffd3869b # purple -- driving subagents
COLOR_UNKNOWN=0xff928374    # grey   -- status not yet known

# Menlo rather than SF Mono: SF Mono ships only as Terminal.app resources and as
# the private .SFNSMono system face, so SketchyBar cannot resolve it by family
# name and silently falls back to a proportional font. Menlo is a real installed
# family on every macOS.
CHIP_FONT="Menlo:Bold:12.0"

CHIP_FILL=0xcc1d2021        # dark translucent, so the pill reads on any wallpaper
CHIP_FILL_FOCUSED=0xff3c3836
COLOR_DIM=0xff928374

color_for() {
  case "$1" in
    permission) echo "$COLOR_PERMISSION" ;;
    idle)       echo "$COLOR_IDLE" ;;
    working)    echo "$COLOR_WORKING" ;;
    delegating) echo "$COLOR_DELEGATING" ;;
    *)          echo "$COLOR_UNKNOWN" ;;
  esac
}

cmd_focus() {
  "$SB_CTL" focus "pid:$1" >/dev/null 2>&1 || true
}

# The status item carries only what the chips cannot say themselves. A session
# count would duplicate the chips, so it stays hidden unless something is
# actually wrong or degraded -- which keeps the bar as clean as the target.
set_status() {
  local text="$1"
  if [ -z "$text" ]; then
    sketchybar --set switchboard.status drawing=off >/dev/null 2>&1
    return
  fi
  sketchybar --set switchboard.status drawing=on label="$text" label.color="$COLOR_DIM" \
    >/dev/null 2>&1
}

cmd_update() {
  local snapshot
  # A daemon that is down is a normal state, not an error: say so and stop.
  if ! snapshot="$("$SB_CTL" --json list 2>/dev/null)" || [ -z "$snapshot" ]; then
    set_status "switchboard: down"
    drop_stale ""
    return 0
  fi

  local navigate count
  navigate=$(jq -r '.capabilities.navigate' <<<"$snapshot")
  count=$(jq -r '.sessions | length' <<<"$snapshot")

  if [ "$count" = "0" ]; then
    set_status "no sessions"
  elif [ "$navigate" = "false" ]; then
    # Worth saying once, quietly: on stock macOS a click cannot raise a window,
    # and without this a no-op click reads as a bug rather than a missing
    # capability.
    set_status "observe only"
  else
    set_status ""
  fi

  local args=() keep=""
  while IFS=$'\t' read -r pid status label focused; do
    [ -z "$pid" ] && continue
    local item="$PREFIX.$pid"
    keep="$keep $item"
    local color fill
    color=$(color_for "$status")
    fill="$CHIP_FILL"
    [ "$focused" = "true" ] && fill="$CHIP_FILL_FOCUSED"

    # Item position is always left/center/right -- it is independent of which
    # screen edge the BAR itself is on.
    args+=(--add item "$item" center
           --set "$item"
             label="$label"
             label.color="$color"
             label.font="$CHIP_FONT"
             label.padding_left=8 label.padding_right=8
             background.drawing=on
             background.color="$fill"
             background.border_color="$color"
             background.border_width=1
             background.corner_radius=9
             background.height=22
             click_script="'$0' focus $pid")
  done < <(jq -r '
    .sessions[]
    | [ (.pid|tostring),
        ((.claude.status // .codex.status) // "unknown"),
        (.resolved_name // (.cwd | split("/") | last) // "?"),
        (.focused|tostring) ]
    | @tsv' <<<"$snapshot")

  [ ${#args[@]} -gt 0 ] && sketchybar "${args[@]}" >/dev/null 2>&1
  drop_stale "$keep"
}

# Remove chips for sessions that have ended. SketchyBar keeps items until told
# otherwise, so without this a dead session's chip lingers forever.
drop_stale() {
  local keep="$1" existing
  existing=$(sketchybar --query bar 2>/dev/null | jq -r '.items[]?' | grep "^$PREFIX\." || true)
  for item in $existing; do
    case " $keep " in
      *" $item "*) ;;
      *) sketchybar --remove "$item" >/dev/null 2>&1 ;;
    esac
  done
}

case "${1:-update}" in
  focus)  cmd_focus "${2:-}" ;;
  update) cmd_update ;;
  *)      echo "usage: $0 [update|focus <pid>]" >&2; exit 2 ;;
esac
