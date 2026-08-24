# No-wrapper Codex child lifecycle

> Decision date: 2026-08-24. Issue #83's discovery phase is complete. The
> production design uses ordinary `codex`, app-server structural topology, and
> exact standard child-lifecycle hooks. Live rollout evidence remains the gate
> for closing #83.

## Decision

Switchboard does not replace or wrap the ordinary `codex` command. Child state
is assembled from two independently scoped authorities:

- the disposable standalone app-server owns child identity, immediate
  parentage, names, graph completeness, and any concrete state it actually
  supplies;
- trusted `SubagentStart` and `SubagentStop` hooks supply runtime/lifecycle
  edges only after the hook's exact `agent_id` matches an existing non-root node
  in a fresh app-server graph for the same root process/session lifetime.

Structural presence is not positive liveness. A child whose runtime is
`unknown` or `not_loaded`, whose lifecycle is `unknown`, and which has no
actionable attention remains visible but contributes zero to `live_children`.
Missing hooks, stale topology, transport loss, and ambiguous evidence all
degrade to unknown. None authorizes a guessed child, parent, completion, or
error.

The OpenAI hooks contract supports this split: subagent hooks carry the parent
`session_id`, the exact child `agent_id`, and turn metadata. A child may continue
after a `SubagentStop`, so completion closes only the current activity interval;
a later `SubagentStart` reopens the same child. See the
[official OpenAI hooks documentation](https://learn.chatgpt.com/docs/hooks).

## Rejected designs and attribution invariants

The following remain rejected dependencies:

- a `codex` wrapper, shell alias, replacement binary, or mandatory launcher;
- a launcher token, cwd/title/time/launch-order correlation, or manual slot;
- a private per-TUI app-server endpoint;
- a required shared app-server daemon or direct dashboard scraping;
- inferring liveness from a descendant's mere presence, nickname, or retained
  transcript item.

Every accepted child edge satisfies all of these invariants:

1. The OS root is already identified by `(pid, started_at)`.
2. The hook's parent `session_id` equals that root lifetime's current exact
   provider root id. A retired or wrong-root id cannot rotate child state.
3. A fresh `source=codex_app_server` graph contains the exact hook `agent_id` as
   a non-root node.
4. The app-server node's existing immediate `parent_id` is retained. A hook
   cannot create a node or flatten a nested child onto the root.
5. Child edges are ordered only against edges for that child, using hook time
   and receive sequence. A newer root or sibling hook cannot make one stale.
6. Missing, stale, duplicated, or unmatched evidence can remove confidence but
   can never manufacture attribution.

## Evidence from August 23–24

Three possible lifecycle channels were observed separately:

1. Standalone app-server notifications produced no child lifecycle evidence.
2. `thread/read` and `thread/turns/list` exposed completed `wait` collaboration
   items, but those items contained neither receiver ids nor usable agent-state
   values for the observed runs.
3. Child snapshots consistently returned runtime `not_loaded` and lifecycle
   `unknown`, while `SubagentStart`/`SubagentStop` supplied exact child ids.

Across approximately 800 snapshot cycles, the August 23 run observed nine
unique child ids and the August 24 run observed three. The cumulative stop-hook
counter reached at least 19. Every recorded child lifecycle-hook outcome joined
an exact non-root graph node; there were no missing-id, graph-absent, unmatched,
or ambiguous outcomes in the captured live runs.

Those results prove an exact join, not that hooks are infallible. Production
therefore queues briefly when topology is missing and drops unmatched evidence
after a finite deadline instead of weakening the join.

### False-live defect

The earlier neutral reducer counted every non-terminal descendant as live.
Because `lifecycle=unknown` is non-terminal, retained app-server nodes with
`runtime=not_loaded` and `lifecycle=unknown` incorrectly increased
`live_children` and could paint an idle root as delegating. The corrected
provider-neutral predicate requires affirmative evidence: pending/running
lifecycle, active/idle runtime, or actionable approval/user-input attention.
Terminal lifecycle still wins over stale positive fields.

## Source authority

| Channel | Owns | Does not own | Failure behavior |
|---|---|---|---|
| OS discovery | Root PID/process lifetime, tty/cwd, navigation target, provider kind | Provider thread identity or child graph | Root remains navigable with unknown enrichment |
| Root lifecycle hooks | Exact root `session_id`; bounded root runtime/attention and the standard-CLI input latch | Child topology, parentage, names, complete snapshots | Missing hook leaves discovery/topology intact but root status may be unknown |
| App-server topology | Root/child ids, immediate parents, names/roles, completeness, and concrete provider states | A live child interval when returned state is unknown/not-loaded | Stale/partial transport preserves uncertainty; complete omission yields `not_found` in history |
| Root hook overlay | Unavailable root runtime/attention until its original deadline | Structural fields or indefinite freshness | Concrete provider state or expiry wins |
| Child lifecycle overlay | Exact matched child runtime/lifecycle edge | Attention, interruption/error synthesis, identity, names, or parentage | Unmatched/expired evidence is dropped; child returns to provider state |

`agent_graph.source` and canonical `agent_state.source` remain
`codex_app_server` after child fusion because app-server is still the graph and
identity authority. Hook-overlay provenance is available only through bounded,
content-free diagnostic counters. No new state or history wire field is added.

## Child state machine

| Evidence | Result |
|---|---|
| Exact matched `SubagentStart` | `runtime=active`, `lifecycle=running`, clear `completed_at` |
| Exact matched `SubagentStop` | `runtime=idle`, `lifecycle=completed`, set `completed_at` |
| Later start after completion | Reopen the same child as active/running |
| Missing or stale topology | Queue temporarily; never create a child |
| Queue expires without a match | Drop and diagnose |
| Newer concrete app-server state | App-server wins |
| Complete app-server omission | Existing projector emits `not_found` |
| Partial observation or transport loss | Preserve uncertainty; never infer completion |

`SubagentStop` is terminal only for the current interval. It is intentionally
reversible because Codex can continue the child later.

## Serialized fusion and retention

The coordinator owns a queue and last-applied child overlay scoped to
`(pid, started_at, root session_id)`. Root rotation, process death, and
coordinator shutdown clear both. The queue is limited to 256 edges; unmatched
edges and unclosed running overlays expire after ten minutes. Completed
overlays remain for the exact root lifetime until a later start, a newer
concrete provider transition, complete omission, root rotation, or process
death.

Hook delivery only enqueues and requests an immediate resnapshot. The
coordinator's serialized reconcile path then:

1. accepts only a fresh `codex_app_server` graph for validation;
2. requires an exact non-root node match;
3. orders matched edges by hook timestamp and receive sequence;
4. retains the app-server node and its immediate parent;
5. applies runtime/lifecycle through the canonical observation, graph, history,
   and timeline projection seam.

Hooks fill an axis only when the provider axis is unavailable or the exact hook
edge is newer than the provider transition. A newer concrete provider value
retires that axis of the overlay. Running-overlay expiry restores the cached
provider value—normally `not_loaded`/`unknown`—without inventing a completion.
Duplicate and stale edges are idempotent and emit no duplicate canonical
history.

Diagnostics use finite categories for queued, applied, expired,
provider-superseded, missing-id, unmatched-topology, and queue-full outcomes.
They never contain child ids, names, prompts, tool input, messages, cwd, or
transcript paths.

## Operational requirement

Codex discovery, navigation, exact root binding from other supported evidence,
and structural topology continue without child hooks. Trusted
`SubagentStart`/`SubagentStop` handlers are required only for live child state on
the currently observed standard-CLI path. Without them, unknown/not-loaded
children stay visible and correctly count as zero live children.

The live acceptance probe is
[`scripts/run-codex-child-lifecycle-probe`](../scripts/run-codex-child-lifecycle-probe).
It must demonstrate `live_children` `0 -> 1 -> 0`, durable
`unknown/not_loaded -> active/running -> idle/completed` edges, immediate nested
parentage, and a non-empty canonical timeline. A follow-up start must create a
new running edge if Codex emits it. If Codex supplies no reactivation hook, that
is an upstream limitation to record—not permission to add a heuristic.
