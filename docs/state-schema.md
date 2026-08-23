# `state.json` Schema (frozen — public contract)

> `state.json` is Switchboard's **stable public contract**. It is emitted in
> every capability tier (Observe and Navigate) and is the integration surface
> for every bar/renderer (waybar, polybar, eww, i3blocks, the reference TUI).
> Consumers parse this file; the daemon owns it. **Changing a field's name,
> type, or presence semantics is a breaking change** — gate it behind the
> versioning rules below.
>
> The canonical example is [`internal/state/testdata/state.golden.json`](../internal/state/testdata/state.golden.json),
> pinned by `TestStateGoldenRoundTrips` (the fixture re-encodes byte-for-byte)
> and `TestStateGoldenPinsCanonicalSnapshot` (the fixture matches
> `canonicalSnapshot()`, so a **new optional field cannot go silently unpinned**)
> in `internal/state/golden_test.go`. The Go source of truth is the
> `Snapshot`/`Session` structs in `internal/state/state.go`.

## How it is written

The daemon writes the file atomically: encode to a temp file in the same
directory (`.state-*.json`), then `rename(2)` over `state.json`. Readers
therefore always see a complete document — never a partial write. Encoding is
`encoding/json` with two-space indentation and a trailing newline.

Default location: `$XDG_CACHE_HOME/switchboard/state.json`, falling back to
`$HOME/.cache/switchboard/state.json`. Overridable with the daemon's `-state`
flag.

The file is rewritten whenever a state mutation **changes something observable**
(a session appearing or dying, a focus change, a hook status update). A mutation
that changes nothing writes nothing: an idle reconcile tick re-derives the same
state and is suppressed, so `updated_at` is the time of the last *change*, not
the time of the last tick.

`updated_at` is therefore advisory. **Do not treat a stale `updated_at` as a
dead daemon** — on a quiet machine it is simply the age of the last real change.
Use the RPC socket if you need liveness.

Consumers that poll should treat the file as a whole-document replace, not a
delta.

## Top level: `Snapshot`

```jsonc
{
	"schema_version": 2,
  "sessions": [ /* Session objects, see below */ ],
	"slots": [ /* CodexSlot objects, see below */ ],
  "updated_at": "2026-05-28T09:05:30Z"
}
```

| Field | JSON type | Always present | Meaning |
|-------|-----------|----------------|---------|
| `schema_version` | integer | yes | Persisted-state schema version. Current value: `2`. A daemon loading an incompatible version performs a clean reset rather than guessing at legacy process/conversation ownership. Unversioned Claude-only mirrors remain readable because Claude's transport is unchanged; any unversioned mirror containing Codex is reset. |
| `sessions` | array of `Session` | yes | All currently-tracked coding-agent sessions (Claude Code and Codex). May be empty (`[]`) when no sessions exist. |
| `slots` | array of `CodexSlot` | when non-empty | Stable visible Codex TUI slots and their replaceable conversation bindings. Omitted when there are no launcher-registered Codex slots. |
| `updated_at` | RFC 3339 timestamp string | yes | When this snapshot was produced (`time.Now()` at encode). Monotonic-ish wall clock; advisory only. |

**Ordering guarantee:** `sessions` is sorted ascending by `started_at`. ⚠ The
sort is currently by `started_at` **only** and is not stabilized by a
tie-break, so sessions with identical timestamps have an unspecified relative
order. A PID tie-break is a pending fix (see `docs/decisions.md`); until then
consumers must **not** rely on positional/index identity across snapshots and
should key on `pid`.

## `Session`

The session record. Five fields are always present; `suspended` and `headless`
appear only when true; the two `mem_*` fields appear only when a reading
succeeded; the backend/enrichment blocks are optional and omitted entirely when
their data has not been resolved yet.

```jsonc
{
  "pid": 4821,
  "cwd": "/home/tjmisko/Projects/switchboard",
  "tty": "/dev/pts/3",
  "started_at": "2026-05-28T09:00:00Z",
  "focused": true,
  "suspended": true,         // omitted when false
  "headless": true,          // omitted when false
  "agent": "claude",         // "claude" | "codex"; omitted until the kind is known
	"slot_id": "a0f75199-...", // Codex launcher slot; omitted otherwise
  "mem_agent_bytes": 461373440,  // omitted when unmeasured
  "mem_tree_bytes": 674234368,   // omitted when unmeasured
  "wezterm":  { /* WeztermInfo, optional */ },
  "hyprland": { /* HyprlandInfo, optional */ },
  "claude":   { /* AgentInfo, optional — present for a claude session */ },
  "codex":    { /* AgentInfo, optional — present for a codex session */ },
  "agent_graph": { /* AgentGraph, optional — root + nested child threads */ }
}
```

