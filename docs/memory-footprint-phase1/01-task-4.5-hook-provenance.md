# #4.5 — Stop the hook overlay relabelling the whole Codex graph

**PR 1. Prereq: #3 (the invariant guard) is in Phase 0 and already landed.**
Expect ~5.7/s after this task, not 0.15/s — see README §"Measured facts".

---

## 1. The defect

`overlayCodexHookObservation` (`cmd/switchboard/agent_observation.go:582-605`)
takes a Codex hook's **single-node** root observation, inflates it back to the
app-server's full graph via `observationFromState`, and relabels the whole thing
unconditionally:

```go
func overlayCodexHookObservation(hook agentgraph.Observation, current *state.AgentGraph) agentgraph.Observation {
	if current == nil || current.RootID != hook.RootID || len(hook.Nodes) != 1 {   // :586
		return hook
	}
	overlay := observationFromState(agentgraph.ProviderCodex, current)              // :589
	overlay.Source = agentgraph.SourceHook                                          // :590  unguarded
	overlay.ObservedAt = hook.ObservedAt                                            // :591
	overlay.FreshUntil = hook.FreshUntil                                            // :592  imports the 600 s horizon
	overlay.Complete = false                                                        // :593  unguarded
	for i := range overlay.Nodes { /* root Runtime/Attention/Lifecycle/UpdatedAt */ }
	return overlay
}
```

The guard at `:586` does **not** look at `current.Source` at all.

### Two measured consequences

**Publish churn.** `codexHookActiveFreshness = 10 * time.Minute`
(`agent_observation.go:27`) vs `codex.DefaultFreshness = 15 * time.Second`
(`internal/provider/codex/observer.go:20`) — a 40x mismatch, and 40,320x for the
`7*24h` idle window. `fresh_until` oscillates between two values ten minutes
apart, ~13 round trips per 20 s on the busier Codex session (26 one-way; the
second session is 16 one-way / 8 round trips — the split is 62/38 and 83/17, not
50/50). No bucket width collapses that.

