# Plan — subagent-raised permission holds RED

**Goal.** A permission prompt raised by *any* writer in a session turns the chip
RED and keeps it red until **that writer's** prompt is proven resolved — even
while the main thread is actively working and other teammates are churning.

**Why.** A pending prompt is the only *actionable* signal a chip can carry: a
quick decision unblocks real work. Work-is-happening is not actionable. So a
prompt anywhere outranks work everywhere (`status-color-state-model.md` §5, the
fold). Today the chip renders green/orange and oscillates instead —
[subagent-permission-oscillation.md](subagent-permission-oscillation.md).

**Scope.** Claude Code sessions only (codex records no approvals and is already
exempt from the hold gate).

---

## 1. Root cause, stated once

Switchboard models a session as **one status-bearing thread**. It is **1 + N
concurrent writers** — main thread plus each in-flight subagent — that share a
pid, a chip, and a `transcript_path`, write to different files, and can each
block on a prompt independently (`status-color-state-model.md` §3.1).

Every defect in the incident is a consequence of that single flattening:

| # | Defect | Consequence of |
|---|---|---|
| 1 | red cleared on a teammate's same-named tool | can't tell writers apart |
| 2 | teammate hooks paint the parent chip green | one chip, one pid |
| 3 | `StatusSince` back-dated to a quiescent main transcript | one transcript assumed |
| 4 | resolution checked against the main transcript | one transcript assumed |
| 5 | hold gate covers only `PostToolUse` | prompt state not owned by a writer |

**The fix is one idea:** make the prompt's *owner* part of the state, and route
every question about that prompt to its owner.

```
P: map[agentID] → PendingPrompt{ tool, inputHash, since }      // "" key = main thread
```

- **entry** — `PermissionRequest` adds `P[req.AgentID]`
- **exit** — `P[a]` is removed **only** by evidence from writer `a`
- **color** — `len(P) > 0 → RED`, ahead of every other rule

---

## 2. Signals available (verified)

From [claude-code-hook-schema.md](claude-code-hook-schema.md), read off the
emitters in the 2.1.222 bundle:

- `agent_id` is on **every** hook event, set iff the hook fired inside a
  subagent. It is the documented main-vs-subagent discriminator, it equals the
  `subagents/agent-<id>` stem, and **switchboard already receives it** as
  `rpc.Request.AgentID` — unread outside the fanout branch.
- `tool_input` is on `PermissionRequest` **and** `PostToolUse`.
- `tool_use_id` is on `PostToolUse` but **not** `PermissionRequest`, so exact
  tool-use correlation is impossible without also wiring `PreToolUse` (a hook on
  every tool call in every session — rejected on cost, see §6 R2).
- `transcript_path` is always the **parent's**, even from a subagent; a
  subagent's own writes live at `subagents/agent-<id>.jsonl`.

Correlator strength: `agent_id` (which writer) ≫ `tool_input` (which call) ≫
`tool_name` (which kind — collides constantly, and is what we use today).

---

## 3. Design

### 3.1 State

```go
// AgentInfo
type PendingPrompt struct {
    Tool      string    // tool_name from the PermissionRequest
    InputHash string    // hash of tool_input — the secondary correlator
    Since     time.Time // for the per-prompt TTL/liveness backstop (case 19)
}
Pending map[string]PendingPrompt `json:"-"`   // key: agent_id; "" = main thread
```

In-memory as written here; T12 (§9) projects its **keys only** onto one additive
wire field so prompt ownership survives a daemon restart. `Tool`/`InputHash`/
`Since` stay off the wire.

`PendingTool` is retained as a derived value for the decision log and the
existing `pending=` forensic field (report the main thread's if present, else any
one, plus a count).

### 3.2 Entry

`PermissionRequest` → `Pending[req.AgentID] = {ToolName, hash(ToolInput), now}`
and chip → `permission`. Already fires correctly for subagent prompts; the only
change is *where the record goes*.

### 3.3 Exit — the routing rule

`P[a]` is removed only by evidence from writer `a`:

| evidence | applies to | resolves |
|---|---|---|
| `PostToolUse` with `agent_id == a` ∧ `tool_name` ∧ `inputHash` match | `P[a]` | approved, at hook speed |
| activity in `a`'s transcript dated after `Since` | `P[a]` | approved (fallback) |
| interrupt notice in `a`'s transcript after `Since` | `P[a]` | declined |
| `a` gone/quiescent ≥ cap (case 19) | `P[a]` | unanswerable — drop, don't latch |

`a`'s transcript is the main `.jsonl` when `a == ""`, else
`subagents/agent-<a>.jsonl` derived as the sibling of the stored transcript path
(never from cwd — `subagent-fanout-detection-plan.md` G10).

Chip leaves `permission` only when `len(Pending) == 0`; the exit color is chosen
by the *last* prompt's resolution kind, per the existing P3 rule.