| Field | JSON type | Presence | Stability | Meaning |
|-------|-----------|----------|-----------|---------|
| `pid` | integer | always | **stable key** | OS process id of the interactive Claude or Codex root. The primary identity of a switchable session. Unique within a snapshot. Child graph nodes do not get separate `Session` records or PIDs. |
| `cwd` | string | always | stable | Working directory of the coding-agent root process. May be `""` if the kernel masked it. Resolved from `/proc/<pid>/cwd`, falling back to the terminal pane's reported cwd. It is never used to bind a provider thread. |
| `tty` | string | always | stable | Controlling pseudo-terminal, e.g. `/dev/pts/3`. **OS-specific literal** (macOS will report `/dev/ttysNNN`); consumers should treat it as an opaque join key, never parse the prefix. May be `""` for a non-tty-attached process — such a session cannot be mapped to a terminal/window (Observe-only). |
| `started_at` | RFC 3339 timestamp | always | stable | When Switchboard first observed the session (wall clock at discovery), **not** the process's real start time. |
| `focused` | boolean | always | stable | Whether this session's window is the active window in the WM. Best-effort; `false` for any session without a resolved WM address. |
| `suspended` | boolean | omitted when false | stable | Whether the agent process is job-control-stopped — paused by `SIGTSTP`/`SIGSTOP` (Ctrl-Z). Derived from the `State:` field of `/proc/<pid>/status` (`T`); refreshed each reconcile tick (~5 s). Renderers grey such chips out, since the status is stale while paused. `t` (tracing stop, e.g. under a debugger) does **not** count. Linux-only signal today; absent on backends that can't read process run-state. |
| `headless` | boolean | omitted when false | additive | Whether this is a non-interactive `claude -p`/SDK run (see `discovery.IsHeadless`). Such a session appears in bars for visibility but has no TUI to navigate to, so renderers style it inert and focus/cycle/pick skip it. |
| `agent` | string | omitted until known | additive | Which coding-agent CLI owns the session: `"claude"` or `"codex"`. Set at discovery from the process. Selects which enrichment block (`claude`/`codex`) carries the status. Consumers should tolerate its absence (pre-multi-agent daemons) and any unrecognized value. |
| `slot_id` | UUID string | omitted when absent | additive | Stable visible-TUI identity supplied by `switchboard-codex`. It outlives `/clear` and joins this process/liveness record to the matching top-level `slots[]` entry. Hooks carrying a known slot use it before process ancestry; an unknown exact slot never falls back to a neighboring pid. |
| `mem_agent_bytes` | integer | omitted when unmeasured | additive | Live resident cost of the agent process alone, in **bytes**: `Pss + SwapPss` from `/proc/<pid>/smaps_rollup`. PSS (proportional set size) charges each shared page to its sharers in fractions, so summing it across sessions never double-counts. Swap is included because a page pushed to swap is still memory the session is responsible for. Refreshed each reconcile tick (~5 s). Linux-only; absent on backends that cannot read it, and absent (rather than `0`) whenever a reading failed — `0` would mean "measured, and empty". |
| `mem_tree_bytes` | integer | omitted when unmeasured | additive | Same measure summed over the agent process **and every descendant** — the session's whole process tree. Subagents have no PIDs of their own, so the tree is the only unit that captures spawned work. Subtract `mem_agent_bytes` to get what the session's children cost. Same units, cadence, and absence semantics as above. |
| `wezterm` | object \| absent | optional | provisional | Terminal-locator data. Present once the tty is matched to a **wezterm** pane. Other terminal backends (e.g. tmux) do **not** populate it — those sessions are still observed via `/proc`, and focus re-locates the pane by tty at request time. Field set is terminal-backend-specific and may generalize when the seam grows a neutral terminal block. |
| `hyprland` | object \| absent | optional | provisional | Window-manager data. Present once the pane is matched to a WM window. WM-backend-specific; will generalize behind a neutral window block as other WM backends land. |
| `claude` | object \| absent | optional | stable | Claude compatibility enrichment fed by the Claude provider adapter. Shape is `AgentInfo` (below). |
| `codex` | object \| absent | optional | additive | Codex compatibility enrichment projected from the authoritative graph or a hook fallback. Same `AgentInfo` shape as `claude`. A session populates exactly one of `claude`/`codex`, matching `agent`. |
| `agent_graph` | object \| absent | optional | additive | Provider-neutral, bounded current view of the root thread and nested child threads. Present after a valid provider observation or restored last-known projection. Child nodes are display/history detail only and are never navigation sessions. See `AgentGraph` below. |

## `slots[]` (`CodexSlot`) — schema v2

A Codex slot is one visible terminal/TUI lifetime. The slot owns navigation,
endpoint, pid, and process-start metadata; its `conversation` owns all mutable
conversation state. `/clear` replaces only the binding and generation, so the
visible chip does not disappear or inherit the retired thread's state.

| Field | JSON type | Presence | Meaning |
|-------|-----------|----------|---------|
| `slot_id` | UUID string | always | Random launcher identity, stable for one visible TUI lifetime. |
| `endpoint` | string | always | That TUI's private app-server Unix socket or `unix://` endpoint. Never shared between visible TUIs. |
| `pid` | integer | always | Visible TUI pid; liveness/discovery metadata, not conversation identity. |
| `started_at` | RFC 3339 timestamp | always | Process-lifetime discriminator paired with `pid`. |
| `conversation` | object \| absent | optional | Current `ConversationBinding`; absent while the endpoint is alive but has no loaded root. |
| `retired` | array | omitted when empty | Slot-lifetime retired identity/name history. Runtime, attention, children, pending operations, and prompts are never copied here. |
| `endpoint_connected` | boolean | always | Whether the per-slot observer currently has an initialized endpoint connection. |
| `snapshot_at` | RFC 3339 timestamp | omitted until observed | Timestamp of the last complete app-server snapshot. |
| `diagnostic` | string | omitted when healthy/unknown | Finite content-free state such as `endpoint disconnected`, `slot alive but no thread bound`, `conversation rotated`, or a snapshot error category. |
| `last_error` | string | omitted before an endpoint error | Most recent content-free endpoint error category, retained independently of current connectivity. |
| `autoname` | string | omitted before naming | `pending`, `generated`, `fallback`, or `suppressed_explicit`. |

