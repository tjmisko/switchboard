local wezterm = require 'wezterm'

local M = {}
local GUI_PID = wezterm.procinfo.pid()
local VARIABLE = 'SWITCHBOARD_SESSION'
local MAX_PAYLOAD_BYTES = 512
local installed = false
local bind_serial = 0
local local_bind_tokens = {}
local instance_identity = {}
local instance_token = tostring(instance_identity)

-- Config reloads can leave an older synchronous callback outstanding while a
-- new Lua state has already cached pane-state. Keep only a tiny completion
-- token in wezterm.GLOBAL (which survives reloads): every successful pane-bind
-- changes it, so every config state observes that it must report pane-state
-- once more. The token is local bookkeeping, not part of the RPC protocol.
local function bind_token_key(pane_id)
  return string.format('switchboard_bind_completion_%d_%s', GUI_PID, pane_id)
end

local function current_bind_token(pane_id)
  local key = bind_token_key(pane_id)
  if wezterm.GLOBAL ~= nil then
    local ok, value = pcall(function() return wezterm.GLOBAL[key] end)
    if ok and type(value) == 'string' then
      return value
    end
  end
  return local_bind_tokens[pane_id] or ''
end

local function mark_bind_complete(pane_id)
  bind_serial = bind_serial + 1
  local token = string.format('%s:%d', instance_token, bind_serial)
  local_bind_tokens[pane_id] = token
  if wezterm.GLOBAL ~= nil then
    pcall(function() wezterm.GLOBAL[bind_token_key(pane_id)] = token end)
  end
end

local function run(argv, failure_message)
  local called, success = pcall(wezterm.run_child_process, argv)
  if not called or not success then
    -- Keep diagnostics finite: never log the payload or terminal content.
    wezterm.log_error(failure_message)
    return false
  end
  return true
end

