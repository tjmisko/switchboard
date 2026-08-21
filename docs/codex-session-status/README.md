# Codex session status and agent-tree integration

> **Status: APPROVED FOR IMPLEMENTATION PLANNING.** The product decisions in
> this document are locked: child agents are nested under their root session,
> Codex app-server is the primary Codex truth source, and approval/user-input
> waits remain distinct on child rows while either makes the root need
> attention.

> **Post-rollout finding (2026-08-21):** shared app-server ownership invalidates
> the implemented hook-parent-to-visible-TUI binding for newly created threads.
> The planning packet remains useful design history, but its shared-daemon
> assumption is not operationally safe until an exact client/thread association
> exists. See the
> [incident report](../codex-app-server-hook-attribution-incident.md).

This directory is an implementation-ready planning packet for adding accurate
Codex status and fanout visibility to the Go daemon without forcing Codex
through Claude-specific transcript heuristics. It is organized for a
multi-agent implementation: every workstream has exclusive path ownership,
explicit prerequisites, and a merge gate.

## Outcome

Switchboard continues to discover and navigate **root interactive processes**.
Each root may additionally carry a provider-owned **agent graph**:

```text
OS discovery --------> root Session (PID, tty, cwd, focus target)
                              |
provider observer ---> AgentGraph (root thread + nested child threads)
                              |
shared reducer ------> summary status for the root chip
                              |
wire/renderers ------> root chip + non-switchable child rows
```

The graph is the shared abstraction. How it is produced remains provider
specific:

- Claude: hooks plus transcript/subagent-directory observation and existing
  per-writer recovery logic.
- Codex: the shared app-server protocol through `codex app-server proxy`, with
  hooks for exact identity where needed and rollout/SQLite data used only as a
  degraded fallback.

## Locked product and architecture decisions

1. **Only root sessions are switchable.** A Codex or Claude child agent has no
   independent terminal target, so it never becomes a top-level `Session`, bar
   slot, focus selector, or cycle target.
2. **The tree is visible.** Reference renderers show a root row followed by
   indented child rows. Compact renderers may put the same detail in a tooltip,
   but must consume the same graph.
3. **Codex app-server is primary truth.** The implementation uses the supported
   proxy command rather than reading the control socket directly. It snapshots
   existing descendants and then consumes live notifications.
4. **Do not guess root identity by CWD.** Prefer `CODEX_THREAD_ID` from the root
   process environment on Linux, then an exact lifecycle-hook association.
   `SessionStart` normally establishes it; a later hook self-heals a discovery
   race. Same-CWD/time correlation may be diagnostic evidence but never a
   binding.
5. **Status has separate axes.** Runtime activity, attention reason, and child
   lifecycle are represented independently and collapsed only by a shared
   reducer.
6. **Approval and user input stay distinct.** Both make the root attention-red;
   the child row says which kind of response is required.
7. **Existing wire keys remain compatible.** `claude`, `codex`, and their legacy
   summary statuses remain readable. The agent graph is additive.
8. **Provider-specific inference stays in provider adapters.** Claude's pending
   writer map, workflow artifacts, and transcript self-heals do not enter the
   Codex adapter or the neutral graph package.
9. **No I/O under `Store.Apply`.** App-server calls, transcript reads, rollout
   reads, process-environment reads, and observer reconciliation happen before
   the state-store write lock is acquired.

## Canonical status reduction

The neutral node carries three axes:

| Axis | Initial values | Meaning |
|---|---|---|
| Runtime | `unknown`, `not_loaded`, `idle`, `active`, `system_error` | What the thread is doing now |
| Attention | `none`, `approval`, `user_input` | Whether a person must respond, and why |
| Lifecycle | `unknown`, `pending`, `running`, `completed`, `interrupted`, `errored`, `shutdown`, `not_found` | Orchestration lifecycle, especially for children |

The reducer derives the legacy root-chip summary in this priority order:

