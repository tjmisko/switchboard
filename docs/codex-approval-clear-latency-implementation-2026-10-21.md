# Codex approval-clear latency — implementation addendum

Companion to `docs/codex-approval-clear-latency-2026-10-21.md`. That document
established the failure mechanism; this one grounds it in the code at both
ends and answers two questions:

1. what can be done with **no Codex-side change**, and
2. what a **minimal cross-repo change set** looks like.

Switchboard revision inspected: `2faccc1` (HEAD at time of writing).
Codex source: `/home/tjmisko/Programs/codex` @ `5c013177d8`.

No runtime behavior was changed. All snippets are quoted from the working
trees unless marked as sketches.

## 0. Three code facts the original proposal under-states

These do not change the diagnosis; they narrow the fix and make it cheaper.

### 0.1 Switchboard already knows how to retire an app-server request

The app-server observer already tracks pending requests by exact JSON-RPC
request id and already consumes the resolution notification. From
`internal/provider/codex/observer.go`:

```go
case "serverRequest/resolved":
    requestID, valid := parseRequestID(params.RequestID)
    if request, resolved := state.resolveRequest(params.ThreadID, requestID); valid && resolved {
        classificationSource = "request_resolved"
        diagnostics = append(diagnostics, requestDiagnostic("resolved", request, classificationSource, eventAt))
        touches = true
    }
```

and `internal/provider/codex/protocol.go`:

```go
func (s *graphState) resolveRequest(id string, requestID rpcRequestID) (requestEvidence, bool) {
    state := s.nodes[id]
    if state == nil || requestID == "" {
        return requestEvidence{}, false
    }
    request, ok := state.wait.requests[requestID]
    if !ok {
        return requestEvidence{}, false
    }
    delete(state.wait.requests, requestID)
    if state.wait.rawApproval || state.wait.rawUserInput {
        state.wait.classifiedUnknown = true
    }
    s.deriveAll()
    return request, true
}
```

`applyStatus` also clears the raw wait flags when `thread/status` reports no
`waitingOnApproval` / `waitingOnUserInput`:

```go
func (s *graphState) applyStatus(state *nodeState, status rpcStatus) {
    ...
    state.wait.rawApproval = false
    state.wait.rawUserInput = false
    for _, flag := range status.ActiveFlags {
        switch flag {
        case "waitingOnApproval":
            state.wait.rawApproval = true
        case "waitingOnUserInput":
            state.wait.rawUserInput = true
        ...
```

So the daemon is not missing the *consumer*. It is missing the *producer*:
those notifications arrive on the owning app-server connection, and
Switchboard's observer is a disposable second server.

### 0.2 Switchboard already has a `PermissionResolved` hook shape

`PermissionResolved` is not a new invention. It is how the **Pi** integration
already reports a dialog closing, and the RPC surface already supports it.
From `internal/rpc/rpc.go`:

```go
// OpenDialogs is the number of dialogs open now (Pi's ui_prompt span plus
// herdr's blocked counter) on PermissionRequest/PermissionResolved. It is
// a count rather than an edge so a lost or reordered hook self-corrects;
// nil means the hook did not say.
OpenDialogs *int `json:"open_dialogs,omitempty"`
```

`cmd/switchboard/pi_hooks.go`:

```go
case "PermissionRequest", "PermissionResolved":
    switch {
    case req.OpenDialogs != nil:
        root.openDialogs = max(*req.OpenDialogs, 0)
    case req.Event == "PermissionRequest":
        root.openDialogs = max(root.openDialogs, 1)
    default:
        root.openDialogs = max(root.openDialogs-1, 0)
    }
    if root.openDialogs > 0 {
        return statustune.RulePiDialogOpen
    }
    return statustune.RulePiDialogClosed
```

and `cmd/switchboard-ctl/main.go` already parses the field:

```go
case "PermissionRequest", "PermissionResolved":
    if payload.OpenDialogs != nil {
        count := max(*payload.OpenDialogs, 0)
        req.OpenDialogs = &count
    }
```

