# Codex session-status validation report

> **Final V7 disposition:** the deterministic implementation, adversarial,
> compatibility, renderer, wire, documentation, vet, and build gates pass at
> final integration commit `080c7b50c4717af17140db2d9456b1595df00871`.
> Runtime tests exercised its code-identical parent
> `328b6c8cdeb7bbd6a2e5337fa68a37f3c2a83ad6`; the final commit removes only
> five trailing blank lines from planning documents and passed the final
> documentation, link, JSON, golden, and branch-wide diff checks.
> **Recommendation: ship disabled** until the approval-gated live Codex
> acceptance and the sandbox-blocked host socket/conformance suite are run in
> their intended environments. No implementation defect remains open.

## Environment and isolation

- Worktree: `/home/tjmisko/Projects/switchboard/.worktrees/codex-status/v7-validation`
- Branch: `codex-status-v7-validation`
- Validated integration checkout:
  `/home/tjmisko/Projects/switchboard/.worktrees/codex-status-integration`
- Exact final integration commit:
  `080c7b50c4717af17140db2d9456b1595df00871`
- Runtime/test parent (code-identical to final):
  `328b6c8cdeb7bbd6a2e5337fa68a37f3c2a83ad6`
- Initial W2 commit used for the pre-fix pass:
  `f83117d9f7b7b6b1c46bd4f4ed0988c4d37054a4`
- Host: Linux `7.1.6-400.asahi.fc44.aarch64+16k`, `arm64`,
  `CGO_ENABLED=1`
- Go: `go version go1.26.5-X:nodwarf5 linux/arm64`
- Primary cache: `GOCACHE=/tmp/switchboard-codex-v7-gocache`
- Primary temporary roots: `GOTMPDIR=/tmp/sbv7` for the initial pass and
  `GOTMPDIR=/tmp/sbv7f` for the final pass. These short paths prevent an
  unrelated Unix socket path-length `EINVAL` from obscuring the sandbox's
  actual socket denial.
- Baseline-reproduction cache/root:
  `/tmp/switchboard-codex-v7-baseline-gocache` and `/tmp/sbv7base`

The first final-candidate compile attempt hit the shared host's temporary-file
quota before any test executed (`link: mapping output file failed: disk quota
exceeded`). V7 cleared only its own obsolete, reproducible Go caches and reran
the commands successfully with the established cache. This was not classified
as a code result.

No automated command connected to the installed Switchboard daemon, a Codex
control socket, or a live app-server. No daemon/app-server, terminal session,
Waybar process, or user hook/configuration was started, stopped, restarted, or
modified. Test transports were in-memory or under the V7 temporary root.

## Stage 1: package and contract suites

The required narrow commands were run in order:

```text
go test ./internal/discovery
go test -race ./internal/agentgraph ./internal/provider/...
go test -race ./internal/state ./internal/history
go test -race ./internal/fanout
go test ./cmd/claude-tui ./cmd/switchboard-waybar
go test ./cmd/switchboard-ctl
go test -race ./cmd/switchboard
```

Results:

| Command | Result |
|---|---|
| discovery | PASS |
| agent graph and all provider packages, race-enabled | PASS |
| state and history, race-enabled | PASS |
| Claude fanout, race-enabled | PASS |
| TUI and Waybar | PASS |
| `switchboard-ctl` | INFRASTRUCTURE FAIL: four Unix-listener hook tests received `setsockopt: operation not permitted` |
| daemon, race-enabled | INFRASTRUCTURE FAIL: one Unix-socket test could not connect; the initial long temp root also produced `EINVAL` before rerun with `/tmp/sbv7` |

The socket-dependent failures are not classified as implementation defects.
The same tests fail for the same reason on the pre-implementation baseline; see
"Baseline reproduction" below.

## Stage 2: full suite and CI checks

```text
go test ./...
go test -race ./...
go vet ./...
go build ./...
```

| Command | Exit | Result |
|---|---:|---|
| `go test ./...` | 1 | Known infrastructure failures only |
| `go test -race ./...` | 1 | Same known infrastructure failures; no race report |
| `go vet ./...` | 0 | PASS |
| `go build ./...` | 0 | PASS; Go emitted a non-fatal stat-cache warning because the shared module cache is read-only |

