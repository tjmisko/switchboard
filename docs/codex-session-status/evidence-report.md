# Codex 0.149 session-status evidence

## Result and provenance boundary

No already-running Codex app-server control endpoint was available, so this
workstream did not perform a live protocol capture. The installed CLI is
`codex-cli 0.149.0`. Its experimental JSON schema generator produced the
schemas used for every protocol fixture in
`internal/provider/codex/testdata/captures`.
The generated aggregate schema bundle had SHA-256
`4f4a8d8f53f971b97f818639f58c8d26bb68bfcdfa2d2f20572cb97e6761ab91`.

The fixture event ordering is a deterministic test sequence, not a claim about
observed server ordering. The fixtures are suitable for parser and reducer
tests, but an observer must tolerate snapshots and notifications arriving in a
different order until live ordering evidence is captured.

The experimental schema was regenerated from the same installed 0.149.0 CLI on
2026-08-23 for wait-ownership feature detection. It confirms
`thread/settings/updated` with reviewer values `user`, `auto_review`, and legacy
`guardian_subagent`; string-or-integer request IDs and
`serverRequest/resolved`; blocking and auto-resolution metadata on user-input
requests; and `source.subAgent.other`. It also labels
`item/autoApprovalReview/started|completed` payloads unstable. The adapter
therefore treats those notifications as supplementary evidence rather than its
primary ownership key.

| Artifact | Provenance | What it establishes |
|---|---|---|
| `initialize.jsonl` | schema-derived | Initialization envelope and experimental API capability request |
| `root-with-two-children.jsonl` | schema-derived | One root, two stable child identities, parentage, ordering permutations, thread/turn status, and spawn/message/follow-up item shapes |
| `child-waiting-approval.jsonl` | schema-derived | `waitingOnApproval`, approval request, resolution, and flag clearing |
| `child-waiting-user-input.jsonl` | schema-derived | `waitingOnUserInput`, input request, sanitized resolution, and flag clearing |
| `drain-to-idle.jsonl` | schema-derived | Completed child turns, wait/close lifecycle, and idle statuses |
| `thread-list-descendants.json` | schema-derived | Direct-parent and ancestor list filter request/response shapes |
| `proxy-help.txt` | installed CLI help | The 0.149.0 CLI exposes `app-server proxy` and `--sock` |

The schema-derived scenario contains one synthetic root, two distinct
synthetic children, stable direct parentage, and a complete spawn-to-shutdown
lifecycle. Synthetic UUIDv7-shaped IDs preserve equality relationships, and
all times use a fixed epoch starting at `1700000000`.

## Supported proxy availability

Installed `codex app-server --help` lists `proxy` as a command that proxies
stdio bytes to a running app-server control socket. Installed
`codex app-server proxy --help` also exposes an optional `--sock` path. A
sanitized transcription is retained in `proxy-help.txt`. The coordinator's
contemporaneous review found no mention of `app-server proxy` in the public
app-server transport documentation, so code should treat it as an installed
0.149.0 capability and continue to version-check it.

A connection through `codex app-server proxy` was attempted against the
default endpoint. The sandboxed attempt could not access the endpoint; the
approved out-of-sandbox retry reported that the control socket did not exist.
No app-server was started to make the capture possible, and no alternate raw
socket client was used. Consequently the running app-server version and
negotiated live capabilities are unknown.

## Safe local observations

These observations are live point samples, but they are not app-server event
captures:

- The process environment exposed both IDs as non-empty:
  `CODEX_THREAD_ID=<redacted-thread>` and
  `CODEX_SESSION_ID=<redacted-session>`.
- The two redacted values were unequal.
- A strictly read-only SQLite connection (`mode=ro&immutable=1`) found rows for
  both values. The current thread had a spawn edge whose direct parent equaled
  `CODEX_SESSION_ID`.
- At the point sample, that parent had three direct child edges, all with the
  persisted edge status `open`.
- The current child's persisted source metadata had depth 1 and a nickname
  present. Its role was absent. Values and paths were not retained.
- The installed state database had `threads` and `thread_spawn_edges` tables,
  but no turn-status table. Prompts, preview text, rollout paths, working
  directories, and thread IDs were not queried into the report or fixtures.
