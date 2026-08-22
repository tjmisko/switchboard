# Agent Watcher Daemon — Requirements v0.2

**Status:** Draft · **Supersedes:** v0.1 · **Scope:** v1

> **2026-08-21 Codex architecture amendment.** The validated Go reference now
> models a visible terminal slot separately from its replaceable conversation,
> launches each new Codex TUI against a private app-server Unix socket, and
> observes status, approval, names, and `/clear` rotations through that endpoint.
> Section 5.S below supersedes the older one-process/one-session and
> rollout-only Codex assumptions wherever they conflict.

> **What changed from v0.1.** v0.1 was written as though the problem were new.
> It is not: `switchboard` is a working ~39k-LOC Go implementation of this
> daemon, carrying four years' worth of empirically-discovered behavior in
> `docs/`. This revision reconciles the spec with that implementation. Three
> structural corrections, twelve new requirement areas, one corrected capability
> matrix, and a risk register drawn from hazards already paid for once.
>
> **The three corrections, up front:**
> 1. **`window↔pid` is not the linchpin join and cannot be.** For a TUI agent,
>    no window is owned by the agent's pid. The join is a four-hop chain
>    anchored on the controlling tty (§4.2). v0.1 is missing an entire seam.
> 2. **Existence and status are different facts with different sources.**
>    Discovery owns existence; hooks only enrich status. v0.1's single
>    reliability hierarchy conflates them and inverts the safety property (§4.3).
> 3. **Status derivation is not a precedence fold.** It is a 19-rule case table
>    with asymmetric error costs, per-writer prompt ownership, and hold rules.
>    `apply(state, event) -> state` is the right *shape* and radically the wrong
>    *size* (§5.G).
>
> **Decisions taken (supersede OQ8/OQ9).**
> - **D1 — Clean break.** The Rust daemon defines a **v2 wire contract**; it does
>   not preserve `state.json` byte-parity. Porting every consumer (waybar module,
>   `claude-tui`, `switchboard-ctl`, bar recipes) is therefore **in scope for v1**
>   — they are the only UI, and a daemon nothing can render is not shippable.
> - **D2 — Windows in v1.** All three OSes ship in v1. This makes the
>   session-anchor generalization (§4.2a) and the non-SCM hosting model (FR-I1)
>   hard requirements rather than future work.
> - **D3 — Multi-agent stays.** Claude *and* Codex ship in v1, and the agent set
>   is open. Dropping a working agent would be a pure regression; the discovery
>   join is agent-agnostic, so the marginal cost is one adapter. What D1 buys is
>   the chance to fix how the schema *models* multiple agents (FR-O0b) and to
>   make per-agent capability gaps structural rather than prose (FR-E6).
> - **D4 — Reconcilers are per-vendor; the questions they answer are not.**
>   Each agent gets its own transcript parser. They implement one shared
>   `SessionLog` trait whose methods are *vendor-neutral questions*, so the
>   H1–H9 hazard logic is written once over the answers (FR-F3).
> - **D5 — The adapter interface is prototyped against a third agent now**, not
>   after v1. Claude and Codex are near-twins (Codex copied Claude's hook design;
>   both persist JSONL), so a two-vendor trait will encode their shared shape as
>   universal (FR-E9).

---

## 0. Prior Art — the actual starting position

A rewrite's first requirement is an honest account of what it is replacing.

| Package | LOC | Does the spec plan for it? |
|---|---|---|
| `internal/history` (activity log, pricing, timeline) | 6,581 | **No** — v0.1 NG4 declares it out of scope |
| `cmd/switchboard-ctl` (CLI: focus/cycle/pick/attention/diagnose) | 4,334 | **No** |
| `internal/rpc` (Unix socket: list/focus/subscribe/hook) | 3,903 | **No** |
| `internal/transcript` (log-derived state, subagents, workflows) | 3,381 | Partly — FR-F is a `[SHOULD]` |
| `internal/state` (store + frozen `state.json` contract) | 2,128 | Partly — FR-H has no contract requirement |
| `cmd/switchboard-waybar` + `claude-tui` + `barlayout` | 2,082 | **No** |
| `internal/fanout` / `label` / `projectname` / `statustune` | 3,607 | **No** |
| `internal/terminal` + `internal/wezterm` (Seam 2) | 1,370 | **No** — seam absent entirely |
| `internal/proc` + `osproc` + `wm` + `hyprland` | 3,485 | Yes |
| `internal/discovery` + `detect` + `mapping` | 1,479 | Yes |
| `internal/conformance` + `testsupport` | 1,544 | Partly — NFR-7 implies it, nothing requires it |

Roughly **8.6k LOC of the existing system falls inside v0.1's requirements and
~25.3k falls outside them** — not as unbuilt future work, but as shipped
behavior the spec does not know exists.

- **PA1 [MUST].** The Go implementation is the **reference oracle** for the
  rewrite, not merely prior art. Any behavior difference is a defect in the
  Rust daemon until explicitly ratified as an intentional change.
- **PA2 [MUST].** `docs/behavior-spec.md`, `docs/timing-hazards.md`,
  `docs/session-lifecycle-hazards.md`, `docs/status-color-state-model.md`,
  `docs/subagent-permission-*.md` and `docs/decisions.md` are **normative
  inputs**. Every hazard catalogued there (§11) is a requirement on the Rust
  daemon, not a lesson to rediscover.
- **PA3 [SHOULD].** Where v0.2 and the Go implementation disagree, v0.2 states
  which wins and why. Silent divergence is the failure mode this section exists
  to prevent.

---

## 1. Purpose & Goals

- **G1.** A single, always-on, local view of every AI coding agent on the
  machine and its live state.
- **G2.** Focus/attention analysis: quantify context-switch cost, local and
  opt-in only.
- **G3.** Feed downstream renderers a clean, transformed data stream.
- **G4.** A first-principles learning vehicle for async Rust and cross-platform
  systems programming.
- **G5 [REVISED per D1].** **Define a clean v2 contract and carry the consumers
  across.** The rewrite does not preserve `state.json` byte-parity. It owes
  instead: a deliberately-designed v2 schema that fixes the quirks the v1 schema
  documents as ⚠ (provisional backend-specific blocks, the never-populated
  `monitor`, the unreachable `unknown` status), *and* ported renderers, so no
  user is left without a UI. Consumer migration is a deliverable, not fallout.
- **G6 [NEW].** **Preserve behavior even while breaking the wire.** A clean wire
  break does not license a behavior break. The 19 status rules (§5.G), the H1–H9
  timing hazards, and the L1–L8 lifecycle hazards carry across unchanged; only
  their serialization changes.

> **On G4.** A learning goal inside a requirements document is a real hazard: it
> biases toward hand-rolling and away from correctness-preserving reuse. It is
> legitimate here — but it governs the *build plan*, never the *acceptance
> criteria*. Where G4 and G6 conflict, G6 wins. Concretely: it is fine to
> hand-roll the Hyprland IPC parser for the lesson; it is not fine to ship a
> status reducer that drops the missed-RED guards because modelling them was
> tedious.

### Non-Goals (v1)

- **NG1.** No cloud sync, no telemetry leaving the machine.
- **NG2.** Not a process manager; it observes and actuates focus, it does not
  start/stop/sandbox agents.
- **NG3.** Not building the dashboard.
- **NG4 [REVISED].** ~~No historical analytics engine inside the daemon.~~ The
  daemon **does** own append-only activity recording and retention (FR-N); it
  does not own *querying, aggregation, or presentation* of that history beyond
  the reference `timeline` renderer. v0.1's NG4 contradicted FR-D2/FR-D3/P5 —
  occupancy and switch-event series are time series, and something must write
  and rotate them. That something is the daemon.

---

## 2. Glossary

Terms from v0.1 are retained. Added, because the domain needs them:

- **Terminal slot** — one visible, navigable TUI lifetime. For launcher-owned
  Codex sessions it is identified by a random UUID and carries endpoint plus
  pid/process-start metadata. It survives `/clear`.
- **Conversation** — one provider thread currently displayed in a terminal
  slot. Conversation-scoped name, status, attention, writers/children, pending
  work, and timestamps are discarded when the binding rotates.