1. Any live node with `approval` or `user_input` -> `permission`.
2. Root runtime `active` -> `working`.
3. Root idle/not-loaded and any live descendant active/pending/running ->
   `delegating`.
4. No active/waiting live node and root idle -> `idle`.
5. No fresh authoritative observation -> empty legacy status (`unknown` to
   renderers).

`system_error` remains explicit on the node. The first version folds a root
system error to unknown rather than silently equating an error with a user
permission prompt. A later UI decision may add a distinct error color.

## Workstreams and exclusive ownership

The coordinator must enforce this table. An agent may read any path, but may
write only its owned paths. If a necessary change falls outside ownership, the
agent reports a handoff instead of editing it.

| ID | Workstream | Exclusive write ownership | Plan |
|---|---|---|---|
| E0 | Live ground truth | `internal/provider/codex/testdata/captures/**`; one new `evidence-report.md` | [00](00-live-ground-truth.md) |
| D1 | Discovery hardening | `internal/discovery/**` | [01](01-discovery-hardening.md) |
| C2 | Neutral graph contract | `internal/agentgraph/**`, `internal/provider/provider.go` | [02](02-agent-graph-contract.md) |
| C3 | Codex observer | `internal/provider/codex/**`, except `testdata/captures/**` | [03](03-codex-app-server-observer.md) |
| C4 | Claude adapter | `internal/provider/claude/**`, `internal/fanout/**` | [04](04-claude-adapter.md) |
| C5 | State/history projection | `internal/state/**`, `internal/history/**` | [05](05-daemon-state-integration.md) |
| C6 | Daemon integration | `cmd/switchboard/**`, `internal/rpc/**`, Codex-hook sections of `cmd/switchboard-ctl/main.go` and its tests | [05](05-daemon-state-integration.md) |
| C7 | Renderers | `cmd/claude-tui/**`, `cmd/switchboard-waybar/**`, `internal/barlayout/**` | [06](06-renderers-history-docs.md) |
| C8 | CLI/history surfaces | `cmd/switchboard-ctl/history*`, `timeline*`, `diagnose*` | [06](06-renderers-history-docs.md) |
| C9 | Documentation | `README.md`, `docs/codex-investigation.md`, `docs/state-schema.md`, `docs/history-schema.md`, `docs/status-color-state-model.md` | [06](06-renderers-history-docs.md) |
| V7 | Validation coordinator | Test execution and `validation-report.md`; no implementation files | [07](07-validation-rollout.md) |

The planning files in this directory are coordinator-owned after work begins.
Implementation agents do not mark their own phase complete here; they report
their commit and evidence to the coordinator.

## Coordinator launch runbook

1. Commit this planning packet by itself and record that clean commit as the
   planning baseline. Do not launch implementation from a checkout containing
   unrelated uncommitted changes.
2. Create one integration branch from that baseline. The coordinator alone
   checks workstream commits onto or merges them into this branch.
3. For each wave, create one worktree/branch per workstream from the gate commit
   named by that wave. Verify the ownership paths are clean before launching.
4. Give each agent the overview, its one phase document, the baseline commit,
   worktree path, workstream ID, and ownership row. Require the standard handoff
   below.
5. Do not allow leaf workstream agents to spawn additional writing agents. A
   leaf may use read-only research help only if it cannot create files, run the
   live app-server, or make Git changes; otherwise all delegation goes through
   the coordinator.
6. Wait for every agent in the wave. Reject out-of-scope changes before merging.
   Merge in the order named by the wave gate, then run the gate tests from the
   integration worktree.
7. If a shared contract must change, stop the affected wave, land the amendment
   once through the coordinator, recreate/rebase all affected branches onto the
   same commit, and resume. Never let two agents repair the contract separately.
8. Remove worktrees only after their commits are merged and the gate is green;
   never prune while agents are running.

Reusable leaf-agent prompt:

