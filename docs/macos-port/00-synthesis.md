# macOS port — hardware findings and the cross-platform data model

> Written on real macOS hardware (14.3.1, arm64, Apple Silicon, unprivileged,
> ad-hoc-signed binaries) — the thing `docs/phase4/README.md` says Phase 4 was
> blocked on. Every claim below is tagged **[verified]** (executed or read in
> this tree) or **[inferred]** (reasoned, not measured). Supporting detail:
> [01-process-model.md](01-process-model.md), [02-window-manager.md](02-window-manager.md),
> [03-terminal-layer.md](03-terminal-layer.md).

## 0. Headline

**The existing abstraction is sound and does not need a rewrite.** The four
seams in [portability-plan.md](../portability-plan.md) — process, terminal, WM,
UI — survive contact with macOS. `osproc.Source`, `wm.Manager`,
`terminal.Locator` and the `internal/conformance` contract suites are the right
shape, and `GOOS=darwin go build ./...` already passes.

What the port actually needs is three model changes and a pile of small
mechanism fixes. The model changes are the interesting part, and each one is
forced by a *measured* macOS fact rather than by taste:

| # | Change | Forced by |
|---|--------|-----------|
| M1 | Process identity becomes `(pid, StartToken)` | pid reuse; `StartedAt` is currently discovery-time, not process-birth |
| M2 | Binary `!= "none"` availability becomes a graded **capability + permission** model | macOS backends are *partially* capable; permission is a first-class state |
| M3 | `PaneRef.Mux int` splits into `MuxKey` + `WindowHint` | tmux structurally cannot reach the WM join today — on Linux too |

And one product fact that outranks all of it: **switchboard on macOS today finds
zero sessions**, and would still find zero with a perfectly correct darwin
process backend. See §1.

## 1. The blocker that outranks everything: `comm` is not the agent's name

`docs/phase4/README.md` names this the "single load-bearing assumption for
discovery" and asks: does `claude` appear as `comm == "claude"` on macOS, or as
`node`? It guesses `claude`.

**[verified] Neither. It is `2.1.259` — the version string.**

```
$ ps -Ao pid,ucomm,comm | grep claude
96956 2.1.259          claude
$ ls -l ~/.local/bin/claude
… -> /Users/…/.local/share/claude/versions/2.1.259
```

`~/.local/bin/claude` is a symlink into a versioned directory, and macOS derives
`p_comm` from the **resolved** binary's basename.

**[verified] The bug is layout-dependent, not platform-universal.** A controlled
experiment with two symlinked binaries settles both halves of the mechanism:

```
LAYOUT         p_comm       argv[0] basename
claude-style   9.9.9        claudelike      <- version-named FILE  -> broken
cask-style     codex        casklike        <- version-named DIR   -> fine
```

It fires only when the installer names the binary FILE after the version, as
claude's native installer does. A Homebrew cask puts the version in a DIRECTORY
component (`…/Caskroom/codex/0.152.1/bin/codex`), so the resolved basename is
still `codex` and comm reads correctly — codex installed that way was never
affected. This is why the predicate must be layout-independent rather than
enumerate known install shapes.

The same experiment shows **`argv[0]` is the SYMLINK's name, not the resolved
target's** — which is exactly why it survives where comm does not, and why it is
the right identity signal. **[inferred]** Linux uses the
path as passed to `execve`, symlink unresolved, so the same install reads
`claude` there — worth 30 seconds on a Linux box to confirm this is a genuine
platform divergence and not a latent bug on both.

`IsClaude` hard-gates on it (`internal/discovery/discovery.go:43`), so **no
amount of correct darwin process code surfaces a single session.**
`IsCodex` (`discovery.go:191`) has the structurally identical gate.
**[verified]** no libproc flavor rescues this — `PROC_PIDTBSDINFO`'s wider
`pbi_name` reads `2.1.259` too.

The fix is to stop treating `comm` as identity. It is a per-platform artifact of
how a kernel derives a process name, and the surviving signals are `argv[0]` and
the exe path:

```go
// namedClaude accepts any of the three places the name survives. macOS sets
// p_comm from the RESOLVED binary, so a versioned-symlink install reports
// comm "2.1.259"; argv[0] and the exe basename still say claude.
func namedClaude(p osproc.Info) bool {
	if p.Comm == "claude" {
		return true
	}
	if len(p.Args) > 0 && filepath.Base(p.Args[0]) == "claude" {
		return true
	}
	return p.Exe != "" && strings.Contains(p.Exe, "/claude/")
}
```

This is Linux-testable and should land as its own PR ahead of any darwin code —
it is exactly the "Linux-doable prep" the Phase 4 README already sequences first.

## 2. M1 — process identity is `(pid, StartToken)`

`internal/provider/provider.go:25-35` already declares `RootKey{PID, StartedAt}`
"the PID-reuse-safe identity". But `StartedAt` is `time.Now()` at *discovery*
(`internal/mapping/mapping.go:62`), and `internal/state/state.go:22-26` openly
disclaims it as "not a kernel process-birth token". So the reuse defence keys on
*when the daemon noticed*, which a daemon restart perturbs.

Every platform hands us the real token for free:

| Platform | Token | Cost |
|---|---|---|
| darwin | `kinfo_proc.Proc.P_starttime` | **[verified]** free — already in the `KERN_PROC_ALL` record we enumerate |
| linux | `/proc/PID/stat` field 22 + `/proc/stat` btime | one read we mostly already do |
| windows | `GetProcessTimes().CreationTime` | **[inferred]** |

Keep it an **opaque comparable `StartToken uint64`**, never a formatted
`time.Time`: Linux's is boot-relative and coarse, and it must never be compared
across hosts — which matters here because switchboard federates over SSH. Add it
as a new field; do **not** repurpose `state.Session.StartedAt`, which is on the
wire and in `state.json`.

## 3. M2 — capability and permission replace `!= "none"`

Today the entire Navigate tier is one boolean over two strings:

```go
// internal/detect/detect.go:117
Navigate: s.Terminal.Name() != "none" && s.WM.Name() != "none",
```

This is wrong in both directions, and macOS makes both failures load-bearing:

- **False negatives.** Alacritty is single-pane — there is no pane to activate,
  so it needs no terminal backend, yet a WM raise is the complete answer and the
  session is fully navigable. The `&&` denies it.
- **No room for partial.** A macOS backend may enumerate windows and read titles
  but only raise the *app*, not the window. There is nowhere to say that, so it
  silently no-ops instead of saying "install AeroSpace" or "grant Accessibility".

Permission is the axis that has no representation at all today, and on macOS it
is the single likeliest reason a jump does nothing — and the one the user can
actually fix. It must be a **first-class state, not a generic error**.

```go
type Capability uint16

const (
	CapEnumerate       Capability = 1 << iota // Clients() returns a real window list
	CapWindowPID                              // Window.PID is a real OS pid
	CapWindowTitle                            // Window.Title is populated
	CapActiveWindow                           // ActiveWindow() is meaningful
	CapFocusWindow                            // Focus(ref) raises THAT window
	CapFocusApp                               // Focus(ref) raises only the owning app
	CapWorkspaceRead                          // Workspace / WorkspaceID are real
	CapEventStream                            // Subscribe() delivers a live stream
	CapEventWindowClosed
)

// CapJoin is the minimum for the pid+title join in internal/mapping. A backend
// missing any of these can never resolve a session to a window.
const CapJoin = CapEnumerate | CapWindowPID | CapWindowTitle

type Grant uint8

const (
	GrantNotRequired Grant = iota
	GrantGranted
	GrantDenied  // user said no, or revoked it
	GrantUnknown // needs asking; surface a prompt affordance
)

type Permission struct {
	Name  string // "Accessibility", "Screen Recording"
	State Grant
	Hint  string // "System Settings > Privacy & Security > Accessibility, then restart"
}

// Status keeps availability, capability and permission as three DIFFERENT axes.
// Collapsing them is what the "none" string gets wrong: a backend can be
// Available, declare CapFocusWindow, and still be GrantDenied.
type Status struct {
	Name        string
	Available   bool
	Caps        Capability
	Permissions []Permission
}

func (s Status) Can(c Capability) bool { return s.Caps&c == c }
```

