# The `AskUserQuestion` prompt model — reconciled gameplan

> Three independent studies of the same defect, from three vantages: transcript
> **resolution semantics**, the hook **protocol**, and the **state model** of
> prompt ownership. This document reconciles them adversarially — where they
> agree the finding is load-bearing, where they conflict one of them is wrong,
> and in two places all three are wrong together.
>
> Diagnosis this builds on: [attention-latency-report.md](attention-latency-report.md).
> Navigation half of that report is already shipped:
> [attention-ring-plan.md](attention-ring-plan.md).

---

## 0. The headline

The latency we set out to fix (median 17 s of stale red on `AskUserQuestion`) is
real, and its mechanism is confirmed by all three studies at the line. But it is
**no longer the most expensive thing found.** Two *missed REDs* — the error class
the whole colour model is built to avoid — fell out of the study:

| # | Defect | Provider | Class | Cost |
|---|---|---|---|---|
| **M1** | A Codex question-shaped prompt is published red but enters no pending record, so the next hook edge from *any* writer erases it | Codex | **missed RED** | silent, minutes–hours |
| **M2** | `rs.pending` holds one prompt per writer, so a second parallel prompt overwrites the first; clearing the survivor clears the chip while the other still blocks | Claude | **missed RED** | silent, minutes–hours |
| L1 | The measured 17 s stale red on `AskUserQuestion` | Claude | stale RED | loud, seconds |

M1 and M2 outrank L1 under §4 of
[status-color-state-model.md](status-color-state-model.md). The plan is ordered
accordingly, which is *not* the order any of the three reports proposed.

---

## 1. Unanimous findings

Where all three studies independently agree, treat it as settled.

- **D2/D3 confirmed verbatim.** `promptMatches` (`internal/provider/claude/observer.go:567`)
  rejects on hash inequality; `AskUserQuestion`'s `PostToolUse` input is a strict
  superset of its `PermissionRequest` input, so the fast path is never taken and
  resolution falls to `resolutionKindOf` (`internal/transcript/transcript.go:495`),
  which accepts only an assistant message or an interrupt — i.e. model latency.
- **R4's plumbing claim is wrong, and all three caught it.** `tool_use_id` is
  already parsed for every agent (`cmd/switchboard-ctl/main.go:614,639`) and
  already on the wire (`internal/rpc/rpc.go:88`). It dies at one struct literal:
  `HookSignal` (`observer.go:42`) has no such field, so
  `cmd/switchboard/agent_observation.go:755` cannot pass it. **Three lines, not a
  pipeline.** The report overprices this by an order of magnitude.
- **R4's hook-time transcript read must not be built.** It breaks `ApplyHook`'s
  documented no-I/O contract (`observer.go:243`) and, decisively, it is ~5 s too
  early: the pending `tool_use` lands on disk **+4.56 / +4.88 / +5.02 s after the
  hook** (`subagent-permission-plan.md:576-640`). It would look implemented and
  latch nothing.
- **An id-matched `tool_result` is the right second signal (R5).** It lands
  20–101 ms after the user hits enter, and it is *strictly narrower* than the
  bare-`tool_result` rule already rejected at `transcript.go:461` — a sibling's
  result carries a different id, a teammate's is in a different file.
- **The fanout floor (`observer.go:318`) may be lifted for id-matched clears
  only**, and all three flag the same precondition: assert `tool_use_id`
  uniqueness across parent and subagent transcripts *before* lifting it.
- **Rejected unanimously:** subset-stable hashing; shortening `PermissionDecayTTL`;
  R6's adaptive tick as a substitute for the id path; porting a transcript
  backstop to Codex.

---

## 2. The conflicts, adjudicated

### 2.1 How the call id is obtained — the one real design fork

|  | Mechanism | Rests on |
|---|---|---|
| **protocol** | Register `PreToolUse` (matcher-limited to `AskUserQuestion\|ExitPlanMode`), join back to `PermissionRequest` by `(writer, tool, hash)` | the hash being **stable** across `PreToolUse→PermissionRequest`, both pre-decision |
| **resolution** | Lazily latch the id on the `Observe` tick from the writer's own unmatched `tool_use` | the pending `tool_use` being on disk, unmatched, for the whole wait |
| **graph** | Same lazy latch; `PreToolUse` listed as an optional later step, "measure first" | same |

