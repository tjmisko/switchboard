# Status-Color State Model — diagnosis, redesign, and neutral graph reducer

> **Status:** the four-color contract and provider-neutral graph reducer are
> shipped. The original Claude-specific diagnosis remains below as the design
> record that motivated the reducer. The authoritative current fold is §0.1.
>
> This document maps the real state space, assigns a color to every state with
> an explicit error-cost justification, enumerates the transitions and their
> frequencies, and lays out a phased, test-first implementation plan.

---

## 0. Color semantics (the contract we are encoding)

Color is an **action semantic**, not a mechanism. The daemon reduces the
provider-neutral graph to a legacy status, and renderers map that status to the
existing palette. Legacy `AgentInfo.Status` remains a compatibility projection.

| Color | Status | Meaning to the user | Salience |
|-------|--------|---------------------|----------|
| **RED** | `permission` | "I need you **now**. Work is **blocked** until you act." | highest — grabs the eye |
| **GREEN** | `working` / `delegating` | "Work is happening. **Do nothing.** Don't look here." | calm/positive |
| **ORANGE** | `idle` | "I'm done/stopped, **your turn** — but nothing is stuck. Come back when convenient." | medium |
| **GRAY** | `unknown` | "I don't know the state." | low |
| grey-out overlay | `suspended` | Ctrl-Z'd; deliberately paused by the user. | de-emphasized |
| hidden | `empty` | no session in this slot. | none |

The unifying rule the user gave us — **GREEN = "work is happening, no action
needed"** — is the key to the redesign. It re-derives every color from two
questions:

```
                 user action needed?
                 NO              YES (blocking)
work     YES   GREEN           RED         (blocked but partial work continues; act)
happening?
         NO    ORANGE          RED         (your turn; or stalled-and-waiting)
```

"Work happening" **includes subagent/teammate work**. That single change fixes
complaint #2. The rest of this doc is about making the *transitions* into and
out of these states fast and faithful — which fixes complaint #1.

### 0.1 Current provider-neutral reducer

Every root graph carries three independent node axes:

| Axis | Values | Purpose |
|------|--------|---------|
| runtime | `unknown`, `not_loaded`, `idle`, `active`, `system_error` | what the thread is doing now |
| attention | `none`, `approval`, `user_input` | whether and why a person must respond |
| lifecycle | `unknown`, `pending`, `running`, `completed`, `interrupted`, `errored`, `shutdown`, `not_found` | orchestration state, especially for children |

`approval` and `user_input` are never conflated on a child node. Either makes
the root chip red because both require action; the row/tooltip retains the exact
reason. If both kinds exist at once, compact summary `attention` chooses
`approval`, while `approval_nodes` and `user_input_nodes` preserve both counts.

Provider adapters may emit those attention values only for a confirmed,
unresolved human-owned request. In particular, Codex's
`waitingOnApproval`/`waitingOnUserInput` active flags describe mechanical
app-server gates, not who must resolve them. The Codex adapter correlates the
gate with reviewer settings, exact server-request IDs and resolutions,
blocking/auto-resolution metadata, auto-review events, and guardian source
evidence before publishing a neutral graph. An unowned gate is gray/unknown,
never red. This ownership rule is enforced before graph history is written, so
an automated review creates no synthetic `permission` interval.

The reducer ignores provider/source names and uses only a valid graph plus its
half-open freshness interval (`observed_at <= now < fresh_until`). It evaluates
the following table top to bottom:

| Fresh valid graph | Live waits | Root runtime | Live descendant activity/lifecycle | Legacy status | Color |
|-------------------|------------|--------------|------------------------------------|---------------|-------|
| no | any | any | any | `""` (unknown) | gray |
| yes | any `approval` | any | any | `permission` | red |
| yes | no approval, any `user_input` | any | any | `permission` | red |
| yes | none | `active` | any | `working` | green |
| yes | none | `system_error` | any | `""` (explicit error axis retained) | gray root; error detail |
| yes | none | any other value | any child runtime `active` or lifecycle `pending`/`running` | `delegating` | green |
| yes | none | `idle` | none | `idle` | orange |
| yes | none | `not_loaded`/`unknown` | none | `""` (unknown) | gray |

The table assumes attention has already passed the provider ownership check;
the shared reducer intentionally has no Codex-specific debounce or reviewer
logic.

A child lifecycle is terminal at `completed`, `interrupted`, `errored`,
`shutdown`, or `not_found`. Terminal children remain available as bounded
display/history evidence but do not count as live work or waiting attention;
their stale attention cannot keep the root red. Error counts remain explicit
for nodes whose runtime is `system_error` or lifecycle is `errored`.

