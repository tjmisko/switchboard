# Phase 3 — one pure status resolver (#96)

[Master](README.md). Depends on Phase 2: its decision record is the resolver's
output, and its characterization tests are the regression suite.

**Specification:** [issue #96](https://github.com/tjmisko/switchboard/issues/96).
Read its Scope section; this plan maps it onto the code.

## Acceptance criteria (from #96, verbatim)

- Fresh unresolved provider attention survives terminal working or idle
  readings until evidence authorized to resolve that request arrives.
- An idle root with positively live descendants remains delegating.
- Unknown/unavailable candidates contribute no new status; previous evidence
  is retained only through its existing deadline.
- Expired or identity-mismatched candidates cannot select a status.
- Restored presentation has no renewed live authority merely because it was
  loaded.
- Fake-clock tests cover conflicting candidates, source withdrawal, expiry,
  request resolution, and background work.
- Existing graph reduction, legacy status fields, navigation behavior, and
  child ownership remain compatible.

Added here: a fresh Pi dialog (hook evidence) holds red against a herdr
`working` reading.

## Design

`Resolve(candidates []Candidate, prior Decision, now time.Time) Decision`:
pure, outside `agentgraph.Reduce`, which stays neutral. Candidate kinds and
what each may do:

| Kind | Examples | Establishes | May resolve open attention |
|---|---|---|---|
| exact lifecycle event | Claude, Codex and Pi hooks | working, idle, attention | yes, for its own writer |
| provider snapshot | Codex app-server, Claude transcript graph | full graph | yes |
| correlated transcript evidence | Codex rollout tail, Pi session tail | idle or working | no |
| coarse terminal reading | herdr | working, idle, blocked | no; valid only when herdr's agent matches the tracked agent and pane |
| partial hook edge | child hooks before topology is known | child overlay | no |
| restored last-known | state.json | presentation until its old deadline | no |

Capabilities come from the kind and its identity match, never from the source
name alone (issue Scope).

## Code it replaces

- `projectStatus` and `herdrAuthority`, including the Codex attention
  exception and its `time.Now()` (`internal/state/herdr.go` ~:78-87, ~:253).
- `piStatusAuthority` (Phase 1).
- The `sourceRank` gate in `shouldApplyObservation`
  (`cmd/switchboard/agent_observation.go` ~:655-736). The Codex `ObservedAt`
  ordering survives inside the resolver as a per-source rule.
- The `hookOwnsTransition` bypasses (`codex_hook_transitions.go`,
  `codex_transcript_poll.go`). These become candidates with explicit kinds;
  the transcript-poll idle correction gains its own evidence kind instead of
  borrowing the graph's `Source`.

## Clock

The other wall-clock reads on the path become parameters:

- `agent_observation.go`: `reconcileCodexChildHooks(time.Now())` ~:399,
  `restoreClaude` ~:452/458, and the hook fallback when `ObservedAt` is zero
  ~:950;
- `codex_hook_transitions.go` ~:1101;
- `SetAgentGraph`, which gains a `now`.

Most existing tests build on real `time.Now()`, so expect fixture churn. Move
them to a fixed base time as they are touched.

## Work units

1. Candidate builders for each source, which can be tested alone.
2. The resolver and its kind table, with fake-clock tests for every criterion.
3. Switch projection to the resolver behind the Phase 2 characterization
   tests. Each changed expectation is listed in the PR as an intentional
   behaviour change.
4. Remove the old precedence code and the remaining `time.Now()` reads.

## As built: 3A (work units 1 and 2)

Phase 3 ships as two PRs: 3A adds `internal/statusresolve`, pure and not
called from production; 3B (work units 3 and 4) switches projection to it
and removes the old precedence code. Where 3A departs from the design above:

- **Signature.** `Resolve(target, candidates, prior, now)`. The tracked
  agent is an explicit `Target` (the `statusexplain.Root` plus its herdr
  pane): identity is matched in the resolver, and a first call has no prior
  to carry it. A `prior` about another root (rotation, new lifetime) counts
  for nothing.
- **Builders.** One per source (`ClaudeGraph`, `ClaudeHook`,
  `CodexAppServer`, `CodexHook`, `CodexChildHooks`, `CodexRolloutTail`,
  `PiHook`, `PiSessionTail`, `Herdr`, `RestoredLastKnown`). Graph builders
  reduce at the observation's own time and need no clock; only `Herdr`
  takes `now` (to date a reading and to expire a withdrawn one).
- **Complete lifecycle.** An exact event outranks a terminal reading only
  when its writer reports both edges of every state it asserts (Pi's
  extension). Claude's and Codex's hooks miss interrupts, so herdr keeps
  outranking them, as today. This is a builder-declared capability, not a
  source name.
- **Descendants.** Claude's and Codex's hooks land on composed graphs, so an
  exact event reports the working descendants it carries, as a snapshot and
  a partial edge do. A partial edge never selects the root's status and, in
  3A, establishes no attention.
- **Prior evidence** holds in two cases only: open provider attention (an
  event's or snapshot's) until its deadline or an authorized resolution, and
  any prior decision with a finite deadline when nothing current selects. A
  terminal reading has no deadline and is never held.
- **Reasons.** Eight codes added to the closed enum, and two evidence kinds
  (`hook_edge`, `transcript`). `EvidenceKindOf` is unchanged until 3B.

The parity tests (`internal/state/resolver_parity_test.go`,
`cmd/switchboard/resolver_parity_test.go`) run each Phase 2
characterization situation through today's code and the resolver; every
difference names the #96 criterion that causes it.

## As built: 3B (work units 3 and 4)

Projection now asks the resolver. Every path that can move a published status
(a provider graph landing, a herdr reading, a Pi hook or session-file read, a
lease lapsing on the reconcile tick) builds candidates from the session's
evidence and calls `statusresolve.ResolveIndex` with the clock it was given;
the decision is the published status and the record explain reads.

- **Evidence lives on the session.** `state.Session` keeps the latest graph
  of each evidence kind for the bound conversation (snapshot, hook event,
  partial child edge, transcript tail, restored), in memory and
  copy-on-write. The coordinator was the suggested home, but herdr readings
  land through `SetHerdr` without passing the coordinator; the session is
  where both meet under the store lock. Discovery's re-announcement of a root
  carries the evidence across (`InheritStatusEvidence` in `admitRoot`).
- **Landing is storage.** `LandAgentGraph(graph, GraphLanding{Kind, ...},
  now)` keeps a graph unless it is older than the graph its own kind holds
  (`older_than_current`); a graph never displaces another kind's. The caller
  states the kind: observe ticks land snapshots (a held observation keeps its
  own provenance), hooks land events, child-hook overlays land partial edges
  that amend the published graph, the transcript poll lands a transcript tail
  under `codex_rollout`, restores land restored evidence.
- **The published graph** (`AgentGraph`, children, names, usage) is the
  landed graph the provider's own evidence decides by: the resolver over the
  landed graphs without herdr. Graph reduction, legacy fields, navigation and
  child ownership are unchanged.
- **Removed:** `projectStatus`, `herdrAuthority` (and its Codex attention
  exception and `time.Now()`), `piStatusAuthority` and the record helpers,
  `admitObservation`/`shouldApplyObservation`/`sourceRank`, the
  `hookOwnsTransition` bypasses and the transition-ownership returns of the
  Codex pending reducers, `statusexplain.Projection`. `hook_owned` and the
  herdr/Pi reason codes stay in the closed set for older records only.
- **Codex event-time order across kinds.** A fresh landing supersedes older
  event-time evidence of the other kinds (`Candidate.Superseded`): it may
  still hold its own open request, but never decides again, so a SessionStart
  hook does not come back when a newer app-server sample lapses. Superseded
  evidence reports no descendants either; the Codex rollout tail's idle
  correction, built from the published graph with its children kept, reports
  that graph's working descendants instead, so an idle root with running
  subagents stays delegating when the correction lands.
- **Hook-latched attention.** A Codex sample whose root request was put
  there by the hooks' pending-input or approval latch carries it as the
  hook's (`statusresolve.HookLatched`), so the hook that answers it resolves
  it at once. The app-server's own request yields only to a newer snapshot or
  its deadline (coordinator decision 1).
- **herdr identity** matches herdr's stable terminal id when both sides carry
  one, the pane id otherwise (decision 3). Partial child edges report working
  descendants only (decision 2).
- **Clock.** `reconcileCodexChildHooks`, `restoreClaude`, the hook fallback
  for a request with no `ObservedAt`, the Pi hook fallback and the Codex
  approval timer take the coordinator's clock (`agentCoordinator.clock`,
  wall clock when nil); the tick, timer and RPC entry points are the only
  reads.

`unrecorded` remains only where a status is written beside the resolver: the
legacy RPC hook path (no coordinator installed, which the daemon never runs),
a Pi block restored from state.json whose persisted status differs from its
restored graph before anything re-resolves it, the usage-limit hydration
rewrite, and a herdr-only agent whose herdr reading names a different agent
(its herdr graph still publishes).

## Out of scope

- New silence thresholds (#51).
- Observer outcome types (Phase 5).