The normal and race full-suite failures were identical:

- `cmd/switchboard`: Unix socket connect denied in
  `TestAnchorClampFreshnessDependsOnWallClockWriters`.
- `cmd/switchboard-ctl`: Unix listener denied in the four
  `TestCmdHookShould...` tests.
- `internal/rpc`: the subscribe server never began listening because Unix
  sockets are denied.
- `internal/conformance`: the sandbox does not provide the expected masked
  `/proc` executable view.
- `internal/projectname`: the sandbox's synthetic `/tmp/.git` mount changes the
  expected no-repository temp-root result.

### Baseline reproduction

The five failing areas were rerun at the original checkout's pre-implementation
commit `426edcee2d2a75a998a4b9bd0e8d454c44f52c27`, with the dedicated baseline
cache and temporary root. Every failure reproduced:

```text
go test ./cmd/switchboard -run TestAnchorClampFreshnessDependsOnWallClockWriters -count=1
go test ./cmd/switchboard-ctl -run TestCmdHookShould -count=1
go test ./internal/rpc -run TestSubscribe_deliversTheSameWireFrameToEverySubscriber -count=1
go test ./internal/conformance -run TestOsprocSourceConformance -count=1
go test ./internal/projectname -run TestProjectRoot_noGitReturnsDirItself -count=1
```

The first three failed on Unix-socket denial, the fourth on the same unmasked
`/proc` executable, and the fifth on the same `/tmp` project-root result. These
must be rerun in the project's normal host/CI environment before release, but
they do not distinguish this change from baseline.

## Stage 3: deterministic replay

The schema-derived E0 trace and related ordering/retention cases were replayed
50 times under race detection:

```text
go test -race -count=50 ./internal/provider/codex \
  -run 'Test(SchemaFixturesMapNestedRuntimeLifecycleAndAttention|ThreadListFixtureUsesExplicitParentage|OrderingNestedParentageUnknownEnumsAndTurnPruning|StatusBeforeMetadataIsGenerationScopedAndAppliedAfterParentage|EveryCodexEnumMapsToNeutralAxisAndTerminalCapIsBounded)$'
```

Result: PASS. This covers idle root, two child spawns, delegating reduction,
simultaneous approval and user-input waits, completion/shutdown drain, explicit
parentage, status-before-metadata ordering, unknown future enums, and bounded
terminal cohorts.

The neutral graph contract ran 50 race-enabled repetitions and passed. The
daemon coordinator observation cases ran 50 race-enabled repetitions and
passed. State graph/broadcast and canonical `agent_state` history cases each ran
20 race-enabled repetitions and passed. TUI, Waybar, and CLI projections ran 20
repetitions and passed.

The initial combined Codex replay/reconnect stress command found
`V7-C3-001`; the fixture-only rerun above stayed green. C3 established that the
failure was stale fake-server state/test scheduling rather than a production
lost update, corrected the fake and semantic wait, and added explicit
notification-ordering coverage.

The exact formerly failing combined command then passed 20/20 under race
detection at the final candidate. The new fake-app-server-to-daemon integration
test passed 20/20 under race detection. It exercises the public Connector seam
through JSONL initialize/read/list, exact hook binding with unavailable process
environment, provider observation, coordinator authority, state projection,
permission-to-delegating transition, freshness expiry to unknown,
reconnect/resnapshot to idle, complete descendant omission, and canonical
approval/clear/not-found history edges. It neither invokes an installed Codex
binary nor touches a live service.

## Required acceptance matrix

