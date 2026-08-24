# Phase 7 — validation and rollout

## Mission

Prove the merged implementation is correct under deterministic replay,
concurrency stress, provider failure, and one controlled live Codex fanout. This
phase does not own implementation fixes: defects are routed to the workstream
that owns the affected path.

## Ownership and start gate

**Workstream:** V7

**May write:** `docs/codex-session-status/validation-report.md` as a new file.

**Must not write:** implementation files, existing tests, public/schema docs,
user configuration, live service files, or planning documents.

**Start after:** W2 merge. Documentation C9 may proceed in parallel after the
wire/behavior freeze.

## Validation environment

Use a dedicated worktree and unique resources:

```bash
export GOCACHE=/tmp/switchboard-v7-gocache
export GOTMPDIR=/tmp/switchboard-v7-tmp
mkdir -p "$GOCACHE" "$GOTMPDIR"
```

All fake-daemon sockets, state mirrors, history directories, fake process data,
and fake app-server transports live beneath `t.TempDir()` or the V7 temporary
root. Do not connect automated tests to the installed Switchboard daemon or
Codex control socket.

## Test stages

### Stage 1 — package and contract tests

Run the narrow suites first so ownership of a failure is obvious:

```bash
go test ./internal/discovery
go test -race ./internal/agentgraph ./internal/provider/...
go test -race ./internal/state ./internal/history
go test -race ./internal/fanout
go test ./cmd/claude-tui ./cmd/switchboard-waybar
go test ./cmd/switchboard-ctl
go test -race ./cmd/switchboard
```

Record exact commands, commit, Go version, and results. Environment-denied
Unix-socket or `/proc` tests are marked infrastructure failures, not silently
skipped; rerun them in the project's normal host test environment.

### Stage 2 — full suite and static checks

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./...
```

Run existing platform/build checks used by CI. A pre-existing flaky or
environment-specific failure must be reproduced on the baseline commit before
being classified as unrelated.

### Stage 3 — deterministic trace replay

Replay E0 fixtures through the fake app-server and daemon integration:

1. initial root idle;
2. two children spawn;
3. root idle with children active -> delegating;
4. child approval -> root permission/approval;
5. child user input -> root permission/user-input detail;
6. one child completes while the other waits;
7. waits resolve and children drain;
8. root returns idle;
9. disconnect, freshness expiry, reconnect, full resnapshot;
10. delayed old-generation notification is ignored.

For every step, assert:

- graph nodes and parent IDs;
- neutral reducer summary and `since` behavior;
- legacy status projection;
- state JSON and subscription publication;
- history transitions exactly once;
- TUI/Waybar presentation;
- no child is a top-level session or navigation target.

### Stage 4 — adversarial concurrency

Exercise:

- notification storm plus periodic reconcile;
- simultaneous process death and child status update;
- PID reuse after `Forget`;
- slow/blocked fake app-server while RPC subscribers and hooks run;
- proxy disconnect during a descendant query;
- daemon shutdown during reconnect backoff;
- repeated identical snapshots and update coalescing;
- two same-CWD Codex roots with interleaved child events;
- Claude and Codex roots active simultaneously.

Use race detection and goroutine-leak checks. Measure the state-store lock budget
with a provider fake blocked for a noticeable interval; snapshot subscriptions
and hook RPC must remain responsive because the wait occurs outside the lock.

### Stage 5 — compatibility replay

- Decode state files with no provider tag, Claude-only enrichment, Codex-only
  legacy enrichment, and new graph enrichment.
- Replay existing Claude timing-hazard, workflow, fanout, permission, history,
  naming, focus, suspend, and memory tests.
- Compare Claude shadow summaries against legacy summaries across the canonical
  case table.
- Confirm existing bar configurations still receive one slot per root.
- Confirm history readers handle old files without graph events and new files
  with both canonical and compatibility events.

### Stage 6 — controlled live acceptance

This stage requires explicit coordinator/user approval because it consumes model
usage and touches a live Codex session. It remains observational:

- use the installed daemon/app-server without starting/stopping/restarting it;
- do not modify global hooks/configuration;
- use one disposable root and two harmless children;
- compare Switchboard graph/status output to Codex's own agent view;
- test approval and user input only with harmless operations;
- stop by completing the disposable task, not by killing shared services.

Record only sanitized status/identity relationships. Do not commit prompts,
reasoning, command payloads, paths, or real IDs.

## Required acceptance matrix

| Scenario | Root summary | Child detail | Navigation |
|---|---|---|---|
| Root active, no children | working | none | root only |
| Root idle, one active child | delegating | child active | root only |
| Child waiting approval | permission | approval | root only |
| Child waiting for user input | permission | user input | root only |
| Main waiting, child active | permission | main wait + child active | root only |
| Two children waiting differently | permission | both reasons retained | root only |
| All children complete, root idle | idle | terminal/current-turn rows only | root only |
| App-server observation expired | unknown/fallback | stale marked/hidden per UI contract | root only |
| Unbound exact root ID | unknown | no guessed tree | root only |
| Sandbox wrapper process | no session | none | none |
| Claude existing fanout | legacy-equivalent summary | neutral nested rows | root only |

## Rollout controls

The daemon integration should expose an operational mode with at least:

- `auto` (default): use app-server when capability is available, degrade safely;
- `off`: retain root discovery/navigation and hook-only legacy Codex behavior;
- a test-only connector/transport injection, not a user-facing arbitrary
  shell string.

Absence of `codex`, absence of standalone app-server capability, version
mismatch, or connection failure must not prevent daemon startup or root
navigation in `auto` mode.

Useful content-free startup/runtime logs:

- observer enabled/disabled and detected protocol version;
- root bound/unbound, with only a short synthetic/redacted ID;
- reconnect attempt/backoff and snapshot generation;
- complete/partial/stale graph counts;
- unknown future enum names (rate limited);
- Claude shadow mismatch rule/status counts.

## Defect routing

V7 writes each failure in the report as:

```text
ID:
Owner:
Baseline/commit:
Command or replay step:
Expected:
Actual:
Fixture/log excerpt (content-free):
Reproducibility:
```

Route by ownership:

- classifier -> D1;
- graph/reducer/provider contract -> coordinator/C2;
- Codex protocol/binding/cache -> C3;
- Claude inference/fanout -> C4;
- state/history wire -> C5;
- daemon/RPC/ordering/lock -> C6;
- TUI/Waybar -> C7;
- history/timeline/diagnostics -> C8;
- public/schema docs -> C9.

The owner returns a fix commit; the coordinator merges it; V7 reruns the failed
stage plus relevant regression stages. V7 never makes a drive-by implementation
edit.

## Final report and completion gate

`validation-report.md` must contain:

- exact merged commit and environment;
- all commands and results;
- deterministic replay matrix results;
- concurrency/lock measurements;
- compatibility results;
- sanitized live acceptance result or an explicit approval/blocker note;
- unresolved defects with owners;
- recommendation: ship `auto`, ship disabled, or do not ship.

The feature is complete only when every required matrix row passes, no critical
defect remains, docs match emitted state/history JSON, and disabling the Codex
observer leaves existing discovery/navigation and Claude behavior intact.
