# Window Management & Navigation — Portability Report

## 0. Headline

The WM seam is **already a real abstraction with three swappable backends** (`hyprland`, `i3`/`sway`, `x11`, plus `none`), not a Hyprland shim. The porting problem is not "extract an interface" — it is (a) one missing macOS backend, (b) three specific leaks, and (c) a **capability model that is currently a string comparison against `"none"`**, which is exactly what breaks when macOS backends can do some operations but not others.

There is also existing planning material in the repo — `docs/portability-plan.md` and `docs/phase4/04-navigate-matrix-macos.md` — and **one of its central claims is now out of date**: it says AeroSpace has "no streaming event API." AeroSpace shipped `aerospace subscribe` (JSON-lines events over the socket) in v0.21.x. I verified this below.

---

## 1. What navigation actually requires

### End-to-end path (local session)

1. **Bar renders a clickable chip.** Polybar embeds a literal shell command: `cmd/switchboard-polybar/main.go:159` -> `switchboard-ctl focus pid:<N>`. Waybar/bottombar go through the same ctl.
2. **ctl resolves the selector and sends RPC.** `cmd/switchboard-ctl/main.go:207` (`cmdFocus`) -> `focusSession` at `main.go:215`, which sends `{Cmd: "focus-session", Hostname, PID, StartedAt}` (`main.go:220`).
3. **Daemon dispatches.** `internal/rpc/rpc.go:385` (`case "focus-session"`) -> `s.exactFocus` -> `federation.Navigator.Focus` (wired at `cmd/switchboard/federation.go:107`).
4. **Navigator splits local vs remote.** `internal/federation/navigator.go:79`. Local -> `FocusLocal` = `rpc.Server.FocusLocalSession` (`rpc.go:573`).
5. **The actual actuation.** `rpc.Server.focusLocalTarget`, `internal/rpc/rpc.go:586-619`:
   - `s.wm.Focus(ctx, target.Hyprland.Address)` — **raise the OS window** (`rpc.go:602`), only if an address was resolved.
   - `s.term.Locate(tty)` then `s.term.Activate(pane)` — **select the pane** (`rpc.go:608-611`).
   - If neither acted -> error (`rpc.go:615`).

Remote (federated) sessions take a longer path: `navigator.go:113-133` resolves a pane binding, activates the remote WezTerm pane, **re-resolves the window** (`RevalidateAfterActivation`), then `WM.Focus(route.Window.Address)`.

### WM capabilities used, and whether they are required

| Capability | Where | Required for jump-to-session? |
|---|---|---|
| **Focus(ref)** | `rpc.go:602`, `navigator.go:131` | **REQUIRED.** This is the feature. |
| **Clients()** — enumerate windows with `{Address, PID, Title, Workspace, WorkspaceID}` | `wm.go:66`; called from `mapping.Resolver.Enumerate` `mapping/mapping.go:151`, `findWindow` `mapping.go:242`, `panebind/local.go:94` | **REQUIRED.** Without it no session ever acquires an `Address`, so `Focus` is never called (`rpc.go:601` guards on it). |
| **Match window <-> pid** | `mapping.go:261`, `mapping.go:269`, `panebind/local.go:102` | **REQUIRED** — it *is* the join (see section 2). |
| **Match window <-> title** | same lines | **REQUIRED in practice.** Every WezTerm window shares one GUI pid, so pid alone is never unique; the title/marker is what disambiguates. |
| **ActiveWindow()** | `wm.go:68`; `cmd/switchboard/main.go:844` (reconcile), `navigator.go:244` (remote focus confirmation) | **Nice-to-have** for the jump itself; **required** for the "which chip is focused" highlight and for federated focus projection. |
| **Subscribe() -> focus-changed** | `wm.go:73`; `cmd/switchboard/main.go:565` `runWMLoop`, `main.go:705` | **Nice-to-have.** Drives live chip highlight; the reconciler at `main.go:844` is a backstop that polls `ActiveWindow`. |
| **Subscribe() -> window-closed** | `main.go:668` | **Nice-to-have.** Already best-effort — the branch only ends a session when `sessionDead(src, pid)` independently confirms it (`main.go:691`). |
| **Subscribe() -> layout-changed** | `main.go:710`, debounced at `main.go:563` | **Nice-to-have**, but degrading it raises staleness from ~200 ms to the reconcile interval. |
| **Workspace list / move-to-workspace** | **Not used at all.** | Not required. Workspace is *read* per-window (see section 3) and never *set*. There is no "move window to workspace" call anywhere in the tree. |

