# Timing Hazards — the hook-vs-transcript skew class

> **Goal:** name the class of bugs where a chip color sticks at the wrong value
> because two asynchronous event streams — Claude Code's **status hooks** and its
> **transcript** — are reconciled against a clock that does not order them the way
> reality did. Keep a running, executable catalog of the specific timing
> scenarios so each one is pinned as a regression.
>
> The catalog lives in `cmd/switchboard/timing_hazards_test.go`
> (`TestTimingHazards`); every `Hn-…` id below maps to a row there.

---

## 1. The two event streams

Switchboard derives a session's color from two independent sources:

1. **Status hooks** — `UserPromptSubmit`, `Stop`, `PostToolUse`, `PreToolUse`,
   `Notification`, … Each is a subprocess Claude Code spawns that connects to the
   daemon socket and reports an event. The daemon maps the event to a color and
   stamps `AgentInfo.StatusSince` (`internal/rpc/rpc.go`, `handleHook`).
2. **The transcript** (`<session>.jsonl`) — the ground-truth record of the turn:
   user prompts, assistant messages, tool results, and the
   `[Request interrupted by user]` marker. Every entry is timestamped by Claude
   Code when it is written.

Hooks are **lossy**: some real transitions fire no hook at all. The two that bite
us (`cmd/switchboard/main.go`, `selfHealStuckStatus`):

- **interrupt (Ctrl+C / Esc)** fires no `Stop`, so a `working` (green) chip never
  clears — the agent stopped but the chip stays green.
- **teammate wakeup** fires no `working` hook on the orchestrator, so an `idle`
  (orange) chip stays orange while subagents run.

The reconciler recovers these *hookless* edges by reading the transcript tail and
asking: **"did anything happen after the chip last transitioned?"** That question
compares a transcript timestamp against `StatusSince`.

---

## 2. The skew (root cause)

A hook reaches the daemon **after** Claude Code has already written the entry that
triggered it — the subprocess spawn + socket round-trip costs tens to hundreds of
milliseconds. If the daemon dates the transition from *when it processed the hook*
(`time.Now()`), `StatusSince` sits **ahead** of the transcript event it represents:

```
T0   Claude writes the user prompt to the transcript        (transcript ts = T0)
T0+δ Claude spawns the UserPromptSubmit hook
T0+Δ daemon processes the hook, stamps StatusSince = now     (Δ ≫ δ, wall clock)
```

Now any **fast follow-up** that lands in the `(T0, T0+Δ]` window gets a transcript
timestamp **earlier than `StatusSince`**, even though it genuinely happened
*after* the prompt. The reconciler's "anything after `StatusSince`?" test discards
it as stale, and the hookless recovery never fires. Worse, once the user stops
interacting, nothing else is ever written — so the chip is stranded **forever**.

This is a **clock-ordering** bug, not a clock-*skew* bug: both timestamps come
from the same system clock. The defect is sampling `StatusSince` at the wrong
*causal point* (hook processing) instead of the point the reconciler compares
against (the transcript entry).

### The fix: anchor `StatusSince` to the transcript

`transcript.AnchorTime` returns the timestamp of the newest turn entry in the
tail. For an edge **into working/permission**, `handleHook` dates the transition
from that, falling back to wall-clock `now` only when the tail holds no
timestamped entry. This puts `StatusSince` on the **same event stream, sampled at
the same causal point**, as the signals the reconciler later compares against it,
so a genuinely later signal always reads as later — independent of hook latency.
It is also more accurate for the permission-decay age and the resume check, which
use the same `StatusSince`.

