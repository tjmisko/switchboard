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

## Out of scope

- New silence thresholds (#51).
- Observer outcome types (Phase 5).
