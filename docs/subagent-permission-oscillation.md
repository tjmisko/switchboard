# Subagent-raised permission: lost RED and the orange/green limit cycle

**Status:** diagnosed, not fixed. Evidence-complete — every claim below is
pinned to a daemon log line or a transcript timestamp.

**Incident:** 2026-08-05, session `5318eb5b-79df-4dee-a9f8-c80df4eca79e`
(pid 1090904, `/home/tjmisko/Tools/DigestDownloads`, chip
`digestdownloads-status-update-request`). Two occurrences: 12:21:48–12:23:26
and 12:38:13–12:39:56 local. The daemon was restarted at 12:39:56, which ended
the second one by accident, not by fix.

> Transcript `timestamp` fields are UTC; the daemon log is local (UTC−7).
> `19:38:14Z` and `12:38:14` are the same instant. Local time is used
> throughout except where a raw transcript value is quoted.

---

## 1. Symptom

A chip sat flipping between orange (idle) and green (working/delegating) on a
roughly 5-second cadence while the session was, in fact, parked on an
unanswered permission prompt:

```
Bash command · from the escalate-cleanup agent
  git checkout -- digest-downloads && git status --short && echo "clean"
Do you want to proceed?
```

The chip should have been **red** and stayed red. It was red for eight seconds
and then never again for the next 95 seconds of the same pending prompt.

The prompt was raised **by a subagent**, not by the main thread — this is the
case the status model has no representation for, and it is the case the user
most wants surfaced: *a human decision is blocking agent work that is otherwise
ready to run.*

---

## 2. What actually happened

### 2.1 The session's shape at the time

Four subagents in flight (`S=4`), all `spawnDepth: 1`, all `general-purpose`,
from `…/5318eb5b-…/subagents/`:

| agent_id | name | role in the incident |
|---|---|---|
| `af5bd126402ac16c7` | `escalate-cleanup` | **raised the prompt; blocked** |
| `aa83942381ce15c04` | `wire-frontmatter` | kept running, kept firing hooks |
| `a158b13da3d13b0ea` | `marc-provider` | kept running, kept firing hooks |
| `ab81d017533efe9d5` | `verify-sources` | running |

The main thread had been blocked since **12:25:24** — its last transcript entry
is `2026-08-05T19:25:23.999Z`, and the next one is `19:39:36.222Z`, a
**14-minute gap**. It wrote nothing at all during the incident.

`escalate-cleanup` wrote its last entry at `19:38:14.068Z` — 0.8 s after its
`PermissionRequest` hook — then went silent until `19:42:55Z`, when the user
finally answered. It was genuinely blocked the whole time.

The other two teammates wrote continuously throughout, every 3–4 seconds.

### 2.2 The daemon's view

```
12:38:13.303  working->permission            (event=PermissionRequest)        ← correct, RED
12:38:20.740  permission==permission  rule=case12-hold-bare-result  [S=4 pending="Bash" age=7s]
12:38:21.288  permission->working     rule=case9-approve-toolmatch
                                      reason="tool-name match: Bash"  [S=4 pending="Bash" age=8s]   ← RED LOST
12:38:26.246  working->idle           rule=case6-idle-title           [S=4 pending="" age=13m2s]     ← ORANGE
12:38:27.338  idle->working                  (event=PostToolUse)                                     ← GREEN
12:38:31.277  working->idle           rule=case6-idle-title           [S=4 pending="" age=13m7s]
12:38:32.011  idle->working                  (event=PostToolUse)
12:38:36.245  working->idle           rule=case6-idle-title           [S=4 pending="" age=13m12s]
12:38:40.062  idle->working                  (event=PostToolUse)
        … this two-line cycle repeats for 95 s …
12:38:51.293  idle->delegating        rule=case5-delegating           [S=4 pending="" age=5s]
12:38:51.492  delegating->working            (event=PostToolUse)
```

Two things to read off this directly:

1. At **12:38:20** the guard worked (`case12-hold-bare-result`) and one second
   later at **12:38:21** the same guard let a clear through — because that
   `PostToolUse` happened to carry `tool_name="Bash"`, matching the pending
   prompt's tool.