`ConversationBinding` contains `thread_id`, monotonic `generation`, optional
`name`, optional `name_origin` (`user`, `generated`, or `fallback`), `bound_at`,
and the accepted-event reorder fence `observed_at`. A different exact `thread_id` advances the generation and clears
the visible conversation state before the discovering event is applied. An
event naming a retired thread, or carrying a stale non-zero generation, cannot
change status, attention, children, pending work, or name.

The app-server thread name is authoritative. The legacy `codex.session_id`,
`codex.status`, and root graph nickname are projections of this current binding,
not independent state. Explicit external names are `origin=user` and suppress a
pending generated name. Automatic-naming prompt text and the pending generated
value are in-memory only and never appear in `state.json`.

### `wezterm` (`WeztermInfo`) — provisional

Present only when the session's tty was matched to a running wezterm pane. All
fields are always present when the block exists (no `omitempty`).

| Field | JSON type | Meaning |
|-------|-----------|---------|
| `mux_pid` | integer | PID of the wezterm mux (gui) process owning the pane. |
| `mux_socket` | string | Path to that mux's control socket (`$XDG_RUNTIME_DIR/wezterm/gui-sock-<pid>`). |
| `pane_id` | integer | Pane id **within its mux's namespace** (not globally unique — always pair with `mux_socket`). |
| `tab_id` | integer | Tab id within the mux. |
| `window_id` | integer | wezterm GUI window id within the mux. |
| `window_title` | string | The pane's window title. Best-effort join key to the WM window (`hyprland.title`). |

### `hyprland` (`HyprlandInfo`) — provisional

Present only when the wezterm window was matched to a Hyprland client. All
fields always present when the block exists.

| Field | JSON type | Meaning |
|-------|-----------|---------|
| `address` | string | Hyprland window address, e.g. `0x5640f1a2b3c0`. ⚠ **Always `0x`-prefixed here**, even though Hyprland's socket2 event stream emits it without the prefix; the daemon normalizes at the event boundary. Treat as an **opaque** window ref — future WM backends store sway `con_id` / X11 window ids in this slot. |
| `workspace` | string | Workspace name the window is on. |
| `workspace_id` | integer | Numeric Hyprland workspace id. Drives the bottom-bar chip ordering (chips follow workspace order). `0` means unresolved (Hyprland workspace ids are positive, or negative for special workspaces). |
| `monitor` | string | Monitor name. ⚠ Currently **never populated** (always `""`); reserved. See `docs/decisions.md`. |

### `claude` / `codex` (`AgentInfo`) — claude stable, codex additive

The legacy per-agent enrichment block. A session populates exactly one, under
the key matching its `agent`. Both share one shape (`AgentInfo`). Hooks can
populate it, and `SetAgentGraph` also projects the graph root id and reduced
legacy status into it so old consumers keep working. Renderers read whichever
is present.

| Field | JSON type | Presence | Meaning |
|-------|-----------|----------|---------|
| `session_id` | string | omitted when empty | Provider root id (Claude session id or Codex thread id), supplied by exact provider evidence and/or hooks. It may change when a live root process starts a new provider session; consumers key navigation on the enclosing `Session.pid`, not this field alone. |
| `transcript` | string | omitted when empty | Path to the session transcript when known: Claude Code's project `.jsonl`, or Codex's `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl`. |
| `status_since` | RFC 3339 timestamp \| absent | optional | When `status` last transitioned to its current value — the wire projection of the daemon's in-memory `StatusSince`, stamped onto the snapshot. Renderers compute the hover duration (`idle · 3m`, `permission · 45s`) as `now - status_since`. **Omitted** (never `null`/zero) until the first status edge stamps it; absent on a block that only carries `session_id`/`transcript`. Additive (Phase: usage-history); consumers tolerate its absence. Formatted identically to `started_at`. |
| `status` | string | always (when block present) | Legacy root-chip activity. One of: `working`, `idle`, `permission`, `delegating`; `""` means no fresh authoritative reduction. `delegating` is an idle root with live descendant work and renders the **same green as `working`**. `permission` folds both approval and user-input waits; use `agent_graph.summary`/nodes when that distinction matters. Consumers must tolerate unknown future strings. |
| `in_flight_subagents` | number | omitted when 0 | How many subagent `Task`s the main thread has launched but not yet collected, recomputed each reconcile tick from the transcript tail. It is the signal behind a `delegating` chip; renderers show it as "N agents" in the tooltip, and `switchboard-ctl list --json` exposes it so a green chip's true state (genuinely working vs delegating) is visible. Claude-only. |
| `pending_writers` | array of string | omitted when empty | Which **writers** are currently blocked on a permission prompt. A session is not one thread but 1 + N concurrent writers — the main thread plus each in-flight subagent — that share a pid and a chip, write to different files, and can each block independently. Each element is a bare subagent `agent_id` (the `<id>` stem of `<session>/subagents/agent-<id>.jsonl`), or the literal **`"main"`** for the main thread. **Sorted ascending**, so a renderer can diff two snapshots directly. A non-empty array means the chip is `permission`; a renderer may name the blocked teammate from it. Claude-only. Additive; consumers tolerate its absence. |

