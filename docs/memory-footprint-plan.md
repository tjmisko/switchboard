# Memory Footprint Reduction — orchestration plan

> **Goal.** Cut the resident footprint of the switchboard process tree from
> ~410 MB (cgroup-charged, measured 2026-08-26) to under ~120 MB with Codex
> running and under ~60 MB without, by removing allocation *churn* rather than
> retained state — the daemon's live heap is already only ~5–10 MB.
>
> **Non-goal.** Deleting the bench fixtures under `~/.local/state/switchboard`
> (issue #89). That is disk, not RAM, and #89 wants a full-day `MemoryPeak`
> reading first. Do not touch those directories from this plan.
>
> **Hard invariant.** Nothing in this plan may change what lands in the history
> day-files. `switchboard-dashboard` reads *only* those files (via
> `switchboard-ctl timeline --json`), and issue #83 makes Codex child
> `agent_state` history a production invariant. Task #3 guards this with a test
> that must stay green through every phase.

---

## 1. Baseline (2026-08-26 16:20–16:25, goosebook: Asahi arm64, 16K pages, Go 1.26.5, 15.2 GiB RAM)

### 1.1 Where the bytes are

| Unit / process | RSS | Anon | Notes |
|---|---|---|---|
| `switchboard.service` cgroup | 149 MB | 62 MB | `MemoryPeak` 172 MB since 16:15 restart |
| ↳ `switchboard` daemon | 33–50 MB sawtooth | 42 MB | `heap_alloc_mb=5` right after seed; rest is GC headroom |
| ↳ `codex app-server --stdio` (daemon child) | 103 MB | 33 MB | spawned **eagerly** in `NewObserver` regardless of Codex roots |
| ↳ `ssh … remote-stream` | 9 MB | — | federation to `nlessfun` |
| `switchboard-waybar.service` cgroup | 136 MB | 100 MB | |
| ↳ 10 × `switchboard-waybar --slot N` | 12 MB each | 7.6 MB each | Go hello-world floor on this box: 2.8 MB RSS / 1.2 MB anon |
| ↳ `waybar -c claude.jsonc` (GTK) | 47 MB | — | not ours |
| `switchboard-dashboard.service` cgroup | 75 MB | 9.5 MB | peak 1.09 GB earlier today = pre-scrub `ctl timeline` child; today's run is 34 MB / 0.5 s — already fixed |
| `switchboard-ctl bottombar watch` | 12 MB | — | an 11th always-on subscriber |
| `arachne-switchboard-recorder.service` | 38 MB cgroup / 6 MB RSS | — | out of scope |

System pressure at the time (9.5 GiB used, 8.3 GiB swap) was Firefox (~2 GB),
`claude` (422 MB), wezterm (397 MB), two `codex resume` (490 MB), Obsidian —
not switchboard. This plan is about switchboard's own hygiene.

### 1.2 The churn (root cause)

A 15 s `subscribe-all` capture: **141 frames = 9.4 publishes/s, 13.7 KB
each, to 11 subscribers** (≈1.4 MB/s of JSON). Diffing consecutive frames,
the only fields that change:

- `sessions[i].wezterm.window_title` on the two Codex sessions — the Codex TUI
  rotates a braille spinner (`⠹ ⠴ ⠧ ⠋ ⠸ …`) in the pane title
- `sessions[i].agent_graph.observed_at` / `fresh_until` — timestamps that
  advance on every observation with no semantic change
- `updated_at` (already excluded from the change key)

Chain: spinner tick → Hyprland `windowtitlev2` → `reresolveAll`
(`cmd/switchboard/main.go:550`, rate-limited to 200 ms, each one forks
`wezterm cli list` + `hyprctl clients`) → title differs →
`snapshotChangeKey` (`internal/state/state.go:656`) includes the title →
`broadcast` + `persist(state.json)`.

With `-remote` configured, `rpc.subscribeAll` (`internal/rpc/rpc.go:467`)
goes through `federation.View`, and **every subscriber rebuilds the aggregate
and re-encodes it itself** (`s.view.Snapshot()` + `enc.Encode` per wakeup) —
11 deep copies + 11 encodes per publish, ~100/s total. `View.publish()`
(`internal/federation/view.go:320`) has no change gate, so remote-side churn
fans out identically.

Each waybar slot decodes 14 KB ~10×/s → the ~4.8 MB heap arena per slot.

### 1.3 Measured levers

| Experiment | Result |
|---|---|
| `switchboard-waybar --slot 0`, default | 10.5 MB RSS / 6.4 MB anon |
| `GOGC=25` | 8.6 MB / 4.5 MB |
| `GOMAXPROCS=1 GOGC=25` | 7.9 MB / 3.7 MB |
| `switchboard-ctl timeline --json --plan-window --day 2026-08-26` | 34 MB peak, 0.54 s |

---

## 2. Dashboard data contract (what must not move)

`switchboard-dashboard` (`~/Projects/switchboard-dashboard`):

- Reads history **only** through `switchboard-ctl timeline --json --plan-window`
  (`~/.config/switchboard/providers.json`); `cmd/switchboard-ctl/timeline.go`
  never dials the socket or reads `state.json`.
- Second provider `arachne-switchboard-ctl` reads the arachne recorder's own log.
- SessionEnd hook reads `~/.local/share/switchboard/summaries`.
- Consumes: `session_start/end`, `transition`, `session_label`, `usage_sample`,
  `focus`, `activity`, `subagent_spawn/stop`, `workflow_*`, `suspend/resume`,
  and Codex child `agent_state` with `parent_thread_id` and
  `source: codex_app_server` (issue #83; `agent_timeline` is built only from
  those).
- Today's composition: 6686 `usage_sample`, 698 `focus`, 514 `agent_state`
  (296 `codex_app_server`, 137 `hook`), 187 `transition`, 29 `session_label`.

Producers: every `sink.Record` call is in the reconcile tick, a hook handler,
or the provider observation loop (`cmd/switchboard/main.go:209,302,920,981,1024`,
`fanout.go:102,122,152`, `agent_observation.go:397,521`). **None depend on
`Store.Apply` deciding to publish** — `Apply` always runs the mutation; only
`broadcast` and `persist` sit behind the change key (`state.go:554`).
`session_label` already strips spinner glyphs (`internal/label/label.go:32`).

Per-change verdict:

| Change | History impact |
|---|---|
| Change-key normalization (Phase 1) | none — wire only |
| Federation single-encode + gate (Phase 1) | none — wire only |
| Lazy Codex app-server (Phase 2) | **touches** first-snapshot latency and would lose child events if the child were down while a root is live → written as an invariant with tests (#10, #11) |
| Single renderer (Phase 3) | none |
| GOGC (Phase 4) | none |

---

## 3. Task graph

Conventions: `#N` IDs are unique across this file. A task is done only when it
is in `DONE.md` with a date. Phases are sequential; do not start a
`[FUTURE]` phase. Every code task ships its tests in the same commit
(conventional commits, one logical change each). Work in a worktree under
`.worktrees/`, never on `main`.

Verification commands used throughout:

```sh
systemctl --user show switchboard switchboard-waybar -p Id -p MemoryCurrent -p MemoryPeak
cat /sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/switchboard.service/memory.stat | head -3
grep -E 'VmRSS|RssAnon' /proc/$(systemctl --user show switchboard -p MainPID --value)/status
journalctl --user -u switchboard -g 'publish-stats|fanout-seed' --since -1h
```

### Phase 0: Measurement apparatus [CURRENT]

- [ ] #1: `scripts/sb-mem-baseline` — one-shot footprint + churn report
  - **Prereqs**: none
  - **DoD**: Script prints (a) `MemoryCurrent/MemoryPeak` and `memory.stat`
    anon/file for `switchboard`, `switchboard-waybar`, `switchboard-dashboard`
    units; (b) `VmRSS`/`RssAnon` for the daemon, its `codex app-server` child,
    each `switchboard-waybar --slot` process, and `bottombar watch`; (c)
    publish rate and mean frame bytes from a 15 s `subscribe-all` capture on
    `$XDG_RUNTIME_DIR/switchboard.sock` (newline-delimited JSON; request is
    `{"cmd":"subscribe-all"}`). Output is one JSON line plus a human table.
    Runs with no arguments on this machine; exits non-zero if the socket is
    absent. Sibling of `scripts/sb-perf` / `scripts/sb-bench-seed` in style.
  - **Phase**: 0
  - **Notes**: The capture logic exists in the analysis scratchpad
    (`subrate.py`, `framediff.py`); port it to Go or POSIX sh — no Python
    dependency in `scripts/`. Bash rules: no `for` loops where `xargs`/`jq`
    will do; `>|` for overwrites.

- [ ] #2: Daemon `publish-stats` telemetry line
  - **Prereqs**: none
  - **DoD**: Once per minute the daemon logs one line
    `publish-stats: publishes=<n/min> suppressed=<n/min> subscribers=<n>
    frame_bytes=<mean> heap_alloc_mb=<n> heap_sys_mb=<n> vm_hwm_mb=<n>`,
    same shape and placement as `fanout-seed`
    (`internal/fanout/seedtelemetry.go`). `suppressed` counts `Apply` calls
    whose change key matched. Unit test asserts the counters increment
    correctly across a publish and a suppressed publish. Line documented next
    to `fanout-seed` in the seed plan's telemetry section or a new
    `docs/telemetry.md`.
  - **Phase**: 0
  - **Notes**: Counters live on `state.Store` (`adoptPublishedLocked` is the
    one place that decides). Reuse `vmHWMKB` from `seedtelemetry.go` — move it
    to a small shared helper rather than copy it.

- [ ] #3: Invariant test — history recording is independent of publish suppression
  - **Prereqs**: none — this is the guard every later phase runs against
  - **DoD**: A test in `cmd/switchboard` (or `internal/state` + a sink fake)
    named `should record history events when Apply suppresses the publish`:
    drive a reconcile/hook path that calls `sink.Record` while the snapshot's
    change key is unchanged, assert the sink received the event and the
    Store's broadcast channel received nothing. Green on `main` before any
    Phase 1 change lands.
  - **Phase**: 0
  - **Notes**: Cheapest seam is `agent_observation.go:397/521` (agent_state
    projection) or `main.go:920` (transition). Pick whichever has an existing
    test harness with an injectable sink.

- [ ] #4: Record the baseline in this document
  - **Prereqs**: #1 (the script is the measurement) complete
  - **DoD**: §1 tables above replaced/confirmed by `sb-mem-baseline` output
    captured with two Codex sessions live and the bottom bar showing; the raw
    JSON line pasted under a `### 1.4 Baseline capture` heading with the
    timestamp.
  - **Phase**: 0

### Phase 1: Stop the publish storm [FUTURE — do not start until Phase 0 is in DONE.md]

- [ ] #5: Shared title normalization
  - **Prereqs**: none
  - **DoD**: One package (`internal/terminal` or new `internal/titlenorm`)
    exports the spinner glyph table and a `Normalize(title string) string`
    that strips one leading spinner rune plus surrounding whitespace, covering
    both Claude's set (`internal/label/label.go:32`) and the braille set the
    Codex TUI uses (`⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏`, plus `◐◓◑◒`). `internal/mapping.normalizeTitle`
    (`mapping.go:300`) and `internal/label` call it; their existing tests still
    pass; a table test covers every glyph, an empty title, a title that is only
    a glyph, and a title whose first rune is a normal letter.
  - **Phase**: 1
  - **Notes**: Issue #80 is the same race seen from the mapping side. Check
    whether #80 can be closed once this lands — `normalizeTitle` may already
    have fixed it; if so, note it on the issue with the commit hash.

- [ ] #6: Normalize the publish change key
  - **Prereqs**: #5 (needs the shared normalizer), #3 (the invariant guard
    must exist before the gate gets stricter) complete
  - **DoD**: `snapshotChangeKey` (`internal/state/state.go:656`) is computed
    from a copy of the snapshot in which (a) every `Wezterm.WindowTitle` is
    passed through `Normalize`, (b) `AgentGraph.ObservedAt` is dropped and
    `AgentGraph.FreshUntil` is replaced by its value **rounded up** to a 5 s
    bucket. Node-level `updated_at`, `Summary.Since`, status, pending writers
    and everything else stay in the key. Tests:
    `should not republish when only the title spinner rotates`,
    `should not republish when only observed_at advances within a bucket`,
    `should republish when fresh_until crosses a bucket boundary`,
    `should republish when a node runtime state changes`,
    `should republish when a session's label or status changes`.
    The wire snapshot is unchanged (title and timestamps still go out raw on
    the frames that do publish).
  - **Phase**: 1
  - **Notes**: Keep the "fail open on encode error" behavior. The 5 s bucket
    means the bar can be at most 5 s behind on freshness; #7 makes that
    invisible.

- [ ] #7: Freshness lease on the wire
  - **Prereqs**: #6 complete — only meaningful once republishes are suppressed
  - **DoD**: On the frames that are published, `agent_graph.fresh_until` is
    the bucket **ceiling** (never earlier than the true value), so a consumer
    evaluating `AgentGraph.Fresh(now)` (`internal/state/agent_graph.go:80`,
    used by `cmd/switchboard-waybar/main.go` `agentTooltip`) never reports
    stale while the daemon holds a fresh graph. Test in the waybar package:
    `should not mark the tooltip stale when the daemon suppressed a
    within-bucket republish` (simulate: publish at t, no publish until t+4.9 s,
    assert not stale; at t+5.1 s with no publish, stale is allowed).
    `docs/state-schema.md` notes the ceiling semantics.
  - **Phase**: 1

- [ ] #8: Federation View — one encode per publish, change-gated
  - **Prereqs**: #6 complete — the View must reuse the same normalized key,
    not invent a second one
  - **DoD**: `federation.View.publish()` (`internal/federation/view.go:320`)
    builds the aggregate once, computes the normalized change key, returns
    early when it matches the last published key, and hands subscribers a
    frame carrying the encoded bytes (mirror `state.Broadcast.JSON`).
    `rpc.subscribeAll` (`internal/rpc/rpc.go:467`) writes the shared bytes;
    the "treat the channel as a notification, re-read the latest" ordering
    guarantee is preserved by re-reading the latest *shared frame*, not by
    rebuilding per subscriber. Tests:
    `should encode the aggregate once per publish regardless of subscriber count`
    (count encodes with a counting encoder or by asserting identical backing
    slices), `should not fan out a remote frame that differs only by spinner
    or observed_at`, `should still deliver the final frame on remote
    disconnect` (the existing coalesce comment's guarantee), and the existing
    B→A ordering test stays green.
  - **Phase**: 1
  - **Notes**: The local-only path (`streamLocalSnapshots`) also re-snapshots
    per subscriber despite `Broadcast.JSON` existing; fix it the same way in
    the same commit.

- [ ] #9: Separate rate limit for `windowtitlev2`
  - **Prereqs**: #6 complete — with the key normalized, the remaining cost of a
    title event is the fork pair, which is what this task bounds
  - **DoD**: `drainWMEvents` (`cmd/switchboard/main.go:580`) coalesces
    title-only layout events in a 1.5 s window while `openwindow` /
    `movewindowv2` keep the 200 ms `layoutDebounce`; a title event never
    delays a pending open/move flush. `internal/wm` events carry enough to
    tell the raw kinds apart (extend `wm.Event` or split `EventLayoutChanged`
    into `EventTitleChanged`). Tests (no real sleeps — use the injected
    debounce): `should coalesce a burst of title events into one re-resolve
    per window`, `should still re-resolve a new window within the short
    window while titles are bursting`, `should re-arm after a firing`. The
    hazard comment above `layoutDebounce` updated to describe both windows.
    Measured: `wezterm cli list` forks ≤ 1/s with two spinning Codex panes
    (count `wezterm` children via `journalctl` or `strace -f -e execve` for
    15 s).
  - **Phase**: 1

- [ ] #10: Phase 1 verification
  - **Prereqs**: #6, #7, #8, #9 complete
  - **DoD**: `sb-mem-baseline` with two Codex sessions spinning and the bottom
    bar up shows publish rate < 0.5/s, daemon `RssAnon` steady < 25 MB over
    10 min, `publish-stats` shows `suppressed` ≫ `publishes`; `state.json`
    mtime advances < 1/s. Bar behavior unchanged by eye: chips, tooltips,
    "idle · Nm" counters, stale marking, remote chips. Numbers recorded in
    §5. Issue #80 updated.
  - **Phase**: 1

### Phase 2: Lazy Codex app-server [FUTURE — do not start]

- [ ] #11: Supervisor holds a connection only while a root or hook binding exists
  - **Prereqs**: #3 (invariant guard) and #10 (Phase 1 verified) complete —
    this is the only change that touches dashboard data, so it lands on a
    quiet, measured base
  - **DoD**: `codex.NewObserver` (`internal/provider/codex/observer.go:147`)
    no longer spawns the child. The supervisor loop (`run()`, line 327) is
    entered when the first root is observed or a hook binding is registered
    (`ReconcileHookBinding` already signals `refresh`), and torn down —
    child closed, per-connection ctx cancelled — after `Config.IdleShutdown`
    (default 2 min) with zero roots and no bindings. `IdleShutdown = 0`
    preserves today's always-on behavior. A root arriving during the grace
    cancels the shutdown; a root arriving after it restarts the supervisor
    with `ReconnectMinimum` backoff reset. `Close()` semantics unchanged.
    Tests (fake app-server via the existing `Connector` seam):
    `should not spawn the app-server before the first codex root`,
    `should spawn on the first root and initialize within one connect`,
    `should keep the child alive through the idle grace when a root returns`,
    `should close the child after the grace with no roots`,
    `should restart the child for a root that appears after shutdown`.
  - **Phase**: 2
  - **Notes**: Today `ctx`/`cancel` are observer-lifetime; introduce a
    per-connection context so shutdown does not poison `Close()`. Keep the
    `EphemeralNamer` path working — check whether it opens its own connection
    (`namer_test.go` suggests it uses `Connector` directly) and if so it is
    unaffected.

- [ ] #12: Dashboard invariant under lazy spawn (issue #83)
  - **Prereqs**: #11 complete
  - **DoD**: With the child not yet connected, `Observe` for a bound root
    returns the existing "snapshot pending" diagnostic and never a fabricated
    graph; on the first complete snapshot after spawn the projector emits the
    `unknown/none/unknown → observed` edges exactly once per node with
    `parent_thread_id` set; a resnapshot after reconnect is idempotent.
    Tests: `should emit first-snapshot agent_state edges exactly once after a
    lazy spawn`, `should not lose child agent_state for a root that appears
    during connect backoff`, `should not convert a child to not_found from a
    partial snapshot during spawn`. Live check: start a Codex session on a
    daemon that had none; the day-file shows the root's first
    `agent_state … source=codex_app_server` within 5 s of `session_start`.
  - **Phase**: 2

- [ ] #13: Journal categories + docs for child lifecycle
  - **Prereqs**: #11 complete
  - **DoD**: `agent-observer: provider=codex category=child_started|child_stopped
    count=1` lines (content-free, like the others); `docs/codex-session-status/
    03-codex-app-server-observer.md` describes the lazy lifecycle and the
    `IdleShutdown` knob; `-codex-observer auto|off` unchanged.
  - **Phase**: 2

- [ ] #14: Phase 2 verification
  - **Prereqs**: #12, #13 complete
  - **DoD**: With no Codex process running for > 2 min, `switchboard.service`
    cgroup anon < 40 MB and no `codex app-server` in `cgroup.procs`; with Codex
    running, footprint equals Phase 1's. `publish-stats` unchanged. Numbers
    recorded in §5.
  - **Phase**: 2

### Phase 3: One renderer instead of eleven subscribers [FUTURE — do not start]

- [ ] #15: Interim env tuning in the bar config (zero code)
  - **Prereqs**: none — independent, ships the same day as Phase 0 if desired
  - **DoD**: Every `custom/claude-N.exec` in `~/.config/waybar/claude.jsonc`
    is `env GOGC=25 GOMAXPROCS=1 /home/tjmisko/go/bin/switchboard-waybar --slot N`
    (and `--width-px` if not already set, which also removes the 10 × `hyprctl`
    startup fork). `sb-mem-baseline` shows per-slot RSS ≤ 8 MB. Reverted by #17.
  - **Phase**: 3
  - **Notes**: The dotfiles repo owns that file; note the change there.

- [ ] #16: FIFO renderer spike
  - **Prereqs**: none
  - **DoD**: A throwaway prototype proves, on Waybar v0.15.0: a `custom`
    module with `exec: cat $XDG_RUNTIME_DIR/switchboard/slot-0` and
    `restart-interval: 1` renders JSON lines written by a writer that holds
    the FIFO open `O_RDWR` (no EOF, no blocking open); killing and restarting
    waybar re-attaches without the writer noticing; killing the writer makes
    `cat` exit and waybar restart it within `restart-interval`; the `cat`
    process is ≤ 1.5 MB RSS. Findings written up in a `### 3.x spike` note
    in this file, including any Waybar quirk (e.g. `exec-on-event` re-running
    `cat` after a click — set it `false`).
  - **Phase**: 3

- [ ] #17: Fold rendering into `switchboard-ctl bottombar watch`
  - **Prereqs**: #16 (design proven), #8 (single shared encode; otherwise the
    renderer's one subscription still costs a per-subscriber rebuild) complete
  - **DoD**: `cmd/switchboard-waybar`'s `renderSlot`, `sessionTooltip`,
    `agentTooltip`, `nameConfig` and `emitter` move to `internal/waybarchip`;
    `switchboard-waybar` keeps aggregate mode and `--slot` for debugging but
    is no longer in `claude.jsonc`. `bottombar watch` — which already owns the
    waybar process and already subscribes — becomes the renderer: one
    subscription, one decode per frame, `renderSlot` for `N` slots
    (`--slots 10` flag, default 10), one FIFO per slot under
    `$XDG_RUNTIME_DIR/switchboard/slot-N`, byte-identical dedupe per slot,
    all slots re-emitted on reconnect. Tests: `should write a slot line only
    when it changes`, `should re-emit every slot after reconnect`,
    `should keep FIFOs open across a waybar restart`, `should render the
    empty class for slots past the session count`, `should stop the bar and
    keep serving FIFOs when the last session ends` (or document that FIFOs
    are torn down with the bar — pick one and test it).
  - **Phase**: 3
  - **Notes**: `bottombar` runs before the daemon dial in `ctl main()` on
    purpose (must tolerate the daemon being down) — keep that.

- [ ] #18: Config, unit, docs cutover
  - **Prereqs**: #17 complete
  - **DoD**: `~/.config/waybar/claude.jsonc` modules use `cat` on the FIFOs
    with `restart-interval` and `exec-on-event: false`;
    `systemd/switchboard-waybar.service` runs the new watcher with
    `Environment=GOGC=50`; `docs/bars/*` describe the FIFO contract;
    `DONE.md` entry for #15 notes it is superseded. `sb-mem-baseline`:
    `switchboard-waybar.service` anon < 25 MB excluding the GTK `waybar`
    process; exactly one `switchboard` subscriber on the socket
    (`ss -xp | grep -c switchboard.sock` → 1, plus the dashboard if it is
    ever changed to subscribe).
  - **Phase**: 3

### Phase 4: GC headroom [FUTURE — do not start]

- [ ] #19: `GOGC=50` on the daemon unit — only if still worth it
  - **Prereqs**: #10 complete — decide from post-Phase-1 numbers, not the baseline
  - **DoD**: If daemon `RssAnon` after Phase 1 is still > 25 MB:
    `systemd/switchboard.service` gains `Environment=GOGC=50` beside
    `GOMEMLIMIT` with a comment pointing at `publish-stats`; before/after from
    `sb-mem-baseline` recorded in §5; CPU (`systemctl --user show -p CPUUsageNSec`
    delta over 10 min) not worse than +10%. If already < 25 MB: task closed
    as "not needed" in `DONE.md` with the number.
  - **Phase**: 4

### Phase 5: Close-out [FUTURE — do not start]

- [ ] #20: Final numbers and hand-off to #89
  - **Prereqs**: #10, #14, #18, #19 complete
  - **DoD**: §5 table filled: per-unit anon and RSS with and without Codex,
    publish rate, fork rate; one full-day `MemoryPeak` reading for
    `switchboard.service` on the final build posted as a comment on issue #89
    (that is the reading #89 is waiting for — do not delete the fixtures here).
    Memory note `memory-footprint-baseline-2026-08-26` updated with the end
    state.
  - **Phase**: 5

---

## 4. Landing order and session loop

Order: #1 #2 #3 (parallel) → #4 → #5 → #6 → #7 → #8 → #9 → #10 → #15 (any time) →
#11 → #12 → #13 → #14 → #16 → #17 → #18 → #19 → #20.

Suggested PRs: Phase 0 (one PR); #5+#6+#7+#8 (one PR — they only make sense
together); #9; Phase 2; #16 findings as a doc commit; #17+#18; #19.

After completing each task:
1. Tick the box here.
2. Move it to `DONE.md` with today's date and a one-line note (measured
   numbers go in the note).
3. Check whether the next task's prereqs are all in `DONE.md`.
4. If yes and it is in the current phase, begin it.
5. At a phase boundary, run the phase's verification task, then stop and
   summarize — every phase boundary here is a live-deploy decision the owner
   makes.

Deploy = `go install ./cmd/...` into `~/go/bin` plus copying into
`~/.config/switchboard/bin/` (the unit's `SWITCHBOARD_BIN` is
`%h/go/bin/switchboard`, but the running instance came from
`~/.config/switchboard/bin/` — check `systemctl --user show switchboard -p
ExecStart` before assuming), then `systemctl --user restart switchboard`.
Note each restart resets `MemoryPeak`.

---

## 5. Results

| Milestone | Daemon anon | switchboard.service anon | waybar unit anon | publish/s | Date |
|---|---|---|---|---|---|
| Baseline | 42 MB | 62 MB (+33 MB codex child) | 100 MB | 9.4 | 2026-08-26 |
| After Phase 1 | _tbd_ | _tbd_ | _tbd_ | _tbd_ | |
| After Phase 2 (no Codex) | _tbd_ | _tbd_ | — | — | |
| After Phase 3 | — | — | _tbd_ | — | |
| Final | _tbd_ | _tbd_ | _tbd_ | _tbd_ | |