That last row matters: **switchboard never switches workspaces and never moves windows.** It reads `Workspace`/`WorkspaceID` purely for chip ordering and display. This substantially de-risks macOS.

---

## 2. The PID -> window join

**It is not process ancestry.** I expected pid -> parent walk -> terminal pid; that is not what happens. The chain is:

```
agent pid ──> controlling TTY ──> terminal pane ──> mux/GUI pid + window-title marker ──> WM window
   (osproc)      (kernel)          (terminal CLI)              (WM Clients())
```

Concretely:

- `mapping.Resolver.Resolve` starts from `osproc.Info{PID, CWD, TTY}` and **bails immediately if TTY is empty** (`internal/mapping/mapping.go:65-67`).
- `r.term.Locate(ctx, info.TTY)` returns a `terminal.PaneRef` (`mapping.go:71`).
- The WM join is gated on `pane.Mux != 0` (`mapping.go:86`, `mapping.go:213`) — the multiplexer's **process id**.
- `matchUniqueClient` (`mapping.go:258-272`) performs the join, in two tiers:
  1. **Marker join (preferred):** `c.PID == muxPID && strings.HasSuffix(strings.TrimSpace(c.Title), marker)` where `marker = "[sbw:<gui-pid>:<window-id>]"` (`internal/panebind/types.go:117`), appended to the compositor-visible title by the WezTerm Lua integration.
  2. **Legacy join (fallback):** `c.PID == muxPID && normalizeTitle(c.Title) == normalizeTitle(pane.WindowTitle)` (`mapping.go:269`), with spinner-glyph stripping at `mapping.go:298-310`.
