# Claude `PreToolUse` call-identity plan

> **Implementation status (2026-08-31).** Phases A and B are implemented on
> `feat/auq-prompt-model`. The matcher has not been registered and no live
> acceptance window has started; those remain Phases C and D after deployment.

> **Goal.** Give `AskUserQuestion` and `ExitPlanMode` prompts their exact
> `tool_use_id` when the red opens, so an answer can clear that exact prompt on
> its `PostToolUse` edge without waiting 10–15 seconds for the transcript latch.
>
> **Safety rule.** `PreToolUse` is an identity proposal, never a new source of
> red. `PermissionRequest` remains the only hook that opens Claude attention.
> Any join failure falls back to the shipped lazy latch unchanged.

## 1. Evidence and corrected assumptions

The installed Claude Code is `2.1.252`. The repository's binary-derived schema
was last verified against `2.1.222`; it and the current official reference agree
that `PreToolUse` carries `tool_name`, `tool_input`, and `tool_use_id`, while
`PermissionRequest` has no `tool_use_id`.

Two earlier Phase 5 gates are now settled by the official contract:

- [`PreToolUse` runs after parameters are created and before the tool call](https://code.claude.com/docs/en/hooks#pretooluse).
- [Permission evaluation runs `PreToolUse` before the permission prompt](https://code.claude.com/docs/en/permissions#extend-permissions-with-hooks).
- A matcher is a tool-name regex, and pipe alternation is supported; the scoped
  matcher is therefore `AskUserQuestion|ExitPlanMode`.

What is still empirical is the join rate: another `PreToolUse` hook can modify
input before the later permission edge, duplicate registrations can redeliver an
edge, and parallel identical calls can share `(writer, tool, input-hash)`. Those
cases determine how often the fast path wins, not whether it is safe to ship,
because a non-unique join never binds.

## 2. Design

### 2.1 Stage, then join

Add a bounded, in-memory candidate set to each Claude root:

```text
preToolCandidates[writer] = [
  {tool, inputHash, callID, observedAt}
]
```

On a matcher-limited `PreToolUse`:

1. Normalize the writer exactly as every other hook does.
2. Require an allowed tool, non-empty input hash, and non-empty call ID.
3. Deduplicate an exact call-ID redelivery.
4. Quarantine a call ID claimed by two writers instead of choosing one.
5. Stage the candidate only. Do not create a prompt, change runtime/attention,
   or write provider content.

On `PermissionRequest`:

1. Find unexpired candidates matching **all three** positive correlators:
   writer, tool, and non-empty input hash.
2. If exactly one distinct call ID matches, consume it and open the normal
   `PendingPrompt` with `CallID` set and `Latch=CallLatchBound`.
3. If zero or several distinct IDs match, open the current unbound prompt and
   let `latchPendingCalls` do exactly what it does today.

On a zero-match where same-writer/tool candidates exist, discard those stale
candidates. Keeping a pre-modification hash would let a later call with that old
shape inherit the wrong ID. A collision also invalidates any already-bound or
proposed claim to the ID before quarantining it.

A `PermissionRequest` with no staged evidence emits no join-miss counter. This
keeps an inert binary deployed before registration from flooding the denominator;
hash drift with an actual candidate emits `pretooluse_join_miss`, while TTL loss
emits `pretooluse_expired` separately.

The join never uses temporal nearest-neighbor or FIFO ordering. Parallel
byte-identical calls are genuinely indistinguishable at `PermissionRequest`, so
ordering them would turn latency work into a missed-RED risk.

### 2.2 Candidate lifecycle

- Cap candidates at 32 per writer, matching the prompt-set bound.
- Expire candidates after 60 seconds; the two edges should be adjacent, and an
  old candidate is more dangerous than useful.
- Remove a candidate on a matching `PostToolUse` even if no permission prompt
  opened (the normal auto-approved case).
- Clear the root's candidates on `SessionStart`, exact session rotation, forget,
  and close.
- Do not persist candidates. A restart between the two edges is a join miss and
  safely returns to the lazy latch.

### 2.3 Existing paths that remain authoritative

- `PermissionRequest` alone still opens every red.
- The transcript latch remains enabled for every unbound prompt.
- An ID-matched `PostToolUse` closes exactly one prompt; a different ID holds.
- `ResolveKindForCalls` remains the no-hook and decline fallback.
- Confirmed call IDs now persist in `pending_prompts.call_id`, so an exact answer
  that lands during daemon downtime can be subtracted at hydrate.
- The fanout floor is lifted only for an exact ID match, as today.

## 3. Implementation sequence

### Phase A — inert protocol support (implemented)

1. Add `PreToolUse` to the Claude adapter's recognized hook vocabulary. Keep the
   legacy `statusFromHookEvent` mapping unchanged: staging identity is not a
   working/color edge.
2. Add the bounded candidate set and pure helpers for stage, expire, consume,
   collision refusal, and post-result cleanup.
3. Join candidates inside the existing `PermissionRequest` branch before
   `openPendingPrompt`.
4. Add finite rules/diagnostics:
   `pretooluse_staged`, `pretooluse_join_hit`, `pretooluse_join_miss`,
   `pretooluse_join_ambiguous`, `pretooluse_call_id_collision`, and
   `pretooluse_expired`.
5. Keep all fields content-free. The raw `tool_input` still dies at
   `parseHookPayload`; only its bounded hash crosses RPC.

### Phase B — regression gates (implemented)

Add tests proving:

- `PreToolUse → PermissionRequest → PostToolUse` binds at open and closes the
  exact prompt without an `Observe` tick;
- `PreToolUse` by itself never raises red;
- missing/mismatched/modified hashes fall back to an unbound prompt;
- two parallel identical calls are ambiguous and neither is guessed;
- same-shape calls from different writers never cross-bind;
- duplicate delivery of one call ID is idempotent;
- an unrelated or auto-approved call consumes/expires its candidate without
  affecting attention;
- candidate TTL/cap and session rotation cannot leak identity;
- the existing lazy-latch, fanout, parallel-prompt, restart, and privacy tests
  pass unmodified.

### Phase C — matcher-limited registration (not yet applied)

After the binary containing Phase A/B is deployed, add this single handler to
the existing Claude `hooks` object (user-scoped if the feature should cover all
projects):

```json
"PreToolUse": [
  {
    "matcher": "AskUserQuestion|ExitPlanMode",
    "hooks": [
      {
        "type": "command",
        "command": "switchboard-ctl hook PreToolUse",
        "timeout": 2
      }
    ]
  }
]
```

Do not register an unmatched handler. Do not return a permission decision or
`updatedInput`; switchboard is an observer and exits successfully with no JSON,
so it cannot allow, deny, defer, or rewrite the tool call.

Use Claude Code's `/hooks` view to confirm there is exactly one effective
registration. User and project registrations both firing would be safe after
deduplication but would distort the join counters.

### Phase D — live acceptance (pending registration)

For one normal workday, read the content-free counters and history:

- `pretooluse_join_hit / (hit + miss + ambiguous)` across the scoped matcher,
  with `pretooluse_expired` reported separately;
- call-ID collisions (target: zero);
- answer-to-clear p50/p95 and red episodes under 2 seconds;
- an ID clear followed within 30 seconds by a new permission wait for the same
  writer/tool (false-clear proxy; target below 1% and manually inspect every
  occurrence).

Low hit rate means remove the registration or investigate hash drift; it does
not require rollback of the inert code. Any collision or false-clear signal
disables the join while leaving the lazy latch available.

## 4. Definition of done

- The prompt opens on `PermissionRequest` carrying the exact ID learned from its
  preceding `PreToolUse` whenever the composite join is unique.
- Fast answers—including the old 6.1-second and 14.9-second tail—clear on the
  matching result edge rather than waiting for latch confirmation.
- A join miss, ambiguity, duplicate registration, restart, modified input, or
  missing field cannot clear a prompt earlier than today's code.
- No raw prompt, question, answer, plan, tool input, or tool output crosses the
  RPC boundary or enters diagnostics/state.
- Full unit/race suites pass before registration; live metrics pass before the
  matcher is widened beyond the two human-input tools.
