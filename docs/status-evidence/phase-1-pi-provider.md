# Phase 1 — Pi as a first-class provider

[Master](README.md). Depends on Phase 0. This plan replaces item 8 of
`docs/usage-limit-status-plan.md`.

**Goal.** A Pi session is tracked as precisely as a Claude Code session.
- It is discovered with or without herdr.
- It is keyed by Pi's own session id and follows rotation.
- Its status comes from the extension's exact lifecycle events, with herdr as
  the fallback.
- It shows red while any dialog is open.
- It carries a name, a transcript path, usage and cost.
- It survives a daemon restart without false authority.

## Where Pi stands (main a06439f)

- **Discovery is herdr-only.** `discovery.Classify` knows only claude and
  codex (`internal/discovery/discovery.go` ~:97-105). herdr discovery uses the
  pane's foreground process-group leader as the session PID
  (`cmd/switchboard/herdr_discovery.go` ~:55-132) and calls the same `appear`
  as the scanner (`cmd/switchboard/main.go` ~:225-296).
- **Session.** `Agent="pi"` and a `Herdr` block. There is no enrichment block:
  `graphEnrichment` and `AgentBlock` handle claude and codex only
  (`internal/state/herdr.go` ~:236-247, `internal/state/state.go` ~:154-166).
  The graph is one node, `herdr:<terminal>` (`herdr.go` ~:189-217).
- **Status** is herdr's only: working→active, blocked→approval (red),
  idle/done→idle (`herdr.go` ~:221-232; `cmd/switchboard/herdr_status.go`).
- **Hooks.** `switchboard-ctl pi-hook` exists (`cmd/switchboard-ctl/main.go`
  ~:144, ~:641-694) but only classifies StopFailure. rpc applies the usage
  limit and returns early for Pi; it drops `session_id` and `transcript_path`
  (`internal/rpc/rpc.go` ~:746-749, ~:1145-1151). Attribution is by process
  ancestry, `findHookAncestor` (~:1547).
- **Pi outside herdr** is never discovered, and its hooks are dropped.
- **Extension** (`integrations/pi/switchboard.ts`): `agent_start` →
  UserPromptSubmit, and a failed `agent_settled` → StopFailure. It runs in TUI
  only, and the live copy at `~/.pi/agent/extensions/switchboard.ts` is a
  **copy**, not a link.

## What Pi offers an extension (pi-coding-agent 0.85.1)

Docs live in `~/.local/lib/node_modules/@earendil-works/pi-coding-agent/docs/`
(`extensions.md`, `session-format.md`, `sessions.md`); types are in
`dist/core/extensions/types.d.ts`.

- **Run lifecycle.**
  - `agent_start`: a run begins.
  - `agent_settled`: no retry, compaction or queued follow-up remains, and
    `ctx.isIdle()` is true. This is the real Stop.
  - `agent_end` can be followed by an automatic retry, so it is **not** a stop.
- **Inside a run:** `turn_start`/`turn_end`, `tool_execution_start`/`_end
  {toolCallId, toolName, isError}`, and `message_end` (assistant: `stopReason`
  ∈ stop|length|toolUse|error|aborted, `errorMessage`, `usage {input, output,
  cacheRead, cacheWrite, totalTokens, cost{…total}}`, `provider`, `model`).
