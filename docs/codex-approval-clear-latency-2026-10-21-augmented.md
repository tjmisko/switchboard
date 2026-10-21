# Codex approval-clear latency — augmented

Companion to `docs/codex-approval-clear-latency-2026-10-21.md`. That document's
findings stand. This revision adds three things:

1. a read-only survey of the herdr discussions and what they say about the same
   missing answer signal;
2. a correction to "the decision-time observation is missing" — for the **local
   TUI an answer-time signal already exists**, and Switchboard is currently
   discarding it;
3. two concrete change sets: one Switchboard-only (no Codex build) and one with a
   minimal Codex-side bridge.

---

## 0. What was verified this round

- Switchboard's title demotion is Claude-gated and carries the stale assumption
  that "codex paints no glyph" — `cmd/switchboard/main.go:1719` and
  `titleShowsIdleGlyph` at `:1775`.
- The approval latch re-asserts on every snapshot —
  `overlayCodexApprovalObservation`, `cmd/switchboard/codex_hook_transitions.go:1419`,
  and the timer path at `:1131` that *requires a direct app-server reading since
  the gate opened*.
- Codex's TUI emits both a title edge and a structured OSC 7501 program status —
  `chatwidget.rs:1198` / `:1213`, `terminal_program_status.rs:132`.
- Codex's app server already knows the exact response —
  `outgoing_message.rs:484` (`notify_client_response`) — and sends
  `serverRequest/resolved` at `thread_lifecycle.rs:1035`.
- Herdr's own API discussion #4462 documents the same missing answer signal.

---

## 1. Herdr discussion survey (read-only)

| # | Topic | Bearing on this |
|---|---|---|
| #5073 | Umbrella: OSC 7501 "Program Status Protocol" for Pi and dashboards | Canonical state-protocol idea; #5106 is closed as its duplicate. |
| #5106 | Claude Code OSC 7501 support | Herdr 0.9.3 does **not** answer the `OSC 7501 ; ?` capability query, so Claude never turns the protocol on; herdr falls back to screen detection (`agent-detection/claude.toml`). |
| #5107 | Native ZCode kind, title/OSC detection data offered | Confirms OSC title is the detected source for several agents. |
| #4921 | Pi extension "keep pane working after agent_settled" | Shows herdr's state model has a settled/working edge problem too. |
| #4462 | **Typed pending-request objects and request-bound responses** | The closest match to the real problem. See below. |
| #1725 | Integrations never report pane titles; task titles missing | Herdr captures OSC title (`AgentOscTracker`, `src/pane/osc.rs`) but pane title is presentation-only; the sidebar consumes `terminal_title_stripped`. |
| #2654 | Show `terminal_title` in Session Navigator | Same title source, different surface. |
| #2247 | Pane border colour driven by agent state | Confirms "blocked/working/idle" is the herdr state vocabulary. |

**#4462 is the important one.** Its own summary of the gap:

> "the only way to answer a permission prompt through the public API is generic
> input injection (`pane.send_keys`, ...). Those methods carry no request
> identity, no fencing, and no acknowledgement semantics — their success means
> 'bytes were accepted by the PTY', never 'the request was resolved'."

That is the same missing answer-time signal as in the original doc, stated from
herdr's side. So the honest answer to "the title signal is all we really need,
right?" is:

- For **presumptive green** on a single blocking request in a local TUI, the
  title/OSC signal *is* enough, and herdr already leans on it.
- For "**what did they answer**" (accept vs decline vs session-accept vs
  amendment) and for **desktop/remote/headless** answers, no. Herdr does not have
  it either; #4462 is still an open idea.

