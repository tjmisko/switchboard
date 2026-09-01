# Why `$mod+A` doesn't move after you answer a question

> **Symptom (2026-08-31).** A chip goes red because the agent wants to ask
> something. `$mod+A` focuses the window. You answer. You press `$mod+A` again to
> leave — and nothing moves, for 10–30 seconds.
>
> **Verdict.** Two independent defects, either of which alone produces "I stay in
> place." Only one of them is a latency problem. The other is a navigation
> semantic that will still bite at zero latency.

---

## 0. Executive summary

| # | Defect | Where | Felt cost | Fix cost |
|---|--------|-------|-----------|----------|
| **D1** | `attention` re-focuses the session you are already in when it is the only member of the top tier | `cmd/switchboard-ctl/main.go:525` | **every press**, even when colors are perfect | small |
| **D2** | `AskUserQuestion` can never take the hook fast path — its `tool_input` is rewritten with the user's answers before `PostToolUse`, so the correlator hash mismatches | `internal/provider/claude/observer.go:571` | **median 17 s**, mean 45 s of stale red | medium |
| **D3** | The fallback resolution signal is "the next assistant message," i.e. model latency | `internal/transcript/transcript.go:495` | is *what* D2 falls back to | medium |
| **D4** | Since `784e3a8` (2026-08-21) the graph is the sole authority for Claude; the whole legacy decision layer — rules, `status: pid=` log, `diagnose`, every `Tuning` knob, and the H9 idle-title recovery — is dormant and silent | `cmd/switchboard/main.go:1107,1339`; `agent_observation.go:568` | you cannot diagnose D2/D3 from any log | small |
| **D5** | `Focused` is window-level, not pane-level, so any "skip the focused session" fix would hide a red in a sibling wezterm pane | `cmd/switchboard/main.go:1039` | a **missed RED** — the worst error class | small |

The recommended order is **D1 → D4 → D2/D3 → D5-as-a-precondition-of-D1**.
D1 alone fixes the reported symptom. D4 is what lets us verify D2/D3.

---

## 1. Measurement

Source: `~/.local/state/switchboard/history/*.jsonl` (`transition` and
`agent_state` events) correlated against the Claude transcripts, for every
`AskUserQuestion` / `ExitPlanMode` prompt since 2026-08-26.

`answer→clear` is the interval from the `tool_result` for the question's own
`tool_use_id` (the instant the answer was submitted) to the daemon's
`permission→working` transition.

```
ask_at (UTC)         tool             red_total  user_think  ANSWER→CLEAR
2026-08-27T00:37:46  AskUserQuestion      138.8       128.9          10.0
2026-08-27T01:11:35  AskUserQuestion      114.2       104.6           9.6
2026-08-27T01:18:56  AskUserQuestion      106.8        74.0          32.9
2026-08-27T01:26:35  AskUserQuestion       79.3        45.4          34.0
2026-08-27T01:37:55  ExitPlanMode         214.8       199.1          15.9
2026-08-28T01:47:21  AskUserQuestion       30.1        25.9           4.2
2026-08-28T01:49:55  AskUserQuestion      429.6        14.9         414.7
2026-08-28T01:50:19  AskUserQuestion      429.6       282.0         123.7
2026-08-28T01:55:18  AskUserQuestion      429.6        56.3          51.1
2026-08-28T01:56:28  AskUserQuestion      429.6        18.6          18.1
2026-08-28T03:44:11  AskUserQuestion       80.3        76.8           3.5
2026-08-28T04:36:01  AskUserQuestion      190.3       180.0          10.3
2026-08-28T05:03:58  AskUserQuestion      297.6       215.6          82.1
2026-08-28T05:59:30  AskUserQuestion      308.3       300.3           8.0
2026-08-28T06:36:21  AskUserQuestion      213.0       209.7           3.4
2026-08-28T07:25:34  AskUserQuestion       25.2        24.6           0.7
2026-08-28T20:33:28  AskUserQuestion       97.1        88.1           9.0
2026-08-31T16:09:32  AskUserQuestion      277.5       263.3          14.3
2026-08-31T16:15:23  AskUserQuestion       98.3        36.4          61.9
2026-08-31T16:20:04  AskUserQuestion       94.2        74.3          19.9
2026-08-31T17:31:16  AskUserQuestion       37.8         6.1          31.8
2026-08-31T18:26:55  AskUserQuestion      263.2       226.6          36.6

n=22   median 17.0 s   mean 45.3 s   max 414.7 s
```