The Codex reducer simply does not have a `case "PermissionResolved"`. That is
the seam to reuse. There is precedent, a payload field, a ctl parse path, and a
test vocabulary (`pi_hooks_test.go`).

### 0.3 Codex's event registry confirms the gap

`codex-rs/protocol/src/protocol.rs`:

```rust
pub enum HookEventName {
    PreToolUse,
    PermissionRequest,
    PostToolUse,
    PreCompact,
    PostCompact,
    SessionStart,
    SessionEnd,
    UserPromptSubmit,
    SubagentStart,
    SubagentStop,
    Stop,
    Interrupt,
}
```

Twelve events. No `PermissionResolved`. The closest things are `PostToolUse`
(after the tool returns) and `Stop` (turn boundary), which is exactly the
unbounded tool-duration dependency the original doc describes.

## 1. Where the accepting side actually is

Two acceptance points matter, and they are at different layers.

**App-server layer.** `notify_client_response` takes the connection-specific
callback and has both the request id and `thread_id` in hand
(`codex-rs/app-server/src/outgoing_message.rs`):

```rust
pub(crate) async fn notify_client_response(
    &self,
    connection_id: ConnectionId,
    id: RequestId,
    result: Result,
) {
    let entry = self.take_connection_callback(connection_id, &id).await;
    match entry {
        Some((id, entry)) => {
            ...
            if entry.callback.send(Ok(result)).is_err() {
                warn!("could not notify callback for {id:?}: receiver dropped");
            }
        }
        None => { warn!("could not find callback for {id:?}"); }
    }
}
```

`PendingCallbackEntry` carries `thread_id: Option<ThreadId>` and the original
`request: ServerRequest`. This is the earliest point and the only place that
sees the raw accepted response for any surface (TUI, desktop, remote).

The typed handlers then run, and each one calls the existing resolution
notification **before** submitting the decision to core. From
`bespoke_event_handling.rs` (`on_command_execution_request_approval_response`):

```rust
let response = receiver.await;
resolve_server_request_on_thread_listener(&thread_state, pending_request_id).await;
drop(permission_guard);
let (decision, completion_status) = match response { /* Accept / Decline / Cancel ... */ };
...
if let Err(err) = conversation
    .submit(Op::ExecApproval { id: approval_id.unwrap_or_else(|| item_id.clone()), turn_id: Some(event_turn_id), decision })
    .await
```

and `resolve_pending_server_request` fans out to subscribed connections only
(`request_processors/thread_lifecycle.rs` + `thread_state.rs`):

```rust
let subscribed_connection_ids = thread_state_manager
    .subscribed_connection_ids(conversation_id)
    .await;
...
outgoing.send_server_notification(ServerNotification::ServerRequestResolved(
    ServerRequestResolvedNotification { thread_id, request_id },
))
```

`ServerRequestResolvedNotification` is `{ thread_id, request_id }`
(`app-server-protocol/src/protocol/v2/notification.rs`). It has no outcome and
no correlation id.

**Core layer.** All four decision kinds converge on one match arm in
`codex-rs/core/src/session/handlers.rs`:

```rust
Op::ExecApproval { id: approval_id, turn_id, decision } => {
    exec_approval(&sess, approval_id, turn_id, decision).await;
    false
}
Op::PatchApproval { id, decision } => {
    patch_approval(&sess, id, decision).await;
    false
}
Op::UserInputAnswer { id, response } => {
    request_user_input_response(&sess, id, response).await;
    false
}
Op::RequestPermissionsResponse { id, response } => {
    request_permissions_response(&sess, id, response).await;
    false
}
```

This is the single choke point where an accepted human answer is handed to the
waiting tool. It is slightly later than the app-server boundary but still
strictly before the approved command begins to run, so it is well inside any
3 s budget.

## 2. What can be done with no Codex-side change

