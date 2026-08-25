local callbacks = {}
local ran = {}
local logs = {}
local fail_run = false
local during_run = nil

package.preload['wezterm'] = function()
  return {
    procinfo = { pid = function() return 4321 end },
    GLOBAL = {},
    on = function(name, callback) callbacks[name] = callback end,
    run_child_process = function(argv)
      table.insert(ran, argv)
      if during_run then
        local callback = during_run
        during_run = nil
        callback(argv)
      end
      if fail_run then return false, '', 'failed' end
      return true, '', ''
    end,
    log_error = function(message) table.insert(logs, message) end,
    json_parse = function(value)
      local version = tonumber(value:match('"v":(%d+)'))
      local host = value:match('"host":"([^"]+)"')
      local pid = tonumber(value:match('"pid":(%d+)'))
      local started_at = value:match('"started_at":"([^"]+)"')
      if not version or not host or not pid or not started_at then error('invalid json') end
      return { v = version, host = host, pid = pid, started_at = started_at }
    end,
  }
end

local module_path = arg[1] or 'switchboard.lua'
local switchboard = dofile(module_path)

assert(switchboard.marker(9) == '[sbw:4321:9]')
assert(switchboard.format_window_title('project', { window_id = 9 }) == 'project [sbw:4321:9]')
assert(switchboard.format_window_title('project [sbw:1:2]', { window_id = 9 }) == 'project [sbw:4321:9]')

switchboard.setup { ctl_path = '/trusted/switchboard-ctl' }
assert(type(callbacks['user-var-changed']) == 'function')
assert(type(callbacks['window-focus-changed']) == 'function')
assert(type(callbacks['update-status']) == 'function')
assert(callbacks['format-window-title'] == nil)

local window = { id = 9, focused = true }
function window:window_id() return self.id end
function window:is_focused() return self.focused end
local pane = { id = 7, vars = {} }
function pane:pane_id() return self.id end
function pane:get_user_vars() return self.vars end
local function signal(target, value)
  target.vars.SWITCHBOARD_SESSION = value
  callbacks['user-var-changed'](window, target, 'SWITCHBOARD_SESSION', value)
end

