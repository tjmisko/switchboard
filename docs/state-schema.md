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
  "sessions": [ /* Session objects, see below */ ],
  "updated_at": "2026-05-28T09:05:30Z"
}
```

| Field | JSON type | Always present | Meaning |
|-------|-----------|----------------|---------|
| `sessions` | array of `Session` | yes | All currently-tracked coding-agent sessions (Claude Code and Codex). May be empty (`[]`) when no sessions exist. |
| `updated_at` | RFC 3339 timestamp string | yes | When the daemon last **published** this state — stamped by the writer that produced the snapshot, not by the reader that fetched it. Monotonic-ish wall clock; advisory only. |

⚠ `updated_at` means slightly different things depending on how you fetched it,
and the difference is one tick wide. It splits three ways, not two — the split is
by **what makes the value reach you**, not by transport:

- **In this file** it is the time of the last observable **change**, because the
  file is only rewritten when something changed (see "How it is written" above).
- **`list`, and `subscribe`'s first frame**, are served straight off the
  published snapshot, so they carry the time of the last **write**, changed or
  not. The daemon republishes on every mutation precisely so a reader's
  `updated_at` cannot freeze; an idle box advances it once per reconcile tick
  while `state.json` holds still.
- **`subscribe`'s subsequent frames** are change-gated exactly like the file: a
  frame is only pushed when something observable changed, so a long-lived
  subscriber's `updated_at` tracks the last change, not the last write. The
  unconditional publish still matters to it — the value it eventually receives is
  a publish instant — but it does not receive one per tick.

So a subscriber that sits idle and a poller hitting `list` on the same daemon
will legitimately disagree about `updated_at`, and neither is stale. Reconnecting
a subscriber re-serves the first frame and resynchronizes it with `list`.

Neither is a liveness signal, which is why both are advisory. What changed when
the daemon stopped stamping this at read time: two `list` calls a second apart
used to return two different `updated_at` values on an idle daemon, and now
return the same one. That is the point — the field says when the state a reader
is holding was current, which a read-time clock could never report.

**Ordering guarantee:** `sessions` is sorted ascending by `started_at`. ⚠ The
sort is currently by `started_at` **only** and is not stabilized by a
tie-break, so sessions with identical timestamps have an unspecified relative
order. A PID tie-break is a pending fix (see `docs/decisions.md`); until then
consumers must **not** rely on positional/index identity across snapshots and
should key on `pid`.

## `Session`

The session record. Five fields are always present; `suspended` and `headless`
appear only when true; the two `mem_*` fields appear only when a reading
succeeded; three nested blocks are optional and omitted entirely when their
data has not been resolved yet.

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
  "mem_agent_bytes": 461373440,  // omitted when unmeasured
  "mem_tree_bytes": 674234368,   // omitted when unmeasured
  "wezterm":  { /* WeztermInfo, optional */ },
  "hyprland": { /* HyprlandInfo, optional */ },
  "claude":   { /* AgentInfo, optional — present for a claude session */ },
  "codex":    { /* AgentInfo, optional — present for a codex session */ }
}
```

| Field | JSON type | Presence | Stability | Meaning |
|-------|-----------|----------|-----------|---------|
| `pid` | integer | always | **stable key** | OS process id of the `claude` process. The primary identity of a session. Unique within a snapshot. |
| `cwd` | string | always | stable | Working directory of the Claude process. May be `""` if the kernel masked it. Resolved from `/proc/<pid>/cwd`, falling back to the terminal pane's reported cwd. |
| `tty` | string | always | stable | Controlling pseudo-terminal, e.g. `/dev/pts/3`. **OS-specific literal** (macOS will report `/dev/ttysNNN`); consumers should treat it as an opaque join key, never parse the prefix. May be `""` for a non-tty-attached process — such a session cannot be mapped to a terminal/window (Observe-only). |
| `started_at` | RFC 3339 timestamp | always | stable | When Switchboard first observed the session (wall clock at discovery), **not** the process's real start time. |
| `focused` | boolean | always | stable | Whether this session's window is the active window in the WM. Best-effort; `false` for any session without a resolved WM address. |
| `suspended` | boolean | omitted when false | stable | Whether the agent process is job-control-stopped — paused by `SIGTSTP`/`SIGSTOP` (Ctrl-Z). Derived from the `State:` field of `/proc/<pid>/status` (`T`); refreshed each reconcile tick (~5 s). Renderers grey such chips out, since the status is stale while paused. `t` (tracing stop, e.g. under a debugger) does **not** count. Linux-only signal today; absent on backends that can't read process run-state. |
| `headless` | boolean | omitted when false | additive | Whether this is a non-interactive `claude -p`/SDK run (see `discovery.IsHeadless`). Such a session appears in bars for visibility but has no TUI to navigate to, so renderers style it inert and focus/cycle/pick skip it. |
| `agent` | string | omitted until known | additive | Which coding-agent CLI owns the session: `"claude"` or `"codex"`. Set at discovery from the process. Selects which enrichment block (`claude`/`codex`) carries the status. Consumers should tolerate its absence (pre-multi-agent daemons) and any unrecognized value. |
| `mem_agent_bytes` | integer | omitted when unmeasured | additive | Live resident cost of the agent process alone, in **bytes**: `Pss + SwapPss` from `/proc/<pid>/smaps_rollup`. PSS (proportional set size) charges each shared page to its sharers in fractions, so summing it across sessions never double-counts. Swap is included because a page pushed to swap is still memory the session is responsible for. Refreshed each reconcile tick (~5 s). Linux-only; absent on backends that cannot read it, and absent (rather than `0`) whenever a reading failed — `0` would mean "measured, and empty". |
| `mem_tree_bytes` | integer | omitted when unmeasured | additive | Same measure summed over the agent process **and every descendant** — the session's whole process tree. Subagents have no PIDs of their own, so the tree is the only unit that captures spawned work. Subtract `mem_agent_bytes` to get what the session's children cost. Same units, cadence, and absence semantics as above. |
| `wezterm` | object \| absent | optional | provisional | Terminal-locator data. Present once the tty is matched to a **wezterm** pane. Other terminal backends (e.g. tmux) do **not** populate it — those sessions are still observed via `/proc`, and focus re-locates the pane by tty at request time. Field set is terminal-backend-specific and may generalize when the seam grows a neutral terminal block. |
| `hyprland` | object \| absent | optional | provisional | Window-manager data. Present once the pane is matched to a WM window. WM-backend-specific; will generalize behind a neutral window block as other WM backends land. |
| `claude` | object \| absent | optional | stable | Claude-side enrichment fed by Claude Code hooks. Present once at least one hook fires for a **claude** session. Shape is `AgentInfo` (below). |
| `codex` | object \| absent | optional | additive | Codex-side enrichment fed by Codex hooks. Present once at least one hook fires for a **codex** session. Same `AgentInfo` shape as `claude`. A session populates exactly one of `claude`/`codex`, matching `agent`. |

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