| Scenario | Expected root/detail/navigation | Deterministic evidence | Result |
|---|---|---|---|
| Root active, no children | working; no child; root only | neutral reducer truth table | PASS |
| Root idle, one active child | delegating; child active; root only | reducer truth table and E0 two-child trace | PASS |
| Child waiting approval | permission; approval retained; root only | E0 approval fixture | PASS |
| Child waiting for user input | permission; user input retained; root only | E0 user-input fixture | PASS |
| Main waiting, child active | permission; both nodes retained; root only | reducer wait-priority cases | PASS |
| Two children waiting differently | permission; both reasons retained; root only | E0 combined wait replay and reducer count case | PASS |
| All children complete, root idle | idle; bounded terminal rows; root only | E0 drain fixture and terminal pruning tests | PASS |
| App-server observation expired | unknown; stale detail bounded; root only | observer freshness/reconnect, daemon expiration, and JSONL integration replay | PASS |
| Unbound exact root ID | unknown; no guessed tree; root only | exact binding and unbound daemon tests | PASS |
| Sandbox wrapper process | no session | full discovery matrix (`TestIsCodex`) | PASS |
| Claude existing fanout | legacy-equivalent summary; neutral nested rows; root only | canonical shadow, provider, fanout, TUI, and Waybar tests | PASS |

Navigation remained rooted in the `state.Snapshot.Sessions` collection.
Waybar's graph test explicitly proves that child nodes do not add slots. The
fake-app-server daemon replay kept the root as the only session while two child
nodes appeared in its graph.

## Stage 4: adversarial concurrency

Focused race/stress results:

```text
go test -race -count=20 ./internal/provider/codex -run 'Test(ObserverReconnectFreshnessGenerationAndCoalescing|SchemaFixturesMapNestedRuntimeLifecycleAndAttention|ThreadListFixtureUsesExplicitParentage|OrderingNestedParentageUnknownEnumsAndTurnPruning|StatusBeforeMetadataIsGenerationScopedAndAppliedAfterParentage)$'
go test -race -count=20 ./internal/provider/codex -run 'Test(NotificationSignalExposesMutation|ObserverDisconnectDuringDescendantListKeepsLastCompleteSnapshot|ObserverCloseInterruptsReconnectBackoff)$'
go test -race -count=20 ./cmd/switchboard -run '^TestCodexAppServerObserverThroughCoordinatorStateAndHistory$'
go test -race -count=100 ./cmd/switchboard -run 'Test(ProviderUpdateRacingRootRemovalCannotLandStateOrHistory|BlockedProviderLeavesRPCSubscriptionAndGraphHookResponsive|CoordinatorCloseCancelsBlockedObserveAndJoinsRunLoop)$'
```

| Exercise | Command summary | Result |
|---|---|---|
| Codex reconnect/freshness/generation/coalescing, final candidate | formerly failing combined race loop x20 | PASS 20/20 in 12.167s |
| Notification mutation-before-signal, disconnect during descendant list, close during reconnect backoff | race loop x20 | PASS 20/20 in 1.082s |
| Fake app-server through daemon state/history | race loop x20 | PASS 20/20 in 6.470s |
| Process removal vs provider update, blocked provider vs RPC/subscription, close/join blocked observation | race loop x100 | PASS 100/100 in 1.622s |
| Claude Apply/Observe/Forget/Close and ordering | race loop x100 | PASS in 12.075s |
| Daemon provider I/O, lifecycle, periodic work, terminal enumeration | race loop x100 | PASS in 130.607s |
| State broadcast/coalescing/subscriber behavior | race loop x100 | PASS in 1.558s |

`TestAgentObservationDoesNotHoldStoreLockAndFencesLateGeneration` blocks the
provider fake and requires `Store.Snapshot` to complete within 100ms. It passed
in the 50- and 100-iteration daemon runs. The new in-memory RPC test keeps the
provider blocked while an ordinary graph-aware hook RPC and a live subscription
both complete within one-second connection deadlines, then proves the blocked
observer had not returned early. This establishes the tested store/RPC lock
budget without a filesystem socket.

The final adversarial additions also prove that a root removal racing a provider
update cannot resurrect state or write late canonical history; a descendant
query disconnect cannot install a partial snapshot over the last complete one;
`Close` interrupts a one-hour reconnect backoff within 250ms; and coordinator
shutdown cancels and joins a blocked Observe before returning. PID reuse is
covered by PID-plus-start binding tests, and the RPC connection tests assert
their goroutines close. No race detector report or bounded-shutdown failure was
observed.