- **Conversation binding** — the slot's current provider conversation id plus a
  monotonically increasing generation. A stale id/generation can never mutate
  the current conversation.
- **Session** — the existing generic live-agent record. New code must say
  *terminal slot* or *conversation* when that distinction matters; pid is
  liveness/navigation metadata, not permanent conversation ownership.
- **Writer** — an independently-blocking actor within a session: the main
  thread, or one in-flight subagent. A session is **1 + N writers** that share
  a pid and a chip and can each block on their own permission prompt.
- **Pane** — the terminal sub-surface an agent's tty is attached to. Invisible
  to the window manager.
- **Mux** — the terminal multiplexer/GUI process that owns the window a pane is
  drawn in. Its pid, not the agent's, is what the WM knows.
- **Tier** — a coarse capability level the whole daemon degrades to: *Observe*
  (process layer only, always available) or *Navigate* (focus works; needs both
  a terminal locator and a WM backend).
- **Lane** — one session's interval on the history timeline.
- **Ghost** — a lane left open because a death was never observed; the failure
  mode of inferred liveness (§11.2).
- **Hold** — a status decision to *deliberately not change color* despite an
  incoming event. Holds are first-class decisions and are logged as such.

---

## 3. Architectural Principles

AP1–AP3, AP5, AP6 stand as written in v0.1. Revised and added:

- **AP4 [REVISED — Selection is separate from dispatch].** The distinction is
  correct and worth keeping. The *justification* is not load-bearing and should
  stop occupying a requirements slot: no decision in this document turns on
  vtable cost, and no measurement will ever make it turn on vtable cost. The
  real performance constraint discovered in practice is **lock-hold time**, not
  dispatch (see NFR-1 and §11.6). Keep the one-sentence rule; delete the
  paragraph defending it.
- **AP7 [NEW] — Discovery is the source of truth; hooks are enrichment.**
  Existence, identity, and liveness come from the OS process layer and are
  never inferred from vendor events. Hooks may only enrich *status*. A user who
  deletes every hook from their config must still see every session, its cwd,
  and (on a Navigate stack) its window. This is the property that makes the
  daemon trustworthy, and v0.1's reliability hierarchy inverts it.
- **AP8 [NEW] — Error costs are asymmetric; encode the asymmetry.** Showing a
  blocked agent as not-blocked (a *missed RED*) is far worse than showing a
  working agent as blocked. Rules resolve toward the safe side, deliberately,
  and each such choice is named and logged. A symmetric "best guess" reducer is
  wrong even when it is more often right.
- **AP9 [NEW] — Death is observed, never inferred.** Liveness is a kernel
  signal (`pidfd_open` / `kqueue` / job objects), not a tick-over-tick diff. A
  poll-based backstop exists only to repair a *lost* watch, never as the primary
  mechanism. Corollary: liveness is **three-valued** — alive / gone /
  unsupported — and "unsupported" must never be collapsed into "gone."
- **AP10 [NEW] — The wire contract is frozen and pinned by fixtures.** The
  published schema is a public API. It changes only through the versioning rules
  in FR-O, and a golden fixture fails the build when a field's name, type, order,
  or presence semantics move.
- **AP11 [NEW] — Endpoint attribution is structural.** A visible Codex TUI owns
  one private app-server endpoint. Slot identity precedes process ancestry;
  ancestry is only a fallback for pre-wrapper sessions. Loaded threads on a
  shared daemon are never correlated to visible clients by cwd or recency.

---

## 4. System Overview — corrected

### 4.1 Four seams, not three

v0.1 models three ingestion seams (`sysprobe`, `wm`, `hooks`) plus `logs` and
`idle`. The implementation has **four**, and the missing one is load-bearing:

```
  SEAM 1  os process   enumerate · read · watch-death      (per-OS, compile-time)
  SEAM 2  terminal     tty → pane · pane focus · title     (runtime-detected)   ← ABSENT IN v0.1
  SEAM 3  window mgr   enumerate · focus · subscribe       (runtime-detected)
  SEAM 4  ui / render  bar adapters · TUI · CLI            (out-of-process)     ← ABSENT IN v0.1
       +  hooks        per-vendor authoritative status     (runtime, open set)
       +  logs         transcript-derived status           (per-vendor)
```

### 4.2 The join chain — replacing FR-C2's `window↔pid`

v0.1 calls `window↔pid` "the linchpin join." For agents that run as TUIs inside
a terminal — which is *all* of them — there is no such join to make. The agent
process owns no window. `internal/mapping/mapping.go:63` states the real chain:

```
agent pid ──/proc──▶ cwd, tty, death handle
   tty     ──SEAM 2─▶ pane  (mux pid, pane id, window title)
 mux pid + title ──SEAM 3─▶ window address, workspace
```

Two properties of this chain drive requirements:

- The **tty hop is exact** — kernel-controlled, cannot drift.
- The **`(mux, title)` hop is best-effort** — it returns *nothing* rather than
  guessing on collision, and retries next tick (`docs/decisions.md` #4).
- A terminal that does not expose a mux pid (tmux, whose pane pid is the
  in-pane process) leaves the WM hop unresolvable; that session stays
  Observe-only rather than failing.

**The `window↔pid` capability v0.1 makes a `[MUST]` is real but serves a
different purpose** — it identifies which *terminal* window the WM is showing,
not which agent. Keep it; stop calling it the join.

### 4.2a The session anchor — what D2 (Windows) forces

The chain above is anchored on the controlling **tty**, and the anchor is what
makes it trustworthy: `/dev/pts/N` is kernel-owned, unique, and cannot drift.
**Windows has no tty.** A console process attaches to a console session —
ConHost, or a ConPTY pseudoconsole owned by a modern terminal (Windows Terminal,
WezTerm, ConEmu) — and there is no filesystem path naming it.

So the anchor must become a canonical type, not a string borrowed from Unix:

- **FR-A6 [MUST].** Define a canonical **`SessionAnchor`**: an opaque,
  comparable, per-OS-produced identity for "the terminal session this agent
  process is attached to." Unix produces it from the controlling tty; Windows
  produces it from the console/pseudoconsole identity, resolved by walking the
  process's console attachment (or its ancestry to the hosting terminal) —
  whichever the Windows backend can make stable.
- **FR-A7 [MUST].** `SessionAnchor` is **opaque to the core and to consumers**.
  Nothing may parse it, infer a device path from it, or assume a `/dev/` prefix.
  The v1 schema's warning that `tty` is "an OS-specific literal … treat it as an
  opaque join key" becomes a type-level guarantee rather than documentation.
- **FR-A8 [MUST].** A process with no resolvable anchor is **Observe-only** and
  must remain fully tracked — it has a pid, cwd, and status, and only the
  Navigate join is unavailable. This is already the Unix behavior for a
  non-tty-attached process; Windows will hit it more often.
- **FR-A9 [SHOULD].** The anchor is the *only* per-OS concept in the join. Once
  produced, hops 2 and 3 (anchor→pane, pane→window) are uniform.

> This is the clearest dividend of D1: under parity, Windows support would have
> had to smuggle a console identity into a field named `tty` and documented as a
> device path. A clean break lets the field be named for what it is.

### 4.3 Two hierarchies, not one

v0.1: `hooks > logs > sysprobe/wm heuristics`. Correct for **status**. Wrong,
and unsafe, for **existence**:

| Fact | Authority | Never from |
|---|---|---|
| Session exists | OS process layer (Seam 1) | hooks, logs |
| Session is alive/dead | kernel death signal, backstopped by liveness poll | activity TTL, hook silence |
| cwd, tty, start time | OS process layer | hooks |
| Window/pane location | Seams 2+3 join | hooks |
| **Status** (working/idle/permission/delegating) | hooks > transcript > title heuristic | — |
| Session id, transcript path | hooks (write-once) | — |

---

## 5. Functional Requirements

v0.1's FR-A/B/C/D/E/F/G/H/I are retained except as revised below. New areas
FR-J through FR-R follow.

### Revisions to existing requirements

- **FR-A3 [REVISED — MUST].** Liveness is **event-driven and three-valued**.
  Each tracked pid gets a kernel death handle (`pidfd_open(2)` on Linux;
  `kqueue`/`EVFILT_PROC` on macOS; job object or handle wait on Windows). A
  liveness *poll* runs each reconcile tick solely as a backstop for a watch that
  was never registered or was orphaned by a daemon restart. The process-read
  result distinguishes `Alive` / `Gone` / `Unsupported`
  (`internal/osproc/osproc.go:45-51`); a backend that cannot answer returns
  `Unsupported`, and the core **must not** treat it as death. v0.1's "detect
  appearance and disappearance across ticks" would fabricate deaths on every
  platform whose backend is incomplete — precisely the macOS case.
- **FR-A5 [NEW — MUST].** A recycled pid resolving to a non-agent process is a
  **definitive death**, not a live session (hazard L4b).
- **FR-C2 [REVISED — MUST].** Replaced by the join chain of §4.2. `window↔pid`
  maps a window to its owning **terminal mux** pid. On ambiguity the match
  returns nothing and retries; it never guesses. Title/class fallback matching
  is *not* a general substitute and must be scoped to backends where the title
  is pushed by the terminal (so titles agree by construction).
- **FR-E5 [REVISED — MUST].** Hooks are the preferred **status** source and are
  never a source of existence or liveness (AP7). A hook for an unknown pid
  creates no session.
- **FR-E6 [NEW per D3 — MUST].** **Agent adapters negotiate capabilities too.**
  AP5 currently applies to OS/WM backends; it applies equally per vendor,
  because vendors differ in what is *observable at all*. Concretely: Claude
  records the pending tool call on disk, so a `permission` status can be
  self-healed from the transcript when no clearing hook fires; **Codex does not
  record approval requests in its rollout**, so for Codex `permission` is
  observable *only* while a live `PermissionRequest` hook is wired, and there is
  no transcript backstop. An adapter therefore advertises which statuses it can
  observe and which reconcilers it supports.
- **FR-E7 [NEW per D3 — SHOULD].** Publish per-agent capability gaps so
  renderers can degrade honestly. A Codex session showing no `permission` must
  not be read as "confirmed not blocked" — it may be "cannot tell." v1 documents
  this asymmetry in prose only; a consumer has no way to act on it. This is the
  same honesty AP5 demands of a WM backend that cannot set focus.

#### FR-E8 · Closing the Codex approval gap

The gap is **information-theoretic, not a parsing deficiency**: while a Codex
session sits blocked on approval, its rollout tail shows a `function_call` with
no `function_call_output` — byte-for-byte identical to a command still running.
`should_persist_event_msg` (`codex-rs/rollout/src/policy.rs`) deliberately drops
`ExecApprovalRequest`, `ApplyPatchApprovalRequest`, `RequestPermissions`,
`RequestUserInput` and `ElicitationRequest`, and the decision (`ReviewDecision`)
is consumed in memory. No better parser recovers it. Three responses, in
descending order of confidence:

- **FR-E8a [MUST] — Hooks are the supported path; say so.** With the
  `PermissionRequest` hook wired, `permission` is fully observable. This is not
  a workaround, it is the answer for any user who configures hooks, and setup
  must be first-class (FR-E4), not a documentation footnote.
- **FR-E8b [MUST] — Implement the Codex-specific *clear* rule.** Even with
  hooks, resolution cannot be confirmed the way Claude's is (Claude's transcript
  records enough for `ResolutionState`). Codex must infer resolution from a
  subsequent `task_complete`, a new assistant message, or `turn_aborted` dated
  after the prompt, with the existing TTL as backstop. This is a different rule,
  not a missing one — and without it every Codex red chip latches until TTL.
- **FR-E8c [SHOULD] — Probe the rendered pane.** The investigation ruled out
  `app-server` / `codex mcp` / `exec --json` because each requires *owning the
  codex process*. The terminal seam does not: a TUI asking "approve this
  command?" must render that prompt visibly, and the terminal backends can read
  it (`tmux capture-pane`, `wezterm cli get-text`) — the same third-stream trick
  that recovers H9, which was likewise a case of both event streams being
  silent. Constraints, because this is the fragile option:
  - **Opt-in**, and gated behind its own consent tier — it reads rendered
    terminal content (§9, P6).
  - **Reduced confidence.** It matches vendor UI strings, which change between
    releases; a miss must degrade to "cannot tell," never to "not blocked."
  - **Capability-advertised** (FR-E6) and version-pinned, so a stale matcher is
    visible rather than silently wrong.
  - Bounded cost: sample only for sessions already suspected blocked, never
    every pane every tick (NFR-1).

  Prototype it before committing: if Codex's prompt is reliably detectable, it
  closes the gap for un-hooked sessions; if not, FR-E7's honest "cannot tell" is
  the shipped answer.
- **FR-E8d [MUST].** Absent all three, a Codex session without hooks reports
  `working`/`idle` only, and **advertises that `permission` is unobservable**.
  A documented tier boundary, never a silently-absent red.

#### FR-E9 · Prototype the adapter interface against a third agent *(D5)*

- **FR-E9a [MUST].** Before the adapter trait is frozen (M4), exercise it with a
  **third agent that is not Claude-shaped**. Claude and Codex share a file-backed
  JSONL transcript, a lifecycle-hook system of near-identical shape, and a
  one-process-per-session model. A trait validated only against them will encode
  all three as universal.
- **FR-E9b [MUST].** The trait must not assume: that a transcript file exists;
  that hooks exist; that one OS process equals one session (a client/server
  agent such as OpenCode splits them); that discovery works by `comm ==` (node
  wrappers and renamed binaries defeat it); or that the vendor's session id is a
  UUID.
- **FR-E9c [SHOULD].** Where no third agent is available to integrate, ship a
  **deliberately hostile fake adapter** in the conformance suite — no
  transcript, no hooks, server-backed sessions, status by polling only. It costs
  little and it is the only thing that will catch a Claude-shaped assumption
  before a real third vendor does.
- **FR-E9d [SHOULD].** Agent match rules stay config-extensible (FR-A4), so a
  new agent that needs no new *capabilities* needs no recompile.
- **FR-F3 [NEW per D4 — MUST].** **One trait, per-vendor parsers.** Each agent
  needs its own transcript reader — Claude's `.jsonl` and Codex's
  `rollout-*.jsonl` share no schema — but the *questions the reconciler asks* are
  vendor-neutral, and that is where the hazard logic lives. Factor the seam so
  the questions are the trait and the parsing is the implementation:

  ```
  trait SessionLog {
      /// Did the turn resume, get interrupted, or neither, after `since`?
      fn resolution_after(&self, since: Timestamp) -> Option<Resolution>;
      /// Kind + timestamp of the newest genuinely-agent activity.
      fn last_activity(&self) -> Option<(ActivityKind, Timestamp)>;
      /// Delegations dispatched but not yet collected.
      fn in_flight_delegations(&self) -> usize;
  }
  ```

  `Resolution` (`Resumed` / `Interrupted` / `Unresolved`) and `ActivityKind` are
  canonical types (AP2). The H1–H9 rules consume *those*, so they are written
  once and every vendor inherits them. A vendor that cannot answer a question
  returns `None` — which is a capability report (FR-E6), not a failure, and is
  exactly how Codex's `permission` gap surfaces to the core.
- **FR-F4 [NEW — MUST].** A `None` answer must never be collapsed into a
  negative one. "The log cannot tell me whether this resolved" and "the log says
  it did not resolve" are different facts with opposite safe actions under AP8.
- **FR-F1 [REVISED — MUST, was SHOULD].** Transcript-derived state is not a
  fallback for missing hooks — it is **required alongside** hooks, because
  several transitions fire no hook at all: declining an `AskUserQuestion`,
  interrupting a turn, and an orchestrator woken by a teammate. Without the
  transcript reconciler the `permission` status latches forever
  (`docs/state-schema.md`, "permission self-heal"). This is the single largest
  mis-prioritization in v0.1.
