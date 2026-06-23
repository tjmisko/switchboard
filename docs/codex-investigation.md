# Investigation — a Codex status indicator in switchboard

> Status: **investigation only** (no code). Scope: what it would take to give
> OpenAI **Codex CLI** sessions the same Observe-tier status chip switchboard
> gives Claude Code — `working` / `idle` / `permission` — plus Navigate.
>
> Grounding: read against `openai/codex@main` source + developers.openai.com
> docs (June 2026), and corroborated against the real on-disk artifacts of a
> local Codex install (`~/.codex/config.toml`, `~/.codex/sessions/**/*.jsonl`).
> Live behavioural claims (hook process parentage, payload paths) are flagged
> ⚠ because there is **no Codex plan on this machine to run an interactive
> session** — those need verification on a box that can.

## TL;DR

Switchboard is already cleanly layered into an **agent-neutral half** and a
**Claude-specific half**. The agent-neutral half — discovery loop, pidfd death
watch, RPC, the store, `state.json`, chip ordering, focus, and the *entire*
Navigate stack (tty → pane → window) — needs **zero Codex-specific work**; it
keys on the controlling tty, which is agent-blind. A discovered `codex` process
maps to its window exactly like a `claude` one.

Only three seams are Claude-specific, and each has a Codex analog:

| Seam | Claude mechanism | Codex equivalent | Verdict |
|------|------------------|------------------|---------|
| **Discovery** | `comm == "claude"` + exe under `/claude/` + verb filter | `comm == "codex"` + **argv[1] subcommand filter** | Easy; the verb filter already exists |
| **Status enrichment** | 5 Claude Code hooks → `switchboard-ctl hook` → RPC | **Codex hooks** (recent) → reuse the *same* plumbing; **`notify`** (legacy) fallback | The big win: recent Codex copied Claude Code's hook design almost 1:1 |
| **Status self-heal** | tail the `.jsonl` transcript | tail the **rollout** `.jsonl` (different schema) | Idle/working is *easier*; **permission is not passively observable** ⚠ |

**The one hard finding** (§4): Codex deliberately does **not** write approval
("waiting on the user") events to its on-disk rollout file. So the `permission`
chip for Codex is only obtainable from the live **hook** stream — there is no
transcript-tail fallback for it the way there is for Claude. On older Codex
without hooks, "blocked on an approval" is invisible to a passive monitor.

---

## 1. What switchboard already gives Codex for free

These layers never name an agent; they operate on `proc.Info` and the tty:

- **Discovery scan loop** (`internal/discovery`) — generic `/proc` walk with a
  seen-set; only the `IsClaude` predicate is agent-specific.
- **Death watch** (`internal/osproc`, pidfd) — keyed on PID.
- **Navigate** (`internal/terminal`, `internal/wm`, `internal/mapping`, and
  `rpc.focus`) — the join is `claude PID → tty → pane → window`. The tty match
  (`internal/rpc/rpc.go:176`) is what makes focus work, and it is identical for
  any tty-attached process. **A Codex session reaches the Navigate tier with no
  new code.**
- **Store / RPC / subscription / `state.json` / chip order / `focused` /
  `suspended` / capabilities** — all PID- and tty-keyed.

So the work is confined to the three seams below. Everything in
`docs/portability-plan.md`'s Navigate work is reused verbatim.

---

## 2. Codex process model (Discovery)

Confirmed: the binary is a single Rust executable, `[[bin]] name = "codex"`
(`codex-rs/cli/Cargo.toml`), so **every** invocation shares `comm == "codex"`
and `argv[0] == codex`. The subcommand is **`argv[1]`** — unlike Claude, where
the interactive session has no verb and the binary name alone nearly suffices.
Codex *requires* an argv[1] filter:

| argv[1] | Kind | Track as a session? |
|---------|------|----------------------|
| *(none)*, `resume`, `fork` | interactive TUI | **yes** |
| `exec` (`e`) | headless/non-interactive | no |
| `app-server`, `mcp-server`, `mcp`, `remote-control`, `sandbox` | server/daemon | no (the `claude daemon`/`claude mcp` analog) |
| `login`/`logout`, `doctor`, `update`, `completion`, `apply`, `cloud`, `resume --last`-style utils, … | one-shot util | no |

`internal/discovery/discovery.go` **already reads `p.Args`** and already has a
verb blocklist (`backgroundSubcommands` → `isBackgroundSubcommand`). An
`IsCodex` predicate is the same shape, inverted to an interactive **allowlist**
(empty verb / `resume` / `fork`) because Codex's surface is mostly non-session
subcommands. No exe-path check is needed (Codex isn't installed under a
distinctive dir the way Claude is under `~/.local/share/claude/`); a bare
`comm == "codex"` + argv filter is enough, with the same "kernel masked exe →
benefit of the doubt" tolerance.