```text
You own workstream <ID> for the Codex session-status plan.

Read docs/codex-session-status/README.md and <PHASE-DOC> completely before
acting. Your baseline is <COMMIT>; work only in <WORKTREE> on <BRANCH>.

You may write only: <OWNED PATHS>. Read other files as needed, but do not edit
them. Do not change shared contracts, merge/rebase other branches, modify user
configuration, touch live services, or spawn additional writing agents. If you
need an out-of-scope change, stop that part and include a precise handoff request.

Implement the phase test-first, run the package-local tests named in the phase,
commit your owned changes, and return the standard handoff: commit, files,
tests/results, fixtures, gaps/handoffs, and confirmation that no user config or
live service was changed.
```

## Parallel execution waves

### Wave 0 — independent foundations

Start E0, D1, and C2 in separate worktrees from the same baseline.

- E0 captures one controlled Codex fanout and sanitizes protocol fixtures.
- D1 fixes false process discovery and expands the argv matrix.
- C2 implements the neutral graph, provider interface, reducer, and contract
  tests using the already-generated Codex 0.149 schema as its starting fixture.

They write disjoint paths. E0 is the only agent permitted to connect to the live
Codex app-server. C2 must not change its public contract in response to a late
capture without coordinator review.

**Gate W0:** merge all three into the integration branch; run the graph and
discovery tests; review the neutral API. Tag or record the resulting commit as
the **contract-freeze baseline**.

### Wave 1 — provider adapters and projections

Branch C3, C4, and C5 from the W0 contract-freeze baseline.

- C3 builds the Codex app-server adapter from E0 fixtures.
- C4 wraps the existing Claude fanout/status evidence as graph observations.
- C5 adds additive state/history wire projections and compatibility tests.

These agents consume `internal/agentgraph` and `internal/provider`; they may not
edit either. If the frozen API is insufficient, the coordinator lands one small
contract amendment first, rebases all Wave-1 worktrees onto it, and only then
resumes work. No Wave-1 agent independently edits the contract.

**Gate W1:** merge C5, C3, and C4; run their package tests and wire goldens.
Confirm provider observers have no dependency on `internal/state` or renderer
packages.

### Wave 2 — daemon and user-facing surfaces

Branch C6, C7, and C8 from the merged W1 baseline.

- C6 owns daemon lifecycle, root-thread binding, snapshots, subscription, and
  hook fallback wiring.
- C7 owns the reference TUI and Waybar presentation.
- C8 owns history/timeline/diagnostic presentation.

They consume the frozen state wire shape and write disjoint command/package
paths. C6 is the only Wave-2 agent allowed to change daemon orchestration or RPC.

**Gate W2:** merge C6 first, then C7 and C8. Run package tests, fake app-server
end-to-end tests, and a manual snapshot inspection.

### Wave 3 — documentation, validation, and rollout

Start C9 and V7 after W2.

- C9 updates public/schema/design documentation against the merged behavior.
- V7 runs validation read-only and writes a report. It sends defects back to
  the workstream that owns the affected path; it does not opportunistically fix
  other agents' code.

**Gate W3:** all acceptance scenarios in [07](07-validation-rollout.md) pass,
documentation matches emitted JSON, and the feature can be disabled or degraded
without affecting root discovery/navigation.

## No-race execution protocol

These rules are part of the plan, not suggestions:

1. **One worktree and branch per agent.** Never run two writing agents in the
   same checkout. Branch names should include the workstream ID, for example
   `codex-status/c3-appserver`.
2. **One merge coordinator.** Only the coordinator merges, rebases integration,
   edits these planning documents, or resolves conflicts. Agents return commits;
   they do not merge one another.
3. **Exclusive path ownership.** An agent stops and reports when a required edit
   is outside its row in the ownership table. Cross-owner fixes are routed back
   after the current wave, never made concurrently.
4. **Freeze shared contracts at W0.** Wave-1 agents import the graph/provider API
   but do not edit it. Contract changes are serialized through the coordinator.