Short answer: **not the 3 s guarantee, for the default interactive client.**
Three options exist; each is either heuristic or conditional.

### 2.1 Option A — terminal-modal observation (heuristic, local only)

Switchboard already owns the terminal seam (`internal/terminal/terminal.go`,
`integrations/wezterm/`), but the current WezTerm integration is title/focus
only. It calls exactly two fixed vectors:

```text
switchboard-ctl pane-bind  <v1-json> <gui-pid> <window-id> <pane-id>
switchboard-ctl pane-state <gui-pid> <window-id> <active-pane-id> <window-focused>
```

It never reads screen text and never sees input. A fallback would have to add
both:

- a screen probe (`pane:get_lines_as_text()` from the single-outstanding
  `update-status` event the module already relies on) that recognizes the
  approval footer,
- a commit edge: modal-present → modal-absent, delivered as a new ctl command
  such as `switchboard-ctl codex-approval-resolved <pane-id>`.

Switchboard would translate that into the Pi-shaped `PermissionResolved` with
`open_dialogs: 0` (outcome unknown) and reuse the retirement path in §3.

Limits, in order of severity:

- It sees only a local WezTerm pane. Desktop and remote answers produce nothing.
- It cannot distinguish accept from decline. Treating both as green is
  defensible (decline hands control back too), but the doc's "cancel follows
  turn state" distinction is lost.
- Modal-render detection is fragile across themes, resize, scroll, obscuring
  and terminal loss.
- It still must clear the resolver's old claim, which is a policy change, not a
  new candidate (see §4).

This is a useful last resort. It is not a latency guarantee.

### 2.2 Option B — subscribe to the owning daemon (conditional, not safe yet)

Codex exposes a control socket at `$CODEX_HOME/app-server-control.sock`
(`app-server-transport/src/transport/mod.rs`, constant
`app-server-control.sock`). The TUI only uses it in `AppServerTarget::LocalDaemon`
mode; the default is `AppServerTarget::Embedded`, an in-process server with no
external transport (see `codex-rs/tui/src/app_server_connection.rs`).

If the user ran the shared daemon, a second client could `initialize` then
`thread/resume` to become a subscribed connection and would then receive the
existing `serverRequest/resolved` and `thread/status/changed` notifications with
no Codex change. But `thread/resume` is not a read: `try_ensure_connection_subscribed`
registers the connection and `ensure_conversation_listener` attaches a listener.
Worse, for ordinary approvals `send_request_to_connections` broadcasts the
request to every subscribed connection and `take_connection_callback` accepts
the first response from any of them, so a passive observer is also a second
potential answerer. That is a correctness hazard, not a read-only tap.

Verdict: viable only with an explicit read-only subscription in Codex
(which is a Codex change), so it does not belong in the no-change column.

### 2.3 Option C — things that do not work

- **Faster polling.** The missing event is absent from the source; the original
  doc already establishes this. Active resnapshot (1 s) and idle resnapshot
  (10 s) bound recovery, not the decision edge.
- **Shrinking `codexHookApprovalGrace` (30 s).** Confirmed separate in
  `startCodexApprovalTimerLocked`: the grace classifies an ambiguous boundary as
  human-owned. It does not gate the release.
- **`UserPromptSubmit`.** Approval selections are structured protocol replies,
  not prompts. The reducer already says so in the `SessionStart, UserPromptSubmit`
  approval-sweep comment.
- **`item/started` alone.** The comment in `codexHookObservation` notes some
  items start before their approval request, so it cannot prove post-approval
  execution.
- **`thread/read` from the disposable observer.** It does return
  `ThreadStatus::Active { active_flags }` and even a `status` on
  `thread/read` (`protocol.go` parses `ActiveFlags`), but it is the disposable
  server's own view. It will report untouched/idle, not the owning server's
  pending flags.

**Conclusion for §2.** Without Codex emitting a resolution at the accepting
boundary, the bar's release remains bounded by tool duration or a later turn
boundary. The no-change options are heuristics that cannot cover the accepted
requirement.

