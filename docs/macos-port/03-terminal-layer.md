# The Terminal / Multiplexer Layer on macOS

Research report — the terminal/mux dimension of the macOS port.
Everything below is verified against the code at `/Users/jentachibana/Documents/Goose/switchboard`
by reading, by running the failing tests, and by executing standalone probes on this Mac.
Items that are documentation-only or inferred are explicitly flagged in section 7.

---

## 1. How a session is bound to a pane

There are **two independent binding paths** in this codebase, and only one of them is the general one.

### Path A — the tty join (the primary, general mechanism)

The identity chain, end to end:

```
agent process (pid)
  └─ osproc.Source.Read(pid) ──────────► Info.TTY          "/dev/pts/N"
       (linux: /proc/<pid>/fd/{0,1,2} readlink)
          │
          ▼  join key = the tty string, verbatim
     terminal.Locator.Locate(ctx, tty) ─► terminal.PaneRef {Backend, Handle, Mux, MuxSocket, PaneID, WindowID, WindowTitle}
          │
          ▼  join key = (PaneRef.Mux == wm.Window.PID) AND (marker or title match)
     wm.Window {Address, PID, Title, Workspace}
          │
          ▼
     wm.Manager.Focus(Address)  +  terminal.Locator.Activate(PaneRef)
```

Citations:

- tty acquisition: `internal/proc/proc.go:124-138` (`readTTY`, readlinks `/proc/<pid>/fd/{0,1,2}`, hardcoded `/dev/pts/` prefix), reached via `internal/osproc/source_linux.go:33-40`. The neutral contract explicitly declares TTY opaque and OS-specific (`internal/osproc/osproc.go:19-21`).
- tty → pane: `internal/terminal/terminal.go:49-50` (`Locate(ctx, tty)`), implemented for wezterm at `internal/terminal/wezterm.go:26-36` → `internal/wezterm/wezterm.go:180-191` (`FindByTTY` linear-scans `Pane.TTYName`), and for tmux at `internal/terminal/tmux.go:46-66` (matches `#{pane_tty}`, format string at `tmux.go:129`).
- pane → OS window: `internal/mapping/mapping.go:258-272` (`matchUniqueClient`). Two joins, tried in order: the exact marker `[sbw:<gui-pid>:<window-id>]` suffix on the WM title (`internal/panebind/types.go:117-119`), then a legacy `pane.Mux == client.PID && normalizeTitle(titles) match`. Both fail closed on ambiguity.
- The gate: `internal/mapping/mapping.go:85, 213` — `if pane.Mux != 0`. tmux leaves `Mux == 0` (`internal/terminal/tmux.go:102-110` never sets it), so **tmux sessions never get a WM address at all**. This is documented as deliberate at `internal/terminal/tmux.go:19-22`.
- Focus dispatch: `internal/rpc/rpc.go:596-618`. WM raise first, then re-`Locate` from the tty and `Activate`. Note it re-locates rather than using persisted pane fields — deliberately backend-agnostic.

**No env var participates in this path.** `WEZTERM_PANE` / `TMUX_PANE` are collected only as bounded,
non-authoritative "hints" for hook attribution (`cmd/switchboard-ctl/main.go:728-729`), and
`hookClientHints`' own doc comment says they "do not authorize attribution" (`main.go:704-706`).
`WEZTERM_UNIX_SOCKET` is only ever *written* into a child env, never read
(`internal/wezterm/wezterm.go:150, 200`).

### Path B — the OSC user-var binding (federation only)

`internal/panebind` is a *separate*, narrower seam for correlating a **remote** session to a
**local WezTerm pane**. It does not use tty as the join:

1. Go writes `OSC 1337 ; SetUserVar=SWITCHBOARD_SESSION=<base64 json>` to the target tty
   (`internal/panebind/osc.go:10, 43-65`; write path `tty_unix.go:30-46`).
2. WezTerm's Lua `user-var-changed` handler fires and shells out to
   `switchboard-ctl pane-bind <payload> <GUI_PID> <window_id> <pane_id>`
   (`integrations/wezterm/switchboard.lua:195-202, 246-257`).
3. The daemon stores `LocalPaneRef{GUIPID, WindowID, PaneID}` (`internal/panebind/types.go:99-106`)
   keyed by the exact session (`internal/panebind/registry.go:68-104`).
4. At action time it re-validates through `wezterm.ListGUI(guiPID)` (`internal/panebind/local.go:42-63`)
   and matches exactly one WM client bearing the `[sbw:pid:winid]` marker (`local.go:98-115`).

This path is **100% WezTerm-specific** (the Lua integration, `wezterm.GLOBAL`,
`wezterm.procinfo.pid()`), and it is the only consumer of `wezterm.ListGUI` and hence of the
socket-peer check.