### 3.4 Hold, generalized

The gate moves from "PostToolUse only" to: **while `len(Pending) > 0`, no hook
event may move the chip off `permission` except through the routing rule above.**
That covers the `Stop` / `UserPromptSubmit` / `SessionStart` holes (defect 5).

`UserPromptSubmit` is the one deliberate exception worth considering: the user
typing a new prompt is strong evidence they are at the keyboard and the prompt
is gone. Recommend **not** exempting it initially — a queued message during a
pending prompt is common — and revisit if it produces a felt stale red (Q6).

### 3.5 Rendering

No renderer change is required. `permission` is already inert to
`case5-delegating` and `case6-idle-title` (`cmd/switchboard/main.go:775`/`806`/
`816`), so a correctly-held red already outranks green teammates and already
survives the flap. Naming *which* teammate is blocked is a follow-on (T11).

---

## 4. Phases

Ordered so the worst error (missed RED) is closed first and each phase is
independently shippable.

### Phase 0 — stop the bleeding (no new data, no schema dependency)

Closes the incident. Trades back some approve-path latency, but only in sessions
with teammates in flight, and only until Phase 2 lands. Per the §4 cost ranking
a slow-but-correct clear is far cheaper than a missed RED.

- **T1 — Log `agent_id` on `PermissionRequest`/`PostToolUse`.** The one
  empirical gap in the schema work: confirm it is non-empty end-to-end for a
  real subagent prompt and empty for a main-thread one. *Blocks T5–T8; blocks
  nothing in Phase 0/1.* **DoD:** a journal line from a live subagent prompt
  showing a non-empty `agent_id`, pasted into `claude-code-hook-schema.md` §5.
- **T2 — Never clear red on `tool_name` alone while `InFlightSubagents > 0`.**
  One condition in `clearsPermission` (`rpc.go:505`). **DoD:** the 12:38:21 edge
  holds red; a teammate `Bash` during a pending `Bash` prompt cannot clear.
- **T3 — Extend the hold to all hook events** (defect 5). `Stop` /
  `UserPromptSubmit` / `SessionStart` must not silently exit `permission`.
  **DoD:** main-thread `Stop` while a prompt is pending leaves the chip red.

### Phase 1 — stop the flap (independent of the schema)

Worth doing regardless: the oscillation exists with no permission involved.

- **T4 — Don't back-date `StatusSince` past the previous one** (defect 3).
  Clamp to `max(anchor, prevStatusSince)` in `AnchorSince` /
  its call site (`transcript.go:716`, `rpc.go:454`). Preserves H1's intent (an
  anchor slightly behind `now`) while making an arbitrarily stale anchor
  impossible. **DoD:** `IdleTitleGrace` actually delays a demotion by 15 s even
  when the main transcript's newest entry is minutes old.

### Phase 2 — writer identity (depends on T1)

- **T5 — `Pending` becomes a map keyed by `agent_id`** (§3.1). Includes clearing
  it on `SessionStart` / session-id rotation.
- **T6 — Forward `tool_input`** as a hash from `switchboard-ctl`
  (`main.go:388`) through `rpc.Request`. Cheap; hash so no payload data is
  retained.
- **T7 — `clearsPermission` requires `(agent_id, tool_name, inputHash)`.**
  Replaces T2's blunt guard with the precise one, restoring hook-speed clearing
  for the *correct* tool. **DoD:** same-agent matching `PostToolUse` clears at
  hook speed; teammate's does not; Phase A's latency win is preserved.

### Phase 3 — writer-routed resolution (depends on T5)

- **T8 — `transcript.SubagentPath(mainPath, agentID)`** helper; reuse the
  derivation already in `subagents.go:110`.
- **T9 — Route `selfHealStaleAttention` per prompt** (defect 4): resolve each
  `P[a]` against `a`'s own transcript. **DoD:** a working main thread does not
  clear a teammate's pending prompt.
- **T10 — Per-prompt liveness backstop** (case 19): drop `P[a]` when `a` is gone
  or its jsonl has been quiescent past a hard cap, so a crashed teammate cannot
  latch red forever. Reuse the Observer's existing quiescence/cap logic.

### Phase 4 — fidelity

- **T11 — Surface *which* writer is blocked** in the tooltip/label
  ("escalate-cleanup needs approval"). The information is now in `Pending`.
- **T12 — Persist prompt *ownership* across a daemon restart. RESOLVED — see
  §9.** Add one additive wire field (`claude.pending_writers`), rebuild `Pending`
  from it in `dropStaleSessions`, and falsify entries at hydrate against each
  subagent's own jsonl. Do **not** persist `Tool`/`InputHash`/`Since`; do **not**
  re-derive `Pending` from transcripts as a *source*. §9 carries the reasoning,
  the migration path, and the four implementation traps. *Depends on T5 (the map
  is what gets projected) and wants T10 (the cap bounds a stale hydrated red).*

