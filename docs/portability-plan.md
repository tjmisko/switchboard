# Switchboard — Portability Plan

> Goal: turn Switchboard from a Hyprland+wezterm+waybar appliance into a
> portable, runtime-detecting "see all your Claude Code sessions at a glance and
> jump between them" tool that installs as a single binary and works across
> Linux WMs/terminals/bars and macOS.

## Decisions (locked)

- **Platforms:** Linux + macOS (one codebase, per-OS files only where syscalls force it).
- **Backends to ship:** WM = Hyprland (have), sway/i3, X11/EWMH; terminal = wezterm (have), tmux; bars = waybar (have) + documented `state.json` contract for polybar/eww/i3blocks/TUI.
- **Selection:** runtime detection. One binary probes the environment and picks backends live. Build tags used **only** for Seam 1 (OS syscalls), never for WM/terminal/bar.
- **Sequence:** plan (this doc) → interface extraction + `none` backends → WM backends → terminal backends → macOS OS backend → docs + reference renderer.

## The capability-tier model

The whole design hangs on two tiers. Every backend either supports a tier or
cleanly declines it (`none` backend), and the daemon never hard-fails on a
missing integration.

| Tier | Capability | Required seams | Degrades to |
|------|-----------|----------------|-------------|
| **Observe** | count, cwd, per-session status (via hooks) | OS process layer only | — (this is the floor; always available) |
| **Navigate** | click/keybind to focus a specific pane+window | terminal locator **and** WM focus | Observe (focus commands return "unsupported") |

`state.json` is emitted in every tier and is the **stable public contract**. The
advertising headline is the Observe tier: *runs anywhere, zero desktop config,
any bar can render it.*

## The four seams (current coupling → target)

### Seam 1 — OS process layer  `internal/proc`, `internal/procwatch`, `internal/discovery`
- **Today:** `/proc/<pid>/{comm,exe,cwd,status,fd}` + `pidfd_open(2)` (Linux 5.3+).
- **Abstraction:** enumerate processes matching a predicate; return `{pid, ppid, comm, exe, cwd, tty}`; signal once when a pid dies.
- **Backends:** `linux` (/proc + pidfd, existing), `darwin` (`libproc`/`KERN_PROC` sysctl enumerate + kqueue `EVFILT_PROC`/`NOTE_EXIT` death). Per-OS via `//go:build`.

### Seam 2 — terminal locator  `internal/wezterm`
- **Today:** wezterm CLI + `gui-sock-<pid>` socket layout; match on `tty_name`; `activate-pane` to focus.
- **Abstraction:** `Locate(tty) → PaneRef{terminal-window identity, focus handle}`; `Activate(PaneRef)`. The **tty key is portable**; only the tool isn't.
- **Backends:** `wezterm` (existing), `tmux` (`tmux list-panes -a -F` exposes pane tty + pane pid + client; `select-pane`/`switch-client` to focus), `none` (terminals without IPC → Observe only).

### Seam 3 — window manager  `internal/hyprland`
- **Today:** Hyprland IPC request socket + socket2 event stream; `clients`/`dispatch focuswindow`; events `activewindowv2`/`closewindow`/`movewindowv2`/`windowtitlev2`/`openwindow`.
- **Abstraction:** `Clients() → []Window{address, pid, title, workspace}`; `Focus(ref)`; `Subscribe() → <-chan Event{focus-changed, window-closed, layout-changed}`. Address type becomes opaque (Hyprland `0x…`, sway `con_id`, X11 window id).
- **Backends:** `hyprland` (existing), `sway/i3` (i3 IPC binary protocol over `$SWAYSOCK`/`$I3SOCK`; `get_tree` for clients, `[con_id=…] focus`, `SUBSCRIBE ["window"]` events), `x11` (EWMH: `_NET_CLIENT_LIST`, `_NET_WM_PID`, `_NET_ACTIVE_WINDOW`; focus via `_NET_ACTIVE_WINDOW` ClientMessage; events via root `PropertyNotify`), `none` (Observe only).