local payload_fields = { v = true, host = true, pid = true, started_at = true }
local function valid_timestamp(value)
  if type(value) ~= 'string' or #value == 0 or #value > 64 then
    return false
  end
  local year, month, day, hour, minute, second, fraction =
    value:match('^(%d%d%d%d)%-(%d%d)%-(%d%d)T(%d%d):(%d%d):(%d%d)%.(%d+)Z$')
  if not year then
    year, month, day, hour, minute, second =
      value:match('^(%d%d%d%d)%-(%d%d)%-(%d%d)T(%d%d):(%d%d):(%d%d)Z$')
  end
  if not year then
    return false
  end
  month, day = tonumber(month), tonumber(day)
  hour, minute, second = tonumber(hour), tonumber(minute), tonumber(second)
  if month < 1 or month > 12 or day < 1 or hour > 23 or minute > 59 or second > 59 then
    return false
  end
  local days = { 31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31 }
  local numeric_year = tonumber(year)
  if numeric_year % 4 == 0 and (numeric_year % 100 ~= 0 or numeric_year % 400 == 0) then
    days[2] = 29
  end
  return day <= days[month] and (fraction == nil or #fraction <= 9)
end

local function valid_payload(value)
  if type(value) ~= 'string' or value == '' or #value > MAX_PAYLOAD_BYTES then
    return false
  end
  local ok, payload = pcall(wezterm.json_parse, value)
  if not ok or type(payload) ~= 'table' then
    return false
  end
  local field_count = 0
  for key, _ in pairs(payload) do
    if not payload_fields[key] then
      return false
    end
    field_count = field_count + 1
  end
  if field_count ~= 4 or payload.v ~= 1 or type(payload.host) ~= 'string' or
      type(payload.pid) ~= 'number' or type(payload.started_at) ~= 'string' then
    return false
  end
  if payload.pid <= 0 or payload.pid > 2147483647 or payload.pid % 1 ~= 0 or
      #payload.host == 0 or
      #payload.host > 253 or payload.host ~= payload.host:lower() or
      payload.host:sub(-1) == '.' or payload.host:match('^%s') or
      payload.host:match('%s$') or payload.host:match('%c') or
      not payload.host:match('^[a-z0-9_%.%-]+$') or
      payload.host:sub(1, 1) == '.' or payload.host:find('%.%.') or
      not valid_timestamp(payload.started_at) then
    return false
  end
  for label in payload.host:gmatch('[^.]+') do
    if #label > 63 then
      return false
    end
  end
  return true
end

function M.marker(window_id)
  return string.format('[sbw:%d:%d]', GUI_PID, window_id)
end

-- Compose this into the user's one format-window-title callback. WezTerm runs
-- only the first handler for that event, so setup() deliberately does not
-- register a competing formatter. `tab.window_id` is the mux window ID; that
-- callback does not receive a Window object.
function M.format_window_title(base_title, tab)
  local title = tostring(base_title or '')
  title = title:gsub('%s*%[sbw:%d+:%d+%]%s*$', '')
  if not tab or tab.window_id == nil then
    return title
  end
  if title == '' then
    return M.marker(tab.window_id)
  end
  return title .. ' ' .. M.marker(tab.window_id)
end

-- Register the non-title callbacks. ctl_path is local trusted configuration;
-- no remote value can choose an executable or alter the argv shape.
function M.setup(opts)
  if installed then
    return
  end
  opts = opts or {}
  local ctl_path = opts.ctl_path or 'switchboard-ctl'
  assert(type(ctl_path) == 'string' and ctl_path ~= '', 'ctl_path must be a non-empty string')
  installed = true

  local last_active = {}
  local bindings = {}

  local function binding_lane(pane)
    local pane_id = tostring(pane:pane_id())
    local binding = bindings[pane_id]
    if not binding then
      binding = { generation = 0, applied_generation = 0, running = false, pane_id = pane_id }
      bindings[pane_id] = binding
    end
    binding.pane = pane
    return binding
  end

  local function current_payload(pane)
    if type(pane.get_user_vars) ~= 'function' then
      return nil
    end
    local ok, vars = pcall(pane.get_user_vars, pane)
    local value = ok and type(vars) == 'table' and vars[VARIABLE] or nil
    if not valid_payload(value) then
      return nil
    end
    return value
  end

  local function adopt_current_payload(binding)
    local current = current_payload(binding.pane)
    if current and current ~= binding.value then
      binding.generation = binding.generation + 1
      binding.value = current
    end
  end

  -- run_child_process yields while the helper runs, so other Lua events may be
  -- observed before it returns. Keep one per-pane observation lane: a newer
  -- binding only updates the desired generation, and the current lane drains
  -- the latest value before any later pane-state is allowed through.
  local function drain_binding(binding)
    if binding.running then
      return false
    end
    binding.running = true
    while binding.applied_generation ~= binding.generation do
      local generation = binding.generation
      local value = binding.value
      local window_id = binding.window_id
      local pane_id = binding.pane_id
      local success = run({
        ctl_path,
        'pane-bind',
        value,
        tostring(GUI_PID),
        window_id,
        pane_id,
      }, 'switchboard binding callback failed')
      if success then
        -- Do this even when the observation became stale while the helper was
        -- running. The daemon did receive it, so another config state's
        -- otherwise-identical cached pane-state must be repaired.
        mark_bind_complete(pane_id)
      end
      -- Config reloads can replace the Lua state while this helper yields. If
      -- another state observed a newer OSC, repair to the pane's authoritative
      -- current value after this older helper returns.
      adopt_current_payload(binding)
      if generation == binding.generation then
        if success then
          binding.applied_generation = generation
          -- A remote-stream re-announcement follows a client-daemon restart.
          -- Force one fresh pane-state report on the next status event.
          last_active[binding.window_id] = nil
        end
        break
      end
    end
    binding.running = false
    return binding.applied_generation == binding.generation
  end

  local function observe_binding(window, pane, value)
    local pane_id = tostring(pane:pane_id())
    local binding = binding_lane(pane)
    binding.generation = binding.generation + 1
    binding.value = value
    binding.window_id = tostring(window:window_id())
    binding.pane_id = pane_id
    drain_binding(binding)
  end

  local function recover_binding(window, pane)
    local value = current_payload(pane)
    if not value then
      return nil
    end
    observe_binding(window, pane, value)
    return bindings[tostring(pane:pane_id())]
  end

  wezterm.on('user-var-changed', function(window, pane, name, value)
    -- The Go emitter clears then sets so an unchanged re-announcement always
    -- creates an edge. Ignore the intentional empty clear event.
    if name ~= VARIABLE or type(value) ~= 'string' or value == '' then
      return
    end
    if not valid_payload(value) then
      wezterm.log_error('switchboard binding payload is invalid')
      return
    end
    observe_binding(window, pane, value)
  end)

  local function report_active(window, pane)
    if not window or not pane then
      return
    end
    local window_id = tostring(window:window_id())
    local pane_id = tostring(pane:pane_id())
    local binding = bindings[pane_id]
    if not binding then
      binding = recover_binding(window, pane)
    end
    if not binding then
      binding = binding_lane(pane)
    end
    binding.window_id = window_id
    if binding.running then
      return
    end
    local bound = binding.value ~= nil and drain_binding(binding)
    -- An ordinary active pane must clear the prior remote focus projection for
    -- this OS window. `false` is an explicit clear even if the window itself is
    -- still focused; Navigator's false edge is deliberately route-independent.
    local focused = bound and window:is_focused() and 'true' or 'false'
    local signature = pane_id .. ':' .. focused .. ':' .. current_bind_token(pane_id)
    if last_active[window_id] == signature then
      return
    end
    local generation = binding.generation
    binding.running = true
    local success = run({
      ctl_path,
      'pane-state',
      tostring(GUI_PID),
      window_id,
      pane_id,
      focused,
    }, 'switchboard state callback failed')
    adopt_current_payload(binding)
    binding.running = false
    if success and generation == binding.generation then
      last_active[window_id] = signature
    end
    -- A user-var event may have arrived while run_child_process yielded. Its
    -- callback staged the newer generation but could not overtake this state;
    -- drain it now and leave state uncached for the next update-status repair.
    if generation ~= binding.generation then
      drain_binding(binding)
    end
  end

  -- Do not launch a second helper from this event: background children can
  -- reach the daemon out of order. Invalidate the dedupe and let update-status
  -- serialize the observation through run_child_process.
  wezterm.on('window-focus-changed', function(window, pane)
    if window then
      last_active[tostring(window:window_id())] = nil
    end
  end)
  wezterm.on('update-status', report_active)
end

return M
