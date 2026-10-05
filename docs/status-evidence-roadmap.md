# Status evidence roadmap: Pi parity, then #95–#98

Plan, 2026-10-05. Owner decisions: Pi first; Pi tracked with or without herdr;
Pi red on any open dialog; Pi subagents deferred. Issue order after Pi is the
issues' own: #95 → #96 → #97 → #98.

## Where things stand

**Pi** (`AgentKindPi`) exists only through herdr. herdr discovery takes the
pane's foreground process-group leader as the session PID
(`cmd/switchboard/herdr_discovery.go:96-132`); the root is `herdr:<terminal>`,
not Pi's session; status is herdr's four states (`internal/state/herdr.go:139`).
The Pi extension (`integrations/pi/switchboard.ts`) sends only `agent_start`
and a failed `agent_settled`; rpc keeps the usage-limit verdict and drops the
session id and transcript path (`internal/rpc/rpc.go:746-749`). Pi outside
herdr is never discovered and its hooks find no ancestor and are dropped.
Missing against Claude/Codex: enrichment block, exact identity, rotation,
transcript, hook-driven status, naming, usage/pricing, history lanes.

**Arbitration** happens in three places, none of which records why:
admission (`shouldApplyObservation`, source rank app-server/transcript 4 >
hook 3 > rollout 2 > restored 1, bypassed by Codex hook ownership and the 90 s
rollout correction); projection (`herdrAuthority` overrides the graph, with a
Codex attention exception that reads `time.Now()`, `internal/state/herdr.go:78-84`);
publication (`ProjectPublished`, the usage-limit overlay). Journal decision
lines exist for Claude and herdr edges only.

**Process identity** is a bare PID everywhere. `StartedAt` is `time.Now()` at
discovery and is copied onto a reused PID of the same agent
(`cmd/switchboard/main.go:235-243`), so every `RootKey` fence passes on reuse.
A late death callback is PID-keyed and can end the replacement.

**Observers** have no outcome type. Claude's failed fan-out scan still returns
a graph stamped `ObservedAt=now` with a new `FreshUntil`, and the coordinator
applies it: a failed read renews authority today. Claude re-reads uncached
128 KiB tails of the main transcript and every subagent transcript per tick.

## Phase 0 — two correctness fixes that need no design

- **0a. A failed Claude scan must not renew freshness.** On `scanErr`
  (`internal/provider/claude/observer.go:425-435`) return the prior observation
  with its original `FreshUntil`. Small, and it is #98's first acceptance
  criterion; doing it now stops the bug living through four phases.
- **0b. Death callbacks fenced by session lifetime.** The death closure ends
  `(pid, StartedAt)` rather than `pid`, and `Stop(pid)` cannot cancel a newer
  watcher. Stops the worst PID-reuse effect before #97's full token work.

## Phase 1 — Pi as a first-class provider

Built as a hook-driven provider like Claude's hook FSM, with its precedence
against herdr isolated in one function so #96 can replace it wholesale.

1. **Extension v2** (`integrations/pi/switchboard.ts`), content-free, TUI-only:
   - `session_start {reason, previousSessionFile}` → `SessionStart`
     (`startup|resume|new|fork|reload`); `session_shutdown {reason}` → `SessionEnd`.
   - `agent_start` → `UserPromptSubmit`; `agent_settled` → `Stop`, or
     `StopFailure` with the error text after an error stop (ctl reduces it to
     the usage-limit verdict, as now). `agent_end` is never a stop: a retry
     can follow it.
   - `ui_prompt_start`/`ui_prompt_end` and the `herdr:blocked {active}` bus
     event → `PermissionRequest` / `PermissionResolved` with an open count.
   - `tool_execution_start/end` → `PreToolUse`/`PostToolUse` with tool name
     only (activity edges, quiet-window reset).
   - `message_end` (assistant) → token counts, cost, provider and model; no text.
   - On `session_start`, report `ctx.isIdle()` so a reload mid-run is not idle.
   - Install as a symlink to the repo file, not a copy (today it is a copy).
2. **Discovery without herdr.** `discovery.Classify` gains `AgentPi`: Pi sets
   `process.title = "pi"` (`pi-rpc` in RPC mode), so comm is `pi` and exe is
   node. Json/print children (subagent tools) share comm `pi`, and the title
   rewrite erases argv, so the interactive gate must be something else. The
   candidate is "fd 0 is a tty", to be verified on a live Pi before
   implementing. herdr discovery attaches its `Herdr` block to a
   scanner-found PID instead of creating a second session.
3. **State and wire.** `Session.Pi *AgentInfo` (session id, transcript, status,
   since), `Enrichment()`/`AgentBlock` cover it, and the root ID becomes Pi's
   session UUID. Additive in schema v3; `docs/state-schema.md` updated.
4. **Pi hook reducer.** `UserPromptSubmit`/tool edges → working; open dialog
   count > 0 → permission; `Stop` → idle; `StopFailure` → idle plus usage
   limit; rotation (`SessionEnd` then `SessionStart` with a new id) rebinds the
   root and clears conversation-bound display state. Hooks match their session
   by process ancestry, as Claude's do.