### Seam 4 — UI  `cmd/switchboard-waybar`, `cmd/switchboard-ctl bottombar`
- **Today:** waybar custom-module JSON + CSS + two-process split + Hyprland-specific bottombar lifecycle.
- **Abstraction:** already decoupled — consumes only RPC `subscribe` + `state.json`. The bottombar auto-hide logic is Hyprland-coupled and stays an opt-in extra.
- **Backends:** waybar (existing); document the contract so polybar/eww/i3blocks/TUI are drop-ins. Ship one reference renderer (standalone TUI) that needs no desktop.

---

## Phases

Encoding: each task has **Pre** (precondition) and **DoD** (definition of done).
Check off as completed. Keep a running `DONE.md` log of merged milestones.

### Phase 0 — Behavioral regression baseline  (test-first; **hard gate for Phase 1**)
> The project ships **zero tests today.** Before any interface extraction we
> build a behavioral suite that pins *current observable behavior* so the
> refactor is provably regression-free: the suite must be **green on `main`
> before Phase 1, and stay green through it.** Behavior is reconstructed from
> three sources — the code, the README/intent invariants the user stated in
> prior sessions, and the per-function specs in `docs/behavior-spec.md` (the
> distilled output of the Phase-0 study). Where current behavior is a latent
> bug, we pin it as a **characterization test** (locks current behavior, flagged
> for a deliberate fix decision) — we never silently "fix" during the refactor.

**Two principles drive the design:**
1. **Test observable contracts, not OS mechanism.** Assertions are written
   against `Info`/`ErrGone`/`""`-tty/`onDeath`-exactly-once — NOT against
   `/dev/pts/` literals or procfs paths. This is what lets the *same* test pass
   on the future macOS backend (`/dev/ttysNNN`, kqueue) without rewrite.
2. **A "testability seam" is a safe, behavior-preserving subset of Phase 1.**
   Several units (discovery's scan loop, `findHyprClient`, the socket2 event
   parser, `findTrackedAncestor`) call package-level funcs directly and can't be
   unit-tested without `/proc`/a live WM. Phase 0 introduces *only* the minimal
   dependency-injection needed to test them; Phase 1 then formalizes those into
   the full interfaces. Nothing in Phase 0 changes runtime behavior.

- [ ] **0.0 Freeze the `state.json` schema.** Write `docs/state-schema.md`: every field, stability guarantees, the future `capabilities` block. DoD: schema doc committed; a golden `testdata/state.golden.json` round-trips through `Snapshot` encode/decode. *(Pins the public contract before anything moves.)*
- [ ] **0.1 Commit `docs/behavior-spec.md`.** The full per-function behavioral spec (Resolve/Reconcile/findHyprClient/decodeCWD; state Apply/Snapshot/persist/Load; rpc focus/hook/ancestor; hyprland event translation; wezterm Muxes/List/FindByTTY; proc/discovery/procwatch). Each item phrased "should … when …". This is the test backlog. DoD: committed; every (a)/(b)/(c) item below traces to a spec line.
- [ ] **0.2 Test harness & fixtures.** `internal/testsupport/`: fake `net.Conn`/line-feeder for stream parsers; temp-dir builders for a fake `XDG_RUNTIME_DIR` (wezterm `gui-sock-<pid>` layout) and a fake `/proc`-like tree; a real-short-lived-child helper (`exec sleep`, kill, observe) for death tests; golden-JSON helpers. DoD: harness compiles, used by ≥1 test in each package below.
- [ ] **0.3 Pure unit tests (no refactor).** Table-driven, fast, deterministic:
  - `proc.parsePPID` — `PPid:` prefix match, whitespace trim, missing/non-numeric → 0.
  - `discovery.IsClaude` — all branches: comm≠claude→false; empty-exe→true; `/claude/` substring→true; non-empty exe without `/claude/`→false; case-sensitivity.
  - `mapping.decodeCWD` — non-`file://`→""; host-strip; `file:///`; percent-decode; trailing-slash trim; bad-escape→""; **`file://host`-no-path → returns host (characterization ⚠).**
  - `rpc.statusFromHookEvent` — full mapping table + unknown→"" (and confirm `unknown` is never emitted ⚠).
  - `rpc.pickSession` — pure over a slice already: "active"/""→focused-else-first; numeric PID-first-then-index; **PID-2-present vs absent collision (characterization ⚠);** out-of-range→nil.
  - `bottombar.shouldRun(topVisible, count)` — extract the `topVisible && count>0` core out of `apply`/`reconcileWith` into a pure func; assert all four F8 truth-table cases.
  DoD: `go test ./...` green; `go test -cover` ≥ these packages' pure logic.