**The reported "10 to 30 seconds" is real and reproducible.** 15 of 22 land in
the 3–40 s band; the median is 17 s. (Rows sharing a `red_total` are one red
episode spanning several consecutive questions, so their `user_think` is
inflated; the `answer→clear` column is still exact.)

Contrast the same measure for ordinary tool approvals in the same window — Bash,
Edit, ListAgents — where `answer→clear` is **0.0 s**. The defect is specific to
the prompt type that *is* "the agent wants to ask you something."

### 1.1 The daemon says who cleared it

Every one of these clears is recorded with `source: claude_transcript` — the
5 s `Observe` poll — and never `source: hook`:

```
16:20:04.879  TRANSITION  working -> permission     agent_graph_authority
16:21:39.079  agent_state active/user_input -> active/none   src=claude_transcript
16:21:39.079  TRANSITION  permission -> working     agent_graph_authority
```

Across 2026-08-26 → 08-31, applied Claude observations by source:

| source | count | share |
|---|---|---|
| `claude_transcript` | 3092 | **98.2 %** |
| `hook` | 73 | 1.8 % |
| `restored_last_known` | 5 | 0.2 % |

### 1.2 The old journal names the exact rule

The pre-`784e3a8` decision log still in the journal is unambiguous. Of the
43 `case12-hold-input-mismatch` decisions ever recorded, **43 are
`AskUserQuestion`** — 100 %:

```
status: pid=80755 session=cc7baf6b permission==permission rule=case12-hold-input-mismatch
  reason="main completed AskUserQuestion but input ffd9ca593ccf2139 != the pending
  ca01c69e9a3d257a, and its transcript shows no resume" [S=0 pending="AskUserQuestion" age=3m13s]
```

Note `S=0`: no teammates. This is not the subagent-collision guard. It is the
input-hash correlator failing on the main thread's own prompt.

---

## 2. Mechanism

### 2.1 D2 — the correlator cannot match `AskUserQuestion`

`switchboard-ctl` hashes the hook payload's whole `tool_input`
(`cmd/switchboard-ctl/main.go:776` `hashToolInput`) and forwards the digest.
The observer's fast path requires that digest to match
(`internal/provider/claude/observer.go:567`):

```go
func promptMatches(pending PendingPrompt, signal HookSignal) bool {
	if signal.Event != "PostToolUse" || signal.ToolName == "" || signal.ToolName != pending.Tool {
		return false
	}
	if pending.InputHash != "" && signal.ToolInputHash != "" && pending.InputHash != signal.ToolInputHash {
		return false
	}
	return true
}
```

For `AskUserQuestion` the two payloads are **structurally guaranteed to
differ**. `PermissionRequest` reports the input before the decision —
`{questions: […]}`. The permission component then writes the user's selections
into the call, so `PostToolUse` reports `{questions: […], answers: {…},
annotations: {…}}`. Different bytes, different digest, no match, no fast clear.