A 2–1 vote is not the argument. The argument is that **the two proposals do not
rest on evidence of the same quality.**

resolution's premise is *already measured in this repo* — `transcript.go:531`
states the main jsonl carries the pending `tool_use` within ~5 s of the hook and
keeps it unmatched for the whole wait, with V4 evidence behind it. And the ~5 s
flush lag is irrelevant against the measured user think-time (45–300 s in the
report's own table): the latch lands long before the answer does.

protocol's premise is *unverified, and protocol says so itself* — it flags hash
stability across `PreToolUse→PermissionRequest` as "the one empirical claim my
design rests on… worth a live probe before landing." Reading the schema doc adds
a second unverified assumption it did not flag: `docs/claude-code-hook-schema.md`
documents that `PreToolUse` *carries* the id, but nowhere establishes that it
fires **before** the permission prompt rather than after approval. If it fires
after, the join cannot open a red at all and the whole mechanism is inert.

**Adjudication: build the lazy latch. Keep `PreToolUse` as a measured upgrade, not
a dependency.** Also note the disagreement is narrower than it reads — resolution
rejects *unmatched* `PreToolUse` registration, which protocol rejects too. The
live question is only matcher-limited registration, and it can be settled later
by probe without redesigning anything.

The three-line hook wiring is worth doing regardless: with the id on `HookSignal`,
an id-matched `PostToolUse` clears in <500 ms instead of ≤5 s.

### 2.2 The container — where graph is right for the wrong reason

graph alone spotted that `rs.pending` is `map[string]PendingPrompt`
(`observer.go:109`) — **one prompt per writer** — while every correlator in the
system names a *call*. It argues R4 landed on that map is a missed-RED generator,
because an id-matched `PostToolUse` deletes the writer's whole entry.

That is right, and it understates the problem. graph writes that "today the hash
mismatch accidentally protects it." **It does not.** Tracing the current code:

```
prompt A opens   → rs.pending[""] = A          (observer.go:286, unconditional overwrite)
prompt B opens   → rs.pending[""] = B          A is now forgotten; Since resets to B's
user answers B   → PostToolUse(B) matches B    → delete(rs.pending, "")  (observer.go:321)
                 → chip leaves red
prompt A is still unanswered and the agent is still blocked.
```

For two gated `Bash` calls the hashes *do* match, so the survivor clears cleanly
and the chip goes green with A outstanding. **M2 is live today**, independent of
R4. R4 widens it to tools whose hashes don't match; it does not create it.

Is the shape real? I measured it against the 25 largest transcripts on this
machine, counting entries per assistant `message.id` (Claude Code writes each
`tool_use` block as its own transcript entry — counting blocks-per-entry
undercounts by construction and gives a misleading ~0):

| Parallel calls in one assistant turn | Turns |
|---|---|
| 1 | 5217 |
| 2 | 385 |
| 3 | 33 |
| 4–8 | 9 |

**427 of 5644 tool-using turns (7.6 %) dispatch two or more calls in parallel, up
to 8-way.** That is the exposed population. It also independently vindicates
`BlockedByPendingTool`'s "any unmatched `tool_use`, never the trailing one".

One step remains unproven and it is the *only* thing standing between "M2 is a
live missed RED" and "M2 is a latent one": parallel *dispatch* is not the same as
two concurrent *permission prompts*, and none of the three studies established
that Claude Code presents a second `PermissionRequest` while the first is
unanswered. §5 gives the one-counter probe that settles it. **The container fix is
justified either way** — it costs little, and it is a hard precondition for the id
path — but the probe decides whether it ships as an urgent fix or as hygiene.

Also confirmed while checking: `prior` at `observer.go:281` feeds only change
detection, so the overwrite silently resets `Since` and restarts the stale-cap
clock. No study named that.

### 2.3 Codex — three studies, three different defects, all real

This is where independence paid. Each study found something the others missed;
none found all three.

**M1 — the ownership gap (resolution and protocol, independently).** Confirmed
directly: `reduceCodexPendingInput` (`codex_hook_transitions.go:843`) opens a
pending only for `req.Event == "PreToolUse" && isCodexUserInputTool(...)`, and
`isCodexUserInputTool` (`:1083`) normalizes to `requestuserinput` only. But
`isCodexHumanInputPermission` (`:1040`) — which is what raises the red — is
`request_user_input` **or `AskUserQuestion`**. So a Codex `AskUserQuestion` red is
owned by nobody, `applyCodexPendingAttention` never re-asserts it, and the next
hook edge mapping to active republishes `attention=none`. **This is the
2026-08-05 oscillation shape reproduced on the other provider**, and it is a
missed RED.

The two studies propose *different* fixes and the difference matters:

- protocol: widen the `PreToolUse` branch predicate at `:845`. One line.
- resolution: open the pending on the **`PermissionRequest`** edge for
  `isCodexHumanInputPermission`.

protocol's one-liner is only sufficient if Codex emits `PreToolUse` for
`AskUserQuestion`. If that tool arrives as a `PermissionRequest` only — which is
precisely why `isCodexHumanInputPermission` exists as a separate predicate from
`isCodexUserInputTool` — the one-liner latches nothing and the missed RED
survives, while the change *looks* landed. **Take the union: open on either edge.**

**The coupled matcher bug (resolution only).** `codexPendingInputMatches:1076` and
`codexPendingApprovalMatches:1033` both read
`if pending.toolUseID != "" || req.ToolUseID != ""` and then require id equality —
so when only *one* side carries an id, the id branch is taken and returns false,
and the composite fallback is unreachable. Today that fails safe. The moment a
`PermissionRequest`-opened pending (no id) meets an id-bearing `PostToolUse`, it
converts M1 from a missed RED into a red that sticks until `Stop`. **These two
changes must land together**, `||` → `&&`. resolution is the only study that saw
this, and it is what makes protocol's "nothing else changes; the close path
already handles it" wrong.

**The cross-writer sweeps (graph only).** `Stop` deletes every pending when
`req.TurnID == "" || pending.turnID == ""` (`:861-866`) regardless of writer;
`SessionStart`/`UserPromptSubmit` resolve every approval regardless of writer.
Confirmed at the line. A `Stop` from writer B erases writer A's live red — a false
green, and a missed RED when A is a human-input wait. graph is right, and this is
a *separate* defect from M1 that survives fixing it.

**Convergence verdict.** All three independently reached the same answer and it
should be recorded: **two models, one shared vocabulary and test corpus, no shared
code.** Claude's authority is a transcript of record; Codex's is hook edges plus
the app-server overlay. Claude needs Codex's call identity; Codex needs Claude's
ownership record. A shared struct would be a lowest-common-denominator with two
disjoint halves, and would invite the exact reflex — "the Claude fix ports" — that
produced M1.

### 2.4 Persistence of the latched id — minor, graph wins

protocol and resolution both persist `CallID` to `state.PendingPrompt`; graph
rejects it as schema plus federation churn for ~5 s once per restart, and would
re-earn it from the transcript. graph is right: re-latching is one `Observe` tick
and reflects what actually happened across the restart.

But resolution found a real persistence bug in the same area that *should* be
fixed: `compatibilityFromState` (`agent_observation.go:682`) hardcodes
`Attention: agentgraph.AttentionApproval`, because `state.PendingPrompt` carries
only `{Tool, InputHash, Since}`. Every question-red re-publishes as approval-red
across a daemon restart. Same colour today, a silent downgrade for anything that
later distinguishes them — and this plan distinguishes them. **Persist
`Attention`, not `CallID`.**

---

## 3. Where each study was wrong

Recorded so the reconciliation is auditable, not just averaged.

- **graph** — claimed the hash mismatch "accidentally protects" the scalar map. It
  does not; M2 is live today (§2.2). Also claimed Codex "already has the container
  right" and missed M1 entirely, which is the single most expensive finding in the
  study.
- **protocol** — its Codex one-liner is insufficient and possibly inert (§2.3),
  and its "nothing else changes" is wrong because of the coupled `||`/`&&` bug. Its
  Claude mechanism rests on two unverified emitter assumptions, one of which it did
  not notice it was making (hook ordering, not just hash stability).
- **resolution** — rejected `PreToolUse` on a cost argument ("fires on every tool
  call") that does not apply to the matcher-limited form it was not asked about;
  the honest answer is "unnecessary given the transcript latch," not "too
  expensive." It also missed the container defect entirely, and its own `CallID`
  proposal inherits M2 unless the container is fixed first.
- **All three** — none established whether two concurrent permission prompts per
  writer actually occur, though two of them build models whose correctness depends
  on it. None priced M1 and M2 against L1 to reorder the work; all three led with
  the latency fix.

---

## 4. The gameplan

Ordered by cost class, not by discovery order. Phases 1 and 2 are independent and
can land in either order.

### Phase 1 — Codex ownership (fixes M1, a missed RED)

1. `codex_hook_transitions.go:1033,1076` — `||` → `&&`, so the composite fallback
   is reachable when only one side carries an id. **Must precede or accompany 2.**
2. `codex_hook_transitions.go:843` — open a `codexPendingInput` for
   `isCodexHumanInputPermission` on **either** `PreToolUse` or `PermissionRequest`,
   keyed by `tool_use_id` when present and by the composite otherwise.
3. `codex_hook_transitions.go:861,918,925` — writer-scope the three sweeps: require
   `pending.writer == req.AgentID` when the turn id is absent. Put the writer in the
   `episode:` key.

**DoD.** A Codex `AskUserQuestion` red survives an unrelated writer's `PreToolUse`,
`PostToolUse` and `Stop`; it clears on its own id-matched `PostToolUse`, and on
`Stop` from its own writer if that never arrives. `request_user_input` behaviour is
byte-identical. Write the current false green as a failing test first.

### Phase 2 — Claude container (fixes M2, a missed RED) — **hard precondition for Phase 4**

4. `rs.pending` becomes `map[string][]PendingPrompt`, ordered by `Since`, capped
   (8/writer). `PermissionRequest` **appends** rather than overwrites; every read
   becomes `len(...) > 0` or first-by-`Since`; node attention folds as max over the
   writer's open prompts; the chip leaves red only at `len == 0`.
   Touches `observer.go:109,281-287,318-322,401-457` and `projection.go:145-200`.
   No wire change.

**DoD.** Two open prompts for one writer; answering one holds red; answering both
clears it. The compatibility projection's key set and `PendingTool` derivation stay
byte-identical.

**Two holes this phase deliberately leaves open**, both consequences of "no
state-schema change" and both belonging to later phases. Recorded here so they are
not lost:

- **Restart collapses the set.** `projectedPending` keeps the newest prompt,
  `state.PendingPrompt` persists `{Tool,InputHash,Since}`, and `restoredPending`
  rebuilds exactly one prompt per writer. Every older parallel prompt is forgotten
  across a daemon restart, and no hook re-opens it — `PermissionRequest` fires once
  per call — so M2's missed RED returns for the window between the restart and the
  writer's next transcript evidence. Answer the surviving prompt first and the chip
  goes green with the others still blocking. **Fix it in step 11**, which already
  reopens `state.PendingPrompt`: persist the set, not the scalar.
- **Every count downstream is a WRITER count.** `state.AgentInfo.PendingSummary`
  (`state.go:385`) renders `"%s+%d"` from `len(a.Pending)` and
  `internal/label/label.go:262` names writers from `PendingWriters`; neither can say
  "this one writer is blocked on three calls". The stale red this phase deliberately
  accepts — chip still red after you answered one of three — therefore has no
  explanation on any surface the user can see. **Phase 3 owns this**: a held red
  should be able to say how many calls are holding it.

### Phase 3 — Observability (R3; makes 4–5 measurable)

5. Thread `HookResult.Rule` and `promptResolution.Reason` into the recorded
   transition, replacing the blanket `agent_graph_authority`
   (`agent_observation.go:528,759`).

**DoD.** `switchboard-ctl diagnose` names the real rule again; a held red says why —
including *how many* of a writer's calls are holding it, which the Phase 2 container
knows and no surface currently renders.

### Phase 4 — Call identity (fixes L1, the 17 s)

6. `ToolUseID` on `HookSignal` (`observer.go:42`), set from `req.ToolUseID`
   (`agent_observation.go:755`). Three lines, inert until 7.
7. `transcript.PendingCall(path, tool, maxBytes)` beside `BlockedByPendingTool`;
   latch lazily in `resolvePending` for unbound prompts. Bind **only** on a unique
   unmatched `tool_use` of that tool name in the writer's own file; on ambiguity stay
   unbound forever rather than guess.
8. `promptMatches` (`observer.go:567`) — id equality wins; both present and unequal
   ⇒ hold (a real negative, unlike a hash mismatch); either absent ⇒ today's rule
   verbatim.
9. Lift the fanout floor (`observer.go:318`) **for id-matched clears only**, after
   the uniqueness assertion in §5 passes.
10. `ResolveKindFor(path, since, callID, maxBytes)` with a `ResolutionDeclined`
    kind; an id-matched user `tool_result` is decisive — approve when clean, decline
    on `is_error` / "User rejected tool use". `ResolveKind` stays as the
    `callID == ""` wrapper so every existing caller is byte-identical. Add
    `IsError` to `block` (`transcript.go:164`); `block.ToolUseID` is already parsed.
11. Persist `Attention` (not `CallID`) on `state.PendingPrompt`, fixing the
    restore-time downgrade at `agent_observation.go:682`. **Persist the prompt set,
    not one prompt per writer**, in the same change: Phase 2 left the restart
    collapse open (see its note above) and this is the one moment the schema is
    already being reopened. Until it lands, step 7's lazy latch must refuse to bind
    an id to a restored prompt — a restored record stands for a writer's residual
    red, not for its open set, and an id-matched clear against it would turn a
    single honest red into a green with real calls still blocking.

**DoD.** An answered `AskUserQuestion` clears in <500 ms with the hook, within one
`Observe` tick without it, with four teammates in flight. A teammate's
byte-identical call still holds. A declined question exits to idle, not green.

### Phase 5 — Optional, gated on §5's probes

12. Register `PreToolUse` matcher-limited to `AskUserQuestion|ExitPlanMode`, giving
    the id at open time and retiring the latch for those tools — **only if** both
    probes pass. Widen further only on measurement.

### 4a. Addendum — what actually landed in Phases 1–3

Written by the cross-phase audit, after the three phases were reviewed together
against the code. The numbered steps above are left as written so the plan stays
auditable; this section records where the code and the plan diverge, and which
DoD lines are still open. **Where they disagree, the code is right and the reason
is at the line.**

**D1 — Phase 1 step 3 landed inverted, and should have.** The plan asked to
writer-scope the three sweeps. The `Stop` sweeps are now writer-*blind*: the
`pending.writer == req.AgentID` conjunct the code already carried was removed
(`codex_hook_transitions.go:897,957`). Two facts verified at the line make the
plan's version a missed RED rather than a fix. `HandleHook` diverts
`SubagentStart`/`SubagentStop` to `enqueueCodexChildHook` before the reducer runs
(`agent_observation.go:830`), so every `Stop` that reaches the reducer is the root
turn boundary — there is no sibling `Stop` for the guard to catch. And Codex does
not guarantee `agent_id` on child tool hooks, so the guard is either inert (every
writer is `""`) or it *strands*: a record opened by an `agent_id`-bearing hook
would have no release edge at all, and `overlayCodexPendingObservation` re-asserts
that red on every snapshot until conversation rotation or the 24 h freshness
expiry. Pinned by `TestCodexQuestionShouldClearOnTheRootStopWhenTheOpeningHookNamedAWriter`.
Consequence: **§2.3's "cross-writer sweeps" finding has no Codex instance**, and
the Phase 1 DoD clause "survives an unrelated writer's `Stop`" is not met and
should not be. The cross-writer erasure it guards against is Claude-shaped, and is
pinned there by `internal/rpc/writer_match_test.go`.

Step 3's "put the writer in the `episode:` key" was also not done, and is
unnecessary: `codexPendingApprovalKey` reaches the `episode:` form only when the
record carries neither an id nor an input hash, and `codexWaitEpisode` is already
a monotonic per-coordinator counter.

**D2 — Phase 1 step 1 is not `||` → `&&`.** It landed as
`if pending.toolUseID != "" { return pending.toolUseID == req.ToolUseID }`. A plain
conjunction over-clears in the other direction: a pending that already names its
call would also be released by an id-less edge sharing only writer, turn, tool and
hash — a sibling call of the same shape — which §4 of
[status-color-state-model.md](status-color-state-model.md) forbids trading toward.
The asymmetric form keeps the composite as a fallback only for a pending that
never got an id, which is exactly the `PermissionRequest`-opened case §2.3 needs.

**D3 — Phase 1 landed one fix the plan did not ask for.** The approval grace used
to *delete* a pending gate when the chip was already red for something else
(`hook_approval_suppressed_existing_attention`). That is a missed RED: answering
the question would then paint green over an approval modal nobody has decided. It
now defers — re-arms a fresh record for another grace — and the diagnostic is
renamed `hook_approval_deferred_existing_attention`. Note the counter's meaning
changed with it: it counts grace periods survived (one per 30 s per held gate),
not gates suppressed.

**Verified: Phase 1's "write the current false green as a failing test first" was
done.** Six of the seven new Codex tests fail against `main`'s
`codex_hook_transitions.go`, including the M1 false green itself. The two that pass
on `main` do so by construction — one is the `request_user_input` byte-identity
pin, which must pass on both, and the root-`Stop` strand cannot exist on `main`
because `AskUserQuestion` opened no record there at all.

**U1 — Phase 3 DoD "`switchboard-ctl diagnose` names the real rule again" is NOT
met.** `diagnose` parses `rule=` out of the *journal* via
`statustune.ParseDecision`, and the only `statustune.Decision{}.Log()` call sites
are the legacy reconciler (`cmd/switchboard/main.go:1136,1153,1422`) and the legacy
hook path (`internal/rpc/rpc.go:840`). The provider-graph landing path emits no
decision line at all, so `diagnose` is blind to every Claude edge — before this
phase and after it. What Phase 3 *did* deliver is the real rule on the history
transition, which is where P4 and P5 read it. Closing this line needs a
`Decision{}.Log()` on `applyObservationWithRule`; it is a separate change and was
not attempted here.

**U2 — Phase 3 DoD "how many of a writer's calls are holding it" is NOT met.**
`HookResult.PromptDepth` exists and is correct, but its only consumer is the P1
counter. `state.AgentInfo.PendingSummary` (`state.go:386`) still renders `"%s+%d"`
from `len(a.Pending)`, a *writer* count, and the compatibility block still carries
one prompt per writer, so no user-visible surface can say "this writer is blocked
on three calls." Phase 2's deliberately-accepted stale red therefore still has no
explanation the user can see.

**Phase 2's DoD is met in full.** One residual worth carrying into Phase 4:
`openPendingPrompt` dedupes on whole-struct equality *including* `Since`, so two
prompts identical to the nanosecond would collapse into one — the missed-RED
direction. It requires two separate `switchboard-ctl` invocations to stamp the
same nanosecond, so it is theoretical rather than live, and step 7's call identity
removes it.

**No bad cross-phase interaction was found.** Phase 3 records only rule ids Phases
1 and 2 actually emit, every one resolves to a `ruleKnobs` entry, and the
vocabulary is Claude-only by design — every Codex transition still records
`agent_graph_authority`, which is what its knob hint says it means.

### 4b. Addendum — the Codex approval red, closed after Phase 1

Phase 1 closed the *question* red on Codex and left one open concern behind, which
is a missed RED of the same class and on the same provider: the pending record for
a generic approval gate survived an unrelated writer's edge, but the **published
colour did not**. `state.pending` is re-asserted on every observation by
`applyCodexPendingAttention`, which is what makes a question red outlive a
sibling's edge; `state.approvals` had no equivalent. After the grace timer
published `AttentionApproval`, the next `Stop` or `PostToolUse` from any writer
produced a root observation of active/none in `handleCodexHookNow`, and
`overlayCodexHookObservation` wrote that straight over the red while the modal was
still undecided.

**The fix is the missing re-assertion path, not a new record.**
`overlayCodexApprovalObservation` sits beside `overlayCodexPendingObservation` in
`applyObservationWithRule`, so it covers the hook landing path and the app-server
`Observe` tick alike. Three properties make it safe:

- **It re-asserts only a red the grace timer actually published**
  (`redPublishedAt` non-zero). Before the deadline a gate stays colourless — that
  silence is what the grace is *for*, and painting red there would be the false red
  §2.3 rejected.
- **It never repaints a more specific human reason.** The pending overlay runs
  first, so the approval overlay only ever fills a root whose attention is `none`.
  A question red therefore still wins while one is open, and D3's deferral still
  brings the gate's own red back the moment the question clears.
- **It reads the record rather than replacing it**, so the red is gone the instant
  the record is. Every release edge that existed before still releases: the gate's
  own `PreToolUse`/`PostToolUse`, the root turn `Stop` (writer-blind, per D1),
  `SessionStart`/`UserPromptSubmit`, conversation rotation and root removal, and
  the app-server settle branch of a deferred grace timer. They are enumerated in the
  comment on the overlay so a later change cannot quietly drop one.

**The stale-red window this deliberately accepts.** Once the red is published the
timer is spent, so an app-server snapshot reporting active/none no longer releases
the gate — only the edges above do. A gate the user *denies* emits no
`PreToolUse`/`PostToolUse`, so its red now stands until the turn `Stop` or the next
prompt instead of being erased by the next unrelated hook edge. That is a loud
stale red replacing a silent missed one, which is the trade §4 of
[status-color-state-model.md](status-color-state-model.md) requires. Extending the
`appServerSettled` trust past publication would shorten it, and was rejected here:
that trust is unproven for approvals on the standard-CLI launch path — the same
blind spot that makes `overlayCodexPendingObservation` refuse app-server evidence —
and buying seconds of staleness with a possible missed RED is the wrong direction.

**Phase 1's open concern is now closed; its DoD is otherwise unchanged.**
`request_user_input` and the whole question path are byte-identical — no reducer,
matcher, key or predicate was touched — and every Phase 1 test passes unmodified.

### 4c. Addendum — what Phase 4 landed (steps 6–10)

Step 11 was **not** attempted; it is the state-schema change and stays open, with
step 7's latch refusing to bind a restored prompt exactly as §4 requires
(`PendingPrompt.Restored`, pinned by `TestRestoredPromptShouldNeverBindACallID`).

**D4 — step 10 is wrong about `is_error`, and the corpus says so.** The plan (and
[attention-latency-report.md](attention-latency-report.md), and
[status-color-state-model.md](status-color-state-model.md) §A3) says to decline on
"`is_error` / `"User rejected tool use"`". Measured over `~/.claude/projects` on
2026-08-31, `is_error` alone is not that signal and is not close: 218 error results
carry a `toolDenialKind`, of which only **14 are `user-rejected`** — the other 204
are auto-denials by a permission rule or the auto-mode classifier, which raise no
prompt at all — and several hundred more error results are ordinary
approved-then-failed tools (`"Error: Exit code 1"`, 132 occurrences of that exact
string alone). All of those resumed the turn. Reading `is_error` as a decline would
send a working session to orange on every failing command.

So `is_error` landed as a **necessary** condition and the entry-level fields decide:
`toolDenialKind == "user-rejected"` or a `toolUseResult` string beginning
`"User rejected tool use"`, either sufficient. All three co-occur 14/14 in the
corpus, so checking two of them is redundancy against field drift rather than a
guess. Note this is an exit-colour error class, not a red-correctness one — both
kinds clear the red — which is why it was safe to correct rather than escalate.

**P3 passes, so step 9's floor is lifted — for id-matched clears only.** Measured
over the same corpus: 2361 sessions, 3963 transcript files, 99,893 distinct
`tool_use` ids, 61 sessions with more than one writer. **Exactly one** intra-session
id is claimed by two writers, and it is not a reuse: a subagent's own file opens
with a copy of the parent's launching `Agent` `tool_use`, so that id appears in
both files. It is **matched in both** (each carries its own spawn ack), so
`PendingCall` never offers it as a candidate. (839 ids are shared *across*
sessions — two main threads of a resumed/forked conversation — which the floor,
scoped to one `rootState`, never sees.) Both halves are pinned:
`TestNoCallIDShouldBeClaimedByTwoWritersAcrossOneSession` builds that exact spawn
echo, and `TestLatchShouldRefuseAndCountACallIDTwoWritersClaim` shows the latch
refusing and counting the id if the uniqueness ever does fail. Shape matches keep
the floor — `TestIDMatchedClearShouldLiftTheFanoutFloorWhileAShapeMatchStillHolds`
pins both directions in one test.

**Two things the plan did not name and the code needed.**

- *The latch binds only when the writer has ONE unbound prompt of that tool AND
  its file ONE unmatched call of it.* The plan's rule was the second conjunct
  alone. Without the first, a writer holding two same-tool prompts against a tail
  that shows one unmatched call binds that id to **both** — one completion then
  closes two prompts, which is the missed RED the ambiguity rule exists to
  prevent.
- *Dedupe had to move from the whole struct to a `promptIdentity`.* A prompt
  accumulates latch state the hook cannot carry, so `slices.Contains` stopped
  recognizing a prompt the moment it bound an id, and a redelivered
  `PermissionRequest` would have opened a second red nothing could clear. The
  same identity addresses the prompt across the Observe merge, where an index
  would be stale.

**Ambiguity is terminal, and is measured rather than assumed.** `prompt_call_latched`
/ `prompt_call_ambiguous` / `prompt_call_id_collision` are drained onto the
diagnostic table each `Observe` (`switchboard-ctl n`), so the reachability of the
fast path is a number rather than a hope. A prompt that reads ambiguous stays on
the `(tool, hash)` rule forever: the candidate set shrinks as siblings complete,
but nothing records *which* one shrank, so a later unique read is exactly as
likely to name the sibling.

**Phase 3's U2 is still open and is now slightly more visible.** A held red can be
`writer_call_mismatch_held` — "I know which call you finished, and it is not the
one you are waiting on" — but no user-facing surface renders it, for U2's reason.

---

## 5. The probes that settle what nobody established

Cheap, and each one decides a real branch above.

| Probe | Settles | Method |
|---|---|---|
| **P1** Count writers holding >1 open prompt, and the ordering `PermissionRequest`(A), `PermissionRequest`(B) before either `PostToolUse` | Whether M2 is live or latent — urgency of Phase 2 | One counter in `applyHookLocked` at the append site; read after a day |

**P1 as landed** is `claude:prompt_parallel_episode` (`switchboard-ctl n`). It counts
*episodes*, not edges: it fires only on the 1→2 transition, so an 8-way dispatch
counts once, and only when the append actually opened a prompt, so a verbatim
redelivery does not re-count. One upward bias remains and needs call identity to
remove — a hook registered in both the user and the project `settings.json` fires the
same edge twice with different wall-clock stamps, which is not a verbatim redelivery
and so opens a second prompt. Read the number with that in mind.
| **P2** Log `pretooluse_join = hit\|miss\|ambiguous` per `PermissionRequest`, and whether `PreToolUse` precedes it at all | Whether Phase 5 is possible; kills it if the hook fires post-approval | Register `PreToolUse` behind the matcher in a scratch settings file for one day |
| **P3** Assert no `tool_use_id` is claimed by two writers | Precondition for step 9 | `internal/fanout/observer.go:60` already keys `resultDone` by id; count violations |
| **P4** Rate of "id-clear followed within 30 s by a new `PermissionRequest` for the same writer+tool" | False-green proxy for the id path; target <1 % | From the history stream once Phase 3 lands |
| **P5** `answer→clear` p50/p95 for `AskUserQuestion`, and red episodes <2 s | Proves L1 fixed without a premature-clear regression | Re-run the report's §1 query |

---

## 6. Rejected

- **R4 as written** — both halves. The plumbing is already built; the hook-time
  transcript read is ~5 s too early and breaks the no-I/O contract.
- **R4 or the id path landed before Phase 2** — converts a stale red into a missed
  red on the 7.6 % parallel-dispatch population.
- **protocol's Codex one-liner alone** — insufficient, possibly inert, and unsafe
  without the `||`/`&&` fix.
- **Subset-stable hashing** — rescues one tool by weakening the sibling
  discriminator on every tool.
- **Persisting `CallID`** — re-earned in one tick; costs a schema field and
  federation churn.
- **Registering `PreToolUse` unmatched** — pay the matcher-limited price first.
- **R6 adaptive tick** — after an exact id match there is nothing left to buy.
- **One shared prompt package for both providers** — unanimous, and M1 is what
  that reflex already cost.
- **Clearing red on `UserPromptSubmit`** — queued messages during a pending prompt
  are common.