Freshness is authority, not decoration. A disconnected observer can leave the
last graph structure visible until/after its deadline, but once expired the
summary becomes unknown and renderers grey child detail as stale. A restored
last-known graph obeys the same deadline. A newer, fresh high-authority source
(`codex_app_server` or `claude_transcript`) outranks ordinary
hook/rollout/restored fallbacks in daemon orchestration. The exception is
Codex's exact, hook-owned `request_user_input` transition: generic app-server
snapshots cannot resolve a standard-CLI question. The reducer itself stays
source-neutral.

Only root `Session` rows are switchable. Child rows have colors and labels for
visibility but never add Waybar slots, focus selectors, picker entries, or cycle
targets.

---

## 1. The legacy pre-graph model (and why it was one-dimensional)

Before the neutral graph landed, the entire state of a session was collapsed
onto a single enum,
`AgentInfo.Status ∈ {working, idle, permission, unknown}`
(`internal/state/state.go:108`), driven two ways:

1. **Edge-triggered hooks** (`internal/rpc/rpc.go:382` `statusFromHookEvent`):
   `UserPromptSubmit`/`PostToolUse`→working, `Stop`/`SessionStart`→idle,
   `PermissionRequest`→permission. Set on a real transition only; stamps
   `StatusSince` (`rpc.go:302`).
2. **A 5 s reconciler** (`cmd/switchboard/main.go:185` `runReconciler` →
   `reconcileOnce`) that self-heals the latches the hooks leave behind:
   - `selfHealStaleAttention` (`main.go:241`): decays a `permission` chip once
     the transcript proves the prompt resolved (`transcript.ResolutionState`),
     else a 30 s TTL **only** on an unreadable transcript.
   - `selfHealStuckStatus` (`main.go:284`): `idle→working` on fresh transcript
     activity, `working→idle` on an interrupt marker (`transcript.NewestSignal`).

That model had **no structured representation of subagent activity**. It could
not express "main idle, teammates working" without Claude-specific enrichment.
And because `permission` was one undifferentiated state, it could not tell
"resolved-by-approve → resume working" from "resolved-by-decline → your turn."

---

## 2. Root-cause analysis (with measured evidence)

This section is a historical Claude incident analysis. “Today” in quoted or
retained notes means the capture date, not the current graph implementation.

### 2.1 Stale RED is a *resolution-latency* problem, not a TTL problem

Empirical (journal `switchboard.service`, 2026-05-30 → 06-23, 43 red episodes):

- **The 30 s `permissionDecayTTL` never fired.** 25/25 reconciler decays were
  `reason=resolved`; **0** were `reason=ttl`. The TTL is a red herring.
- The felt symptom is the **gap from work-demonstrably-resuming to the chip
  clearing**. Measured as *first held `PostToolUse` → clear*: **median 26 s,
  mean 34 s, p90 78 s** (n=26; 19 of 26 in the 10–40 s band). This is an exact
  match for the reported "10–30 s."
- The reconciler itself is fast: *last hold → decay* is median **3 s** (≤5 s tick).
  The latency is **upstream of the tick** — resolution becomes *provable* late.

**Mechanism.** When a prompt resolves:
- **Decline / Esc** fires **no clearing hook** at all (empirically verified;
  `PostToolUse` only fires on success, `Stop` not on interrupt).
- **Approve** does fire `PostToolUse`, but `handleHook` **deliberately holds** it
  (`rpc.go:289`): a bare `tool_result` is not treated as resolution, because a
  background subagent's `Task` result or a sibling auto-approved tool in the same
  turn also flushes `tool_result`s dated after the prompt — counting them would
  flash the chip green while the prompt is still genuinely pending.

So the **only** clear path is `transcript.ResolutionState` seeing the *main
thread advance past the prompt* — an **assistant message** newer than
`StatusSince` (`transcript.go:292`, `:305`). The *next* assistant message arrives
only after **model latency (≈5–25 s)**. That model-latency window + the 5 s tick
granularity **is** the 10–30 s band.

> **Correction (2026-08-05).** This passage previously attributed the band partly
> to Claude Code "withholding the pending tool_use's assistant message until it
> resolves." **That is false** — the pending `tool_use` *is* written to the main
> transcript while the prompt waits, measured contemporaneously against a
> byte-cursor reader that cannot be fooled by timestamp back-dating
> (`subagent-permission-plan.md` §9.7, V4). The latency band itself is real; only
> the stated cause was wrong. The withholding claim also appeared in
> `state-schema.md` and in `internal/transcript`'s package comment, both now
> corrected. This matters beyond bookkeeping: a hydrate-time falsifier built on
> the withholding assumption would have dropped live main-thread prompts.

> The conservatism is **correct** (see §4: missed-RED is the most expensive
> error). The bug is that the resolution *signal* is later than it needs to be.
> The fix is an **earlier, equally reliable** resolution signal — not a looser one.