Herdr's program-status story is also incomplete for Codex: its OSC tracker
watches **OSC 0/2 titles**, and program-status support is gated behind the
`OSC 7501 ; ?` handshake (#5106). I verified in the Codex source that Codex emits
OSC 7501 **unconditionally** — `set_terminal_program_status` in
`terminal_program_status.rs:132` has no query handshake, and the writer at `:235`
emits `ESC ] 7501 ; state=<s>:app=codex[:kind=permission|question|auth] ESC \`.
So the structured signal is already on the wire; herdr simply is not reading
program-status yet. That matters for the no-Codex-change options below.

---

## 2. Correction: the answer edge is not missing — it is discarded

The original doc says the release hook is too late and the app-server observer
cannot tap the owning connection. Both true. But there is a third observation the
doc treats only as a fallback row ("Terminal modal disappears", "Terminal
input"): the **agent's own title / program-status emission at the moment the modal
closes**.

Codex emits a title edge exactly when the approval modal opens and closes:

```rust
// ~/Programs/codex/codex-rs/tui/src/chatwidget.rs:1196
pub(crate) fn pre_draw_tick(&mut self) {
    ...
    if self.terminal_title_shows_action_required() != self.last_terminal_title_requires_action {
        self.refresh_terminal_title();          // title flips to "Action Required" on open,
    }                                           // and back to spinner/idle on submit
    ...
    self.refresh_terminal_program_status();     // OSC 7501 re-emitted every tick
}
```

```rust
// ~/Programs/codex/codex-rs/tui/src/chatwidget.rs:1211
pub(crate) fn refresh_terminal_program_status(&self) {
    let status = self.desired_program_status();
    crate::terminal_program_status::set_terminal_program_status(status, ...)
}
```

The program-status writer distinguishes the blocked kind:

```rust
// ~/Programs/codex/codex-rs/tui/src/terminal_program_status.rs:248
write!(f, "\x1b]7501;state={}:app=codex", self.status.protocol_state())?;
if let ProgramStatus::Blocked(kind) = self.status {
    let kind = match kind {
        ProgramStatusKind::Permission => "permission",
        ProgramStatusKind::Question   => "question",
        ProgramStatusKind::Auth       => "auth",
    };
    write!(f, ":kind={kind}")?;
}
```

So the state `blocked:kind=permission` clears on the same draw tick that clears
the modal. This edge fires at **submit time**, before the approved command
finishes — precisely what the original doc says is unavailable from hooks and
from the observer's `thread/read` path. It is available; it is just coming out of
the pane's stdout rather than the app server.

The doc's summary table already has a "Terminal modal disappears" row. The
augmentation is to recognize that for Codex this row is not a fuzzy heuristic but
an **agent-emitted structured event**, and to make it first-class.

### Two hard limits of this edge

1. **Presentation-only and configurable.** The title is driven by
   `terminal_title_item` and the spinner/action animation; a user can disable
   either. The original doc's "Fallback without promising exact answers" caveat
   applies in full.
2. **It cannot identify which request was answered.** The title shows "blocked"
   while *any* request is pending. With two concurrent approvals in one pane,
   answering one leaves the title blocked, so the edge under-fires — it only
   clears when the last one clears. That is safe (it will not clear a sibling's
   request) but not per-request. Per-request resolution still needs the server
   bridge or #4462-style request identity. This is the same "concurrent activity
   is not resolution" caution as the original doc.



<!-- limpid comment
This is fine---I at the end of the day wnat to know that any is blocked---that's our model for read anyway
-->
---

## 3. Why Switchboard already pulls the title and still stays red

### 3.1 The title is collected but not used for Codex

The WezTerm seam already stores the title on the neutral ref:

```go
// internal/terminal/wezterm.go:70
func weztermPaneRef(p wezterm.Pane) PaneRef {
    return PaneRef{
        ...
        Title:       p.Title,
        WindowTitle: panetitle.Normalize(p.WindowTitle),
        ...
    }
}
```

But the demotion rule is gated on the agent kind and carries a stale assumption:

```go
// cmd/switchboard/main.go:1719
// ... Claude-only (codex paints no glyph), never on a suspended session ...
if tun.IdleTitleDemotionEnabled && c.Status == state.StatusWorking &&
    sess.Agent == state.AgentKindClaude && !sess.Suspended &&
    sess.Wezterm != nil && sess.Wezterm.TitleAt.After(c.StatusSince) &&
    titleShowsIdleGlyph(sess.Wezterm.Title, tun.IdleTitleGlyphs) &&
    now.Sub(c.StatusSince) >= tun.IdleTitleGrace {
```

```go
// cmd/switchboard/main.go:1775
func titleShowsIdleGlyph(title, glyphs string) bool {
    title = strings.TrimSpace(title)
    if title == "" || glyphs == "" {
        return false
    }
    r, _ := utf8.DecodeRuneInString(title)
    return strings.ContainsRune(glyphs, r)
}
```

This rule only ever **demotes working → idle**. It has no counterpart that
observes *blocked → working*, and it does not run for Codex at all. That is the
gap: the title edge is collected and thrown away for exactly the agent we care
about.


<!-- limpid comment
Ok, let's fix this.
-->

### 3.2 Even a fresh title observation cannot retire the latch

The approval latch lives in the hook reducer and is deliberately sticky:

```go
// cmd/switchboard/codex_hook_transitions.go:1419
func (c *agentCoordinator) overlayCodexApprovalObservation(key provider.RootKey, observation agentgraph.Observation, now time.Time) agentgraph.Observation {
```

The timer path spells out why the reducer will not accept indirect evidence:

```go
// cmd/switchboard/codex_hook_transitions.go:1131
// The graph alone cannot settle the gate: a notLoaded root overlaid with
// this hook's own deferred "working" looks identical. Only a direct
// app-server reading of the root since the gate opened is evidence.
appServerSettled = appServerSettled &&
    c.codexAppServerRootLiveSinceLocked(pending.ref.Key(), pending.sessionID, pending.startedAt)
```

And the central resolver refuses to let green evidence retire a semantic request
(`internal/statusresolve/resolve.go:138,169`, `internal/state/status_resolve.go`).
So there are **two independent blockers**, and fixing only one leaves red. This
is why "paint one green graph" is insufficient, as the original doc says.

---

## 4. What can be done without a Codex-side integration

Ordered by payoff.

### 4.1 Primary: make the title/OSC edge a first-class resolution signal

The strongest no-Codex-build option. It uses data already flowing for the local
TUI. Three sub-options, increasing robustness:


<!-- limpid comment
Yes, agree.
-->

- **4.1a — Title edge.** Extend the existing title watcher to Codex and add a
  *retirement* edge on blocked→working/idle. Works with `wezterm cli list` as-is.
  Fragile to title config and version text, same class as herdr's screen manifest.
- **4.1b — OSC 7501 program-status edge.** Structured
  (`state=blocked:kind=permission`), agent-emitted, no text matching. But
  `wezterm cli list` does not expose OSC 7501. Needs the terminal to surface it: a
  WezTerm OSC/user-var hook, or a small local sink. This is terminal integration,
  not Codex integration.
- **4.1c — Terminal modal observer.** The original doc's section C. Herdr #4462
  documents why this is weak: "bytes accepted by the PTY" is not "request
  resolved." Use only as a last resort.

### 4.2 Do not expect grace/polling changes to help

Reiterating the doc: the 30 s `codexHookApprovalGrace` classifies ownership; it
does not gate answer→green. Polling constants (`active` 1 s, `idle` 10 s, RPC 5 s)
add latency *after* an edge exists; they cannot manufacture one. Lowering them
cannot fix an absent edge. The acceptance test in §5 explicitly targets this.

### 4.3 What remains genuinely impossible without a Codex change

- Desktop, remote, or headless answers that never touch the local TUI's stdout.
- Distinguishing accept / decline / session-accept / amendment.
- Per-request resolution under concurrent approvals.

For those, the bridge in §6 is required. No terminal-side trick closes them.

---

## 5. Minimal change set A — Switchboard-only, no Codex build

Target: "approve a command that runs 60 s; the bar goes green while it runs," for
the local TUI. Roughly 4–6 files.

**Step 1 — stamp the title edge.** The seam already records `TitleAt`; make sure a
title change on a Codex pane updates it the same way it does for Claude. Verify in
`internal/terminal/wezterm.go` and the poller that `TitleAt` advances on title
transitions. No new field if `TitleAt` already tracks every observed title change.

**Step 2 — detect the Codex action edge.** Add a helper next to
`titleShowsIdleGlyph` (sketch, not existing code):

```go
// cmd/switchboard/main.go — sketch
// codexTitleRequiresAction reports the Codex title while a permission or
// question modal is open. Exact strings come from the Codex TUI title logic;
// keep them in one place and version them, as herdr does with its manifest.
func codexTitleRequiresAction(title string) bool {
    t := strings.ToLower(strings.TrimSpace(panetitle.Normalize(title)))
    return strings.Contains(t, "action required") ||
        strings.Contains(t, "waiting for approval")
}
```

The upstream text is produced by Codex's `terminal_title_shows_action_required` /
`refresh_terminal_title` (`chatwidget.rs:1198`); copy the strings from there rather
than guessing, and mark the fragility explicitly.

**Step 3 — retire the request on the edge.** The reducer must learn the edge. In
the approval reducer / timer path (`codex_hook_transitions.go`, around
`reduceCodexPendingApprovalLocked` and `startCodexApprovalTimerLocked`), before
publishing or re-asserting red, check for a newer title edge:

```go
// cmd/switchboard/codex_hook_transitions.go — sketch
// A title sampled after the gate opened that no longer requires action means
// the local modal closed: the human answered. Retire this request.
if sess.Wezterm != nil &&
    sess.Wezterm.TitleAt.After(pending.startedAt) &&
    !codexTitleRequiresAction(sess.Wezterm.Title) {
    c.stopCodexApprovalLocked(pending)
    delete(state.approvals, key)
    c.markCodexRequestResolved(key, pending) // tombstone; see step 4
    logCodexHookWait("resolved", pending, "title_answer_edge", now)
    return false
}
```

**Step 4 — fence delayed hooks and snapshots.** The original doc's "resolution
tombstone/sequence fence" requirement. Add a per-root set of resolved request IDs
(or a monotonic `resolvedSeq`) so that:

- `overlayCodexApprovalObservation` (`:1419`) and
  `overlayCodexPendingObservation` (`:1378`) skip any request already tombstoned,
  instead of re-asserting attention on the next snapshot;
- a late `PermissionRequest`/`PreToolUse` for the same tool call cannot reopen the
  episode;
- reconnect replay cannot revive it.

**Step 5 — teach the resolver to accept the edge.** Non-negotiable; without it the
overlay fix alone still loses to the resolver. In `internal/statusresolve/resolve.go`,
introduce a resolution-flavored observation (an `EvidenceKind` "answer-edge" or a
`ResolvesRequest` flag) that clears the matching attention claim rather than
competing with it. Preserve the invariant: a *plain* green terminal reading still
cannot clear a semantic request — only a signal that represents the answer edge
can. This encodes the intended contract:

> "once your action removes the block, green can represent presumed continuation
> before execution is independently confirmed."

**Step 6 — tests.** Extend `codex_hook_transition_test.go` and the statusresolve
suite: title edge retires an open approval; edge does not clear a sibling's
request; edge older than the gate is ignored; a late hook cannot resurrect;
decline/ESC leaves the expected state.

### Why this is the true minimum

It touches no Codex source and no terminal protocol. It converts data Switchboard
already receives into the edge it already needs. The cost is the fragility and
coverage limits in §2 / §4.3.

---

## 6. Minimal change set B — small Codex bridge

Only worth it for desktop/remote/headless and accept/decline fidelity. The bridge
must be emitted where the **owning** app server accepts the response, then
delivered to Switchboard out-of-band — Switchboard's separate
`codex app-server --stdio` is not on that link. The original doc is right about
this.

### 6.1 The server already has the exact response

```rust
// ~/Programs/codex/codex-rs/app-server/src/outgoing_message.rs:484
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
            if entry.callback.send(Ok(result)).is_err() { ... }
        }
        None => { warn!("could not find callback for {id:?}"); }
    }
}
```

This is the exact point where an answer becomes a typed `Result` for a specific
connection/request. It is early — before the approved tool runs — and it has the
response kind.

### 6.2 The resolution notification already exists but is too coarse

```rust
// ~/Programs/codex/codex-rs/app-server/src/request_processors/thread_lifecycle.rs:1035
outgoing.send_server_notification(ServerNotification::ServerRequestResolved(
    ServerRequestResolvedNotification {
        thread_id,
        request_id,
    },
)).await;
```

The installed schema's `ServerRequestResolvedNotification` carries only `threadId`
and `requestId` (the doc's evidence). The smallest useful bridge is one of:

- **Additive fields** on this notification — `outcome`
  (accept/decline/session-accept/amendment/cancel) and `kind`
  (permission/question/elicitation) — populated from the callback result in
  `notify_client_response`. Additive to the protocol; existing clients ignore
  unknown fields.
- **Or a new notification** (e.g. `serverRequest/answered`) emitted from the same
  handler, carrying the same fields plus a connection generation.

Both are small. The hard part is delivery to Switchboard.

### 6.3 Delivery options, cheapest first

1. **Out-of-band sink.** The owning server writes a one-line JSON event to a
   well-known path or Unix socket (the original doc's "small Codex-side event
   bridge"). Switchboard tails it. Smallest Codex diff; needs a Codex build or
   upstream acceptance. Covers desktop/remote/headless because the event is emitted
   by the server, not the TUI.
2. **Terminal integration for OSC 7501.** No Codex change: a WezTerm hook that
   observes OSC 7501 and forwards it to Switchboard. Local TUI only; gives `kind`
   but not accept/decline. This is really a variant of §4.1b.
3. **Subscribe Switchboard to the owning thread.** Rejected by the original doc
   for good reasons: `thread/read` does not subscribe; `thread/resume` has runtime
   effects; a second app-server is not a tap. Do not do this.

### 6.4 Switchboard consumer side

Same ledger retirement as §5 steps 3–5, but keyed by the real request ID and
connection generation instead of the title edge. The original doc's recommended
sequence stands verbatim: mark resolved, cancel timer, retire latch + attention,
preserve siblings, publish presumptive green, fence delayed hooks. The only
difference from A is the trigger source and the availability of `outcome`, so
decline can select the correct follow-on (green for "agent continues", idle for
cancel).

---

## 7. Which to pick

| Goal | Path |
|---|---|
| Local TUI, single blocking request, no Codex build, fast | **A** (§5) |
| Herdr-like "state label" only, no answer fidelity | A or 4.1b |
| Desktop/remote/headless, accept vs decline | **B** (§6) |
| Upstreamable, general | B with additive resolution fields |

If the only goal is the stated acceptance test — approve a 60 s command and see
green promptly — A is sufficient and ships without touching Codex. B is the general
fix and the right upstream ask, but it is not the minimum to satisfy the stated
requirement.

---

## 8. Updated validation

Add to the original doc's capture list one more clocked edge: **title/OSC
transition at submit**. For A, acceptance reduces to local TUI: approve a 60 s
command (green before completion), deny, ESC, and one concurrent-sibling case
where the title edge must not clear the other request. For B, add desktop/remote
and accept/decline/amendment outcomes. Measure answer→render against the same
<250 ms target and 3 s ceiling. Everything else in the original doc's validation
section stands.

---

## 9. Open questions

- Does WezTerm surface OSC 7501 through any hook API? This decides whether §4.1b
  is free or needs a small terminal integration.
- Is the Codex action-required title text stable across versions? Herdr's screen
  manifest risk applies; version the strings.
- Under concurrent approvals in one pane, the title edge under-fires. Confirm that
  the app-server bridge (B) is the accepted answer for per-request fidelity.
