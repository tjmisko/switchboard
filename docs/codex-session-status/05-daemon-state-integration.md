# Phase 5 — state/history projection and daemon integration

This phase has two sequentially gated workstreams with disjoint ownership:

- **C5** adds the additive state/history representation during Wave 1.
- **C6** wires provider observers into the daemon during Wave 2, after both
  adapters and C5 are merged.

C5 and C6 must not run concurrently from different baselines. C6 starts from the
merged W1 commit.

## Part A — C5 state and history projection

### Ownership

**Exclusive write ownership:**

- `internal/state/**`
- `internal/history/**`

Do not edit daemon, RPC, provider packages, CLI/renderers, or schema docs.

### Additive state wire shape

Add one optional graph field to `state.Session`, recommended wire key
`agent_graph`:

```jsonc
{
  "pid": 123,
  "agent": "codex",
  "codex": {
    "session_id": "root-thread",
    "status": "permission"
  },
  "agent_graph": {
    "root_id": "root-thread",
    "source": "codex_app_server",
    "observed_at": "...",
    "fresh_until": "...",
    "complete": true,
    "summary": {
      "runtime": "idle",
      "attention": "user_input",
      "status": "permission",
      "live_children": 2,
      "waiting_nodes": 1,
      "error_nodes": 0,
      "since": "..."
    },
    "nodes": [
      {
        "id": "root-thread",
        "runtime": "idle",
        "attention": "none",
        "lifecycle": "running"
      },
      {
        "id": "child-thread",
        "parent_id": "root-thread",
        "nickname": "documents",
        "role": "explorer",
        "runtime": "active",
        "attention": "user_input",
        "lifecycle": "running"
      }
    ]
  }
}
```

The exact Go projection types live in `internal/state`; do not expose mutable
`agentgraph` slices by pointer. Snapshot cloning and persistence must deep-copy
the graph and preserve deterministic node order.

Omit zero/unknown optional metadata where existing schema style does so, but do
not omit the node's runtime/attention/lifecycle axes: explicit unknown values
make forward/backward behavior testable.

### Legacy compatibility projection

- Continue emitting `Session.Agent` (`agent`) and exactly one `claude`/`codex`
  enrichment block.
- Project graph `Summary.LegacyStatus` into the existing block's `status`.
- Project root ID into `session_id`.
- Keep `status_since` semantics: it changes only on summary transition.
- Preserve Claude `in_flight_subagents`, workflows, pending tool, and pending
  writers from the Claude adapter compatibility view.
- A consumer that ignores `agent_graph` must render the same chip color and
  navigation behavior as before.
- A persisted snapshot with no graph must still hydrate and decode.

Do not rename/delete `AgentInfo` fields in this phase. Mark provider-specific
legacy fields in comments so a later schema version can retire them.

### History projection

Add an additive canonical event for graph-node transitions, recommended type
`agent_state`, with fields:

- provider/root session ID;
- thread ID and parent thread ID;
- nickname/role when privacy level permits;
- from/to runtime, attention, and lifecycle;
- observation source;
- transition timestamp and previous-state duration.

Continue emitting existing `subagent_spawn`/`subagent_stop` events for Claude
compatibility. Codex node appearance/completion may also project to those legacy
events when the semantics are exact, but `agent_state` is the authoritative
provider-neutral record.

History dedupe keys include root ID, node ID, axis, target value, and transition
identity/time so a reconnect/resnapshot cannot duplicate events. Privacy tiers
must not start recording prompts, reasoning, commands, tool inputs, or raw app-
server payloads.

### C5 tests and acceptance

- Golden JSON for Claude without graph remains unchanged.
- Golden JSON for old Codex enrichment still decodes.
- New graph JSON is deterministic and additive.
- Snapshot/store mutation tests prove deep-copy isolation.
- Hydration of missing, partial, and expired graphs is safe.
- Status-since transition behavior is pinned.
- History dedupes reconnect/resnapshot and honors privacy tiers.
- `go test -race ./internal/state ./internal/history` passes.
- Only C5-owned paths changed.

## Part B — C6 daemon and RPC integration

### Ownership and prerequisites

**Exclusive write ownership:**

- `cmd/switchboard/**`
- `internal/rpc/**`
- only the Codex-hook forwarding portions of `cmd/switchboard-ctl/main.go` and
  directly associated tests

**Start only after:** C3, C4, and C5 are merged at W1.

C6 consumes provider/state contracts. It does not edit their packages. Any
defect found there is routed to the owning workstream.

### Runtime topology