This is a known-and-documented property of the payload
(`docs/claude-code-hook-schema.md:129` — "`tool_input` is NOT stable across the
two events"), and the design deliberately falls through rather than latching red
on a mismatch. That fall-through is the right call. The problem is what it falls
through *to*.

There is a second floor on the same branch (`observer.go:318`):

```go
if pending, owns := rs.pending[writer]; owns && promptMatches(pending, signal) &&
	!(writer == "" && rs.fanout.InFlight > 0) {
```

`!(writer == "" && rs.fanout.InFlight > 0)` disables the main thread's fast path
entirely whenever *any* subagent is running — correct today, because a
`tool_name`+hash match names a *call*, not a *writer*, and fanned-out teammates
run byte-identical calls. It is unnecessary once the correlator names a writer.

### 2.2 D3 — the fallback is model latency

With the fast path refused, the prompt survives in `rs.pending` and can only be
retired by `resolvePending` on the next `Observe`
(`internal/provider/claude/observer.go:605`), which asks
`transcript.ResolveKind`. That classifier
(`internal/transcript/transcript.go:495`) accepts exactly two resolutions:

```go
func resolutionKindOf(e entry) ResolutionKind {
	if e.Message.Role == "assistant" { return ResolutionResumed }
	if classify(e) == SignalInterrupt { return ResolutionInterrupted }
	return ResolutionNone
}
```

A user `tool_result` — the entry written the instant you hit enter — is
deliberately `ResolutionNone`, because a *sibling's* result would false-clear a
genuinely pending prompt. So the only proof of "approved" is **the next
assistant message**, which arrives after model latency: 5–25 s. Add up to 5 s of
`Observe` tick. That is the measured 10–30 s band, exactly.

The remaining `resolvePending` branches are backstops, not fast paths:
`BlockedByPendingTool` only *holds*; `writerQuiescentPastCap` needs 30 minutes;
the `PermissionDecayTTL` branch needs an *unreadable* transcript. None of them
fires here.

### 2.3 D4 — the diagnostic layer is dormant, so none of this is visible

`784e3a8` (2026-08-21) made the provider graph the authority. Three consequences
that were not intended as behavior changes:

**Every legacy self-heal path is gated off for Claude.** Both
`selfHealStaleAttention` (`cmd/switchboard/main.go:1107`) and
`selfHealStuckStatus` (`:1339`) open with `if c == nil || sess.AgentGraph != nil
{ continue }`, and a Claude session always has a graph. Confirmed in the data —
`case*` rules stop dead on 2026-08-21:

```
2026-08-21  {case9-approve-transcript:1, case5-delegating:21, resume-activity:4,
             case9-approve-toolmatch:1, case4-drained:1, case6-interrupt:1,
             agent_graph_authority:2}
2026-08-23  {agent_graph_authority:4}
2026-08-26  {agent_graph_authority:293}
2026-08-31  {agent_graph_authority:77}
```

This silently retired the **H9 silent-abort recovery** (`case6-idle-title`,
`docs/timing-hazards.md`): a prompt interrupted before its first token fires no
`Stop` hook and writes no interrupt marker, and the pane-title demotion was the
only recovery. `titleShowsIdleGlyph` is now reachable only from dead code. The
graph path has no equivalent — `reconcileRootRuntime` reads the transcript only.
Last `case6-idle-title` in history: 2026-08-21.

**The decision log is gone.** `rpc.handleHook` returns at its first line when an
agent hook handler is installed (`internal/rpc/rpc.go:684`), which the daemon
always does. Zero `status: pid=` lines in a week of journal;
`switchboard-ctl diagnose` answers *"no status-decision lines matched"* for any
window. Every `Tuning` knob in the §10 operating table
(`docs/status-color-state-model.md`) — `EarlyClearApproveByToolName`,
`ResumeExitStatus`, `InterruptExitStatus`, `EscWithTeammatesStatus`,
`DelegatingEnabled`, `IdleTitleDemotionEnabled` — now has no live reader on the
Claude path. Only `TailBytes`, `PermissionDecayTTL` and `PendingWriterStaleCap`
survive, read by `resolvePending`.

**The replacement rules are computed and thrown away.**
`applyHookLocked` returns a precise rule for every hook edge —
`writer_tool_match_cleared`, `writer_prompt_held`, `non_owner_prompt_held` — and
`HandleHook` (`cmd/switchboard/agent_observation.go:759`) discards
`result.Rule`. `resolvePending` likewise computes a `Reason`
(`writer_resumed`, `child_terminal`, `writer_stale_backstop`,
`main_unreadable_ttl`) that never leaves the function. Every transition is
recorded as the uninformative `Rule: "agent_graph_authority"`
(`agent_observation.go:528`).

**Aggravating, though not the cause of the felt latency:** the authority gate
(`agent_observation.go:568`) drops hook observations outright while a transcript
graph is fresh —

```go
currentRank := sourceRank(current.Source)          // claude_transcript = 4
candidateRank := sourceRank(observation.Source)    // hook              = 3
if currentFresh && currentRank > candidateRank { return false }
```

`Observe` runs every 5 s with a 15 s freshness window, so `currentFresh` is
effectively always true and no Claude hook edge is ever published as itself. The
state still surfaces quickly, because `ApplyHook` signals an immediate re-`Observe`
(`observer.go:267`), but the published `source` is then always
`claude_transcript` — which is why §1.1 shows 98 % transcript and why
hook-vs-transcript attribution is impossible from the record.

### 2.4 D1 — `attention` re-focuses where you already are

This one has nothing to do with color latency and will outlive every fix above.

```go
func pickAttentionExact(sessions []state.Session, focused *state.Session) *state.Session {
	tier := topAttentionTier(sessions)
	if len(tier) == 0 { return nil }
	for i, session := range tier {
		if focused != nil && sameExactSession(*session, *focused) {
			return tier[(i+1)%len(tier)]     // ← single-member tier: returns ITSELF
		}
	}
	return tier[0]
}
```

`cmd/switchboard-ctl/main.go:525`. When the top tier has one member and it is the
focused session, `(0+1)%1 == 0` — the cycle wraps onto itself and
`cmdAttention` calls `focusSession` on the window you are already in.

`topAttentionTier` (`:580`) makes this common. It returns the *first non-empty*
of `[permission, idle, working]`, and the green tier is suppressed entirely
unless **every** navigable session is green. So after a red clears you land in
one of three no-op states:

1. still red, only red → self-focus (this is the reported case);
2. now green, exactly one orange elsewhere and it is you → self-focus;
3. now green, someone else is grey (a Codex root mid-`snapshot_root_not_loaded`
   reads `unknown`) → tier is `nil` → **silent no-op**.

State 3 is live right now. This machine currently holds four navigable
sessions — two `working`, one `delegating`, one `idle` — and the `idle` one is
the focused window. `$mod+A` at this moment re-focuses the window it is pressed
in, with no red anywhere.

### 2.5 D5 — `Focused` is a window, not a pane

`applyFocus` (`cmd/switchboard/main.go:1039`) sets
`focused = sess.Hyprland.Address == activeAddr`. Two Claude sessions in two
wezterm panes of one window therefore **both** read `Focused: true`. Any fix to
D1 that says "skip the focused session" would, in a split window, make a red in
the inactive pane unreachable by `$mod+A` — a *missed RED*, the most expensive
error in the §4 cost ranking.

The data is already available and already parsed: `wezterm cli list --format
json` returns `is_active` per pane and `internal/wezterm/wezterm.go:34,45`
decodes it. It is simply not carried onto `state.WeztermInfo` or into
`state.json`.

---

## 3. Recommended next steps

Ordered by *felt improvement per unit of risk*. Every item is guarded by the
§4 cost ranking in `docs/status-color-state-model.md`: **never trade toward a
missed RED to shave latency.**

### R1 — Make `$mod+A` always move. *(no accuracy cost at all)* — **superseded by [attention-ring-plan.md](attention-ring-plan.md)**

The insight: **red means "needs you, and you are not there." Once you are in the
window, the chip has already done its job.** Focus is ground truth from the WM,
so excluding the session you are looking at from the *navigation target set*
costs zero accuracy — the color, the tooltip and the TUI stay exactly as
truthful as they are today. It decouples the navigation predicate from the
color, which is the actual conflation behind the complaint.

The sketch below was "first tier containing a non-focused member." It is
superseded by the **bounded ring**: walk the most urgent populated tier and at
most the adjacent color above it, skipping every focused session and wrapping
inside that bound. Thus red permits orange but excludes green; repeated presses
cannot use orange as a staircase to green. The ring also fixes the `delegating`
denominator bug, which was a second, independent dead key. See
[attention-ring-plan.md](attention-ring-plan.md) for the current rule and its
regression matrix.

Superseded sketch, kept for the record:

- ~~Rework `topAttentionTier` to return the tiers in priority order rather than
  the single top one, and have `pickAttentionExact` select the **first tier
  containing a member other than the focused session**. Within that tier keep
  today's wrap-around cycle.~~
- Drop the "all sessions green" precondition on the working tier, so a grey
  Codex root can no longer strand the keybind (state 3 above). The ring drops it
  unconditionally: grey is simply not a member.
- ~~**Precondition: R2.**~~ See R2 — demoted to a quality fix.
- ~~Keep `pickAttention` (the PID-keyed variant, `:542`) and its tests in step~~ —
  the ring deletes it, along with `pickAttentionExact`, `topAttentionTier` and
  `sameExactSession`.
- **DoD:** with N≥2 navigable sessions, `$mod+A` never focuses the
  already-focused session. Table test over: one-red-and-it-is-you; one-orange-
  and-it-is-you; all-green-plus-one-grey; two reds (pops out to orange on the
  second); single session total (still a no-op).

### R2 — Publish pane-accurate focus. *(quality fix, no longer a precondition)*

Carry `is_active` from `internal/wezterm` onto `state.WeztermInfo`, and make
`applyFocus` require *window active* **and** *pane active* before setting
`Focused`. Sessions without a terminal backend fall back to window-level, as
today.

Demoted from precondition: the ring skips *every* session reporting `Focused`,
not just the one it started from, so a window-level flag shared by two panes
costs a longer hop rather than a dead key or a missed RED. Pane accuracy still
buys the shorter hop and a correct "which pane am I in" answer everywhere else.

- **DoD:** two Claude sessions in one split wezterm window; exactly one reads
  `focused: true` in `state.json`, and it tracks the active pane. Regression
  test: a red in the inactive pane of the focused window is reachable in one
  press rather than being skipped with its sibling.

### R3 — Restore the decision log on the graph path. *(unblocks verifying R4/R5)*

We currently cannot answer "why did that chip hold?" from any record. Fix that
before changing the state machine, not after.

- Thread `HookResult.Rule` through `HandleHook`
  (`agent_observation.go:759`) and `promptResolution.Reason` out of
  `resolvePending` into `applyObservation`, and record the real rule on
  `history.EventTransition` instead of the blanket `agent_graph_authority`.
- Re-emit the `status: pid=` line from the graph path.
  `statustune.Decision.Log()` already has the shape; extend `ruleKnobs`
  (`internal/statustune/knobs.go`) with the observer's rule ids so
  `switchboard-ctl diagnose` keeps answering "what do I change?"
  `TestRuleKnobCoverage` will enforce exhaustiveness.
- Correct `docs/status-color-state-model.md` §9/§10: the shipped-status table
  and the whole rule→knob operating table describe a layer that no longer runs
  for Claude.
- **DoD:** `switchboard-ctl diagnose --since "1 hour ago" red` returns real
  lines again; a held red names the rule that held it.

### R4 — Correlate the prompt by `tool_use_id`. *(kills D2 outright)*

`tool_use_id` is an exact call identity that no teammate and no sibling can
satisfy, and — unlike the input hash — it is **immune to input rewriting**.
`PostToolUse` carries it (`docs/claude-code-hook-schema.md:106`).
`PermissionRequest` does not, but we do not need it from the hook:

- At `PermissionRequest`, resolve the pending call's id from the *raising
  writer's own* transcript tail — the newest `tool_use` with
  `name == tool_name` that has no matching `tool_result`. `internal/transcript`
  already parses `name` / `id` / `tool_use_id` on blocks (added for
  `InFlightTasks`), so this is a small addition beside `BlockedByPendingTool`,
  which already walks exactly this structure.
- Forward `tool_use_id` from `cmdHook` into `rpc.Request`; store it on
  `PendingPrompt` alongside `InputHash`.
- `promptMatches`: an id match ⇒ clear at hook speed. Id unavailable on either
  side ⇒ fall back to today's `(agent_id, tool_name, hash)` rule unchanged.
- Because an id match names a **writer and a call**, the
  `!(writer == "" && rs.fanout.InFlight > 0)` floor (`observer.go:318`) may be
  lifted *for id-matched clears only*. This is a strict strengthening of the
  missed-RED guard, not a loosening — keep the floor for every hash-only match.
- **DoD:** `AskUserQuestion` answered ⇒ red clears at hook speed (< 500 ms), with
  four teammates in flight. A teammate's `AskUserQuestion` completing while the
  main thread's prompt is pending still holds red (pin the 2026-08-05
  oscillation regression). The `PermissionRequest`-side lookup returning nothing
  degrades to today's behavior, not to a false clear.