Add one additive `Status() Status` method to `Manager` and `Locator` — cheap and
side-effect-free like `Available()`, and **never cached at construction**, since
the user can toggle a TCC grant while we run. Two sentinels join `ErrUnsupported`
(`internal/wm/wm.go:56`):

```go
// ErrPermissionDenied is a fixable, user-actionable state, not a failure.
var ErrPermissionDenied = errors.New("wm: required OS permission not granted")

// ErrAppLevelOnly: raised the owning app but not the specific window. Non-fatal
// — the caller still activates the pane and reports an approximate jump.
var ErrAppLevelOnly = errors.New("wm: raised the application, not the window")
```

`state.Capabilities` then publishes the permission list so **any** bar can render
"grant Accessibility" — which keeps the fix in the UI tier where it belongs, and
costs the Linux bars nothing.

The `!= "none"` comparison appears at `rpc.go:555`, `rpc.go:589`,
`navigator.go:69`, `navigator.go:107`, `navigator.go:203`, `detect.go:117`.

## 4. M3 — split `PaneRef.Mux`, and give tmux a window

**[verified]** `internal/terminal/tmux.go:100-110` never sets `Mux`, and
`internal/mapping/mapping.go:85` and `:213` gate the whole WM join on
`pane.Mux != 0`. The code says so in its own comment:

```go
// (e.g. tmux, whose pane pid is the in-pane process) leave Mux 0 and the
// session stays Observe-only on the WM axis.
```

So **tmux sessions never acquire a window address — on Linux today, not just
macOS.** `docs/phase4/04-navigate-matrix-macos.md` builds its macOS
recommendation on tmux giving Navigate "today, zero new code"; it gives *pane*
selection with no window raise. On stock macOS with no WM backend, that means a
jump is invisible whenever the terminal is backgrounded.

The root cause is a type overload: `Mux` is documented as "the mux pid" but is
used as "the pid the WM will report". For WezTerm those coincide; for tmux they
never do. Split them:

```go
type PaneRef struct {
	Backend string
	Handle  string // opaque, backend-scoped pane id
	MuxKey  string // opaque mux instance identity (was: Mux int)
	TTY     string
	CWD     string
	Window  WindowHint // what the WM seam needs to find the OS window
}

// WindowHint is the terminal seam's contract WITH the WM seam: everything the
// terminal knows that could identify the hosting OS window, and nothing about
// how any particular WM finds it.
type WindowHint struct {
	OwnerPID int    // pid the WM will report as the window owner; 0 if unknown
	Marker   string // exact-match token, e.g. "[sbw:<gui-pid>:<window-id>]"
	Title    string // fallback, normalized
}
```

tmux can then fill `OwnerPID` via a genuine two-stage resolution:
`tmux list-clients -F '#{client_tty} #{client_session}'` → attached client's tty
→ process layer → the hosting emulator's pid. That is real engineering, not a
config change — and it fixes the identical gap on Linux, which is why it is the
highest-value item after the two three-line WezTerm fixes below.

## 5. Mechanism fixes: neutral observable replaces platform mechanism

Each of these is a place where a Linux mechanism leaked into supposedly-portable
code. All are small; two are three-liners that resurrect a whole seam.

| Leak | macOS replacement | Status |
|---|---|---|
| `os.Stat("/proc/<pid>")` liveness, `internal/wezterm/wezterm.go:75` | `unix.Kill(pid, 0)`, tolerating `EPERM` | **[verified]** the fix already exists in-repo as `pidAlive`, `internal/hyprland/hyprland.go:352-356`, whose comment calls it "the portable analogue of `[ -e /proc/<pid> ]`" |
| `socketDir()` returns "" without `$XDG_RUNTIME_DIR`, `wezterm.go:223-228` | WezTerm on macOS ignores XDG entirely and uses `$HOME/.local/share/wezterm` | **[inferred]** from wezterm#6737; wezterm not installed here |
| `readTTY` readlinks `/proc/<pid>/fd/{0,1,2}`, `internal/proc/proc.go:124-138` | `fcntl(fd, F_GETPATH)` | **[verified]** by pty probe |
| hardcoded `/dev/pts/` prefix | `/dev/ttysNNN` | **[verified]** |

