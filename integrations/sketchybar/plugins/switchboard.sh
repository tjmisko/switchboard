#!/usr/bin/env bash
# Render switchboard sessions as SketchyBar chips.
#
# SketchyBar is the macOS answer to Seam 4 (see docs/portability-plan.md). Like
# the waybar renderer it consumes ONLY the public contract -- the RPC snapshot
# that `switchboard-ctl --json list` prints -- so it stays ignorant of every
# platform detail below it.
#
# Invoked two ways:
#   switchboard.sh update   rebuild the chip set from the current snapshot
#   switchboard.sh focus N  jump to the session with pid N (chip click_script)
set -uo pipefail

SB_CTL="${SWITCHBOARD_CTL:-$HOME/.local/bin/switchboard-ctl}"
PREFIX="sb.session"

# Status colours, 0xAARRGGBB. These mirror the semantics the daemon assigns, not
# a theme: red is "needs you now", orange "waiting on you", green "busy".
COLOR_PERMISSION=0xfff7768e # red    -- blocked on a permission prompt
COLOR_IDLE=0xffe0af68       # orange -- finished, awaiting input
COLOR_WORKING=0xff9ece6a    # green  -- actively working
COLOR_DELEGATING=0xff7aa2f7 # blue   -- driving subagents
COLOR_UNKNOWN=0xff565f89    # grey   -- status not yet known
COLOR_LABEL=0xffc0caf5
COLOR_FOCUSED_BG=0xff414868

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

cmd_update() {
  local snapshot
  # A daemon that is down is a normal state, not an error: say so and stop.
  if ! snapshot="$("$SB_CTL" --json list 2>/dev/null)" || [ -z "$snapshot" ]; then
    sketchybar --set switchboard.summary label="switchboard: down" icon.color="$COLOR_UNKNOWN" \
      >/dev/null 2>&1
    drop_stale ""
    return 0
  fi

  local count navigate
  count=$(jq -r '.sessions | length' <<<"$snapshot")
  navigate=$(jq -r '.capabilities.navigate' <<<"$snapshot")

  # The summary carries the capability tier, because on macOS "nothing happened
  # when I clicked" is usually Navigate being unavailable rather than a bug.
  local summary="$count session(s)"
  [ "$navigate" = "false" ] && summary="$summary · observe only"
  sketchybar --set switchboard.summary label="$summary" icon.color="$COLOR_LABEL" >/dev/null 2>&1

  local args=() keep=""
  while IFS=$'\t' read -r pid status label focused navigable; do
    [ -z "$pid" ] && continue
    local item="$PREFIX.$pid"
    keep="$keep $item"
    local color; color=$(color_for "$status")
    local bg=0x00000000
    [ "$focused" = "true" ] && bg="$COLOR_FOCUSED_BG"
    # A non-navigable session still renders -- it is observable, just not
    # jumpable. Dimming it is honest; hiding it would lose the session.
    local text="$label"
    [ "$navigable" = "false" ] && text="$label ·"

    # Item position is always left/center/right -- it is independent of which
    # screen edge the BAR itself is on.
    args+=(--add item "$item" left
           --set "$item"
             label="$text" label.color="$color"
             background.color="$bg" background.corner_radius=4
             background.drawing=on background.height=20
             click_script="'$0' focus $pid")
  done < <(jq -r '
    .sessions[]
    | [ (.pid|tostring),
        ((.claude.status // .codex.status) // "unknown"),
        (.resolved_name // (.cwd | split("/") | last) // "?"),
        (.focused|tostring),
        ((.navigable // false)|tostring) ]
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