## Stage 5: compatibility and rollout controls

All focused compatibility commands passed again at the final candidate:

```text
go test -race -count=10 ./internal/provider/claude ./internal/fanout
go test -race -count=20 ./internal/state -run 'Test(LoadHydratesFromGolden|LoadHydratesPartialAgentGraphAxesAndOrder|LoadExpiresRestoredAgentGraphWithoutDroppingStructure|LoadDropsStructurallyInvalidAgentGraphSafely|StateGoldenRoundTrips|AgentGraphGolden)$'
go test -race -count=10 ./internal/history -run 'Test(AgentState|ReadDayToleratesTornLine|ReadDaySkipsForeignJSONLine|BuildSwimlanes|Summarize|SinkWritesEventsPartitionedByDay)'
go test -count=20 ./cmd/claude-tui ./cmd/switchboard-waybar -run 'Test(RenderSnapshot|AgentTooltip|SessionTooltip|RenderSlot)'
go test -count=20 ./cmd/switchboard-ctl -run 'Test(FormatAgentState|FormatHistory|HistoryTail|BuildAgentTimeline|TimelineJSONMixedHistory|RunDiagnose|BuildObserverDiagnostics|RenderObserverDiagnostics|DiagnoseObserver|ObserverRollout)'
go test -race -count=20 ./cmd/switchboard ./internal/provider/codex -run 'Test(CodexObserverModeValidationAndConstruction|CodexObserverOffKeepsHookFallbackAndClaudeGraph|FreshCodexAppServerGraphOutranksHookFallback|UnboundCodexRootRemainsVisibleAndUnknown|ProxyVersionCapabilityChecks|BindingIgnoresSessionIDAndCWDAndFallsBackToHook|BindingPrecedenceAndPIDStartIdentity)$'
go test -count=20 ./internal/discovery
```

- Claude provider and fanout full suites, race-enabled, 10 repetitions.
- Old state golden, partial/new graph state, expired graph, invalid optional
  graph, and state wire goldens, race-enabled, 20 repetitions.
- Old history readers/swimlanes plus canonical `agent_state` history,
  race-enabled, 10 repetitions.
- TUI and Waybar graph/legacy renderers, 20 repetitions. Children did not add
  bar slots; stale/error/wait details remained distinct.
- CLI history, timeline, and observer diagnostics, 20 repetitions. Raw history
  retains canonical and compatibility events while the human view suppresses
  only matching duplicate projections.
- Claude shadow/fanout equivalence and simultaneous Claude/Codex coordinator
  behavior.
- Codex binding keyed by PID plus process start, including PID reuse, process
  environment denial fallback, no CWD binding, and no use of
  `CODEX_SESSION_ID` as a thread ID.
- Discovery's complete Codex invocation matrix, including sandbox wrappers,
  `exec`, app-server forms, help/version, unknown verbs/options, and masked
  process data.

Rollout verification:

- Default mode is `auto`; accepted modes are exactly `auto` and `off`.
- `off` does not construct the app-server observer and retains Codex hook
  fallback, root discovery/navigation, and Claude graphs.
- `auto` construction is non-blocking with respect to later proxy retry; proxy
  version parsing accepts `>=0.149.0` and rejects older/unparseable versions.
- Environment read denial falls back only to an exact hook binding; an unbound
  root remains visible and unknown rather than using CWD.
- `go run ./cmd/switchboard -h` exited 0 and advertised
  `-codex-observer auto|off` with default `auto`.
- `go run ./cmd/switchboard -codex-observer=invalid` exited 1 before daemon
  startup with the expected validation error.

## Fixture and privacy disposition

All JSON and every non-empty JSONL line under
`internal/provider/codex/testdata/captures` parsed successfully. Identifiers,
timestamps, paths, roles, and nicknames are synthetic. The only path is
`/synthetic-workspace`; preview strings are empty; prompt, reasoning, command,
approval, and project payload fields are null where present. No username,
hostname, real home path, socket endpoint, authorization token, API/account ID,
prompt text, reasoning text, command payload, or assistant output was found.