**[verified] The tty join key survives.** A real pty probe agreed across
`TIOCPTYGNAME`, `fcntl(F_GETPATH)` and `tty(1)`: all yield `/dev/ttys001`, and
tmux's `#{pane_tty}` comes from `ttyname()` on the same pty. **[inferred]**
WezTerm's `tty_name` is very likely the same form but is *unconfirmed* — and the
join breaks silently if it isn't, so check it on a machine with WezTerm.

**Footgun for whoever writes darwin osproc:** the other natural route, libproc
`PROC_PIDTBSDINFO` → `e_tdev` → `devname(3)`, yields bare `"ttys001"` with **no
`/dev/` prefix**. That string fails every join silently.

### Two corrections to earlier assumptions

- **`LOCAL_PEERPID` works — the socket peer check is portable. [verified]** The
  "macOS `xucred` carries no pid" fact is true of `LOCAL_PEERCRED` only. A
  two-process probe over a real AF_UNIX socket read the peer's pid exactly as
  Linux `SO_PEERCRED` does. No redesign needed. (For future BSD/Windows, declare
  authority rather than assume it: `PeerAuthority{PeerUnknown|PeerUID|PeerPID}`.)
- **`internal/wezterm`'s three test failures are one root cause, and
  `socket_peer_darwin.go` is a real, correct implementation — merely
  unreachable. [verified]** The `/proc` liveness stat makes `Muxes()` return nil
  unconditionally, so the injected fake peer fn is never reached.

### The RPC failure is the test harness, not the design

**[verified]** macOS `sun_path` is **103 usable bytes** (104 with NUL); measured
by sweep, `len=103` binds and `len=104` fails `EINVAL`. `t.TempDir()` embeds the
test name, and `TestSubscribe_deliversCurrentWireFrameToEverySubscriber` is 54
characters, producing a ~124–135 byte socket path. Linux never reaches this: 108
bytes and a short `/tmp`.

Production paths are safe (`/tmp/switchboard-<uid>.sock`, ~28 bytes). The fix is
a shared short-path test helper. **Latent trap:** `testsupport/runtimedir.go:23`
points `XDG_RUNTIME_DIR` at `t.TempDir()`, so any future test binding a socket
there hits the same wall.

## 6. cgo: decision D1 is now questionable

`docs/phase4/README.md` locks **D1 = use cgo**, on the grounds that cwd and exe
are only reachable through libproc. That in turn forces **D2**, a macOS CI
runner with `CGO_ENABLED=1`, because cgo-darwin cannot cross-compile from Linux.

**[verified] cwd works unprivileged, from pure Go.** `PROC_PIDVNODEPATHINFO`
returned cwd for 330 of 563 processes — exactly the own-uid set
(`kern.proc.uid` reports 330) — with `EPERM` only for foreign uids and `ESRCH`
after reap. The "needs root" folklore comes from `lsof`, which needs root
because it walks *all* processes.

**But do not reopen D1 on that alone.** The pure-Go route reaches `proc_pidinfo`
via `syscall.Syscall(336, …)` through a libSystem shim Apple deprecated in
10.12, verified only on 14.3.1/arm64, with **hand-computed struct offsets**
(`152 + 1024` per `vnode_info_path`) validated on arm64 only. Before rewriting
D1 it needs: proof on macOS 15/26, amd64 offset verification, and a startup
self-test that degrades to the existing `pane.CWD` fallback
(`internal/mapping/mapping.go:78-80`) rather than returning wrong paths.

Keeping the WM and terminal backends **pure Go is a design rule, not an
accident** — `docs/phase4/03-ci-and-cgo.md:16` treats it as incidental. A native
AX/CGWindowList backend would break `GOOS=darwin go build ./...` from Linux.