#### Legacy hook fallback mapping

Claude compatibility still consumes its hook/transcript state machine. Codex
uses app-server as primary structural truth, plus a standard-CLI hook-owned
`request_user_input` latch that app-server snapshots cannot resolve. Other
Codex hook evidence remains a lower-authority partial fallback. Active evidence
expires after 10 minutes, approval or user-input waits after 24 hours, and idle
edges after 7 days. The event mapping is:

| Hook event | `claude` status | `codex` status |
|------------|-----------------|----------------|
| `UserPromptSubmit` | `working` | `working` |
| ordinary `PreToolUse` | (unmapped) | `working` |
| `request_user_input` `PreToolUse` | (unmapped) | `permission` (`user_input`) |
| ordinary/unmatched `PostToolUse` | `working` | `working`, without clearing a pending question |
| matching `request_user_input` `PostToolUse` | `working` | `working`, and clears that exact question |
| `PermissionRequest` | `permission` | `permission` |
| `Stop` | `idle` | `idle`, and clears an interrupted question for that turn |
| `SessionStart(startup|resume|clear)` | `idle` | provisional `idle`, coalesced for 250 ms with an immediate continuation |
| `SessionStart(compact)` | `idle` | `working` |
| (any other / unknown) | unchanged | unchanged |

For Codex, `PermissionRequest` with tool `AskUserQuestion` becomes structured
attention `user_input`; other permission requests become `approval`. Either
reduces to legacy `permission`. `request_user_input` is correlated only by its
opaque `tool_use_id`; question and answer content are not persisted. A fresh
app-server graph outranks ordinary hook fallback, but cannot clear this
independently owned standard-CLI latch. Rollout files alone still cannot recover
approval state, but app-server can.

##### `permission` self-heal (reconciler)

`permission` is the only status with no guaranteed clearing hook: declining an
`AskUserQuestion` — or interrupting a turn — fires nothing (`PostToolUse` only
fires on success, `Stop` not on interrupt), so the chip would latch red forever.
Each reconcile tick the daemon reads the tail of a `permission` session's
transcript (`transcript` field above) and exits it once the prompt is resolved.
Resolution is signalled by the **main conversation thread advancing past the
prompt** after `StatusSince` (the moment the chip went red), and the *kind* of
resolution now selects the **exit color**:

- an **assistant message** (the blocked turn resumed → the awaited tool was
  approved) exits to **`working`** (green) **directly** — no orange bounce.
  (An earlier revision added "Claude Code withholds the pending tool_use's
  assistant message until it resolves" here. That is false and has been removed —
  see `subagent-permission-plan.md` §9.7. The pending `tool_use` *is* on disk
  while the prompt waits; what arrives late is the *next* assistant message.);
- a **user interrupt notice** (`[Request interrupted by user…]` → declined / Esc)
  exits to **`idle`** (orange), or to **`delegating`** (green) if subagents are
  still in flight (work continues).

A bare `tool_result` is **deliberately ignored**: a background teammate/subagent,
or a sibling auto-approved tool in the same turn, keeps flushing `tool_result`s
dated after the prompt while it is still genuinely pending, so counting them would
flash the chip green the instant any concurrent work landed — a pending decision
must stay red even while subagents work. If the transcript can't be read, a TTL
backstop (`statustune.Tuning.PermissionDecayTTL`, default 30 s) exits it anyway so
it never nags forever.

There is also a **hook-speed early clear**: the `PermissionRequest` hook stashes
the tool it was raised for, and a later `PostToolUse` whose `tool_name` matches
(the *approved* tool completed) exits red immediately — collapsing the
approve-path lag without waiting for the transcript. A non-matching / `Task`
`PostToolUse` keeps the chip red.

This is purely a daemon-internal status correction. Every exit is recorded by a
canonical decision log line (see below). The `StatusSince` it keys off is
**in-memory only** (not in `state.json`); it is stamped to startup time on
re-hydrate so a prompt live across a daemon restart is not misjudged as resolved.

##### prompt ownership and `pending_writers`

The daemon holds a pending prompt as a map from the **writer that raised it** to
that prompt's correlators (its tool, a hash of its `tool_input`, its onset). The
chip may leave `permission` only when no writer still owns a prompt, which is why
a teammate finishing an unrelated tool cannot repaint a red chip.

`pending_writers` is the **key set of that map, and only the key set**. The
correlators are in-memory (`json:"-"`) and are deliberately not persisted:

- Losing **ownership** across a restart is unrecoverable. `PermissionRequest` is
  edge-triggered, none of the registered hooks re-raises a live prompt, and a
  blocked writer runs no tools — so a dropped entry is a permanent missed red for
  the rest of that prompt's life. Persisting the keys is what prevents it.
