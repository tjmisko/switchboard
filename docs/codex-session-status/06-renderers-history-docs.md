# Phase 6 — renderers, history surfaces, and documentation

This phase has three disjoint workstreams that may run in parallel after daemon
integration begins from the merged W1 baseline. They consume the additive state
wire contract and must not reinterpret provider-specific evidence.

## Part A — C7 reference TUI and Waybar

### Ownership

**Exclusive write ownership:**

- `cmd/claude-tui/**`
- `cmd/switchboard-waybar/**`
- `internal/barlayout/**`

Do not edit state, RPC, daemon, providers, CLI control commands, or external bar
configuration.

### Reference TUI behavior

The reference TUI is the canonical nested presentation. Keep the existing
binary name for compatibility in this feature; a future rename to a provider-
neutral command is separate.

For each root session:

```text
● main/project       permission · waiting on child documents
  ├─ ● documents     user input · 22m
  ├─ ● tagging       active · 21m
  ├─ ○ keyboard      completed · 20m
  └─ ● metadata      approval · 3m
```

Requirements:

- Root rows preserve current focus/workspace/CWD/navigation information.
- Child rows are indented by graph depth and are never described as focusable.
- Prefer nickname, then role, then a short stable ID fallback.
- Display approval and user-input waits distinctly.
- Active is green, idle is yellow/orange, waiting is red, completed/not-loaded
  is grey, and system error has a distinct textual marker even if v1 has no new
  bar color.
- Suspended applies to the root process; child states remain visible but clearly
  stale with the root.
- Durations derive from node state timestamps. Usage is shown only when present;
  never display a fabricated zero.
- Render deterministic graph order from state; do not resort differently in
  each renderer.
- Avoid unbounded historical output. Render live nodes plus provider-retained
  current-turn/recent terminal nodes; history commands own older agents.
- Empty graph falls back to the existing root-only row.

Update stale provider-specific wording such as "no claude sessions" in rendered
output while retaining binary compatibility.

### Waybar behavior

Waybar slots remain one per root session. Child agents do not consume slots and
do not alter focus/cycle selectors.

Enhance the root tooltip with a compact indented tree:

- attention rows first, then active, idle, terminal;
- explicit `approval` versus `question` wording;
- nickname/role and elapsed duration;
- a folded `+N more` suffix when the tree exceeds a bounded line count;
- legacy in-flight/workflow/pending text when no graph is present.

The chip class uses the shared summary status already projected into state. The
renderer must not independently aggregate nodes and risk disagreeing with the
daemon. It may inspect nodes only for tooltip detail.

### C7 tests and acceptance

- Golden root-plus-tree frames for Codex and Claude.
- Nested depth/order and fallback naming.
- Approval and user-input child wording.
- Root attention color comes from summary; children never create slots.
- Same root-only output for snapshots without `agent_graph`.
- Waybar tooltip folding and no unbounded lines.
- Suspended/stale and unknown/error presentation.
- Optional usage omitted versus populated.
- Existing focus, label-fitting, workflow, and pending-writer tests remain green.
- Package tests pass and only C7-owned paths changed.

## Part B — C8 history, timeline, and diagnostics

### Ownership

**Exclusive write ownership:**

- `cmd/switchboard-ctl/history.go` and matching tests
- `cmd/switchboard-ctl/timeline.go` and matching tests
- `cmd/switchboard-ctl/diagnose.go` and matching tests

C8 does not edit `cmd/switchboard-ctl/main.go`; C6 owns the Codex hook-forwarding
sections there. If a new subcommand/flag needs main registration, C8 supplies a
small patch description to C6 or the coordinator.

### History output

Teach human and JSON history views about canonical `agent_state` events:

- group child events under the root lane;
- show nickname/role with privacy-tier enforcement;
- distinguish runtime, attention, and lifecycle transitions;
- preserve existing Claude `subagent_spawn`/`subagent_stop` output;
- dedupe legacy and canonical projections in human output when they describe
  the same transition, without dropping either raw JSON event.

