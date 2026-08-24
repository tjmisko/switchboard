# Phase 0 — live Codex ground truth and fixtures

## Mission

Capture one small, controlled Codex multi-agent run and turn it into sanitized,
deterministic fixtures. This closes the remaining gap between the installed
Codex 0.149 protocol schema and actual event ordering without changing user
configuration or implementation code.

This workstream is evidence-only. It does not design the neutral model or build
the observer.

## Ownership and isolation

**Workstream:** E0

**May write:**

- `internal/provider/codex/testdata/captures/**`
- `docs/codex-session-status/evidence-report.md` (new file only)
- scratch files under an E0-specific `/tmp` directory

**Must not write:** implementation Go files, existing documentation, user Codex
configuration, user rollout/SQLite files, or daemon/service configuration.

E0 is the only implementation-session agent permitted to connect to the live
Codex app-server. It must not start, stop, restart, archive, delete, or otherwise
manage the daemon. Use the supported `codex app-server proxy` connection to the
already-running control endpoint.

## Capture scenario

Use one disposable root task whose only purpose is status observation:

1. Record the initial `thread/list`/`thread/read` representation of the root.
2. Spawn two children with distinct roles/nicknames.
3. Keep one child running long enough to observe the parent idle while a child
   remains active.
4. Cause one child to request an approval.
5. Cause one child to request user input, if the available Codex surface can do
   so without modifying global configuration.
6. Resolve each wait, collect both child results, and observe drain to idle.
7. Record final descendant query results.

If generating either waiting state would require dangerous commands, global
configuration changes, or an unreviewed permission escalation, skip that action
and construct the missing fixture from the installed JSON schema. Mark it
`schema-derived` in the evidence report.

## Data to capture

- App-server initialization request/response and negotiated capabilities.
- Root thread object, including source, status, parent, role, and nickname
  fields.
- `thread/status/changed` notifications for root and children.
- Turn start/completion notifications.
- Collaborative tool items for spawn, message/follow-up, wait, and close/finish.
- `thread/list` filtered by `parentThreadId` and `ancestorThreadId`.
- Exact lifecycle-hook `session_id` and optional `turn_id` relationships, with
  values replaced by stable synthetic IDs.
- Hook payload shapes for `SubagentStart` and `SubagentStop` only if hooks are
  already configured; do not configure them for this phase.
- Read-only observations of `thread_spawn_edges` and turn status for the captured
  thread IDs, with all prompts/content removed.

## Sanitization contract

Fixtures may retain:

- stable synthetic UUIDs preserving parent/child equality relationships;
- event/method names, enum values, timestamps normalized to a fixed epoch;
- ordering, request IDs, status transitions, roles, and synthetic nicknames;
- token counts replaced with small representative integers.

Fixtures must remove or replace:

- prompts, assistant output, reasoning, command text, tool input, file content;
- home paths, repository paths, usernames, hostnames, socket paths, API/account
  identifiers, and authorization data;
- real thread/session/turn/item IDs;
- unrelated threads returned by a broad list query.

The sanitization script, if any, lives in `/tmp` for this phase. Fixtures are
reviewed manually before entering the repository.

## Fixture layout

```text
internal/provider/codex/testdata/captures/
  initialize.jsonl
  root-with-two-children.jsonl
  child-waiting-approval.jsonl
  child-waiting-user-input.jsonl
  drain-to-idle.jsonl
  thread-list-descendants.json
  manifest.json
```

`manifest.json` records the Codex CLI/app-server version, whether each fixture is
live-captured or schema-derived, and a short description. It contains no local
paths or user content.

## Questions the evidence report must answer

1. Does `thread/status/changed` arrive before or after the corresponding turn or
   collaborative-item notification?
2. Does a child carry `parentThreadId` immediately at thread start and after a
   daemon reconnect?
3. Are completed children returned by an ancestor query, and for how long?
4. Does the root become idle while children remain active?
5. Which event first exposes `newThreadId`, nickname, role, and lifecycle state?
6. Are waiting flags present on inactive/background children?
7. Does `codex app-server proxy` attach read-only without taking ownership away
   from the existing TUI?
8. Can two simultaneous proxy clients subscribe safely?
9. Does the root process environment expose an exact thread ID throughout
   resume/fork behavior?

## Acceptance

- The capture has one root, two distinct children, stable parentage, and a full
  spawn-to-drain lifecycle.
- Each fixture parses independently and has deterministic synthetic IDs/times.
- The manifest labels provenance honestly.
- A content scan finds no prompt, reasoning, command, user path, or real ID.
- The evidence report answers every question or marks it explicitly unresolved.
- No user configuration or live service lifecycle was changed.

## Handoff to C3

E0 gives the Codex observer agent the fixture commit and evidence report. C3
must implement against these fixtures, not the live service. Any disagreement
between live evidence and the frozen neutral contract is escalated to the merge
coordinator; neither E0 nor C3 edits `internal/agentgraph` directly.