There is a secondary latency on the **green half**: when `selfHealStaleAttention`
decays `permission→idle` it re-stamps `StatusSince=now`, so the *same tick's*
`selfHealStuckStatus` is mtime-gated out (`main.go:291`) and cannot promote
`idle→working`. A session that resumed work after an approval therefore shows
RED → ORANGE → (≥1 tick later) GREEN, adding ≥5 s of false-orange.

### 2.2 ORANGE-while-teammates-working is a *missing-dimension* problem

Documented in `orange-chip-orchestrator-drift.md` and confirmed in code+logs:

- An orchestrator's main turn **ends** between teammate wake-ups → `Stop` →
  `idle`/orange (`statusFromHookEvent`).
- When a teammate finishes and the orchestrator is woken to recompute, it is
  mid-turn / pre-tool, so **no working hook fires**; the chip lags orange.
- `selfHealStuckStatus` catches it (`idle→working reason=transcript-activity`,
  **65 occurrences** in the window) but only after the *next* transcript write
  **and** the next 5 s tick — and it flaps each time the turn ends again.

The deeper issue: the daemon has no notion of "delegated work in flight," so it
can only infer activity from transcript writes after the fact. Claude Code stores
subagent transcripts at
`~/.claude/projects/<cwd>/<session-id>/subagents/agent-*.jsonl`, with a
`*.meta.json` carrying `{agentType, description, toolUseId}` that links each
subagent to its spawning `Task` tool_use in the **main** transcript. **In-flight
Task count is directly derivable**: `tool_use`(name∈{Task,Agent}).id minus
`tool_result`.tool_use_id over the main transcript tail. And `SubagentStop`
**is** emitted (fires on the parent session_id; carries `agent_id`,
`agent_transcript_path`) — it was **not wired** to Switchboard at the time (only
to the temporary `.claude/hook-logger.sh`). There was no `SubagentStart` hook in
that capture.

---

## 3. The Claude compatibility state space that motivated the graph

The true state of a session is a tuple of orthogonal dimensions; the legacy
model was a lossy projection of it onto one axis.

| Dim | Name | Values | Source signal |
|-----|------|--------|---------------|
| **M** | main-thread turn state | `working` · `turn-ended` · `interrupted` · `blocked-on-prompt` · `unknown` | hooks + transcript |
| **S** | subagents in flight | `0` · `>0` (count) | main-transcript Task pairing / `SubagentStop` |
| **P** | genuine user-blocking prompts pending | **a set**, keyed by the writer that raised each — `∅` · `{main}` · `{a₁…}` · `{main, a₁…}` | `PermissionRequest` + a **per-writer** resolution check |
| **L** | process liveness/control | `live` · `suspended` · `gone` | `/proc` state |
| **F** | confidence/freshness of our info | `fresh-hook` · `inferred` · `stale/unknown` | which path set it + age |

`P` is a refinement of `M=blocked-on-prompt`, kept separate because a prompt can
also be raised by a **subagent** and still requires user action. The color is a
function `color(M, S, P, L, F)`.

### 3.1 The writer dimension (added 2026-08-05)

The model above treats `P` as a scalar and every other dimension as a property
of *the session*. A Claude Code session is not one thread — it is **1 + N
concurrent writers**: the main thread plus each in-flight subagent. They

- **share one pid** — hooks identify themselves with `getppid()`, so a teammate's
  event is indistinguishable from the main thread's by pid;
- **share one chip** — one color must summarize all of them;
- **share one `transcript_path` on the wire** — every hook reports the *parent's*
  transcript, even when fired from inside a subagent
  ([claude-code-hook-schema.md §3](claude-code-hook-schema.md));
- **write to different files** — main `<session>.jsonl` vs.
  `subagents/agent-<id>.jsonl`;
- and each can **independently block on a permission prompt**.

Two consequences the original model does not capture, and which together produced
the 2026-08-05 incident:

1. **`P` must be a set keyed by writer, not a boolean.** "A prompt is pending" is
   a property of a writer, not of a session. Four teammates can block
   independently, and the main thread can block while a teammate is blocked.
2. **Every question about a prompt must be routed to the writer that raised it.**
   "Did it resolve?" answered against the *main* transcript is a different
   question from the one being asked when a *subagent* raised the prompt. Main
   thread activity is not evidence about a teammate's prompt, and a teammate's
   `PostToolUse` is not evidence about the main thread's.

The writer identity is available and unambiguous: the `agent_id` hook field
(absent ⇒ main thread). It is already on the wire and already forwarded as
`rpc.Request.AgentID`.

**P0 — Attribute every prompt to its writer, and resolve it only with evidence
from that writer.** This is the design principle the §4 ranking was missing;
without it, P1's "never clear red on a bare/unrelated `tool_result`" cannot be
enforced, because "unrelated" is undecidable from `tool_name` alone.

---

## 4. Asymmetric error costs (why simple rules fail)

The user is right that costs are asymmetric; the design must be tuned to them.
Ranked most-expensive first:

