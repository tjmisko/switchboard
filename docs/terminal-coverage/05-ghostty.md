# Phase 05 — Ghostty backend

**Status: blocked** on a Ghostty release that contains upstream PRs #12027 and
#11762. Estimate 1–1.5 days once unblocked. Not verifiable on the dev box
until Ghostty is installed (not packaged there; building needs Zig).

**Goal.** Resolve an agent's tty to its Ghostty surface (a split) and focus it —
selecting the tab, focusing the split and raising the window.

## Unblock check (do this first, then stop if still blocked)

```bash
git ls-remote --tags https://github.com/ghostty-org/ghostty | tail -5
```

Unblocked when the newest tag is later than `v1.3.1` (2026-03-13) and its
`src/Surface.zig` injects `GHOSTTY_SURFACE_ID`. If still blocked, record the
date checked in `## Outcome` and stop. Optionally decide the 1.3.x fallback
(below) — that part can ship without the release.

## Facts

Verified in Ghostty source (`main` @ 12752b2, 2026-09-28) unless marked.

- **Surface id in the child env:** every surface has a random nonzero u64 id,
  exported as `GHOSTTY_SURFACE_ID=0x<16 hex>` (`src/Surface.zig`). PR #12027,
  merged 2026-03-31 — **not in v1.3.1**.
- **Focus action over D-Bus:** the GTK GApplication exports `present-surface`
  (parameter type `t`, u64) via `org.gtk.Actions` at bus name = the `class`
  config (default `com.mitchellh.ghostty`), object `/com/mitchellh/ghostty`.
  Handler: select the tab, grab focus on the split, `gtk_window_present` (which
  requests xdg-activation on Wayland). PR #11762, merged 2026-08-09, switched its
  argument from a raw pointer to the surface id. **In v1.3.1 the argument is a
  raw memory pointer**, unusable from outside.
- Expected call (**unverified** syntax):
  `gdbus call --session --dest com.mitchellh.ghostty --object-path /com/mitchellh/ghostty --method org.gtk.Actions.Activate present-surface '[<uint64 0x…>]' '{}'`
- **No enumeration API** on Linux (the AppleScript/App Intents surface is
  macOS-only). Upstream prefers a future cross-platform text protocol over D-Bus
  (Discussion #2353).
- **Process model:** `gtk-single-instance` defaults to `detect` — single
  instance (one process for every window) when launched from a desktop
  launcher/keybind, a separate process when launched from inside a terminal or
  with CLI args. **Unverified:** in a non-unique instance, whether its actions are
  still reachable on its unique bus name (`:1.NNN`, found via
  `org.freedesktop.DBus.GetConnectionUnixProcessID`).
- OSC 1337 SetUserVar is parsed but unimplemented — no user-var marker trick.

## Design

1. **tty → Ghostty process:** pty-owner scan, exe `ghostty` (window-level
   fallback, and the process that owns the bus name).
2. **tty → surface id:** read `GHOSTTY_SURFACE_ID` from the environ of the
   agent, walking up its ancestors to the Ghostty process (the shell has it even
   if the agent scrubbed it). Absent ⇒ window-level only.
3. **Activate:** D-Bus `present-surface` with the id. Either add a pure-Go D-Bus
   client (`github.com/godbus/dbus/v5`; the module deliberately has two
   dependencies and leans pure-Go so `go install` and cross-compiles stay
   clean — see `docs/portability-plan.md`) or shell out to `gdbus`, as the
   wezterm backend shells out to `wezterm cli`. Justify the choice. The handler returns nothing on an unknown id,
   so verify the focus landed (active window + the Ghostty window title).
4. **WM join:** single-instance Ghostty is one pid for every window, so the
   pid-only join is ambiguous with 2+ windows. Rely on `present-surface`
   raising the window (needs `misc:focus_on_activate`, see phase 04) and treat
   the WM address as optional.
5. **Ghostty 1.3.x:** window-level only, and ambiguous under single-instance.
   Options: accept Observe-only there, or phase 06's tagging. Record the choice.

## Tests

- should read the surface id from the nearest ancestor that carries it
- should activate present-surface with the surface id when focusing
- should fall back to window-level focus when no surface id is exported
- should target the unique bus name when the instance is non-unique (after verifying the fact)
- live: two windows, a tab and a split; agent in the split; jump; assert focus

## Definition of done

Unblocked release installed; tests and live check pass; README and
`-terminal` flag updated; deployed.

## Outcome

_Not started._ Last blocked check: 2026-09-28 (newest tag v1.3.1).
