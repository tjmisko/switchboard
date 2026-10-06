# Phase 2 — per-root decision record and `switchboard-ctl explain` (#95)

[Master](README.md). Depends on Phase 1, so Pi decisions are explained from the
start. **Specification:**
[issue #95](https://github.com/tjmisko/switchboard/issues/95). Read its Scope
section first. This plan maps the issue onto the code.

The issue says to implement this before arbitration changes, so existing
behaviour can be inspected and characterized. This phase changes **no**
status outcome.

## Acceptance criteria (from #95, verbatim)

- An unresolved input request with a conflicting terminal working reading
  explains both the selected permission status and the rejected candidate.
- Missing binding, expired observation, and unsupported coverage have distinct
  reasons.
- Process replacement cannot expose the previous root's decision as current.
- Text and JSON output agree, remain bounded, and contain no user content.

## Where decisions happen today

Today three layers decide a root's status, and none of them records a
per-root reason.

1. **Graph admission.** `applyObservationWithRule`
   (`cmd/switchboard/agent_observation.go` ~:493) and `shouldApplyObservation`
   (~:655).
   - The source rank is app-server/transcript 4 > hook 3 > codex rollout 2 >
     restored 1 (`sourceRank` ~:736).
   - `hookOwnsTransition` skips that gate. Codex hook reduction sets it
     (`codex_hook_transitions.go` ~:283-319, ~:581, ~:795, ~:1148), and so
     does the 90 s rollout correction (`codex_transcript_poll.go` ~:74).
   - Expiry happens in `expireCurrent` (~:751).
   - Silent rejections are the early returns at ~:503-515.
2. **Projection.** `Session.projectStatus` (`internal/state/herdr.go` ~:253) and
   `herdrAuthority` (~:78-87).
   - herdr overrides the graph, except under the Codex attention exception
     (~:82-84), which reads `time.Now()`.
   - After Phase 1, `piStatusAuthority` is a third case.
3. **Publication.** `ProjectPublished` (`internal/state/usage_limit.go`) puts
   `limited` over everything except permission.

## What exists to build on

- **Journal lines.** `statustune.Decision.Log()`
  (`internal/statustune/statustune.go` ~:179) logs Claude graph edges and red
  holds, herdr edges, and (after Phase 1) Pi edges. Codex edges get no rule
  (`applyObservationWithHookOwnership` passes "", ~:480).
- **Rule codes.** `internal/statustune/knobs.go`.
- **History.** `switchboard-ctl diagnose` reads these back from journalctl.
  `agentCoordinator.Diagnostics()` (~:1319) holds **global** per-provider
  counters only.
- **Candidate inputs** already carry provenance:
  - `state.AgentGraph{Source, ObservedAt, FreshUntil, Complete}`
  - `HerdrInfo{Live, Status, Agent}`
  - `UsageLimit`
  - the Codex hook root state (`rootFreshUntil`, `transcriptStoppedAt`,
    `pending`, `approvals`)

## Work units

1. **Record type.** Add `internal/statusexplain` (or a package beside
   `statustune`) with these pure types and no I/O:
   - `Decision{Root: {PID, StartedAt, Provider, SessionID}, Status, Source,
     EvidenceKind, Reason, ObservedAt, FreshUntil, DecidedAt, Rejected:
     []Candidate (bounded, e.g. 8)}`
   - `Candidate{Source, EvidenceKind, Status, ObservedAt, FreshUntil,
     RejectReason}`

   Reason codes are a closed enum and include these distinct values:
   `binding_missing`, `observation_expired`, `coverage_unsupported`,
   `source_outranked`, `stale_vs_fresh`, `hook_owned`, `herdr_override`,
   `herdr_yield_attention`, `pi_hook_authority`, `usage_limit_overlay`.
2. **Recording.** Keep it in memory, keyed by `RootKey` plus provider session
   id, inside the coordinator. Write it:
   - at the admission early-returns (as rejected candidates);
   - inside the `store.Apply` that commits a graph (~:536);
   - in `applyHerdrPane`;
   - in `piStatusAuthority`;
   - in `expireCurrent`.

   Unchanged decisions refresh `DecidedAt` only. Clear a root's record in
   `forgetCodexHookState`/`refreshTrackedRoots` when it goes. Replacing the
   lifetime (a new `StartedAt`) must start a new record, which covers the
   third criterion.
3. **Projection reasons.** `projectStatus`/`herdrAuthority` return their reason
   alongside the status, a small signature change. Its callers file the reason
   into the record.
4. **Usage limit.** Computed on demand at explain time, from the same rule as
   `projectUsageLimit`. Nothing is stored for it.
5. **RPC and CLI.**
   - The RPC is `cmd: "explain"` with a pid, optionally a hostname, local only.
   - The CLI is `switchboard-ctl explain --pid <pid> [--json]`.
   - The text and JSON forms are rendered from one struct, bounded in size,
     and carry ids, enums and times only.
   - Add a one-line pointer from `diagnose` (history) to `explain` (now).
6. **Characterization tests.** Pin today's precedence before #96 rewrites it.
   Name each test for the rule it pins. These tests are #96's regression
   suite.

## Tests

The four criteria above each become a test. In addition:

- should explain an unchanged decision with its current source and deadline;
- should list a source-outranked candidate as rejected;
- should explain herdr overriding a graph, and herdr yielding to Codex
  attention;
- should explain a Pi hook decision over a herdr reading;
- should report `limited` with the underlying decision beneath it;
- should bound the rejected list when many candidates arrive;
- should start a new record when the process is replaced.

## Out of scope

Changing any status outcome. That is Phase 3.

## As built

Where the implementation departs from the work units above:

- **Two halves, one record.** Projection runs outside the coordinator (the
  herdr watcher, the reconciler, Pi hooks), so its half of the record lives
  on the session as an unexported, in-memory `statusexplain.Projection`,
  stamped with the process lifetime. `projectStatus`, `projectPiStatus` and
  `SetHerdr`'s herdr-only path write it. Graph admission's half (rejected
  candidates, hook ownership, why observe stopped short) lives in the
  coordinator, keyed by `RootKey` and bound to the provider session id.
  `agentCoordinator.Explain` merges the two.
- **Expiry** is recorded by the projection that `expireCurrent` already
  triggers, rather than at `expireCurrent` itself.
- **Reasons added** to the closed enum: `graph_authority`, `herdr_only`,
  `herdr_fallback`, `observation_pending`, `older_than_current` and
  `unrecorded`. The last is what explain says when a path that records
  nothing (the legacy hook FSM, hydration) has moved the status since the
  last recorded decision, rather than attribute the status to evidence that
  did not decide it.
- **`SetAgentGraph` takes `now`**, to date the record. `herdrAuthority` keeps
  its own `time.Now()` for the Codex attention exception, unchanged; #96
  removes it.
- **The Pi reconcile tick** also applies a re-projection that moves which
  source decides without changing the status, so explain follows a hook
  lease lapsing onto a herdr reading of the same colour. It publishes
  nothing.
