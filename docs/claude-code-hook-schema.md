# Claude Code hook payload schema (binary-verified)

The single authority for what switchboard can rely on arriving in a hook
payload. Every field below was read off the **construction site** in the Claude
Code bundle, not from prose documentation — extract with:

```bash
strings ~/.local/share/claude/versions/<ver> > cc.strings
rg -o 'function \$f\(.{0,700}' cc.strings          # the common base builder
rg -o 'hook_event_name:"PostToolUse".{0,200}' cc.strings
```

**Verified against 2.1.222** (2026-08-05). Re-verify on a version bump; record
the version you verified against when you do.

---

## 1. The common base — every hook input spreads it

```js
function $f(e,t,r){
  let n = t ?? Ot(), o = r?.agentType ?? rH(), …
  return {
    session_id:      n,
    transcript_path: HM(n),
    cwd:             Nt(),
    prompt_id:       XPt() ?? void 0,
    permission_mode: e,
    agent_id:        r?.agentId,     // ← subagent identity
    agent_type:      o,
    effort:          a,
  }
}
```

Call sites look like `{...$f(ctx, void 0, toolUseContext), hook_event_name: …}`,
so **these eight fields are present on every hook event**, including
`PermissionRequest` and `PostToolUse`.

| field | meaning | present when |
|---|---|---|
| `session_id` | parent session UUID | always |
| `transcript_path` | **the parent session's** `.jsonl` — see §3 | always |
| `cwd` | working directory | always |
| `prompt_id` | UUID correlating a user prompt with every event until the next prompt; equals the OTel `prompt.id` | absent until the first user input of the process lifetime |
| `permission_mode` | `default` \| `acceptEdits` \| `bypassPermissions` \| `plan` \| `dontAsk` \| `auto` | always |
| **`agent_id`** | **subagent identifier — the main-thread/subagent discriminator** | **only when the hook fires from within a subagent**; absent on the main thread, *even in `--agent` sessions* |
| `agent_type` | e.g. `general-purpose` | inside a subagent (with `agent_id`), **and** on the main thread of an `--agent` session (without `agent_id`) |
| `effort` | `{level}` for the current turn | tool-context hooks on models supporting effort |

The binary's own field doc for `agent_id` is explicit about the intended use:

> "Subagent identifier. Present only when the hook fires from within a subagent
> (e.g., a tool called by an AgentTool worker). Absent for the main thread, even
> in `--agent` sessions. **Use this field (not `agent_type`) to distinguish
> subagent calls from main-thread calls.**"

⚠ `agent_type` is **not** a subagent discriminator — it is populated on the main
thread of an `--agent` session. Only `agent_id` is.

`agent_id` identifies the same subagent as the `subagents/agent-<id>.*` files,
so it joins the hook stream to the on-disk fanout records directly.

⚠ **The exact string shape is not yet confirmed.** On-disk files are
`agent-<rawid>` where `rawid` itself begins with `a` (e.g. file
`agent-a158b13da3d13b0ea` ⇒ `rawid = a158b13da3d13b0ea`; a named teammate's file
`agent-aauth-tests-7152e6a858d30551` shows the same doubled `a`). The daemon's
`Subagent.AgentID` and `history.Event.AgentID` both store the **prefix-stripped**
`rawid` — but both are written by the Observer from a directory scan, **not**
from a hook, so they are not evidence about what the hook sends. Whether the
hook's `agent_id` is `a158b13da3d13b0ea` or `agent-a158b13da3d13b0ea` remains
unobserved (this is plan T1).

⇒ **Normalize `agent_id` once at the RPC boundary** (strip a leading `agent-` if
present) so every map in the daemon is keyed identically. Otherwise a `Pending`
map keyed by the raw hook value will silently fail to join the Observer's
seen-set keyed by the stripped value.

---

## 2. Per-event fields (tool events)

Read off the emitters verbatim:

```js
// PreToolUse                (guarded by a registration check)
{...$f(…), hook_event_name:"PreToolUse",  tool_name, tool_input, tool_use_id}

// PermissionRequest
{...$f(…), hook_event_name:"PermissionRequest", tool_name, tool_input, permission_suggestions}

// PostToolUse
{...$f(…), hook_event_name:"PostToolUse", tool_name, tool_input, tool_response, tool_use_id, duration_ms}

// PostToolUseFailure
{...$f(…), hook_event_name:"PostToolUseFailure", tool_name, tool_input, tool_use_id, error, is_interrupt, duration_ms}

// PostToolBatch
{...$f(…), hook_event_name:"PostToolBatch", tool_calls}
```

| | `tool_name` | `tool_input` | `tool_use_id` | other |
|---|---|---|---|---|
| `PreToolUse` | ✅ | ✅ | ✅ | — |
| `PermissionRequest` | ✅ | ✅ | ❌ | `permission_suggestions` |
| `PostToolUse` | ✅ | ✅ | ✅ | `tool_response`, `duration_ms` |
| `PostToolUseFailure` | ✅ | ✅ | ✅ | `error`, `is_interrupt`, `duration_ms` |

**The load-bearing asymmetry:** `PermissionRequest` does **not** carry
`tool_use_id` — the emitter passes `toolUseID` to the hook *runner* but omits it
from `hookInput`. So a `PermissionRequest` → `PostToolUse` pair **cannot** be
correlated by tool-use identity without also wiring `PreToolUse` (which does
carry it, for the same call).

Available correlators for "is this `PostToolUse` the one my prompt was raised
for?", strongest first:

1. `agent_id` — **which writer**. Rules out every teammate.
2. `tool_input` — **which call**. Present on both events; rules out a different
   call by the same writer.
3. `tool_name` — **which kind of call**. Weakest; collides constantly
   (`Bash`). Insufficient alone — see
   [subagent-permission-oscillation.md](subagent-permission-oscillation.md).

`(agent_id, tool_name, tool_input)` needs **no new hook registration**.
`tool_use_id` via `PreToolUse` would be exact, at the cost of a hook firing on
every tool call in every session.

### ⚠ `tool_input` is NOT stable across the two events

`PermissionRequest` reports the input as it stands **before** the decision
resolves. `PostToolUse` reports `updatedInput` — the input **after** it. They
differ by design on several approval paths, verified in the bundle:

```js
updatedInput:{...e,command:g}, decisionReason:{…reason:"Bare output redirection with no command; path layer approved"}
updatedInput:{...e,path:r},    decisionReason:{type:"safetyCheck",reason:`permission-root relocation …`}
updatedInput:{...e,[Mkr]:lHe(t)}, suppressAlwaysAllowRule:!0        // injected keys
```

plus a `userModified` flag (the user can edit the call in the approval dialog),
and a `PermissionRequest` **hook** may itself return an `updatedInput` — the
binary logs `Hook … modified tool input keys: [...]`.

**Consequence for the permission gate:** a `tool_input` hash **match** is strong
positive confirmation, but a **mismatch proves nothing** — it is equally "same
call, input rewritten on approval" (exactly the approve path we want to clear
fast) and "different call by the same writer" (a sibling we must hold red for).
Note the rewrite paths include `command:` rewriting on **Bash**, the tool in the
2026-08-05 incident.

⇒ Never latch red on a hash mismatch. Treat the hash as: match ⇒ clear at hook
speed; mismatch ⇒ *fall through to the writer-routed transcript check*, which
asks the question that is actually decisive ("did the blocked writer resume?").

### Lifecycle events

`SubagentStart` / `SubagentStop` carry `agent_id`, `agent_type`, `session_id`
(parent), `cwd`, and on Stop `agent_transcript_path`. Neither carries
`tool_use_id` — correlate by `agent_id`. (This is the fact recorded in
`subagent-fanout-detection-plan.md`; note its scope is those two events. It does
**not** imply `tool_use_id` is absent from tool events — §2 shows it is present.)