- Both fail **closed** on ambiguity: `uniqueClient` (`mapping.go:279-293`) returns nil when the count != 1, and the prior address is left in place (decisions.md #4).
- `panebind.LocalResolver.Resolve` repeats the identical join for the federated path (`internal/panebind/local.go:101-107`).

### What the join depends on — the portability contract

Every platform must supply **all four**:

1. **A controlling-tty string for the agent process** that matches what the terminal CLI reports. (`osproc` seam, Phase 4.1/4.2 — not this dimension, but the join dies without it.)
2. **A terminal CLI that maps tty -> pane and exposes the GUI/mux process id.** WezTerm does (`terminal/wezterm.go:73`, `Mux: p.MuxPID`).
3. **A WM that reports, per window, the OWNING PROCESS ID.** Not an app bundle id — a Unix pid, and specifically the pid of the terminal's GUI process.
4. **A WM that reports the per-window TITLE string**, faithfully enough that a suffix match survives.

> **Finding that contradicts the repo's macOS plan.** `docs/phase4/04-navigate-matrix-macos.md:20` claims tmux delivers Navigate on macOS "today, zero new code," and line 115 claims "tmux + AeroSpace delivers end-to-end Navigate." But `tmuxPaneRef` (`internal/terminal/tmux.go:102-110`) sets **neither `Mux` nor `WindowID`** — both stay 0. The chain is innermost-first (`terminal/chain.go:65-70`), so a claude running inside tmux is claimed by the tmux locator, `pane.Mux == 0`, and `mapping.go:213` **skips the WM join entirely**. Such a session never acquires `Hyprland.Address`, so `rpc.go:601` never calls `wm.Focus`.
>
> Net: **a tmux-hosted session gets pane selection but never a window raise — on Linux today, and on macOS.** If the target terminal is not focused, tmux selects the pane inside an unfocused window and nothing visible happens. This is a pre-existing gap the macOS plan inherits and mis-states. WezTerm-without-tmux is the only configuration that currently gets a full window+pane jump.

---

## 3. Hyprland assumptions that leak

`internal/wm/wm.go` is a **real interface with genuinely swappable backends**, and it is well-designed. Evidence:
- `Manager` (`wm.go:59-74`) is 6 methods, no Hyprland types.
- `Window.Address` is documented opaque, "shared code never parses" (`wm.go:3-4`) — and I verified no consumer parses it.
- Address normalization is explicitly a **seam responsibility**, so backends hand out already-comparable refs (`wm.go:12-15`, `hyprland.go:96-105`, `x11.go:187-189`, `i3.go:150-152`).
- `internal/wm/i3.go` and `internal/wm/x11.go` are full independent implementations — proof the seam actually swaps.
- `internal/conformance/conformance.go:202-285` encodes the contract, adopted per-backend at `internal/conformance/wm_test.go:64-74`.

That said, five leaks:

**L1 — `state.HyprlandInfo` is the persisted, public wire name.** `internal/state/state.go:84` (`Hyprland *HyprlandInfo` with json tag `hyprland,omitempty`) and lines 178-183. It is populated by *every* backend (`mapping.go:87`, `:118`, `:215`) and read by the focus path (`rpc.go:601`). The x11 backend's window id lands in a field called `hyprland.address`. `state.Session.LocalWorkspace` is documented as "the Hyprland workspace ID" at `state/state.go:51`. Since `state.json` is a frozen public contract, this is a naming leak, not a structural one — but it is user-visible on a Mac.

**L2 — `internal/barlayout/barlayout.go:225` shells out to `hyprctl monitors -j` directly**, bypassing the seam entirely. This is the only place in the tree that forks a WM binary. It has a graceful fallback (`fallbackWidthPx = 1920`, `barlayout.go:201`), so it degrades rather than breaks, but macOS chips will size against a hardcoded 1920px. There is no `Monitors()` on the `Manager` interface to route this through.

**L3 — Workspace-ID semantics assume positive ints.** `internal/federation/workspace.go:92-97`: *"Workspace ID 0 is unset, not a workspace (Hyprland numbers are positive, or negative for special workspaces)"* — a match with `workspace == 0` is discarded. `Window.WorkspaceID int` (`wm.go:29`) is the numeric field; `Workspace string` is the name. The X11 backend already had to fake this (`x11.go:297-304`, `_NET_WM_DESKTOP + 1`, sticky -> 0). **AeroSpace workspace names are arbitrary strings** ("1", "M", "media"), so a non-numeric workspace yields `WorkspaceID == 0` and remote chips silently drop to end-of-bar ordering. Not fatal, but a real degradation.

**L4 — `HYPRLAND_INSTANCE_SIGNATURE` / `XDG_RUNTIME_DIR`** are correctly confined to `internal/hyprland/hyprland.go` (`:245`, `:270`, `:273`) and referenced from `internal/detect/detect.go:74-78` and `systemd/switchboard.service:23,29`. `detectWMAuto` (`detect.go:77-91`) probes Hyprland *first*, unconditionally, on every platform — on macOS that's a cheap failed `os.Getenv` + `ReadDir`, harmless but ordered wrong.

**L5 — `detect.Options.WM` is a closed string enum** documented as `"auto" | "hyprland" | "sway" | "i3" | "x11" | "none"` (`detect.go:32`), with `detectWM`'s switch at `detect.go:45-57`. Adding backends means editing this switch — fine, just noting it is the single extension point.

Also note the event-name coupling in a comment at `cmd/switchboard/main.go:547`, and `internal/hyprland/hyprland.go:105-146` (`FocusWindow`) carries genuinely Hyprland-specific behavior — a `[[BATCH]]` that toggles `cursor:no_warps` around the dispatch so focusing doesn't warp the mouse. **That's a UX requirement no other backend currently honors**, and macOS focus (which does not warp the cursor) satisfies it for free.

---

## 4. macOS reality check

### 4.1 Window enumeration — `CGWindowListCopyWindowInfo`

Returns an array of dicts. **Without Screen Recording permission you still get** `kCGWindowNumber` (the CGWindowID), `kCGWindowOwnerPID`, `kCGWindowOwnerName` (the *app* name, e.g. "WezTerm"), `kCGWindowBounds`, `kCGWindowLayer`.

**`kCGWindowName` — the per-window title — is gated behind Screen Recording since macOS 10.15 Catalina.** The gotcha is that it fails *silently*: no permission dialog is raised, the key is simply absent from the dictionary and `kCGWindowSharingState` reads zero. Apple's own developer forum thread confirms this (https://developer.apple.com/forums/thread/126860).

**This is disqualifying for our join.** Section 2 established the join needs pid **and** title. CGWindowList without Screen Recording gives pid but not title — and pid alone is never unique for WezTerm (one GUI pid, N windows). So a CGWindowList-based backend would need Screen Recording, whose UX is bad: macOS 15 Sequoia added a recurring re-authorization prompt — originally weekly, softened to **monthly** before 15.0 shipped, and relaxed further in 15.1 for regularly-used apps, but the timestamp resets on each invocation so an app unused for 30 days nags again (9to5Mac, iDownloadBlog, TidBITS — see Sources). A background daemon getting a monthly screen-recording nag is a product problem.

**The Accessibility API gives titles without Screen Recording** — `AXUIElementCreateApplication(pid)` -> `kAXWindowsAttribute` -> `kAXTitleAttribute` per window. That is the correct source for our join. It costs the *Accessibility* grant instead (4.2), which is a one-time toggle rather than a recurring nag.

### 4.2 Focusing a window — Accessibility API

Confirmed: raising a **specific window** requires the Accessibility API. The recipe is `AXUIElementCreateApplication(pid)` -> enumerate `kAXWindowsAttribute` -> `AXUIElementPerformAction(window, kAXRaiseAction)`, usually paired with `NSRunningApplication.activate` to bring the app itself forward. Apple documents `kAXRaiseAction` as making a window "as frontmost as is allowed by the containing application's circumstances" (https://developer.apple.com/documentation/applicationservices/kaxraiseaction).

**Permission UX:** the user opens System Settings -> Privacy & Security -> Accessibility, clicks `+`, navigates to the binary, and toggles it on. The app must typically be restarted. `AXIsProcessTrustedWithOptions` with `kAXTrustedCheckOptionPrompt` shows a dialog that deep-links to that pane — it cannot grant the permission itself.

**Can an unsigned/dev binary hold it? Yes, but fragilely.** TCC stores a code-signing-requirement (csreq) blob alongside the identity. For ad-hoc-signed binaries (the default for a local `go build`) the designated requirement pins the exact **cdhash**, which changes on every rebuild — so **the grant is invalidated by every recompile**. Developer-ID-signed releases keep a stable identity and the grant persists. AeroSpace itself demonstrates the failure mode: its v0.20.3 release notes read *"Update codesign certificate due to expiration. AeroSpace will re-request accessibility permission."*

Practical consequence: **if we build our own AX backend, `go install` from source produces a binary that loses its permission on every rebuild.** That is a genuinely bad developer and early-adopter experience.

### 4.3 Cheaper paths — and the critical limitation

| Path | Can it target a specific window? |
|---|---|
| `NSRunningApplication.activate(options:)` | **No — app only.** Worse: since Big Sur it brings *all* of the app's windows forward (Apple forums thread 668913). |
| `open -a WezTerm` | **No — app only.** |
| `osascript -e 'tell application "X" to activate'` | **No — app only.** |
| AppleScript via `System Events` (`perform action "AXRaise" of window N`) | **Yes**, but it is the AX API wearing a costume — it needs the same Accessibility grant (held by `System Events` *and* an Automation/Apple-Events grant for our binary), plus a process spawn per call. |
| AeroSpace / yabai CLI | **Yes**, window-level, and *they* hold the permission, not us. |

**This is the load-bearing finding for the data model.** Every permission-free path is **app-level only**. If we fall back to app activation, "jump to session" means "raise WezTerm" — and if two sessions live in two WezTerm windows, the jump is ambiguous. The data model must be able to say *"this backend can raise an app but not a window"* rather than silently doing the wrong thing.

### 4.4 Spaces (virtual desktops)

Plainly: **there is no public API for reading or switching Spaces.** Everything that does it uses private CoreGraphics `CGS*` symbols (`CGSCopyManagedDisplaySpaces`, `CGSGetActiveSpace`). yabai uses them and needs **partial SIP disabled plus a scripting addition injected into Dock.app** for the privileged operations. AeroSpace deliberately sidesteps this: it implements its own **virtual** workspaces inside a single native Space, hiding/showing windows, which is why it is SIP-free.

**Verdict for us: this is a non-issue.** Per section 1, switchboard never switches workspaces and never moves windows — it only *reads* a per-window workspace label for chip ordering. AeroSpace's `list-windows` supplies exactly that label. On stock macOS, no label is available, and workspace is simply unset (which `federation/workspace.go:95` already treats as "unresolved, order by start time"). Do not build Spaces support.

### 4.5 Pane addressing after the app is focused

Unchanged from Linux — the terminal seam owns it, and both backends are OS-agnostic CLI drivers:
- **WezTerm:** `wezterm cli list --format json` / `activate-pane` (`internal/terminal/wezterm.go:62`). Ships in the macOS app bundle. Works today.
- **tmux:** `select-window` + `select-pane` (`internal/terminal/tmux.go:112-126`). Works today — **but see the section 2 caveat: tmux contributes no WM join, so it selects a pane without raising the window.**

The handoff order in `focusLocalTarget` is window-raise first (`rpc.go:602`), pane-select second (`rpc.go:608`) — correct for macOS too.

### 4.6 cgo

| Approach | cgo? | Consequence |
|---|---|---|
| `CGWindowListCopyWindowInfo` | **Yes** (CoreGraphics framework) | Kills `GOOS=darwin go build` from Linux |
| AXUIElement / `AXRaise` | **Yes** (ApplicationServices) | Same |
| `NSRunningApplication` | **Yes** (AppKit, + Obj-C runtime) | Same |
| `osascript` subprocess | **No** | Pure Go, but per-call spawn latency |
| **AeroSpace / yabai CLI subprocess** | **No** | **Pure Go. Cross-compiles. No permission for us.** |

`ebitengine/purego` can call CoreGraphics without cgo in principle, but AX and CGWindowList are CoreFoundation-heavy (CFDictionary/CFArray/CFString marshalling, retain/release) — that is a meaningful hand-rolled binding, not a few `RegisterLibFunc` calls, and it would be novel unproven code in the make-or-break subsystem.

This matters *more* than it looks, because of `docs/phase4/03-ci-and-cgo.md`: the repo has already decided to take cgo for `osproc` (libproc), which **already** costs the Linux->darwin cross-build. But note the doc's own line 16: *"the WM backends ... are pure Go. This is why `GOOS=darwin GOARCH=arm64 go build ./...` passes cleanly."* Keeping the **WM** backend cgo-free preserves that property independently of the osproc decision, and keeps `internal/wm` testable on any host.

**Recommendation: the macOS WM backend drives the AeroSpace CLI. Pure Go, no cgo, no permission grant for switchboard, no SIP.**

### 4.7 AeroSpace, verified (and one correction to the repo's own doc)

- `list-windows [--workspace] [--monitor] [--pid] [--app-bundle-id] [--format] [--count] [--json]`, with format variables `%{window-id}`, `%{app-pid}`, `%{app-name}`, `%{window-title}`, `%{workspace}` -> maps 1:1 onto `wm.Window{Address, PID, Title, Workspace}`. Supplies pid **and** title, both without Screen Recording (AeroSpace reads them via AX).
- `focus --window-id <id>` -> `Focus`.
- `list-workspaces --monitor <m> [--visible] [--empty] [--format] [--json]`.
- **`aerospace subscribe [--all] [--no-send-initial] [<event>...]`** — persistent socket connection, **JSON lines**, events `focus-changed` (carries `windowId`, `workspace`), `focused-monitor-changed`, `focused-workspace-changed`, `mode-changed`, `window-detected`, `binding-triggered`. Sends current state on connect.

I verified `subscribe` is in a **released** version, not just main: `docs/aerospace-subscribe.adoc` returns HTTP 200 at tag `v0.21.3-Beta` and 404 at `v0.20.3-Beta`. So it landed in the 0.21.x line. Current release as of this research is **v0.21.3-Beta (2026-07-16)** — still pre-1.0/Beta.

**This corrects `docs/phase4/04-navigate-matrix-macos.md:56` and its Risks section (line 124), which state AeroSpace has no subscribable event stream and that `Subscribe` must be poll-based.** It no longer must be. `focus-changed` maps to `EventFocusChanged`; `window-detected` and `focused-workspace-changed` map to `EventLayoutChanged`.

Two real gaps remain:
1. **No window-closed/destroyed event.** `EventWindowClosed` is unemittable on macOS. Benign — `cmd/switchboard/main.go:691` already requires independent `sessionDead` confirmation, and the liveness sweep covers it.
2. **Workspace names are arbitrary strings** -> `WorkspaceID` is 0 for non-numeric names -> the `workspace.go:92-97` guard drops those rows to default ordering (L3 above).

Also worth stating: AeroSpace's `window-id` is a `CGWindowID`, obtained via the one private symbol it uses, `_AXUIElementGetWindow` (the same one yabai uses). That means the address is stable and, if we ever add a native backend, directly comparable.

---

## 5. Windows sketch

Enumeration is `EnumWindows` + `GetWindowThreadProcessId(hwnd, &pid)` for the owning pid and `GetWindowTextW` for the title — **no permission prompt of any kind**, which makes the section 2 join (pid + title) straightforward and makes Windows structurally *easier* than macOS. `HWND` is the opaque address. Focus-change events come from `SetWinEventHook` with `EVENT_SYSTEM_FOREGROUND` / `EVENT_OBJECT_DESTROY` / `EVENT_OBJECT_NAMECHANGE`, which maps onto our full neutral event vocabulary including `EventWindowClosed` — better coverage than macOS.

The catch is actuation. `SetForegroundWindow` is subject to the **foreground lock**: a process may only steal foreground if it owns the current foreground window, is processing recent user input, or holds a grant via `AllowSetForegroundWindow`. A background daemon satisfies none of these, so the call **returns FALSE or degrades to flashing the taskbar button** rather than raising. The usual workarounds (`AttachThreadInput` to the foreground thread, or a paired `ShowWindow`/`SetForegroundWindow`) are the standard hacks; they mostly work and are what every window-switcher ships, but they are exactly the kind of "may silently no-op" behavior the capability model in section 6 must be able to report. Virtual Desktops (`IVirtualDesktopManager`) expose only `IsWindowOnCurrentVirtualDesktop` and `MoveWindowToDesktop` publicly — no public "switch to desktop N" — but per section 1 we never need it.

> Not verified against current documentation in this research session — treat the Windows paragraph as design-informing background rather than a verified finding.

---

## 6. Proposed data model

### 6.1 Window identity stays an opaque string — don't change it

`Address string` (`wm.go:25`) already works: Hyprland `0x...`, i3 `con_id` decimal, X11 window id decimal, AeroSpace `window-id` decimal, future `HWND` decimal. It is persisted into `state.json` and compared for equality only. **Do not introduce a typed handle** — it would force serialization and platform types into `state`, which is a frozen public contract. The existing `NormalizeEventAddress`/`RawForm` pair (`conformance.go:185-196`) already handles the one place where a backend's event form differs from its list form.

### 6.2 Capability negotiation — the actual change needed

Today capability is a **string comparison against the literal `"none"`**, in six places: `rpc.go:555`, `rpc.go:589`, `navigator.go:69`, `navigator.go:107`, `navigator.go:203`, `detect.go:117`. That is a single boolean — "is there a backend at all" — and it cannot express *"can enumerate and read titles, can raise the app, cannot raise a specific window, permission not yet granted."* Every macOS degradation is invisible to it.

```go
// Capability is one discrete thing a WM backend may or may not be able to do.
type Capability uint16

const (
    CapEnumerate    Capability = 1 << iota // Clients() returns a real window list
    CapWindowPID                           // Clients() fills Window.PID with an OS pid
    CapWindowTitle                         // Clients() fills Window.Title
    CapActiveWindow                        // ActiveWindow() is meaningful
    CapFocusWindow                         // Focus(ref) raises THAT window
    CapFocusApp                            // Focus(ref) raises the owning app only
    CapWorkspaceRead                       // Window.Workspace / WorkspaceID are real
    CapEventStream                         // Subscribe() delivers a live stream
    CapEventWindowClosed                   // ...including EventWindowClosed
)

// CapJoin is the minimum required for the pid+title join in internal/mapping.
// A backend missing any of these can never resolve a session to a window.
const CapJoin = CapEnumerate | CapWindowPID | CapWindowTitle

// Grant is the state of any OS permission the backend needs. Backends with no
// permission requirement report GrantNotRequired.
type Grant uint8

const (
    GrantNotRequired Grant = iota
    GrantGranted
    GrantDenied  // user said no, or revoked
    GrantUnknown // needs asking; the daemon should surface a prompt affordance
)

// Permission describes one OS grant, so the UI can name it and link to it.
type Permission struct {
    Name  string // "Accessibility", "Screen Recording"
    State Grant
    // Hint is user-facing remediation, e.g.
    // "System Settings > Privacy & Security > Accessibility, then restart switchboard".
    Hint string
}

// Status is the full self-report. Availability, capability and permission are
// three DIFFERENT axes and collapsing them is what the "none" string does wrong:
// a backend can be Available with CapFocusWindow declared but GrantDenied.
type Status struct {
    Name        string
    Available   bool
    Caps        Capability
    Permissions []Permission
}

func (s Status) Can(c Capability) bool { return s.Caps&c == c }

// Blocked reports the first permission standing between the backend and its
// declared capabilities, so callers can say WHY instead of failing opaquely.
func (s Status) Blocked() (Permission, bool) {
    for _, p := range s.Permissions {
        if p.State == GrantDenied || p.State == GrantUnknown {
            return p, true
        }
    }
    return Permission{}, false
}
```

Extend `Manager` additively — one method, so the four existing backends need one small addition each and nothing else moves:

```go
type Manager interface {
    Name() string
    Available() bool
    // Status reports what this backend can do RIGHT NOW, including permission
    // state. Cheap and side-effect-free, same contract as Available(); callers
    // may call it per-tick. Permission state may change between calls (the user
    // can toggle a grant while we run), so it is never cached at construction.
    Status() Status
    Clients(ctx context.Context) ([]Window, error)
    ActiveWindow(ctx context.Context) (string, error)
    Focus(ctx context.Context, ref string) error
    Subscribe(ctx context.Context) (<-chan Event, error)
}
```

Graceful degradation then becomes explicit rather than a silent no-op. New sentinel errors sit alongside the existing `ErrUnsupported` (`wm.go:56`):

```go
var (
    ErrUnsupported = errors.New("wm: focus unsupported on this backend")
    // ErrPermissionDenied is a FIRST-CLASS state, not a generic failure: on
    // macOS it is the single most likely reason a jump does nothing, and the
    // user can fix it. Callers must surface the Permission, not just "failed".
    ErrPermissionDenied = errors.New("wm: required OS permission not granted")
    // ErrAppLevelOnly reports that focus raised the owning APPLICATION but
    // could not raise the specific window. Returned as a non-fatal signal so
    // the caller can still activate the pane and can tell the user the jump
    // was approximate.
    ErrAppLevelOnly = errors.New("wm: raised the application, not the window")
)
```

The three call-site families change like this:

- `detect.Stack.Capabilities()` (`detect.go:114-121`): `Navigate` stops being `WM.Name() != "none"` and becomes `status.Can(CapJoin|CapFocusWindow)`, with a new degraded tier for `CapJoin|CapFocusApp`. The `state.Capabilities` block gains the permission list so bars can render "grant Accessibility to enable jumping."
- `rpc.focusLocalTarget` (`rpc.go:586`): the guard at line 589 becomes a capability test; the `wm.Focus` error at line 602 distinguishes `ErrPermissionDenied` (actionable, tell the user how) from `ErrAppLevelOnly` (partial success — continue to the pane activate at line 608 and report an approximate jump) from a real failure.
- `federation.Navigator` (`navigator.go:69`, `:107`, `:203`): the three `Name() == "none"` checks become `Status().Can(...)`. Note `navigator.go:203`'s comment already reasons correctly about the *concept* ("no outer OS-window focus fact to project on an Observe-only stack") — it just expresses it as a string compare.

### 6.3 Workspace: capability-gated, not portable

Keep `Window.Workspace string` + `WorkspaceID int` on the struct, but gate meaning on `CapWorkspaceRead`. Two fixes needed:

- The `WorkspaceID == 0` sentinel (`federation/workspace.go:92-97`) must not be the only signal; when `!Can(CapWorkspaceRead)`, callers should skip workspace ordering entirely rather than treating every row as "unresolved."
- For string-named workspaces (AeroSpace), the backend should hash or index the name into a stable positive `WorkspaceID` for **ordering only**, keeping the human name in `Workspace`. Ordering is all `view.go:150-175` uses it for.

**Do not add `MoveToWorkspace` or `Workspaces()` to the interface** — nothing calls them (section 1), and adding them would create a capability macOS genuinely cannot supply.

`Monitors()` is a separate question: adding it would let `barlayout.go:225` stop forking `hyprctl` (L2). Worth doing, gated on its own capability bit, but it is cosmetic (chip width), not navigation.

### 6.4 The join key in the portable model

Unchanged, and that is the good news: **`(mux/GUI pid, window-title suffix marker)` is already portable.** It requires `CapWindowPID | CapWindowTitle` and nothing platform-specific. AeroSpace supplies both. The WezTerm Lua integration that paints `[sbw:<gui-pid>:<window-id>]` into the title (`panebind/types.go:117`) is OS-agnostic Lua and works on macOS unchanged.

Two model-level notes:
- The join's dependence on **titles** is precisely what rules out a permission-free CGWindowList backend (4.1). Encode that as `CapWindowTitle`, so a hypothetical CGWindowList backend without Screen Recording declares `CapEnumerate|CapWindowPID` but *not* `CapWindowTitle`, fails `CapJoin`, and is correctly reported as unable to navigate instead of silently matching nothing.
- If we ever need an app-level fallback, the join key degrades from `(pid, title)` to `(pid)` alone, which is only correct when the app owns exactly one window. That should be an explicit, separate capability rather than an implicit relaxation of `matchUniqueClient` — relaxing it in place would break the fail-closed-on-ambiguity contract at `mapping.go:279-293` that the whole system's correctness rests on.

### 6.5 Where permission-denied surfaces

Three places, all of which currently have nowhere to put it:
1. **`state.Capabilities`** (`detect.go:114`) -> the persisted block, so any bar can render a "permissions needed" affordance without an RPC.
2. **The `wm.Focus` error path** (`rpc.go:602`) -> so a click that does nothing produces "grant Accessibility in System Settings," not an opaque `wm focus:` error.
3. **`Navigator.RouteReady`** (`navigator.go:65-74`) -> it is documented as "a cheap in-memory/backend-availability hint for navigation controls," which is exactly the right place to gray out a chip when a grant is missing.

---

## 7. Feasibility verdict

**Yes — the core jump-to-session feature can work on macOS at full window+pane fidelity, with zero permission prompts for switchboard itself, provided the user runs AeroSpace and uses WezTerm without tmux.**

Fidelity by configuration:

| Configuration | Fidelity | Permission prompts for switchboard |
|---|---|---|
| **AeroSpace + WezTerm (no tmux)** | **Full: window raised + pane selected.** The `(gui-pid, title-marker)` join resolves; `aerospace focus --window-id` raises. | **None.** AeroSpace holds the Accessibility grant; we only exec its CLI. |
| AeroSpace + tmux | **Pane-level only.** Per section 2, tmux sets `Mux=0` so no window address is ever resolved and `wm.Focus` is never called. The pane is selected inside a possibly-unfocused window. | None |
| yabai + WezTerm | Full, same join | None for us — but yabai needs **partial SIP disabled + Dock scripting addition** |
| **Stock macOS, no tiling WM** | **Pane-level only** (WezTerm/tmux `activate-pane` still runs). Window is not raised. Observe tier fully intact. | None |
| Stock macOS + hypothetical native AX backend | Full | **Accessibility grant required**, and it is **invalidated on every rebuild** for locally-built binaries |

Three things to state plainly:

1. **The recommendation is unchanged from the repo's own plan — ship an AeroSpace backend — but for a sharper reason than the plan gives.** It is not merely "low friction." It is that AeroSpace reads titles via AX and hands them to us over a CLI, which is the *only* way to satisfy the `(pid, title)` join on macOS without switchboard itself holding a permission that either nags monthly (Screen Recording) or evaporates on rebuild (Accessibility). The permission story, not the tiling, is what makes it the right choice.

2. **Do not build a native AX/CGWindowList backend for stock macOS.** `docs/phase4/04-navigate-matrix-macos.md:123` already reaches this conclusion and it holds up under the research: it would force cgo (losing the pure-Go WM property), force switchboard into the TCC grant flow, and — for anyone who `go install`s — break on every rebuild. Stock macOS should be Observe + pane-level activate, reported honestly through the capability model rather than failing opaquely.

3. **Two corrections to the existing plan doc, both worth acting on before implementation.** (a) `aerospace subscribe` exists as of v0.21.x, so `Subscribe` can be a real event stream rather than the poll the doc prescribes — verified against the released tag. (b) The doc's claim that tmux delivers Navigate on macOS is wrong in a way that also affects Linux today: `terminal/tmux.go:102-110` leaves `Mux` zero, so tmux sessions structurally cannot acquire a WM address. If macOS users are expected to run tmux — and many will — **that gap, not the WM backend, is the real ceiling on macOS Navigate**, and it is fixable independently (tmux can report its client's terminal via `#{client_tty}`/`#{client_pid}`, which would let the chain attribute an outer GUI pid to a tmux pane). I have not verified which tmux format variables reliably yield the hosting emulator's pid — flagging that as the open question rather than asserting a solution.

**Open questions / things not verified:** AeroSpace's exact JSON field names in `list-windows --json` output (the `--format` variable names come from the docs, not a live sample); whether `aerospace list-windows --all` includes windows AeroSpace does not manage; the Windows foreground-lock specifics against current documentation; and the tmux client-pid approach in the previous paragraph.

---

## Sources

- CGWindowListCopyWindowInfo / kCGWindowName gating — https://developer.apple.com/forums/thread/126860
- macOS Sequoia screen recording prompts moved weekly -> monthly — https://9to5mac.com/2024/08/14/macos-sequoia-screen-recording-prompt-monthly/
- macOS 15.1 reduced screen-recording prompt frequency — https://www.idownloadblog.com/2024/10/09/macos-sequoia-15-1-macos-screen-recording-prompts-frequency-reduced/
- Sequoia permission-prompt behavior — https://tidbits.com/2024/09/23/how-to-avoid-sequoias-repetitive-screen-recording-permissions-prompts/
- kAXRaiseAction — https://developer.apple.com/documentation/applicationservices/kaxraiseaction
- NSRunningApplication activateWithOptions behavior change in Big Sur — https://developer.apple.com/forums/thread/668913
- Accessibility / TCC and code-signing requirements — https://hacktricks.wiki/en/macos-hardening/macos-security-and-privilege-escalation/macos-security-protections/macos-input-monitoring-screen-capture-accessibility.html
- Accessibility permissions and csreq for ad-hoc-signed builds — https://docs.mumbli.app/for-developers/accessibility-permissions
- AeroSpace — https://github.com/nikitabobko/AeroSpace
- AeroSpace CLI commands reference — https://nikitabobko.github.io/AeroSpace/commands
- yabai SIP / native Spaces discussion — https://news.ycombinator.com/item?id=45554466
- ebitengine/purego — https://github.com/ebitengine/purego
