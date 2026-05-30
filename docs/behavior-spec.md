# Switchboard — Behavioral Spec (Phase 0 test backlog)

> This is the **test backlog** for Phase 0. It reconstructs the *current
> observable behavior* of every load-bearing function so the portability
> refactor is provably regression-free. Each item is phrased "should … when …"
> and maps to a test. Items marked ⚠ are **characterization tests**: they pin
> behavior that is a latent bug or an unwritten contract. Per the pin-then-fix
> policy (plan §0.9), each ⚠ first gets a test capturing *current* behavior
> (green on `main`), then a dedicated follow-up commit flips it to the intended
> behavior — recorded in `docs/decisions.md`.
>
> **Design principle (plan §0):** assert *observable contracts*, not OS
> mechanism. Where current code uses a Linux-ism (`/dev/pts/`, `/proc`, pidfd),
> the spec asserts the neutral observable (`tty != ""`, `ErrGone`,
> `onDeath`-once) so the same test passes on the future macOS backend.
>
> The §0.x labels on each heading are the plan task that consumes it. The
> traceability matrix at the end maps every plan test item to a spec section.

---

## 1. `internal/proc`

### 1.1 `parsePPID(status string) int`  — §0.3
- should return the numeric PPid when a `PPid:\t<n>` line is present.
- should trim surrounding whitespace/tabs around the value before parsing.
- should match on the exact `PPid:` line prefix (first match wins).
- should return `0` when no `PPid:` line exists.
- should return `0` when the PPid value is non-numeric (the `Atoi` error is swallowed).

### 1.2 `readTTY(pid int) string`  — §0.3 / conformance §13.1
- should return the first of fd `0,1,2` whose link starts with `/dev/pts/`.
- should return `""` when none of fd `0,1,2` point at a pts.
- ⚠ the `/dev/pts/` literal is **OS mechanism** — the conformance suite (§13.1)
  asserts `tty != ""` for an interactive child, **never** the prefix, so the
  macOS backend (`/dev/ttysNNN`) passes the same contract.

### 1.3 `Read(pid int) (Info, error)`  — conformance §13.1
- should return `ErrGone` when `comm`/`status` read hits "not exist" (process
  vanished mid-read — the common race).
- should leave `Exe` empty (not error) when the `/exe` readlink fails (kernel-masked).
- should leave `CWD` empty (not error) when the `/cwd` readlink fails.
- should populate `PPID` from the status file and `TTY` from `readTTY`.

### 1.4 `AllPIDs() ([]int, error)`  — §0.3
- should return only entries that are directories with an all-numeric name.
- should skip non-directory entries and non-numeric names.
- should propagate the error when `/proc` cannot be read.

---

## 2. `internal/discovery`

### 2.1 `IsClaude(p proc.Info) bool`  — §0.3
- should be `false` when `comm != "claude"`.
- should be `true` when `comm == "claude"` and `Exe == ""` (kernel masked the
  exe — benefit of the doubt).
- should be `true` when `comm == "claude"` and `Exe` contains `/claude/`.
- should be `false` when `comm == "claude"` and `Exe` is non-empty but lacks `/claude/`.
- should be case-sensitive on comm (`"Claude"` → `false`).
- should be `false` for a background subcommand — `Args[1] ∈ {daemon, mcp}` — even
  with `comm == "claude"` and a `/claude/` exe. The detached `claude daemon run`
  that Claude Code spawns and reparents to init shares comm + exe with a real
  session but has no controlling tty; without this it surfaces as an
  un-navigable "zombie" chip that outlives the session that spawned it. A session
  invoked with flags (`--resume`), a positional prompt, or nothing carries no
  verb and stays a session.

### 2.2 `Scanner` seen-set state machine  — §0.5 (inject `procSource` = AllPIDs+Read)
- should fire `onAppeared` exactly once for a newly-seen claude PID.
- should re-fire `onAppeared` after `Forget(pid)` (kernel recycled the PID for a
  fresh claude process).
- should never remember a PID whose `Read` errored (a later successful scan
  re-evaluates it).