### Timeline accounting

Do not silently change historical cost/attention formulas merely because richer
nodes exist. Add graph-derived spans only under explicit rules:

- lifecycle running/pending bounds an agent activity lane;
- terminal lifecycle closes it;
- reconnect/resnapshot duplicate events do not open duplicate lanes;
- approval/user-input attention is attributed to the root user-attention total
  according to the existing definition, with child identity retained;
- stale/expired observations cannot stretch a lane to the query boundary
  without the existing suspect/cap treatment.

Maintain compatibility with history files that have no canonical agent events.

### Diagnostics

Add content-free diagnostics for:

- Codex root bound/unbound;
- graph source and freshness/expiry;
- app-server complete versus partial snapshot;
- node counts by live/waiting/error;
- last provider error category without raw payload/content;
- legacy summary versus graph summary during Claude shadow mode.

Diagnostics must not suggest CWD correlation as a fix for an unbound root.

### C8 tests and acceptance

- Old history fixtures produce unchanged summaries.
- Canonical graph events produce correct nested spans and attention totals.
- Reconnect/resnapshot dedupe is pinned.
- Privacy tiers redact descriptions/names as required.
- Suspect/phantom-lane protections remain effective.
- Diagnostics cover bound, unbound, stale, disconnected, and shadow-mismatch
  states without leaking payload content.
- Only C8-owned paths changed.

## Part C — C9 public and schema documentation

### Ownership and start gate

**Exclusive write ownership:**

- `README.md`
- `docs/codex-investigation.md`
- `docs/state-schema.md`
- `docs/history-schema.md`
- `docs/status-color-state-model.md`

Start after W2 behavior and JSON are frozen. C9 documents merged behavior; it
does not design new fields or edit implementation.

### Required updates

`README.md`:

- explain root sessions versus nested child agents;
- describe app-server proxy primary integration and automatic degradation;
- include the exact current Codex hook configuration shape only for identity and
  fallback enrichment;
- explain approval versus user-input display;
- state version/capability requirements and the `off`/fallback mode;
- update provider-specific "Claude sessions" wording where it describes all
  providers.

`docs/codex-investigation.md`:

- mark the old hook-primary recommendation superseded;
- retain historical findings that are still true;
- point to this planning packet and the implemented app-server observer;
- distinguish supported protocol from internal rollout/SQLite evidence.

`docs/state-schema.md`:

- document every `agent_graph`, summary, and node field;
- state optionality, enum forward compatibility, deterministic ordering,
  freshness, and legacy projection rules;
- add root-only, Codex tree, Claude tree, stale/disconnected, and unknown
  examples;
- state that child nodes are not navigation sessions.

`docs/history-schema.md`:

- document canonical agent transition events, dedupe identity, privacy behavior,
  and compatibility with legacy subagent events.

`docs/status-color-state-model.md`:

- add the graph reducer truth table and explicit wait-reason handling;
- preserve the legacy four-color mapping;
- explain error/unknown behavior and source freshness.

Use official OpenAI sources for current Codex protocol/hook claims:

- <https://learn.chatgpt.com/docs/agent-configuration/subagents>
- <https://learn.chatgpt.com/docs/app-server>
- <https://learn.chatgpt.com/docs/hooks>

### C9 acceptance

- Every documented JSON example round-trips through the merged state types.
- Hook configuration is copied from the current official shape and validated as
  JSON/TOML as applicable.
- Documentation does not claim hooks are primary Codex status truth.
- Documentation does not claim children own PIDs or switchable terminals.
- No implementation or planning-packet files changed.

## Parallel merge rules

C7, C8, and C9 own disjoint paths, but C9 starts later because it needs frozen
behavior. Merge C6 first, then C7 and C8 in either order. Run their tests before
starting C9. The coordinator resolves any requested main-command registration
instead of allowing C8 and C6 to edit `cmd/switchboard-ctl/main.go`
concurrently.