### The darwin tty — verified

`readTTY` (`internal/proc/proc.go:124-138`) does not port: `/proc` does not exist, and on macOS
`/dev/fd/N` is a real device node, not a symlink, so `os.Readlink` fails outright. I confirmed this
by allocating a real pty (`/dev/ptmx` + `TIOCPTYGRANT`/`TIOCPTYUNLK`/`TIOCPTYGNAME`) and probing the
slave fd:

```
TIOCPTYGNAME(master)      = "/dev/ttys001"
fcntl(F_GETPATH) on slave = "/dev/ttys001"
child `tty` reports       = "/dev/ttys001"
slave st_rdev             = 268435457   (major=16 minor=1)
os.Readlink(/dev/fd/N)    = err: readlink /dev/fd/4: invalid argument
```

So the darwin equivalent is **`fcntl(fd, F_GETPATH)`**, applied to fds 0/1/2 exactly as `readTTY`
already does, and it yields the identical string form (`/dev/ttysNNN`, `/dev/`-prefixed) that
`tty(1)` reports. The `/dev/pts/` prefix test at `proc.go:133` must become a platform-appropriate
predicate.

One footgun for whoever writes the darwin `osproc` backend: the *other* natural implementation,
libproc `proc_pidinfo(PROC_PIDTBSDINFO)`, yields `e_tdev` as a raw `dev_t` which `devname(3)`
renders as `"ttys001"` — **without** the `/dev/` prefix. That string would silently fail the join
against every mux. Whichever route is chosen must produce the `/dev/`-prefixed form.

---

## 2. Socket peer credentials

### What it defends

`internal/wezterm/wezterm.go:107-136`. `ListGUI(ctx, guiPID)` enumerates exactly one
`gui-sock-<pid>`. The threat is **PID reuse**: a stored `LocalPaneRef.GUIPID` names a WezTerm GUI
that has since exited; a *different* process later gets that pid; a stale-or-hostile
`gui-sock-<pid>` file is still on disk. Without the check, `Muxes()` sees a live `/proc/<pid>`,
`listOne` connects, and switchboard would enumerate and then *activate a pane in a GUI process that
is not the one the binding was made against* — crossing pane-id namespaces, which the package doc at
`wezterm.go:1-5` says must never happen.

The check therefore asserts: *the process actually listening on `gui-sock-N` is pid N.* The error is
deliberately distinguished from transient failures at `wezterm.go:123-131` and consumed as
definitive stale-route evidence at `internal/panebind/local.go:65-73`.

### The darwin implementation is real and correct — verified at runtime

`socket_peer_darwin.go:32` uses `getsockopt(SOL_LOCAL, LOCAL_PEERPID)`. The common belief that macOS
cannot supply a peer pid is true of `LOCAL_PEERCRED` specifically — its `xucred` carries uid/groups
and no pid — but **macOS has a separate `LOCAL_PEERPID` sockopt** and it is exactly the right tool.

- Constants exist in the vendored `x/sys`: `LOCAL_PEERCRED = 0x1`, `LOCAL_PEERPID = 0x2`,
  `SOL_LOCAL = 0x0` (`golang.org/x/sys@v0.27.0/unix/zerrors_darwin_arm64.go:935,938,1373`).
- I wrote a **two-process** probe (separate server and client binaries, `AF_UNIX` socket in `/tmp`).
  Result: client pid 15671, server pid 15670, `LOCAL_PEERPID` read from the **client** socket
  returned **15670**. It returns the *peer's* pid, with exactly Linux `SO_PEERCRED` semantics.
  A single-process test would have been ambiguous; this one is not.
- Also confirmed `LOCAL_PEERCRED` on the same socket returns `uid=501 gid=20` and no pid, as expected.

**Verdict: the security check IS portable to macOS, `socket_peer_darwin.go` is not a stub, and it
needs no redesign.** The three test failures have nothing to do with it.

### Why the three tests actually fail — `/proc`

`internal/wezterm/wezterm.go:75`:

```go
if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err != nil {
    continue
}
```

There is no `/proc` on macOS, so **every** candidate socket is skipped and `Muxes()` returns
`nil, nil` unconditionally. Cascade:

- `TestMuxesKeepsOnlyLiveGuiSockets` (`muxes_test.go:42`) — got 0, want 1. Direct.
- `TestListGUIRejectsSocketWhosePeerDoesNotMatchFilenamePID` and
  `TestListGUIPreservesTransientPeerInspectionError` — `listGUI` loops over `Muxes()`
  (`wezterm.go:121`), finds nothing, falls through to `return nil, nil` at `wezterm.go:135`.
  The injected fake peer function is **never called**. Both tests get `err = nil`.