- [ ] **0.4 Fixture / in-memory tests.**
  - `state`: Apply→broadcast+persist ordering; Snapshot sorts by StartedAt (and **pin tie-break ⚠**); subscriber buffer cap-4 **drops without blocking**; cancel closes channel; persist is atomic temp+rename, no `.state-*.json` litter on failure, no-op on empty path; Load no-op on missing, returns err on corrupt, hydrates by PID. Run under `-race`.
  - `hyprland.Subscribe` parsing: split on first `>>`; drop delimiter-less lines; 1 MiB buffer; channel closes on ctx-cancel/EOF — all against a fake conn (extract the parse loop to take an `io.Reader`).
  - `wezterm.Muxes`: temp `XDG_RUNTIME_DIR/wezterm` with `gui-sock-<pid>` entries; only numeric-pid + live-`/proc` kept (use the test's own pid as "live"); non-`gui-sock-`/dead skipped.
  - `bottombar`: `topVisible` (marker present/absent), `bottomPID` (stale pidfile cleanup, comm≠waybar guard), `envOr`/`runtimeDir`.
  DoD: green under `-race`.
- [ ] **0.5 Minimal testability seams (behavior-preserving).** Introduce injectable deps ONLY where 0.3/0.4 need them, with the existing concrete impl as the default so runtime behavior is identical:
  - `discovery.Scanner` takes a `procSource` (AllPIDs+Read) → unit-test the seen-set state machine: fire-once, `Forget` re-fires, error/non-claude never remembered, recycled-PID-without-Forget shadowed, callback runs lock-free.
  - `mapping.findHyprClient` split into a pure `matchUniqueClient([]Client, pid, title)` → test zero/one/exactly-two(ambiguous→nil)/both-keys-required.
  - `rpc.findTrackedAncestor` takes a `readProc` func → test self-match, ppid-walk, depth-20 bound (inspects depths 0..19 ⚠), pid≤1/err/ppid0 → 0.
  DoD: same binaries behave identically (smoke-run the daemon on the dev box); new unit tests green.
- [ ] **0.6 `procwatch` death-semantics tests (real processes).** Against real short-lived children: onDeath fires **exactly once** on kill; duplicate `Watch` is a no-op (no second fd/goroutine); `Stop` cancels **without** firing onDeath; already-dead PID (ESRCH path) fires onDeath immediately; `Watched()` excludes exited/ESRCH PIDs; EINTR doesn't fire. **Document the unhandled POLLERR-without-POLLIN spin as a known gap ⚠.** DoD: green under `-race`, no goroutine leaks (goleak or manual).
- [ ] **0.7 Cross-platform conformance suite** — *the scaling centerpiece.* Author a backend-agnostic contract suite, parameterized over an implementation, that today runs against thin adapters wrapping the Linux/Hyprland/wezterm concretes and **is reused verbatim in Phases 2–4** for sway/x11/tmux/macOS:
  - `osproc.Source` contract: enumerate returns claude procs with cwd + non-empty tty for an interactive child / empty tty otherwise (**assert tty non-empty, NOT the `/dev/pts/` prefix**); `Read` of a dead pid → `ErrGone`; `Exe`/`CWD` empty (not error) when unobtainable; `Watch` onDeath-exactly-once on any death cause.
  - `wm.Manager` contract: `Clients()` shape `{address, pid, title, workspace}`; `ActiveWindow`; `Focus` ok/err; event stream emits the canonical neutral names (`closewindow`/`activewindowv2`/`movewindowv2`/`windowtitlev2`/`openwindow`); **address normalization** — the value in an `activewindow` event compares equal to `Clients().Address` (pins the Hyprland `0x`-prefix quirk as a *seam responsibility*, the single most fragile cross-layer contract); `Available()` reports false cleanly when the WM isn't running.
  - `terminal.Locator` contract: `Locate(tty)` returns the owning pane with stable `(mux, pane)` identity; one failing endpoint doesn't blank healthy ones; dead endpoints skipped (no hang); `Activate` focuses.
  DoD: suite green against Linux/Hyprland/wezterm adapters; suite is exported/importable so a new backend gets coverage by satisfying the interface.
- [ ] **0.8 Hyprland-specific lifecycle regression tests** (no-regression for the existing appliance; stays a Hyprland-only extra, not portable core). Drive `reconcile`/`apply` against a fake session-count + marker and a stub launcher/killer:
  - the **four F8 truth-table cases** (intent items): top-hidden→bottom absent regardless of count; top-visible+0→absent; top-visible+≥1→present; **(top-hidden, bottom-present) is unreachable**.
  - daemon-unreachable → leave bottom as-is (no flap); start/stop idempotence; `ensureStopped` targets the **process group** (no orphans); the reaper prevents zombies across repeated start/kill cycles; self-heal restores within the 3 s tick. DoD: green; zombie/orphan assertions verified with a fake child tree.
- [ ] **0.9 CI matrix + decision register.** GitHub Actions: `go vet`, `go build ./...`, `go test -race ./...` on `linux/amd64` + `linux/arm64`; stub a `darwin` build-only job now (test job lands in Phase 4). Commit `docs/decisions.md` listing every ⚠ characterization item (0x-normalization, PID-vs-index selector, StartedAt sort tie, Reconcile stale-address-on-ambiguity, unpopulated `Monitor`, never-emitted `unknown`, `file://host` decode, write-once SessionID, corrupt-JSON-drops-all, POLLERR spin) with a verdict. **Resolved policy: pin-then-fix** — every ⚠ item first gets a characterization test capturing *current* behavior (so the suite is green on `main`), and is then corrected in a dedicated follow-up commit that flips the test to the intended behavior, *before* Phase 1 extracts that seam. Sequencing within Phase 0: `0x`-normalization and the `StartedAt` sort tie-break are fixed first (they are seam contracts the refactor depends on); the remainder follow in priority order. DoD: CI green on a PR; decision register reviewed; each ⚠ has a pin commit + a fix commit.

### Phase 1 — Interface extraction + `none` backends  (pure refactor, no behavior change on the dev box)
> Pre: **Phase 0 suite green on `main`** + decision register (0.9) resolved. This
> phase must not change observable behavior under Hyprland+wezterm — proven by
> the Phase 0 suite (incl. the 0.7 conformance suite) staying green after each
> extraction. Each interface defined here adopts the 0.7 contract suite directly.

- [ ] **1.1 Define Seam-1 interface.** New `internal/osproc` with `type Source interface { Enumerate() ([]Info, error); Watch(ctx, pid, onDeath) error; Stop(pid) }`. Move existing `/proc`+`pidfd` code behind `osproc_linux.go`. `discovery` and `procwatch` collapse into / call this. DoD: daemon builds with `osproc` injected; existing tests pass; `go vet` clean.
- [ ] **1.2 Define Seam-2 interface.** New `internal/terminal` with `type Locator interface { Locate(ctx, tty string) (*PaneRef, error); Activate(ctx, PaneRef) error; Name() string }`. Move wezterm behind `terminal/wezterm.go`. Add `terminal/none.go`. DoD: mapping calls the interface; wezterm path unchanged.
- [ ] **1.3 Define Seam-3 interface.** New `internal/wm` with `type Manager interface { Clients(ctx) ([]Window, error); ActiveWindow(ctx) (Ref, error); Focus(ctx, Ref) error; Subscribe(ctx) (<-chan Event, error); Name() string }`. `Ref` is opaque (interface or tagged string). Move Hyprland behind `wm/hyprland.go`; add `wm/none.go`. Translate Hyprland's raw socket2 events into the neutral `Event` enum here. DoD: daemon's `runHyprlandLoop`/`reconcileOnce` consume neutral events; Hyprland behavior identical.
- [ ] **1.4 Runtime detection + capability reporting.** New `internal/detect`: probe env and select `{osproc, terminal, wm}` backends. Daemon logs the chosen stack at startup and writes a `capabilities` block into `state.json` (`{observe:true, navigate:bool, wm:"hyprland", terminal:"wezterm"}`). DoD: on dev box, detection picks Hyprland+wezterm; forcing `none` backends via flags yields Observe-only with `navigate:false`.
- [ ] **1.5 Graceful Navigate degradation.** `rpc.focus` returns a clean "navigate unsupported on this stack" error instead of "no hyprland address". DoD: with `wm=none`, `focus` returns the typed error; `list`/`subscribe`/hooks unaffected.

### Phase 2 — WM backends  (biggest reach)
> Pre: Phase 1 merged. Each backend is independently shippable.

- [ ] **2.1 sway/i3 backend.** `wm/i3.go`: i3 IPC over `$SWAYSOCK`/`$I3SOCK`, magic-string binary framing. `get_tree`→windows (pid, name, workspace, con_id), `RUN_COMMAND [con_id=…] focus`, `SUBSCRIBE ["window","workspace"]`→events. Detection: `SWAYSOCK`/`I3SOCK` present. DoD: on a sway session, `list` shows correct workspaces; `focus` jumps; closing a window drops the chip; focus changes flip `Focused`.
- [ ] **2.2 X11/EWMH backend.** `wm/x11.go`: connect via Xlib/xcb (cgo) or a pure-Go X client. `_NET_CLIENT_LIST` + `_NET_WM_PID` + `_WM_NAME`; focus via `_NET_ACTIVE_WINDOW` ClientMessage; events via root-window `PropertyNotify` on `_NET_ACTIVE_WINDOW`/`_NET_CLIENT_LIST`. Detection: `DISPLAY` set and not Wayland. Decide cgo vs pure-Go (affects cross-compile — see Risks). DoD: under a stacking/tiling X11 WM, list+focus+focus-events work.
- [ ] **2.3 Detection precedence.** Define order when multiple are present (Wayland compositor wins over X11 when both signal). DoD: documented and unit-tested precedence table.

### Phase 3 — terminal backends
> Pre: Phase 1 merged.

- [ ] **3.1 tmux backend.** `terminal/tmux.go`: `tmux list-panes -a -F '#{pane_tty} #{pane_pid} #{session_name} #{window_id} #{pane_id} #{client_name}'`; match by `pane_tty`; focus via `select-window`+`select-pane` (and `switch-client` if detached). Note the tmux↔WM bridge: the WM window owning tmux is the tmux **client's** terminal — Navigate may need both the tmux focus *and* the WM focus of the client's terminal window. DoD: in a tmux session inside any supported terminal, chips resolve; focus selects the right pane.
- [ ] **3.2 kitty backend (optional/stretch).** `kitten @ ls` if `$KITTY_LISTEN_ON` set. DoD: deferred unless trivial.
- [ ] **3.3 Locator chaining.** Support "tmux pane lives inside a wezterm/foot window" by composing locators (tty→tmux pane, then tmux client tty→WM window). DoD: nested case documented; at least the tmux-in-X11 path works.

### Phase 4 — macOS OS backend
> Pre: Phase 1 merged. Opens the second platform.

- [ ] **4.1 darwin enumerate.** `osproc_darwin.go`: `proc_listallpids`/`KERN_PROC_ALL` + `proc_pidpath` (exe) + `proc_pidinfo`/`VNODE` for cwd; tty via `proc_pidinfo`. DoD: `Enumerate()` returns claude procs with cwd+tty on macOS.
- [ ] **4.2 darwin death watch.** kqueue `EVFILT_PROC` + `NOTE_EXIT`, one kevent per pid (mirror the pidfd-per-pid model). DoD: killing a claude proc fires `onDeath` once, promptly.
- [ ] **4.3 macOS terminal/WM reality check.** Aerospace/yabai for WM (if any), iTerm2/Terminal.app/wezterm/tmux for terminal. Decide what Navigate looks like on mac (likely tmux + yabai, else Observe-only). DoD: macOS runs Observe tier out of the box; document the Navigate matrix.

### Phase 5 — UI portability & docs  (the advertising payload)
> Pre: Phases 1–4 as far as shipped. Can start the docs subset right after Phase 1.

- [ ] **5.1 `state.json` contract doc** (done in 0.1, finalize here with examples per tier).
- [ ] **5.2 Reference TUI renderer.** `cmd/claude-tui`: reads RPC `subscribe`, draws a live list (count, cwd, status). Zero desktop deps — the demo that works in any terminal, even over SSH. DoD: `claude-tui` shows live sessions with no WM/bar.
- [ ] **5.3 Bar recipes.** `docs/bars/` with copy-paste polybar, eww, i3blocks, waybar configs that consume the same `status`/`pick` commands. DoD: at least polybar + eww recipes verified.
- [ ] **5.4 Install story.** Single `go install`; runtime detection means no per-WM build. Update README: capability matrix table, "works with X" badges, quickstart that reaches Observe tier in one command. DoD: README leads with the portable pitch; per-WM setup demoted to an appendix.

---

## Cross-cutting concerns

- **Detection must be cheap and side-effect-free.** Probe env vars + socket existence; never spawn or block. Wrong guesses degrade to a lower tier, never crash.
- **Every backend implements `none` semantics.** Missing tool → typed "unsupported," logged once, not per tick.
- **Opaque window `Ref`.** Don't leak Hyprland `0x…` strings into shared code; each WM owns its ref encoding. `state.json` may store a stringified ref but consumers treat it as opaque.
- **`comm == "claude"` stays the predicate** (16-char comm holds on both Linux and Darwin); the exe-path heuristic in `discovery.IsClaude` needs a Darwin-aware path (`/Applications/…` / homebrew / `~/.local`).
- **Testing seams:** each interface gets a fake backend so the daemon's orchestration is testable without a live WM. Behavior tests named "should … when …".

## Risks / open questions

- **X11 cgo vs pure-Go.** cgo (Xlib) breaks easy cross-compilation and static binaries; a pure-Go X client (e.g. `xgb`/`xgbutil`) keeps `go install` clean. **Lean pure-Go** unless a blocker appears. *(decision needed in 2.2)*
- **macOS cgo for libproc.** Some `proc_*` calls need cgo; acceptable since macOS builds run on macOS. Confirm kqueue path is cgo-free (it's syscall-based via `x/sys/unix`).
- **tmux↔WM focus bridge** is the trickiest Navigate composition (Phase 3.3). May ship tmux as Observe-enhancing first, full Navigate second.
- **Wayland focus-stealing policies** (sway/Hyprland may reject programmatic focus depending on config). Document the activation-token caveat.
- **Two-bar auto-hide** (`bottombar`) is deeply Hyprland-specific; keep it as a Hyprland-only extra, not part of the portable core.

### Fragile contracts surfaced by the Phase-0 study (pin before refactor)

These are the cross-layer contracts most likely to silently break during the
seam extraction. Each has a characterization test (0.7/0.9) and a verdict in
`docs/decisions.md`:

- **Address `0x`-prefix normalization** *(highest risk).* socket2 emits window
  addresses without `0x`; `hyprctl clients` stores them with `0x`; `main.go`
  reconstructs `"0x"+Data` at the event boundary. The WM seam must own this
  normalization so non-Hyprland backends produce already-comparable refs.
- **PID-vs-index `focus` selector collision** — selector `2` means PID 2 if such
  a session exists, else index 2. Same input → different session by state.
- **`Snapshot` sort tie-break** — sort is by `StartedAt` only and not stable;
  equal timestamps make `pickSession` index/`sessions[0]` nondeterministic. The
  selector contract depends on a deterministic order → pin a PID tie-break.
- **`Reconcile` keeps a stale WM address on ambiguity** — intentional "retry
  next tick," but unwritten; a moved/closed window can hold a stale address.
- **Snapshot shares pointer fields** (Wezterm/Hyprland/Claude) — read-only by
  convention only; a backend mutating through a snapshot would race the store.
- Minor: `Monitor` never populated; `unknown` status never emitted though
  documented; `file://host`-no-path decodes to the hostname; SessionID
  write-once; corrupt-JSON `Load` drops all sessions; `procwatch` POLLERR
  -without-POLLIN can spin without firing onDeath.

## Definition of done (project-level)

0. **Phase 0 behavioral suite exists and is green**, including the reusable 0.7 conformance suite — and stays green through every later phase. No backend is "done" until it passes the conformance suite.
1. `go install ./...` on a fresh Linux box with no Hyprland → daemon runs, `state.json` populates, `claude-tui` shows sessions. (Observe tier, zero config.)
2. On Hyprland+wezterm, sway+tmux, and X11+tmux → Navigate (focus) works.
3. On macOS → Observe tier works out of the box.
4. README leads with the portable pitch + capability matrix; per-environment setup is an appendix.
