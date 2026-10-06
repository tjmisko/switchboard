# Phase 4 — OS process birth token (#97)

[Master](README.md). Depends on Phase 3. **Specification:**
[issue #97](https://github.com/tjmisko/switchboard/issues/97). Read its Scope
section; this plan maps it onto the code. Phase 0b was the stopgap for death
callbacks, and this phase is the real fix.

## Acceptance criteria (from #97, verbatim)

- A reused PID, including one on the same tty with the same executable, cannot
  inherit the previous session's status or provider binding.
- Late observations, callbacks, and timers for an old lifetime cannot modify or
  remove its replacement.
- A daemon restart preserves display timestamps while revalidating process
  lifetime before restoring authority.
- Unavailable birth tokens have explicit, tested behavior.
- OS seam/conformance tests and deterministic lifecycle tests cover reuse and
  restoration without relying on timing races.

## Identity today (main a06439f)

- **Every layer uses a bare PID.** `osproc.Info` has no birth field
  (`internal/osproc/osproc.go` ~:26-34). `proc.Reader.Read` never reads
  `/proc/<pid>/stat` (`internal/proc/proc.go` ~:57-84). The scanner's `seen`
  set and the store are keyed by PID.
- **`StartedAt` is a clock reading, not a birth token.** It is `time.Now()` at
  resolve time (`internal/mapping/mapping.go` ~:68). `appear` **copies it
  onto a reused PID of the same agent** (`cmd/switchboard/main.go` ~:240-243),
  so every `RootKey{PID, StartedAt}` fence passes on reuse. That includes:
  - the Codex binding registry,
  - the observers' `Forget`,
  - timers (`codex_hook_transitions.go` ~:174-200, ~:1084-1100),
  - async applies (`agent_observation.go` ~:540, ~:1040, ~:1211).
- **Restart revalidation checks the agent kind, not the process.**
  `processIsSession` (`main.go` ~:418-423) accepts any process that classifies
  as the same agent. A herdr-only agent passes on TTY equality alone, which is
  the issue's "same tty" case.
- **The pidfd can watch the wrong process.** It is opened by PID after the
  scanner's read (`internal/osproc/source_linux.go` ~:74), so a reuse inside
  that window watches the replacement.
- **Hook attribution never checks lifetime.** It walks PPIDs and matches
  `m[pid]` (`internal/rpc/rpc.go` ~:1547-1561).
- **herdr discovery caches by PID.** `announced` is keyed by PID, and the pane
  PID cache is revalidated by TTY only (`cmd/switchboard/herdr_discovery.go`
  ~:95-170). This covers Pi inside herdr.

## Token

- **Linux.** `/proc/<pid>/stat` field 22 (`starttime`, parsed after the
  **last** `)`) together with `/proc/sys/kernel/random/boot_id`.
- **darwin.** `proc_pidinfo(PROC_PIDTBSDINFO)` `pbi_start_tvsec/usec`, when #13
  lands. Inferred, not yet checked against the code.
- **Plumbing.** Read it in `proc.Reader.Read`, carry it through `proc.Info` →
  `osproc.FromProc` → `osproc.Info.Birth` (opaque). The rpc PPID walk uses
  `FromProc` too, so hook attribution gets the token for free.
- **Where it applies.**
  - `RootKey` carries it.
  - `appear` inherits state only on a token match.
  - Watch re-reads the token after `PidfdOpen`. The pidfd pins the process, so
    a match proves which lifetime is being watched, and Watch/Stop are keyed by
    lifetime.
  - Timers and async results compare tokens.
  - herdr discovery caches and Pi's PID are validated the same way.
- **When the token is unavailable** (darwin stub, a masked `/proc`, an old
  mirror): the value is empty, which means "unverified". It never matches,
  never inherits authority, and the session is reported as degraded in
  `explain`.

## Persistence

`state.json` stays at schema v3. Add a `birth` field, `omitempty`, host-local
and out of federation. `started_at` stays the display timestamp. On restart,
restore display state, but restore authority only after the live token
matches.

## Work units

1. Token read and conformance test (`internal/proc`, `internal/osproc`,
   `internal/conformance`).
2. `RootKey` and `appear` inheritance, with deterministic reuse tests using a
   fake proc source.
3. Watch keyed by lifetime and re-checked after open; death callbacks; timers
   and async results.
4. Restart revalidation; herdr and Pi caches.
5. The degraded path and its `explain` reason.

## Tests

Every acceptance criterion above becomes a test. Use a fake proc source that
can reuse a PID on demand; never race real processes.

## As built

One PR, `feat/process-birth-token`.

- **Token.** `boot_id:starttime`, read in `proc.Reader.Read` (and alone by
  `proc.Birth` for the watch re-check). A missing stat or boot id leaves it
  empty without failing the read. `osproc.CompareBirth` is three-valued:
  same, different, unverified.
- **Lifetime.** `osproc.Lifetime{PID, Birth}` keys `Watch`/`Stop`, the
  scanner's `Forget`, herdr's pane cache and announcements, and the daemon's
  `forget` callback. The store map stays keyed by pid: one pid holds one
  lifetime at a time, and every entry carries its `Birth`.
- **Inheritance.** `admitRoot` inherits only the same agent and the same
  verified lifetime. A reused pid gets a fresh `StartedAt`, so the
  `StartedAt` fences in the WM and reconcile paths separate lifetimes too; a
  provably dead prior has its lane closed there.
- **Unavailable token.** Never matches: no inheritance, `Watch` refuses it
  (`osproc.ErrUnverifiedLifetime`), death callbacks leave it to the liveness
  sweep, which keeps the classifier rule for it, restart drops it without a
  `session_end` and rediscovers it fresh, and `explain` reports
  `lifetime_unverified`. Hook attribution keeps its old behaviour for it.
- **Persistence.** `birth` on each session, `omitempty`, cleared from
  federation frames in both directions.
- **Upgrade.** A `state.json` written before this phase carries no token. The
  first restart after the upgrade trusts such a session on first use: when a
  token is readable now and the process still classifies as the session (the
  pre-#97 check), it is restored, start time, name and status included, and
  adopts the live token, which is persisted. From then on every restart and
  death is fenced by that token. A pid reused while the daemon was down by
  the same agent passes that one restart, as it did before #97. With no token
  readable now, the session is dropped and rediscovered fresh, as above.
- **Hook attribution.** A ppid walk that reaches a tracked pid now held by
  another lifetime drops the hook.