Construct one provider observer per provider at daemon startup. The Codex
observer may launch its proxy lazily when the first bound Codex root appears and
stop retrying when no Codex roots remain. Claude observation retains periodic
reconciliation.

Two triggers request observation:

- the existing periodic reconcile tick;
- a coalesced provider `Updates()` invalidation.

Both flow through one serialized reconcile coordinator. An older periodic
result must not land after and overwrite a newer event-triggered result. Carry
an observation time/generation and reject stale application.

### Lock discipline

Per cycle:

1. Take a read snapshot of live root references.
2. Resolve terminal/window data and call provider `Observe` **outside**
   `Store.Apply`.
3. Compute graph summaries and history diffs outside the lock where possible.
4. Enter `Store.Apply` only to verify root identity/generation, assign immutable
   state projections, update small in-memory transition state, and enqueue
   already-built history events.

No app-server, transcript, rollout, `/proc/environ`, filesystem stat, subprocess,
or JSON-RPC wait may occur while holding the store lock. Add a lock-budget test
using blocking fakes to prove it.

### Session lifecycle

- On process discovery, create the root as today and schedule an immediate
  provider observation.
- On exact Codex environment/hook binding, register the binding and resnapshot.
  Any later lifecycle hook self-heals a `SessionStart`/discovery race, and an
  exact identity persisted for the same PID/start pair is restored after daemon
  restart.
- On process death, call provider `Forget` before discarding the root; close any
  graph/history lanes once.
- PID reuse is guarded by PID plus start identity.
- On daemon shutdown, close observers once after reconcile loops stop accepting
  work.
- A provider failure never removes a live root session or disables navigation.

### Source precedence

For Codex:

1. Fresh complete app-server observation.
2. Fresh partial app-server observation, merged only according to explicit
   completeness rules.
3. Exact hook fallback for root identity/summary.
4. Optional rollout root task-boundary fallback.
5. Restored last-known graph until its freshness expiry.
6. Unknown.

A lower source cannot overwrite a fresher higher source. In particular,
`PostToolUse` cannot clear an app-server-reported child approval or user-input
wait.

For Claude, the Claude adapter owns source fusion. Daemon code applies its
observation and shared reducer output without interpreting transcript details.

### Hook integration

- Keep the shared external hook RPC envelope backward compatible.
- Route Claude hook payloads into the Claude adapter's hook API.
- Route Codex `SessionStart` session ID into the Codex exact-binding registry.
- Treat Codex hook status as fallback only when no fresh app-server graph is
  authoritative.
- Do not apply Claude's assumption that `agent_id` exists on every child tool
  hook to Codex.
- `SubagentStart`/`SubagentStop` may trigger an immediate resnapshot, but the
  app-server parent/thread graph remains truth.

### Claude shadow and authority flip

Initially run both the legacy Claude summary and graph reducer. Record
rate-limited, content-free mismatch diagnostics containing rule/status/counts.
Use the C4 parity fixtures to resolve mismatches.

Flip the graph summary to authority only after:

- canonical status cases match;
- multi-writer permission tests match;
- workflow/fanout counts match;
- timing-hazard tests match or an intentional difference is documented.

After the flip, delete only code proven redundant and still owned by C6. Keep
compatibility wire projections.

### C6 tests

- one Codex TUI plus repeated tool calls remains one root;
- exact two same-CWD roots bind to different thread graphs;
- no identity -> root visible/navigable with unknown graph, never guessed;
- child approval and user input immediately turn the root red;
- idle root with active child -> delegating; drain -> idle;
- stale/disconnected app-server expires status and reconnect resnapshots;
- a late old-generation observation cannot overwrite a newer one;
- process death forgets binding/cache/history exactly once;
- provider blocking fake proves no I/O under `Store.Apply`;
- update storms coalesce without starving periodic reconciliation;
- Claude shadow parity and authority flip cases;
- daemon shutdown has no leaked proxy/observer goroutines;
- RPC/state subscribers continue to receive backward-compatible snapshots.

### C6 acceptance

- Fake app-server end-to-end scenario passes under `-race`.
- Existing daemon/RPC timing, lifecycle, fanout, and permission tests remain
  green or have reviewed intentional expectation changes.
- No provider-specific event parsing enters the neutral reducer/state package.
- No CWD-based binding exists.
- Only C6-owned paths changed.

## Merge order

1. Merge C5 in Wave 1 and freeze the additive wire projection.
2. Merge C3 and C4; run provider tests.
3. Branch C6 from the merged W1 baseline.
4. Merge C6 first in Wave 2 so renderer/CLI branches see the actual emitted
   snapshot during conflict-free rebases or final merge.