## Controlled live acceptance

**NOT RUN — pending explicit user/coordinator approval.** This stage consumes
model usage and observes a live Codex session, so V7 did not access or restart
any live service, change hooks/configuration, create a disposable Codex root, or
capture a real identity. The blocker is approval, not a technical failure.

## Defects and remaining gaps

### V7-C3-001

```text
ID: V7-C3-001
Owner: C3 (Codex protocol/binding/cache)
Baseline/commit: observed at f83117d9f7b7b6b1c46bd4f4ed0988c4d37054a4; resolved and rerun at code parent 328b6c8cdeb7bbd6a2e5337fa68a37f3c2a83ad6; final candidate 080c7b50c4717af17140db2d9456b1595df00871
Command or replay step: race-enabled combined Codex replay/reconnect loop, count=20
Expected: after the fake status notification and delivered invalidation, Observe returns child attention=approval
Actual: one iteration delivered an invalidation while Observe still returned attention=none (observer_test.go:79)
Fixture/log excerpt (content-free): node=child; got attention=none; want approval
Reproducibility: 1/20 in the original combined loop; isolated pre-fix rerun passed 30/30; corrected final combined rerun passed 20/20
```

The coordinator accepted and routed this to C3. C3 showed that an older
coalesced invalidation plus a fake snapshot that was not updated with its own
notification made the test assert event-acknowledgement semantics that the
provider contract does not promise. The fake now keeps snapshot truth coherent;
the test waits for the requested semantic observation; and a separate test
proves a notification mutation is visible when its signal is consumed. The
final stress and new adversarial suites pass. **Status: RESOLVED; not a
production implementation defect.**

No implementation defect remains open. The two external gates are still open:
the normal-host rerun of the sandbox-denied test areas and controlled live
acceptance after explicit approval.

## Documentation comparison and recommendation

The final public/design docs describe the merged behavior and operational
boundaries:

- the README documents one navigable root, nested non-focusable children,
  app-server primary truth, exact binding, `auto|off`, freshness degradation,
  locally verified proxy/version scope, hooks, and content-free diagnostics;
- `state-schema.md` describes every additive graph/summary/node field, enum,
  ordering rule, authority boundary, and legacy projection;
- `history-schema.md` describes canonical `agent_state`, privacy, dedupe,
  compatibility coexistence, and the additive child timeline;
- the investigation and status-color design record agree with the implemented
  primary/fallback sources and neutral reducer.

All seven strict JSON blocks in `state-schema.md` and all three strict JSON
blocks in the README parse with `jq`. The documented minimal `agent_state` line
is byte-for-byte identical to
`internal/history/testdata/agent-state-minimal.golden.jsonl`.
`TestAgentGraphGolden`, both canonical state-golden tests, and
`TestAgentStateMinimalJSONGolden` pass at the final hash. The final-head link
scan found zero broken local links across README and `docs/`; Markdown fence
counts are balanced; and `git diff --check` is clean from the planning baseline
through `080c7b50`.

### Recommendation: ship disabled

The deterministic candidate is suitable for a disabled rollout: `off` preserves
root discovery/navigation, Claude behavior, and Codex hook fallback, while all
new wire fields remain additive. Do not make `auto` the production rollout
setting until both remaining external gates are green:

1. rerun the socket, `/proc` mask, and temp-project-root failures in the normal
   host/CI environment where they do not inherit this sandbox's restrictions;
2. obtain explicit approval and complete Stage 6 against one disposable live
   Codex root with two harmless children.

After those pass without a critical mismatch, V7's deterministic evidence
supports promoting the recommendation to **ship auto** without another code
change.

## Original checkout preservation

The original checkout still reports exactly the four pre-existing untracked
entries: `Force Manipulation Ramp.html`, `docs/codex-session-status/`,
`docs/rust-rewrite-kickoff.md`, and
`docs/rust-rewrite-requirements-v0.2.md`. SHA-256 fingerprints for all three
standalone files and all nine original planning files match the fingerprints
recorded before validation. V7 did not modify, stage, or commit any of them.