> Note: the `notify`/hook payload carries `client: "codex-tui"` for TUI turns,
> which is a clean post-hoc confirmation — but it is only available at hook
> time, not from `/proc`, so the argv[1] filter remains the discovery signal.

**Verified locally:** `codex` is on `PATH` (`/usr/local/bin/codex`) and
`~/.codex/sessions/` exists with the dated rollout layout below.

---

## 3. Status enrichment — two paths, version-gated

### 3a. Codex hooks (recent Codex — the primary path)

Recent Codex (community-dated ~v0.135+, exact version unconfirmed ⚠) ships a
**Claude-Code-style hooks system** — same vocabulary, same **stdin-JSON**
convention, same allow/deny gating (`codex-rs/hooks/`, docs:
developers.openai.com/codex/hooks). Event names (from `schema.rs`
`HookEventNameWire`):

`SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`,
`PermissionRequest`, `Stop`, plus `PreCompact`/`PostCompact`,
`SubagentStart`/`SubagentStop`.

This maps almost **1:1** onto the existing `statusFromHookEvent`
(`internal/rpc/rpc.go:354`):

| Codex hook event | switchboard status |
|------------------|--------------------|
| `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `SubagentStart`/`Stop` | `working` |
| `PermissionRequest` | `permission` |
| `Stop`, `SessionStart` | `idle` |

Why this is the big win: the hook payload's common fields are `session_id`,
`transcript_path`, `cwd`, `hook_event_name` — **the same snake_case keys
`cmdHook` already parses from stdin** (`cmd/switchboard-ctl/main.go:274`). The
forwarder, the RPC `hook` command, the ancestor-walk PID resolution, and the
status state machine are reusable with only a new event→status mapping. Wiring
is a `~/.codex/hooks.json` (or inline `[hooks]` in `config.toml`) pointing at
`switchboard-ctl hook <event>`, mirroring the README's Claude block.

⚠ **Must verify on a live box:** (1) the hook command runs as a **child of the
codex TUI process** so `getppid()`/the ancestor-walk resolves to the tracked
PID; (2) `transcript_path` in the payload points at the rollout `.jsonl` (needed
for §3c). Both are true for Claude; both are *expected* for Codex but unproven
here (no plan to run a session). Locally, `~/.codex/config.toml` has **neither**
`notify` nor `[hooks]` set, so nothing is wired today.

### 3b. `notify` (legacy Codex — the fallback)

Older Codex has only `notify = ["prog", "arg"]` in `config.toml`. It fires
**one** event type, `agent-turn-complete`, and passes the JSON payload as the
**last argv** (not stdin), fire-and-forget (`codex-rs/hooks/src/legacy_notify.rs`).
Payload: `{type, thread-id, turn-id, cwd, client, input-messages,
last-assistant-message}` (kebab-case).

This is a **Stop-only** signal → it can drive `idle`, nothing else. It gives no
`working` edge (you'd re-derive that from the rollout file or a proc heuristic)
and no `permission` edge at all. Supporting it means a tiny
`switchboard-ctl codex-notify` shim that reads argv-JSON instead of stdin. It is
the lowest-common-denominator, present on every Codex version.

### 3c. Rollout-file self-heal (the transcript analog)

Codex persists every session — including the interactive TUI — to
`~/.codex/sessions/YYYY/MM/DD/rollout-<ISO8601>-<uuid>.jsonl`
(`codex-rs/rollout/src/recorder.rs`). **Verified locally**: the path layout and
the line shape match exactly. Every line is three keys:

```
{"timestamp":"<RFC3339 ms>","type":"<record_type>","payload":{ ... }}
```

with top-level `type` ∈ `session_meta`, `response_item`, `event_msg`,
`turn_context`, `compacted`, `inter_agent_communication`. This is a **different
schema** from Claude's (`message.role` + content blocks); the existing
`internal/transcript` parser cannot read it. But the *abstractions* port:

| `internal/transcript` concept | Claude signal | Codex rollout signal |
|-------------------------------|---------------|----------------------|
| `NewestSignal` → activity | newest assistant/user entry | `event_msg`/`task_started`, `response_item`/`message` role=assistant |
| `NewestSignal` → interrupt | `[Request interrupted by user]` text | `event_msg`/`task_complete`; `event_msg`/`turn_aborted` reason=`interrupted` |
| `ResolutionState` (permission cleared) | assistant msg / interrupt after prompt | **not recordable — see §4** |

Good news: idle↔working self-heal is **more reliable** for Codex than for
Claude. Codex writes *explicit, persisted* turn boundaries — `task_started`,
`task_complete`, `turn_aborted{reason}` (`rollout/src/policy.rs` allows these) —
where Claude forces a heuristic over a free-text interrupt marker. A
Codex-aware parser behind the same interface is arguably simpler.

The cleanest refactor: lift `internal/transcript` to an interface
(`SessionLog` with `NewestSignal`/`ResolutionState`) and add a `codexrollout`
implementation selected by the session's agent kind. The self-heal call sites in
`cmd/switchboard/main.go` (`selfHealStuckStatus`, `selfHealStaleAttention`)
dispatch on kind.

---

## 4. The hard constraint: `permission` is not passively observable on Codex

This is the single most important finding and it shapes the whole design.

Codex's `should_persist_event_msg` (`codex-rs/rollout/src/policy.rs`)
**deliberately drops** every approval-related event from the on-disk rollout:
`ExecApprovalRequest`, `ApplyPatchApprovalRequest`, `RequestPermissions`,
`RequestUserInput`, `ElicitationRequest` — and the user's decision
(`ReviewDecision`) is consumed in-memory, never written. So while a Codex
session sits blocked on "approve this command?", the rollout tail shows a
`function_call` with no `function_call_output` yet — **byte-for-byte identical
to a command that is simply still running.** You cannot tell "blocked on the
user" from "busy" from the file.

Contrast Claude Code, where the transcript records enough that
`transcript.ResolutionState` can recover the permission state with no live hook
(this is exactly what `selfHealStaleAttention` exploits). Codex has no such
fallback. Consequences:

- **With hooks (3a):** `permission` works fully — `PermissionRequest` sets it
  live. But its *clearing* can't be confirmed from the rollout file. Resolution
  must be inferred from a **subsequent `task_complete` / new assistant message /
  `turn_aborted` after the prompt** (feasible, just a different rule), or fall
  back to the TTL backstop (`permissionDecayTTL`) that already exists.
- **Without hooks (notify-only / old Codex):** there is **no** `permission`
  signal at all for a passively-discovered session. The chip can only ever show
  `working`/`idle`. This must be documented as a tier limit, not a bug.

The richer live protocols (`app-server` `thread/status/changed` with
`waitingOnApproval`; `codex mcp` `elicitation/create`; `exec --json`) all expose
approval state cleanly — but **every one requires owning/launching the codex
process.** None is a side-channel into a user-started interactive TUI. They are
irrelevant to a `/proc`-discovery monitor and should be explicitly ruled out.

**Observability matrix (passive monitor, session discovered via `/proc`):**

| Signal | Hooks (recent) | notify (legacy) | rollout tail | app-server/mcp/exec |
|--------|:--:|:--:|:--:|:--:|
| working | ✅ | ➖ (infer) | ✅ (`task_started`) | ✅ but owns proc |
| idle / turn-complete | ✅ | ✅ | ✅ (`task_complete`/`turn_aborted`) | ✅ but owns proc |
| **permission / waiting-on-approval** | ✅ live only | ❌ | ❌ **dropped from file** | ✅ but owns proc |
| interrupt | ✅ | ❌ | ✅ (`turn_aborted` reason=`interrupted`) | ✅ but owns proc |

---

## 5. State-schema decision

`state.json` is a frozen public contract (`docs/state-schema.md`), and the
`claude` block (`status`, `session_id`, `transcript`) is agent-specific. Two
ways to carry Codex status:

- **(A) Additive `codex` block** — mirror `claude` with a parallel optional
  block; renderers gain an `agentStatus(s)` helper that reads whichever is
  present. **Non-breaking** (additive optional fields are explicitly allowed by
  the versioning rules). Downside: two near-identical blocks; a `kind` is
  implicit in which block exists.
- **(B) Neutral `agent` block** — `{ "kind": "claude"|"codex", "status": …,
  "session_id": …, "transcript": … }`, with `claude` retained as a deprecated
  alias for one version. Cleaner long-term, models multi-agent directly, but
  touches the golden fixture and the contract doc (a deliberate, reviewed
  change).

Recommendation: **(B)**, because the moment there's a second agent the right
model is "a session has *an* agent," not "a session has a claude *and* a codex."
But (A) ships faster with zero contract risk. This is a genuine call for the
maintainer — see Open Questions.

Either way the renderers (`cmd/claude-tui/main.go:223`,
`cmd/switchboard-waybar/main.go:121`) need one new helper; the status *values*
(`working`/`idle`/`permission`) and their colors are unchanged. Consider a
fourth value `waiting-input` for Codex's `RequestUserInput` (distinct from a
command approval) — optional polish.

---

## 6. Proposed work breakdown

Phased to match the project's existing style; effort is rough, risk is the
honest part.

| Phase | Work | Effort | Risk |
|-------|------|--------|------|
| **C-1 Discovery** | `IsCodex` predicate (comm + argv[1] allowlist); tag session with agent kind; conformance fixtures for codex argv shapes | S | Low — mirrors `IsClaude`, well-tested seam |
| **C-2 Schema** | Decide (A)/(B); add the block + `kind`; update golden + `state-schema.md`; `agentStatus` helper in both renderers | S–M | Low–Med — touches the frozen contract (gated, reviewed) |
| **C-3 Hooks** | Codex event→status map; confirm `cmdHook` getppid model on a live session; ship a `~/.codex/hooks.json` recipe in README | S | **Med — needs a live Codex box to verify** ⚠ |
| **C-4 Rollout self-heal** | `SessionLog` interface; `codexrollout` parser (`task_started`/`task_complete`/`turn_aborted`); dispatch self-heal by kind; Codex permission-resolution rule (no file fallback) | M | Med — new parser, but explicit turn records make it cleaner than Claude's |
| **C-5 notify fallback** *(optional)* | `switchboard-ctl codex-notify` argv-JSON shim → idle edge for legacy Codex | S | Low — but idle-only, no permission |
| **C-6 Docs/polish** | README Codex section; capability/limitation note on the permission gap; optional `waiting-input` status | S | Low |

Sequencing: C-1 → C-2 unlock everything; C-3 is the high-value path and the one
true unknown (live verification); C-4 backstops C-3 and is the only path on
notify-only Codex; C-5/C-6 are polish.

The reusable surface is large: the entire Navigate stack, the daemon core, the
RPC hook command, the forwarder, and the self-heal *scaffolding* are kept. The
genuinely new code is one predicate, one schema block, one event map, and one
rollout parser.

---

## 7. Open questions (need a live Codex session — blocked here, no plan)

1. **Hook process parentage** — does `switchboard-ctl hook` run as a child of
   the codex TUI PID (so `getppid()`/ancestor-walk resolves)? (Expected; the
   load-bearing assumption of C-3.)
2. **`transcript_path` target** — does the hook payload's `transcript_path`
   point at the `~/.codex/sessions/**/rollout-*.jsonl`? (Needed to wire C-4 to
   C-3.)
3. **Hook availability floor** — exact minimum Codex version with the hooks
   system, to gate C-3 vs the C-5 notify fallback.
4. **Permission-clear rule** — confirm that a `task_complete` / assistant
   message reliably follows an approved-and-run command in the rollout, so C-4
   can clear a `permission` chip without a live decision event.
5. **Schema choice (A) vs (B)** — maintainer call on the public contract.
6. **`exec`/headless sessions** — out of scope (not interactive TUIs), but
   confirm none should surface as chips.

## 8. Recommendation

Build **C-1 → C-2 → C-3 → C-4**, with **(B)** the neutral `agent` block, gating
`permission` honestly: full fidelity on hooks-capable Codex, and an explicit
"Codex (no hooks): working/idle only — approval state not observable" capability
note otherwise. Skip the live-protocol routes (app-server/mcp/exec) entirely —
they require owning the process and don't serve a discovery monitor. Treat the
permission-observability gap (§4) as a documented tier boundary, exactly how the
project already documents Observe-vs-Navigate degradation.

The headline for a maintainer: **~80% of a Codex indicator already exists and is
agent-neutral; the new work is one discovery predicate, one schema block, one
hook map, and one rollout parser — and the only real risk is that the highest-
value signal (`permission`) depends on Codex's recent hooks system and on a live
verification this machine can't perform.**

---

### Source pointers

- Process model: `codex-rs/cli/Cargo.toml`; CLI reference — developers.openai.com/codex/cli/reference
- Hooks: `codex-rs/hooks/src/schema.rs`, `legacy_notify.rs`; developers.openai.com/codex/hooks, /config-advanced
- Rollout schema + filter: `codex-rs/rollout/src/{recorder,policy}.rs`, `codex-rs/protocol/src/protocol.rs`
- Live protocols (ruled out): `codex-rs/app-server-protocol/src/protocol/v2/thread.rs` (`ThreadStatus`/`ThreadActiveFlag`), `codex-rs/exec/src/exec_events.rs`, `codex-rs/mcp-server/src/`
- Local corroboration: `~/.codex/config.toml`, `~/.codex/sessions/2026/**/rollout-*.jsonl`