The per-agent enrichment block. A session populates exactly one, under the key
matching its `agent`. Both share one shape (`AgentInfo`) and appear once that
agent's first hook fires. Renderers read whichever is present.

| Field | JSON type | Presence | Meaning |
|-------|-----------|----------|---------|
| `session_id` | string | omitted when empty | Agent session UUID, supplied by hooks (Claude Code's session id; Codex's thread/conversation id). **Write-once**: set on the first hook that carries it and never overwritten. |
| `transcript` | string | omitted when empty | Path to the session transcript when known: Claude Code's project `.jsonl`, or Codex's `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl`. |
| `status_since` | RFC 3339 timestamp \| absent | optional | When `status` last transitioned to its current value — the wire projection of the daemon's in-memory `StatusSince`, stamped onto the snapshot. Renderers compute the hover duration (`idle · 3m`, `permission · 45s`) as `now - status_since`. **Omitted** (never `null`/zero) until the first status edge stamps it; absent on a block that only carries `session_id`/`transcript`. Additive (Phase: usage-history); consumers tolerate its absence. Formatted identically to `started_at`. |
| `status` | string | always (when block present) | Session activity. One of: `working`, `idle`, `permission`, `delegating`. `delegating` is daemon-derived (added with the status-color state-model work): an idle main thread with subagents still in flight — it renders the **same green as `working`** ("work is happening, no action needed"). Consumers that do not know it MUST treat it as `working`, never as attention-worthy. ⚠ The doc-comment also lists `unknown`, but the daemon **never emits it** — tolerate an unrecognized value defensively. May be `""` before the first status-bearing hook. |
| `in_flight_subagents` | number | omitted when 0 | How many subagent `Task`s the main thread has launched but not yet collected, recomputed each reconcile tick from the transcript tail. It is the signal behind a `delegating` chip; renderers show it as "N agents" in the tooltip, and `switchboard-ctl list --json` exposes it so a green chip's true state (genuinely working vs delegating) is visible. Claude-only. |
| `pending_writers` | array of string | omitted when empty | Which **writers** are currently blocked on a permission prompt. A session is not one thread but 1 + N concurrent writers — the main thread plus each in-flight subagent — that share a pid and a chip, write to different files, and can each block independently. Each element is a bare subagent `agent_id` (the `<id>` stem of `<session>/subagents/agent-<id>.jsonl`), or the literal **`"main"`** for the main thread. **Sorted ascending**, so a renderer can diff two snapshots directly. A non-empty array means the chip is `permission`; a renderer may name the blocked teammate from it. Claude-only. Additive; consumers tolerate its absence. |

#### `status` value mapping

`status` is derived from the hook `event` that last fired. The two agents share
most of the vocabulary; Codex additionally maps `PreToolUse`:

| Hook event | `claude` status | `codex` status |
|------------|-----------------|----------------|
| `UserPromptSubmit` | `working` | `working` |
| `PreToolUse` | (unmapped) | `working` |
| `PostToolUse` | `working` | `working` |
| `PermissionRequest` | `permission` | `permission` |
| `Stop`, `SessionStart` | `idle` | `idle` |
| (any other / unknown) | unchanged | unchanged |

⚠ Codex caveat: Codex does **not** record approval requests in its on-disk
rollout, so the `permission` status is recoverable only from a live
`PermissionRequest` hook — there is no transcript-tail self-heal for it (see the
`permission` self-heal below, which is Claude-only today). A Codex session with
no hooks configured shows only `working`/`idle`.

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
  `codex` enrichment block) are **not** breaking;
  consumers must ignore unknown fields and tolerate missing optional fields. The
  `claude` block is unchanged — a consumer reading `.claude.status` keeps working;
  to be agent-aware, read `.codex.status` too (e.g. `.claude.status // .codex.status`).
  The Go struct behind `claude`/`codex` was renamed `ClaudeInfo` → `AgentInfo`
  (with a `ClaudeInfo` alias retained); the wire format is unchanged.
- **Empty vs. absent:** always-present string fields use `""` for "unknown";
  optional blocks are **omitted** entirely (never `null`) when unresolved.
  `claude.session_id`/`transcript` are omitted when empty; `claude.status` is
  present-but-`""` before the first status hook.
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