- **FR-G2 [REVISED — MUST].** State reduction is a **case table with holds**,
  not a precedence fold. See §5.G below.
- **FR-I1 [REVISED per D2 — MUST].** The daemon runs as a **per-user process
  inside the user's interactive desktop session** on all three platforms. It is
  explicitly **not** a system service:
  - **Linux:** `systemd --user` unit, with the graphical environment imported
    (§6, correction 3).
  - **macOS:** a **LaunchAgent** (per-user, GUI session). A `LaunchDaemon` has
    no window server access and cannot hold the TCC grant.
  - **Windows:** a logon-triggered background process — Task Scheduler "at log
    on", the `Run` registry key, or the Startup folder — **not** an SCM service.
    An SCM service runs in **session 0**, which is isolated from every
    interactive desktop: it cannot enumerate windows, set foreground, or read
    idle input. Shipping Windows as an SCM service would silently reduce it to
    Observe-only with no diagnosable cause.

  The common requirement is *desktop-session membership*, not *service-manager
  integration*; the three managers are packaging details behind it.
- **FR-I2 [REVISED — MUST].** Graceful shutdown translates each platform's stop
  mechanism into one uniform cancellation: `SIGTERM` (systemd's actual stop
  signal) and `SIGINT` on Unix; on Windows, console control events
  (`CTRL_CLOSE_EVENT`/`CTRL_LOGOFF_EVENT`/`CTRL_SHUTDOWN_EVENT`) and
  `WM_QUERYENDSESSION` if a message-pump thread exists (FR-I5). `ctrl_c()` alone
  is insufficient on every platform.
- **FR-I5 [NEW per D2 — MUST].** Windows focus events (WinEvent hooks) require a
  **running Win32 message pump**, which is a dedicated OS thread outside the
  async runtime. Host it on its own thread and bridge it into the core with a
  channel, exactly as any other event source. Do not attempt to drive it from a
  runtime worker.
- **FR-I6 [NEW per D2 — MUST].** `SetForegroundWindow` is refused for a process
  that does not own the current foreground window. The Windows backend must use
  a sanctioned path (e.g. `AttachThreadInput` to the foreground thread for the
  duration of the call, or an equivalent documented technique) and must **report
  focus-set as unavailable** (AP5) when the OS refuses, rather than silently
  no-op'ing. Focus actuation is the single most permission-fragile capability in
  the matrix on all three platforms.
- **FR-I3 [REVISED — MUST].** Config load must state its failure policy: a
  malformed config **fails fast at startup**; a config naming an unavailable
  backend **degrades with a logged warning** rather than exiting.

### FR-G · State reduction — the real shape

