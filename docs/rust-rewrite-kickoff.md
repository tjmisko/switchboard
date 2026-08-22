# Kickoff Brief — Agent Watcher (Rust) scaffolding run

**For:** a fresh Claude Code instance · **Written:** 2026-08-12 · **Read this first, entirely, before any tool call.**

> **2026-08-21 supersession.** The Go oracle now has a validated Codex terminal
> slot/conversation split, per-TUI app-server socket, generation-fenced reducer,
> authoritative rename projection, and isolated automatic naming. The Rust port
> must adopt `docs/rust-rewrite-requirements-v0.2.md` FR-S and must not reproduce
> the older one-process/one-session or rollout-only Codex model described later
> in this historical kickoff brief.

---

## Oracle Brief

We are rewriting `switchboard` — a working ~39k-LOC Go daemon that discovers running
AI coding agents (Claude Code, Codex), tracks their status, and lets the user jump to
them — as a new Rust project called **agent-watcher**. The requirements are in
`docs/rust-rewrite-requirements-v0.2.md` in this repo; they are the product of a full
audit of the Go implementation and they supersede an earlier v0.1 draft. Three
decisions are already locked: a **clean break** on the wire contract (no `state.json`
parity, renderers get ported), **Windows in v1**, and **multi-agent stays** (Claude +
Codex, open set).

**The single most important scoping fact: the project owner is hand-rolling the async
spine himself, deliberately, to learn Rust.** Phases 0–5 of his build guide — the
tokio skeleton, the canonical types, the `/proc` probe, the Hyprland IPC wire, the
Claude hook ingest, and the `reduce`/publish core — are **his work, not yours.** You
are not building the daemon. You are building the scaffolding, oracles, and spikes
that make his hand-rolled spine verifiable and de-risked, and then you stop.

---

> **Scope note added 2026-08-12.** A second repo is also migrating:
> `~/Projects/switchboard-dashboard` (~10.2k LOC Go + a framework-free embedded
> JS frontend). Its requirements are in
> `switchboard-dashboard/docs/rust-rewrite-requirements.md`. The two migrations
> are **independent** — the dashboard couples to the daemon only through a
> *process* boundary (exec a provider, read JSON from stdout), so either can be
> rewritten first, or both concurrently. The dashboard work is fully delegable:
> the owner's hand-rolled spine never touches it.

## ⚠ Operating constraints in this environment (measured 2026-08-12)

Nine agents were run as a controlled test on this machine. Two failure modes are
real and both change how work must be delegated here.

**1. Agent replies are unreliable and delayed. Output must go to a file.**
Of five test agents that provably completed their work correctly, **one**
delivered its reply to the parent — and that one lagged its own file write by
about a minute. The other four produced only an "idle" event. The parent
therefore cannot distinguish "still working", "finished but the reply has not
landed", and "died" from the absence of a reply.

> **Rule: every delegated task must write its result to a file as its final
> action.** State the exact output path in the task. **Verify completion by
> polling for the file** (`until [ -f <path> ]; do sleep 5; done` as a background
> Bash task), never by waiting on a reply. A returned summary is a bonus, not a
> channel — treat its absence as carrying no information.

**2. Unbounded research tasks die mid-work; bounded ones succeed.**
Measured, holding everything else constant:

| Shape | Result |
|---|---|
| 1 file write | ✅ |
| 1 read + 1 write | ✅ |
| **4 reads + 150-word summary, capped at 10 tool calls** | ✅ **and high quality** |
| 10–20 reads, open-ended, multi-section report | ❌ ×8, no output at all |

The likely mechanism is subagent context exhaustion — one failed agent left a
57 KB `git log` dump in its task output, having consumed a large share of its
window on a single command before dying. Path style (`~` vs absolute) was tested
and is **not** a factor.