Why anchor instead of just dropping the `> StatusSince` gate? The gate is what
makes the recovery non-flapping (a healed edge re-stamps `StatusSince`, so the
triggering entry can't re-fire it). Keeping the gate and fixing the value it
compares against preserves that property while closing the race at its source.

## 2.1 The opposite direction: the flush-ordering race (into idle)

The transcript anchor is exactly **wrong** for an edge **into idle** (`Stop` /
`SessionStart`), because the two streams race the other way. A `Stop` hook fires
right after the turn's final assistant message is generated, but Claude Code
flushes that message's `.jsonl` line a beat **later** — so at the instant the
daemon processes the `Stop`, the newest entry *on disk* is an **earlier** turn
entry:

```
T0    Claude writes a user tool_result                  (on disk, ts = T0)
T0+9s Claude generates the turn's final assistant msg   (ts = T0+9s, NOT yet flushed)
T0+9s Claude spawns the Stop hook
T0+Δ  daemon processes Stop, AnchorTime sees only T0  →  StatusSince = T0
T0+Δ+ε Claude flushes the final assistant line          (now on disk, ts = T0+9s)
```

Next reconcile tick, `NewestSignal` finds the assistant message at `T0+9s`, which
is **after** `StatusSince = T0`, so the idle→working "resume-activity" rule reads
the **completing turn's own last message** as "the session resumed" and re-greens
the chip. It then latches green until the next `Stop` — which flaps the same way.
The non-flapping guarantee (§2) is defeated because the triggering entry is *newer*
than the anchored `StatusSince`, not older.

The race-free anchor for an idle edge is wall-clock `now`: a `Stop` can only fire
**after** the turn truly ended, so the turn's own messages — all dated before
`now` — cannot re-trigger, while a genuine resumption (an orchestrator woken by a
teammate) is dated after `now` and still does. `transcript.AnchorSince` folds both
directions into one policy:

```go
now := time.Now()
info.StatusSince = transcript.AnchorSince(info.Transcript, now, status == state.StatusIdle, s.tun.TailBytes)
// into working/permission → anchor to the triggering entry (§2, the skew fix)
// into idle               → wall-clock now            (the flush-race fix)
```

---

## 3. The catalog

| id | scenario | hook sets | then | reconciler must |
|----|----------|-----------|------|-----------------|
| **H1-quick-interrupt** | prompt → green, then Ctrl+C ~1 s later | `working` | interrupt marker, no `Stop` | demote → **idle**. The marker (T0+1s) is newer than the anchored `StatusSince` (T0). A wall-clock `StatusSince` would sit after it and strand green. |
| **H2-slow-interrupt** | interrupt minutes into a turn | `working` | activity, then interrupt marker | demote → **idle** (always worked; contrast case). |
| **H3-busy-no-interrupt** | a long, active turn | `working` | tool_results + assistant text, no marker | stay **working** — demotion keys on the marker, never a no-activity TTL, so a busy session is never falsely decayed. |
| **H4-local-command-after-idle** | `Stop` → orange, then `!bash` | `idle` | `<bash-stdout>` user entry | stay **idle** — a `!`/`/` local command is not agent activity, and the anchor must not let the final assistant message (at `StatusSince`) read as fresh. |
| **H5-teammate-resume-after-idle** | orchestrator idle, teammate lands a result | `idle` | tool_result after the chip went idle | resume → **working**. |
| **H6-delegating-quiet-transcript** | idle main thread, subagents in flight | `idle` | (nothing written) | promote → **delegating** from the subagent count, even with a stale mtime the activity pre-gate would skip. |
| **H7-stop-final-message-flush-race** | `Stop` → orange, then the turn's own final assistant message flushes late | `idle` | assistant message dated before the `Stop` lands on disk after it | stay **idle** (§2.1). The idle edge anchors `StatusSince` to wall-clock `now`, not to the stale on-disk entry, so the late-flushed message (dated before `now`) is not "activity after idle" — without this the chip re-greened after every `Stop`. |

### Adding a hazard

When a new timing scenario surfaces:

1. Add a row to `TestTimingHazards` with a new `Hn-…` id, modeling the three
   phases (entries at hook time → entries appended before reconcile → expected
   color).
2. Add the matching row to the table above with a one-line "why it's tricky".
3. If it exposes a new skew/ordering defect (not just a new shape of an existing
   one), extend §2.

The test is the source of truth; this document is its index.
