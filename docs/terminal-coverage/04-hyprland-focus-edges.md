# Phase 04 — Hyprland focus edge cases

**Status: ready.** Estimate 1 day. Partly verifiable on the dev box (Hyprland
0.56.2, Lua config); the pre-Lua path needs an older Hyprland.

**Goal.** The WM step every backend relies on works in the situations Omarchy
users actually hit: agents on special workspaces, fullscreen windows, and
Hyprland releases before the Lua config.

## Preconditions

None beyond phase 01. Independent of 02/03; can run in parallel with them.

## Facts

- `internal/hyprland/hyprland.go` dispatches **only** the Lua form:
  `hl.dsp.focus({window="address:0x…"})`, batched with
  `eval hl.config({cursor={no_warps=…}})`. Hyprland evaluates `dispatch`'s
  argument as Lua once the config is Lua, so the legacy
  `focuswindow address:0x…` is a syntax error there — and, conversely, the Lua
  form fails on a pre-Lua Hyprland. **There is no fallback today.**
- Omarchy's own `bin/omarchy-hyprland-focus-app` does
  `hyprctl dispatch "hl.dsp.focus({ window = \"address:$address\" })" || hyprctl dispatch focuswindow "address:$address"`
  — prior art for the fallback.
- Omarchy seeds the first agent onto a special workspace:
  `default/hypr/qconsole.lua` runs `[workspace special:scratchpad silent] omarchy-agent`.
  Every Omarchy agent window has app-id `org.omarchy.agent`.
- Hyprland's focus dispatcher switches workspace and applies no focus-stealing
  check (from source reading, not a live test). **Unverified:** whether focusing a
  window on a hidden special workspace shows that special workspace on the
  current monitor, or leaves it hidden with focus on an invisible window.
- `misc:focus_on_activate` governs whether a terminal's self-activation (kitty,
  Ghostty) focuses or merely marks urgent. It is `true` on the dev box;
  Omarchy's value is **unverified**.

## Work

1. **Special workspaces.** Put a test window on `special:sbtest`, hide it, jump
   to its session. If the special workspace stays hidden, add the toggle
   (`hl.dsp` form of `togglespecialworkspace`) before the focus, only when the
   target's workspace name starts with `special:` and it is not already shown.
   The workspace is already in the client list the reconcile fetched.
2. **Fullscreen.** Jump from a fullscreen window to a session on the same
   workspace; record whether the target is raised above it. Fix only if broken.
3. **Pre-Lua fallback.** On a dispatch error whose text shows a Lua parse
   failure, retry with the legacy `focuswindow address:0x…` and the legacy
   `keyword cursor:no_warps` batch; cache which dialect worked per daemon run so
   the fallback costs one failed request once, not per jump. There is no fake
   request-socket harness yet: stand one up as a unix listener under
   `t.TempDir()` (the `socketPath` tests show how the instance signature and
   `XDG_RUNTIME_DIR` select the path), replaying recorded replies from each
   dialect.
4. **focus_on_activate.** Record Omarchy's value (from `quattro` config) in this
   file's outcome; phases 02/05 depend on it.

## Tests

- should show a hidden special workspace when focusing a session on it
- should not toggle a special workspace that is already shown
- should retry with the legacy dispatcher when the Lua form fails to parse
- should remember the working dialect after the first fallback
- should keep the no-warp batch in whichever dialect succeeded

## Definition of done

Special-workspace jump verified live; fallback unit-tested against both
dialects' recorded responses; outcome records Omarchy's `focus_on_activate`
and the fullscreen behavior; deployed.

## Stop conditions / out of scope

Stop if the special-workspace toggle would need per-monitor state the WM seam
does not carry — record the gap. Out of scope: sway/i3/X11 equivalents.

## Outcome

_Not started._