## 3. Minimal change set with Codex-side integration

The change is one signal plus one retirement. Neither side needs a new
architecture.

### 3.1 Codex: emit `PermissionResolved`

Reuse the existing hook transport so the embedded TUI is covered without any
socket work. The smallest correct insertion is the core `Op::*` match in
`session/handlers.rs`: every human decision already funnels through it, and it
runs before the tool proceeds.

Sketch (core, `session/handlers.rs`):

```rust
Op::ExecApproval { id: approval_id, turn_id, decision } => {
    let decision = decision.clone();
    exec_approval(&sess, approval_id, turn_id, decision).await;
    // proposed: fire the resolution hook here, before the tool runs
    false
}
Op::PatchApproval { id, decision } => {
    patch_approval(&sess, id, decision).await;
    false
}
Op::UserInputAnswer { id, response } => {
    request_user_input_response(&sess, id, response).await;
    false
}
Op::RequestPermissionsResponse { id, response } => {
    request_permissions_response(&sess, id, response).await;
    false
}
```

The hook needs the three facts the original doc lists: exact request/tool
identity, a normalized outcome, and a monotonic ordering token. Concretely:

- add `PermissionResolved` to `HookEventName` in
  `codex-rs/protocol/src/protocol.rs`,
- add request/outcome types in the `codex_hooks` crate (mirroring
  `PermissionRequestRequest` / `PermissionRequestOutcome`),
- add `preview_permission_resolved` / `run_permission_resolved` and wire the
  event into `hook_runtime.rs`, next to `run_permission_request_hooks`,
- normalize the decision in one place: command `Accept`/`AcceptForSession`/
  exec-policy amendment = accepted; `ApplyNetworkPolicyAmendment` follows its
  action; `Decline` = declined; `Cancel` = cancelled; user-input = answered;
  elicitation follows its action; permissions = granted,
- carry `session_id`, `turn_id`, `tool_use_id` (or the approval/call id),
  `permission_mode`, and `subagent` so Switchboard can correlate by the same
  key the `PermissionRequest` hook used.

If a hook event is too broad a change, the smaller-but-narrower alternative is
to emit an enriched `serverRequest/resolved`:

```rust
// app-server/src/outgoing_message.rs, inside notify_client_response
if let Some(thread_id) = entry.thread_id {
    // proposed: emit ServerRequestResolved{thread_id, request_id, outcome, kind}
    // to subscribed connections now, rather than only from the typed handler.
}
```

That is a two-file change (notification struct + one call), but it only reaches
a *connected* observer. Since the default TUI server is embedded, it does not
cover the primary case; it is listed for completeness, not recommended as the
only fix.

### 3.2 Switchboard: retire the exact request, then publish presumptive green

The reducer already has the cancellation point. Add `PermissionResolved` as a
first-class releasing edge in `cmd/switchboard/codex_hook_transitions.go`.

`reduceCodexPendingApprovalLocked` currently releases on the tool's own
progress, `Stop`, and conversation boundaries:

```go
resolved := make([]string, 0, 1)
switch req.Event {
case "PreToolUse", "PostToolUse":
    for key, pending := range state.approvals {
        if codexPendingApprovalMatches(pending, req) {
            resolved = append(resolved, key)
        }
    }
case "Stop":
    ...
case "SessionStart", "UserPromptSubmit":
    for key := range state.approvals {
        resolved = append(resolved, key)
    }
}
```

Add the decision edge beside `PreToolUse`/`PostToolUse`:

```go
case "PermissionResolved":
    for key, pending := range state.approvals {
        if codexPendingResolutionMatches(pending, req) {
            resolved = append(resolved, key)
        }
    }
```

where the match prefers the exact call id and only falls back to the existing
composite (`writer`, `turnID`, `toolName`, `inputHash`). Do not widen it to the
whole conversation: a sibling agent's answer must not clear this root's
unrelated request. That is the concurrent-questions acceptance case.

