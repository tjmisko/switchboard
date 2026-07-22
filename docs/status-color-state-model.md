# Status-Color State Model — diagnosis & redesign

> **Goal:** make the chip color as faithful as possible to the agent's *actual*
> state. Two live complaints drive this: (1) RED lingers 10–30 s after it is no
> longer warranted; (2) a session whose main thread is idle but is waiting on
> running subagents/teammates shows ORANGE when it should show GREEN.
>
> This document maps the real state space, assigns a color to every state with
> an explicit error-cost justification, enumerates the transitions and their
> frequencies, and lays out a phased, test-first implementation plan.

---

## 0. Color semantics (the contract we are encoding)

Color is an **action semantic**, not a mechanism. The renderer maps
`AgentInfo.Status` → CSS class (`cmd/switchboard-waybar/main.go:120` `sessionStatus`),
and the bar CSS paints it (canonical example palette: permission `#e06c75` red,
idle `#e5c07b` yellow, working green).

| Color | Status | Meaning to the user | Salience |
|-------|--------|---------------------|----------|
| **RED** | `permission` | "I need you **now**. Work is **blocked** until you act." | highest — grabs the eye |
| **GREEN** | `working` | "Work is happening. **Do nothing.** Don't look here." | calm/positive |
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

---

## 1. The current model (and why it is one-dimensional)

Today the entire state of a session is collapsed onto a single enum,
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

The model has **no representation of subagent activity at all** (confirmed
repo-wide). It cannot express "main idle, teammates working," so it cannot color
it green. And because `permission` is one undifferentiated state, it cannot tell
"resolved-by-approve → resume working" from "resolved-by-decline → your turn."

---

## 2. Root-cause analysis (with measured evidence)

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
`StatusSince` (`transcript.go:292`, `:305`). Claude Code withholds the pending
tool_use's assistant message until it resolves, and the *next* assistant message
arrives only after **model latency (≈5–25 s)**. That model-latency window + the
5 s tick granularity **is** the 10–30 s band.

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
`agent_transcript_path`) — it is just **not wired** to switchboard today (only to
the temporary `.claude/hook-logger.sh`). There is **no** `SubagentStart` hook.

---

## 3. The real state space

The true state of a session is a tuple of orthogonal dimensions; the current
model is a lossy projection of it onto one axis.

| Dim | Name | Values | Source signal |
|-----|------|--------|---------------|
| **M** | main-thread turn state | `working` · `turn-ended` · `interrupted` · `blocked-on-prompt` · `unknown` | hooks + transcript |
| **S** | subagents in flight | `0` · `>0` (count) | main-transcript Task pairing / `SubagentStop` |
| **P** | genuine user-blocking prompt pending | `no` · `yes` | `PermissionRequest` + resolution check |
| **L** | process liveness/control | `live` · `suspended` · `gone` | `/proc` state |
| **F** | confidence/freshness of our info | `fresh-hook` · `inferred` · `stale/unknown` | which path set it + age |

`P` is a refinement of `M=blocked-on-prompt`, kept separate because a prompt can
also be raised by a **subagent** and still requires user action. The color is a
function `color(M, S, P, L, F)`.

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

`if (Main, Subagents, Pending-prompt, Liveness) then COLOR`. Liveness/confidence
modifiers are applied first, then the M×S×P core. "Worst error" names the costly
mistake the rule is protecting against.

| # | Main thread (M) | Subagents (S) | Prompt pending (P) | Liveness (L) | **Color** | Why | Worst error avoided |
|---|---|---|---|---|---|---|---|
| 1 | * | * | * | **gone** | *hidden* | session ended | — |
| 2 | * | * | * | **suspended** | **grey-out** | Ctrl-Z; user paused it, nothing can progress | false-green on a halted proc |
| 3 | working | any | no | live | **GREEN** | work happening | — |
| 4 | turn-ended | **0** | no | live | **ORANGE** | done, your turn, nothing stuck | false-green (#3 cost) |
| 5 | turn-ended | **>0** | no | live | **GREEN** | **teammates working — no action needed** *(fix #2)* | false-orange (#4 cost) |
| 6 | interrupted (Esc) | 0 | no | live | **ORANGE** | you stopped it; your turn | false-green |
| 7 | interrupted (Esc) | >0 | no | live | **GREEN**\* | work still in flight; *(see Q3 — could be orange if Esc means "I want control")* | low either way |
| 8 | **blocked-on-prompt** | any | **yes** | live | **RED** | main stalled; you must act (even if teammates still churn) | **missed-RED (#1, worst)** |
| 9 | blocked, **resolved-by-approve** | any | no | live | **GREEN** | turn resumed → work continues *(go direct, not via orange — P3)* | false-orange + #2-latency |
| 10 | blocked, **resolved-by-decline/interrupt** | 0 | no | live | **ORANGE** | you answered/declined; your turn | persistent false-red (#2) |
| 11 | blocked, **resolved-by-decline** | >0 | no | live | **GREEN** | you declined but teammates still working | false-orange |
| 12 | blocked, **bare unrelated tool_result lands** (subagent/sibling) | any | **still yes** | live | **RED** (hold) | not resolution — keep nagging | **missed-RED (#1)** |
| 13 | unknown | 0 | unknown | live | **GRAY** | no signal yet / just rehydrated | confident wrong guess |
| 14 | unknown | >0 | unknown | live | **GREEN** | we can see in-flight Tasks even with no main signal | false-orange |
| 15 | blocked, transcript **unreadable** ≥ TTL | any | unknown | live | **ORANGE** (decay) | last-resort backstop; observed 0× | nagging forever |
| 16 | a **subagent** raises a prompt | any | **yes** | live | **RED** | surfaces to user; needs action | missed-RED |

\* Case 7 is the one genuine judgment call — see Open Questions Q3.

The rows that change today's behavior: **5, 9, 11, 14** (subagent-awareness and
direct red→green), and the *latency* of **8→9/10** (earlier resolution).

---

## 6. Target state machine, transitions & frequencies

States: `WORKING(green)`, `IDLE(orange)`, `DELEGATING(green; idle-but-S>0)`,
`PERMISSION(red)`, `UNKNOWN(gray)`, plus the `SUSPENDED`/`GONE` overlays.
`DELEGATING` need not be a stored enum value — it can be `Status=idle` rendered
green when `InFlightSubagents>0` — but naming it clarifies the transitions.

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

## 7. Implementation plan (test-first, pin-then-fix per §0.9 convention)

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
- **Q3 — Case 7 (Esc with teammates still in flight):** GREEN (follow
  "work-happening") or ORANGE (Esc signals "I want control")? Low frequency,
  low cost either way. Recommend GREEN for consistency with P4.
- **Q4 — Adaptive tick:** shorten the reconcile interval while any chip is red?
  Only worth it if A1–A3 leave a felt tail.

---

## 9. Implementation status (shipped)

Phases A and B landed together. Decisions taken — all wired as `statustune.Tuning`
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

## 10. Operating it: diagnosing a wrong color, then retuning

This is the loop for "the chip was X, it should have been Y."

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