- **Waiting on the user.** `ui_prompt_start {kind, title?}` and
  `ui_prompt_end` wrap any extension dialog (`ctx.ui.select/confirm/input/
  editor/custom`). Nested dialogs coalesce into one span, and handlers are not
  awaited. herdr's own signal is a custom `pi.events` event, `herdr:blocked
  {active, label}`, which approval-gate extensions emit
  (`~/.pi/agent/extensions/herdr-agent-state.ts` ~:191-262). **Forward `kind`
  only, never `title` or `label`.**
- **Rotation.**
  - `/new`, `/resume`, `/fork`, `/clone` and reload send the old instance
    `session_shutdown {reason, targetSessionFile?}`.
  - The extension then reloads, and the new instance gets `session_start
    {reason, previousSessionFile?}`. In-memory extension state is lost across
    that reload.
  - `ctx.sessionManager.getSessionId()`, `getSessionFile()` and
    `getSessionName()` (the latest `session_info` entry, set by `/name`).
- **Mode.** `ctx.mode` ∈ tui|rpc|json|print; gate on `"tui"`.
- **Not visible to extensions:** auto-retry, compaction progress and queue
  updates. `agent_settled` absorbs all of them.
- **Session file.** Lives at `~/.pi/agent/sessions/--<path>--/<ts>_<uuid>.jsonl`.
  Entries are a tree (`id`/`parentId`). There are no explicit run markers: a
  run's end is inferred from the last assistant `stopReason`. The last line
  may not be on the active branch.
- **Process.** Pi sets `process.title = "pi"` (`pi-rpc` in RPC mode), so
  `/proc/<pid>/comm` reads `pi` and argv is overwritten. Subagent children run
  as `pi --mode json -p` with the same title.

## Verify first (spike, no code merged)

Run a live Pi and record the results at the bottom of this file before work
unit 1.2:

1. **Interactive gate.** For an interactive TUI Pi and a json-mode child (run
   `pi --mode json -p "hi"` from inside a Pi bash tool), record comm, exe,
   cmdline, the targets of `/proc/<pid>/fd/{0,1}`, and the controlling tty.
   The candidate gate is "fd 0 is a tty". Confirm that it separates the two,
   or find another gate.
2. **Built-in dialogs.** Do Pi's own dialogs (the `/resume` picker, project
   trust, `/model` selector) fire `ui_prompt_*`? Log events from a throwaway
   extension. If they do, an idle Pi goes red at a picker. Report that to the
   owner, do not decide it.
3. **Hook ancestry.** Confirm that `switchboard-ctl`, spawned by the
   extension, has the Pi process as its parent both in herdr and in a plain
   WezTerm pane.
4. **herdr pane PID.** Confirm that the herdr pane's foreground
   process-group leader is the Pi process itself and not a wrapper, so that
   scanner and herdr discovery agree on one PID.

## Work units

Ship as four PRs, in order: 1A (1.1, 1.3), 1B (1.2), 1C (1.4–1.6), 1D (1.7–1.10).

### 1.1 Extension v2 and the ctl edge

`integrations/pi/switchboard.ts` maps Pi events onto Claude's hook vocabulary,
so the daemon keeps one table:

| Pi event | `pi-hook` event | Payload (beyond `session_id`, `transcript_path`, `cwd`) |
|---|---|---|
| `session_start` | `SessionStart` | `hook_source` = reason; `previous_session_file`; `session_name`; `busy` = `!ctx.isIdle()` |
| `session_shutdown` | `SessionEnd` | `hook_source` = reason |
| `agent_start` | `UserPromptSubmit` | — (no prompt text) |
| `tool_execution_start` / `_end` | `PreToolUse` / `PostToolUse` | `tool_name`, `tool_use_id` = toolCallId |
| `ui_prompt_start` / `_end`, `herdr:blocked` | `PermissionRequest` / `PermissionResolved` | `open_dialogs` = current count (ui span plus herdr counter) |
| `message_end` (assistant) | `Usage` | token counts, `cost_total`, `provider`, `model` |
| `agent_settled` | `Stop`, or `StopFailure` after an error stop | `error_message` on failure only; ctl reduces it to a verdict, as now |

- **TUI gate.** Keep the existing `ctx.mode === "tui"` gate. Rebuild
  per-session state in `session_start`, because the reload loses it.
- **Dialog count.** Send the **count**, not edges: lost or reordered hooks
  then self-correct on the next event.
- **Send path.** Keep the spawn-and-forget sender and its 2 s kill timer.
- **ctl** `parseHookPayloadAt` carries the new fields into `rpc.Request`. Add
  only what is missing: `open_dialogs`, usage counts, cost, provider, model,
  previous session file and session name. Classify `StopFailure` as now.
- **Install** with a symlink from `~/.pi/agent/extensions/switchboard.ts` to
  the repo file. Update the install note in the file header.

### 1.2 Discovery without herdr

- Add `discovery.AgentPi` and `IsPi(osproc.Info)`. The predicate is comm `pi`,
  a node exe (accept a masked exe, as Claude does), and the interactive gate
  verified in the spike. Reject comm `pi-rpc`.
- `Classify` returns `AgentPi`, and the scanner then announces Pi like the
  others.
- herdr discovery must not create a second session for a PID the scanner
  already holds. It attaches the `Herdr` block to it, as `appear` with a
  `herdrPane` already does for a known PID. If the scanner wins the race, herdr
  only enriches; if herdr wins, the scanner's `appear` keeps the herdr block.
- `processIsSession` (`main.go` ~:418-423): a scanner-found Pi is alive while
  `IsPi` holds for its PID. The TTY-equality rule stays only for herdr-only
  agents.

### 1.3 State and wire

- Add `Session.Pi *AgentInfo` (`json:"pi,omitempty"`), and extend
  `AgentBlock`, `Enrichment()` and `graphEnrichment` to cover `AgentKindPi`.
  The block holds the session id, transcript, status, since and display
  state.
- The root ID becomes Pi's session UUID. The `herdr:<terminal>` root is used
  only while no Pi hook has bound the session.
- Update the `AgentKindPi` comment (`state.go` ~:114).
- Update `docs/state-schema.md`: a `pi` block parallel to `claude`/`codex`,
  and how the root id is chosen. Keep it additive.
- `hydrate`: a persisted Pi block restores display state only (see 1.7).

### 1.4 The Pi hook reducer

This is a daemon-side FSM in a new `cmd/switchboard/pi_hooks.go`, dispatched
from rpc the way Claude and Codex hooks reach `agentCoordinator.HandleHook`.
rpc stops returning early for Pi once usage-limit handling is done.

- `SessionStart` binds or rebinds the root. It is working if `busy`, otherwise
  idle.
- `UserPromptSubmit`, `PreToolUse` and `PostToolUse` → working.
- `PermissionRequest`/`PermissionResolved`: `open_dialogs > 0` → permission
  (attention user_input); `0` → back to working while a run is open, else
  idle.
- `Stop` → idle. `StopFailure` → idle, plus the usage-limit record as today.
  A run is open from `UserPromptSubmit` until `Stop`.
- `SessionEnd` holds the current status until the following `SessionStart`.
  If the process dies first, death ends the session.
- The graph is one root node, `Source=hook`. Take the freshness lease from the
  Claude hook path's constants; do not invent one. Clock: `req.ObservedAt`
  passed through.
- History: every edge becomes a `transition` with a rule code. Add Pi rule
  codes to `internal/statustune/knobs.go` and log `statustune.Decision` lines
  like Claude's, so `diagnose` sees them.

### 1.5 Precedence against herdr (interim)

One function, `piStatusAuthority`, called from status projection for
`AgentKindPi`:

- Pi hook evidence within its lease wins.
- Otherwise a live herdr reading decides, mapped as now.
- Otherwise the status is unknown.

The herdr `blocked` state and the hook dialog count both mean red, so they
cannot disagree on red. A herdr `working` reading cannot clear a hook-held red.
Leave a comment saying #96 replaces this function.

### 1.6 Rotation

- **New session.** `SessionStart` with a session id different from the bound
  one, whether from `/new`, `/resume`, `/fork` or `/clone`.
- **What follows.** The root rebinds and conversation-bound display state
  (name, usage) resets. History closes the old lane and opens a new one.
- **Pairing.** `previous_session_file` pairs the two sessions for history.
- **Reload.** `hook_source=reload` with the same session id is not a rotation.

### 1.7 Daemon restart

- A restored Pi block has no live authority. It shows its last status until
  its original deadline, then unknown, per the rule in #96.
- On rediscovery, read the session file **tail only**, through the shared
  bounded tail reader (see `codex_usage_limit_scan.go`'s
  size/mtime-cached read). Take the last assistant `stopReason` on the active
  branch: `toolUse` → working, anything else → idle. Treat it as the same
  evidence class as the Codex rollout read.
- herdr readings and the next hook then take over.

### 1.8 Naming

`session_name` (Pi's `/name`) becomes the display name with origin `native`,
following the Codex native-name path in `internal/state/display_name.go`.
Without it, use the existing fallback chain (`internal/label/label.go`
~:55-101). There is no generated naming for Pi in this phase.

### 1.9 Usage and cost (decision 5)

`Usage` hooks add per-message tokens and **Pi's own `cost_total`** to the
session's usage, under a Pi billing identity (agent client `pi`, plus the
provider and model as reported). Find the seam in the usage tracker and
pricing code (`internal/transcript/usage_tracker.go`, `internal/pricing`). A
supplied cost must bypass the rate table, not be repriced. Sum by message id
if Pi supplies one; otherwise sum per hook, at most once per `message_end`.

### 1.10 Docs and cleanup

- Mark items 7 and 8 of `docs/usage-limit-status-plan.md` as moved here.
- Update `docs/state-schema.md` and the extension header.
- Add a `diagnose` binding source for Pi: hook, herdr or none.

## Tests (acceptance bar)

**rpc / ctl**
- should carry a Pi hook's session id, transcript and dialog count into the
  daemon;
- should carry no prompt, dialog title or tool input from any Pi event.

**discovery**
- should classify an interactive Pi;
- should not classify a json-mode Pi child or `pi-rpc`;
- should merge scanner and herdr discovery of one Pi into one session,
  whichever arrives first;
- should keep tracking a Pi outside herdr through death.

**reducer**
- should go working on agent_start and idle on agent_settled, but not on
  agent_end alone;
- should show red while a dialog is open and return to working when it closes
  mid-run;
- should stay red while either the ui prompt span or herdr's blocked counter
  is open;
- should treat a reload mid-run as working when Pi reports busy;
- should rebind the root on /new and /resume, and not on reload;
- should record the usage limit on StopFailure and clear it on the next
  agent_start.

**precedence**
- should prefer fresh Pi hook evidence over a herdr reading;
- should fall back to herdr when no hook has arrived;
- should not let a herdr working reading clear a hook-held red.

**restart**
- should seed idle or working from the session file tail without live
  authority;
- should hand authority to the next hook or herdr reading.

**naming / usage**
- should show Pi's /name as the display name;
- should add Pi's own per-message cost without repricing.

**end to end** (manual, recorded in the PR): a Pi in a plain WezTerm pane and
a Pi in herdr both show working, red at a confirm dialog, idle, and limited.

## Out of scope

- Pi subagents (decision 4).
- Generated names for Pi.
- Pi's RPC mode.
- Claude/Codex cost accuracy (decision 6).

## Spike results

Recorded 2026-10-05 against pi-coding-agent 0.85.1, with a throwaway logging
extension (since deleted) and a probe spawned exactly as `switchboard.ts`
spawns `switchboard-ctl`.

1. **Interactive gate: confirmed.** A TUI Pi has fd 0 and fd 1 on its pty and
   a nonzero `tty_nr`. A json-mode child started from Pi's bash has fd 0 on
   `/dev/null`, a pipe on fd 1, and `tty_nr` 0, because Pi's bash runs
   commands in their own session with no controlling tty. Either "fd 0 is a
   tty" or "`tty_nr` ≠ 0" separates them; the second needs no readlink. comm
   (`pi`), exe (`node-22`) and cmdline (`pi` plus padding) cannot.
2. **Built-in dialogs: they do not fire `ui_prompt_*`.** The project-trust
   prompt, the `/model` selector and the `/resume` picker emit nothing; an
   extension `ctx.ui.select` emits `ui_prompt_start {kind}` and
   `ui_prompt_end`. An idle Pi at a built-in picker therefore does not go red,
   and is not seen as waiting on the user. The owner accepted this: no red at
   Pi's own pickers.
3. **Hook ancestry: confirmed.** In a plain WezTerm pane, after `/reload`, and
   in herdr, the process the extension spawns has the Pi process as its
   direct parent.
4. **herdr pane PID: confirmed.** The pane's foreground process-group leader
   is the Pi process itself, with no wrapper, and herdr's
   `pane process-info` reports the same PID.

Additions to the plan:

- `project_trust` fires before `session_start` and reaches user extensions,
  so a trust prompt blocks startup before the extension's TUI gate is set.
- A json-mode child runs the full extension set (`session_start` through
  `session_shutdown`) with `mode=json`; the `ctx.mode === "tui"` gate drops
  it correctly.
- argv is overwritten (cmdline is `pi` plus padding), so the scanner cannot
  read `--mode json` from it.

## PR 1C notes (1.4–1.6)

- **Hook ordering** (owner decision). The extension stamps `event_at`, the
  instant the Pi event fired, on every hook. `switchboard-ctl` makes it the
  request's `ObservedAt` when it is at most 2 s ahead (clamped to now) and at
  most 10 s old, else uses its own clock. The reducer drops any hook older
  than the newest one it applied for that process.
- **Lease.** The Claude hook path's only lease is the observer's 15 s window,
  which its transcript scan renews every tick. Pi has no such renewal, so a
  15 s lease would turn an idle Pi unknown within seconds when herdr is
  absent. The reducer uses the Codex hook-only fallback windows instead
  (`codexHookActiveFreshness`, `…AttentionFreshness`, `…IdleFreshness`).
- **Rotation in history.** A rotation writes a `session_start` carrying the
  new `session_id` and `prev_session_id`; as for a Claude `/clear`, the new id
  on the live pid ends the old lane and no `session_end` is written.
