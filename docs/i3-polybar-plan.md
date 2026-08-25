# i3 + polybar support — implementation spec

Target stack: **i3** (X11) + **polybar** + **wezterm**. Goal is *basic
functionality*: the Navigate tier working under i3, and a polybar module that
renders sessions with click-to-focus. The Hyprland two-bar appliance
(auto-hide, ten fixed slots) is explicitly **not** in scope.

Status: implemented 2026-08-24. The native streaming renderer, i3 X11-PID
hydration, spinner-safe title join, service environment fallback, and dedicated
bottom-bar configuration now ship in this repository. Session-count-driven
auto-hide remains out of scope as described below.

---

## 0. Verified findings

Four independent gaps sit between the current build and a working i3 setup.
Each was confirmed on the target machine, not inferred from the source.

### F1 — The daemon never sees i3 (environment, not code)

`systemctl --user show-environment` contains no `DISPLAY`, `I3SOCK`, or
`XAUTHORITY`. i3 was started from a tty (`XDG_SESSION_TYPE=tty`), and nothing
ever imported the X session into the user manager. The daemon therefore probes
an empty environment and reports:

```
backends: wm=none terminal=wezterm observe=true navigate=false
```

Both `I3SOCK=/run/user/1000/i3/ipc-socket.832` and `DISPLAY=:0` *are* present in
an ordinary interactive shell. This is purely a systemd-environment gap.

### F2 — Auto-detection picks the one backend that cannot do the join

`internal/detect/detect.go:78` orders the probe Hyprland → `SWAYSOCK` →
`I3SOCK` → `DISPLAY`. Under i3, `I3SOCK` is set, so `wm.NewI3()` wins over
`wm.NewX11()`. But `internal/wm/i3.go:17` documents the fatal caveat:

> i3's GET_TREE does not expose a pid (only sway does), so under pure i3 the
> terminal↔window join cannot resolve and such sessions stay Observe-only.

The join at `internal/mapping/mapping.go:231` (`matchUniqueClient`) requires
`window.PID == pane.Mux` **and** `window.Title == pane.WindowTitle`. Under the
i3 backend `PID` is always `0`, so it never matches.

The X11/EWMH backend *does* populate PID, from `_NET_WM_PID`
(`internal/wm/x11.go:57`). Verified on the live session:

```
0x1800035  _NET_WM_PID = 1054  _NET_WM_NAME = "◑ i3 and polybar integration"
0x1800077  _NET_WM_PID = 1054  _NET_WM_NAME = "◐ Polybar battery percentage"
```

`1054` is exactly the wezterm mux pid `switchboard-ctl list` reports, and the
two windows carry distinct titles — precisely the `(pid, title)` pair
`matchUniqueClient` needs. **The X11 backend already works under i3 today; auto
detection just refuses to select it.**

### F3 — The join key animates

Claude Code writes a spinner into the pane title, wezterm propagates it to the
window title, and it is *inside* the join key. Sampled at 0.4s intervals on one
window:

```
"◑ Polybar battery percentage"
"◑ Polybar battery percentage"
"◐ Polybar battery percentage"
"◐ Polybar battery percentage"
```

The terminal seam and the WM seam read the title in separate syscalls at
separate instants. Whenever the glyph rotates between those two reads, the
exact-string compare fails and the session drops to Observe for that tick.
`matchUniqueClient` keeps the prior address and retries, so an established
mapping only flickers — but **initial** resolution becomes a coin flip per tick,
and a session whose title changes at the wrong moment can stall unmapped.

No normalization exists anywhere in `internal/mapping/` or `internal/terminal/`
(grepped for `Normaliz|TrimPrefix|spinner|◐◑◒◓` — no hits). This affects
Hyprland equally; i3 just makes it visible.

### F4 — No polybar integration exists

`docs/bars/README.md` carries a polybar `custom/script` recipe, explicitly
marked unverified and never run in CI. It polls `state.json` at `interval = 1`,
renders a single count, and has no per-session chips. There is no
`switchboard-polybar` analogue to `cmd/switchboard-waybar`.

---

## Phase 1 — Navigate tier under i3

### 1a. Get the X session into the daemon's environment  *(config, no code)*

In `~/.config/i3/config`:

```
exec_always --no-startup-id systemctl --user import-environment DISPLAY XAUTHORITY I3SOCK
exec_always --no-startup-id systemctl --user restart switchboard.service
```

`import-environment` alone is not enough — an already-running daemon keeps its
old (empty) environment, hence the restart.

**Also add a self-discovery fallback to the unit**, mirroring what
`systemd/switchboard.service` already does for Hyprland's lock file. The
Hyprland path exists because a user manager restarted on its own never
re-inherits the compositor environment; i3 has the identical failure mode. The
i3 socket is discoverable at `$XDG_RUNTIME_DIR/i3/ipc-socket.<pid>`, and
`DISPLAY` can default to `:0`:

```sh
# in ExecStart, before exec, when I3SOCK is unset:
for s in "$XDG_RUNTIME_DIR"/i3/ipc-socket.*; do
  [ -S "$s" ] || continue
  p=${s##*.}; [ -e "/proc/$p" ] || continue
  I3SOCK=$s; export I3SOCK; break
done
[ -n "$DISPLAY" ] || { DISPLAY=:0; export DISPLAY; }
```

Without `XAUTHORITY` the xgb connection will be refused, so it must be imported
even though the socket-discovery trick covers the other two.

**Definition of done:** `switchboard-ctl --json list` shows a non-empty window
address for a wezterm session, and the startup log reads `navigate=true`.

### 1b. Select a backend that can actually join

Two routes. Ship both, in order.

**Route A — force X11 (zero code, unblocks today).** Add `-wm x11` to the unit's
`ExecStart`. Verified viable in F2. Costs two things:

- **Workspace *names* are lost.** `desktopOf` (`internal/wm/x11.go:297`) returns
  the 1-indexed `_NET_WM_DESKTOP` number as a string. This machine has named
  workspaces — `_NET_DESKTOP_NAMES` is `"Notes", "Wezterm"` — so chips would
  read `ws 2` instead of `ws Wezterm`. Cheap fix, worth doing inside Route A:
  read `_NET_DESKTOP_NAMES` off the root window, index into it by desktop
  number, fall back to the number when absent or short.
- **Coarser events.** EWMH only signals `_NET_ACTIVE_WINDOW` and
  `_NET_CLIENT_LIST`; a pure title change fires no event, so title-driven
  remapping waits for the next reconcile tick instead of being pushed.

**Route B — hydrate the i3 backend's PIDs (the durable fix).** i3's `GET_TREE`
omits `pid` but *does* give `window` (the X11 window id) — already parsed into
`i3Node.Window` at `internal/wm/i3.go:~250`. So the missing pid is one
`_NET_WM_PID` read away, using the xgb code already vendored for the X11
backend.

Implementation: in `parseI3Tree`, collect the X11 window ids, then batch-resolve
`_NET_WM_PID` for each and fill `Window.PID`. This keeps everything the i3
backend is better at — con_id focus, real workspace names and numbers straight
from the tree, and the precise `window::title` / `workspace` event stream that
makes remapping push-driven rather than tick-driven — while closing the one gap
that makes it useless.

Keep it Wayland-safe: under sway, `Window` is null and `PID` is already
populated, so the hydration step must be skipped when `n.PID != 0` or when there
is no X11 window id. Gate the xgb connection on `DISPLAY` being set so a sway
session never opens one.

**Then fix detection** (`detectWMAuto`, `internal/detect/detect.go:78`). Today
`SWAYSOCK` and `I3SOCK` both fall through to `wm.NewI3()`. After Route B they can
stay merged. If Route B slips, `I3SOCK`-without-`SWAYSOCK` must prefer
`wm.NewX11()` instead — otherwise every i3 user silently gets the Observe tier,
which is the bug this whole phase is about.

**Definition of done:** `switchboard-ctl focus pid:<n>` raises the correct
wezterm window *and* activates the correct pane, from a different workspace.

---

## Phase 2 — Normalize the title before comparing

Fixes F3. Benefits every backend, not just i3.

Add a pure function in `internal/mapping/`:

```go
// normalizeTitle strips the leading activity spinner Claude Code writes into
// the pane title, which wezterm propagates to the window title. The glyph
// rotates about once a second, so an un-normalized compare races: the terminal
// seam and the WM seam read the title at different instants.
func normalizeTitle(s string) string
```

Strip a leading rune in the braille/circle spinner set (`◐◑◒◓`, plus the
braille run `⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏` other agents use) together with any following space,
then `TrimSpace`. Apply to **both** sides of the compare in `matchUniqueClient`
— never mutate the stored `WindowTitle`, which is user-facing in tooltips and
`ctl list`.

`matchUniqueClient` is already pure and documented as such
(`internal/mapping/mapping.go:229`), so this is directly unit-testable: a table
test asserting that two titles differing only in spinner glyph match, and that
two genuinely different titles still do not.

**Definition of done:** a test that feeds every spinner phase against a fixed
pane title and asserts a unique match on all of them.

---

## Phase 3 — polybar module

### The key simplification

Do **not** port the waybar slot architecture. Waybar needs ten fixed
`custom/claude-N` modules because each chip must be its own GTK widget to carry
its own CSS class, and `internal/barlayout` exists to divide a non-wrapping row
into fixed pixel cells. **Polybar has neither constraint**: a `custom/script`
module emits a formatted string, colors are inline tags rather than CSS classes,
and one module can render every chip. One module, one process, variable width.