Note the shape of the bug: it fails *open* on enumeration (returns nothing), which means `ListGUI`
returns `(nil, nil)` where it should return a typed error. `lookupSystemPane` (`local.go:42-63`) then
maps that to `ErrPaneNotFound` — safe, but it makes the whole panebind path permanently dead on
macOS rather than degraded.

**The fix already exists in-repo**: `internal/hyprland/hyprland.go:352-356` has `pidAlive(pid)` using
`unix.Kill(pid, 0)` and tolerating `EPERM`, described in its own comment as "the portable analogue of
`[ -e /proc/<pid> ]`". `wezterm.go:75` should call that.

### A second, larger darwin bug in the same file

`socketDir()` (`wezterm.go:223-228`) returns `""` unless `$XDG_RUNTIME_DIR` is set, and `Muxes()`
returns `nil, nil` on `""` (`wezterm.go:54-56`).

**WezTerm on macOS does not use `XDG_RUNTIME_DIR` at all.** It uses the Rust `dirs-next` crate, whose
`runtime_dir()` returns `None` on macOS, so WezTerm unconditionally falls back to
`$HOME/.local/share/wezterm` — even when `XDG_RUNTIME_DIR` *is* explicitly set
(wezterm#6737, wezterm discussion #4422).

So on a real Mac: `XDG_RUNTIME_DIR` is unset → `socketDir()` returns `""` → `Muxes()` returns nil →
`weztermLocator.Available()` is false (`internal/terminal/wezterm.go:21-24`) → `detect` reports
`terminal: "none"` → `Navigate: false` (`internal/detect/detect.go:114-121`).
**The wezterm backend can never activate on macOS today**, independent of the `/proc` bug. The claim
at `docs/phase4/04-navigate-matrix-macos.md:21` that wezterm "works today if wezterm is installed,
zero new code" is wrong on both counts.

### Portable contract I would propose

Keep the check; generalize the abstraction. Replace the concrete `socketPeerPIDFunc` with a
capability-declaring verifier:

```go
// PeerAuthority reports what a platform can prove about the process behind a
// unix socket. Backends whose OS cannot supply a pid declare it, so callers
// choose a policy instead of silently trusting or silently failing.
type PeerAuthority int

const (
    PeerUnknown PeerAuthority = iota // no credential API (plan9, js/wasm)
    PeerUID                          // uid only (BSD xucred without LOCAL_PEERPID)
    PeerPID                          // full pid (linux SO_PEERCRED, darwin LOCAL_PEERPID)
)

type SocketPeer struct {
    Authority PeerAuthority
    PID       int    // valid iff Authority == PeerPID
    UID       uint32
}
```

Policy: `PeerPID` → today's exact-match rule. `PeerUID` → require `peer.UID == os.Getuid()` and treat
pid-reuse as unmitigated (the socket path itself must then carry unforgeable entropy, e.g. a random
suffix minted by the terminal, rather than a guessable pid). `PeerUnknown` → refuse to navigate
through a socket-addressed backend entirely.

Both Linux and darwin land in `PeerPID`, so nothing degrades on the two target platforms; the
abstraction only earns its keep for Windows named pipes and any future BSD.

---

## 3. Unix socket path limits

**Empirically measured on this machine**, not inferred. I bound `AF_UNIX` listeners at path lengths
95–110 under `/tmp`:

| len | result |
|---|---|
| ≤ 103 | ok |
| ≥ 104 | `bind: invalid argument` |

So macOS `sun_path` is 104 bytes including the NUL → **103 usable characters**. Linux allows 107.

`$TMPDIR` here is `/var/folders/lc/9zcy4qt94zs__384dy7jhbq40000gn/T/` — **53 characters**, leaving
only 50 for everything else.

### The rpc failure IS path length. Confirmed.

`internal/rpc/subscribe_test.go:34` does `filepath.Join(t.TempDir(), "s.sock")`. Go's `t.TempDir()`
embeds the **test name** in the directory. For this test:

```
/var/folders/lc/9zcy4qt94zs__384dy7jhbq40000gn/T/TestSubscribe_deliversCurrentWireFrameToEverySubscriber255169532/001/s.sock
len = 124
```

I reproduced it in isolation with a standalone test of the identical name:
`listen unix …: bind: invalid argument`. `srv.Serve` therefore fails at `internal/rpc/rpc.go:306-308`,
the goroutine's error is discarded (`subscribe_test.go:38` — `go srv.Serve(ctx)`), and the dial loop
times out into `t.Fatal("server never started listening")` at `subscribe_test.go:48`. The 2.00s test
duration is exactly the `subscribe_test.go:40` deadline.

The trigger is the 54-character test name, not the RPC design. Sibling tests in the same package with
shorter names pass. Per scope, the RPC layer's design is left to another agent; the finding is that
this is a **test-harness portability defect**, and the fix belongs in a shared helper (a short-named
socket dir under `/tmp`, e.g. `os.MkdirTemp("/tmp", "sb")`) rather than in `rpc`.

### Production paths are fine

`cmd/switchboard/main.go:1499-1502` and `cmd/switchboard-ctl/main.go:955-958` both fall back to
`/tmp/switchboard-<uid>.sock` (~28 chars) when `XDG_RUNTIME_DIR` is unset, which is the macOS
default. Safe.

**But there is a latent trap**: if the macOS port sets `XDG_RUNTIME_DIR=$TMPDIR` (a natural-looking
choice), the daemon socket becomes 53 + 18 = 71 chars — still fine — but
`internal/testsupport/runtimedir.go:23-24` points `XDG_RUNTIME_DIR` at `t.TempDir()`, and any future
test that binds a real socket under a `WeztermRuntime` will blow the limit the same way. Add an
assertion in `NewWeztermRuntime` that the dir leaves ≥ 40 bytes of headroom, or have it use a short
`/tmp` dir.

`tmuxLocator.Available()` (`internal/terminal/tmux.go:33-44`) builds `/tmp/tmux-<uid>/default` —
short, and correct on macOS where `TMUX_TMPDIR` is normally unset.

---

## 4. macOS terminal landscape

| Terminal | Identify pane from inside | Activate specific pane from outside | tty exposed? | Rank |
|---|---|---|---|---|
| **tmux** | Yes — `pane_tty` via `list-panes -a` | Yes — `select-window` + `select-pane` | **Yes** | **1** |
| **WezTerm** | Yes — `tty_name` in `cli list --format json` | Yes — `cli activate-pane --pane-id` | **Yes** | 2 (after 2 bug fixes) |
| **iTerm2** | Yes — session `tty` variable, Python API | Yes — `Session.async_activate(select_tab, order_window_front)` | **Yes** | 3 |
| **kitty** | Partial — `kitten @ ls` gives pid/cwd/cmdline, **no tty** | Yes — `kitten @ focus-window --match pid:N` | No (pid instead) | 4 |
| **Ghostty** | Partial — AppleScript `terminals` gives `id`/`name`/`working directory`, **no tty** | Yes — AppleScript `focus t` | **No** | 5 |
| **Terminal.app** | Tab-level only — AppleScript `tty` of tab | Weak (`selected tab`), no panes | Yes (per tab) | 6 |
| **Alacritty** | N/A — no panes | N/A — nothing to activate | — | n/a (WM-only) |

Detail:

- **tmux** — genuinely platform-agnostic; `internal/terminal/tmux.go` needs zero changes for macOS.
  Its `Available()` probe is already portable. This is the strongest substrate and this agrees with
  `docs/phase4/04-navigate-matrix-macos.md:20`. **Its one real gap is the WM half**: `tmuxPaneRef`
  leaves `Mux = 0` (`tmux.go:102-110`), so `mapping.go:213` skips the WM join entirely and the OS
  window is never raised. The bridge is buildable —
  `tmux list-clients -F '#{client_tty} #{client_session}'` gives the attached client's tty, which
  resolves via the process layer to the hosting emulator's pid, which is the `wm.Window.PID` join.
  That is real work, not "free", and it is the single highest-value item for macOS Navigate.
- **WezTerm** — the CLI ships in the `.app` bundle and is identical. Both blockers are
  switchboard-side (`wezterm.go:75` and `wezterm.go:223-228`), not WezTerm-side. Separately,
  `wezterm cli activate-pane` selects the pane *within* its window and does not raise the OS window —
  that is stated in switchboard's own comment at `internal/wezterm/wezterm.go:193-195` and is why the
  WM handoff exists. There are open upstream reports of macOS-specific `activate-pane` focus
  lag/staleness; those were not verified here and should not be planned around.
- **iTerm2** — the strongest *native* option. `TTY` is a first-class session variable, and
  `async_activate(select_tab=True, order_window_front=True)` does precisely window-raise +
  tab-select + pane-focus in one call — meaning **iTerm2 can do the WM half itself**, which no other
  backend can. `ITERM_SESSION_ID` is `w0t0p0:<UUID>`; the `wNtNpN` prefix is *positional and
  unstable* (it changes when tabs move) — use only the UUID as identity. Cost: the transport is a
  Python gRPC API needing a runtime and an opt-in preference, or AppleScript with per-call spawn
  latency. Note the Python `Session` object does **not** expose `.tty` as a property; you must call
  `async_get_variable("tty")`.
- **kitty / Ghostty** — both can focus a pane but **neither exposes a tty**, which breaks
  switchboard's single portable join key at its root. kitty is recoverable: `kitten @ ls` reports the
  foreground process pid, and pid → tty is a process-layer lookup, so the join moves to pid. Ghostty
  exposes only `id` / `name` / `working directory` — no tty, no pid — so it has **no sound join key
  at all** and is Observe-only until upstream adds one. Ghostty also requires remote-control /
  AppleScript to be enabled.
- **Alacritty** — single-pane by design. There is no pane to activate; a WM raise is the complete
  answer. Worth stating explicitly because it means "no terminal backend" is *sufficient*, not merely
  degraded, for such terminals — which the current
  `Navigate = terminal != "none" && wm != "none"` rule at `detect.go:114-121` gets wrong (see §6).

**Is leaning on tmux viable?** Yes, and it is the right default — with one honest caveat. tmux gives
a *correct and portable pane identity* everywhere, but on its own it gives **half of Navigate**: it
selects the pane inside a terminal window it cannot raise. On Linux the WM raises the window; on
stock macOS there is no WM backend at all. So "tmux everywhere" plus stock macOS yields
pane-select-without-raise, which is invisible to the user if the terminal is behind another app.

---

## 5. Env var vs OSC vs mux CLI

| Mechanism | Portability | Trust | Cost | Used where |
|---|---|---|---|---|
| **tty via kernel** | High — every Unix has one; only the *literal form* varies | Highest — kernel-controlled, unforgeable by the agent | 3 readlinks / F_GETPATHs | The primary join. `proc.go:124-138` |
| **Mux CLI** | Medium — per-terminal, but each ships a stable CLI | High — the mux is the authority on its own panes | ~2–15ms process spawn | `wezterm.go:149`, `tmux.go:158` |
| **OSC user-var** | Low — WezTerm-specific `OSC 1337 SetUserVar` + Lua hooks | Medium — needs the trusted-config Lua integration installed | One tty write + a `run_child_process` spawn per bind | `panebind/osc.go:10`, `switchboard.lua:246` |
| **Env vars** | High to read, but *stale by construction* | Lowest — inherited, forgeable, survives `tmux move-pane` | Free | Hints only: `main.go:728-729` |

**Current dependency, precisely**: the production locator path depends on **tty + mux CLI**, and
nothing else (`terminal/wezterm.go:26-36`, `terminal/tmux.go:46-66`). The env-var read at
`cmd/switchboard-ctl/main.go:728-729` is advisory hint data only. The OSC path is used exclusively by
federation / `panebind`.

That is the right ranking and the codebase already made the right call — the package doc at
`internal/terminal/terminal.go:1-9` states it explicitly ("The tty is the portable join key... only
the tool that resolves it is backend-specific"). **Do not move toward env vars.** `WEZTERM_PANE` is
captured at process spawn and is wrong the moment a pane is moved between tabs or windows — the exact
scenario `panebind`'s `paneIdentity` comment at `registry.go:20-23` is designed around.

The one thing to fix: `proc.go:133` hardcodes `strings.HasPrefix(link, "/dev/pts/")`, and
`cmd/switchboard-ctl/main.go:735` readlinks `/proc/self/fd/N`. Both must change for `/dev/ttysNNN`
and `fcntl(F_GETPATH)` on darwin. That is the process agent's territory but it is *upstream of
everything in this dimension* — no tty, no pane, ever.

---

## 6. Proposed data model

The existing seam is already ~80% right. These are targeted changes, not a rewrite.

### 6a. The one real modeling defect: `PaneRef.Mux` overloads two concepts

`terminal.PaneRef.Mux int` (`terminal/terminal.go:27`) is documented as "multiplexer process id
owning the pane", and `mapping.go:214` uses it as *the WM window's owning pid*. Those coincide for
WezTerm and for nothing else. Consequences already visible: tmux sets it to 0 and loses the WM join
(`tmux.go:102-110`, `mapping.go:213`); iTerm2 would want to skip the WM entirely; kitty's owning pid
is the kitty app, not a "mux".

Split it:

```go
// PaneRef is the neutral pane record.
type PaneRef struct {
    Backend string // locator that produced this ref
    Handle  string // opaque, backend-owned focus token — the ONLY identity shared code may compare

    // MuxKey is the opaque identity of the multiplexer INSTANCE that owns
    // Handle's id-namespace. Two refs are the same pane iff MuxKey and Handle
    // both match. Formerly encoded as the int Mux; now opaque, because tmux
    // sockets, iTerm2 app instances and wezterm gui pids are not all pids.
    MuxKey string

    Window WindowHint // how (or whether) to reach the OS window — see 6b

    TTY   string // controlling tty: the portable join key
    Title string // the pane's own title
    CWD   string
}
```

`MuxSocket` / `PaneID` / `TabID` / `WindowID` move behind `Handle` for shared code; the wezterm
backend keeps them internally to build its `activate-pane` argv. This makes `registry.paneIdentity`
(`panebind/registry.go:20-23`) expressible portably as `(MuxKey, Handle)` instead of
`(GUIPID, PaneID)`.

### 6b. The handoff contract needed from the WM layer

The terminal layer must **never construct a WM address**. It emits a *hint*; the WM resolves it.

```go
// WindowHint is what the terminal seam can say about the OS window hosting a
// pane. It is evidence, not an address: only the WM seam may resolve it, and
// only at action time (it is never persisted — a window can close between
// reconcile and focus).
type WindowHint struct {
    Kind WindowHintKind

    OwnerPID int    // WindowHintOwnerPID: the pid the WM will attribute the OS window to.
                    // NOT necessarily the mux pid — for tmux this is the pid of the
                    // EMULATOR hosting the attached client, resolved via
                    // `list-clients -F '#{client_tty}'` + the process layer.
    Marker   string // WindowHintMarker: an exact, unforgeable substring the terminal
                    // has arranged to appear in the OS window title (today: "[sbw:p:w]").
                    // Strictly stronger than OwnerPID+Title; prefer when present.
    Title    string // WindowHintTitle: best-effort title, used only with OwnerPID.
}

type WindowHintKind int

const (
    WindowHintNone     WindowHintKind = iota // backend cannot identify the OS window
    WindowHintSelf                           // the backend raises the window ITSELF during
                                             // Activate (iTerm2 order_window_front). The
                                             // WM seam must be SKIPPED, not merely allowed
                                             // to fail — a redundant raise races.
    WindowHintOwnerPID
    WindowHintMarker
    WindowHintTitle
)

// WindowResolver is what the terminal layer needs from the WM seam. It replaces
// the ad-hoc (muxPID, windowID, windowTitle) triple currently threaded through
// mapping.matchUniqueClient (internal/mapping/mapping.go:258).
type WindowResolver interface {
    // Resolve returns the single OS window matching hint, or (nil, nil) when
    // zero or MORE THAN ONE match. Ambiguity must fail closed and retry next
    // tick, never guess — this preserves decisions.md #4.
    Resolve(ctx context.Context, hint WindowHint, clients []Window) (*Window, error)
}
```

Three guarantees required from the WM seam:

1. **`Resolve` is pure** — it takes an already-fetched `[]Window` and performs no I/O, so the batched
   reconcile at `mapping.go:198` keeps its "does no I/O" property.
2. **`WindowHintSelf` short-circuits the WM.** `rpc.focusLocalTarget` (`rpc.go:596-618`) currently
   always raises then always activates. With iTerm2 that double-acts. The order must become: if
   `hint.Kind == WindowHintSelf`, call `term.Activate` only.
3. **`acted` accounting.** `rpc.go:615` errors when neither step ran. Under a single-pane terminal
   (Alacritty) with a real WM, only the WM step runs and that is a *complete* success — already
   handled. But under tmux-on-stock-macOS only the terminal step runs, which is a *partial* success
   the user should be told about, not silently reported as full focus.

### 6c. Capability declaration

```go
// Capability is what a terminal backend can actually do. Declared, not inferred:
// detect currently infers Navigate from Name() != "none"
// (internal/detect/detect.go:117), which is wrong for a single-pane terminal
// (nothing to activate, yet fully navigable via the WM) and wrong for a
// backend that can identify a pane but not focus it.
type Capability uint8

const (
    // CanIdentifyPane: Locate(tty) yields a stable pane identity. The floor for
    // any title/status display.
    CanIdentifyPane Capability = 1 << iota
    // CanActivatePane: Activate() focuses a pane within its window. Absent for
    // single-pane terminals — where it is UNNECESSARY, not missing.
    CanActivatePane
    // CanMapPaneToWindow: Locate() populates a WindowHint the WM seam can resolve.
    CanMapPaneToWindow
    // CanRaiseWindow: the backend raises the OS window itself (WindowHintSelf).
    CanRaiseWindow
    // SinglePane: the terminal has exactly one pane per window, so pane focus is
    // implied by window focus. Distinguishes "no activation needed" from
    // "activation impossible".
    SinglePane
)

// Capabilities is optional on Locator. A backend that does not implement it is
// assumed CanIdentifyPane|CanActivatePane|CanMapPaneToWindow — today's wezterm
// behaviour, so nothing existing changes.
type Capabilities interface{ Capabilities() Capability }
```

Declared values, per the findings above:

```go
tmux:       CanIdentifyPane | CanActivatePane | CanMapPaneToWindow // once the client-tty bridge lands; today: no CanMapPaneToWindow
wezterm:    CanIdentifyPane | CanActivatePane | CanMapPaneToWindow
iterm2:     CanIdentifyPane | CanActivatePane | CanRaiseWindow
kitty:      CanIdentifyPane | CanActivatePane | CanMapPaneToWindow // joins on pid, not tty
ghostty:    0                                                     // no tty, no pid -> no sound join
appleterm:  CanIdentifyPane | SinglePane                          // tab-level only
alacritty:  SinglePane | CanMapPaneToWindow
none:       0
```

And the Navigate rule becomes honest:

```go
func (s Stack) Capabilities() state.Capabilities {
    tc := capsOf(s.Terminal)
    // Pane focus is achievable when the terminal can do it OR when there is
    // only one pane to begin with.
    paneOK := tc&CanActivatePane != 0 || tc&SinglePane != 0
    // Window raise is achievable via the WM, or by the terminal itself.
    windowOK := s.WM.Name() != "none" || tc&CanRaiseWindow != 0
    return state.Capabilities{Observe: true, Navigate: paneOK && windowOK, ...}
}
```

### 6d. Registry / detection, replacing the hardcoded WezTerm assumptions

`detect.detectTerminal` (`detect.go:93-109`) hardcodes a two-element candidate list, and
`terminal.NewAuto` (`auto.go:26`) hardcodes `{NewTmux(), NewWezterm()}`. Make it a registry keyed on
ordered probes:

```go
// Registration is one backend and how to cheaply decide it is present.
// Probe MUST be side-effect-free and non-blocking (env reads, file stats) —
// auto.current() runs every probe on EVERY Locate at reconcile cadence
// (internal/terminal/auto.go:33-48).
type Registration struct {
    Name     string
    Priority int    // ascending = innermost-first. tmux 10, emulators 100.
    Probe    func() bool
    New      func() Locator
}

var registry []Registration // append-only; init() in each backend file

// Probes, all verified cheap:
//   tmux     : $TMUX != "" || stat($TMUX_TMPDIR|/tmp)/tmux-<uid>/default   [tmux.go:33-44 — already correct]
//   wezterm  : len(weztermSocketDir() entries) > 0                          [needs the §2 socketDir fix]
//   iterm2   : $TERM_PROGRAM == "iTerm.app" && $ITERM_SESSION_ID != ""
//   kitty    : $KITTY_LISTEN_ON != ""
//   ghostty  : $TERM_PROGRAM == "ghostty"       (-> registers as Observe-only)
//   appleterm: $TERM_PROGRAM == "Apple_Terminal"
```

Two probe hazards worth flagging, both of which the current design already dodges and a naive port
would reintroduce:

- **`$TERM_PROGRAM` is per-session, the daemon is not.** The daemon does not run inside any pane; its
  own environment says nothing about where sessions live. Emulator probes therefore cannot key on the
  *daemon's* env — they must key on an observable artifact (a socket, a running app) or be derived
  from the *session's* environment via the process layer. `$KITTY_LISTEN_ON` is a socket path and is
  fine; `$TERM_PROGRAM` alone is not. This is exactly the trap
  `docs/phase4/04-navigate-matrix-macos.md:104` walks into by listing `$TERM_PROGRAM` as a detection
  signal.
- **The boot race.** `auto.go:8-17` documents that the daemon autostarts before the terminal and that
  one-shot detection would freeze `terminal="none"` for the session. Any registry must preserve
  `auto`'s re-probe-per-call behaviour.

And the platform-specific socket-dir resolution the §2 bug needs:

```go
// weztermSocketDir mirrors WezTerm's OWN runtime-dir logic, which is not
// XDG on macOS: dirs-next::runtime_dir() returns None there, so WezTerm always
// uses $HOME/.local/share/wezterm even when XDG_RUNTIME_DIR is set
// (wezterm#6737). Today's socketDir (internal/wezterm/wezterm.go:223-228)
// returns "" on macOS, which is why the backend is permanently unavailable.
func weztermSocketDir() string {
    if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
        home, err := os.UserHomeDir()
        if err != nil { return "" }
        return filepath.Join(home, ".local", "share", "wezterm")
    }
    if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
        return filepath.Join(x, "wezterm")
    }
    return ""
}
```

---

## 7. Verdict

**Achievable fidelity on macOS, per terminal:**

| Config | Identify pane | Activate pane | Raise window | Net |
|---|---|---|---|---|
| tmux + AeroSpace/yabai | Yes | Yes | Yes — needs the client-tty→emulator-pid bridge | **Full Navigate** |
| tmux + stock macOS | Yes | Yes | **No** | Pane focus only, invisible if app is backgrounded |
| WezTerm + AeroSpace | Yes* | Yes* | Yes | Full Navigate — *after two switchboard bug fixes* |
| WezTerm + stock macOS | Yes* | Yes* | No | Pane focus only |
| iTerm2 + anything | Yes | Yes | **Yes, by itself** | **Full Navigate with no WM at all** |
| kitty + AeroSpace | Via pid, not tty | Yes | Yes | Full, but needs a second join key |
| Ghostty / Terminal.app | No / tab-level | No | via WM | **Observe only** |
| Alacritty + AeroSpace | n/a (single pane) | n/a | Yes | Full Navigate, no terminal backend needed |

**Recommended default supported configuration: tmux, on any terminal emulator, paired with
AeroSpace.** tmux is the only backend that is portable, already implemented, correct today, and
independent of which emulator hosts it. This concurs with
`docs/phase4/04-navigate-matrix-macos.md:78` on the pairing — but that doc's claim that "the terminal
half is essentially free" (line 12) is not accurate, for two reasons it does not mention: tmux never
populates the WM join (`tmux.go:102-110` + `mapping.go:213`), and the wezterm backend is dead on
macOS for both bugs in §2.

**Ordered by value per unit of work:**

1. `wezterm.go:75` → `pidAlive` (`hyprland.go:352`). ~3 lines. Fixes all three failing tests and
   makes the peer-credential check reachable on darwin.
2. `wezterm.go:223-228` → platform-aware socket dir. ~8 lines. Without it, the wezterm backend can
   never be `Available()` on macOS regardless of item 1.
3. Short-path socket helper in `internal/testsupport`, adopted by `rpc`'s test fixtures. Fixes the
   `sun_path` failure; guards every future socket test on macOS.
4. **tmux → WM bridge** (`list-clients -F '#{client_tty}'` → emulator pid → `WindowHint{OwnerPID}`).
   This is the genuinely new engineering, and it is what turns "tmux is the recommended default" from
   aspiration into fact. It also fixes the identical gap on Linux.
5. `PaneRef.Mux` split into `MuxKey` + `WindowHint`, and declared `Capabilities`. Prerequisite for
   iTerm2 and for an honest `Navigate` bit; not urgent before items 1–4.

### Verified vs guessed

**Verified by execution on this Mac:**
- `LOCAL_PEERPID` returns the *peer's* pid over `AF_UNIX` (two-process probe: client 15671 read
  server 15670). `LOCAL_PEERCRED` returns uid/gid and no pid.
- macOS `sun_path` limit: 103 usable chars; 104+ fails `bind: invalid argument` (swept 95–110).
- The `internal/rpc` `TestSubscribe_deliversCurrentWireFrameToEverySubscriber` failure is caused by
  the 124-char `t.TempDir()` path, reproduced standalone.
- All three `internal/wezterm` failures, reproduced and traced to `wezterm.go:75`.
- darwin tty naming: `fcntl(F_GETPATH)` on a pty slave, `TIOCPTYGNAME`, and `tty(1)` all agree on
  `/dev/ttys001`; `os.Readlink("/dev/fd/N")` fails on macOS.
- `x/sys` darwin constants `SOL_LOCAL=0x0`, `LOCAL_PEERCRED=0x1`, `LOCAL_PEERPID=0x2`.

**Documentation-only or inferred — NOT verified:**
- WezTerm's macOS socket location `~/.local/share/wezterm` (from wezterm#6737 / discussion #4422;
  wezterm is not installed on this machine).
- That WezTerm's `tty_name` field on macOS reports the `/dev/ttysNNN` form. Highly likely (it comes
  from the same pty), but unconfirmed — and the tty join breaks silently if it does not.
- Reported macOS-specific `wezterm cli activate-pane` focus lag (search results only; several
  reports marked fixed in nightly).
- All iTerm2 / kitty / Ghostty / Terminal.app behaviour (vendor docs only; none installed here).
  In particular the exact shape of `kitten @ ls` output and Ghostty's AppleScript dictionary.
- libproc `devname(3)` returning the bare `"ttys001"` without a `/dev/` prefix — documented macOS
  behaviour, not observed here.
- AeroSpace / yabai capabilities are quoted from `docs/phase4/04-navigate-matrix-macos.md` and are
  the WM agent's dimension, not independently checked.