- should never remember a non-claude PID (re-evaluated every tick — cheap, and
  lets a PID that *becomes* claude get caught).
- ⚠ should **shadow** a recycled PID that is still in the seen set if `Forget`
  was never called — it will not re-fire (characterization; relies on procwatch
  always `Forget`-ing on death).
- should invoke `onAppeared` **without holding the lock** (the callback re-enters
  the store; holding the scanner lock would not deadlock but is contractually
  lock-free).
- should be a no-op for the tick when `AllPIDs` errors (returns, nothing fired).

---

## 3. `internal/mapping`

### 3.1 `decodeCWD(cwdURL string) string`  — §0.3
- should return `""` when the input lacks a `file://` prefix.
- should strip the host component (everything up to the first `/` after `file://`).
- should decode `file:///abs/path` (empty host) to `/abs/path`.
- should percent-decode escapes (`%20` → space).
- should trim trailing slash(es) from the result.
- should return `""` when the percent-escape is malformed (`PathUnescape` error).
- ⚠ should return the **host string** when given `file://host` with no path
  component (characterization — a hostname leaks through as a "path").

### 3.2 `matchUniqueClient([]Client, muxPID int, title string) *Client`  — §0.5 (extract from `findHyprClient`)
- should return the single client matching **both** `pid == muxPID` AND `title == windowTitle`.
- should return `nil` when no client matches either key.
- should return `nil` when **two or more** clients match (ambiguous — let the
  next reconcile retry rather than guessing).
- should require **both** keys: a pid-only match or a title-only match is not selected.

### 3.3 `findHyprClient` wrapper  — §0.5
- should return `nil` when `hyprland.Clients` errors (WM unreachable → retry next tick).

### 3.4 `Resolve(ctx, info proc.Info) state.Session`  — §0.1 spec / behavior
- should return a bare session (`PID/CWD/TTY/StartedAt`) and skip all enrichment
  when `info.TTY == ""`.
- should set `StartedAt` to discovery wall-clock time (now), **not** the
  process's real start time.
- should leave `Wezterm` nil when `FindByTTY` errors or returns a nil pane.
- should populate `Wezterm` from the matched pane.
- should backfill `CWD` from `decodeCWD(pane.CWDURL)` **only** when `info.CWD == ""`.
- should populate `Hyprland` (address + workspace) when `findHyprClient` returns a
  client, and leave it nil otherwise.

### 3.5 `Reconcile(ctx, sess *state.Session)`  — §0.1 spec / behavior
- should no-op when `sess.TTY == ""`.
- should no-op (leave the session unchanged) when `FindByTTY` errors or returns nil.
- should overwrite every `Wezterm` field from the freshly-matched pane,
  allocating `Wezterm` if it was nil.
- should update `Hyprland.Address`/`Workspace` when a unique client matches,
  allocating `Hyprland` if it was nil.
- ⚠ should **keep the previous `Hyprland` address** when the match is ambiguous
  or absent — intentional "retry next tick," so a moved/closed window can hold a
  stale address transiently (characterization; unwritten contract).

---

## 4. `internal/state`

### 4.1 `Store.Apply(fn)`  — §0.4
- should run the mutation under the write lock, then broadcast, then persist —
  in that order.
- should report a persist failure to stderr without panicking and without
  blocking or rolling back the in-memory mutation.

### 4.2 `Store.Snapshot` / `snapshotLocked`  — §0.4
- should return sessions sorted ascending by `StartedAt`.
- ⚠ should leave sessions with **equal `StartedAt`** in unspecified relative
  order — the sort has no tie-break and is not stable (characterization; the fix
  pins a PID tie-break, which the `pickSession` index contract depends on).
- ⚠ should copy session *values* but **share** the `Wezterm/Hyprland/Claude`
  pointer blocks — read-only by convention only; a consumer mutating through a
  snapshot would race the store (characterization).
- should set `UpdatedAt` to now on every snapshot.

