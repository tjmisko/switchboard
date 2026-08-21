# Phase 1 — Codex process discovery hardening

## Mission

Ensure each real interactive Codex TUI produces exactly one Switchboard root and
that Codex subprocesses, utility commands, app-server components, and sandbox
wrappers never become sessions.

This phase is independent of the status graph and can run in parallel with the
ground-truth and contract workstreams.

## Ownership and dependencies

**Workstream:** D1

**Exclusive write ownership:** `internal/discovery/**`

**Dependencies:** none beyond the current `osproc.Info` contract.

Do not add status, app-server, state, RPC, or renderer changes. If discovery
needs a new process field, report that requirement to the coordinator; do not
edit `internal/osproc` in this workstream.

## Current defect

`IsCodex` currently requires only `Comm == "codex"` and then treats any leading
flag as an interactive TUI. During a tool call, `codex-linux-sandbox` can present
with `comm=codex` and an argv beginning with sandbox-policy flags. The scanner
therefore publishes transient ghost roots.

The current blocklist also drifts behind the CLI command surface and classifies
utility flags such as `--help` and `--version` as sessions.

## Target classifier

The classifier answers two independent questions:

1. **Is this the real Codex CLI executable?**
2. **Does this invocation enter an interactive TUI?**

### Executable identity

Accept when all available evidence is consistent with the Codex CLI:

- `Comm == "codex"`; and
- if `Args[0]` is available, its basename is exactly `codex` (allow an optional
  platform suffix only if a real installation fixture proves it); and
- if `Exe` is available, its basename is exactly `codex` or the known installed
  launcher relationship is covered by a test.

Reject explicitly:

- `codex-linux-sandbox` in `Args[0]` or `Exe`;
- app-server daemon/proxy processes;
- a different executable merely setting its comm to `codex`.

Masked `Args`/`Exe` must fail conservatively unless the remaining evidence is
strong enough to prove a bare interactive Codex process. Pin the chosen behavior
with a test and document why it cannot admit the sandbox wrapper.

### Invocation parsing

Do not treat `Args[1]` as the verb. Walk recognized global options first,
including options that consume a value and repeatable enable/disable flags. The
first non-option token is the command or positional prompt.

Interactive forms include:

- bare `codex`;
- global TUI options followed by no command;
- a positional initial prompt;
- interactive `resume` and `fork` forms.

Non-interactive/utility forms include at least the commands printed by the
installed CLI:

- `agents`, `exec`, `e`, `review`, `login`, `logout`, `mcp`, `mcp-server`,
  `plugin`, `app-server`, `remote-control`, `completion`, `update`, `doctor`,
  `sandbox`, `debug`, `apply`, `queue`, `archive`, `delete`, `migrate-rollouts`,
  `unarchive`, `cloud`, `exec-server`, `features`, and `help`;
- global `--help`, `-h`, `--version`, and `-V` exits;
- malformed option/value combinations, unless observed Codex behavior proves
  they still launch a TUI.

Prefer an explicit interactive-mode parser over an ever-growing inverse
blocklist. Unknown command-like tokens should fail closed; a natural-language
positional prompt should remain supported according to a documented rule and
tests.

## Test matrix

Add table-driven tests covering:

- exact real `codex` argv/exe shapes from bare, configured, resume, fork, and
  positional-prompt invocations;
- `codex-linux-sandbox --sandbox-policy-cwd ...` with `Comm == "codex"`;
- app-server `daemon`, `proxy`, schema generation, and listener forms;
- every current top-level utility command;
- all help/version spellings;
- global options with values before interactive and non-interactive commands;
- `--` handling;
- missing/masked Args and Exe;
- misleading comm/exe/argv combinations;
- scanner seen-set behavior: rejecting a sandbox PID must not prevent a later
  real Codex PID from being discovered.

Tests must not execute Codex or inspect the live process table. They use
`osproc.Info` fixtures and the injected scanner source.

## Acceptance

- The live sandbox-wrapper shape is rejected by a regression test.
- One representative real TUI shape is accepted for bare, options, resume, and
  fork modes.
- Every installed non-TUI command is rejected.
- Help/version invocations are rejected.
- `go test ./internal/discovery` passes.
- Existing Claude and headless discovery behavior is byte-for-byte unchanged.
- No files outside `internal/discovery/**` are modified.

## Handoff

Return the classifier rules and test matrix with the commit. The daemon
integration agent consumes discovery unchanged; it must not duplicate Codex
argv filtering elsewhere.