2. The `age` on every `case6-idle-title` line is **13+ minutes** on a chip that
   was set to `working` one second earlier, and it grows by exactly 5 s per
   tick. `12:38:26 − 13m2s = 12:25:24` — the main transcript's last write. The
   age baseline is stale by design (§3.3).

### 2.3 The hooks that re-greened the chip were teammates'

Daemon hook edges line up to the millisecond with *other* subagents' transcript
writes — never with the main thread's (silent) or `escalate-cleanup`'s (silent):

| daemon `idle->working` | matching subagent write | agent |
|---|---|---|
| 12:38:27.338 | `19:38:27.304` / `.343` | `wire-frontmatter` |
| 12:38:32.011 | `19:38:32.016` | `wire-frontmatter` |
| 12:39:04.630 | `19:39:04.552` | `marc-provider` |

The chip was being painted green by the tool activity of agents that had nothing
to do with the blocked prompt.

---

## 3. Root cause

Three defects. **Defect 1 alone caused the lost red**, and it is also what
*admitted* the oscillation — a `permission` chip is inert to both oscillating
rules (`cmd/switchboard/main.go:775` gates on idle/delegating, `:806` gates on
working, and `:816` drops everything else), so the flap can only run once the
chip has been wrongly knocked out of `permission`. Defects 2 and 3 are the
engine that then keeps it flapping, and they are independently real.

### 3.1 Defect 1 — `clearsPermission` matches on tool *name*, not tool *identity*

`internal/rpc/rpc.go:504`:

```go
if s.tun.EarlyClearApproveByToolName && toolName != "" && toolName == info.PendingTool {
    return true, statustune.RuleApproveToolMatch, "tool-name match: " + toolName
}
```

The comment above it calls this "the identity-correlated fast path (A2)". It is
not identity-correlated — it is a **string comparison on a tool type**. With any
subagent in flight, a teammate running the same kind of tool satisfies it. And
the tool in question is `Bash`, the most frequently-executed tool there is, so
the collision is not a rare race — it is the expected outcome within a few
seconds of any subagent-raised Bash prompt.

`docs/status-color-state-model.md` §7 A2 states the DoD as "a sibling/`Task`
`PostToolUse` during a pending prompt still holds red," and Q2 explicitly
records the choice of tool-name matching over `tool_use_id` as covering "the
common cases … without" a settings change. The incident is precisely the case
that choice does not cover: a sibling `PostToolUse` **of the same tool name**.

### 3.2 Defect 2 — every subagent hook is attributed to the parent chip

`cmd/switchboard-ctl/main.go:381` identifies the calling session with
`os.Getppid()`. Subagents run in-process inside the same Claude Code process, so
a teammate's hook yields the **same pid** as the main thread's. `PostToolUse`
maps unconditionally to `working` (`internal/rpc/rpc.go:612`), so with four
teammates running, the parent's chip is dragged to green every ~4 s regardless of
what the main thread is doing — including while it is parked at a prompt.

The daemon models a Claude Code session as one status-bearing thread. It is
actually 1 + N threads that share a pid and a chip and write to different files.

### 3.3 Defect 3 — `AnchorSince` back-dates `StatusSince`, defeating the damper

`internal/rpc/rpc.go:454` dates an edge into `working` from the newest entry in
the **main** transcript (`transcript.AnchorSince` → `AnchorTime`,
`internal/transcript/transcript.go:716`/`:675`). The rationale (H1) is sound:
the hook arrives after the entry that triggered it, so a wall-clock stamp would
sit ahead of a fast follow-up signal.

But when the edge is driven by a *subagent's* hook, the main transcript is
quiescent — here, 13–14 minutes stale. Every re-green therefore stamped
`StatusSince = 12:25:24`, which is why `age` reads `13m2s` on a one-second-old
chip.

That breaks the two gates that exist to prevent exactly this flap
(`cmd/switchboard/main.go:806`):

```go
sess.Wezterm.TitleAt.After(c.StatusSince) &&          // freshness gate — trivially true
now.Sub(c.StatusSince) >= tun.IdleTitleGrace          // 15s grace — trivially true
```

With a 15 s `IdleTitleGrace` and a 5 s reconcile tick, a correctly-dated chip
could not be demoted for three ticks and the cycle would damp itself. Back-dated,
the demotion fires on the **very next tick**, every time.

