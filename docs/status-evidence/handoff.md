# Implementer handoff — status evidence programme

You are implementing a planned programme in the Switchboard repo
(`~/Projects/switchboard`, Go). The plans are written; your job is to execute
them one phase at a time, with tests, and to stop at each phase boundary for
review.

## Read first, in this order

1. [README.md](README.md): decisions, phase order, and the rules every phase
   follows. The decisions are settled; do not reopen them.
2. The plan for your phase. Start with
   [phase-0-correctness.md](phase-0-correctness.md).
3. For phases 2–5, the linked GitHub issue (`gh issue view <n>`). The issue is
   the specification and the plan maps it onto the code.
4. `CLAUDE.md` in the repo, if present, and the owner's global instructions.

File:line references in the plans are from `main` at a06439f. Re-locate each
one with `rg` before editing; do not trust the line numbers.

## How to work

- **Branches.** One branch per PR, made with `gh worktree create --branch
  <type>/<name>`; the worktree lands at `.worktrees/<branch>`. Never work on
  `main`. Phase 1 ships as four PRs (1A–1D), listed in its plan.
- **Tests with the code.** Name them "should … when …". The plan's test list
  is the acceptance bar. For each important test, break the code once and
  confirm the test fails, then restore it.
- **Commits.** Conventional and atomic: `fix:`, `feat:`, `test:`, `docs:`,
  `refactor:`. No emoji. End every message with the attribution line your
  harness specifies.
- **Before pushing,** run `go vet ./...` and `TZ=UTC go test ./...`. CI is
  UTC, and one test has already broken on that.
- **PRs.** Draft, with `gh pr create -d`. The description lists the plan
  items covered, the tests added, and any intentional behaviour change.
- **Clock and content.** Pass the clock into new status logic. Keep content
  out of rpc, state and logs (see the README rules).
- **Persistence.** Additive fields only. Do not bump the schema.

## Do not

- Merge, deploy, push to `main`, or edit `~/.claude`, `~/.codex` or `~/.pi`
  config without the owner's explicit go-ahead for that specific action.
- Build in `~/Tools/switchboard`. It is a stale clone of the same module, and
  building there overwrites the live binaries. Deploys happen only from the
  main checkout via `scripts/deploy`, after a merge, when the owner says so.
- Decide anything the plans mark "report to the owner". For example, Phase 1's
  verify-first question about built-in Pi dialogs turning a chip red.
- Widen a phase's scope. Note follow-ups in the PR description instead.

## If you delegate to subagents

On this machine, open-ended research subagents die without output. Give each
one at most ~6 named files, an explicit tool-call budget and an output cap.
Make writing an output file its last action, and check that the file exists
rather than waiting for a reply.

## When a phase is done

1. Every test in the plan exists and passes, including under `TZ=UTC`.
2. The PR is open (draft is fine) and its description maps to the plan.
3. You set the Status column for that phase in README.md (in the PR).
4. You stop and report to the owner:
   - the PR link;
   - what was built and what was not;
   - test results, with any failures quoted;
   - every deviation from the plan and why;
   - open questions.

   Then wait. Do not start the next phase until the owner says so.

## Start here: Phase 0

Two independent fixes, detailed in
[phase-0-correctness.md](phase-0-correctness.md):

- **0a.** A failed Claude fan-out scan must return the prior observation with
  its original freshness, not a newly dated one.
- **0b.** The pidfd death callback must end only the session lifetime it was
  registered for, and a re-watch of the same PID must not be silently dropped.

Suggested branches: `fix/claude-scan-freshness` and `fix/death-callback-fence`.