1. **Missed RED** — agent is *blocked waiting on you*, shown green/orange.
   **Worst.** Persistent, *silent* wall-clock waste (minutes–hours); the user
   never learns to act. ⇒ Be **eager to enter** red, **reluctant to leave it on
   a weak signal**. This is why the `PostToolUse`-hold gate exists and must stay.
2. **Persistent false RED** — red lingers after resolution (the current
   complaint). Costly because red is the loudest color and the cost **scales with
   duration** (1 s = fine; 26 s = bad). ⇒ Once resolution is *proven*, leave red
   **fast**. The tension with #1 is the whole game: distinguish resolved from
   pending **quickly and reliably**.
3. **False GREEN** — idle/finished shown as working. Moderate: the user ignores a
   chip that actually wants their input, but usually the agent is merely "done,"
   not hard-stuck, and the user returns eventually.
4. **False ORANGE / missed GREEN** — teammates working, shown idle (complaint #2).
   Low–moderate: work continues regardless; the harm is a needless context
   switch + erosion of trust in the colors.
5. **Sub-2 s transients** during a legitimate transition — negligible; do not
   over-engineer them away.

**Design principles that fall out of this ranking:**

- **P1 — Red is sticky on entry, fast on a *proven* exit.** Never clear red on a
  bare/unrelated `tool_result`. Do clear it the moment a signal *tied to this
  prompt* proves resolution.
- **P2 — Prefer an earlier identity-correlated signal over a late generic one.**
  Match the *specific* pending tool (by id or name) so the approve/decline edge
  clears red in ≤5 s instead of waiting for the next assistant message.
- **P3 — Resolution *kind* selects the exit color.** Approve → `working`
  (green, work resumed); decline/interrupt → `idle` (orange, your turn). Never
  bounce through orange on the way to green.
- **P4 — Work-happening includes subagents.** `idle ∧ S>0 → green`.
- **P5 — Unknown is its own color.** When freshly rehydrated or the transcript is
  unreadable, prefer GRAY/last-known with a decay, not a confident guess.

---

## 5. The canonical case table

`if (Main, Subagents, Pending-prompts, Liveness) then COLOR`. Liveness/confidence
modifiers are applied first, then the M×S×P core. "Worst error" names the costly
mistake the rule is protecting against.

The **P** column names *which writers are blocked* (§3.1), not merely whether
something is: `∅` none · `{main}` the main thread · `{a}` a subagent · `{main,a}`
both. This is the column that disambiguates rows 8/12/16 — previously all three
read "yes" and the M column silently carried the writer identity.

**The fold**, stated once so no row is ambiguous. Evaluated top to bottom; the
first match wins:

```
L = gone                    → hidden
L = suspended               → grey-out
P ≠ ∅                       → RED          ← dominates M and S entirely
M = unknown ∧ S = 0         → GRAY
M = working ∨ S > 0         → GREEN
otherwise                   → ORANGE
```

`P ≠ ∅ → RED` is the priority rule the table previously encoded only
implicitly: **a prompt pending anywhere in the session tree outranks any amount
of work happening anywhere in it.** Blocking-the-user is actionable;
work-is-happening is not, so the actionable signal wins. This is what makes
case 16 fall out rather than be a special case.

| # | Main thread (M) | Subagents (S) | Blocked writers (P) | Liveness (L) | **Color** | Why | Worst error avoided |
|---|---|---|---|---|---|---|---|
| 1 | * | * | * | **gone** | *hidden* | session ended | — |
| 2 | * | * | * | **suspended** | **grey-out** | Ctrl-Z; user paused it, nothing can progress | false-green on a halted proc |
| 3 | working | any | **∅** | live | **GREEN** | work happening | — |
| 4 | turn-ended | **0** | **∅** | live | **ORANGE** | done, your turn, nothing stuck | false-green (#3 cost) |
| 5 | turn-ended | **>0** | **∅** | live | **GREEN** | **teammates working — no action needed** *(fix #2)* | false-orange (#4 cost) |
| 6 | interrupted (Esc) | 0 | **∅** | live | **ORANGE** | you stopped it; your turn | false-green |
| 7 | interrupted (Esc) | >0 | **∅** | live | **GREEN**\* | work still in flight; *(see Q3)* | low either way |
| 8 | **blocked-on-prompt** | any | **{main}** | live | **RED** | main stalled; you must act (even if teammates still churn) | **missed-RED (#1, worst)** |
| 9 | blocked, **resolved-by-approve** | any | **∅** | live | **GREEN** | turn resumed → work continues *(go direct, not via orange — P3)* | false-orange + #2-latency |
| 10 | blocked, **resolved-by-decline/interrupt** | 0 | **∅** | live | **ORANGE** | you answered/declined; your turn | persistent false-red (#2) |
| 11 | blocked, **resolved-by-decline** | >0 | **∅** | live | **GREEN** | you declined but teammates still working | false-orange |
| 12 | blocked | any | **still {main}** — an unrelated `tool_result` landed (teammate/sibling) | live | **RED** (hold) | not resolution — keep nagging | **missed-RED (#1)** |
| 13 | unknown | 0 | **∅** | live | **GRAY** | no signal yet / just rehydrated | confident wrong guess |
| 14 | unknown | >0 | **∅** | live | **GREEN** | we can see in-flight Tasks even with no main signal | false-orange |
| 15 | blocked | any | **{main}**, transcript **unreadable** ≥ TTL | live | **ORANGE** (decay) | last-resort backstop; observed 0× | nagging forever |
| 16 | **working** | **>0** | **{a}** — a *subagent* is blocked | live | **RED** | *you* are blocking teammate work; a quick decision unblocks it | **missed-RED (#1)** |
| 17 | turn-ended | >0 | **{a}** | live | **RED** | same, with the main thread already done | missed-RED |
| 18 | blocked-on-prompt | >0 | **{main, a}** | live | **RED** | two independent decisions outstanding; answering one does **not** clear the chip | missed-RED via partial resolution |
| 19 | any | >0 | `{a}` where **a died / quiesced ≥ cap** | live | drop `a` from P, re-fold | a dead teammate's prompt can never be answered | red leaking forever |

\* Case 7 is the one genuine judgment call — see Open Questions Q3.

**Rows 16–19 are the writer dimension (§3.1).** 16 and 17 are the cases that
motivated it: a teammate blocked on the user while work continues elsewhere.
18 is why `P` must be a *set* — with a scalar, resolving either prompt clears
both. 19 is the liveness backstop the set needs so a prompt from a crashed
teammate cannot latch red forever (the per-writer analogue of case 15).

Resolution routing follows directly from §3.1's P0: **each entry in `P` is
removed only by evidence from the writer that raised it** — the main transcript
for `{main}`, `subagents/agent-<a>.jsonl` and `agent_id`-matched hooks for `{a}`.
Rows 9/10/11 therefore describe removal of `{main}` specifically; teammate
activity is not evidence about it, and vice versa.

Rows **5, 9, 11, and 14** motivated subagent-awareness and direct red→green;
rows **16–19** motivated the per-writer pending map. The earlier implementation
did violate cases 12 and 16 by correlating only on tool name. That diagnosis is
preserved in
[subagent-permission-oscillation.md](subagent-permission-oscillation.md); the
current Claude compatibility adapter preserves writer-keyed pending ownership,
while the neutral graph independently represents each node's attention.

---

## 6. Target state machine, transitions & frequencies

States: `WORKING(green)`, `IDLE(orange)`, `DELEGATING(green; idle-but-S>0)`,
`PERMISSION(red)`, `UNKNOWN(gray)`, plus the `SUSPENDED`/`GONE` overlays.
`DELEGATING` is now a stored legacy summary value derived by the neutral
reducer. It renders green like `WORKING` but remains distinct on the wire.

```
                         UserPromptSubmit / activity
        ┌───────────────────────────────────────────────────────┐
        │                                                         ▼
   ┌────────┐  Stop ∧ S=0        ┌────────┐   PermissionRequest   ┌────────────┐
   │WORKING │ ─────────────────▶ │  IDLE  │ ───────────────────▶ │ PERMISSION │
   │ green  │ ◀───────────────── │ orange │ ◀──── decline/Esc ─── │    red     │
   └────────┘  activity (resume) └────────┘   (resolved-decline)  └────────────┘
        │ ▲                          │ ▲                                │
   Esc  │ │ Stop ∧ S>0         S→0   │ │ S>0 (Task launched)   approve  │
 (intr) │ │ (delegating)            │ │                    (resolved-  │
        ▼ │                         ▼ │                     approve)    │
   ┌────────┐                  ┌────────────┐                          │
   │  IDLE  │                  │ DELEGATING │ ◀────────────────────────┘
   │ orange │                  │   green    │   (resolved-approve ∧ S>0)
   └────────┘                  └────────────┘
```

**Transition frequencies** (from the 4-day window; "heavy dev" sample):

| Transition | Trigger | Frequency | Current handling | Health |
|---|---|---|---|---|
| working↔idle | UserPromptSubmit / Stop | every turn (very high) | hooks | ✅ fast |
| idle→working (orchestrator wake) | teammate resumes main | **65×** | reconciler ≤5 s + flaps | ⚠ lag/flap → **DELEGATING fixes** |
| working→permission | PermissionRequest | 43 episodes | hook | ✅ fast |
| permission→working/idle | approve/decline/Esc | 43× (17 hook, 25 decay) | **median 26 s lag** | ❌ **the complaint** |
| working→idle (Esc) | interrupt, no Stop hook | 8× | reconciler marker | ✅ acceptable |
| decay reason=ttl | unreadable transcript | **0×** | TTL backstop | ✅ (keep as backstop) |

---

## 7. Historical implementation plan (test-first, pin-then-fix)

Phased so each step is independently shippable and independently verifiable.
Each item names the seam and the Definition of Done.

### Phase A — Earlier RED exit (fixes complaint #1; biggest felt win)

**A1. Resolution *kind* drives the exit color (P3).**
- `transcript.ResolutionState` → return a richer result that distinguishes
  `ResolvedApprove` (assistant message past `since`) from
  `ResolvedDeclineOrInterrupt` (interrupt notice / rejected tool_result).
- `selfHealStaleAttention` (`main.go:241`): on approve → `working`; on
  decline/interrupt → `idle` (current behavior). Removes the RED→ORANGE→GREEN
  bounce (§2.1 secondary latency).
- **DoD:** approving a prompt drives `permission→working` in one tick; declining
  drives `permission→idle`; a still-pending prompt stays red. Unit tests on the
  transcript classifier + a reconciler table test.

**A2. Identity-correlated early clear of the approve path (P2).**
- Plumb the **tool name** from the hook through to the daemon: extend
  `switchboard-ctl` `cmdHook` (`cmd/switchboard-ctl/main.go:277`) to forward
  `tool_name`, and add it to `rpc.Request` (`rpc.go:36`). `PermissionRequest`
  already carries `tool_name`; stash it on `AgentInfo` at red-onset.
- In `handleHook`'s hold gate (`rpc.go:289`): clear red on a `PostToolUse`
  whose `tool_name` **matches** the pending prompt's tool (the approved tool
  completed) — while still holding on a non-matching / `Task` tool_use.
- This collapses the approve-path lag from ≈26 s to sub-second (hook-speed),
  without weakening the missed-RED guard.
- **DoD:** approve → red clears at hook speed; a sibling/`Task` `PostToolUse`
  during a pending prompt still holds red (regression test for the
  `resume-career-exploration` false-clear).
- *Alternative considered:* wire `PreToolUse` to capture the exact
  `tool_use_id`. More precise but needs a settings change and a new hook; the
  tool-name match covers the common cases (AskUserQuestion, ExitPlanMode, Bash
  approval) without it. See Q2.

**A3. Faster decline detection.**
- `ResolutionState` should also count the **interrupt notice** and a
  **rejected tool_result for the pending tool** as resolution (it already counts
  the interrupt). Decline of `AskUserQuestion` records a `tool_result`
  `is_error:true` / `"User rejected tool use"` — match it by the pending tool
  identity from A2 so it counts *without* re-opening the subagent confound.
- **DoD:** declining a question clears red within ≤1 reconcile tick (≤5 s), not 26 s.

**A4. (Optional) Tighten the tick for permission chips.**
- Consider a shorter reconcile cadence *only while any chip is `permission`* (or
  an event-driven recheck), so the worst-case tail shrinks from 5 s to ~1 s.
  Low priority once A1–A3 land. See Q4.

### Phase B — Subagent awareness → DELEGATING green (fixes complaint #2)

**B1. In-flight Task counter from the main transcript.**
- New `transcript.InFlightTasks(path, maxBytes) (int, error)`: pair
  `tool_use`(name∈{Task,Agent}).id against `tool_result`.tool_use_id over the
  tail. Requires extending the `block` struct (`transcript.go:143`) with
  `name`, `id`, `tool_use_id`.
- **DoD:** unit tests over a fixture transcript with 0, 1, N in-flight Tasks and
  fully-drained Tasks.

**B2. Render idle-with-teammates as green.**
- Add `InFlightSubagents int` (or `WaitingOnSubagents bool`) to `AgentInfo`
  (`state.go:105`), `json:"-"` like `StatusSince`.
- `selfHealStuckStatus` (`main.go:284`): when `Status==idle` and
  `InFlightTasks>0`, treat as `working` (or set a delegating flag the renderer
  paints green); when the count returns to 0 and the main thread is still idle,
  fall back to orange.
- Renderer: `sessionStatus` returns `working` for the delegating case (or a new
  `delegating` class that the CSS paints green). Tooltip can show the count.
- **DoD:** an orchestrator with a running teammate stays green across the `Stop`
  between wake-ups; reverts to orange within one tick of the last teammate
  finishing. Kills the 65× idle→working flap.

**B3. Wire `SubagentStop` as the precise drain edge (optional but clean).**
- Add `SubagentStop` to `~/.claude/settings.json` → `switchboard-ctl hook
  SubagentStop`; handle in `statusFromHookEvent`/`handleHook` to decrement the
  in-flight count (or trigger an immediate recount) at hook speed instead of
  waiting for the tick. (No `SubagentStart` exists → increment from the Task
  `tool_use` seen by B1, or a `PreToolUse` wiring.)
- **DoD:** the green→orange revert on last-teammate-finish happens at hook speed.

### Phase C — Confidence/UNKNOWN fidelity (smaller)

**C1.** Ensure a freshly-rehydrated session reads GRAY (not a guessed
working/idle) until its first real signal; verify `dropStaleSessions`
StatusSince stamping still holds (`behavior-spec.md §7.3`). Keep the 30 s TTL as
the unreadable-transcript backstop only.

> **Sequencing note:** Phase A and Phase B are independent and can land in
> either order; A is the higher-felt win. B2 also *enables* a cleaner A2 (knowing
> which `PostToolUse` came from a `Task` makes the hold gate's identity check
> simpler), so doing B1 first slightly simplifies A2.

---

## 8. Open questions / decisions

- **Q1 — Delegating visual:** pure GREEN (indistinguishable from a normally
  working session), or GREEN with a subtle marker (count badge / different
  shade / tooltip "2 agents")? User asked for green; default to pure green,
  badge is a cheap add for fidelity.
- **Q2 — Approve early-clear mechanism:** tool-name match (no settings change,
  covers common cases) vs wiring `PreToolUse` for exact `tool_use_id`
  (most precise, needs a settings + hook change). Recommend tool-name first.
  **⚠ Reopened by the 2026-08-05 incident** — tool-name matching false-cleared a
  genuinely-pending RED because a *teammate's* `Bash` collided with the pending
  prompt's `Bash`, and the resulting green/orange limit cycle ran for 95 s. The
  correlator to use is `agent_id`, which the hook payload already carries on
  tool events and which `rpc.Request` already receives unread. See
  [subagent-permission-oscillation.md](subagent-permission-oscillation.md).
- **Q3 — Case 7 (Esc with teammates still in flight):** GREEN (follow
  "work-happening") or ORANGE (Esc signals "I want control")? Low frequency,
  low cost either way. Recommend GREEN for consistency with P4.
- **Q4 — Adaptive tick:** shorten the reconcile interval while any chip is red?
  Only worth it if A1–A3 leave a felt tail.

---

## 9. Implementation status (shipped)

Phases A and B landed together, followed by the provider-neutral graph. The
graph is now the canonical cross-provider authority; `statustune.Tuning` and the
details below remain the Claude compatibility/self-heal layer. Decisions taken —
all wired as `statustune.Tuning`
fields so they are retunable in one place (`cmd/switchboard/main.go` builds the
`Tuning`; override a field there):

- **Q1 → pure green**, with fidelity for free: a `delegating` chip renders with
  waybar class `["working","delegating"]` (green via existing `.working` CSS; the
  `.delegating` class is an optional badge hook) and the tooltip shows `N agents`.
  `in_flight_subagents` is on the wire so `switchboard-ctl list --json` reveals the
  true state behind any green chip.
- **Q2 → tool-name match** (`Tuning.EarlyClearApproveByToolName`, default on). No
  settings/hook change; the transcript `ResolutionResumed` check is the fallback.
- **Q3 → green** when interrupted/declined with teammates in flight
  (`Tuning.EscWithTeammatesStatus`, default `delegating`).
- **Q4 → not done.** A1–A3 + the hook-speed early clear collapse the approve path
  to sub-second; revisit only if a tail is still felt.

What shipped, by seam:

| Item | Where | Status |
|---|---|---|
| A1 resolution *kind* → exit color (resume→green direct, no bounce) | `transcript.ResolveKind`, `main.permissionExit` | ✅ |
| A2 identity-correlated early red-clear (tool-name) | `rpc.clearsPermission`, `ctl` forwards `tool_name`, `AgentInfo.PendingTool` | ✅ |
| A3 faster decline detection (rejected tool_result) | — | ⏭ deferred (decline still clears via the resume/interrupt signals; revisit if a decline-tail is felt) |
| B1 in-flight Task counter | `transcript.InFlightTasks` | ✅ |
| B2 idle-with-teammates → `delegating` green | `main.selfHealStuckStatus`, renderers | ✅ |
| B3 wire `SubagentStop` for hook-speed drain | — | ⏭ deferred (the tick-based recount works; this only sharpens the green→orange revert) |
| C UNKNOWN/GRAY fidelity on rehydrate | — | ⏭ deferred |

The later neutral work added:

| Item | Where | Status |
|------|-------|--------|
| independent runtime/attention/lifecycle axes | `internal/agentgraph` | ✅ |
| approval vs user-input preservation | graph nodes + summary counts | ✅ |
| one shared reducer and `delegating` projection | `agentgraph.Reduce`, `state.SetAgentGraph` | ✅ |
| explicit source freshness and stale/unknown expiry | provider observations + daemon authority gate | ✅ |
| nested, non-switchable child presentation | reference TUI + Waybar tooltip | ✅ |
| canonical `agent_state` history and child timeline | `internal/history`, `switchboard-ctl timeline` | ✅ |
| Claude compatibility shadow/projection | `internal/provider/claude` | ✅ |

## 10. Operating it: diagnosing a wrong color, then retuning

This is the loop for "the chip was X, it should have been Y."

For provider binding, graph authority, or stale/unknown failures, start with the
content-free observer view:

```text
switchboard-ctl diagnose --observer
switchboard-ctl diagnose --observer --json
switchboard-ctl diagnose --observer --state /path/to/state.json --file /path/to/journal.txt
```

It reports bound/unbound state, binding source, graph source, fresh/expired
status, complete/partial snapshot, live/wait/error counts, observer rollout
mode, the latest finite error category, and Claude graph-vs-legacy shadow
agreement. It never emits cwd, transcript paths, node names/descriptions,
prompts, commands, or raw provider payloads. For an unbound Codex root, verify
that its trusted `SessionStart` or later lifecycle hook is reaching
Switchboard. Never “repair” a binding with cwd correlation.

**0. Use `switchboard-ctl diagnose` (the built tool).** It pulls the relevant
decision lines for a time window, keeps the ones a plain-English symptom makes
relevant, and prints each with the rule that fired **and the `Tuning` knob to
change** — plus a summary with the RED-episode durations (recovered from `age=`).
No hand-rolled grep, no daemon connection needed:

```
switchboard-ctl diagnose --around 14:32 red was stuck for ages
switchboard-ctl diagnose --since "20 min ago" should have been green not orange
switchboard-ctl diagnose --session ce13c0f2 --symptom green went green too early
# offline / from a saved dump or a pasted log:
journalctl --user -u switchboard.service -o short-iso | switchboard-ctl diagnose --file - red
```

It infers the symptom (stale-red / false-green / false-orange) from your words, or
take `--symptom red|green|orange|all`; `--around <t> [--window 2m]`, `--since/--until`,
`--session`, `--pid`, and `--json` narrow or reshape the output. Steps 1–3 below are
what `diagnose` automates — reach for them only to read the raw lines directly.

**1. Recover what the daemon saw.** Every status decision — change *or* deliberate
hold — is logged with a stable prefix and the full observed tuple:

```
journalctl --user -u switchboard.service | grep 'status: pid=<PID>'
# or by session: ... | grep 'session=<first8-of-uuid>'
```

Each line reads:

```
status: pid=4821 session=ce13c0f2 permission->working rule=case9-approve-toolmatch reason="tool-name match: AskUserQuestion" [S=0 pending="AskUserQuestion" age=2s]
```

- `FROM->TO` (or `FROM==TO` for a hold) — the decision.
- `rule=` — maps to the §5 case table; this is the exact branch that fired.
- `reason=` — human detail.
- `[S=… pending=… age=…]` — the M/S/P tuple at decision time: subagents in
  flight, the tool the red prompt was for, how long the chip had held.

Reconciler and permission-gate decisions carry `rule=` + the tuple (the lines that
matter for stale-red / wrong-delegating / wrong-idle complaints). Plain
hook-driven edges (`UserPromptSubmit`→working, `Stop`→idle) instead end in
`(agent=… event=…)` — they are unambiguous and rarely the subject of a complaint.

Find the line at the complaint's timestamp; its `rule=` names the branch to change.

**2. Map the rule to the knob.** Rules and the `statustune.Tuning` field that
governs them:

| Rule id | Branch | Knob |
|---|---|---|
| `case9-approve-toolmatch` | red cleared at hook speed by tool-name | `EarlyClearApproveByToolName` |
| `case9-approve-resume` | red exited on transcript resume | `ResumeExitStatus` |
| `case10-decline-idle` | interrupt/decline, no teammates | `InterruptExitStatus` |
| `case11-decline-delegating` | interrupt/decline, teammates in flight | `EscWithTeammatesStatus` |
| `case12-hold-bare-result` | red held on a bare/Task PostToolUse | (the missed-RED guard; intentional) |
| `case15-ttl-backstop` | red exited on the unreadable-transcript TTL | `PermissionDecayTTL` |
| `case5-delegating` / `case4-drained` | idle↔delegating on subagent count | `DelegatingEnabled` |
| `resume-activity` / `case6-interrupt` | idle↔working via transcript signal | — |
| `case6-idle-title` | working→idle on a fresh idle-glyph pane title (the silent abort, timing-hazards H9) | `IdleTitleDemotionEnabled` (+ `IdleTitleGrace`, `IdleTitleGlyphs`) |

**3. Retune and verify.** Change the field in `main.go`'s `Tuning`, rebuild, and
add a table row to `TestPermissionExit` (reconciler) or the `rpc`/transcript tests
pinning the new expectation, so the desired behavior is locked and the change can't
silently regress another case. The asymmetric-cost ranking in §4 is the guardrail:
never trade toward **missed RED** (the worst error) to shave latency.
