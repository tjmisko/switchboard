# Switchboard WezTerm integration

Put `switchboard.lua` somewhere on WezTerm's Lua module path, then load it from
the local WezTerm configuration:

```lua
local wezterm = require 'wezterm'
local switchboard = require 'switchboard'

switchboard.setup()

wezterm.on('format-window-title', function(tab, pane, tabs, panes, config)
  -- Keep any existing title computation here. This minimal base matches the
  -- usual default closely enough when there is no existing formatter.
  local base = tab.active_pane.title
  return switchboard.format_window_title(base, tab)
end)
```

If the configuration already has a `format-window-title` callback, call
`switchboard.format_window_title(existing_title, tab)` at the end of that
callback instead of registering a second one. WezTerm executes only the first
handler for this event.

The module invokes only these fixed argument vectors, without a shell:

```text
switchboard-ctl pane-bind <v1-json> <gui-pid> <window-id> <pane-id>
switchboard-ctl pane-state <gui-pid> <window-id> <active-pane-id> <window-focused>
```

Both callbacks use fixed-argument local helper calls. A tiny per-pane lane
serializes them even while `run_child_process` yields: a newer binding replaces
the pending generation, then drains before a later pane-state can run.
Pane-state observations run only from WezTerm's single-outstanding
`update-status` event, so successive active-pane changes cannot reach the
daemon in reverse order. `window-focus-changed` invalidates the state dedupe
and the next status event reports the new value.
Switching to an ordinary, unbound pane emits one deduplicated false edge to
clear that window's prior remote focus projection. After a configuration
reload, the active pane's persistent `SWITCHBOARD_SESSION` user variable
reconstructs the lane and binding automatically.
Successful bind completions also stamp one small per-pane string in
[`wezterm.GLOBAL`](https://wezterm.org/config/lua/wezterm/GLOBAL.html). That
local, process-lifetime token survives configuration
reloads and forces one repairing pane-state edge if an older callback finishes
after the new Lua state had already cached its result; it is never sent over
SSH or stored by Switchboard.

To use an absolute local executable path, pass trusted local configuration:

```lua
switchboard.setup { ctl_path = '/usr/local/bin/switchboard-ctl' }
```