- Losing the **correlators** costs only latency: a hydrated prompt cannot take the
  hook-speed early clear above and resolves via the transcript on the next
  reconcile tick instead. It also *must* fail closed that way — a persisted entry
  has no tool to match, so nothing at hook speed can match it.

At startup the daemon may use each writer's own transcript to **falsify** an entry
— if every tool that writer dispatched has its `tool_result`, the tool returned, so
whatever gate it sat behind opened and the entry is dropped (this is what catches a
prompt answered while the daemon was down). It may never *manufacture* one: an
unmatched `tool_use` means "a tool is dispatched and has not returned", which
covers *awaiting approval* and *executing right now* with no field separating them.
A `state.json` written before this field existed hydrates its red as a main-thread
prompt, reproducing the pre-field behavior exactly.

###### naming a blocked writer

`pending_writers` carries ids, not names, and that is deliberate — **no
`pending_writer_names` field exists or is planned.** A renderer that wants to say
*which teammate* is stuck derives the name itself, entirely from fields already on
this contract:

```
claude.transcript          = <dir>/<session-id>.jsonl
one non-"main" writer id w → <dir>/<session-id>/subagents/agent-<w>.meta.json
```

and reads that meta's `name` (the teammate name, e.g. `escalate-cleanup`), falling
back to `agentType` — the only field present in *every* meta — and then to the id
itself. Do **not** strip a second `agent-` prefix from `w`: an agent whose own id
begins with `agent-` lives in `agent-agent-<…>.meta.json`, and stripping would name
a different agent.

The daemon leaves this to the renderer on purpose. The name lives on disk, never
changes once written, and is wanted only when a chip is actually being drawn — so
resolving it at render time (memoized) keeps the reconcile tick free of the
per-writer I/O that a wire field would add to every snapshot, and keeps this
contract from carrying data it does not already imply. Switchboard's own renderers
go through `internal/label.NameCache.BlockedWriters`, which is the reference
implementation of the derivation above.

##### `delegating` self-heal & decision log

Independently, each tick recomputes `in_flight_subagents`; an **idle** main thread
with a count `> 0` is promoted to **`delegating`** (green), reverting to `idle`
when the last teammate drains. This fixes the orchestrator-goes-orange-between-
wake-ups drift.

Every reconciler/hook status decision — change *or* deliberate hold — emits one
line prefixed `status: pid=<n> session=<id>` carrying the from→to (or `==` for a
hold), the **rule id** (maps to the case table in
`docs/status-color-state-model.md` §5), the reason, and the observed tuple
`[S=<subagents> pending=<tool> age=<dur>]`. `switchboard-ctl diagnose` reads these
back — given approximate timing and a plain-English symptom it surfaces the
relevant lines, names the `statustune.Tuning` knob behind each, and reports the
RED-episode durations — so a wrong-color complaint maps directly to the field to
change. (Grepping the prefix by hand still works.)

## `agent_graph` (`AgentGraph`) — additive neutral contract

`agent_graph` is a bounded current-session view attached to one switchable root
`Session`. It is not a list of additional sessions and it is not durable
history. The enclosing session owns the PID, tty, cwd, focus target, and
navigation actions; graph child nodes own only provider thread state and display
metadata.

### Graph fields

| Field | JSON type | Presence | Meaning |
|-------|-----------|----------|---------|
| `root_id` | string | always | Stable provider id of the root node. Exactly one element of `nodes` has this id and that node has no `parent_id`. |
| `source` | string | omitted when empty | Evidence source: `codex_app_server`, `hook`, `claude_transcript`, `codex_rollout`, `restored_last_known`, or absent/unknown. Source precedence is daemon policy, not a confidence score consumers should recompute. |
| `observed_at` | RFC 3339 timestamp | omitted when unknown | Start of the observation's authority interval. |
| `fresh_until` | RFC 3339 timestamp | omitted when unknown | Exclusive end of the authority interval. A graph is fresh only when `observed_at <= now < fresh_until`. |
| `complete` | boolean | always | `true` means omission is authoritative for that observation; `false` means a partial view. Completeness does not imply freshness and freshness does not imply completeness. |
| `summary` | object | always | Shared root-chip reduction described below. |
| `nodes` | array | always | Root and descendants in deterministic root-first depth-first preorder. May contain retained terminal nodes. |

Known `source` values are additive. Consumers must tolerate an absent or
unrecognized value and should still use the explicit freshness timestamps.

### `summary` (`AgentGraphSummary`)

| Field | JSON type | Always present | Meaning |
|-------|-----------|----------------|---------|
| `runtime` | string | yes | Root runtime: `unknown`, `not_loaded`, `idle`, `active`, or `system_error`. Becomes `unknown` when the observation is not fresh or invalid. |
| `attention` | string | yes | Folded wait reason: `none`, `approval`, or `user_input`. If both wait kinds exist, `approval` wins only in this compact field; the two counts below preserve both. |
| `status` | string | yes | Legacy root-chip value: `working`, `idle`, `permission`, `delegating`, or `""` when no fresh rule produces a confident status. |
| `live_children` | integer | yes | Non-terminal descendants. Terminal means lifecycle `completed`, `interrupted`, `errored`, `shutdown`, or `not_found`. |
| `waiting_nodes` | integer | yes | Live root/descendant nodes waiting for either approval or user input. Equals `approval_nodes + user_input_nodes`. |
| `approval_nodes` | integer | yes | Live nodes whose attention is `approval`. |
| `user_input_nodes` | integer | yes | Live nodes whose attention is `user_input`. |
| `error_nodes` | integer | yes | Nodes with runtime `system_error` or lifecycle `errored`; unlike live/wait counts this includes retained terminal evidence. |
| `since` | RFC 3339 timestamp | omitted when unknown | When this complete derived tuple last changed. Count-only changes move it even when `status` stays the same. The legacy `AgentInfo.status_since` moves only when legacy `status` changes. |