### 3.4 The limit cycle

```
        ┌──────────────────────────────────────────────────────────┐
        │                                                          │
   working ──[tick: idle-title, grace defeated by D3]──► idle ─────┤
        ▲                                                          │
        │                                                          │
        └──[teammate PostToolUse, D2 — re-anchors stale, D3]───────┤
        ▲                                                          │
        └──[delegating->working]◄──[tick: case5-delegating, S>0]───┘
```

Period ≈ the 5 s reconcile tick, phase-modulated by teammate hook arrivals. Both
`working` and `delegating` render green, so the user sees green ↔ orange.

This cycle is **self-sustaining without any permission request at all** — a
genuinely idle main thread with teammates in flight will flap the same way. The
permission case is the damaging instance because it also discards the red.

### 3.5 Two further defects, latent here, fatal to the target behavior

Both were found while designing the fix. Neither fired in this incident because
the main thread happened to be blocked too (silent since 12:25:24) — but the
target behavior is *"a subagent prompt turns the chip red **even while the main
thread is working**"*, and in that scenario each one independently discards the
red within seconds.

**Defect 4 — resolution is checked against the wrong writer's transcript.**
`selfHealStaleAttention` (`cmd/switchboard/main.go:718`) and
`clearsPermission`'s fallback (`internal/rpc/rpc.go:508`) both call
`transcript.ResolveKind(c.Transcript, …)` — the **main** transcript — no matter
who raised the prompt. `ResolveKind` returns `ResolutionResumed` for any
assistant message dated after the prompt
(`internal/transcript/transcript.go:392`). So a main thread that simply *keeps
working* while a teammate is blocked produces a stream of assistant messages
that read as "the prompt resolved." Main-thread activity is not evidence about a
subagent's prompt.

**Defect 5 — the hold gate only guards `PostToolUse`.**
`internal/rpc/rpc.go:388` gates on
`status == "working" && req.Event == "PostToolUse" && info.Status == "permission"`.
Every other hook transitions out of `permission` unguarded: a main-thread `Stop`
→ `idle`, a `UserPromptSubmit` → `working`, a `SessionStart` → `idle`. With a
teammate blocked, the main thread finishing its turn silently repaints the chip
orange. The guard protects one edge out of four.

Together these mean the red cannot survive a working main thread today, which is
precisely the case the fix must support.

---

## 4. Determining this reliably

The question is: *how do we know a subagent-raised permission is pending, so we
can hold red?* The discriminator is purpose-built and already on the wire.

### 4.1 `agent_id` — the intended field, already being received

From the Claude Code 2.1.222 hook-input schema (extracted from the binary):

> **`agent_id`** — "Subagent identifier. Present **only** when the hook fires
> from within a subagent (e.g., a tool called by an AgentTool worker). Absent
> for the main thread, even in `--agent` sessions. **Use this field (not
> `agent_type`) to distinguish subagent calls from main-thread calls.**"

> **`agent_type`** — "Present when the hook fires from within a subagent
> (alongside `agent_id`), or on the main thread of a session started with
> `--agent` (without `agent_id`)."

This is a **general hook-input field, not a `SubagentStart`/`Stop`-only field**.
`docs/subagent-fanout-detection-plan.md` §"Authoritative facts" verified it for
the lifecycle hooks; the schema shows it applies to any hook firing inside a
subagent, `PermissionRequest` and `PostToolUse` included.

Crucially, **switchboard already receives it today**:
`cmd/switchboard-ctl/main.go:405-410` parses `agent_id` from the payload
unconditionally for every event, and `:413-423` forwards it as
`rpc.Request.AgentID` (`internal/rpc/rpc.go:70`). The daemon consults it only in
the fanout-trigger branch (`internal/rpc/rpc.go:480`) and ignores it everywhere
else. The field that would have prevented this incident was arriving on both the
`PermissionRequest` at 12:38:13 and the `PostToolUse` at 12:38:21, unread.

So the discrimination is: `PermissionRequest` carries the raising agent's
`agent_id` (`af5bd126402ac16c7`, or empty for the main thread); a `PostToolUse`
may only clear that red if its `agent_id` matches the one the prompt was raised
under. `wire-frontmatter`'s Bash carries a different `agent_id` and is rejected.