---

## 3. `transcript_path` is always the parent's

`transcript_path` is `HM(session_id)` where `session_id` is the **parent
session**, so a hook fired from inside a subagent still reports the *parent's*
transcript. `SubagentStop` carries `agent_transcript_path` separately precisely
because of this.

Consequences for switchboard:

- `info.Transcript = req.Transcript` never gets clobbered by a subagent hook —
  benign.
- Any transcript read triggered by a subagent hook reads the **main thread's**
  file. While the main thread is blocked or quiet, that file is arbitrarily
  stale — the mechanism behind defect 3 in the oscillation incident.
- A subagent's own writes are only visible at
  `<transcript-without-.jsonl>/subagents/agent-<agent_id>.jsonl`. Derive it as
  the sibling of the stored transcript path, never from cwd or a project slug
  (`subagent-fanout-detection-plan.md` G10).

---

## 4. What switchboard currently does with this

| field | on the wire | parsed by ctl | used by daemon |
|---|---|---|---|
| `session_id` | ✅ | ✅ | ✅ |
| `transcript_path` | ✅ | ✅ | ✅ |
| `tool_name` | ✅ | ✅ | ✅ (`clearsPermission`) |
| **`agent_id`** | **✅ on every event** | **✅ (`main.go:405`, all events)** | **⚠ only in the fanout trigger — ignored by `clearsPermission`** |
| `agent_type` | ✅ | ✅ | fanout only |
| `tool_input` | ✅ | ❌ | ❌ |
| `tool_use_id` | ✅ (not on `PermissionRequest`) | ❌ | ❌ |
| `prompt_id` | ✅ | ❌ | ❌ |

`agent_id` is **already arriving and already forwarded** as `rpc.Request.AgentID`
on `PermissionRequest` and `PostToolUse`. The daemon simply does not read it
outside the `SubagentStart`/`Stop` branch.

---

## 5. Verification status

| claim | how verified | confidence |
|---|---|---|
| base fields incl. `agent_id` on every hook | `$f` construction site | **high** |
| `PermissionRequest` lacks `tool_use_id` | emitter construction site | **high** |
| `PostToolUse` has `tool_use_id`, `tool_input` | emitter construction site | **high** |
| `transcript_path` is the parent's | `HM(session_id)` + observed stale-anchor behavior in the 2026-08-05 incident | **high** |
| `agent_id` non-empty in practice for a subagent's hook | **observed live 2026-08-05 17:21** | **high** |
| `agent_id` arrives **bare** (no `agent-` prefix) | same observation | **high** |
| `transcript_path` is the parent's even from a subagent hook | same observation | **high** |

### 5.1 The live observation (plan T1 / T20, closed)

Instrumented daemon, three teammates in flight on one session:

```
hook-identity: pid=1090904 session=191d2044 event=PostToolUse
  agent_id="ac24f297af282c150" agent_type="general-purpose" tool="Bash"
  chip=permission pending="AskUserQuestion" S=3
```

Three distinct ids appeared across the sample — `a1a4f5cc80cc6d9c2`,
`ac24f297af282c150`, `acd4e3a19f8487bdd` — matching `S=3`, so teammates are
individually identified, not merged. `agent_type` rides alongside as documented.

Three things this settles:

1. **`agent_id` is present on `PostToolUse` from a subagent.** The whole
   writer-attribution design (T7, T17) is live rather than inert.
2. **The bare form is what arrives** — no `agent-` prefix. `normalizeAgentID`
   is therefore a no-op in practice; it stays as insurance, and it costs nothing.
3. **`transcript_path` stayed the parent's.** That session took many teammate
   hooks and its stored `transcript` remained
   `…/DigestDownloads/191d2044-….jsonl`. The unconditional
   `info.Transcript = req.Transcript` is safe, and §3's claim is confirmed from
   the wire and not only from the emitter.

Recheck all three on a Claude Code version bump — §5's table is the place to
record the result.