The reducer is source-neutral. Given a fresh valid graph it applies, in order:

1. any live approval/user-input wait → `permission`;
2. active root → `working`;
3. root `system_error` → empty/unknown legacy status (error remains explicit);
4. any live descendant active/pending/running → `delegating`;
5. idle root → `idle`;
6. otherwise empty/unknown.

An expired graph keeps `root_id`, `source`, `complete`, and its normalized node
structure for stale display, but its summary is no longer authoritative:
`runtime=unknown`, `attention=none`, empty `status`, and zero live/wait/error
counts. Renderers grey the child detail and label it stale.

### `nodes[]` (`AgentNode`)

| Field | JSON type | Presence | Meaning |
|-------|-----------|----------|---------|
| `id` | string | always | Stable provider thread/node id; unique within this graph. |
| `parent_id` | string | omitted on root | Immediate parent id. Every child has an explicit chain to `root_id`. |
| `nickname` | string | omitted when empty | Provider display nickname. Optional and potentially user/content-derived. For a Codex root, this is only the explicit `Thread.name`; it remains absent while unnamed, and renderers then use a compact prefix of `root_id`. Child nodes retain their agent nickname. |
| `role` | string | omitted when empty | Provider agent role/type. |
| `description` | string | omitted when empty | Optional task description. Renderers do not require it. |
| `runtime` | string | always | `unknown`, `not_loaded`, `idle`, `active`, or `system_error`. |
| `attention` | string | always | `none`, `approval`, or `user_input`. Independent of runtime and lifecycle. |
| `lifecycle` | string | always | `unknown`, `pending`, `running`, `completed`, `interrupted`, `errored`, `shutdown`, or `not_found`. |
| `started_at` | RFC 3339 timestamp | omitted when unknown | Provider-reported node start time. |
| `updated_at` | RFC 3339 timestamp | omitted when unknown | Best provider transition/update time. |
| `completed_at` | RFC 3339 timestamp | omitted when unknown | Terminal completion time when available. |
| `usage` | object | omitted when wholly unmeasured | Optional token accounting; absence means unavailable, not measured zero. |

`usage`, when present, can contain `input_tokens`, `cached_input_tokens`,
`cache_write_input_tokens`, `output_tokens`, `reasoning_output_tokens`,
`total_tokens`, and `model_context_window`. Each is an integer omitted when
zero. Status reduction never depends on usage.

Axis strings are designed for additive evolution. A consumer must treat an
unrecognized future runtime/lifecycle as unknown and an unrecognized attention
value as no actionable reason unless a newer contract says otherwise; it must
not crash or silently turn a future value red. The current daemon canonicalizes
empty/unrecognized provider values to `unknown`/`none` before emitting state.

### Ordering, authority, and legacy projection

The daemon validates each graph and emits nodes in deterministic root-first DFS
preorder. Siblings sort lexicographically by `nickname`, then `role`, then `id`.
Consumers may preserve that order directly; it is stable for equal input but is
not an identity key.

For partial observations (`complete=false`), a missing node is not a deletion.
Canonical history retains prior nodes until complete evidence removes them. For
complete observations, a previously known omitted node transitions to
`not_found`. Provider adapters may retain a bounded terminal cohort for current
display, so `nodes` is not an unbounded archive.

Applying a graph updates only these legacy compatibility values:

- the matching `claude.session_id` or `codex.session_id` becomes `root_id`;
- its `status` becomes `summary.status`;
- its `status_since` moves when that legacy status changes.

Claude-only `in_flight_subagents`, workflows, pending writers, transcript, and
other compatibility fields remain owned by the Claude adapter. Existing
consumers can continue reading `.claude.status // .codex.status`; new consumers
should read the graph when they need parentage or distinct wait reasons.

### Agent-graph examples

These are complete `Snapshot` values and round-trip through the merged Go state
types. Timestamps are illustrative; freshness is evaluated against the reader's
current time, not `updated_at`.

**Root only, no provider graph yet.** The session is discoverable and
switchable, but its status is unknown.

```json
{
  "sessions": [{
    "pid": 4100,
    "cwd": "/work/root-only",
    "tty": "/dev/pts/4",
    "started_at": "2026-08-21T16:00:00Z",
    "focused": false,
    "agent": "codex"
  }],
  "updated_at": "2026-08-21T16:00:01Z"
}
```

**Codex tree.** The waiting child makes the legacy root summary red while
preserving `user_input` as the exact reason.