### R5 — Accept an id-matched `tool_result` as resolution. *(kills D3; also finally lands A3)*

Closes the loop when the hook is missed, and covers the decline path that
`docs/status-color-state-model.md` §7 has carried as deferred since Phase A.

- With the pending `tool_use_id` from R4, teach `resolutionKindOf`
  (`internal/transcript/transcript.go:495`) to accept a user `tool_result`
  **whose `tool_use_id` matches the pending prompt** as decisive — approve when
  clean, decline when `is_error` / `"User rejected tool use"`. Route the id in
  via `ResolveKind`'s signature; an absent id keeps today's assistant-message-only
  behavior verbatim.
- This is the signal that lands *the instant the user hits enter*, with zero
  model latency. It is strictly narrower than the rule that was rejected in
  §2.1 of the model doc — that one accepted *any* bare `tool_result`, which is
  exactly the sibling false-clear. An id match is not a bare result.
- **DoD:** with the `PostToolUse` hook disabled, an answered question still
  clears within one `Observe` tick. A sibling `tool_result` during a pending
  prompt still holds red.

### R6 — Adaptive tick while any root has attention. *(the residual tail; Q4, still open)*

After R4/R5 the remaining latency is the ≤5 s `Observe` granularity on paths
that miss the hook. Run `Observe` at 1 s for roots whose graph summary has
`attention != none`, reverting on clear. Cost is bounded to red roots while they
are red. Low priority — measure after R4/R5 before spending it.