A resolution tombstone is required so a delayed `PermissionRequest`/snapshot
cannot re-open the episode:

```go
// codexHookRootState
retired map[string]struct{} // keyed by the same key codexPendingApprovalKey uses
```

- set the tombstone in the `PermissionResolved` branch,
- have `reduceCodexPendingApprovalLocked` skip a `PermissionRequest` whose key
  is tombstoned,
- clear the tombstone only on conversation rotation or a genuinely new request
  generation (the `episode` counter already provides the generation).

Then make the overlay honor the tombstone. Today
`overlayCodexApprovalObservation` re-asserts any published red:

```go
func (c *agentCoordinator) overlayCodexApprovalObservation(key provider.RootKey, observation agentgraph.Observation, now time.Time) agentgraph.Observation {
    if codexObservationRootAttention(observation) != agentgraph.AttentionNone {
        return observation
    }
    c.codexHookMu.Lock()
    defer c.codexHookMu.Unlock()
    state := c.codexHookRoots[key]
    if state == nil || state.sessionID != observation.RootID || !codexApprovalRedPublishedLocked(state) {
        return observation
    }
    return applyCodexPendingAttention(observation, agentgraph.AttentionApproval, true, now)
}
```

After a resolution there is no `state.approvals` entry, so this already stops
re-asserting — provided the resolution truly deleted the record. The tombstone
handles the race where an older queued snapshot or hook lands afterward. No
separate overlay change is needed if deletion and tombstone are both done under
`codexHookMu`.

Finally, publish immediately and invalidate, mirroring the existing hook
landing:

```go
if mapped {
    c.rememberCodexHookRootObservation(ref.Key(), hookFallback)
    generation := c.begin(ref.Key())
    c.applyObservationAs(ref, generation, observation, claudeprovider.Compatibility{}, now, "", state.GraphHookEvent)
}
...
if c.codex != nil {
    c.Request(ref.Key())
}
```

`codexHookObservation` must map the new event to active/none:

```go
case "UserPromptSubmit", "PreToolUse", "PostToolUse", "PermissionResolved":
    runtime = agentgraph.RuntimeActive
```

so the release lands as a same-writer exact event observed after the claim.

### 3.3 Resolver: let a fresh answer outrank a stale terminal `blocked`

This is the one policy change the original doc calls out and it is easy to
miss. The terminal reading outranks hooks by rank
(`internal/statusresolve/resolve.go`):

```go
const (
    rankNone = iota
    rankRestored
    rankTranscript
    rankEvent        // exact hook edge
    rankSnapshot
    rankTerminal     // herdr
    rankCompleteEvent
)
```

and `beats` only orders by event time *within* the provider ranks:

```go
func providerRank(rank int) bool { return rank >= rankTranscript && rank <= rankSnapshot }

func beats(a, b Candidate) (wins, byRank bool) {
    ra, rb := Rank(a), Rank(b)
    if providerRank(ra) && providerRank(rb) && a.EventTimeOrder && b.EventTimeOrder &&
        !a.ObservedAt.Equal(b.ObservedAt) {
        return a.ObservedAt.After(b.ObservedAt), false
    }
    if ra != rb {
        return ra > rb, true
    }
    return a.ObservedAt.After(b.ObservedAt), false
}
```