> **Rule: bound every delegated task.** Name the specific files to read (roughly
> ≤6), give an explicit tool-call budget, and cap the output length. If a task
> needs twenty files, split it into four agents of five and have a fifth agent
> merge the four output files.

Tasks below that are large — **#2 (trace capture) especially** — must be
decomposed before being handed to a single agent. #2 as written ("cover H1–H9
and L1–L8") is exactly the shape that failed nine times out of nine; run it as
one agent per hazard family, each writing its own file.

## Required reading, in order

1. `docs/rust-rewrite-requirements-v0.2.md` — the spec. Normative.
1b. `switchboard-dashboard/docs/rust-rewrite-requirements.md` — the dashboard
   companion spec, if you are touching tasks #9–#11.
2. `README.md` — what the Go daemon does today, and the two load-bearing invariants.
3. `docs/state-schema.md` — the v1 wire contract you are replacing (read for its
   *presence semantics*, which carry forward, not its field list, which does not).
4. `docs/timing-hazards.md` and `docs/session-lifecycle-hazards.md` — H1–H9 and
   L1–L8. These are the behaviors the oracle you are about to build must capture.
5. `internal/statustune/knobs.go` — the 19 status rules. Skim; do not port.

Do not read the whole Go codebase. It is large and you do not need most of it.

---

## What you are building, and what you are not

| Build | Do **not** build |
|---|---|
| Trace capture from the running Go daemon (the behavioral oracle) | The tokio skeleton / core loop |
| The trace-replay harness | Canonical types (`Event`, `ProcSample`, `AgentState`) |
| Test fixtures (fake process tree, fake socket peer, golden helpers) | The `/proc` probe |
| The conformance-suite *contract* (the trait-level test spec) | The Hyprland IPC client |
| De-risking spikes (Windows anchor, Codex pane probe) | The Claude hook ingest |
| The v2 schema draft | The `reduce` / status reducer |
| The hostile fake adapter | Any `cargo new` of the real daemon crate |

If a task tempts you across that line, stop and say so rather than proceeding.

---

## Non-negotiables

These come from the audit. Violating one is a defect, not a style difference.

- **AP7 — Discovery is truth; hooks are enrichment.** Existence and liveness come
  from the OS process layer, never from vendor events. A hook for an unknown pid
  creates no session.
- **AP8 — Error costs are asymmetric.** Showing a blocked agent as *not* blocked (a
  "missed RED") is far worse than the converse. When uncertain, stay red.
- **AP9 — Death is observed, never inferred.** Kernel death handles, not tick diffs.
  Liveness is three-valued: alive / gone / **unsupported**. Never collapse
  "unsupported" into "gone."
- **The join is anchored on the session anchor, not on `window↔pid`.** A TUI agent
  owns no window; the window belongs to the terminal. See §4.2 / §4.2a of the spec.
- **"Cannot tell" is never "no."** A log that cannot answer a question returns
  `None`, and `None` must not be read as a negative answer (FR-F4).

### Anti-patterns

- **No inferring liveness from activity silence.** A genuinely idle session that sits
  for hours is alive (hazard L4). Liveness keys on pid death only.
- **No clearing a `permission` status on ambiguous evidence.** A bare `PostToolUse`,
  a tool-name match from a non-owning writer, or a non-tool event all must *hold*.
  These are deliberate guards with no tuning knob.
- **No modelling a session as one agent.** A session is 1 + N independently-blocking
  writers sharing a pid (FR-M1).
- **No modelling a visible Codex terminal as one permanent conversation.** The
  slot survives `/clear`; its replaceable `{thread_id, generation}` binding owns
  every conversation-scoped field (FR-S1/FR-S2).
- **No parsed-JSON golden comparison.** Byte-level, or the fixture is not a tripwire.

---

## Project location

The Rust project is a **new repository**, separate from this Go one — the Go daemon
must keep running and serving the user until milestone M6, and the two cannot share a
build. Default: `~/Projects/agent-watcher`. If the owner wants a different path, that
is a one-line change; confirm before creating it.

