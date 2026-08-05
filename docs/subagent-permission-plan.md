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
- **T12 — Decide whether `Pending` should survive a daemon restart.** It is
  `json:"-"` today, so a restart drops the red — which is what accidentally
  ended the 2026-08-05 incident. Either persist it and re-verify against the
  transcripts on hydrate, or document the drop as intentional fail-open. **This
  is a real decision, not a cleanup** (Q7).

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
- **Q7 — should `Pending` survive a daemon restart?** T12. Persisting is more
  correct and re-verifiable on hydrate; dropping is simpler and fails toward the
  *second*-worst error rather than the worst. Needs a call.
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
T12     ── decision, any time
```

Phase 0 (T2, T3) is a same-day change and closes the worst error. T1 should be
started immediately since it gates the rest and needs a live session to observe.