- No standalone hooks file was present, so there was no already-configured
  `SubagentStart` or `SubagentStop` payload to inspect. Hooks were not added.

The database point sample corroborates persisted direct parentage and an
`open` child lifecycle edge. It does not establish app-server notification
ordering, runtime status, reconnection behavior, or completion retention.

## Evidence questions

1. **Does `thread/status/changed` arrive before or after the corresponding turn
   or collaborative-item notification?** Unresolved. The generated schemas do
   not specify inter-message ordering and no live proxy connection was
   available. Fixture ordering is illustrative only.

2. **Does a child carry `parentThreadId` immediately at thread start and after a
   daemon reconnect?** Partially schema-derived and partially corroborated at
   rest. `Thread` has an optional `parentThreadId`, and `thread/started` carries
   a full `Thread`; the local database point sample retained an exact direct
   edge and matching parent in source metadata. Immediate population and
   post-reconnect behavior remain unresolved.

3. **Are completed children returned by an ancestor query, and for how long?**
   Unresolved. `thread/list.ancestorThreadId` is documented by the installed
   schema to return spawned descendants at any depth while excluding the
   ancestor, but the schema defines no completion-retention duration. The
   completed descendants in `thread-list-descendants.json` are a parser case,
   not live retention evidence.

4. **Does the root become idle while children remain active?** Unresolved live.
   Root and child runtime statuses are independently expressible, and the
   schema-derived fixture includes an idle root with active children, but the
   local spawn-edge database does not expose runtime status.

5. **Which event first exposes `newThreadId`, nickname, role, and lifecycle
   state?** Schema-derived: a `collabAgentToolCall` spawn item exposes the new ID
   in `receiverThreadIds` and may expose `pendingInit`/`running` in
   `agentsStates`. It has no nickname or role fields. A `thread/started`
   notification or a `thread/read`/`thread/list` snapshot can expose
   `agentNickname`, `agentRole`, `parentThreadId`, and duplicated spawn metadata
   in `source.subAgent.thread_spawn`. Which of those messages arrives first is
   unresolved.

6. **Are waiting flags present on inactive/background children?** The schema
   permits `activeFlags` only on status type `active`; the allowed flags are
   `waitingOnApproval` and `waitingOnUserInput`. `idle`, `notLoaded`, and
   `systemError` have no `activeFlags` field. Whether a background child remains
   runtime-`active` while waiting is unresolved without live evidence.

7. **Does `codex app-server proxy` attach read-only without taking ownership
   away from the existing TUI?** Unresolved. The installed help describes a
   byte proxy to an already-running control socket, but no such socket was
   available for a safe attach test.

8. **Can two simultaneous proxy clients subscribe safely?** Unresolved. No
   first connection was available, and no service was started or reconfigured.

9. **Does the root process environment expose an exact thread ID throughout
   resume/fork behavior?** Partially observed for the current child process
   only. Both environment variables were set and unequal; the thread value
   matched the child row, while the session value matched its direct parent.
   Root-process, resume, and fork continuity remain unresolved.

## Sanitization and validation

All protocol messages parse independently. Forty request, response, and
notification envelopes were validated against the exact experimental schemas
generated by Codex 0.149.0, including method-specific initialization,
thread-read, thread-list, approval-response, and user-input-response payloads.

The repository artifacts contain only the documented synthetic UUID prefix,
fixed timestamps, empty previews, null collaboration prompts, sanitized input
placeholders, and synthetic paths. Scans found no real UUID, home/repository
path, username, hostname, socket endpoint, command text, reasoning, assistant
output, authorization data, or account/API identifier.

## Remaining capture handoff

When a control endpoint is already running in a future session, E0 should be
repeated without changing service lifecycle or configuration. That follow-up
should replace, not silently relabel, the schema-derived ordering and retention
cases; record app-server version/capabilities; test one and then two proxy
clients; snapshot both descendant filters before and after completion; and
sample root/child status transitions across reconnect. Any disagreement with
the frozen neutral graph contract is a coordinator-level stop-the-wave issue.

No user configuration, hook configuration, daemon, TUI, session, or live
service lifecycle was changed during this workstream.