5. **Unique runtime paths.** Every agent sets unique `GOCACHE`, `GOTMPDIR`, Unix
   socket, state file, and history directory paths under `/tmp`. No test uses the
   installed daemon's socket or state.
6. **One live-app-server reader.** E0 owns live capture. All other agents use
   recorded fixtures or a fake server. Nobody starts, stops, or restarts the
   user's app-server daemon, Switchboard service, Waybar, or terminal sessions.
7. **No user configuration mutation.** Agents do not edit `~/.codex/config.toml`,
   `~/.codex/hooks.json`, systemd units, or bar configuration. Hook tests inject
   stdin fixtures and temporary configuration.
8. **No global Git maintenance.** Agents do not run `git gc`, `git worktree
   prune`, destructive resets, or clean commands while other worktrees exist.
9. **Package-local tests during waves.** `go test ./...` and race tests run only
   at merge gates. This avoids unrelated agents competing for global resources
   and makes ownership of failures clear.
10. **Defect routing, not drive-by edits.** Validation reports the failing test,
    owner, expected behavior, and fixture. The owner fixes it in its worktree and
    returns a new commit.

Suggested per-agent environment (replace the suffix):

```bash
export GOCACHE=/tmp/switchboard-codex-c3-gocache
export GOTMPDIR=/tmp/switchboard-codex-c3-tmp
mkdir -p "$GOCACHE" "$GOTMPDIR"
```

Tests that create sockets/state/history must use `t.TempDir()` or an equivalent
workstream-specific `/tmp` directory.

## Merge contract

Every agent handoff contains:

- branch and commit hash;
- files changed, all within its ownership boundary;
- tests run and exact result;
- fixtures added or consulted;
- known gaps and any requested cross-owner change;
- confirmation that no user configuration or live service was modified.

The coordinator rejects a handoff that changes an unowned path, weakens a
contract test, or relies on a heuristic CWD-to-thread binding.

## Definition of done

- A real Codex TUI produces exactly one root session; sandbox/tool wrappers
  produce none.
- A Codex root binds to the correct app-server thread without CWD guessing.
- New and already-running child threads appear beneath their root with stable
  parentage, nickname/role when present, lifecycle, runtime, and attention.
- Approval and user-input waits are distinct on child rows and both make the
  root chip attention-red.
- An idle root with active descendants is `delegating`; after drain it becomes
  idle without a stale grace-period latch.
- App-server disconnect does not freeze a falsely authoritative state; freshness
  expires and the implementation reconnects/resnapshots.
- Claude's current status, workflows, pending writers, and fanout behavior stay
  compatible while being projected through the neutral graph.
- Existing JSON consumers continue to decode snapshots. New consumers can
  render the full graph.
- Provider I/O is outside the state-store lock and the race suite finds no data
  races.
- Public and schema documentation describe the shipped configuration and
  degraded modes accurately.

## Phase documents

1. [Live ground truth and fixtures](00-live-ground-truth.md)
2. [Codex process discovery hardening](01-discovery-hardening.md)
3. [Neutral agent-graph contract](02-agent-graph-contract.md)
4. [Codex app-server observer](03-codex-app-server-observer.md)
5. [Claude provider adapter](04-claude-adapter.md)
6. [State projection and daemon integration](05-daemon-state-integration.md)
7. [Renderers, history surfaces, and documentation](06-renderers-history-docs.md)
8. [Validation and rollout](07-validation-rollout.md)

## Primary references

- [OpenAI Codex subagents](https://learn.chatgpt.com/docs/agent-configuration/subagents)
- [OpenAI Codex app-server](https://learn.chatgpt.com/docs/app-server)
- [OpenAI Codex hooks](https://learn.chatgpt.com/docs/hooks)
- Existing process classifier: `internal/discovery/discovery.go`
- Existing Claude fanout observer: `internal/fanout/observer.go`
- Existing session wire model: `internal/state/state.go`
- Existing daemon reconcile loop: `cmd/switchboard/main.go`