So a `PermissionResolved` hook can resolve the open claim (a snapshot or the
claim's own writer may resolve it), but a lagging herdr `blocked` reading will
still win the base by rank and keep the chip red. That is the "stale terminal
blocked state" acceptance case.

Minimal, targeted rule: a live terminal reading dated before a same-root
resolution event must not outrank it. Add a `terminal blocked` freshness test to
`beats` rather than reordering ranks:

```go
// Sketch: a terminal blocked reading older than a fresh provider resolution
// loses to that resolution, so a modal that has just closed cannot repaint red.
if Rank(a) == rankEvent && Rank(b) == rankTerminal &&
    b.Status == agentgraph.LegacyPermission && a.ObservedAt.After(b.ObservedAt) {
    return true, false
}
```

This is deliberately narrow: it only demotes a *blocked* terminal reading that
predates the answer, and only against a provider event. It does not let hooks
clear arbitrary terminal working/idle readings, and it does not change the
terminal's role in establishing the base.

## 4. File-by-file minimal change set

Codex (`~/Programs/codex`):

| File | Change |
|---|---|
| `codex-rs/protocol/src/protocol.rs` | add `HookEventName::PermissionResolved` |
| `codex-rs/hooks/...` | request/outcome types + matcher wiring for the new event |
| `codex-rs/core/src/hook_runtime.rs` | `run_permission_resolved_hooks`, alongside `run_permission_request_hooks` |
| `codex-rs/core/src/session/handlers.rs` | fire it from the `Op::ExecApproval` / `PatchApproval` / `UserInputAnswer` / `RequestPermissionsResponse` / `ResolveElicitation` arms |
| (alternative) `codex-rs/app-server/src/outgoing_message.rs` + protocol notification | enrich and early-emit `ServerRequestResolved`; covers connected observers only |

Switchboard:

| File | Change |
|---|---|
| `cmd/switchboard/codex_hook_transitions.go` | release case for `PermissionResolved`; tombstone in `codexHookRootState`; map event in `codexHookObservation`; skip tombstoned `PermissionRequest` |
| `cmd/switchboard/codex_hook_transition_test.go` | accept/decline/cancel, concurrent requests, late onset after resolution, request-id reuse, stale snapshot replay |
| `internal/statusresolve/resolve.go` | narrow rule letting a fresh provider resolution outrank a stale terminal `blocked` |
| `internal/statusresolve/resolve_test.go` | terminal-blocked-after-answer case |
| `cmd/switchboard-ctl/main.go` | already parses `PermissionResolved`; no change if the Codex hook reuses `open_dialogs`, otherwise add the exact-correlation field |
| `integrations/wezterm/switchboard.lua` + README | only for the §2.1 fallback; not part of the minimal set |

## 5. The decisive tests

Add to the existing baseline
(`go test ./internal/provider/codex ./internal/statusresolve ./cmd/switchboard`):

1. **Long command.** Acceptance event fires, latch retired, status goes green
   while the command is still running. Assert wall-clock answer-to-`applyObservation`
   under 250 ms in the unit harness; the 3 s ceiling is the integration bound.
2. **Decline.** Returns green; control goes back to the agent.
3. **Cancel.** Follows the resulting turn state (idle), not a blind green.
4. **Two concurrent requests.** Resolving one leaves the other red.
5. **Child/sibling request.** A resolution for one writer does not clear the
   root's unrelated request.
6. **Late onset.** A `PermissionRequest` hook queued after a resolution with the
   same call id does not re-open the latch.
7. **Stale terminal.** herdr still reporting `blocked` after a resolution does
   not repaint red.
8. **Request-id reuse / reconnect.** The tombstone is keyed by call id or
   generation, never by request id alone.

## 6. Bottom line

The original document is correct that the missing piece is a decision-time
signal. The code inspection narrows the fix:

- Switchboard already consumes `serverRequest/resolved`
  (`observer.go`, `protocol.go`) and already has a `PermissionResolved` hook
  shape (`pi_hooks.go`, `rpc.go`) — the retirement side is largely present.
- Codex has no resolution event in its 12-event registry, and the default TUI
  owns the request in an embedded server with no external transport.
- Therefore the minimal complete fix is **one new resolution hook event in
  Codex** (fired from the core `Op::*` decision arms) **plus one release case
  and tombstone in Switchboard**, with a narrow terminal-staleness rule in the
  resolver.
- Everything achievable without a Codex change is heuristic (terminal modal) or
  conditional (daemon-only subscription) and cannot carry the 3 s guarantee for
  the default interactive client.
