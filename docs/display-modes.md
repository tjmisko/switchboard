# Display modes and the ordered session view

Switchboard is an ordered session list with live changes. A display is a
consumer of that list, not the owner of its order or navigation.

The daemon and federation view use `state.SortSessionOrder`: resolved workspace
first, workspace number, session start time, then PID, with stable host order
for ties. `sessionview` supplies common status flags, generation-scoped focus
selectors, and next/previous navigation. Informational headless or unreachable
sessions remain visible in the list but are skipped by focus navigation. An
empty navigable list makes next/previous a valid no-op.

## Presentations

| Mode | Surface | Session appearance |
| --- | --- | --- |
| `chips` (default) | Independent bottom Waybar | Existing named chips, tooltips and navigation |
| `circles` | Right end of the existing top Waybar | One compact GTK circle per session, no visible text |

The circle view has no fixed slot limit. Both views share working/idle/permission
colors, focus highlights, remote double borders, and suspended/headless flags.
Each circle has a tooltip and a click target tied to the session's host, PID and
start time, so reordering cannot redirect an already displayed circle's click.
Scroll uses the existing previous/next commands; right-click opens the picker.
The current named-chip example retains its ten configured slots; the ordered
session data and navigation are never truncated to those slots.

```bash
switchboard-ctl display status
switchboard-ctl display mode circles
switchboard-ctl display mode chips
switchboard-ctl display mode toggle
```

The preference survives broker restarts in
`$XDG_STATE_HOME/switchboard/display-mode` (default
`~/.local/state/switchboard/display-mode`). `SWITCHBOARD_DISPLAY_MODE_FILE`
overrides the path. Concurrent toggles are serialized and the file is replaced
atomically. Invalid values are rejected without replacing the last preference.
Changing a mode never starts another broker.

The host recipe binds **Super+Shift+B** to the toggle. Existing
**Super+Alt+Left/Right**, attention, picker, and F8 bindings keep their meaning.
F8 remains the master visibility toggle. Switching modes while hidden keeps
both surfaces hidden until the master is shown again.

## Ownership and live updates

`switchboard-waybar.service` runs one `display serve` process. The historical
`bottombar watch` entry point remains compatible. The broker subscribes once to
`subscribe-all` and prepares both presentation contracts from that ordered view.
A lifetime file lock rejects a competing broker, including a manually launched
one; one-shot visibility reconciliation uses a separate lifecycle lock.

- A bottom process is desired only in chips mode, with master visibility on and
  at least one session. The top process remains owned by the desktop.
- Mode and master-marker renames wake the broker through inotify. Session
  snapshots and owned-child exits also wake it immediately. The existing
  three-second safety tick is a recovery path, not normal update latency.
- Switching to circles requests bottom termination and waits for that owned
  generation to exit before enabling the compact view. Switching to chips
  publishes the hidden-circle state before launching the bottom process.
- Startup/readiness checks never erase ownership of a live child. The prior
  duplicate-spawn repair remains in force.
- The GTK module follows atomic file changes in the GTK main context, reusing
  widgets by session identity. It monitors the publisher's pidfd, hiding itself
  if that publisher dies. Recreated files and a replacement publisher reconnect
  without restarting the top bar.

The combined Waybar profile can use the same circle adapter in its named top
window, but retains its separate `bottombar publish` lifecycle and signal map.
Its source config must be regenerated as part of that profile's setup; the
split-host installation recipe does not silently change combined ownership.

## Other consumers

`$XDG_RUNTIME_DIR/switchboard/display.json` is an atomically replaced, versioned
full frame. `switchboard-ctl display watch` emits changed frames as JSON lines.
It reports a disconnected, empty view if its publisher exits. The frame contains
`mode`, `visible`, `connected`, and an unlimited `sessions` array in canonical
order. Each entry includes `key`, `index`, `hostname`, `pid`, `started_at`,
`label`, `status`, `classes`, `focused`, `navigable`, `focus_selector`, and
`tooltip`. Publisher PID/start time provide a liveness fence.

Consumers can draw a menu, panel, TUI, or other surface without sorting again or
copying focus/status rules. They can also subscribe directly to the daemon's
existing `list-all`/`subscribe-all` RPC if they need the full underlying session
model. Geometry and names are presentation choices; mode changes do not mutate
sessions or navigation.

The Waybar extension consumes an equivalent GKeyFile adapter at
`$XDG_RUNTIME_DIR/switchboard/waybar-circles.ini`. This is an adapter detail, not
the public JSON contract. Escaped user text cannot introduce additional fields.

## Waybar and Hyprland configuration

The circle renderer uses Waybar's supported
[CFFI module interface](https://github.com/Alexays/Waybar/tree/0.15.0/resources/custom_modules/cffi_example).
It uses the v1 string-configuration ABI, opaque GTK 3 function declarations, and
GLib/GIO. It adds no per-circle processes or polling scripts. Building requires
a C compiler, `pkg-config`, GLib/GIO development headers, and the GTK 3 runtime
already used by Waybar:

```bash
scripts/build-waybar-circles /path/to/release/libswitchboard-waybar.so
scripts/configure-waybar-displays \
  --top ~/.config/waybar/config.jsonc \
  --style ~/.config/waybar/style.css \
  --hypr ~/.config/hypr/hyprland.lua \
  --module-path /absolute/path/to/release/libswitchboard-waybar.so \
  --output-dir /tmp/switchboard-display-config
```

The helper prepares files without modifying its inputs. It removes only the
unused `custom/task` top-bar timer, appends `cffi/switchboard` at the far right,
adds the circle stylesheet import, and adds the mode hotkey. It checks hotkey
conflicts and preserves existing navigation bindings. Inspect the prepared
files, install them at their corresponding config paths, reload Hyprland and
the top Waybar, and restart the active display broker. Keep machine-local
systemd overrides pointing at the same release as the daemon.

Use an immutable release path for `module_path`: Waybar may retain loaded
library handles when reloading its configuration. A different release path
ensures the next reload loads the intended module. Edit
`switchboard-circles.css` in the Waybar config directory to customize diameter,
spacing, colors and borders. Removing or stopping the display broker does not
stop the desktop's top bar.

## Verification

Go tests cover mode persistence/atomic change notification, complete list order,
mode-independent navigation, surface visibility, stable selectors, and the
existing lifecycle/renderer behavior. `waybar/circles/module_test.c` uses real
GLib file events and pidfds with a GTK test double to cover 32 circles, reordered
widgets, status changes, hidden/empty modes, publisher death/recovery and
cleanup. The stylesheet is also checked by GTK's actual CSS parser and the
shared library is loaded against the installed GTK runtime. A visual check on
the desktop remains part of activation.
