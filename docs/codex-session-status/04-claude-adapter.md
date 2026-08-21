# Phase 4 — Claude provider adapter

## Mission

Project Claude's existing hook, transcript, subagent-directory, workflow, and
per-writer permission evidence into the same neutral agent graph used by Codex,
without regressing the current status/color behavior.

This is an adapter and migration phase, not a rewrite of Claude inference. The
existing engine remains a compatibility oracle until shadow comparisons and
tests prove equivalent summaries.

## Ownership and start gate

**Workstream:** C4

**Exclusive write ownership:**

- `internal/provider/claude/**`
- `internal/fanout/**`

**Start only after:** W0 contract freeze.

Do not edit state, daemon, RPC, transcript, history, or renderers. C6 performs
the later wiring and deletion/retirement of duplicated legacy paths.

## Existing evidence to preserve

The current Claude implementation combines several signals that are not
available from one authoritative stream:

- root hook status (`UserPromptSubmit`, `PostToolUse`, `PermissionRequest`,
  `Stop`, `SessionStart`);
- per-writer prompt ownership and tool/input correlators;
- main and child transcript resolution checks;
- directory scans for `agent-*.meta.json` and `agent-*.jsonl`;
- parent transcript Task launches/results;
- asynchronous launch acknowledgements that are not completions;
- workflow journals and workflow-child transcripts;
- stale writer and interrupted-turn recovery;
- terminal-title recovery for a silent abort, applied later by the daemon.

Codex must not inherit these heuristics, but Claude must keep them until a
Claude-native authoritative source exists.

## Adapter shape

Implement `provider.Observer` and expose a Claude-specific hook ingestion method
for C6:

```go
type HookSignal struct {
    Root          provider.RootRef
    Event         string
    AgentID       string
    AgentType     string
    ToolName      string
    ToolInputHash string
    At            time.Time
}

func (o *Observer) ApplyHook(HookSignal) HookResult
```

The exact API may vary, but the provider package owns the semantics. RPC should
eventually translate wire input into `HookSignal`; it should not continue to be
the permanent home of Claude status reduction.

Keep adapter state keyed by root key/session ID and child ID. Returned graph
observations are deep copies and carry transcript/fanout freshness.

## Graph construction

### Root node

- ID is the exact Claude session ID.
- Runtime derives from root-thread hook/transcript/title evidence.
- A main-thread pending approval maps to root attention.
- `AskUserQuestion` or the equivalent known question tool maps to
  `user_input`; command/file/tool approval maps to `approval`.
- Unknown/ambiguous prompt type maps conservatively to `approval`, preserving
  the current attention-red behavior.

### Child nodes

- ID is the normalized bare Claude `agent_id`, scoped to the root graph.
- Direct child files under the session subagent directory attach to the root.
- A deeper parent is represented only when metadata/path evidence identifies it
  explicitly. Never invent ancestry from timing.
- Nickname/type/description come from sanitized metadata already used for
  labels/history.
- Spawned with no completion -> pending/running according to available
  transcript activity.
- Authoritative completion/result -> completed.
- Interrupted/stale force-close evidence -> interrupted, retaining the existing
  suspect reason for history/diagnostics.
- A child-owned pending prompt sets that child's attention, not the root node's
  attention. The shared reducer then makes the root summary attention-red.
- Include every live child plus terminal children attributable to the current or
  most recent parent turn. Because Claude's artifact directory is append-only,
  explicitly prune older completed children from the live graph while retaining
  their durable history events.

### Workflows

Workflow children enter the same graph. Preserve workflow ID/name as optional
group metadata or node description; do not add a fourth provider-specific
status axis. Existing `Workflows` wire output remains a compatibility
projection until consumers migrate.

## Fanout observer evolution

Extend `internal/fanout.Observer` to return a structured snapshot in addition to
its existing count/events API. Keep the old `Reconcile` behavior available
during migration so current daemon/RPC tests remain green before C6 rewires
them.

The structured result should include:

- stable child ID;
- explicit parent when known;
- type/nickname/description;
- spawn/start/update/completion timestamps;
- lifecycle and suspect/stale-close reason;
- workflow association;
- transcript path as internal adapter data only, not public graph output.

Directory scan remains the authoritative Claude spawn source. Parent transcript
cursors and hooks are accelerators/cross-checks. Preserve restart idempotence and
the existing history seen-set semantics.

## Permission behavior

Port or wrap the existing per-writer state machine rather than replacing it with
a scalar:

- pending prompts remain keyed by normalized writer ID (`""` means main);
- tool name/input hash remain call correlators, never writer identity;
- a teammate's completion cannot clear another writer's prompt;
- multiple blocked writers keep aggregate attention until all resolve;
- subagent hook activity cannot make the main root appear active when the main
  is idle, except through the reducer's delegating rule;
- stale/unreadable evidence fails toward retaining attention until the existing
  bounded backstop applies.

The adapter should be able to export enough compatibility state for C5/C6 to
continue populating legacy `pending`, `pending_writers`, and workflow fields.

## Shadow migration

C6 will integrate in two steps:

1. **Shadow:** legacy Claude code remains authoritative for the legacy status.
   The adapter receives the same hooks/reconcile inputs, emits a graph, and the
   daemon compares the graph reducer summary to the legacy result. Mismatches are
   rate-limited diagnostics with rule names, never user content.
2. **Flip:** after parity fixtures pass, the graph summary becomes authoritative.
   Legacy fields are projections from adapter state. Existing public wire values
   and history remain unchanged.

C4 must provide deterministic comparison fixtures and explain any intentional
differences. It does not perform the flip itself.

## Tests

Reuse/copy representative existing fixtures into adapter-local tests as needed,
without editing tests owned by RPC/state workstreams. Cover:

- root working/idle/permission transitions;
- main versus child prompt ownership;
- simultaneous blocked writers;
- teammate tool-name collision;
- async launch acknowledgement is not completion;
- direct child spawn/completion and daemon restart seeding;
- idle root plus active children -> reducer delegating;
- child approval/user question -> child attention plus root red summary;
- workflow child progress and drain;
- stale child force-close and suspect reason;
- compaction/truncation cursor reset;
- missing/partial metadata and deterministic node order;
- repeated observe/update/forget/close under `-race`.

## Acceptance

- Existing fanout package tests stay green.
- Adapter tests reproduce the canonical Claude status cases and child counts.
- Structured fanout contains enough information to render individual child
  rows, not just a count.
- Child attention is attributed to the correct node.
- No Codex/app-server code or enum appears in this package.
- Only owned paths changed.

## Handoff to C6

Return:

- constructor/lifecycle requirements;
- hook-ingestion API;
- compatibility projection API;
- structured fanout semantics and freshness;
- shadow-comparison fixtures and any intentional deviations from legacy status.

C6 owns routing live hooks and making the eventual authority flip.