5. **Precedence against herdr (interim).** Fresh Pi hook evidence wins. herdr is
   the fallback when no Pi hook has arrived since discovery (extension missing)
   or the hook evidence has expired. One function, `piStatusAuthority`, owns
   this.
6. **Restart.** Read the Pi session file tail (last assistant `stopReason`) to
   seed idle/working once, as Codex now does from `thread.path`; until a hook
   or herdr reading arrives, a restored status carries no live authority.
7. **Naming, usage, history.** Pi's own session name (`session_info` entry)
   when present, else the existing fallback. Usage counts feed the usage
   tracker under a Pi pricing identity. Hook edges go to the history sink and
   `diagnose` shows the Pi binding.

Deferred: Pi subagents (owner decision), and auto-retry, which is invisible
to extensions: an error stop followed by another `turn_start` before
`agent_settled` is the only signal, and `agent_settled` already absorbs it.

## Phase 2 — #95 per-root decision record and `switchboard-ctl explain`

An in-memory `Decision` per `RootKey` + provider session id: selected source,
evidence kind, reason code, observed-at, fresh-until, and up to N rejected
candidates with reasons. It is written at the admission early-returns, inside
the `store.Apply` that commits a graph, in `applyHerdrPane`, in the Pi
precedence function, and in `expireCurrent`. The usage-limit overlay is
computed on demand at explain time, since it exists only at publication.
Exposed over the existing diagnostics RPC. Text and JSON come from the same
struct, and the record holds no content. Phase 2 also characterizes today's
precedence in tests that #96 must keep passing or explicitly change.

## Phase 3 — #96 one pure resolver

`Resolve(candidates []Candidate, prior Decision, now time.Time) Decision`,
outside `agentgraph.Reduce`. Candidate kinds: exact lifecycle event (Claude,
Codex and Pi hooks), provider snapshot (app-server, Claude transcript graph),
correlated transcript evidence (rollout/session tail), coarse terminal reading
(herdr; valid only when herdr's agent matches the tracked agent and pane),
partial hook edge, restored last-known. Each kind declares what it may
establish and what it may resolve; for example, only a provider event or
snapshot may resolve an open approval. It replaces `projectStatus`,
`herdrAuthority`, the Codex attention exception, the source-rank gate and
`piStatusAuthority`. The clock is always passed in.

## Phase 4 — #97 process birth token

`osproc.Info.Birth`, opaque: Linux `/proc/<pid>/stat` field 22 plus
`boot_id`; on darwin `pbi_start_tvsec/usec` when #13 lands. It is read in
`proc.Reader`, so the hook path gets it too. `RootKey` carries it, `StartedAt`
stays the display time, `appear` inherits only on a token match, `pidfd` is
re-checked against the token after open, and the herdr pane PID cache and
Pi's PID are validated the same way. State is additive (`birth`, omitempty);
an empty token means unverified, never a match. Kept host-local, out of
federation.

## Phase 5 — #98 observer outcomes and change-driven refresh

`Observation.Outcome ∈ {Usable, Unavailable, Unsupported, Reset}`; `Complete`
keeps its omission meaning. The coordinator keeps the prior graph to its
original deadline on Unavailable, explains Unsupported, and drops on Reset.
One shared tail cache keyed by (dev, inode, size, mtime), generalizing
`codexLimitScan.read` and gaining the inode it lacks. Its users are
`NewestRuntimeSignal`, `SubagentsForTranscript`, the Codex rollout reads and
Pi session tails. It is invalidated by hooks, binding changes, `Forget` and
generation bumps. Confirmation delays apply only to inferred transitions, with
bounds taken from replay. Coordinate with #74 (idle flicker) and #51
(staleness bound).

## Tests to agree before each phase

Named "should … when …". Each one is a candidate for the owner to keep, cut
or change.

- Phase 0: should keep the prior FreshUntil when a Claude fan-out scan fails;
  should not end a replacement session when the old lifetime's death
  callback fires late.
- Phase 1:
  - should discover an interactive Pi outside herdr;
  - should not discover a json-mode Pi child;
  - should show red while an extension dialog is open, and return to working
    when it closes mid-run;
  - should go idle on agent_settled but not on agent_end;
  - should rebind the root on /new and on /resume;
  - should prefer fresh Pi hooks over herdr, and fall back to herdr when no
    hook has arrived;
  - should merge herdr and scanner discovery of one Pi into one session;
  - should seed status from the session file after a restart without live
    authority.
- Phase 2: the #95 acceptance list.
- Phase 3: the #96 list, plus should let a fresh Pi dialog hold red against a
  herdr working reading.
- Phase 4: the #97 list, including a reused PID on the same tty with the same
  executable.
- Phase 5: the #98 list, with an operation-count test that unchanged polls do
  no reads beyond a stat.

## Open questions

- Is fd 0 a reliable interactive gate for Pi? Verify on a live Pi and its
  subagent children before Phase 1.2.
- Do Pi's built-in dialogs (the `/resume` picker, project trust) fire
  `ui_prompt_*`? The docs list only extension dialogs. If they do, idle Pi
  goes red at a picker; decide whether that is wanted.
- Pi pricing: trust Pi's own per-message `cost`, or reprice from the canonical
  rate table as Claude and Codex are?