## 7. Navigate on macOS: what is actually achievable

**[verified]** enumeration cost is a non-issue: `KERN_PROC_ALL` returns pid,
ppid, comm, tdev, uid and start-time for all 563 processes in **122–562 µs** —
one syscall, richer *and* cheaper than Linux's `AllPIDs`, which returns bare pids
and charges a read per field. The existing "enumerate cheap, hydrate only unseen"
design fits both platforms unchanged.

The constraint is windows, not processes:

- **[verified] Screen Recording is required merely to read window *titles*, and
  it fails silently.** Since 10.15, `CGWindowListCopyWindowInfo` omits
  `kCGWindowName` without the grant — no dialog, the key is just absent. Since
  one WezTerm GUI pid owns N windows, pid alone is never unique and the
  `[sbw:<gui-pid>:<window-id>]` **title marker is the join**. So CGWindowList
  cannot serve it.
- **[verified] Titles are available via the Accessibility API's
  `kAXTitleAttribute` with no Screen Recording at all** — and AeroSpace already
  reads them that way and hands them over its CLI.
- Focusing a *specific* window without private APIs needs the Accessibility TCC
  grant, which requires cgo and — because TCC pins the grant to the code-signing
  requirement — **evaporates on every rebuild** for ad-hoc-signed binaries. Every
  permission-free alternative (`NSRunningApplication.activate`, `open -a`,
  `osascript … to activate`) is **app-level only**.

**This is the whole argument for the AeroSpace backend:** it is the only route
that satisfies the title half of the join without switchboard holding a
permission that either nags (Screen Recording) or dies on rebuild
(Accessibility). We only exec its CLI — pure Go, no cgo, no TCC grant, no SIP
changes.

**[verified] `aerospace subscribe` exists** (docs present at tag v0.21.3-Beta,
absent at v0.20.3-Beta), so AeroSpace satisfies the full `wm.Manager` contract
including `Subscribe`. It is pre-1.0, so pin the `--format` field names and
version-probe, degrading to Observe rather than mis-focusing.

### Tiers

| Configuration | Fidelity |
|---|---|
| AeroSpace + WezTerm (no tmux) | **window + pane** — full |
| AeroSpace + tmux | pane only, until M3 lands |
| Stock macOS, any terminal | pane only; no window raise without a grant |
| iTerm2, any WM | **window + pane** — uniquely, `async_activate(order_window_front=True)` raises the window itself, needing no WM backend |

Terminals ranked by achievable fidelity: **tmux** (works today, emulator-
independent) → **WezTerm** (after the two fixes) → **iTerm2** (self-raising) →
**kitty** (exposes no tty; join must move to pid) → **Ghostty** (no tty, no pid —
**no sound join key**, Observe-only) → **Terminal.app** (tab-level, Observe-only).

Recommended default: **tmux on any emulator + AeroSpace**, with the M3 fix as the
prerequisite that makes it actually mean window-level.

**Do not build a native AX backend for stock macOS.** cgo, a TCC grant flow, and
breakage on every `go install`. The existing phase4 conclusion holds.

## 8. Corrections owed to the committed plan docs

- `phase4/README.md` — the `comm == "claude"` assumption (§1). Refuted.
- `phase4/04-navigate-matrix-macos.md:56,64,124` — "AeroSpace has no streaming
  event API", "`Subscribe` must be poll-based". Wrong as of v0.21.x.
- `phase4/04-navigate-matrix-macos.md:20,31` — tmux "works today, zero new code"
  / "Yes — pane-level, today" under a *Navigate* column the doc defines at :10 as
  window **+** pane. Needs splitting into pane-level vs window-level.
- `phase4/04-navigate-matrix-macos.md:78` — "AeroSpace + tmux is the recommended
  Navigate combo". With tmux the window is never raised; correct pairing today is
  AeroSpace + WezTerm-without-tmux.
- `phase4/04-navigate-matrix-macos.md:89,115` — the "identical two-stage handoff,
  no new macOS wrinkle" and the end-to-end DoD. Stage one is skipped entirely for
  tmux refs; not achievable as written without M3.