### 4.3 `Store.Subscribe` / `broadcast`  — §0.4 (run under `-race`)
- should deliver every post-mutation snapshot to a subscribed channel.
- should **drop** snapshots without blocking `Apply` when the cap-4 buffer is
  full (slow consumer must never stall the daemon).
- should close the channel and remove the subscriber when the cancel func is called.
- should never send on a channel after its cancel ran.

### 4.4 `Store.persist`  — §0.4
- should be a no-op (return nil) when the path is `""`.
- should create the parent directory if missing.
- should write atomically: encode to a `.state-*.json` temp file, then rename
  over the target (readers never see a partial document).
- should leave **no `.state-*.json` litter** when the encode step fails (temp
  removed on error).

### 4.5 `Store.Load`  — §0.4
- should be a no-op (return nil) when the path is `""`.
- should be a no-op (return nil) when the file does not exist.
- should hydrate sessions keyed by `PID` from a valid file.
- ⚠ should return an error early and hydrate **nothing** when the file is corrupt
  JSON — previously-persisted sessions are not restored; the daemon logs and
  continues, relying on the live scan to rebuild (characterization:
  "corrupt-JSON drops all sessions").

---

## 5. `internal/rpc`

### 5.1 `statusFromHookEvent(event string) string`  — §0.3
- should map `UserPromptSubmit` → `working` and `PostToolUse` → `working`.
- should map `PermissionRequest` → `permission`.
- should map `Stop` → `idle` and `SessionStart` → `idle`.
- should return `""` for any unrecognized event.
- ⚠ should **never** return `unknown` — the `ClaudeInfo.Status` doc lists it but
  it is unreachable (characterization; either emit it or drop it from the doc).

### 5.2 `pickSession(sessions, selector) *Session`  — §0.3
- should return the first `Focused` session when selector is `""` or `"active"`.
- should return `sessions[0]` when selector is `""`/`"active"` and none is focused.
- should return the session whose `PID == n` when selector parses to int `n` and
  some session has that PID.
- should fall back to index `n` (0-based) when no PID matches and `0 <= n < len`.
- ⚠ should prefer **PID 2 over index 2** when a session with PID 2 exists — same
  numeric selector resolves to different sessions depending on current state
  (characterization: PID-vs-index collision).
- should return `nil` for a non-numeric, non-keyword selector.
- should return `nil` when a numeric selector matches no PID and is out of index range.

### 5.3 `focus(ctx, selector)`  — §0.1 spec / behavior
- should error `"no sessions"` when the snapshot is empty.
- should error `"no session matches <selector>"` when `pickSession` returns nil.
- should error `"session has no hyprland address yet"` when the target's
  `Hyprland` is nil or its address is empty. *(Phase 1.5 replaces this with a
  typed "navigate unsupported" error.)*
- should dispatch `focuswindow address:<addr>` and then, if `Wezterm` is present,
  activate the pane — in that order — on success.

### 5.4 `handleHook(req)`  — §0.1 spec / behavior
- should no-op when both `status == ""` and `SessionID == ""`.
- should no-op when no tracked ancestor is found (a misconfigured hook can never
  corrupt state).
- should set `Claude.Status` when the mapped status is non-empty, allocating
  `Claude` if nil.
- ⚠ should set `Claude.SessionID` **write-once** — only when a session_id is
  supplied and the current value is empty; it is never overwritten
  (characterization).
- should attribute the update to the nearest tracked claude ancestor of `req.PID`.

### 5.5 `findTrackedAncestor(m, pid) int`  — §0.5 (inject `readProc`)
- should return `pid` immediately when `pid` is itself a tracked session (self-match).
- should walk the ppid chain and return the first tracked ancestor.
- should return `0` when `pid <= 1`.
- should return `0` when `readProc` errors or returns ppid `0`.
- ⚠ should bound the walk to depth 20, inspecting depths `0..19`, and return `0`
  when no tracked ancestor is found within that bound (characterization on the
  exact bound).

### 5.6 RPC protocol — `Serve`/`handle`/`subscribe`  — §0.1 spec / behavior
- `Serve` should remove the socket file on startup (stale unclean-shutdown
  socket) and again on exit.