**This must be verified empirically before relying on it** — the schema string
is documentation embedded in the binary, not an observation. See §6 V1.

### 4.2 Second correlator: `tool_input`

The documented tool-event payload includes `tool_input` alongside `tool_name`.
The pending prompt's input was
`{"command": "git checkout -- digest-downloads && …"}`; a teammate's Bash
carries different input. Forwarding a hash of `tool_input` and requiring
`(tool_name, tool_input_hash)` to match would have rejected the false clear even
with no `agent_id` at all. `switchboard-ctl` does not currently parse
`tool_input`; adding it is cheap. Use this as the belt to `agent_id`'s braces,
since it degrades gracefully if `agent_id` proves absent on tool events.

`prompt_id` ("UUID correlating a user prompt with all subsequent events until
the next prompt") is also available and is the right key for grouping a turn's
events, though it does not by itself separate main thread from subagent.

### 4.3 File-level fallback: the blocked-writer signature

If neither field can be relied on, the state is still derivable from files
already on disk, with no new hook plumbing. A pending permission has a distinct
and specific signature:

- a `PermissionRequest` was seen and no resolution has been proven; **and**
- the writer it was raised under has been **quiescent since the moment it was
  raised** — for a subagent prompt, `subagents/agent-<id>.jsonl` stops within
  ~1 s of the hook (`escalate-cleanup`: hook 12:38:13.30, last write
  12:38:14.07, then nothing for 4.5 minutes); for a main-thread prompt, the main
  transcript stops.

Resolution is the mirror image: **the quiescent writer resumes.** That is a
direct observation of the thing we care about, and it is immune to teammate
noise by construction — the current check asks "did *any* tool of this name
finish?", which is not the same question.

The subagents dir is already read by the fanout Observer
(`internal/transcript/subagents.go:89`, derived as the sibling of the parent
transcript per G10), so the file access pattern exists.

### 4.4 The invariant worth adopting regardless

> **A `PostToolUse` must never clear a red chip on `tool_name` alone while
> `InFlightSubagents > 0`.**

One condition, no new data, available today. With `S == 0`, name-matching is
nearly sound — only a sibling parallel call in the same turn can collide. With
`S > 0` it is unsound, because teammates run the same tool names constantly.
This alone would have prevented the entire incident, including the earlier
12:21:48 occurrence.

### 4.5 Rendering: subagent-blocked should be RED — the spec already says so

The user's requirement — *"I'm blocking agent work from happening, and a quick
decision could unblock it"* — is **already the specified behavior**.
`docs/status-color-state-model.md` §5 case 16 reads:

> | 16 | a **subagent** raises a prompt | any | **yes** | live | **RED** | surfaces to user; needs action | missed-RED |

and case 12 says a bare unrelated `tool_result` from a subagent/sibling must
**hold** red. This incident is not a gap in the model; it is the implementation
violating two rows of it, because `clearsPermission` cannot tell case 12's
"unrelated tool_result" from case 9's "the approved tool completed" when both
carry the same `tool_name`.

That reframes the work: **no new state, no renderer change, no new case row.**
`permission` is already inert to `case5-delegating` and `case6-idle-title`
(`cmd/switchboard/main.go:775`/`:806`/`:816`), so a correctly-held red already
outranks the green teammates and already survives the flap. The entire fix is
making the case-12 hold robust, i.e. §4.1–§4.4.

The one thing worth adding to §5 is the *priority principle* the table encodes
only implicitly: **a pending permission anywhere in the session tree outranks
any amount of teammate activity**, because blocking-the-user is actionable and
work-is-happening is not.

---

## 5. Recommended fixes, in priority order

> Superseded by [subagent-permission-plan.md](subagent-permission-plan.md),
> which turns these into a phased tasklist and adds the two defects in §3.5.
> Kept here as the diagnosis-time reading.

| # | Fix | Site | Cost |
|---|---|---|---|
| F1 | Never clear red on tool-name alone when `S > 0` (§4.4) | `rpc.go:505` | one condition |
| F2 | Require `agent_id` match to clear; store the raising `agent_id` on `AgentInfo` at red onset next to `PendingTool` | `rpc.go:456`, `:504` | small; field already on the wire |
| F3 | Forward `tool_input` hash; require it to match too | `switchboard-ctl/main.go:388`, `rpc.go:504` | small |
| F4 | Don't back-date `StatusSince` when the anchor predates the previous `StatusSince` — clamp the anchor to `max(anchor, prevStatusSince)` rather than accepting an arbitrarily stale one | `transcript.go:716`, `rpc.go:454` | small; fixes the flap generally |
| F5 | Track per-writer status: a subagent hook should not drive the main thread's chip to `working` | `rpc.go:612` + `AgentInfo` | structural |
| F6 | Add the "pending permission outranks teammate activity" row to §5 | `status-color-state-model.md` | doc |

F1 is the stop-the-bleeding change. F2+F3 are the correct fix. F4 is
independently worth doing because the flap exists without any permission
involved. F5 is the real model correction and the natural home for a future
"which teammate is blocked" chip decoration.

Note that F1 deliberately trades *back* some of the approve-path latency
Phase A bought (§2.1's 26 s → sub-second), but only in sessions with teammates
in flight, and only until F2 lands. Per §4's asymmetric-cost ranking a missed
RED is the worst error and a slow-but-correct clear is the cheapest, so the
trade is the right way round.

---

## 6. Verification steps before implementing

- **V1 — confirm `agent_id` on tool events.** The schema string says it is
  present; observe it. Log `req.AgentID` on `PermissionRequest`/`PostToolUse`,
  run a subagent that trips a permission prompt, and confirm the raising hook
  carries a non-empty `agent_id` and a main-thread hook carries none. F2 depends
  entirely on this; F1 and F3 do not.
- **V2 — confirm `tool_input` is present on `PermissionRequest`.** Needed for F3.
- **V3 — confirm the quiescence signature generalizes** across prompt kinds
  (`AskUserQuestion`, `ExitPlanMode`, Bash approval) before relying on §4.3.

## 7. Regression tests

Alongside the fixes, in `internal/rpc` (mirroring the existing
`case12-hold-bare-result` coverage):

- should hold red when a `PostToolUse` matches the pending tool **name** but
  carries a different `agent_id`, with `S > 0` — the exact 12:38:21 edge.
- should hold red when `S > 0` and the payload carries no `agent_id` at all
  (F1's fallback path).
- should still clear red at hook speed when the matching `PostToolUse` carries
  the **same** `agent_id` as the prompt — the Phase A win must survive.
- should clear red for a main-thread prompt (empty `agent_id` both sides) with
  `S == 0` — the common case, unchanged.

And in `cmd/switchboard` for F4:

- should not demote a working chip via `case6-idle-title` within
  `IdleTitleGrace` of a hook-driven re-green, **even when the main transcript's
  newest entry is minutes old** — the anchor-staleness regression.

## 8. Reproducing

```bash
# the incident window, both occurrences
journalctl --user -u switchboard.service \
  --since "2026-08-05 12:20" --until "2026-08-05 12:41" -o short-precise \
  | grep 'pid=1090904'

# the main thread's 14-minute silence
rg -o '"timestamp":"2026-08-05T19:(2[0-9]|3[0-9]):[^"]+"' \
  ~/.claude/projects/-home-tjmisko-Tools-DigestDownloads/5318eb5b-*.jsonl | sort -u

# the blocked agent vs. the noisy ones
cd ~/.claude/projects/-home-tjmisko-Tools-DigestDownloads/5318eb5b-*/subagents/
rg -o '"timestamp":"2026-08-05T19:3[89]:[^"]+"' agent-af5bd126402ac16c7.jsonl | tail   # blocked
rg -o '"timestamp":"2026-08-05T19:3[89]:[^"]+"' agent-aa83942381ce15c04.jsonl | head   # running
```

## 9. Related

- `docs/status-color-state-model.md` §5 (case table), §7 Phase A2 (the
  tool-name early clear), §8 Q2 (the tool-name vs. `tool_use_id` decision this
  incident reopens).
- `docs/timing-hazards.md` H1 (the anchoring rationale D3 exploits), H9 (the
  silent abort the idle-title demotion recovers).
- `docs/subagent-fanout-detection-plan.md` (the `subagents/` dir layout and the
  `agent_id` keying convention §4.3 reuses).