- `phase4/04-navigate-matrix-macos.md:21` — WezTerm "works today". It is dead on
  macOS twice over (§5).
- `phase4/04-navigate-matrix-macos.md:53` — AeroSpace workspace names are
  arbitrary strings, so `WorkspaceID` lands 0 and
  `internal/federation/workspace.go:92-97` discards the row for chip ordering.
- `portability-plan.md` Seam 3 / Navigate-tier — encodes the binary `"none"`
  model; macOS partial capability has nowhere to live (§3).
- `phase4/03-ci-and-cgo.md:16` — pure-Go WM backends described as incidental;
  should be a stated design rule (§6).

## 9. Sequencing

Each phase is independently landable and leaves Linux green.

1. **Linux-only, no Mac needed.** Fix the `comm` gate (§1) for `IsClaude` and
   `IsCodex`; add `StartToken` (M1); short-path test helper for sockets (§5).
   Unblocks discovery on macOS *and* hardens pid-reuse on Linux.
2. **Linux-only, still no Mac.** The capability model (M2). Pure refactor of six
   `!= "none"` sites; adds the permission axis to `state.Capabilities`. Do this
   before any macOS backend so the backend has somewhere to report partial
   capability.
3. **Linux-testable, fixes a real Linux gap.** M3 — split `PaneRef.Mux`, resolve
   tmux's hosting emulator pid. Gives tmux window-level Navigate on both
   platforms.
4. **On the Mac, one PR** (the moment darwin uses cgo, Linux can no longer build
   darwin, so backend + CI + test-support must arrive together):
   `child_darwin.go` pty helper, `osproc/source_darwin.go` (enumerate + kqueue
   watch), the blocking `test (darwin/arm64)` CI job.
5. **The two WezTerm three-liners** (§5) — `pidAlive` and the macOS socket dir.
6. **Opt-in Navigate:** `wm/aerospace.go` with `subscribe`-backed events.

## 10. Open questions for the human

1. **Seam 4 has no macOS answer.** polybar/waybar are Linux-only. `cmd/claude-tui`
   already exists and is platform-neutral. Options: TUI-first (no new platform
   code, working switcher soonest), SketchyBar, or a native menu-bar app
   (NSStatusItem → cgo + signing). **Recommendation: TUI-first.**
2. **Is requiring AeroSpace acceptable for the headline macOS experience?** Full
   window-level Navigate needs it. **Recommendation: ship stock macOS as
   pane-level with an honest capability report, AeroSpace as the opt-in upgrade.**
   Requiring a third-party tiling WM to jump between sessions is a hard sell for
   the market this targets.
3. **Reopen D1 (cgo)?** **Recommendation: no, not yet** — verify the pure-Go
   route on macOS 15/26 and amd64 first (§6).
4. **Does this Mac get a WezTerm + AeroSpace + tmux install?** Currently it has
   none of them, so the entire Navigate tier is untestable here. Observe-tier
   work needs nothing.

## 11. Verification status

**Verified on hardware:** `p_comm` = version string and the symlink layout; cwd
unprivileged 330/563 = own-uid set; `KERN_PROC_ALL` 122–562 µs/563 procs;
`P_starttime` present; `sun_path` = 103 usable bytes and the RPC root cause;
`LOCAL_PEERPID` returns the peer pid across two processes; darwin tty is
`/dev/ttysNNN` via `F_GETPATH` and `os.Readlink("/dev/fd/N")` fails; all three
`internal/wezterm` failures trace to the `/proc` stat; `aerospace subscribe` docs
present at v0.21.3-Beta / absent at v0.20.3-Beta; every file:line in this
document.

**Not verified — treat as open:** the Linux `comm` mechanism (no Linux box); whether a real
codex TUI is discovered end to end (codex is now installed but was not run); `syscall(336)` ABI
durability beyond 14.3.1 and amd64 struct offsets; kqueue at ~20 pids;
**WezTerm's macOS `tty_name` form and socket dir** (not installed); all
iTerm2/kitty/Ghostty/Terminal.app behavior (vendor docs only); Windows
throughout.