```json
{
  "sessions": [{
    "pid": 4200,
    "cwd": "/work/codex-tree",
    "tty": "/dev/pts/5",
    "started_at": "2026-08-21T16:00:00Z",
    "focused": true,
    "agent": "codex",
    "codex": {"session_id": "codex-root", "status": "permission", "status_since": "2026-08-21T16:00:10Z"},
    "agent_graph": {
      "root_id": "codex-root",
      "source": "codex_app_server",
      "observed_at": "2026-08-21T16:00:10Z",
      "fresh_until": "2026-08-21T16:00:25Z",
      "complete": true,
      "summary": {"runtime": "idle", "attention": "user_input", "status": "permission", "live_children": 1, "waiting_nodes": 1, "approval_nodes": 0, "user_input_nodes": 1, "error_nodes": 0, "since": "2026-08-21T16:00:10Z"},
      "nodes": [
        {"id": "codex-root", "runtime": "idle", "attention": "none", "lifecycle": "running", "started_at": "2026-08-21T16:00:00Z", "updated_at": "2026-08-21T16:00:10Z"},
        {"id": "child-question", "parent_id": "codex-root", "nickname": "metadata", "role": "explorer", "runtime": "idle", "attention": "user_input", "lifecycle": "running", "started_at": "2026-08-21T16:00:05Z", "updated_at": "2026-08-21T16:00:10Z", "usage": {"total_tokens": 42}}
      ]
    }
  }],
  "updated_at": "2026-08-21T16:00:10Z"
}
```

**Claude tree with compatibility fields.** The neutral graph is additive; the
legacy Claude fanout projection remains intact.

```json
{
  "sessions": [{
    "pid": 4300,
    "cwd": "/work/claude-tree",
    "tty": "/dev/pts/6",
    "started_at": "2026-08-21T16:01:00Z",
    "focused": false,
    "agent": "claude",
    "claude": {"session_id": "claude-root", "status": "delegating", "status_since": "2026-08-21T16:01:05Z", "in_flight_subagents": 1},
    "agent_graph": {
      "root_id": "claude-root",
      "source": "claude_transcript",
      "observed_at": "2026-08-21T16:01:10Z",
      "fresh_until": "2026-08-21T16:01:25Z",
      "complete": true,
      "summary": {"runtime": "idle", "attention": "none", "status": "delegating", "live_children": 1, "waiting_nodes": 0, "approval_nodes": 0, "user_input_nodes": 0, "error_nodes": 0, "since": "2026-08-21T16:01:05Z"},
      "nodes": [
        {"id": "claude-root", "runtime": "idle", "attention": "none", "lifecycle": "running"},
        {"id": "worker-a", "parent_id": "claude-root", "role": "Explore", "runtime": "active", "attention": "none", "lifecycle": "running"}
      ]
    }
  }],
  "updated_at": "2026-08-21T16:01:10Z"
}
```

**Stale/disconnected.** Structure and its prior node axes remain available for
grey stale detail, but the summary is explicitly non-authoritative.

```json
{
  "sessions": [{
    "pid": 4400,
    "cwd": "/work/disconnected",
    "tty": "/dev/pts/7",
    "started_at": "2026-08-21T15:59:00Z",
    "focused": false,
    "agent": "codex",
    "codex": {"session_id": "stale-root", "status": ""},
    "agent_graph": {
      "root_id": "stale-root",
      "source": "codex_app_server",
      "observed_at": "2026-08-21T16:00:00Z",
      "fresh_until": "2026-08-21T16:00:15Z",
      "complete": true,
      "summary": {"runtime": "unknown", "attention": "none", "status": "", "live_children": 0, "waiting_nodes": 0, "approval_nodes": 0, "user_input_nodes": 0, "error_nodes": 0, "since": "2026-08-21T16:00:15Z"},
      "nodes": [
        {"id": "stale-root", "runtime": "idle", "attention": "none", "lifecycle": "running"},
        {"id": "old-child", "parent_id": "stale-root", "runtime": "idle", "attention": "none", "lifecycle": "completed", "completed_at": "2026-08-21T16:00:10Z"}
      ]
    }
  }],
  "updated_at": "2026-08-21T16:00:15Z"
}
```

**Fresh but unknown.** This differs from disconnection: the provider recently
reported a node whose state it could not map.

```json
{
  "sessions": [{
    "pid": 4500,
    "cwd": "/work/unknown",
    "tty": "/dev/pts/8",
    "started_at": "2026-08-21T16:02:00Z",
    "focused": false,
    "agent": "codex",
    "codex": {"session_id": "unknown-root", "status": ""},
    "agent_graph": {
      "root_id": "unknown-root",
      "source": "hook",
      "observed_at": "2026-08-21T16:02:10Z",
      "fresh_until": "2026-08-21T16:02:25Z",
      "complete": false,
      "summary": {"runtime": "unknown", "attention": "none", "status": "", "live_children": 0, "waiting_nodes": 0, "approval_nodes": 0, "user_input_nodes": 0, "error_nodes": 0, "since": "2026-08-21T16:02:10Z"},
      "nodes": [{"id": "unknown-root", "runtime": "unknown", "attention": "none", "lifecycle": "running"}]
    }
  }],
  "updated_at": "2026-08-21T16:02:10Z"
}
```

## The `capabilities` block (Phase 1.4)