**History corruption (#83).** `:590`/`:593` stamp `source: hook` onto
app-server-derived children. Today's day-file: **33** Codex child `agent_state`
rows with `source: hook`; **31** duplicate a `codex_app_server` row on the
`to_*` identity, **2** are hook-only. On the fuller identity including `from_*`
it is **24 twinned / 9 orphaned**.

### Why the two producers alternate — it is not a race

`shouldApplyObservation` (`agent_observation.go:544-580`) has a Codex-specific
early branch: it returns at `:559` (strictly older) or `:565` (strictly newer)
**before** `sourceRank` (`:607-620`, app_server 4 > hook 3) is ever consulted at
`:568`. Both producers stamp their own `now`, so the rank table is reached only
on an exact-nanosecond tie. Last writer by wall clock wins. Then
`codex_hook_transitions.go:270` schedules an app-server re-observation after
every hook, closing the loop.

---

## 2. The core change

Invert the conditional so the **fallback** is the branch that mutates —
`observationFromState` (`:620-646`) already copies `Source`, `ObservedAt`,
`FreshUntil`, `Complete` from the graph, so the preserve branch does nothing:

```go
overlay := observationFromState(agentgraph.ProviderCodex, current)
// observationFromState already carries current.Source/Complete/FreshUntil.
if !codexAppServerGraphOwnsHookHorizon(current, hook.ObservedAt) {
    overlay.Source = agentgraph.SourceHook
    overlay.Complete = false
    overlay.FreshUntil = hook.FreshUntil
}
overlay.Diagnostic = codexComposedObservationDiagnostic   // §3.1
overlay.ObservedAt = hook.ObservedAt
// unchanged: the root Runtime/Attention/Lifecycle/UpdatedAt overlay loop

// codexAppServerGraphOwnsHookHorizon reports whether a hook landing at hookAt
// composes onto app-server evidence that is still authoritative. It is
// deliberately MORE than !codexAppServerRootUnavailable: that predicate is also
// false for a graph that is not app-server sourced at all.
func codexAppServerGraphOwnsHookHorizon(current *state.AgentGraph, hookAt time.Time) bool {
	return current.Source == agentgraph.SourceCodexAppServer &&
		current.Fresh(hookAt) &&
		!codexAppServerRootUnavailable(current)
}
```

Reuse the existing predicate, do not write a second one:

```go
// codex_hook_transitions.go:826
func codexAppServerRootUnavailable(graph *state.AgentGraph) bool {
	if graph == nil || graph.Source != agentgraph.SourceCodexAppServer { return false }
	for _, node := range graph.Nodes {
		if node.ID == graph.RootID { return codexRootStateUnavailable(node.Runtime, node.Attention) }
	}
	return false
}
```

It returns `false` **both** when the graph is not app-server sourced and when the
app-server knows the root — which is why the composed predicate needs all three
clauses.

### Do NOT add a `now` parameter

At both call sites `now` and `hook.ObservedAt` are the same value by
construction:

- `codex_hook_transitions.go:252` — `HandleHook` sets `now := req.ObservedAt`
  (falling back to `time.Now()`) at `agent_observation.go:740-743`, threads it to
  `codexHookObservation(..., now)` which stamps `ObservedAt: now`
  (`codex_hook_transitions.go:1176`). `applyCodexPendingAttention` moves
  `FreshUntil` but never `ObservedAt`.
- `codex_hook_transitions.go:994` — `now := time.Now()` at `:969`, passed to
  `codexHookApprovalObservation` which stamps `ObservedAt: now` (`:1188`).

Using `hook.ObservedAt` makes coherence a **theorem**: `current.Fresh(hookAt)`
⟹ `current.ObservedAt ≤ hookAt < current.FreshUntil` ⟹ the composed
`ObservedAt < FreshUntil`. With a separate `now` the two can diverge and produce
`ObservedAt ≥ FreshUntil`, which makes `Observation.Fresh` false for **every**
instant. Downstream that means `history.Project` returns nil at
`internal/history/agent_state.go:71` — **silent total history loss for that
frame** — and `agentgraph.Reduce` greys the chip (`reduce.go:11`). Choosing
`hook.ObservedAt` makes that state unrepresentable.

It also keeps the function pure and total, so five of the required tests are
direct table tests with no coordinator.

**Neither call site needs a signature edit.** Call site 1 needs one unrelated
edit (§3.2).

---

## 3. Two compensating changes — omitting either is a regression

`Source` is doing **two jobs** in this codebase: a provenance label that reaches
the wire and the day-files, and an internal routing bit meaning *"was this
composed from a hook?"*. #4.5 fixes the first and silently removes the second at
five call sites. This is the real shape of the task.

### 3.1 The child-hook overlay would destroy child overlays

`overlayCodexChildObservation` (`codex_hook_transitions.go:599-669`) is gated on
`observation.Source == agentgraph.SourceCodexAppServer` at `:600`. Today a hook
overlay carries `Source: hook` and is skipped. Under the fix it carries
`codex_app_server` and **runs** — on an observation whose child nodes were built
by `observationFromState` from the stored graph, which already has previous child
overlays baked in (`applyCodexChildOverlay` at `:535-556` writes
`node.UpdatedAt = overlay.at` into the graph before `ProjectAgentGraph`).

At `:618-619` it caches the overlaid node as `state.childProvider[id]`, then at
`:630` `childHookOwnsRuntime(providerNode, overlay.at)` sees
`hookAt.After(providerNode.UpdatedAt)` as **false** (equal, not after) →
`runtimeOwned = false`, `lifecycleOwned = false` → **the child overlay is
deleted** and a spurious `subagent_hook_provider_superseded` diagnostic is
recorded, on the first root hook after any `SubagentStart`.

The mechanism to fix it already exists: `observation.Diagnostic =
codexChildHookOverlayDiagnostic`, set at `:509` and `:716` on observations
synthesised from the current graph and consumed by the gate at `:601`.

**Set the marker unconditionally in the overlay path**, not just in the preserve
branch — every observation this function returns via `observationFromState` is
composed, and that removes the child gate's dependence on `Source` entirely.

Rename `codexChildHookOverlayDiagnostic` → `codexComposedObservationDiagnostic`
(four package-private sites: `:282` declaration, `:509`, `:601`, `:716`). The
value is never logged, persisted, or rendered — verified, nothing reads
`Observation.Diagnostic` except `:601` and `agentgraph.Normalize`'s error path.
**Ship the rename as a separate `refactor:` commit** so the fix diff stays tight.

### 3.2 The hook's own fallback deadline would shrink 40x

`codex_hook_transitions.go:256` remembers the **post-overlay** observation,
storing `state.rootFreshUntil = observation.FreshUntil`. Under the fix that
becomes the app-server's ≤15 s horizon instead of the hook's 10 min / 24 h / 7 d.
`overlayCodexHookRootObservation` (`:793-823`) uses `state.rootFreshUntil` to
bound how long an exact hook root status may compose over a *later* `notLoaded`
app-server snapshot. Shrinking it silently narrows that window and contradicts
the DoD's "the 24 h / 7 d attention and idle horizons are untouched".

Capture before the overlay in `handleCodexHookNow`:

```go
observation, mapped := codexHookObservation(rootID, req, ref.StartedAt, now)
var hookFallback agentgraph.Observation
if mapped {
	observation = applyCodexPendingAttention(observation, pendingAttention, now)
	// The hook's OWN bounded deadline is what may later compose over a notLoaded
	// app-server snapshot; the published observation may adopt a live
	// app-server's much shorter horizon instead.
	hookFallback = observation.Clone()
	if current, ok := sessionForKey(c.store.Snapshot(), ref.Key()); ok { ... }
}
if mapped {
	c.rememberCodexHookRootObservation(ref.Key(), hookFallback)
	...
}
```

**`Clone()`, not a shallow copy** — `applyCodexPendingAttention` later mutates
the shared `Nodes` backing array via `overlayCodexPendingObservation`
(`agent_observation.go:449`), and relying on call ordering to make a shallow copy
safe is too subtle. `rememberCodexHookRootObservation` reads only
`root.{Runtime,Attention,Lifecycle,UpdatedAt}`, `ObservedAt`, `FreshUntil` — all
identical between pre- and post-overlay forms in the fallback branch, so this is
a strict no-op on today's paths.

The alternative — letting the composition window follow the app-server horizon —
is defensible but is a *different* decision, out of scope here.

---

## 4. `Complete`: preserving it is provably safe

**Conclusion: preserving `Complete` cannot mark any node `not_found` that was not
already `not_found`.** The load-bearing detail is that we **carry over the
app-server graph's `Complete`, never hardcode `true`**.

Let `O` be the observation that produced the current `*state.AgentGraph`.

1. `state.ProjectAgentGraph` (`internal/state/agent_graph.go:101-125`) sets
   `graph.Nodes = Normalize(O).Nodes` and `graph.Complete = Normalize(O).Complete`.
   `history.Project` (`internal/history/agent_state.go:66-137`) normalises the
   **same** `O` and uses `Normalize(O).Nodes` as `present` and
   `Normalize(O).Complete` for the sweep gate at `:109`.
   ⟹ `(present, Complete)` for `O`'s projection is identical to
   `(nodes(current), current.Complete)`.
2. The composed observation's node set is `observationFromState(current).Nodes` =
   `nodes(current)`, and its `Complete` is `current.Complete`.
   ⟹ its sweep has **exactly the same** `(present, Complete)` pair as the sweep
   that already ran when `O` was projected.
3. Anything that sweep would mark was marked then, and is skipped by the
   `prior.node.Lifecycle == agentgraph.LifecycleNotFound` guard at `:112`.

The one way step 2 breaks is if `O`'s sweep never ran, because `Project` returns
early at `:71` when `!normalized.Fresh(now)`. Excluded by the preserve condition
itself: `current.Fresh(hookAt)` requires `hookAt < current.FreshUntil`, and
`hookAt` is at or after `O`'s apply instant, so `O` was fresh when projected.
(Assumes the clock does not step backwards between `O`'s apply and the hook; the
app-server's `ObservedAt` is stamped in-process at
`internal/provider/codex/observer.go:204`, so it is never future-dated.)

Two more paths checked:

- **`Forget` in between** (`agent_observation.go:306`, `:311`, `:508`, `:516`,
  `:827`) wipes `root.nodes`, so `present ⊇ tracked` and the sweep is vacuous.
  This is precisely the #83 regression scenario and where the fix pays off: after
  a `Forget` the composed hook re-emits initial rows for every node, and under
  the fix those carry `source: codex_app_server` with the app-server's own
  `UpdatedAt` — fully app-server-derived facts, honestly labelled.
- **A node tracked but absent from `current`** requires an observation with a
  *larger* node set than the current graph. Raw hooks are 1 node; composed hooks
  equal the current set; `expireCurrent` (`:610`) replays
  `observationFromState(current)`; the child republish paths at `:509`/`:716`
  likewise. `applyCodexChildOverlay`, `applyCodexPendingAttention` and
  `overlayCodexHookRootObservation` mutate existing nodes only. **There is no
  node-adding path.**

In practice preserving means preserving `true`
(`internal/provider/codex/protocol.go:887` emits `Complete: true`; degraded
observations at `observer.go:245/249/276` have zero nodes and are rejected at
`shouldApplyObservation:546`) — but the code must **copy, not assert**, because a
graph hydrated from `state.json` can carry anything.

---

## 5. Owner decision needed — `appServerSettled`

`codex_hook_transitions.go:955-960`. Under the fix a composed hook graph
satisfies `graph.Source == SourceCodexAppServer && !ObservedAt.Before(startedAt)
&& Runtime == active && Attention == none`, so an ordinary
`PostToolUse`/`UserPromptSubmit` during a 30 s approval grace can resolve a
pending approval as "resolved by app-server" and **suppress the red**.

Two reasons to accept it:

- **Already reachable today** by the same kind of evidence:
  `overlayCodexHookRootObservation` composes a *remembered hook's* runtime onto a
  `notLoaded` app-server snapshot and publishes it with
  `Source: codex_app_server`. Hook-derived runtime already satisfies
  `appServerSettled`. The fix widens the aperture; it does not open a new one.
- Semantically defensible: Codex blocks while awaiting approval, so an active
  hook frame past the boundary is real evidence it did not block.

Cost of being wrong: a suppressed red (false negative) where today's cost is a
spurious 24 h red (false positive).

**Recommendation: accept, and pin it with a test so it is deliberate.**

**Escalation if rejected:** `state.AgentGraph` already carries an unexported,
non-serialised field (`provider agentgraph.ProviderKind`,
`internal/state/agent_graph.go:25`, copied by `Clone` at `:92`). An analogous
`composed bool`, set by `ProjectAgentGraph` from
`observation.Diagnostic == codexComposedObservationDiagnostic`, would let
`:187`, `:958` and `:1044` keep exact current semantics while `Source` becomes a
pure provenance label. Architecturally correct, more surface than this DoD asks
for, and every behaviour it protects is either provably identical or already
reachable. **File as a follow-up**, referenced from the commit body.

### The related approval-timeout question (call site 2, `:994`)

Preserving hands the timeout red the app-server's ≤15 s horizon instead of 24 h.
With a live app-server this is already the effective behaviour — the next poll
reports `attention: none` with a later `ObservedAt` and `shouldApplyObservation:565`
lets it through, repainting green within 15 s either way. Unlike the
`request_user_input` latch, `overlayCodexPendingObservation` does **not** cover
`state.approvals`, so there is no re-latch. The only real divergence is
"app-server observer dies immediately after the red is published": 24 h of red
today vs grey after 15 s under the fix. Grey is more honest than a day-long red
inferred from a 30 s timeout, and **one rule at both call sites** matters — #83
requirement 3 forbids hook-labelled child rows without exception, and a flag
parameter would reintroduce the two-rules problem this task exists to remove.

---

## 6. Full audit of `Source`-sensitive sites

| site | effect | verdict |
|---|---|---|
| `codex_hook_transitions.go:600` `overlayCodexChildObservation` | would run on composed hooks and destroy child overlays | **must compensate** — §3.1 |
| `codex_hook_transitions.go:794` `overlayCodexHookRootObservation` | now entered on composed hooks | **proven no-op** — returns at `:812` unless the root is `codexRootStateUnavailable`, and every mapped hook root sets Runtime active or idle (`:1140-1166`), never unknown/notLoaded |
| `codex_hook_transitions.go:827` `codexAppServerRootUnavailable` | composed graph is app-server sourced with a hook root | still `false`, same reason — unchanged |
| `codex_hook_transitions.go:354/438/712` child-hook validation gates | composed graphs now qualify as "fresh app-server topology" | **improvement** — they read only topology and child nodes, verbatim app-server values; fewer needless requeues. The `childProvider` caching hazard at `:477` is pre-existing and not widened |
| `agent_observation.go:481` `authoritativeNativeName` (local) | composed hooks now qualify | **benign** — `nativeName` comes from `observationFromState(current)`, i.e. the app-server's own nickname; `nativeOverride` cannot fire spuriously because the value is identical to what the next poll supplies |
| `agent_observation.go:1036-1057` `authoritativeNativeName` (func) | composed graphs take the `:1044` branch instead of `:1051` | **behaviourally identical** — the only divergence is `("", true)` vs `("", false)` for an empty nickname, and the next app-server apply sets the baseline to `""` regardless. **The comment at `:1048-1051` describes the old overlay and must be rewritten** |
| `agent_observation.go:562/576` `shouldApplyObservation` | the "different source" escape no longer separates hook from app-server | unreachable: a newer app-server sample always carries a strictly later `FreshUntil` than a composed graph derived from an older one, so `candidateFresh` is true whenever `currentFresh` is |
| `agent_observation.go:607-620` `sourceRank` | composed hooks rank 4 not 3 | reached only on an exact-ns tie; both orderings still return `true` |
| `codex_hook_transitions.go:958` `appServerSettled` | hook evidence can now satisfy it | **owner decision** — §5 |
| `codex_hook_transitions.go:187` `currentAt` seeding | a persisted composed graph no longer seeds `state.latestAt` | narrow: only on the first hook of a daemon lifetime for a root whose hydrated graph was composed. `shouldApplyObservation` still fences by `ObservedAt` unless `hookOwnsTransition`. Accept and document |
| `internal/state/agent_graph.go:272` hydrate downgrade | a composed `user_input` red is no longer failed-unknown on restart | bounded: today's downgrade guards a 24 h horizon; the composed horizon is ≤15 s, so it is almost always already expired at hydration. Accept and document |
| `cmd/switchboard-ctl/timeline.go` | never reads `Source` (zero matches) | producer fix is the only lever |

### `hookOwnsTransition` composes cleanly

At `codex_hook_transitions.go:251` it is OR'd with
`codexAppServerRootUnavailable(current.AgentGraph)`, and a true value
short-circuits `shouldApplyObservation` entirely at `agent_observation.go:462`.
The two are **mutually exclusive on the same axis**:

| `codexAppServerRootUnavailable(current)` | `hookOwnsTransition` forced | preserve fires |
|---|---|---|
| true | yes | **no** — hook keeps `source: hook` and its own horizon (today's behaviour) |
| false, current is a fresh app-server graph | unchanged | **yes** |
| false, current is not app-server sourced | unchanged | no |

They cannot fight, because the same predicate decides both.

The one case worth naming: `hookOwnsTransition` true from the pending-input
reducer (a `request_user_input` onset) while the app-server graph is fresh. The
preserve branch would hand that red a ≤15 s horizon instead of 24 h — except it
does not, because `overlayCodexPendingObservation` (`:1112-1120`) runs afterwards
at `agent_observation.go:449` on **every** Codex observation regardless of
source, and `applyCodexPendingAttention:1102` re-stamps
`FreshUntil = now.Add(codexHookAttentionFreshness)`. The latch is re-established
on every frame while `state.pending` is non-empty. **No regression.**

---

## 7. Hook-on-hook — the untested gap

**Decision: keep today's behaviour, and pin it with a test.** When
`current.Source` is not `codex_app_server`, still inflate the topology (a 1-node
hook must never wipe a child graph — the function's original reason to exist) but
stamp `Source: hook`, `Complete: false`, and the hook's horizon. The composed
graph's provenance is genuinely mixed and the hook is the freshest authority in
it; claiming `codex_rollout` or `restored_last_known` would be a new lie in place
of the old one.

Reachability audit: `SourceCodexRollout` is never produced anywhere (it exists
only in the `sourceRank` table at `:613`); `SourceRestoredLastKnown` is
Claude-only (`internal/provider/claude/projection.go:107`). So for Codex,
`current.Source ∈ {codex_app_server, hook, ""}` — the non-app-server case is
hook-on-hook plus a hydrated graph.

**Benign chaining the fix creates:** hook #1 preserves and stores
`Source: codex_app_server` with the app-server's `FreshUntil`; hook #2 two
seconds later sees an app-server-sourced, still-fresh graph and preserves the
**same** `FreshUntil` verbatim. A burst of hooks shares one horizon and
`fresh_until` stops moving. **That is the churn mechanism**, and it deserves its
own test. Preserving never *extends* app-server evidence: `FreshUntil` is copied,
never recomputed.

---

## 8. The remote `nlessfun` session — the fix does not reach it, and should not

`cmd/switchboard/federation.go:60-80` wires `remotestate.Manager`, which pulls
**the remote daemon's own published snapshot** over SSH. That `agent_graph` is
produced by nlessfun's `switchboard`, not by this coordinator. So:

1. The fix is host-local; nothing changes there until the remote runs it.
2. Even then nothing would change: the capture shows `app_server 0 / hook 231`,
   so that host never produced a Codex app-server graph for that root and
   `current.Source == SourceCodexAppServer` is never true.

`memory-footprint-plan.md` §1.4.3 frames nlessfun as "the same defect in its
permanently degraded form". **That framing is wrong.** A 600 s hold with no
attachable app-server is the **designed** fallback
(`agent_observation.go:22-30`), which the DoD explicitly preserves, and it
contributes **zero** churn (0 transitions, 0 round trips). The real question is
why nlessfun has no attachable Codex app-server — a different investigation
deserving its own issue.

---

## 9. Correction to the plan's #83 evidence

"All 33 mislabelled rows reach the dashboard's `agent_timeline`" is too strong.
`cmd/switchboard-ctl/timeline.go` dedupes on `canonicalTimelineEdge{session,
thread, pid, at, from/to × runtime/attention/lifecycle}` — which excludes
`source` but **includes** the `from_*` axes. Twins agreeing on `from_*` are
collapsed by the reader itself. **At most 24 collapse; at least 9 survive** (7
disagreeing on `from_*` + the 2 hook-only rows). That makes the 24/9 grouping the
operative one, and the surviving 9 are exactly the rows that fabricate
transitions the user never made. Restate this in the commit body.

---

## 10. Tests

**Naming.** Use `TestShould…`, transliterated from the DoD. `cmd/switchboard` is
split 17 `TestShould*` / 87 other, but the split is chronological: the newest
file — `publish_suppression_invariant_test.go` (task #3, this branch) — is
entirely `TestShould*`. Follow its house style: a substantial file-level comment,
and failure messages that explain *what broke in production* rather than *which
value differs*.

**File.** One new `cmd/switchboard/codex_hook_provenance_test.go` with a header
citing §1.4.3 and #83.

**Clock.** `time.Date(2026, 8, 23, 16, 0, 0, 0, time.UTC)` plus arithmetic.

### The six required

1. `TestShouldNotChangeAgentGraphSourceWhenAHookArrivesForALiveAppServerGraph`
2. `TestShouldNotChangeAgentGraphCompleteWhenAHookArrivesForALiveAppServerGraph`
3. `TestShouldKeepTheAppServerFreshnessHorizonWhenAHookArrivesForALiveAppServerGraph`

Do these **twice** — once as a direct table over `overlayCodexHookObservation`
(pins the function contract), once end-to-end through `HandleHook` asserting the
**stored** `*state.AgentGraph`. **The end-to-end pass is load-bearing**: it is
the only thing proving the three post-overlay passes at
`agent_observation.go:448-450` do not undo the preservation. Seam:
`newStandardCodexHookTestCoordinator` (`codex_hook_transition_test.go:19`) +
`applyObservation` with `testCodexObservation` (root + one child, root
`idle`/`none`), then `sendCodexHook(… Event: "PostToolUse", ObservedAt: hookAt)`.

Test 3 must assert `FreshUntil.Equal(appServer.FreshUntil)` **and explicitly
not** `hookAt.Add(codexHookActiveFreshness)` — assert the negative by name, since
that constant is the bug.

4. `TestShouldStillApplyTheHookFallbackHorizonWhenThereIsNoAppServerGraph` —
   `overlayCodexHookObservation(hook, nil)` returns the hook untouched; plus the
   coordinator path with no prior graph. Table the four horizons against
   `codexHookObservation` so all three constants are pinned (extends
   `TestCodexHookFallbackFreshnessIsBoundedByState`,
   `agent_observation_test.go:239`).
5. `TestShouldStillOverlayTheRootNodeStatusWhenTheAppServerReportsTheRootUnavailable`
   — current root `not_loaded`/`none` → `Source: hook`, `Complete: false`,
   `FreshUntil == hookAt.Add(codexHookActiveFreshness)`, topology retained, root
   axes overlaid. Add a `runtime: unknown` row too, since
   `codexRootStateUnavailable` (`:838`) accepts both.
6. `TestShouldEmitNoChildAgentStateRowCarryingSourceHookAfterAForgetFollowedByAHookFrame`
   — the #83 regression. Seam: `newCodexChildHookCoordinator(t, sink)`
   (`codex_child_hook_test.go:15`, the **only** cmd-level ctor wired to a real
   `*history.Sink`) with `newRecordingSink` from
   `publish_suppression_invariant_test.go:66`.
   1. `applyCodexChildTopology` with root + two children at `base`
      (`codexChildTopology` gives `Complete: true`, `FreshUntil: +1h`, root
      `idle`/`none` — exactly the preserve case).
   2. `coordinator.history.Forget(agentgraph.ProviderCodex, "root")` — direct and
      deterministic. Comment naming the production triggers
      (`agent_observation.go:508`, `:516`, `:306`, `:311`, `:827`) so nobody
      "simplifies" it away.
   3. `sendCodexHook(…, rpc.Request{Event: "UserPromptSubmit", …, ObservedAt: base.Add(time.Second)})`.
      **Avoid `SessionStart`** — that ctor does not shorten `codexStartSettle`, so
      it would defer through a timer.
   4. `flush()`, `readEvents`, `eventsOfType(…, history.EventAgentState)`.
   5. Assert **zero** rows with `ParentThreadID != "" && Source == agentgraph.SourceHook`.
   6. **Positive control, non-negotiable:** assert the hook emitted ≥2 child rows
      and that all carry `SourceCodexAppServer`. Without it the test passes
      trivially if child projection breaks entirely.

### Five more the design demands

7. `TestShouldKeepTheHookFallbackHorizonWhenTheCurrentGraphIsItselfHookSourced` —
   §7, so a future generalisation is deliberate.
8. `TestShouldNotMoveFreshUntilAcrossABurstOfHooksOnOneAppServerHorizon` — one
   app-server frame, then N hooks at +1s…+Ns; assert `FreshUntil` is identical
   across all N. **This is the churn mechanism and the link to #10's DoD.**
9. `TestShouldStillComposeTheHookRootOntoALaterNotLoadedSnapshotForTheHooksOwnDeadline`
   — regression for §3.2. Fresh **known** app-server graph → hook → app-server
   goes `not_loaded`; assert the root status still composes and
   `FreshUntil == hookAt.Add(codexHookActiveFreshness)`. The existing
   `TestCodexNotLoadedAppServerSnapshotRetainsBoundedHookRootStatusAndTopology`
   (`agent_observation_test.go:357`) cannot catch this — its graph is already
   `not_loaded` at hook time.
10. `TestShouldNotSupersedeAChildHookOverlayWhenARootHookComposes` — regression
    for §3.1. `SubagentStart` → `reconcileCodexChildHooks` → root hook; assert
    the child's `Runtime`/`Lifecycle` survive and
    `diagnosticCount(coordinator, "subagent_hook_provider_superseded") == 0`.
    Without the `Diagnostic` marker this goes red, which is the point.
11. `TestShouldMarkNoNodeNotFoundWhenAHookComposesOnALiveAppServerGraph` — §4 made
    executable. App-server (root + 2 children, `Complete: true`) → hook; assert
    zero `agent_state` rows with `ToLifecycle == LifecycleNotFound`. Companion:
    app-server *drops* a child, then a hook; assert exactly **one** `not_found`
    row carrying `source: codex_app_server`, emitted by the app-server frame and
    not duplicated by the hook.

### Existing tests — exactly one assertion changes

`TestNewerCodexHookIsImmediateAndLaterAppServerCorrectsIt`
(`agent_observation_test.go:318`, assertion at `:340`) asserts
`graph.Source == SourceHook` after a `PostToolUse` hook onto a fresh app-server
graph whose root is `idle`/`approval` — the preserve case. **Keep the test name**
(the hook is still immediate; `Summary.Status` still flips to `working`); change
to `SourceCodexAppServer` and **strengthen** it to pin `Complete == true` and
`FreshUntil.Equal(observation.FreshUntil)`, with a comment naming §1.4.3.

Audited and **not** affected (verify by running, don't take this on faith):

- `agent_observation_test.go:178`, `:220`, `:313` — no prior graph at hook time,
  so `current == nil` and the function returns the hook unchanged.
- `agent_observation_test.go:381` — root is `not_loaded`, so
  `codexAppServerRootUnavailable` is true and preserve does not fire. Its
  `FreshUntil == hookAt.Add(codexHookActiveFreshness)` assertion at `:402` stays
  green and becomes the guard for the fallback branch.
- `agent_observation_adversarial_test.go:239` — fake provider blocked, no graph.
- `codex_display_name_test.go:556` — hand-builds `Source: hook` and calls
  `applyObservationWithHookOwnership` directly, bypassing the overlay.
- `publish_suppression_invariant_test.go` — Claude-only, zero Codex surface.
  Stays green trivially. Its role here is exactly as the prereq intends.

---

## 11. Order of work

1. **`refactor(switchboard): name the composed-observation marker for what it is`**
   — rename `codexChildHookOverlayDiagnostic` →
   `codexComposedObservationDiagnostic` at `codex_hook_transitions.go:282`,
   `:509`, `:601`, `:716`; update the doc at `:282` and the gate comment at `:600`
   to say "synthesised from the current graph, not provider evidence". Pure
   rename. `go build ./... && go test ./cmd/switchboard/` green before proceeding.
2. **`fix(switchboard): keep app-server provenance and horizon when a Codex hook composes`**
   — one commit, tests included:
   - `agent_observation.go`: the preserve branch, `codexAppServerGraphOwnsHookHorizon`,
     rewritten doc comment; rewrite the stale comment at `:1048-1051`.
   - `codex_hook_transitions.go`: the `hookFallback := observation.Clone()` capture.
   - The updated assertion in `agent_observation_test.go:340`.
   - New `cmd/switchboard/codex_hook_provenance_test.go` with tests 1-11.
3. **Verify**: `go test ./cmd/switchboard/ ./internal/state/ ./internal/history/
   ./internal/agentgraph/`, then `go test ./...` accepting the pre-existing
   `internal/conformance` failure. Then the DoD's Verify step — re-run the
   ablation on a fresh capture.
4. **File the follow-ups**: the `composed bool` separation (§5); a note on #83
   that requirement 3's producer is fixed while requirement 5's `Forget`-driven
   re-emission is not; the nlessfun note (§8).

### Verify step — what to expect

Re-running the ablation should show `source`, `complete` and `fresh_until` no
longer oscillating. **Expect ~5.7/s, not 0.15/s** — see README §"Measured facts".
This task removes a floor no bucket can collapse; #6 then does the rest.