- **FR-G3 [REVISED — MUST]. Port the writer-state FSM, not the shipped rule
  table.** `internal/statustune/knobs.go` defines 19 rule ids on `main`, but that
  table is **already superseded** by in-flight work on the unmerged branch
  `docs/writer-state-model` (commit `2040a91`, "model the session as a per-writer
  FSM with a pure fold"). The FSM is a *reduction* of the rule table — five case
  rows and two rule ids collapse — and it is pure, total, and platform-free,
  making it the single most portable and most valuable artifact in the Go repo.
  Its structure:

  | Layer | What | Memory? |
  |---|---|---|
  | 1 | Per-writer FSM — `Apply(State, EvidenceKind) → Transition` | **yes** — the only real machine |
  | 2 | Session belief — `map[writer]Writer` + liveness | no — a product |
  | 3 | Chip color — `Fold` | no — pure and total |

  The thesis is load-bearing and must survive the port: *a color must not have
  memory, because it renders current belief; memory in the color layer is a latch
  that desynchronizes from the world, and "stale red" and "false green" are the
  names of that desynchronization.* v1's mutable `AgentInfo.Status` enum, which
  hooks transition directly, **is** the defect class.

  The key structural insight: `ToolInFlight` splits out of `Working` because
  `ToolInFlight` and `Blocked` have **identical on-disk signatures** — an
  unmatched `tool_use` in the writer's tail — separated only by an
  edge-triggered `PermissionRequest`. Three consequences become structural rather
  than argued: `Blocked` is the only state not reconstructible from level
  evidence (**hence the only one that must persist across restart** — this is
  FR-L6's justification, derived); transcript evidence can only ever be a
  **falsifier**, never a source (this is FR-F3/FR-F4, derived); and leaving
  `Blocked` requires the **negation** of that shared signature — every dispatched
  `tool_use` matched. Any weaker exit predicate cannot distinguish the two states
  and therefore cannot prove a prompt resolved.

  The fold also **deletes the subagent dimension**: a live teammate is simply a
  writer in `Working`, so `delegating` is not a state, not a rule, and not a
  special case — it is the ordinary green branch. This simplifies FR-M4.
- **FR-G3a [MUST].** Establish which model the port targets **before** the state
  model is frozen (M4). Porting the 19-rule table and then the FSM means doing
  the hardest work in the project twice. See the fork-point requirement, FR-G3b.
- **FR-G3b [MUST].** **Declare an explicit fork point.** The Go repo has 28
  unmerged branches and 261 commits across all refs in the last three weeks. A
  rewrite forking from `main` today forks from a snapshot already behind the
  design thinking. Either land `writerstate` on `main` first, freeze the Go
  status model for the duration of the port, or budget explicitly for re-porting.
- **FR-G3c [MUST].** Whatever the model, every status decision — change **or
  hold** — emits one structured decision line carrying `from→to` (or `==`), a
  rule/transition id, a reason, and the observed tuple. This is what the
  trace-replay oracle (FR-O6) asserts against.
- **FR-G4 [MUST].** Rules that deliberately refuse to clear a `permission`
  status are **missed-RED guards** and must be implemented as such: a bare or
  `Task` `PostToolUse` may not clear a prompt; a `tool_name` match from a writer
  that does not own the prompt may not clear it; a non-tool event carries no
  evidence and may not clear it. These have no tuning knob by design.
- **FR-G5 [MUST].** Timing-sensitive rules must anchor `status_since`
  correctly per edge: wall-clock `now` on the `idle` and `permission` edges (so
  a late-flushed entry dated *before* the edge cannot read as activity *after*
  it), and to the transcript on others. Hazards H7/H8 are exactly this bug, and
  both were shipped before being found.
- **FR-G6 [SHOULD].** Threshold constants (decay TTLs, stale caps, grace
  windows) live in one tunable structure with a documented knob per rule, so a
  wrong-color report maps to a field to change.

### FR-J · Terminal seam *(new — the missing seam)*

- **FR-J1 [MUST].** Define a canonical `TerminalLocator` capability: given a
  tty, return the owning pane (backend, mux pid, pane id, window title, cwd), or
  nothing.
- **FR-J2 [MUST].** Focus a pane (raise its tab/pane within the mux), as a
  distinct operation from focusing a window. Navigate = window focus **and**
  pane focus; either alone lands the user in the wrong place.
- **FR-J3 [MUST].** Backends compose. A tmux session inside a wezterm window
  requires chaining both locators; the detected terminal may be a chain
  (`"tmux+wezterm"`), not a single value (`internal/terminal/chain.go`).
- **FR-J4 [SHOULD].** Sample the pane **title**. It is a third signal stream
  independent of hooks and transcript, and it is the *only* recovery for the
  silent-abort hazard (H9) where neither other stream ever emits anything.
- **FR-J5 [MUST].** Runtime-detected, open set, degrades to `none` (Observe).

### FR-K · Control surface *(new)*

The daemon is actuated, not merely observed. v0.1 has `Publisher` (one-way,
write-only) and nothing else.

- **FR-K1 [MUST].** A local IPC endpoint (Unix socket / named pipe) supporting
  at minimum: `list` (snapshot), `focus` (actuate), `subscribe` (server-push
  stream of snapshots), and `hook` (ingest). `subscribe` is what lets renderers
  update without polling; an NDJSON file cannot replace it.
- **FR-K2 [MUST].** A CLI client exposing `list`, `status`, `focus <selector>`,
  `cycle next|prev`, `attention`, `pick`, and the hook forwarders. Selectors
  must include unambiguous `pid:<n>` and `idx:<n>` forms
  (`docs/decisions.md` #3).
- **FR-K3 [MUST].** The hook forwarder is **fire-and-forget**: a broken or slow
  daemon can never block or fail the agent that invoked it.
- **FR-K4 [MUST].** On an Observe-only stack, actuation commands return a clean
  "navigate unsupported" result, not an obscure failure (AP5).
- **FR-K5 [SHOULD].** A `diagnose` command that reads decision logs back and
  maps a symptom to the rule and knob responsible.

### FR-L · Session identity & lifecycle *(new)*

- **FR-L1 [MUST].** Exactly one lifecycle-end record per session, whichever
  observer notices first; every later trigger no-ops (hazard L5).
- **FR-L2 [MUST].** A session whose death was observed during daemon downtime
  is closed **at the observed time**, not at restart time, and the end is
  recorded *before* the session is dropped (hazard L2).
- **FR-L3 [MUST].** Never delete a session without routing through the
  end-of-session path — a bare delete writes no end record and hides the pid
  from later scans forever (hazard L7).
- **FR-L4 [MUST].** Liveness never keys on an activity TTL. A genuinely idle
  session that sits for hours is alive (hazard L4).
- **FR-L5 [SHOULD].** State rehydrates across daemon restart from the mirrored
  snapshot; the mirror is a **cache, not a source of truth** — a corrupt mirror
  is discarded and state is rebuilt from a live scan.
- **FR-L6 [MUST].** In-memory-only correlators that cannot be safely rebuilt
  must **fail closed** on rehydrate. A hydrated pending prompt may be
  *falsified* by evidence but never *manufactured*.

### FR-M · Multi-writer status *(new)*

- **FR-M1 [MUST].** The canonical model represents a session as **1 + N
  writers**, not one agent. v0.1's per-process `AgentState` cannot express two
  writers blocked on two different prompts sharing one pid.
- **FR-M2 [MUST].** A pending prompt is owned by the **writer that raised it**,
  held as a map from writer to that prompt's correlators. The session may leave
  `permission` only when no writer still owns a prompt.
- **FR-M3 [MUST].** Publish the blocked-writer key set. Publish ids, not names —
  names are derivable by renderers from on-disk metadata and would add
  per-writer I/O to every tick.
- **FR-M4 [SHOULD].** Track in-flight subagent count and derive a `delegating`
  state (an idle main thread with teammates working), rendered as "work is
  happening, no action needed."

### FR-N · Activity history *(new — reconciles NG4)*

- **FR-N1 [MUST].** Opt-in, off by default, append-only local event log:
  status transitions, session lifecycle, subagent fan-out, token-usage samples.
- **FR-N2 [MUST].** Retention and rotation are the daemon's job — bounded by
  both age and total bytes.
- **FR-N3 [MUST].** A privacy tier that omits cwd and task descriptions.
- **FR-N4 [SHOULD].** Suspect-lane flagging: a lane whose end record was lost is
  **flagged and still drawn in full**, never silently dropped, and aggregate
  totals are held to the suspect point (hazard L8).

### FR-O · The v2 wire contract *(revised per D1 — clean break)*

> **Scope correction.** There are **three** public contracts, not one, and D1's
> clean break applies to the first two only:
> 1. the `state.json` snapshot (waybar, `claude-tui`, `ctl`, user scripts) — **breaking**;
> 2. the RPC socket (`list`/`focus`/`subscribe`/`hook`) — **breaking**;
> 3. the **timeline envelope** emitted by `switchboard-ctl timeline --json`,
>    consumed by `switchboard-dashboard` — **NOT in scope for D1.**
>
> The dashboard does not read `state.json` at all. The envelope
> (`switchboard-dashboard/internal/timeline/types.go`) has an independent
> vocabulary and embeds no snapshot structures, by deliberate design. Breaking it
> while "cleaning up the wire format" would be an accident, not a decision — see
> `switchboard-dashboard/docs/rust-rewrite-requirements.md` §1.
>
> **FR-O9 [MUST].** The provider **process** contract is preserved exactly: argv
> shape, the four window flags (`--dir`/`--day`/`--since`/`--until`), tolerance of
> unknown flags, exit-0-plus-stdout-JSON, stderr-as-operator-diagnostic, and the
> partial-failure semantics (one provider failing lands in `provider_errors` and
> everything else still renders). Because this seam is a **process** boundary
> rather than a library boundary, the daemon and dashboard rewrites are
> **independent and may proceed in either order or concurrently** — the single
> largest de-risking fact available to this programme.

Parity is retired. What replaces it is *not* "no contract" — it is a contract
designed once, deliberately, with the v1 schema's known defects fixed and its
hard-won presence semantics carried forward.

- **FR-O0 [MUST].** The v2 schema is **specified before it is implemented**, in
  a document that supersedes `docs/state-schema.md`, and it explicitly resolves
  each ⚠ quirk the v1 schema records: generalize the provisional
  `wezterm`/`hyprland` blocks into backend-neutral `terminal`/`window` blocks;
  either populate `monitor` or remove it; drop the unreachable `unknown` status;
  rename `tty` to the anchor type of FR-A6; and state whether the session key is
  the pid or the agent session id (v1 uses pid and documents the tension).
- **FR-O0b [MUST per D3].** **Collapse the per-agent blocks.** v1 encodes one
  fact three ways: an `agent` discriminator naming the kind, plus two mutually
  exclusive optional keys `claude` and `codex` carrying the *identical*
  `AgentInfo` type (`state.go:57-58`). Every added agent costs a new key and a
  new renderer branch, and every renderer must write `.claude.status //
  .codex.status`. v2 carries **one** `agent` block with the kind inside it, so
  the agent set is genuinely open and renderers read one path regardless of
  vendor.
- **FR-O0a [MUST].** Carry forward the v1 presence discipline verbatim, because
  it is behavioral, not cosmetic: **omitted ≠ empty ≠ null**. An optional block
  is *absent* until resolved, never `null`; an always-present string uses `""`
  for unknown; a measurement that failed is *absent*, never `0`, because `0`
  means "measured and empty." These distinctions are load-bearing for renderers.
- **FR-O1 [REVISED — MUST].** The v2 snapshot carries every *fact* the v1
  contract carried. A clean break licenses re-shaping the schema; it does not
  license dropping `pending_writers`, `in_flight_subagents`, `status_since`,
  `suspended`, `headless`, the `capabilities` block, or the memory readings. Any
  field intentionally retired is listed in FR-O0's document with a reason.
- **FR-O2 [MUST].** Written atomically — temp file in the same directory, then
  `rename(2)` — so readers never see a partial document.
- **FR-O3 [MUST].** Written only when something **observably changed**; an idle
  reconcile that re-derives identical state writes nothing, so `updated_at`
  means "last change," not "last tick."
- **FR-O4 [MUST].** A golden fixture pins the contract and fails the build on
  any change to field name, order, type, or presence — including the case where
  a *new optional field* is added but never set in the fixture.
- **FR-O5 [MUST].** Consumers ignore unknown fields and tolerate missing
  optional ones; additive changes are non-breaking; stable-field changes need a
  major version and a migration note.
- **FR-O6 [REVISED per D1 — MUST].** The snapshot-diff oracle is unavailable, so
  it is replaced by a **trace-replay oracle**, which survives a wire break
  because it operates one layer below the wire:
  1. Instrument the Go daemon (or capture from its existing decision log and
     history log) to record a real timeline of *inputs* — hook payloads,
     transcript deltas, process-table samples, WM events — with timestamps.
  2. Replay that trace into the Rust reducer.
  3. Assert on the **derived status decisions** — the `from→to`, the rule id,
     and the holds — not on serialized bytes.

  This is where the 19 rules and the H1–H9 / L1–L8 hazards actually live, so it
  is the oracle that matters for G6. Capturing traces from the Go daemon **while
  it is still running in production** is time-limited: it must happen before the
  Go daemon is retired, which makes it an early task, not a late one.
- **FR-O7 [MUST per D1].** Ported renderers ship with v1: the waybar module,
  `claude-tui`, `switchboard-ctl`, and updated bar recipes for the v2 schema. A
  daemon with no renderer is not a deliverable.
- **FR-O8 [SHOULD].** The v2 snapshot carries an explicit **schema version**
  field from day one. The v1 contract had none, which is why every subsequent
  change had to be additive-only; a clean break is the one chance to fix that.

#### FR-O1a · Encoder configuration is part of the contract

Under D1 these no longer matter for *matching Go*. They still matter, for a
different reason: a golden fixture is only a tripwire if the encoder's output is
deterministic and pinned. `serde_json`'s defaults differ from `encoding/json`'s
in four ways, and each is a place where output can drift silently between
library versions or field-modelling choices if left unstated:

| # | Go `encoding/json` | `serde_json` | Consequence |
|---|---|---|---|
| 1 | **Escapes `<`, `>`, `&`** to `<`/`>`/`&` by default (`state.go:821` never calls `SetEscapeHTML(false)`) | Emits them literally | v2 should emit them literally. Decide it explicitly — `window_title` and `cwd` routinely contain `&`, so this is a real output difference, not a theoretical one. |
| 2 | `Encoder.Encode` appends a trailing newline | `to_writer` does not | Pick one and pin it; the v1 schema documented the newline as contractual. |
| 3 | `time.Time` → RFC 3339, `Z` suffix, **variable** fractional-second precision (trailing zeros trimmed, fraction omitted when zero) | `chrono`'s `to_rfc3339()` → `+00:00`, fixed precision | Variable precision is a bad contract — it means the same instant can serialize two ways. **Fix it in v2:** pin one seconds-format with `use_z = true`. |
| 4 | `omitempty` omits the **zero value of any type** | Needs a per-field `skip_serializing_if` predicate | Rust forces the choice to be explicit per field, which is an improvement. Model FR-O0a's three-way distinction deliberately: `Option<T>` where absence is meaningful, an explicit predicate where "zero means omit." |

- **FR-O1a [MUST].** The encoder configuration (escaping, trailing newline,
  timestamp format, per-field presence predicates) is **specified in FR-O0's
  document and pinned by a byte-level golden fixture**. Parsed-JSON comparison
  is blind to every row above and is not sufficient.
- **FR-O1b [MUST].** Struct field **declaration order** determines output order
  and is therefore part of the contract. Reordering fields is a wire change and
  must fail the fixture.
- **FR-O1c [MUST per D1].** Resolve `mem_agent_bytes`'s documented-but-unimplemented
  intent. The v1 schema says absent means "unmeasured" and `0` would mean
  "measured and empty," but `omitempty` on a bare `int64` omits both identically
  — the distinction was never real. In v2, model it as an option type so the
  documented semantics and the implementation agree.

### FR-P · Supervision & resilience *(new)*

- **FR-P1 [MUST].** Every long-lived ingestion task is supervised: if the
  Hyprland event socket closes (compositor restart), the hook listener socket
  goes stale, or a source task panics, it is restarted with backoff and the
  degradation is surfaced. v0.1 has no requirement here; a spawned task holding
  a `Sender` that dies silently is invisible.
- **FR-P2 [MUST].** Single-instance enforcement — two daemons writing one
  `state.json` and racing one socket must be prevented, not merely discouraged.
- **FR-P3 [MUST].** Bounded resources: the event channel has a defined capacity
  and a defined overflow policy (drop-oldest with a counter, or apply
  backpressure — but *chosen*, not accidental); tracked sessions and history
  files are bounded.
- **FR-P4 [MUST].** The hook ingest surface accepts input from any local
  process. It must validate and bound payloads, and its socket must be created
  with restrictive ownership/permissions. "Local-only" is not a security
  property by itself.
- **FR-P5 [SHOULD].** A stale socket file from a crashed predecessor is
  detected and reclaimed rather than causing a startup failure.

### FR-Q · Conformance & test infrastructure *(new)*

- **FR-Q1 [MUST].** A backend-agnostic **conformance suite** every backend of
  every seam must pass. AP5 and NFR-7 are unenforceable without it, and it is
  what makes delegating backend implementations to agents safe.
- **FR-Q2 [MUST].** Test fixtures for: a synthetic process tree, a fake
  IPC/socket peer, real-child death (to exercise the death watch), and golden
  snapshots.
- **FR-Q3 [SHOULD].** Every hazard in §11 has a named regression test whose name
  states the invariant.

### FR-R · Clock & time semantics *(new)*

- **FR-R1 [MUST].** Wall-clock timestamps (published, comparable to file mtimes
  and transcript entries) and monotonic durations (elapsed, decay, grace) are
  **distinct types** and are not interchanged. Go's `time.Time` carries both
  transparently; Rust splits them into `SystemTime` and `Instant`, which will
  surface latent confusion in the current logic as compile errors. Treat each
  such error as a design question, not a cast to silence.
- **FR-R2 [MUST].** All published timestamps are RFC 3339 UTC.

### FR-S · Codex terminal slots and app-server endpoints *(2026-08-21 amendment)*

- **FR-S1 [MUST] — Split slot from conversation.** A Codex terminal slot is
  keyed by a random UUID created by the launcher and is stable for one visible
  TUI lifetime. Its current conversation is `{thread_id, generation}`. PID and
  process start time are liveness/discovery metadata only and must never be the
  permanent provider-session key.
- **FR-S2 [MUST] — Rotation, not conflict.** An exactly attributed event naming
  a different, non-retired thread increments generation, retires the old
  binding, clears all conversation-scoped state, and applies the triggering
  event immediately. Any later event naming a retired thread, carrying a
  mismatched non-zero generation, or predating the newest accepted event is
  rejected without changing name, status, attention, writers/children, pending
  work, or timestamps. The timestamp fence also rejects an out-of-order
  intermediate thread that was never observed before a rapid second `/clear`.
  Duplicate current-thread events are idempotent.
- **FR-S3 [MUST] — One endpoint per visible TUI.** The `agent-watcher-codex`
  launcher creates `$XDG_RUNTIME_DIR/agent-watcher/codex/<slot-id>/`, starts
  `codex app-server --listen unix://.../app-server.sock`, exports a stable slot
  variable and the endpoint, starts `codex --remote unix://...`, registers the
  slot, and supervises both children. It removes the socket on exit. A shared
  Codex daemon is not an attribution source for visible TUIs.
- **FR-S4 [MUST] — Launcher control protocol.** The RPC vocabulary includes
  `codex_slot_register {slot_id, endpoint, pid, started_at}`,
  `codex_slot_unregister {slot_id}`, hook observations carrying `slot_id`,
  `session_id`, and `observed_at` (plus optional generation), and
  `autoname {slot_id?}`. Exact
  slot identity precedes ancestry; an unknown explicit slot is dropped and
  counted rather than falling back to a different process.
- **FR-S5 [MUST] — Per-slot observer.** One supervised app-server client exists
  per registered endpoint. It initializes without starting turns or answering
  approvals, reconciles `thread/loaded/list`, reads the bound thread and
  descendants, treats notifications as invalidation hints, polls every second
  while active and every ten seconds while idle, reconnects with capped
  exponential backoff, and retains a last complete snapshot only through its
  explicit freshness boundary. The [official Codex app-server protocol](https://learn.chatgpt.com/docs/app-server)
  defines the thread/read/name methods; the Unix JSONL proxy bridge is a
  version-pinned local capability.
- **FR-S6 [MUST] — Status authority.** Hooks supply immediate prompt, tool,
  completion, and permission edges. App-server runtime/attention state corrects
  missed or delayed hooks. Reduction precedence remains attention first,
  active/delegating second, idle third, and cannot-tell otherwise. This endpoint
  closes the old rollout-only approval gap for wrapper-launched sessions;
  FR-E8's weaker hook/rollout tier still describes legacy sessions without a
  registered endpoint.
- **FR-S7 [MUST] — Names are conversation state.** App-server thread name is
  authoritative. `thread/name/updated` and thread reads project into the visible
  nickname. Any non-empty name not matching Switchboard's pending generated
  value is `origin=user`, cancels generation, and is never overwritten by
  autonaming. `/clear` starts unnamed; retired history may preserve the old
  name, but the visible slot may not.
- **FR-S8 [MUST] — Zero-effort autonaming.** After the first substantive prompt
  on an unnamed generation, send at most its first 1,000 characters plus cwd
  basename to an isolated, ephemeral naming app-server. Default model is
  `gpt-5.6-luna`, configurable. Validate lowercase 2–5-word kebab-case output
  capped at 40 characters, retry one transient failure, then use a deterministic
  prompt-derived fallback. Gate `thread/name/set` by slot, thread, and
  generation; serialize name writes per slot. Never persist or log the prompt.
  Manual `autoname` is retry-only.
- **FR-S9 [MUST] — Normalized event contract.** Both provider paths enter the
  reducer as `{slot_id, provider, conversation_id, generation?, activity,
  attention, optional_name_update, observed_at, provenance}`. Provider adapters
  may add richer evidence beside this contract, but may not bypass the
  generation fence. Claude's hook/transcript behavior remains unchanged.
- **FR-S10 [MUST] — Clean Rust boundary.** Port the slot/conversation types,
  normalized event reducer, launcher RPC vocabulary, and conformance suite.
  Do **not** port Go schema-v1 compatibility or the one-process/one-session
  binding model. The v2 schema publishes the slot UUID, current binding and
  generation, name origin, endpoint health/snapshot age, and autoname state.
- **FR-S11 [MUST] — Conformance.** The shared suite covers the discovering event
  across rotation, stale status/name/attention/children rejection, missed
  `SessionStart`, duplicate/reordered hooks, rapid `/clear`, daemon reconnect,
  active→idle→active and approval transitions, explicit rename races,
  generated-name validation/retry/fallback/cancellation, and two concurrent
  same-cwd slots whose state never crosses. Ephemeral naming threads must never
  appear as visible slots. Claude's existing corpus must remain green.

---

## 6. Cross-Platform Capability Matrix — corrected

| Capability | Linux | macOS | Windows | Reality check |
|---|---|---|---|---|
| Process enumerate | `/proc` | libproc / sysctl | Toolhelp / NtQuery | Low risk |
| Per-proc mem | `/proc/<pid>/smaps_rollup` (PSS+SwapPss) | task_info (RSS only) | PSAPI | **PSS has no macOS/Windows equivalent** — cross-platform memory numbers are not comparable; the field must carry its measure or be absent |
| **Death watch** | `pidfd_open` (5.3+) | `kqueue`/`EVFILT_PROC` | job object / `WaitForSingleObject` | Feasible everywhere; **must be event-driven** (AP9) |
| Liveness `Unsupported` | n/a | **current Go darwin backend returns Unsupported for all pids** | n/a | Three-valued result is not theoretical — it is the live macOS state |
| Window enumerate | Hyprland/sway/i3/X11 | Accessibility (AX) | EnumWindows | AX requires a TCC grant |
| window ↔ **mux** pid | compositor IPC (direct) | AX `kAXPIDAttribute` | `GetWindowThreadProcessId` | Fine on all three — but it yields the *terminal's* pid (§4.2) |
| **session anchor** *(new row, FR-A6)* | controlling tty (`/dev/pts/N`) | controlling tty (`/dev/ttysNNN`) | **console / ConPTY identity — no path exists** | **D2's structural cost.** Unix gets a kernel-owned string; Windows must synthesize a stable identity from console attachment or terminal ancestry. Highest-risk Windows item |
| **anchor → pane** *(new row)* | mux CLI/socket | same | Windows Terminal / WezTerm / ConEmu | **The actual join.** Terminal-specific, not OS-specific. Windows Terminal's programmatic surface is thinner than wezterm's — confirm before committing |
| **pane focus** *(new row)* | mux CLI | mux CLI | mux CLI | Independent of the WM |
| Query focus | compositor IPC | AX / NSWorkspace | GetForegroundWindow | OK |
| Set focus | compositor IPC | AX + TCC | SetForegroundWindow + `AttachThreadInput` | **Foreground-lock refuses background processes** (FR-I6). Must report unavailable rather than silently no-op |
| Focus events | Hyprland `socket2` | AX notifications | WinEvent hooks | **Requires a Win32 message pump** on a dedicated OS thread, bridged by channel (FR-I5) |
| Idle / AFK | idle-notify / XScreenSaver | CGEventSource | GetLastInputInfo | OK; Wayland needs the compositor to implement the protocol |
| Hook ingest | Unix socket | Unix socket | **named pipe** | Not one transport — Windows differs structurally. `tokio::net::windows::named_pipe` covers it, but the accept loop shape differs from `UnixListener` |
| Hosting (FR-I1) | systemd **--user** | launchd **LaunchAgent** | **logon task / Run key — never SCM** | Common requirement is *desktop-session membership*; SCM session-0 isolation makes a Windows service unusable here |

**Corrections that change plans, not just wording:**

0. **D2 makes the matrix a v1 work-list, not a roadmap.** Every cell above is
   now committed for v1. That is 3 OSes × (process + anchor + terminal + WM +
   idle) plus per-desktop WM backends on Linux — realistically 15–20 backend
   implementations, each owing conformance-suite compliance (FR-Q1). The
   conformance suite therefore stops being good practice and becomes the
   critical path: it is the only mechanism by which that many backends can be
   delegated without the result being unverifiable.
1. **"One working backend per OS" is wrong for Linux.** Linux is not a platform
   for this daemon; the WM is. The Go implementation ships four Linux WM
   backends (Hyprland, sway, i3, X11) because a single one covers a minority of
   users. v0.1's SD1 ("variance axis is target OS, known at compile time") holds
   for Seam 1 only. Seams 2 and 3 vary along *desktop environment* and
   *terminal*, orthogonally to OS.
2. **Service-manager symmetry is a fiction.** A Windows SCM service runs in
   session 0 with **no access to the user's desktop** — it cannot enumerate
   windows, set focus, or read idle. On Windows this daemon must be a per-user
   background process, not an SCM service; FR-I1's `[MUST]` as written is
   unimplementable. macOS likewise requires a `LaunchAgent` (per-user GUI
   session), never a `LaunchDaemon`.
3. **Linux under systemd needs the graphical environment imported.** A
   `systemd --user` unit does not inherit `HYPRLAND_INSTANCE_SIGNATURE`,
   `WAYLAND_DISPLAY`, or `DISPLAY` unless the compositor imports them
   (`systemctl --user import-environment …`). Without that the WM seam silently
   detects `none` and the daemon degrades to Observe with no obvious cause. This
   is a startup-diagnostics requirement, not a packaging detail.
4. **macOS AX needs a TCC grant** that a headless/launchd context cannot prompt
   for, and the grant is per-binary — it resets when the binary changes, which
   means every rebuild during development re-prompts.

---

## 7. Selection & Dispatch

SD1–SD4 stand, with one correction: **SD1 applies only to Seam 1.** Seams 2 and
3 vary on runtime environment, and their candidate sets are per-OS but not
per-OS-determined. Add:

- **SD5 [NEW].** Detection order is explicit and documented, and every seam
  supports an explicit config override plus a `none` backend, so degradation can
  be tested deliberately (the Go daemon exposes `-wm` and `-terminal` flags for
  exactly this).
- **SD6 [NEW].** The chosen stack is logged at startup **and published** in the
  snapshot's capabilities block, so a renderer can decide whether to offer
  actuation affordances and a user can see why Navigate is off.

---

## 8. Non-Functional Requirements

NFR-2 through NFR-8 stand. Revised and added:

- **NFR-1 [REVISED] Performance — the constraint is lock-hold time.** Measured
  on the reference machine (`docs/decisions.md`): a full per-session memory
  sampling pass costs **14 ms on a quiet machine and 54–96 ms under memory
  pressure** — a ~7× degradation exactly when the reading is most wanted,
  because `smaps_rollup` walks the target's VMA list. The design response was to
  do sampling *outside* the store's write lock, holding it for **833 ns**. The
  requirement is therefore: **no I/O inside the state lock**, and per-tick I/O
  budgeted against its degraded cost, not its quiet cost. Dispatch overhead
  remains irrelevant — which is why it should not be the section's headline.
- **NFR-9 [NEW] Observability of degradation.** The daemon reports *why* a
  capability is unavailable, not merely that it is. "wm=none" is a symptom;
  "HYPRLAND_INSTANCE_SIGNATURE unset — did the compositor import its
  environment?" is a diagnosis.
- **NFR-10 [NEW] Acceptance.** v1 is done when: the differential harness
  (FR-O6) shows no stable-field divergence from the Go daemon over a
  representative session; every §11 hazard has a passing regression test; and
  the existing waybar config, `claude-tui`, and `switchboard-ctl` scripts run
  unmodified against the Rust daemon.

---

## 9. Data & Privacy

P1–P5 stand, with P5 promoted: **[MUST]**, since FR-N makes the daemon the
owner of retention. Added:

- **P6 [MUST].** The two consent tiers govern *focus* capture. Activity history
  (FR-N) is a **separate** opt-in with its own privacy tier — enabling one must
  not enable the other.

---

## 10. Milestones — revised

The build order is sound; the gates are not. Revised for D1 + D2:

0. **M-1 — Capture the traces.** *(new, and first — it is time-limited.)*
   Record real input timelines from the running Go daemon for FR-O6's replay
   oracle, covering each H1–H9 and L1–L8 scenario. Everything downstream is
   verified against these; once the Go daemon stops running, they are
   unobtainable.
1. **M0 — Skeleton.** Host/core split, runtime, uniform cancellation from
   `SIGTERM`/`SIGINT` (and the Windows console-control path), config load, tick
   loop, stdout publisher.
2. **M1 — process layer, Linux.** Detection, resources, **and the death watch**
   (AP9). *Gate: three-valued liveness; L1–L5 replay green.*
3. **M2 — session anchor + terminal seam.** FR-A6's `SessionAnchor` and
   anchor→pane for one backend. Ahead of the WM, because M3 cannot complete its
   join without it.
4. **M3 — wm.** Capability negotiation, the §4.2 join chain, one backend.
   *Gate: **conformance suite exists** (FR-Q1) — under D2 this is the critical
   path, not a nicety.*
5. **M4 — the v2 contract.** Write FR-O0's schema document, implement it, pin it
   with a byte-level golden fixture. **This is the gate that makes every
   subsequent delegation safe**, and under D1 it is also the last cheap moment
   to change the schema.
6. **M5 — hooks + transcript together.** Both, not hooks alone (FR-F1).
   *Gate: H1–H9 replay green against the M-1 traces.*
7. **M6 — RPC + CLI + ported renderers.** Under D1 the renderers are v1 scope
   (FR-O7); this is the milestone where the Rust daemon first becomes *usable*
   rather than merely correct, and where the Go daemon can be retired.
8. **M7 — macOS.** Process layer (incl. `kqueue` death watch), anchor, AX
   backend, TCC handling.
9. **M8 — Windows.** Process layer, console-anchor resolution (the highest-risk
   item in the matrix), terminal backend, WinEvent message-pump thread, named
   pipes, logon hosting.
10. **M9 — focus surveillance & history.** Consent tiers, occupancy, switches.
11. **M10 — remaining WM/terminal backends.** The genuinely delegable breadth,
    now behind a conformance suite that can verify it.

> **Sequencing note under D2.** M7 and M8 are large and mostly independent of
> each other; they are the natural delegation targets. M8 carries a discovery
> risk M7 does not — the Windows session anchor (FR-A6) may not have a clean
> answer, and if it does not, Windows lands at Observe-tier only. **Spike the
> Windows anchor during M2**, when the canonical type is being designed, rather
> than discovering its limits at M8 with the type already frozen.

---

## 11. Risk Register — hazards already paid for

Full detail in the cited docs. Each is a requirement, not a warning.

**11.1 Hook/transcript timing skew** (`docs/timing-hazards.md`, H1–H9) — nine
catalogued scenarios where the hook stream and the transcript stream disagree.
H7 and H8 are *flush-ordering races*: an entry dated **before** an edge lands on
disk **after** it, so naive "is there activity after `status_since`?" logic
re-greened every chip after every `Stop`, and released a red chip while the
prompt still waited. H9 is a *silent abort* — double-Esc before the first token
emits nothing in either stream, recoverable only from the pane title.

**11.2 Ghost lanes** (`docs/session-lifecycle-hazards.md`, L1–L8) — the death
watch lives in daemon memory; a restart or SIGKILL orphans it and that death is
never observed. This stranded three sessions as 4½-hour ghosts on 2026-07-22.
The fix is a liveness sweep *backstopping* the watch, plus recording the end
before dropping.

**11.3 Permission-status oscillation** (`docs/subagent-permission-oscillation.md`)
— five distinct defects in one incident. Root cause: a session is 1+N writers,
and a whole-session `ClearPending` cannot express "one writer's prompt resolved,
another's did not." `tool_name` is a tool *kind*, not a tool *identity* —
fanned-out teammates run byte-identical commands routinely, so even a matching
input hash does not identify a writer.

**11.4 Latched red** — `permission` has no guaranteed clearing hook. Declining
an `AskUserQuestion` or interrupting fires nothing. Requires transcript
self-heal plus a TTL backstop.

**11.5 Address normalization** (`docs/decisions.md` #1) — Hyprland's event
stream emits window addresses *without* the `0x` prefix its query API returns
*with*. Normalize at the seam boundary or every comparison silently fails.

**11.6 Lock-hold under pressure** (§NFR-1) — the 7× degradation.

**11.7 Second-system risk** — the largest one. The Go implementation's value is
concentrated in ~25k LOC of behavior the v0.1 spec does not mention. A rewrite
that reproduces the architecture without the behavior will feel correct in
demos and be wrong in exactly the situations that motivated the original code.

---

## 12. Open Questions

OQ1–OQ6 stand. Added:

- **OQ7 [REVISED].** Cutover. D1 removes the snapshot-diff motive for
  coexistence, but the two daemons must still run side by side through M6 (the
  Go one serving the user, the Rust one being built). They need distinct socket
  and state paths, and FR-P2's single-instance enforcement must therefore be
  **path-scoped, not global**, or each will refuse to start against the other.
- **OQ8 — RESOLVED (D1).** Clean break; v2 contract; renderers ported in v1.
- **OQ9 — RESOLVED (D2).** Windows in v1, hosted as a logon-triggered per-user
  process (FR-I1), not an SCM service.
- **OQ10 — RESOLVED (D3).** Both agents ship; the agent set is open. The schema
  consequence is FR-O0b (one `agent` block, kind inside), the capability
  consequence is FR-E6/FR-E7 (adapters advertise what they can observe).
- **OQ13 — RESOLVED (D4).** Per-vendor parsers behind one `SessionLog` trait of
  vendor-neutral questions (FR-F3). Hazard logic written once over the answers.
- **OQ14 [NEW per D5].** Which third agent to prototype against? The choice
  should maximize *dissimilarity* from Claude/Codex, not familiarity — an agent
  with a client/server split or no file transcript is worth more as a design
  probe than a third JSONL-and-hooks tool. Failing an integration target, the
  hostile fake of FR-E9c is the fallback.
- **OQ15 — RESOLVED by FR-S5/FR-S6.** Wrapper-launched Codex sessions expose
  structured approval/user-input state through their private app-server
  endpoint, so pane scraping is unnecessary for that tier. Legacy sessions
  without an endpoint retain FR-E8's documented observability gap until exit.
- **OQ11 [NEW per D2].** Does the Windows session anchor (FR-A6) have a reliable
  answer? Spike it at M2. If a stable console identity cannot be resolved,
  Windows ships Observe-only and FR-A8 becomes its normal case rather than its
  edge case — which is a legitimate outcome, but one to discover early.
- **OQ12 [NEW per D1].** What is the deprecation path for the Go daemon? It
  keeps serving users until M6, so it needs a stated end-of-life and a
  user-facing migration note covering the v2 schema change.

---

## 13. Out of Scope

As v0.1, minus the analytics carve-out corrected in NG4.