- `handle` should respond `{"error":"unknown cmd: <c>"}` for an unrecognized command.
- `handle` should respond `{"error":...}` on a malformed (non-EOF) request line
  and close on EOF.
- `subscribe` should send the current snapshot immediately, then stream each
  subsequent snapshot until ctx is done or the channel closes.

---

## 6. `internal/hyprland`

### 6.1 `Subscribe` parse loop  — §0.4 (extract the loop to take an `io.Reader`)
- should split each line on the **first** `>>` into `{Name, Data}`.
- should drop lines that contain no `>>` delimiter.
- should tolerate long lines up to the 1 MiB scanner buffer (no error).
- should close the channel when ctx is cancelled or the reader reaches EOF.

### 6.2 `Clients` / `ActiveWindowAddress` / `Dispatch`  — §0.1 spec
- `Clients` should parse the `j/clients` JSON into `[]Client`; error on parse failure.
- `ActiveWindowAddress` should return the `address` field, or `""` when nothing
  is focused.
- `Dispatch` should succeed when the response begins with `ok`, error otherwise.
- `socketPath` should error when `HYPRLAND_INSTANCE_SIGNATURE` or
  `XDG_RUNTIME_DIR` is unset (the `Available() == false` path in conformance §13.2).

### 6.3 `HyprlandInfo.Monitor`  — characterization
- ⚠ should remain unpopulated — `Monitor` is declared but never written by any
  code path (always `""`); reserved (characterization).

---

## 7. Daemon event translation — `cmd/switchboard/handleHyprlandEvent`

### 7.1 Address `0x`-prefix normalization  — §0.7 / §0.9 (**highest-risk contract**)
- ⚠ should reconstruct the full address as `"0x" + evt.Data` for `closewindow`
  and `activewindowv2` — socket2 emits addresses **without** the `0x` prefix,
  while `hyprctl clients` stores them **with** it. The future WM seam must own
  this normalization so non-Hyprland backends produce already-comparable refs.

### 7.2 Event handling  — §0.7 conformance (neutral event names)
- `closewindow` should delete every session whose `Hyprland.Address == "0x"+Data`.
- `activewindowv2` should set `Focused = (Hyprland.Address == "0x"+Data)` for
  sessions with a `Hyprland` block, and `Focused = false` for sessions without one.
- `movewindowv2`, `windowtitlev2`, `openwindow` should re-`Reconcile` every live
  session.
- should ignore any other event name.

---

## 8. `internal/wezterm`