**This repo (`switchboard`) is read-only for you except `docs/` and any capture
tooling you are explicitly asked to add.** Do not refactor Go code.

---

## Phase A — Bootstrap & Capture [CURRENT]

- [ ] **#1: Bootstrap the agent-watcher project structure**
  - **Prereqs**: none.
  - **DoD**: `~/Projects/agent-watcher` exists as a git repo containing `CLAUDE.md`
    (all six required sections, seeded from this brief's non-negotiables and the
    spec's §3), `todo.md` (this task graph, migrated), `DONE.md` (empty),
    `session-notes/`, and an initial commit `chore: initialize claude code project
    structure`. **No `Cargo.toml` and no Rust source** — the crate is the owner's to
    create.
  - **Phase**: A
  - **Notes**: The `CLAUDE.md` "Current Phase" is "Phase A — scaffolding for a
    hand-rolled spine." Record D1–D5 from the spec under Architecture.

- [ ] **#2: Capture behavioral traces from the running Go daemon** ⏳ **TIME-LIMITED**
  - **Prereqs**: #1 complete — traces need somewhere versioned to live.
  - **DoD**: A `traces/` corpus in agent-watcher, each trace a timestamped sequence of
    daemon *inputs* (hook payloads, transcript deltas, process samples, WM events) plus
    the Go daemon's *derived* status decisions as the expected output. Coverage:
    at least one trace per H1–H9 and per L1–L8, each named for its hazard. A README
    documents the trace format.
  - **Phase**: A
  - **Notes**: **Do this early.** These traces are unobtainable once the Go daemon
    stops running, and they are the only oracle that survives the clean break (FR-O6).
    Start by extracting from artifacts that already exist — the decision log (lines
    prefixed `status: pid=… session=…`, see `docs/status-color-state-model.md` §5) and
    the activity history under `$XDG_STATE_HOME/switchboard/history/` if enabled. Where
    a hazard is not represented in existing logs, write down what the owner would need
    to *do* to produce it and ask — several (H9's double-Esc, L1's SIGKILL) need a human
    at a keyboard. Do not fabricate traces.

- [ ] **#3: Trace-replay harness**
  - **Prereqs**: #2 complete — a harness with no corpus cannot be validated, and the
    corpus's shape determines the harness's input type.
  - **DoD**: Given a trace, the harness feeds inputs to a pluggable reducer interface
    and asserts on emitted status decisions (`from→to`, rule id, holds). Ships with a
    deliberately wrong stub reducer that it **fails** against, proving the harness
    detects divergence rather than passing vacuously.
  - **Phase**: A
  - **Notes**: The reducer interface here is a *test seam*, not the owner's real
    reducer. Keep it minimal so his `apply` can be dropped in behind it later.

- [ ] **#4: Spike — Windows session anchor** (resolves OQ11)
  - **Prereqs**: none (independent of #1–#3; can run in parallel).
  - **DoD**: A written finding in `docs/spikes/windows-anchor.md`: can a stable,
    comparable identity be resolved for "the console/terminal session a given Windows
    process is attached to," from outside that process? Names the concrete API path if
    yes; states the failure mode and its consequence (Windows ships Observe-only,
    FR-A8 becomes the normal case) if no. Cites documentation, not guesses.
  - **Phase**: A
  - **Notes**: This gates the shape of the canonical `SessionAnchor` type (FR-A6),
    which the owner will define during his Phase 1. Landing this *before* he freezes
    that type is the whole point — late is worthless.

- [x] **#5: Superseded — Codex approval detection via rendered pane** (OQ15 resolved by FR-S5/FR-S6)
  - **Prereqs**: none.
  - **Historical DoD**: A written finding in `docs/spikes/codex-approval-probe.md`: is Codex's
    approval prompt reliably detectable from pane content (`tmux capture-pane` /
    `wezterm cli get-text`), and is the rendered string stable enough across releases
    to pin? Includes an actual captured sample if a live Codex session is available;
    if not, says so plainly and states what is needed.
  - **Phase**: A
  - **Notes**: Approval visibility is the owner's top-priority correctness concern.
    Background: the gap is information-theoretic — Codex's rollout drops approval
    events, so a blocked session is byte-identical on disk to a busy one
    (`docs/codex-investigation.md` §4). The pane is the one side-channel that does not
    require owning the codex process. Constraints if it works: opt-in, own consent
    tier, reduced confidence, version-pinned matcher (FR-E8c).

- [ ] **#6: Draft the v2 wire schema** (FR-O0)
  - **Prereqs**: #4 complete — the schema cannot name the anchor field until the
    anchor's feasibility is known.
  - **DoD**: `docs/v2-schema.md` in agent-watcher specifying every field, its
    presence semantics (omitted vs empty vs null — FR-O0a), the encoder configuration
    (FR-O1a), and a resolution for each v1 ⚠ quirk: neutral `terminal`/`window` blocks,
    `monitor`, the unreachable `unknown` status, the collapsed single `agent` block
    (FR-O0b), and `mem_*` option typing (FR-O1c). Includes a schema version field.
    **A draft for review — do not implement it.**
  - **Phase**: A

- [ ] **#7: Hostile fake adapter — design + conformance contract** (FR-E9c)
  - **Prereqs**: #6 complete — the adapter contract and the schema constrain each other.
  - **DoD**: A specification (not an implementation) of a deliberately un-Claude-shaped
    agent adapter: no file transcript, no hooks, server-backed sessions where one
    process ≠ one session, discovery not by `comm ==`. Plus the conformance assertions
    every adapter must satisfy. Written so it can be implemented against the owner's
    trait once that trait exists.
  - **Phase**: A
  - **Notes**: Claude and Codex are near-twins (Codex copied Claude's hook design),
    so a trait validated only against them bakes in their shared shape as universal.
    This fake is the cheapest defense.

- [ ] **#8: Resolve the fork point and the status-model question** (FR-G3a/b) ⚠ **DECIDES OTHER WORK**
  - **Prereqs**: none — and it should run early, because it constrains #6.
  - **DoD**: A written finding in `docs/spikes/fork-point.md` answering: (a) does
    `internal/writerstate` (branch `docs/writer-state-model`, commit `2040a91`)
    land on `main` before the port begins? (b) what else among the 28 unmerged
    branches changes the status model or the wire contract? (c) a recommended fork
    point, with the re-porting cost of each option stated. **Presents options; does
    not decide** — this is the owner's call.
  - **Phase**: A
  - **Notes**: The Go repo had 261 commits across all refs in the last three weeks.
    `writerstate` is a *reduction* of the shipped 19-rule table (five case rows and
    two rule ids collapse) and is pure and platform-free — the ideal functional
    core. Porting the old table and then the FSM is doing the hardest work twice.

- [ ] **#9: Port the timeline envelope types to Rust** (dashboard DR7/DR8)
  - **Prereqs**: #1 complete — needs a repo to live in.
  - **DoD**: A standalone Rust crate (or module) with serde types mirroring
    `switchboard-dashboard/internal/timeline/types.go` (182 LOC). Round-trips
    `testdata/timeline/2026-06-26-full.json` — deserialize, re-serialize, diff —
    with the fixture as a committed test. Presence semantics preserved (omitted vs
    empty vs null).
  - **Phase**: A
  - **Notes**: This is the highest value-per-hour task in the programme. It is
    near-mechanical, it is validated by a fixture that already "exercises every
    field," and it is the shared vocabulary of both rewrites. It is also genuinely
    delegable — no hand-rolled-spine conflict, since the owner's plan never touches
    the dashboard.

- [ ] **#10: Answer the dashboard's open questions** (DQ1–DQ5)
  - **Prereqs**: none.
  - **DoD**: `switchboard-dashboard/docs/rust-rewrite-requirements.md` §6 updated
    in place with verified answers to the **three remaining** questions: whether
    `sessiondigest` (2,053 LOC) is load-bearing or optional enrichment (DQ2); how
    `web/*.test.js` is run — Node, browser harness, or Go-side runner (DQ3); and
    any coupling beyond the envelope, including env vars, assumed paths under
    `$XDG_STATE_HOME/switchboard/`, and **who writes `/tmp/claude-plan-usage.json`**
    (DQ4).
  - **Phase**: A
  - **Notes**: DQ1 and DQ5 are already answered in that doc — do not redo them.
    `arachne` is confirmed out of scope (separate binaries behind the process
    seam), which fixes the dashboard port at ~6k LOC; `/api/memory` is a provider
    capability, not a `state.json` coupling.

- [ ] **#11: Read and summarize the dashboard's performance invariants** (DR11)
  - **Prereqs**: none.
  - **DoD**: A summary appended to the dashboard requirements doc covering
    `daycache.go`, `docs/incremental-poll.md`, and `docs/instant-day-switch.md`:
    what is cached, keyed how, invalidated when, and which invariants are
    *correctness* rather than *speed*. Flags anything a naive rewrite would lose.
  - **Phase**: A
  - **Notes**: Named, documented optimizations are usually load-bearing. Assume
    cache-invalidation correctness until proven otherwise.

## Phase B — Conformance & fixtures [BLOCKED — do not start]

Blocked on the owner's hand-rolled Phase 1 (canonical types + trait contracts). There
is nothing to write a conformance suite *against* until those traits exist. When
unblocked, this phase builds: the fixture library (fake process tree, fake socket peer,
golden helpers), and the executable conformance suite from #7's contract.

## Phase C — Backend breadth [FUTURE — do not start]

macOS and Windows process/anchor/WM backends; additional WM backends (sway, i3, X11);
additional terminal backends; additional agent adapters. All of it sits behind the
conformance suite from Phase B, which is what makes it safely delegable.

---

## Self-prompting loop

After completing each task:
1. Update the checkbox in `todo.md`.
2. Move the task to `DONE.md` with today's date and a one-line note.
3. Check whether any subsequent task now has all prereqs met.
4. If yes **and it is in Phase A**, begin it.
5. **At the Phase A boundary, stop and summarize.** Do not enter Phase B.

Before starting any task, verify its prereqs are in `DONE.md`. A task checked in
`todo.md` but absent from `DONE.md` is incomplete — ask before proceeding.

---

## Stop conditions

Stop and ask rather than proceeding if:

- A task would require writing the daemon's spine (see the build/don't-build table).
- A spike's answer is "no" — that is a real finding and it changes the spec; surface
  it, do not work around it.
- Trace capture for a hazard needs a human at a keyboard.
- You are about to modify Go source in this repo.
- You are about to create the `agent-watcher` crate itself.
- More than **2 hours** of unsupervised work have elapsed — checkpoint and re-orient
  (autonomy budget: this is a medium-risk run on an unfamiliar domain).

---

## Session end

Write `session-notes/YYYY-MM-DD HH:mm - <short-topic>.md` (under 300 words) with:
Completed / In Progress / Decisions Made / Blockers & Open Questions / Recommended
Next Session. Update `CLAUDE.md`'s Current State. Move completed tasks to `DONE.md`.
Commit: `chore: session notes and state update YYYY-MM-DD HH:mm`.

---

## First message to send

> Read `docs/rust-rewrite-kickoff.md` in `~/Projects/switchboard`, then the required
> reading it lists, in order. Then tell me, in your own words: what phase we are in,
> what you are and are not building, which tasks are ready to start, and what you
> recommend starting with. Do not begin any implementation until I confirm your
> understanding.