---

## 5. Tests

Written alongside, per repo convention. Naming: "should … when …".

`internal/rpc` — the hold gate:
- should hold red when a `PostToolUse` matches the pending tool **name** but
  carries a different `agent_id`, with `S > 0` *(the 12:38:21 edge)*
- should hold red when `S > 0` and the payload carries **no** `agent_id`
  *(T2's fallback path)*
- should hold red on main-thread `Stop` while a subagent prompt is pending
  *(defect 5)*
- should clear red at hook speed when the `PostToolUse` carries the **same**
  `agent_id` and matching input *(Phase A's win survives)*
- should clear red for a main-thread prompt with empty `agent_id` and `S == 0`
  *(common case unchanged)*
- should keep red when one of two pending prompts resolves *(case 18)*

`cmd/switchboard` — the reconciler:
- should not demote a working chip via `case6-idle-title` within
  `IdleTitleGrace` of a re-green **even when the main transcript's newest entry
  is minutes old** *(T4, defect 3)*
- should not clear a subagent's pending prompt when the **main** transcript
  shows fresh assistant activity *(T9, defect 4)*
- should drop a pending prompt whose subagent is gone past the cap *(T10)*

`internal/transcript`:
- `SubagentPath` should resolve worktree/renamed sessions correctly *(T8, G10)*

Regression fixture: the 2026-08-05 hook sequence, replayable — the log lines in
the incident doc §2.2 are sufficient to reconstruct it.

---

## 6. Risks

- **R1 — `agent_id` empty in practice.** The emitter reads `r?.agentId` from the
  `toolUseContext`; if a path exists where a subagent tool call carries no
  context, `agent_id` is empty and T7 would fail *open*. Mitigation: T2 stays in
  place as the floor (empty `agent_id` + `S > 0` ⇒ never clear on name alone).
  T1 exists to find this before it matters.
- **R2 — the `PreToolUse` temptation.** It would give exact `tool_use_id`
  correlation, but fires on **every tool call in every session**. Given this
  repo's recent work on daemon lock contention and subprocess cost, rejected.
  Revisit only if `(agent_id, tool_name, inputHash)` proves insufficient.
- **R3 — stale red from a lost resolution.** Broadening the hold (T3) increases
  exposure to a prompt we never see resolved. T10's per-prompt backstop is the
  mitigation and should not lag Phase 3.
- **R4 — `tool_input` size/PII.** Hash it at the ctl edge; never forward or
  persist raw input.
- **R5 — nested subagents.** `spawnDepth ≥ 2` teammates are excluded from the
  in-flight count today. A grandchild *can* raise a prompt; its `agent_id` still
  keys `Pending` correctly, but its transcript is only closable by quiescence
  (`subagent-fanout-detection-plan.md`). Acceptable: red is keyed by
  `agent_id`, not by depth.

---

## 7. Open questions

- **Q5 — exit color when a subagent prompt is approved.** Main thread may be in
  any state. Recommend: re-fold from scratch (§5) rather than reusing the
  main-thread P3 rule — with the teammate resumed and the main thread working,
  the fold yields GREEN anyway.
- **Q6 — should `UserPromptSubmit` clear a pending prompt?** §3.4. Recommend no,
  initially.
- **Q7 — should `Pending` survive a daemon restart?** **Resolved in §9:** the
  *key set* is persisted, the correlator values are not, and the transcripts may
  only falsify entries at hydrate, never manufacture them. The question as posed
  ("persist vs. drop") contained a false premise — `Status` is already on the
  wire, so a restart never dropped the red; it dropped the red's *owner*. §9.1.
- **Q2 (reopened)** — resolved by this plan in favor of
  `(agent_id, tool_name, tool_input)` over both bare `tool_name` (insufficient)
  and `PreToolUse`/`tool_use_id` (too costly).

---

## 8. Sequencing

```
T1 ──────────────────────────────► T5 ─► T7      (needs live agent_id)
                                    └─► T8 ─► T9 ─► T10 ─► T11
T2, T3  ── ship first, independent
T4      ── independent, fixes the flap generally
T6      ── independent, feeds T7
T12     ── decided (§9); implement after T5, ship with or after T10
```

Phase 0 (T2, T3) is a same-day change and closes the worst error. T1 should be
started immediately since it gates the rest and needs a live session to observe.

T12 moved out of "any time": the decision (§9) is to project `Pending` to the
wire, so it cannot precede T5, and its stale-red exposure is bounded by T10's
per-writer cap. Shipping T12 before T5 would be strictly worse than today —
§9.1 explains why.

---

## 9. T12 resolved — `Pending` across a daemon restart

**Decision.** Persist the **key set** of `Pending` as one additive wire field;
drop the correlator *values*; use the transcripts at hydrate **only to falsify**
a persisted entry, never to manufacture one.

```
persist   Pending's keys           → claude.pending_writers: ["main", "af5b…"]
drop      Tool, InputHash          → re-earned from the next hook; loss costs latency only
re-stamp  Since := startup         → unchanged from today's dropStaleSessions rule
falsify   at hydrate, per writer   → a subagent whose jsonl shows no unmatched
                                     trailing tool_use is dropped immediately
```

The one-line justification: **losing ownership is error #1, losing the
correlators is error #2, and re-deriving ownership manufactures error #2 at a
rate proportional to how often a restart lands mid-tool.** Persist what guards
#1, drop what only costs #2, and let disk evidence subtract but never add.

### 9.1 The premise Q7 was built on is false

`AgentInfo.Status` is `json:"status"` — **on the wire, and restored by
`Store.Load`** (`internal/state/state.go:154`, `:610`). A `permission` chip
therefore already survives a daemon restart today, and `dropStaleSessions`
(`cmd/switchboard/main.go:250`) contains a deliberate, commented re-stamp
(`StatusSince = now`) whose stated purpose is *"a zero value would read every old
`tool_result` as 'resolved after', wrongly demoting a still-pending prompt that
was live across the restart."* Someone already made this call, in the direction
of keeping the red.

So the restart at 12:39:56 did **not** drop a red. There was no red to drop: it
had been lost 95 s earlier at 12:38:21 by `case9-approve-toolmatch`. What the
restart did was re-stamp `StatusSince` to startup time, which is precisely what
T4 does permanently — it broke the flap's engine (`IdleTitleGrace` became a real
15 s again), not a permission hold. The incident doc's "ended it by accident" is
correct; the T12 gloss "a restart drops the red" was not.

What a restart actually destroys is **ownership**, and the consequence is worse
than a clean drop:

| after a restart, today | value |
|---|---|
| `Status` | `permission` — restored |
| `Transcript`, `SessionID`, `InFlightSubagents` | restored |
| `StatusSince` | re-stamped to startup (deliberate) |
| `PendingTool` | **lost** (`json:"-"`) |
| the raising `agent_id` (post-T5: all of `Pending`) | **never stored** |

An ownerless red is not a neutral red. `clearsPermission`'s second gate
(`internal/rpc/rpc.go:508`) and `selfHealStaleAttention`
(`cmd/switchboard/main.go:718`) both resolve against `info.Transcript` — the
**main** transcript. With no owner recorded, a hydrated red silently becomes a
*main-thread-owned* red, and defect 4 clears it on the first main-thread
assistant message after startup. For case 16 — the target behavior, main working
while a teammate is blocked — that is seconds. **T9 does not fix this**: T9
routes `Pending[a]` to `a`'s transcript, and after a restart `Pending` is empty,
so there is nothing to route.

And post-T5 the fold is `len(Pending) > 0 → RED`. An empty map beside a
persisted `Status == "permission"` is an *inconsistent* hydrate: the authority
says not-red, the persisted field says red. Whichever wins, T5 shipped without
T12 would regress the one restart property the codebase currently has. That is
why T12 lost its "any time" slot.

### 9.2 Why the drop does not self-heal — and why that is decisive

Nearly every piece of transient status state in this daemon is safe to drop
because the next event rebuilds it. A pending permission is the exception, and
the same property that makes the reconciler's self-heal necessary makes the drop
unrecoverable:

- Claude Code fires `PermissionRequest` **once**, when the prompt appears. It is
  edge-triggered, with no repeat and no level signal.
- Switchboard registers seven hooks (README) and **none of them is a
  `Notification`-style waiting hook.** No hook re-raises a live prompt.
- A blocked writer runs no tools, so it emits no `PostToolUse` either. The
  quiescence *is* the state.
- Codex records no approvals in its rollout at all, so there is not even a
  file-level fallback there.

So a dropped `Pending` entry is not a transient false GREEN that the next tick
repairs. It is a **permanent missed RED for the entire remaining life of that
prompt** — cost #1, the worst error, unbounded in duration, and concentrated on
exactly the sessions the signal exists for. This is what moves the decision off
the naive "fail open, it's only #3" reading.

### 9.3 What the transcripts can and cannot reconstruct

`subagent-permission-oscillation.md` §4.3 proposes reconstructing the state from
the blocked-writer signature. Read carefully, §4.3 is a **conjunction**:

> a `PermissionRequest` was seen and no resolution has been proven; **and** the
> writer it was raised under has been quiescent since

The first conjunct is exactly the hook state a restart destroys. Drop it and the
remainder — "this writer is quiescent" — is not identifiable: it is equally
consistent with a long-running tool, a drained teammate, a crashed teammate, and
a main thread parked at the human prompt. §4.3 is a *resolution* primitive, not
a *reconstruction* primitive. Using it as the latter is a category error.

There is, however, a stronger on-disk signal than §4.3 assumed, and it is worth
recording because it changes what a hydrate can do. Checked against the incident
files (`agent-af5bd126402ac16c7.jsonl`, the blocked `escalate-cleanup`):

```jsonc
{"type":"assistant","ts":"…19:38:13.284Z","stop_reason":null,   "content":[{"type":"tool_use","name":"Bash"}]}
{"type":"assistant","ts":"…19:38:14.068Z","stop_reason":"tool_use","content":[{"type":"tool_use","name":"Bash"}]}
// EOF — no tool_result for 4.5 minutes, until the user answered
```

A subagent's own jsonl **does** record the pending `tool_use`, with its
`tool_name` and `tool_input`, and simply never receives the matching
`tool_result`. ⚠ This contradicts `status-color-state-model.md`'s "Claude Code
withholds the pending tool_use's assistant message until it resolves". That claim
was made about the **main** transcript, and V4 (§9.7) has since shown it does not
hold there either: the main jsonl carries the pending `tool_use` within ~5 s of
the hook and keeps it unmatched for the whole wait. The claim is simply false and
is scheduled for correction wherever it appears.

But an unmatched trailing `tool_use` means "a tool is dispatched and has not
returned," which covers *awaiting approval* **and** *executing right now*. There
is no third field that separates them. Re-deriving `Pending` from this signature
would therefore raise a false RED on every session that happened to be mid-tool
when the daemon restarted — for the remaining duration of that tool. Negligible
for a 2 s `Bash` (#5), several minutes for a test suite (#2, and #2's cost scales
with duration). Restarts land at arbitrary points in a session's life, so the
false-positive rate is roughly the duty cycle of tool execution: high.

The asymmetry is the whole decision: **the same check is unsound as a source and
sound as a falsifier.** An unmatched trailing `tool_use` does not prove a prompt
is pending, but its *absence* does prove the writer is no longer blocked — the
tool returned, so whatever gate it was behind opened. Applied only to entries we
already persisted, the check can only remove, so its worst outcome is shortening
a red (#2/#3), never inventing or missing one.

That matters because naive persistence has a real hole the falsifier closes: a
prompt **answered while the daemon was down**. The `Since := startup` re-stamp
makes the pre-restart resolution invisible, so the red would latch until T10's
cap. The falsifier sees the landed `tool_result` and drops the entry on the first
tick.

### 9.4 The hydrate-I/O objection does not survive contact with the code

The brief framed hydrate-time I/O as a possible blocker, given the recent work
keeping I/O out of the daemon's store lock. It is not:

- **The daemon is not serving yet.** `store.Load()` (`main.go:96`) and
  `dropStaleSessions` (`:106`) both run before the scanner and reconciler
  goroutines start (`:141`–`:150`) and long before `server.Serve` (`:160`). There
  is no lock contention to create. `dropStaleSessions` already performs a
  `/proc` read per session *inside* `store.Apply`, for exactly this reason.
- **The reads are already in the per-tick budget.** `fanout.Observer.Reconcile`
  calls `transcript.SubagentsForTranscript` for every session on **every**
  reconcile tick (`internal/fanout/observer.go:134`), and that already does a
  `ReadDir` plus a bounded 128 KiB tail read of *every* `agent-<id>.jsonl` for
  its `Done` check (`internal/transcript/subagents.go:173`, `:192`). The
  falsifier reads the same files, once, at startup — strictly cheaper than one
  ordinary tick.

The real costs of persisting are contract cost and stale-red cost, not I/O.
Still, do the reads **outside** `store.Apply` and pass the verdicts in, per the
direction the recent perf work established.

### 9.5 The options, weighed

| Option | Guards #1 (missed RED) | Introduces | Verdict |
|---|---|---|---|
| **A — drop and accept** | ✗ permanent missed RED (§9.2); post-T5 also a regression vs. today (§9.1) | — | rejected |
| **B — re-derive from transcripts** | ✗ cannot reconstruct the "a prompt was raised" conjunct (§9.3) | false RED ∝ tool duty cycle | rejected as a source |
| **C — persist the whole `PendingPrompt`** | ✓ | frozen-contract obligation on `input_hash`; a red latched across a restart that was answered while down | rejected — pays contract cost for a latency-only gain |
| **D — replay the history sink** | ✓ when enabled | history is **off by default** and privacy-tiered (`internal/history/history.go:18`) — a red chip's survival would depend on a telemetry setting | rejected on coupling |
| **E — keys only + hydrate falsifier** | ✓ | one additive wire field; a bounded stale red; approve-path latency reverts to the transcript path for one prompt after a restart | **adopted** |

Against §4's ranking, E is the only option that spends nothing on #1. What it
spends is #2-latency, and only for prompts alive across a restart: the hook-speed
early clear is unavailable for a hydrated entry (no `Tool`/`InputHash` to match),
so it resolves via the transcript on the next reconcile tick instead — one tick,
bounded, and exactly the trade Phase 0's T2 already makes deliberately.

Why not persist `Tool`/`InputHash` too, since they are cheap? Because they buy
one prompt's worth of sub-tick latency and cost a permanent field on a frozen
public contract, and because a hydrated entry **must** fail closed on the hook
path anyway (§9.6, trap 2). Keys carry the #1 protection; values carry the #2
polish. Persist the first, re-earn the second.

Why not persist `Since`? The existing re-stamp comment answers it: a true
pre-restart onset makes every pre-restart transcript entry read as "resolved
after." Keeping it would require two clocks — true onset for T10's cap, restart
for the resolution window — for a marginal gain. Consequence, stated plainly: a
prompt raised ten minutes before a restart gets a **fresh full cap** after it.
That is the same #1-over-#2 trade `dropStaleSessions` already makes. Revisit only
if hydrated reds become a felt nag (§9.8).

### 9.6 Implementation notes — the four things that will go wrong

Non-obvious enough that the follow-up task should be written from this list.

1. **Sort the projection, or you break publish-suppression.** `Pending` is a
   map; `snapshotChangeKey` (`state.go:424`) JSON-encodes every tagged field to
   decide whether to publish. An unsorted `[]string` built by ranging the map
   differs between snapshots of identical state, so every reconcile tick would
   republish to all ten waybar slots and rewrite `state.json` — reintroducing
   precisely the wake-storm the change key exists to suppress. Sort ascending.
2. **A hydrated entry must fail closed on the hook path.** `clearsPermission`'s
   first gate already guards `toolName != "" && toolName == info.PendingTool`, so
   an empty `PendingTool` cannot match — do not "fix" that by relaxing it to an
   `agent_id`-only match. Same-writer matching looks sound ("a blocked writer
   runs no tools") but is not: Claude Code emits parallel `tool_use` blocks in
   one assistant message, so a writer can complete an auto-approved sibling while
   its own prompt is still pending. That is the incident's bug at a narrower
   radius. A hydrated entry resolves by transcript only.
3. **Falsify main-thread entries too — V4 settled it (§9.7).** The unmatched-
   `tool_use` signature was verified for *subagent* jsonls first (§9.3); V4 has
   since verified it for the **main** transcript, contemporaneously and inside
   three real pending windows, so the `a != main` gate this trap once demanded is
   **not** needed and should not be built. Two constraints survive and are
   load-bearing: (a) test for **any** unmatched `tool_use` in the tail, never the
   trailing one alone — a gated tool with an auto-approved parallel sibling
   leaves the *last* `tool_use` matched while the prompt still waits, and
   dropping on that is the missed RED this trap was written to prevent; (b)
   falsify each writer against **its own** file (main → `<session>.jsonl`,
   subagent → `<session>/subagents/agent-*.jsonl`) — a subagent-raised prompt
   leaves the main tail fully matched, so crossing the two inverts the answer.
   Unreadable, truncated-tail, or tail-window-missed-the-`tool_use` all mean
   *keep*, matching `permissionExit`'s `unreadable` handling.
4. **Handle the three hydrate combinations explicitly.**

   | persisted `Status` | persisted `pending_writers` | action |
   |---|---|---|
   | `permission` | non-empty | rebuild `Pending`, `Since := startup`, falsify every entry against its own writer's jsonl (main included — §9.7) |
   | `permission` | empty / absent | a pre-T12 `state.json`. Seed `Pending{main}` — this reproduces today's behavior exactly and is the honest downgrade across the version boundary |
   | not `permission` | non-empty | should be unreachable. `Pending` is the authority post-T5: keep it, re-fold to RED, and log it — a silent disagreement here is how a missed RED hides |

Mechanics, briefly:

```go
// AgentInfo — the wire projection of Pending's key set (T12, §9).
PendingWriters []string `json:"pending_writers,omitempty"` // sorted; "main" = main thread
```

projected in `enrichForWire` (`state.go:474`) from the in-memory map exactly as
`StatusSinceWire` is projected from `StatusSince` — never written by hook or
reconciler logic. Use the literal
`"main"` for the `""` map key rather than emitting an empty string on a public
contract (`main` is not a valid `agent-<id>` stem, so it cannot collide);
translate at the wire boundary in both directions. Rebuild inside
`dropStaleSessions`, on the same lines as the `StatusSince` re-stamp and for the
same stated reason. Falsifier lives in `internal/transcript` next to
`ResolveKind`, reading the same bounded tail: report *definitively resolved* /
*still unmatched* / *unknown*, and let the caller keep the entry on anything but
the first. Per `docs/state-schema.md`: additive, so no major bump, but it must be
documented there, set on the canonical session in `canonicalSnapshot()` (left
absent on the minimal session to pin the omission), and the golden regenerated.

Tests, in the §5 style:
- should keep a subagent-owned red across a hydrate when that subagent's jsonl
  still ends in an unmatched `tool_use`
- should drop a hydrated entry when the subagent's jsonl shows the matching
  `tool_result` (answered while the daemon was down)
- should keep a main-owned red across a hydrate when `<session>.jsonl` holds an
  unmatched `tool_use` (V4, §9.7)
- should keep a main-owned red when the tail's *trailing* `tool_use` is matched
  but an earlier parallel sibling is not (trap 3a — the missed-RED case)
- should keep a main-owned red when only a *subagent* jsonl shows a matching
  `tool_result` (trap 3b — writers are falsified against their own file)
- should not clear a hydrated red on a `PostToolUse` from the owning writer
  carrying a *different* tool (trap 2)
- should seed `Pending{main}` from a `state.json` written before this field
  existed (trap 4)
- should produce a byte-identical change key for two snapshots whose `Pending`
  maps are equal but iterate in different orders (trap 1)

### 9.7 V4 — RESOLVED: the main transcript *does* record a pending `tool_use`

- **V4 — does the main transcript record a pending main-thread `tool_use`?**
  **Yes.** The pending `tool_use` is a complete, newline-terminated line in the
  main `<session>.jsonl` within ~5 s of the `PermissionRequest` hook and stays
  there, unmatched, for the whole wait. The "Claude Code withholds the pending
  tool_use's assistant message until it resolves" claim is **false about the main
  transcript**, not merely about subagent files (§9.3). **Trap 3's gate can be
  lifted: extend the falsifier to `a == main`.** Confidence: high — the decisive
  measurement is contemporaneous, not reconstructed.

**Why the naive check could not settle it.** "The entry is in the file now" does
not prove it was in the file while the prompt waited; H7/H8 (`docs/timing-hazards.md`)
document exactly that skew, and every one of the seven long main-thread windows
below has its `tool_result` on the *very next line*, so file order alone proves
nothing. Under wall-clock anchoring the daemon's own `ResolveKind` reads cannot
settle it either — the anchor was moved to `now` precisely so the prompt's own
turn-mates (dated before the hook) stop registering. The reader that *can* settle
it is the one that ignores timestamps entirely.

**The measurement — `usage_sample` as a line counter.** `UsageSince` /
`UsageSinceByModel` (`transcript.go:524`, `:552`) walk a **byte cursor** over
`c.Transcript` — the *main* jsonl (`fanout.go:137`) — sum `message.usage` over
every assistant line appended since the last offset, and emit one `usage_sample`
history event per non-zero delta. Two properties make it a proof instrument:

- `readNewLines` truncates at the last `\n`, so **a counted line was complete and
  newline-terminated on disk at sample time**; a line caught mid-write is
  excluded and re-read next call.
- Claude Code splits one assistant message into one jsonl line per content block
  (thinking / text / `tool_use`), and **repeats the identical message-level
  `usage` on each**. So a sample that counted *N* lines of message *M* reports
  exactly *N* × *M*`.usage`. The ratio is a line count.

Each blocked turn below is a 3-line message — thinking, text, `tool_use` — and
the `tool_use` is the **last** of the three. A sample of exactly 3× therefore
counted the `tool_use` line, and it did so at a wall-clock the history log
records independently of any transcript timestamp.

| session | project | hook (local) | sample | sample − hook | sample → user's answer | `got/unit` |
|---|---|---|---|---|---|---|
| `f4aff00a` | zettel-llm | 2026-07-22 13:50:32.637 | 13:50:37.197 | **+4.56 s** | **−453.4 s (7 m 33 s)** | 3.0 / 3.0 / 3.0 / 3.0 |
| `e57df9f4` | resume | 2026-08-03 09:42:23.901 | 09:42:28.778 | **+4.88 s** | **−703.4 s (11 m 43 s)** | 3.0 / 3.0 / 3.0 / 3.0 |
| `77baa90a` | arachne | 2026-08-05 10:32:13.153 | 10:32:18.177 | **+5.02 s** | **−275.5 s (4 m 36 s)** | 3.0 / 3.0 / 3.0 / 3.0 |

`got/unit` is (`tok_in`, `tok_out`, `tok_cache_read`, `tok_cache_create`) divided
by the pending message's per-line usage — four independent fields, exact integer
3× in all three cases, no rounding. Worked example (`f4aff00a`, the 2026-07-22
zettel episode H8 already cites): lines 249/250/251 are `thinking` / `text` /
`tool_use(AskUserQuestion)` of message `…pC6FE8`, each carrying
`usage = (2, 2504, 154510, 889)`; the 13:50:37.197 sample recorded
`(6, 7512, 463530, 2667)`. The `tool_result` did not land until 13:58:10.632.
Corroborating: that episode's `age=40s` on a 5 s-old red pins the pre-fix
`AnchorTime` to line 248 (`ts 20:49:57.055Z`), i.e. lines 249–251 were **not** on
disk when the hook was processed and **were** ~4.6 s later — the flush lands
seconds into the wait, not at resolution. Two further windows (`6169e789`
13 m 20 s, `4286c0d4` 10 m 48 s) show the same exact 3×, but their sample fell
~1 s *after* the answer, so they corroborate the 3×-means-3-lines reading without
being in-window. Reproduce with the history log
(`$XDG_STATE_HOME/switchboard/history/<day>.jsonl`) plus the journal's
`working->permission` edges.

**Independent confirmations that the writer never withholds a `tool_use`:**

- *Live sampling* — 90 s polling every live session's main jsonl at ~0.28 s
  produced **8 distinct moments** where a main transcript's on-disk tail ended in
  an unmatched assistant `tool_use` (`stop_reason:"tool_use"`) while the tool was
  in flight, held 0.65–10.9 s, file mtime 0.008–0.213 s old (sessions `067e9f45`,
  `4efbd84c`). The main transcript is written at **dispatch**.
- *A durable frozen tail* — `d15e58ff…jsonl` (switchboard, cc 2.1.218) ends at
  line 231 on a main-thread (`isSidechain:false`) `tool_use(Bash)` dated
  `2026-07-23T03:29:01.537Z`, mtime `03:29:02.439Z` (**+0.902 s**), no
  `tool_result` ever, untouched since. The write of a main-thread `tool_use` does
  not wait on its result.
- *Entry dating* — across all seven main-thread windows the pending `tool_use`
  entry is dated **6–374 ms before** its `PermissionRequest` hook and its
  `tool_result` **20–101 ms after** the clearing edge. Nothing in the entry
  depends on the approval; only its flush instant was ever in question, and the
  `usage_sample` measures that directly.

**Two carry-overs for the implementer, both empirical, both in the same files.**

1. **Falsify on *any* unmatched `tool_use` in the tail, not the trailing one.**
   Parallel dispatch is common — `f4aff00a` 245/246, `d15e58ff` 210/211 and
   219/220, `20f63439` 624/625 all show two `tool_use` lines from one message.
   With a gated tool and an auto-approved sibling, file order is
   `tool_use(gated)`, `tool_use(sibling)`, `tool_result(sibling)`: the *trailing*
   `tool_use` is matched while the prompt still waits, so a trailing-only test
   would drop a live red. This is trap 2's hazard on the transcript path, and it
   applies to subagent jsonls equally.
2. **Falsify a writer only against that writer's own file.** Live capture of
   subagent-raised prompts in sessions `d053f268` and `40f2f003` shows the main
   tail fully **matched** throughout, while the raising `agent-*.jsonl` under
   `<session>/subagents/` carried the unmatched `tool_use`. A main-keyed entry
   checked against a subagent file (or the reverse) inverts the answer.

**Follow-ups this verdict obliges** (outside this task's file scope):
`status-color-state-model.md:99` and `state-schema.md:177` both assert the
withholding as fact and must be corrected — the resolution latency they explain
is real, but its cause is model latency to the *next* assistant message plus tick
granularity, not a withheld write. `transcript.go`'s package comment is corrected
here. Joins V1–V3 in the oscillation doc §6.

### 9.8 What would change this decision

- **V3 shows the trailing-`tool_use` signature is not stable across prompt kinds**
  (`AskUserQuestion`, `ExitPlanMode`, plan mode). Then drop the falsifier
  entirely and rely on persistence plus T10's cap. Cost: a prompt answered while
  the daemon was down latches red until the cap. Everything else in §9 stands —
  the falsifier is an optimization on the #2 axis, not load-bearing for #1.
- **Hydrated reds become a felt nag.** Then persist `Since` as a second field and
  run two clocks (true onset for the cap, restart for the resolution window),
  rather than weakening the hold. This is the escape hatch §9.5 declined to build
  pre-emptively.
- **Restarts become frequent and unattended** (daemon auto-update, a crash loop).
  Persistence gets *more* valuable, not less, and the falsifier moves from
  optional to required — the answered-while-down window is proportional to
  downtime.
- **A `Notification`-style waiting hook is registered.** That would give a live
  prompt a level signal instead of an edge, and §9.2's "no hook re-raises it"
  — the load-bearing premise of the whole decision — collapses. Then drop the
  wire field and let the hook rebuild `Pending` after a restart. Worth a
  standing recheck at each Claude Code version bump alongside
  `claude-code-hook-schema.md` — and recheck V4 (§9.7) there too: it is a fact
  about Claude Code's transcript writer, not about switchboard, so a writer
  change could restore the `a != main` gate. The `usage_sample` 3×-ratio method
  in §9.7 re-runs in minutes against the history log.
- **Not a mind-changer:** the cost of the hydrate reads (§9.4), or discomfort
  with a new field on the frozen contract. The field is additive, it is the
  minimum that guards #1, and it is information a renderer wants anyway (T11).