### 8.1 `Muxes()`  — §0.4
- should list `gui-sock-<pid>` entries whose `<pid>` is numeric **and** has a
  live `/proc/<pid>` (use the test's own pid as the "live" one).
- should skip entries not prefixed `gui-sock-`.
- should skip entries whose pid is non-numeric.
- should skip sockets whose owning pid is dead (no `/proc` entry) — connecting to
  a dead socket would hang until the per-call timeout.
- should return `nil` (no error) when `XDG_RUNTIME_DIR` is unset (`socketDir` `""`).
- should return `nil` (no error) when the wezterm dir does not exist.

### 8.2 `List` / `FindByTTY`  — §0.7 conformance (terminal locator)
- `List` should return the union of panes across all muxes, each tagged with its
  `MuxPID` + `MuxSocket`.
- ⚠ `List` should skip (and log) a mux whose `cli list` fails **without** failing
  the whole call — one bad endpoint must not blank the healthy ones
  (conformance contract: "one failing endpoint doesn't blank healthy ones").
- `FindByTTY` should return the pane whose `tty_name == tty`, or nil when none match.
- `FindByTTY` should propagate a `List` error.

### 8.3 `ActivatePane`  — §0.1 spec
- should run `wezterm cli activate-pane --pane-id <n>` against the given mux
  socket and wrap the combined output in the error on failure.

---

## 9. `internal/procwatch`  — §0.6 (real short-lived children; run under `-race` + goleak)

- should fire `onDeath` **exactly once** when a watched child is killed
  (regardless of cause: signal, exit, OOM).
- should be a no-op on a duplicate `Watch(pid)` — no second fd, no second goroutine.
- should fire `onDeath` immediately when the pid is already dead at `Watch` time
  (the `ESRCH` path).
- `Stop(pid)` should cancel the watcher **without** firing `onDeath`.
- `Watched()` should exclude exited and ESRCH-on-open PIDs.
- should **not** fire `onDeath` on `EINTR` (interrupted poll → continue looping).
- should leak no goroutines after Stop / death (goleak).
- ⚠ **known gap:** a `POLLERR` revent without `POLLIN` falls through the loop and
  can spin without ever firing `onDeath`. Documented, **not fixed** in Phase 0
  (characterization — recorded in `docs/decisions.md`).

---

## 10. `cmd/switchboard-ctl` bottombar — pure core

### 10.1 `shouldRun(topVisible bool, count int) bool`  — §0.3 (extract from `apply`/`reconcileWith`)
- should return `true` **iff** `topVisible && count > 0`. The four F8 cases:
  - `(hidden, 0)` → `false`
  - `(hidden, ≥1)` → `false`
  - `(visible, 0)` → `false`
  - `(visible, ≥1)` → `true`

### 10.2 `topVisible` / `bottomPID` / `envOr` / `runtimeDir`  — §0.4
- `topVisible` should be `true` when the marker file is **absent**, `false` when present.
- `bottomPID` should return the recorded pid only when it is live **and** its
  `comm` is `waybar`.
- `bottomPID` should remove the pidfile and return `0` on: missing pidfile,
  non-numeric/`<=0` content, dead pid, or `comm != waybar` (pid-reuse guard).
- `envOr` should return the env value when set and non-empty, else the fallback.
- `runtimeDir` should return `XDG_RUNTIME_DIR` when set, else `/tmp/run-<uid>`.

---

## 11. Bottombar lifecycle — Hyprland-only extra  — §0.8 (fake session-count + marker, stub launcher/killer)

- `reconcile`: top-hidden → `ensureStopped` regardless of session count, **without**
  dialing the daemon.
- `reconcile`: daemon-unreachable (`sessionCount` returns `ok == false`) → leave
  the bottom bar in its current state (no flap).
- `reconcile`/`reconcileWith`: top-visible + `count > 0` → `ensureStarted`;
  `count == 0` → `ensureStopped`.
- the **four F8 truth-table cases** end-to-end; `(top-hidden, bottom-present)` is
  **unreachable**.
- `ensureStarted` idempotence: no-op when the bottom bar is already running.
- `ensureStopped` idempotence: no-op when already stopped.
- `ensureStopped` should target the **process group** (`-pid`) so the
  `switchboard-waybar` slot subprocesses die with it (no orphans), falling back
  to the bare pid if the group kill errors.
- `reapChildren` should reap killed bottom-bar children so they don't pile up as
  zombies across repeated start/kill cycles.
- `watch` self-heal: should restore the correct bottom-bar state within the 3 s
  safety tick after an out-of-band change.

---

## 12. (reserved) — cross-cutting daemon wiring

- `dropStaleSessions` should delete every hydrated session whose PID is gone or
  no longer looks like claude, before live discovery starts.
- `onClaudeAppeared` should resolve, store, and `Watch` each new session, and on
  death delete the session and `Forget` the PID.

---

## 13. Cross-platform conformance suite  — §0.7 (**the scaling centerpiece**)

Backend-agnostic contract suites, parameterized over an implementation. Today
they run against thin adapters wrapping the Linux/Hyprland/wezterm concretes and
are **reused verbatim** in Phases 2–4 for sway/x11/tmux/macOS. Assertions are on
neutral observables only.

### 13.1 `osproc.Source` contract
- `Enumerate` should return claude procs carrying a `cwd` and a **non-empty tty**
  for an interactive child — assert `tty != ""`, **never** the `/dev/pts/`
  prefix.
- `Enumerate` should report an **empty tty** for a non-interactive child.
- `Read` of a dead pid should return `ErrGone`.
- `Exe`/`CWD` should be empty (not an error) when unobtainable.
- `Watch` should fire `onDeath` **exactly once** on any death cause.

### 13.2 `wm.Manager` contract
- `Clients()` should return items shaped `{address, pid, title, workspace}`.
- `ActiveWindow` should return the focused window's ref.
- `Focus` should succeed for a valid ref and error for an invalid one.
- the event stream should emit the canonical neutral names: `closewindow`,
  `activewindowv2`, `movewindowv2`, `windowtitlev2`, `openwindow`.
- ⚠ **address normalization:** the address in an `activewindow` event should
  compare **equal** to the corresponding `Clients().Address` — pins the Hyprland
  `0x`-prefix quirk as a *seam responsibility* (the single most fragile
  cross-layer contract).
- `Available()` should report `false` cleanly (no panic, no hang) when the WM is
  not running.

### 13.3 `terminal.Locator` contract
- `Locate(tty)` should return the owning pane with a stable `(mux, pane)` identity.
- one failing endpoint should not blank healthy ones.
- dead endpoints should be skipped (no hang).
- `Activate` should focus the located pane.

---

## Traceability matrix (plan §0.3–0.8 → spec section)

| Plan item | Spec section(s) |
|-----------|-----------------|
| 0.3 `proc.parsePPID` | 1.1 |
| 0.3 `discovery.IsClaude` | 2.1 |
| 0.3 `mapping.decodeCWD` (incl. ⚠ `file://host`) | 3.1 |
| 0.3 `rpc.statusFromHookEvent` (incl. ⚠ never-`unknown`) | 5.1 |
| 0.3 `rpc.pickSession` (incl. ⚠ PID-2 vs index-2) | 5.2 |
| 0.3 `bottombar.shouldRun` (four F8 cases) | 10.1 |
| 0.4 `state` Apply/Snapshot(⚠ tie)/Subscribe-drop/cancel/persist/Load(⚠ corrupt) | 4.1–4.5 |
| 0.4 `hyprland.Subscribe` parsing | 6.1 |
| 0.4 `wezterm.Muxes` | 8.1 |
| 0.4 `bottombar` topVisible/bottomPID/envOr/runtimeDir | 10.2 |
| 0.5 `discovery.Scanner` seen-set (inject procSource) | 2.2 |
| 0.5 `mapping.matchUniqueClient` (split from findHyprClient) | 3.2, 3.3 |
| 0.5 `rpc.findTrackedAncestor` (inject readProc, ⚠ depth-20) | 5.5 |
| 0.6 `procwatch` death semantics (⚠ POLLERR spin) | 9 |
| 0.7 `osproc.Source` contract | 13.1 |
| 0.7 `wm.Manager` contract (⚠ 0x normalization) | 13.2, 7.1, 7.2 |
| 0.7 `terminal.Locator` contract | 13.3, 8.2 |
| 0.8 F8 truth table + daemon-unreachable + idempotence + proc-group + reaper + self-heal | 11 |

### Characterization (⚠) items → `docs/decisions.md` (§0.9 pin-then-fix)

| ⚠ Item | Spec section |
|--------|--------------|
| `0x`-prefix normalization (highest risk; fix first) | 7.1, 13.2 |
| `Snapshot` `StartedAt` sort tie-break (fix first) | 4.2 |
| PID-vs-index `focus` selector collision | 5.2 |
| `Reconcile` keeps stale WM address on ambiguity | 3.5 |
| `Snapshot` shares pointer fields | 4.2 |
| `HyprlandInfo.Monitor` never populated | 6.3 |
| `ClaudeInfo.Status` `unknown` never emitted | 5.1 |
| `decodeCWD("file://host")` returns the host | 3.1 |
| `ClaudeInfo.SessionID` write-once | 5.4 |
| Corrupt-JSON `Load` restores no sessions | 4.5 |
| `procwatch` POLLERR-without-POLLIN can spin | 9 |
| `Scanner` shadows recycled PID without `Forget` | 2.2 |
