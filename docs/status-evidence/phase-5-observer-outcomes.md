# Phase 5 — explicit observer outcomes and change-driven refresh (#98)

[Master](README.md). Depends on Phase 4. **Specification:**
[issue #98](https://github.com/tjmisko/switchboard/issues/98). Read its Scope
section; this plan maps it onto the code. Related: #74 (idle flicker), #51
(staleness bound). Phase 0a already fixed the Claude scan that renewed
freshness on failure.

## Acceptance criteria (from #98, verbatim)

- Temporary failures neither fabricate idle nor extend freshness; fresh
  evidence recovers promptly after expiry.
- Unsupported coverage and authoritative reset produce distinct explanations
  and behavior.
- Unchanged polls reuse extraction results; hooks and file replacement/
  truncation trigger reevaluation.
- Weak transient idle evidence does not fragment active intervals, while
  explicit stops and resolutions are not delayed.
- Fake-clock transition tests, stale-event race tests, and operation-count
  tests verify correctness and reduced unchanged-poll work.

## The contract today (main a06439f)

`Observe(ctx, RootRef, now) (Observation, error)`
(`internal/provider/provider.go` ~:46-51). An outcome is spread across the
error, an empty `RootID`/`Nodes`, `Complete` and `Diagnostic`
(`internal/agentgraph/types.go` ~:182-201).

- **Claude** returns a set of errors (`ErrMissingSession`, `ErrSuperseded`, …)
  plus the partial graph. Phase 0a stopped that graph from being newly dated.
- **Codex** never errors for unavailability (`internal/provider/codex/observer.go`
  ~:375-418). It returns one of:
  - `Complete:false` with "process start identity unavailable";
  - an empty `RootID` with the binding diagnostic;
  - a pending placeholder;
  - the cache, with its **original** `FreshUntil`.
- **Coordinator** `observeAt` (`cmd/switchboard/agent_observation.go`
  ~:382-432) applies the result even when there is an error. An empty result
  goes to `snapshot_pending` / `exact_binding_unavailable` → `expireCurrent`.

## Unchanged files read on every tick

- **Claude, every tick.** `NewestRuntimeSignal` reads a 128 KiB tail of the
  main transcript (`observer.go` ~:1603). `SubagentsForTranscript` reads a
  128 KiB tail of **every** `agent-*.jsonl` (`internal/transcript/subagents.go`
  ~:236-258).
- **Claude, while a prompt is pending.** Separate tail reads per writer
  (~:1258, ~:1282, ~:1366, ~:1559).
- **Codex.** `pollCodexStoppedRoot` reads a 256 KiB rollout tail on every tick
  once hooks have been quiet for 90 s (`codex_transcript_poll.go`).
  `scanCodexUsageLimit` is already limited to one pass every 5 s and caches by
  size and mtime.
- **Pi.** Phase 1.7 adds a session-tail read on restart.
- **Already incremental.** The fan-out parent and workflow journals (forward
  cursors), and the Codex rollout collector (offset cursor).

## Delays today

None of these values came from replay.

- `transcriptStopQuietWindow`, 90 s: Claude transcript-inferred idle. A hook
  `Stop` is not delayed.
- `callLatchConfirmGrace` 2 s / `callLatchSkewGrace` 3 s.
- `codexTranscriptQuietWindow`, 90 s, which then sets a made-up
  `FreshUntil = now + 90s`.
- `codexHookStartSettle` 250 ms, `codexHookApprovalGrace` 30 s,
  `codexHookLegacyGrace` 500 ms.
- `DefaultWaitClassification` 30 s, `IdleTitleGrace` 15 s, herdr `DefaultGrace`
  10 s.

## Work units

1. **Outcome type.** `Observation.Outcome ∈ {Usable, Unavailable, Unsupported,
   Reset}`. `Complete` keeps its omission meaning. Map each observer:
   - Codex no-identity → Unsupported;
   - Codex no-binding and the pending placeholder → Unavailable;
   - Codex thread rebind → Reset;
   - Claude scan failure → Unavailable, carrying the prior graph.
2. **Coordinator.** Switch on the outcome in `observeAt`:
   - Unavailable keeps the prior graph to its original deadline;
   - Unsupported and Reset get their own explanation reasons (Phase 2's
     enum), and Reset drops the graph.

   An error alone never publishes.
3. **Shared tail cache.** Generalize `codexLimitScan.read`
   (`cmd/switchboard/codex_usage_limit_scan.go`) into one cache keyed by
   (dev, inode, size, mtime). It adds the inode the current cache lacks.
   - Users: `NewestRuntimeSignal`, `SubagentsForTranscript`, the
     pending-prompt reads, `ReadRolloutState`, the Pi session tail.
   - Invalidated by: a hook for that root, a binding change, `Forget`, a
     generation bump, and truncation (size shrinks).
   - No I/O under the store lock. A dedicated lock is fine, but keep reads
     outside it if contention shows.
4. **Confirmation only on inferred transitions.** These are transcript- or
   rollout-inferred idle and the herdr/title fallbacks. Explicit hook `Stop`
   and authorized resolutions publish immediately. Derive any new bound from
   replay of the history log (#74 has the data shape). Do not copy the
   existing constants.

## Tests

Every acceptance criterion above becomes a test. In addition:

- operation counts: an unchanged poll does one stat per file and no read;
- a hook invalidates the cache entry for its root;
- a replaced file with the same size and mtime but a new inode is re-read.