Emitted since Phase 1.4. A top-level `capabilities` object reports the detected
backend stack and which tier is active, so a renderer can decide whether to show
"jump to" affordances. It is **omitted entirely** (never `null`) by a daemon
that has not set it — consumers must tolerate its absence for forward/backward
compatibility:

```jsonc
{
  "sessions": [ /* ... */ ],
  "updated_at": "...",
  "capabilities": {
    "observe": true,            // always true — the floor tier
    "navigate": true,           // focus works (terminal locator AND WM focus present)
    "wm": "hyprland",           // detected WM backend: hyprland|sway|i3|x11|none
    "terminal": "wezterm"       // detected terminal backend: wezterm|tmux|none
  }
}
```

Consumers must tolerate its **absence** (pre-1.4 daemons, or to stay
forward-compatible). When present, `observe` is always `true`; `navigate` is
`true` only when both a terminal locator and a WM focus backend are available.

## Examples per tier

**Observe tier** (no WM/terminal backend detected — e.g. a headless box or an
unsupported desktop). Sessions still carry `pid`/`cwd`/`tty`/`status`; the
`wezterm`/`hyprland` blocks are absent and `capabilities.navigate` is `false`:

```json
{
  "sessions": [
    {
      "pid": 4821,
      "cwd": "/home/u/Projects/switchboard",
      "tty": "/dev/pts/3",
      "started_at": "2026-05-28T09:00:00Z",
      "focused": false,
      "claude": { "status": "working", "status_since": "2026-05-28T09:05:00Z" }
    }
  ],
  "updated_at": "2026-05-28T09:05:30Z",
  "capabilities": { "observe": true, "navigate": false, "wm": "none", "terminal": "none" }
}
```

**Navigate tier** (a WM focus backend and a terminal locator are both present).
The optional blocks are filled and `capabilities.navigate` is `true`:

```json
{
  "sessions": [
    {
      "pid": 4821,
      "cwd": "/home/u/Projects/switchboard",
      "tty": "/dev/pts/3",
      "started_at": "2026-05-28T09:00:00Z",
      "focused": true,
      "wezterm":  { "mux_pid": 4790, "mux_socket": "/run/user/1000/wezterm/gui-sock-4790", "pane_id": 12, "tab_id": 7, "window_id": 3, "window_title": "claude — switchboard" },
      "hyprland": { "address": "0x5640f1a2b3c0", "workspace": "4", "workspace_id": 4, "monitor": "" },
      "claude":   { "status": "working", "status_since": "2026-05-28T09:05:00Z" }
    }
  ],
  "updated_at": "2026-05-28T09:05:30Z",
  "capabilities": { "observe": true, "navigate": true, "wm": "hyprland", "terminal": "wezterm" }
}
```

A tmux-hosted session reaches the Navigate tier with `terminal` reported as
`"tmux"` (or a chain like `"tmux+wezterm"`) and **no** `wezterm` block — focus
re-locates the pane by `tty` at request time.

## Stability rules / versioning

- **Stable fields** (`pid`, `cwd`, `tty`, `started_at`, `focused`, all of
  `claude`): name and type will not change without a major-version bump and a
  migration note. `pid` is the canonical session key.
- **Provisional blocks** (`wezterm`, `hyprland`): the *presence contract*
  (omitted until resolved) is stable, but the **internal field set may evolve**
  as portable WM/terminal backends land — likely generalizing into
  backend-neutral `terminal`/`window` blocks with the WM-specific blocks
  retained or aliased. Treat `hyprland.address` as an opaque ref.
- **Additive changes** (new optional fields like `status_since`,
  `pending_writers`, the `capabilities` block, the `agent` discriminator and the
  `codex` enrichment block, and `agent_graph`) are **not** breaking;
  consumers must ignore unknown fields and tolerate missing optional fields. The
  `claude` block is unchanged — a consumer reading `.claude.status` keeps working;
  to be agent-aware, read `.codex.status` too (e.g. `.claude.status // .codex.status`).
  The Go struct behind `claude`/`codex` was renamed `ClaudeInfo` → `AgentInfo`
  (with a `ClaudeInfo` alias retained); the wire format is unchanged.
- **Empty vs. absent:** always-present string fields use `""` for "unknown";
  optional blocks are **omitted** entirely (never `null`) when unresolved.
  `claude.session_id`/`transcript` are omitted when empty; `claude.status` is
  present-but-`""` before the first authoritative status. `agent_graph` is
  omitted until a valid/restored graph exists; once present, its
  `complete`/`summary`/`nodes` fields are always emitted even when false, empty,
  or unknown.
- The golden fixture is the tripwire: any change to field name, order, type, or
  omitempty behavior breaks `TestStateGoldenRoundTrips`, forcing a deliberate
  `UPDATE_GOLDEN=1` regen and a review of this document. **Adding an optional
  field also means setting it in `canonicalSnapshot()`** — round-tripping the
  fixture against itself cannot see a field the fixture never carried, so
  `TestStateGoldenPinsCanonicalSnapshot` is the test that fails until the
  fixture is regenerated. Every optional field must be set on at least one
  session there, with the minimal session left bare to pin the absence side.

The ⚠ items above are tracked characterization quirks; each has a pin-then-fix
entry in `docs/decisions.md` (Phase 0.9).
