# Status evidence programme

The master document for six phases of work: two correctness fixes, Pi as a
first-class provider, then GitHub issues #95–#98. Each phase has its own plan;
this page holds the decisions, the order, the rules every phase follows, and
the status of each phase.

The survey behind it was made against `main` at a06439f (2026-10-05). File:line
references in the phase plans are from that revision; re-check them before
editing.

## Phases

| # | Plan | Issue | Depends on | Status |
|---|------|-------|-----------|--------|
| 0 | [Correctness fixes](phase-0-correctness.md) | — | — | merged (#101, #102) |
| 1 | [Pi as a first-class provider](phase-1-pi-provider.md) | — | 0 | merged (#103–#106), deployed 62b170e; manual E2E with owner |
| 2 | [Per-root decision record, `explain`](phase-2-decision-record.md) | [#95](https://github.com/tjmisko/switchboard/issues/95) | 1 | merged (#107) |
| 3 | [One pure status resolver](phase-3-resolver.md) | [#96](https://github.com/tjmisko/switchboard/issues/96) | 2 | merged (#108, #109) |
| 4 | [OS process birth token](phase-4-process-identity.md) | [#97](https://github.com/tjmisko/switchboard/issues/97) | 3 | merged (#110) |
| 5 | [Explicit observer outcomes, change-driven refresh](phase-5-observer-outcomes.md) | [#98](https://github.com/tjmisko/switchboard/issues/98) | 4 | in review |

The issues are the specification for phases 2–5. Their plans map the issue
text onto the code and add the tests; where a plan and its issue disagree, the
issue wins unless a decision below says otherwise.

Implementer entry point: [handoff.md](handoff.md).

## Decisions

All by the owner, 2026-10-05.

1. **Pi before the issues.** Pi becomes a first-class provider before #95.
   Phase 1 keeps Pi's precedence against herdr in one function so #96 can
   replace it rather than untangle it.
2. **Pi is tracked with or without herdr.** The process scanner discovers Pi
   itself, and herdr becomes one evidence source among several.
3. **Pi shows red while any dialog is open:** the core
   `ui_prompt_start`/`ui_prompt_end` span, or herdr's `herdr:blocked` counter
   above zero.
4. **Pi subagents are deferred.** A long-running subagent tool reads as
   working. Revisit when a subagent extension is actually in use.
5. **Pi cost is Pi's own per-message `cost`,** not a reprice from the
   canonical rate table.
6. **Claude and Codex cost should be the most exact estimate obtainable.**
   This is tracked as a separate issue, **not yet opened**; it is outside this
   programme.
7. **After Pi, the issues land in their own order:** #95 → #96 → #97 → #98.
8. **Two correctness fixes go first** (Phase 0), because both are confirmed
   bugs that would otherwise survive four phases.
9. **Earlier choices carried forward from the usage-limit work:** `limited` is
   a published-only status, and the hook privacy boundary stands, so nothing
   user-authored crosses RPC.

## Rules for every phase

- **Branch per unit of work.** Use `gh worktree create --branch <name>` under
  `.worktrees/`; never work on `main`. Make one logical change per commit,
  with a conventional message.
- **Write tests with the change, not after,** named "should … when …". A
  phase's test list is its acceptance bar, and a test that cannot fail is not
  one: mutate the code once to prove it bites.
- **New status logic takes its clock as a parameter.** Do not add new
  `time.Now()` reads on a decision path.
- **Content-free.** No new path may carry prompts, commands, tool inputs or
  transcript text into rpc, state, logs or decision records. A hook may carry
  a tool name, counts, ids, paths and a user-set session name. The existing
  Codex naming carriage (`Prompt` and `LastAssistantMessage` on `rpc.Request`)
  is unchanged, and Pi does not use it.
- **Additive persistence.** `state.json` stays schema v3, and new fields are
  `omitempty`. A schema bump wipes every user's state on upgrade
  (`internal/state/state.go:1430-1441`).
- **Deploy only from the main checkout,** with `scripts/deploy`, after the
  merge. `~/Tools/switchboard` is a stale clone of the same module, and
  building there silently overwrites the live binaries.
- **CI runs in UTC.** Run `TZ=UTC go test ./...` before pushing.
- **Update this table's Status column** when a phase merges.