`barlayout` stays useful only as label-abbreviation logic if many sessions
crowd the bar — and there it should be driven by a **character budget**, not
pixels. Treat that as optional polish, not basic functionality.

### `cmd/switchboard-polybar`

New binary. Subscribes to the daemon socket and writes one line per snapshot to
stdout — the push model polybar's `tail = true` expects. Reuses the same RPC
subscribe path as `switchboard-waybar`, so nothing new is needed daemon-side.

Polybar formatting, in place of waybar's JSON `class` array:

| need | waybar | polybar |
| --- | --- | --- |
| status color | CSS class | `%{F#RRGGBB}…%{F-}` |
| click to focus | `on-click` | `%{A1:cmd:}…%{A}` |
| right-click menu | `on-click-right` | `%{A3:cmd:}…%{A}` |
| scroll to cycle | `on-scroll-up/down` | `%{A4:…:}` / `%{A5:…:}` |
| tooltip | `tooltip` | **none — see below** |

**Polybar has no tooltips.** The per-session detail waybar puts in a tooltip
(cwd, status duration, workspace) has nowhere to go. Surface it on right-click
instead — either `notify-send` or a rofi picker, reusing the existing
`switchboard-ctl pick` output, which already emits `pid<TAB>label<TAB>ws<TAB>cwd`
for exactly this.

Flags: `-socket` (parity), `-max-sessions` (truncate with a `+N` overflow chip),
and colors as flags so the palette lives in polybar's config rather than being
compiled in.

Emitted line, one chip per session, each independently clickable:

```
%{A1:switchboard-ctl focus pid:4821:}%{F#8ABEB7}● switchboard%{F-}%{A} %{A1:switchboard-ctl focus pid:5102:}%{F#F0C674}● other%{F-}%{A}
```

Click commands run through `/bin/sh`, so `switchboard-ctl` must be resolvable —
pass an absolute path (`%h/go/bin/switchboard-ctl`), matching the reasoning
already recorded in `systemd/switchboard-dashboard.service`.

### polybar config

The standalone bottom bar ships as `polybar/switchboard.ini`. Its
`[module/switchboard]` follows the existing `[module/task]` idiom
(`type = custom/script`, `tail = true`):

```ini
[module/switchboard]
type = custom/script
exec = /home/tjmisko/go/bin/switchboard-polybar
tail = true
click-right = /home/tjmisko/go/bin/switchboard-ctl pick | rofi -dmenu | awk '{print $1}' | xargs -r /home/tjmisko/go/bin/switchboard-ctl focus
scroll-up = /home/tjmisko/go/bin/switchboard-ctl cycle prev
scroll-down = /home/tjmisko/go/bin/switchboard-ctl cycle next
```

Map status onto the palette already defined in `[colors]` rather than
introducing new hex values: `permission` → `alert` `#A54242`, `idle` →
`primary` `#F0C674`, `working` → `secondary` `#8ABEB7`, `unknown` → `disabled`
`#707880`.

With `tail = true` polybar restarts the script if it exits, so the daemon being
down must be an ordinary emitted line (an `✕` chip in `disabled`, as
`switchboard-waybar` already does at `cmd/switchboard-waybar/main.go:67`) rather
than a non-zero exit — otherwise polybar spins on respawn.

**Definition of done:** chips appear in the bar within one second of a session
changing status; left-clicking one focuses that session's window and pane;
killing the daemon degrades the module to a single `✕` without a respawn loop.

---

## Testing

- **Phase 1a** is environment, not logic — verify by asserting the startup log
  line, not with a unit test.
- **Phase 1b Route B** — the PID-hydration step is the part worth testing:
  given a tree with X11 window ids and an injected `_NET_WM_PID` lookup, assert
  PIDs land on the right windows and that a sway-shaped tree (pid present,
  `window` null) is passed through untouched. The xgb lookup needs to be an
  injected interface for this to be testable at all.
- **Phase 2** — pure table test, described above.
- **Phase 3** — the render function should be pure `snapshot → string`, mirroring
  `renderSlot` in the waybar adapter, so chip output is golden-testable without a
  daemon or a bar.
- The backend-agnostic suites in `internal/conformance/` already cover the i3
  backend (`internal/wm/i3_test.go`); Route B must keep them green.

## Out of scope

The bottom-bar **auto-hide appliance**. A dedicated i3 bottom bar now ships, but
it is launched normally by i3. The Hyprland process-existence lifecycle tied to
an F8 marker remains compositor-specific; Polybar's equivalent would use
`polybar-msg cmd hide/show`, which is a different mechanism and is not needed
for chips plus click-to-focus.