signal(pane, '')
assert(#ran == 0)
local payload = '{"v":1,"host":"h","pid":1,"started_at":"2026-08-24T20:00:00Z"}'
fail_run = true
signal(pane, payload)
assert(#ran == 1)
fail_run = false
signal(pane, payload)
assert(#ran == 2)
local bind = ran[2]
assert(#bind == 6)
assert(bind[1] == '/trusted/switchboard-ctl' and bind[2] == 'pane-bind')
assert(bind[3] == payload and bind[4] == '4321' and bind[5] == '9' and bind[6] == '7')

fail_run = true
callbacks['update-status'](window, pane)
fail_run = false
callbacks['update-status'](window, pane)
callbacks['update-status'](window, pane)
assert(#ran == 4)
local active = ran[4]
assert(#active == 6)
assert(active[2] == 'pane-state' and active[3] == '4321')
assert(active[4] == '9' and active[5] == '7' and active[6] == 'true')

window.focused = false
callbacks['window-focus-changed'](window, pane)
assert(#ran == 4)
callbacks['update-status'](window, pane)
callbacks['update-status'](window, pane)
assert(#ran == 5 and ran[5][6] == 'false')

-- Ordinary successive A -> B bindings remain ordered, and B invalidates state
-- cached against A so the next status repairs focus.
local payload_b = '{"v":1,"host":"h","pid":2,"started_at":"2026-08-24T20:00:01Z"}'
signal(pane, payload)
signal(pane, payload_b)
callbacks['update-status'](window, pane)
assert(#ran == 8)
assert(ran[6][2] == 'pane-bind' and ran[6][3] == payload)
assert(ran[7][2] == 'pane-bind' and ran[7][3] == payload_b)
assert(ran[8][2] == 'pane-state')

-- run_child_process yields. Inject B while A is outstanding to prove that the
-- per-pane lane stages B instead of launching an overtaking helper.
local payload_c = '{"v":1,"host":"h","pid":3,"started_at":"2026-08-24T20:00:02Z"}'
local payload_d = '{"v":1,"host":"h","pid":4,"started_at":"2026-08-24T20:00:03Z"}'
during_run = function(argv)
  assert(argv[2] == 'pane-bind' and argv[3] == payload_c)
  signal(pane, payload_d)
end
signal(pane, payload_c)
assert(#ran == 10)
assert(ran[9][2] == 'pane-bind' and ran[9][3] == payload_c)
assert(ran[10][2] == 'pane-bind' and ran[10][3] == payload_d)
callbacks['update-status'](window, pane)
assert(#ran == 11 and ran[11][2] == 'pane-state')

-- If a new bind arrives while an older state helper is outstanding, it is
-- drained afterward and the old state result is deliberately not cached.
local payload_e = '{"v":1,"host":"h","pid":5,"started_at":"2026-08-24T20:00:04Z"}'
callbacks['window-focus-changed'](window, pane)
during_run = function(argv)
  assert(argv[2] == 'pane-state')
  signal(pane, payload_e)
end
callbacks['update-status'](window, pane)
assert(#ran == 13)
assert(ran[12][2] == 'pane-state')
assert(ran[13][2] == 'pane-bind' and ran[13][3] == payload_e)
callbacks['update-status'](window, pane)
callbacks['update-status'](window, pane)
assert(#ran == 14 and ran[14][2] == 'pane-state')

-- A callback from an older Lua/config state has a separate in-memory lane.
-- It still repairs after returning by comparing against the pane's current
-- authoritative user variable.
local payload_f = '{"v":1,"host":"h","pid":6,"started_at":"2026-08-24T20:00:05Z"}'
local payload_g = '{"v":1,"host":"h","pid":7,"started_at":"2026-08-24T20:00:06Z"}'
during_run = function(argv)
  assert(argv[2] == 'pane-bind' and argv[3] == payload_f)
  pane.vars.SWITCHBOARD_SESSION = payload_g
end
signal(pane, payload_f)
assert(#ran == 16)
assert(ran[15][2] == 'pane-bind' and ran[15][3] == payload_f)
assert(ran[16][2] == 'pane-bind' and ran[16][3] == payload_g)

-- A newly active pane is reported only after its own binding is established.
-- Here update-status recovers the binding directly from the persistent user
-- variable, which is what happens after a WezTerm configuration reload.
local pane_two = { id = 8, vars = { SWITCHBOARD_SESSION = payload_b } }
function pane_two:pane_id() return self.id end
function pane_two:get_user_vars() return self.vars end
callbacks['update-status'](window, pane_two)
assert(#ran == 18 and ran[17][2] == 'pane-bind' and ran[18][5] == '8')

-- A focused window switching from a remotely bound pane to an ordinary pane
-- must send one explicit false edge, then dedupe it.
window.focused = true
callbacks['window-focus-changed'](window, pane_two)
callbacks['update-status'](window, pane_two)
local ordinary = { id = 9, vars = {} }
function ordinary:pane_id() return self.id end
function ordinary:get_user_vars() return self.vars end
callbacks['update-status'](window, ordinary)
callbacks['update-status'](window, ordinary)
assert(#ran == 20)
assert(ran[19][2] == 'pane-state' and ran[19][5] == '8' and ran[19][6] == 'true')
assert(ran[20][2] == 'pane-state' and ran[20][5] == '9' and ran[20][6] == 'false')

-- A bind arriving while that unbound clear yields is staged behind the clear,
-- then its true state is reported on the following status tick.
local payload_h = '{"v":1,"host":"h","pid":8,"started_at":"2026-08-24T20:00:07Z"}'
local racing = { id = 10, vars = {} }
function racing:pane_id() return self.id end
function racing:get_user_vars() return self.vars end
during_run = function(argv)
  assert(argv[2] == 'pane-state' and argv[5] == '10' and argv[6] == 'false')
  signal(racing, payload_h)
end
callbacks['update-status'](window, racing)
assert(#ran == 22)
assert(ran[21][2] == 'pane-state' and ran[21][6] == 'false')
assert(ran[22][2] == 'pane-bind' and ran[22][3] == payload_h)
callbacks['update-status'](window, racing)
assert(#ran == 23 and ran[23][2] == 'pane-state' and ran[23][6] == 'true')

signal(pane, string.rep('x', 513))
signal(pane,
  '{"v":2,"host":"h","pid":1,"started_at":"2026-08-24T20:00:00Z"}')
signal(pane,
  '{"v":1,"host":"h","pid":1,"started_at":"2026-99-24T20:00:00Z"}')
signal(pane,
  '{"v":1,"host":"h","pid":1,"started_at":"2026-08-24T20:00:00Zjunk"}')
assert(#ran == 23 and #logs == 6)

-- A callback from an old config may complete A -> repair B after the current
-- config has already cached B's successful pane-state. Every successful bind
-- completion changes a wezterm.GLOBAL token, forcing the current config to
-- send one repairing state edge instead of suppressing it forever.
local old_user_var = callbacks['user-var-changed']
local reloaded = dofile(module_path)
reloaded.setup { ctl_path = '/trusted/switchboard-ctl' }
local current_user_var = callbacks['user-var-changed']
local current_status = callbacks['update-status']
local cross_config = { id = 11, vars = { SWITCHBOARD_SESSION = payload_b } }
function cross_config:pane_id() return self.id end
function cross_config:get_user_vars() return self.vars end
current_user_var(window, cross_config, 'SWITCHBOARD_SESSION', payload_b)
current_status(window, cross_config)
local cached_count = #ran
current_status(window, cross_config)
assert(#ran == cached_count)

-- Do not change the pane's authoritative current value: this is a delayed A
-- callback, so its old state must emit A and then repair to current B.
old_user_var(window, cross_config, 'SWITCHBOARD_SESSION', payload)
assert(#ran == cached_count + 2)
assert(ran[cached_count + 1][2] == 'pane-bind' and ran[cached_count + 1][3] == payload)
assert(ran[cached_count + 2][2] == 'pane-bind' and ran[cached_count + 2][3] == payload_b)
current_status(window, cross_config)
assert(#ran == cached_count + 3)
assert(ran[cached_count + 3][2] == 'pane-state' and ran[cached_count + 3][6] == 'true')