### R7 — Recover the H9 silent-abort demotion.

`case6-idle-title` has been unreachable since 2026-08-21 (§2.3) and nothing in
the graph path replaces it. Either port the pane-title demotion into
`reconcileRootRuntime` as a third runtime signal, or re-enable the legacy
reconciler for that one rule. Not part of this complaint, but it is a live green
chip that should be orange, and it fell out of the same change.

---

## 4. What this does *not* recommend

- **Loosening the hold gate on a hash mismatch.** A mismatch genuinely proves
  nothing (`docs/claude-code-hook-schema.md:145`) — `updatedInput` rewrites
  `command:` on Bash approvals too. R4 replaces the correlator instead of
  weakening it.
- **A subset-stable hash** (hashing only the keys present at
  `PermissionRequest`). It would rescue `AskUserQuestion`, whose PostToolUse
  input is a strict superset, but not the in-place rewrites, and it weakens the
  sibling test for every tool. Only worth it as a stopgap if R4 slips.
- **Shortening `PermissionDecayTTL`.** It has never fired on a readable
  transcript — 0 occurrences across the whole history. It is a backstop for
  unreadable files, not a latency knob.
- **Any change that makes red leave on weaker evidence.** The asymmetric cost
  ranking stands: a missed RED is silent and costs minutes to hours; a stale RED
  is loud and costs seconds. R1 improves the felt experience without touching
  that trade at all, which is why it is first.
