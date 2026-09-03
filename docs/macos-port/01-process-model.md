# Process model portability report

All macOS claims below are from probes actually run on this host (macOS 14.3.1,
arm64, unprivileged, ad-hoc-signed Go binary). Scratch probe code lived in
`$SCRATCH/{darwinprobe,purego}` (session scratchpad, not persisted in-repo).

---

## 0. Headline finding, before anything else

**On this Mac, the live Claude Code process has `p_comm == "2.1.259"`, not `"claude"`.**
`discovery.IsClaude` gates on `p.Comm != "claude"` at
`/Users/jentachibana/Documents/Goose/switchboard/internal/discovery/discovery.go:43`
and returns false. **A perfectly correct darwin `osproc.Source` would discover
zero sessions.**

Verified against the running agent (pid 96956):

```
comm     = "2.1.259"                                                    (kinfo_proc P_comm AND proc_bsdinfo pbi_name)
exe      = /Users/jentachibana/.local/share/claude/versions/2.1.259     (proc_pidpath)
execpath = /Users/jentachibana/.local/bin/claude                        (KERN_PROCARGS2)
argv     = ["claude"]
cwd      = /Users/jentachibana/Documents/Goose/switchboard
tdev     = 0x10000000 -> /dev/ttys000
```

Cause: `~/.local/bin/claude` is a symlink to
`~/.local/share/claude/versions/2.1.259`, and macOS sets `p_comm` from the
**resolved** binary's basename. Linux sets `comm` from
`kbasename(bprm->filename)` — the path *as passed to execve*, symlink
unresolved — so the same install reads `comm == "claude"` there. (The macOS half
is measured; the Linux mechanism is my reading of `setup_new_exec`, not
something I could test here.)

Directory listing that confirms the layout:

```
lrwxr-xr-x  /Users/jentachibana/.local/bin/claude -> /Users/jentachibana/.local/share/claude/versions/2.1.259
-rwxr-xr-x  ~/.local/share/claude/versions/2.1.132
-rwxr-xr-x  ~/.local/share/claude/versions/2.1.133
-rwxr-xr-x  ~/.local/share/claude/versions/2.1.259
```

`ps -Ao pid,ppid,comm,args` *does* print `claude`, which is why this has gone
unnoticed: `ps` reports the accounting/argv name, not `p_comm`. Only a direct
`kinfo_proc` read exposes the divergence.

This resolves the item `docs/phase4/05-discovery-and-testsupport.md:336` calls
"comm/`node` ambiguity (HIGHEST)". The answer is neither `claude` nor `node`.
**No libproc flavor rescues it** — `PROC_PIDTBSDINFO`, whose wider
`pbi_name[32]` field is sometimes suggested as the untruncated name, also reads
`"2.1.259"`. The only surviving identity signals are `argv[0] == "claude"`, the
KERN_PROCARGS2 exec path, and the `/claude/` substring in the resolved exe
(which `claudeExeValid` at `discovery.go:71` would accept — the comm gate just
rejects first).

Everything below assumes this gets fixed; it is a discovery-predicate bug, not a
process-layer bug, but it determines what the process layer must supply.

---

## 1. What the code actually needs from a process

| Query | Where | Why — the feature that dies without it |
|---|---|---|
| **pid enumeration** | `discovery.go:362` via `osprocSource.AllPIDs` (`discovery.go:300`), backed by `proc.AllPIDs` (`proc.go:143`) | The 1 Hz discovery scan. `scanOnce` lists pids, skips the seen set, and `Read`s only the unseen (`discovery.go:361-385`). This is the hot path; everything else is per-session. |
| **comm** | `proc.go:60`; consumed at `discovery.go:43`, `discovery.go:191` | The primary, cheap agent filter. Also `cmd/switchboard-ctl/bottombar.go:327` re-reads `/proc/<pid>/comm` to confirm a pidfile still names `waybar`. Currently load-bearing and, per section 0, currently wrong on macOS. |
| **argv** | `proc.go:66`, `proc.go:112-122` | Three distinct consumers. `isBackgroundSubcommand` (`discovery.go:83`) rejects `claude daemon`/`claude mcp`, which share comm and exe with a real session and otherwise surface as un-navigable ghost chips. `IsHeadless` (`discovery.go:124`) finds `-p`/`--print` to mark SDK runs inert. `IsCodex` (`discovery.go:194`) **hard-requires** it: `len(p.Args) == 0` implies false, unconditionally, plus the whole `codexIsInteractive` option-prefix parse. Codex is undiscoverable without argv. |
| **exe** | `proc.go:68-70`; consumed at `discovery.go:49`/`:197` | Anti-impostor insurance. Explicitly tolerant of `""` (`discovery.go:55, 68-70`). |
| **cwd** | `proc.go:71-73`; consumed at `mapping.go:59`, logged at `main.go:199`, carried to `provider.RootRef.CWD` | The project label on the bar, and the provider root's directory. Has a documented degradation: `mapping.go:78-80` falls back to `pane.CWD` from the terminal locator when the process cwd is empty. |
| **tty** | `proc.go:82`, `proc.go:127-138` | **The load-bearing join key.** `mapping.Resolve` returns Observe-only immediately when `info.TTY == ""` (`mapping.go:65-66`); `ReconcileFrom` keys the batch pane map on it (`mapping.go:202`); `tmuxLocator.Locate` matches `pane_tty` (`internal/terminal/tmux.go:60`); `wezterm.FindByTTY` matches `TTYName` (`internal/wezterm/wezterm.go:177`). No tty implies no Navigate tier at all. |
| **ppid, walked upward** | `rpc.go:1397-1412`, called from `rpc.go:704` and `rpc.go:1001` | Hook attribution. A hook fired inside a shell wrapper has `getppid()` = the wrapper; the walk climbs to the nearest tracked agent. Bounded at depth 20 and `pid > 1`. Also `proc.go:79` / `parsePPID` at `:181`. |
| **run state (`T`)** | `proc.go:90-98`, `proc.go:103-105`; called at `cmd/switchboard/main.go:890-891` | `state.Session.Suspended` — greys out Ctrl-Z'd chips. Explicitly split out from `Read` as a cheaper reconcile-tick call. |
| **liveness (definitive death)** | `sessionDead` (`main.go:333-342`), `sweepDeadSessions` (`main.go:358`), `dropStaleSessions` (`main.go:386`) | Closing history lanes. Note the shape: death is only believed on `ErrGone`, or on the pid resolving to a non-agent. Any other error means "not dead" — `main.go:329-331` explicitly names darwin's `ErrUnsupported` as the reason. |
| **observed death (once)** | `Source.Watch` (`osproc.go:62-64`), Linux impl `source_linux.go:68-122` | Prompt teardown. Must be a kernel observation, not polling, and must fire exactly once regardless of cause. |
| **uid** | *not queried* | — |
| **environment** | *not queried* | Hook client hints come from the ctl process's own env (`cmd/switchboard-ctl/main.go:326-329`), never another process's. |
| **start time** | *not queried from the kernel* | See section 5.1 — this is the notable gap. `provider.RootKey{PID, StartedAt}` (`internal/provider/provider.go:25-35`) already exists as "the PID-reuse-safe identity", but `StartedAt` is `time.Now()` at discovery (`mapping.go:62`), and `state.go:22-26` documents it as explicitly **"not a kernel process-birth token."** |

**Shape conclusion:** the caller wants *both*. A cheap all-pids list plus per-pid
hydration of only the unseen (the current `pidLister` upgrade,
`discovery.go:284-313`) is the steady state; per-pid `Read` is the
liveness/hook path.

---

## 2. Linux assumptions that leak

**a. `internal/proc` is Linux-only but carries no build tag, and callers above
the seam use it directly.** `osproc.go:39-40` justifies this ("internal/proc
carries no build tags, so this compiles on every platform") — true for
*compiling*, false for *working*:

- `cmd/switchboard/main.go:890` calls `proc.State(before.PID)` on the reconcile
  hot path, bypassing `osproc.Source` entirely. On darwin this fails for every
  session every tick, so `Suspended` is silently never set.
- `internal/rpc/rpc.go:242` defaults `readProc` to `proc.Read`. On darwin every
  ppid walk returns an error at `rpc.go:1403` and drops the hook. Hook
  attribution is dead on macOS even with a working `Source`.

**b. `proc.AllPIDs` is `os.ReadDir("/proc")`** (`proc.go:144`) — this is the
concrete cause of the failing test, `proc_test.go:80`
(`AllPIDs: open /proc: no such file or directory`).

**c. `/dev/pts/` string-prefix filtering as the definition of "is a tty."**
`proc.go:133` returns `""` for anything not under `/dev/pts/`. On macOS the
literal is `/dev/ttysNNN`, so this returns empty for every process — and empty
tty is the "not navigable" sentinel, so the whole Navigate tier fails closed
rather than loudly. The neutral-contract literal
`unknownTTY = "/dev/pts/this-is-not-a-real-tty"` (`conformance.go:324`) has the
same shape but is harmless.

**d. tty derived from fd 0/1/2 rather than from the controlling terminal.**
`proc.go:127-138` reads `/proc/<pid>/fd/{0,1,2}`. That is a *proxy* for the
controlling tty and it is Linux-specific machinery. Both BSD-derived and Linux
kernels track the ctty directly. This matters: a process whose stdio is
redirected but which still owns a ctty reads as tty-less today.

**e. `/proc/PID/status` line parsing escapes into shared test support.**
`internal/testsupport/procstatus.go` is untagged and builds `Name:`/`State:`/
`PPid:` bodies (`:40-41`) with kernel parenthetical labels (`:14-23`). It is the
only fixture generator for `parsePPID`/`parseState`. A darwin backend derives
both from `kinfo_proc` binary fields and cannot use any of it.

**f. `MaskedExePID` is `pid 2 == kthreadd`.** `conformance/source_test.go:88-94`.
There is no kthreadd on macOS. The conformance case at `conformance.go:124-136`
needs a darwin answer (see section 6).

**g. `Args` is missing from the neutral contract.** `conformance.ProcInfo`
(`conformance.go:37-44`) has no `Args` field, and `toNeutral`
(`source_test.go:23-25`) drops it. So `RunSourceContract` passes for a backend
that never populates argv — while `discovery.IsCodex` (`discovery.go:194`)
refuses every process without it. A darwin implementer can be fully green and
ship a build where Codex is invisible. This is the single most dangerous hole in
the harness. (`docs/phase4/01-osproc-darwin-enumerate.md` still lists the field
set as "PID, PPID, Comm, Exe, CWD, TTY" and `docs/phase4/05:97` says
`osproc.Info` "does not currently carry argv" — both stale; `osproc.go:33` added
it.)

**h. Ancestry depth 20** (`rpc.go:1397`). Fine everywhere; noting it because on
macOS the wrapper chain is genuinely longer (`launchd` -> login shell ->
wezterm/tmux -> shell -> agent). Not a leak, just an assumption to keep honest.

**i. `os.Stat("/proc/<pid>")` as a liveness test.**
`internal/wezterm/wezterm.go:75` filters stale `gui-sock-<pid>` entries this way
— so on macOS *every* wezterm mux is filtered out and the wezterm locator
returns nothing. `internal/hyprland/hyprland.go:353-356` already shows the right
idiom (`unix.Kill(pid, 0)`, treating `EPERM` as alive); `wezterm.go:75` should
adopt it.

**j. Assorted `/proc/self`.** `internal/fanout/seedtelemetry.go:74` (`VmHWM`,
already documents "0 when unreadable (non-Linux)" — honest) and
`cmd/switchboard-ctl/main.go:735-739` (`/proc/self/fd/N` -> tty for hook client
hints, plus a `/dev/pts`/`/dev/tty` prefix filter). The latter is a real macOS
gap: `switchboard-ctl` running as a hook would report no tty.
`unix.Ttyname(fd)` (or `ttyname_r`) is the portable replacement and works on
both.

**k. Non-leak worth noting.** `main.go:329-331` and `discovery.go:284-291` show
the codebase already reasoning correctly about partial backends: unsupported is
not dead, and optional fast paths degrade rather than fail. The abstraction is
in decent shape; it is the *bypasses* (a, i, j) that hurt.

---

## 3. macOS equivalents and their limits

### 3.1 Measured results (macOS 14.3.1, arm64, uid 501, no entitlements, no root, `CGO_ENABLED=0`)

| Query | Mechanism | Result |
|---|---|---|
| all pids + ppid + comm + tdev + uid + state + starttime | `unix.SysctlKinfoProcSlice("kern.proc.all")` | **563 procs in 122-562 microseconds.** One syscall, six fields. |
| own-uid pids only | `unix.SysctlKinfoProcSlice("kern.proc.uid", uid)` | **330 procs in 164 microseconds.** ~40% of the table. |
| exe path | `proc_pidpath` | **Works for every process, including root-owned and SIP-protected.** Returned `/sbin/launchd` for pid 1 and a `CoreSpeechXPC` path for a foreign-uid XPC service. No entitlement needed. |
| **cwd** | `proc_pidinfo(PROC_PIDVNODEPATHINFO)` | **Works for same-uid without root.** 330/563 succeeded — exactly the caller's own processes. `EPERM` for other uids, `ESRCH` when dead. |
| argv + exec path | `unix.SysctlRaw("kern.procargs2", pid)` | **Works same-uid.** `EINVAL` for foreign uid. Blob is `argc(uint32)`, exec path, NUL padding, then argv. |
| tty | `kinfo.Eproc.Tdev` (`dev_t`), `-1` = NODEV | `0x10000000` maps to `/dev/ttys000`, confirmed against `stat().Rdev`. |
| suspension | `kinfo.Proc.P_stat` | `SIGSTOP` implies `P_stat == 4` (`SSTOP`). |
| death watch | kqueue `EVFILT_PROC` + `NOTE_EXIT` | Fires once (`Fflags=0x80000000`, `Flags=0x8031`). Register on an already-dead pid gives **`ESRCH`**, mapping cleanly onto `source_linux.go:77-80`. Registering on root pid 1 succeeded. |

**Permission story, plainly: the "cwd needs root on macOS" folklore is false for
this use case.** It comes from `lsof`, which needs root because it inspects
*all* processes. Switchboard only inspects its own uid's agents, and there
`PROC_PIDVNODEPATHINFO` is unrestricted. SIP and hardened runtime do not enter
into it — they restrict *writing* to and *injecting* into other processes, not
reading vnode paths of your own. The failure mode for foreign processes is
`EPERM` on cwd/argv only; exe stays readable. `docs/phase4/01` section 6 reaches
the same conclusion; this now confirms it on hardware.

### 3.2 Pure-Go vs cgo — the phase-4 doc's verdict is overturned

`docs/phase4/01-osproc-darwin-enumerate.md:107` states: *"**There is no clean
cgo-free path** for cwd... **Verdict: cgo is required.**"*

**Measured otherwise.** `proc_pidinfo` is the `SYS_proc_info` syscall (336),
reachable from Go without cgo:

```go
r1, _, e := syscall.Syscall6(336, /*PROC_INFO_CALL_PIDINFO*/ 2,
    uintptr(pid), uintptr(flavor), uintptr(arg),
    uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
```

With `CGO_ENABLED=0` this returned correct cwd and exe for self, for a child,
`EPERM` for pid 1's cwd, `ESRCH` for a dead pid, and `/sbin/launchd` for pid 1's
exe. Combined with `SysctlKinfoProcSlice` and `SysctlRaw("kern.procargs2")`,
**every field in section 1 is obtainable with `CGO_ENABLED=0`.**

The honest caveat: Go's darwin `syscall.Syscall` routes through libSystem's
`syscall()` shim, which Apple deprecated in 10.12. It works today and is what
several pure-Go process libraries rely on, but it is not a supported ABI and
could break in a future macOS. Weigh it:

- **cgo (`libproc.h`)** — sanctioned, stable. Costs: a macOS SDK on the builder,
  `CGO_ENABLED=1`, no cross-compile from Linux, slower builds, and
  `docs/phase4/03-ci-and-cgo.md` exists precisely because of this.
- **pure-Go raw trap** — verified working, cross-compiles, keeps
  `CGO_ENABLED=0` uniform. Costs: deprecated ABI, no compile-time struct-layout
  checking (offsets hand-written; `proc_vnodepathinfo` is
  `2 * (vnode_info 152 + path 1024)`, and the layout must be re-verified per
  arch).
- **shelling to `lsof -a -d cwd -p PID`** — don't. `microsoft/vscode#318863` is
  a public bug report about exactly this being a syscall-storm performance
  problem.

**Recommendation: pure-Go, with the raw-trap calls isolated in one file behind a
`cwd(pid)`/`exePath(pid)` pair, guarded by a startup self-test** (call
`cwd(os.Getpid())` once at init; if it fails, set a flag and degrade cwd to the
`pane.CWD` fallback that `mapping.go:78-80` already provides). That converts
"Apple broke the ABI" from a crash into the same degradation the code already
handles. If that self-test ever fires in the wild, swap to cgo — the interface
does not change.

### 3.3 Assessment of the existing `source_darwin.go`

`internal/osproc/source_darwin.go` is a 21-line stub: every method returns
`ErrUnsupported` (`:15-21`). It is *sound as a stub* — the daemon degrades
rather than crashes, and `main.go:329-331` was written to accommodate it. It
implements nothing.

### 3.4 Traps for whoever implements it

1. **`SysctlKinfoProc("kern.proc.pid", deadpid)` returns `EIO`, not `ESRCH`.**
   Verified, and confirmed in the source:
   `golang.org/x/sys@v0.27.0/unix/syscall_darwin.go:512-514` returns `EIO` when
   the read size is not exactly one record. `docs/phase4/01` flags this as
   "verify on-device" — verified, it happens. Map `EIO` **and** `ESRCH` to
   `ErrGone`.
2. **`devname(3)` is not thread-safe** (static buffer). Use `devname_r`, or skip
   cgo entirely: `/dev/ttys000` has `Rdev == 0x10000000`, i.e. `minor(tdev)`
   formatted as `/dev/ttys%03d` is exact for the pty major. A `/dev` scan (344
   entries) building a `dev_t -> name` map also works but **is a cache that can
   miss**: `/dev/ttys001` did not exist a moment after its master was closed.
   Prefer the minor-number formatter for the pty major, with a `/dev` scan as
   fallback for other device classes.
3. **`P_comm` is `[17]byte`, NUL-terminated** — trim at the first NUL, do not
   `string()` the array.
4. `Tdev` is `int32`; `NODEV` is `-1`.
5. Full hydrated enumerate of 563 processes measured **7.4 ms** total (scan 0.12
   + argv 4.3 + cwd 1.1 + exe 1.9). Affordable even without the cheap-list fast
   path — but section 5.3 makes it unnecessary anyway.

---

## 4. Windows sketch

Enumeration is `CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS)` +
`Process32First/Next`, which yields pid, **ppid**, and `szExeFile` (image
basename, approximately `comm`) in one pass — structurally the same as
`KERN_PROC_ALL`. Full image path is `QueryFullProcessImageName` on a
`PROCESS_QUERY_LIMITED_INFORMATION` handle (obtainable for same-user processes
without elevation). Death watching is a solved problem and cheaper than either
Unix: `WaitForSingleObject` on the process handle, one goroutine per pid,
exactly mirroring the pidfd model. Job-control suspension has no Windows
analogue at all — `Suspended` should be permanently false, not faked from thread
suspend counts.

The two hard ones. **cwd and argv both live in the target's PEB**, reachable
only via `NtQueryInformationProcess(ProcessBasicInformation)` ->
`ReadProcessMemory` of `RTL_USER_PROCESS_PARAMETERS`. Microsoft's own
documentation says `NtQueryInformationProcess` "may be altered or unavailable in
future versions" and lists supported alternatives for everything *except* cwd.
It also needs `PROCESS_VM_READ`, is WOW64-brittle, and reads exactly like the
memory-inspection primitive malware uses — some EDR will flag it. Treat both as
**best-effort, expected-absent**. **`TTY` is not merely hard but conceptually
absent**: Windows consoles are not device files and there is no per-process ctty
path. `GetConsoleWindow`/`AttachConsole` gives an HWND, not a join key.

That last point is the one that should shape the abstraction: `TTY` is currently
*the* Navigate join key, and Windows cannot supply it. Windows will need a
different join (ConPTY handle, terminal-emulator-reported pane id) or will sit
permanently at Observe tier. Do not let the neutral type imply otherwise.

---

## 5. Proposed data model

### 5.1 Identity: `(pid, kernel start time)`, not bare pid

**Recommendation: introduce a kernel-sourced start time and key process identity
on the pair.**

The codebase already believes this — `provider.RootKey`
(`internal/provider/provider.go:25-35`) says "PID alone is insufficient because a
later root can reuse it" — but its `StartedAt` is `time.Now()` at discovery
(`mapping.go:62`), and `state.go:22-26` admits it is "not a kernel process-birth
token." So the reuse defence rests on *when the daemon noticed*, which is
exactly what a daemon restart perturbs.

The kernel value is free on both platforms and currently unused:
`kinfo_proc.Proc.P_starttime` (a `Timeval`; measured `1788393355.696122` for the
live agent) and Linux `/proc/PID/stat` field 22 (jiffies since boot, plus
`/proc/stat`'s `btime`). Windows has `GetProcessTimes` -> `CreationTime`.

Concretely, this closes a real hole at `main.go:333-342`: `sessionDead`
currently says "not dead" whenever the recycled pid *happens to* classify as an
agent. With a birth token that becomes exact.

Two cautions. (i) Linux's jiffies-since-boot resolution is coarse (~10 ms) and
boot-relative — do **not** compare it across hosts; federation already warns
about this at `state.go:776`. Keep it an opaque comparable token, not a
`time.Time` anyone formats. (ii) `state.Session.StartedAt` is on the wire and in
`state.json`; do not repurpose it. Add a separate field.

### 5.2 Types

```go
// Package osproc

// StartToken is an opaque, kernel-sourced process-birth marker, comparable
// only against another token from the SAME host and the same boot. Together
// with PID it identifies one process LIFETIME, defeating pid reuse. The zero
// value means the backend could not supply one; callers must then fall back to
// bare-pid identity rather than treating two processes as equal.
type StartToken uint64

// Availability records, per field, why a value is absent — so a consumer can
// tell "this process has no controlling terminal" (a fact) from "this platform
// cannot tell me" (ignorance). Conflating them is how an unimplementable field
// becomes a silent mis-render.
type Availability uint8

const (
	Present     Availability = iota // the value is authoritative
	NotApplicable                   // authoritative absence: no ctty, no argv (zombie)
	Denied                          // exists, but this process may not read it (EPERM)
	Unsupported                     // this platform/backend cannot answer at all
)

type Info struct {
	PID   int
	PPID  int
	Start StartToken

	Comm string   // ALWAYS (linux comm, darwin p_comm, windows szExeFile)
	Exe  string   // best-effort
	CWD  string   // best-effort
	TTY  string   // best-effort; opaque join key, never parsed
	Args []string // best-effort

	// State is the neutral run state. Backends map their native encoding here
	// rather than exporting a platform letter; Suspended is the only distinction
	// any caller makes today (cmd/switchboard/main.go:890).
	State RunState

	// Missing records why each best-effort field is empty. A field absent from
	// the map with an empty value is a bug in the backend, not "unknown".
	Missing map[Field]Availability
}

type RunState uint8

const (
	StateUnknown RunState = iota
	StateRunning
	StateSuspended // linux "T"; darwin SSTOP; windows: never
	StateZombie
)

func (i Info) Suspended() bool { return i.State == StateSuspended }
```

`Missing` is the answer to "how does the type represent unavailable without
lying." Today `CWD == ""` means all four of: no cwd, EPERM, kernel masked it,
and darwin-stub-returned-nothing. That is exactly the ambiguity that makes a
partially-implemented backend look correct.

**Per-platform truth table:**

| Field | Linux | macOS | Windows |
|---|---|---|---|
| PID, PPID, Comm | always | always | always |
| Start | always (`stat` f22 + btime) | always (`P_starttime`) | always (`GetProcessTimes`) |
| State/Suspended | always | always (`P_stat`) | **`StateUnknown`, always `Unsupported`** |
| Exe | best-effort (masked implies `NotApplicable`) | **always**, even cross-uid | best-effort (`QueryFullProcessImageName`) |
| CWD | own-uid yes; else `Denied` | own-uid yes; else `Denied` | **`Denied`/`Unsupported` in practice** |
| Args | own-uid yes; zombie implies `NotApplicable` | own-uid yes; cross-uid `Denied` | `Denied`/`Unsupported` |
| TTY | always (ctty) | always (`Tdev`) | **`Unsupported` — no analogue** |

**Degradation each absence maps onto (all already exist in the code):** `CWD` ->
`pane.CWD` (`mapping.go:78-80`); `Exe` -> comm-only match
(`discovery.go:55, 68-70`); `Args` -> **currently fatal for Codex**
(`discovery.go:194`), so a `Denied`/`Unsupported` argv must be surfaced as a
startup warning, not silently swallowed; `TTY` -> Observe tier
(`mapping.go:65-66`); `State` -> `Suspended` stays false; `Start` zero ->
bare-pid identity.

### 5.3 Interface

```go
type Source interface {
	// Snapshot returns every process the backend can see, with the CHEAP fields
	// (PID, PPID, Comm, Start, State, TTY) populated and the path-bearing ones
	// (Exe, CWD, Args) left absent and marked NotHydrated. One syscall on both
	// darwin (kern.proc.all) and windows (ToolHelp32); on linux it is one
	// getdents plus a comm read per pid.
	Snapshot() ([]Info, error)

	// Hydrate fills the expensive fields for one process. Returns ErrGone if it
	// vanished between Snapshot and here (the common, benign race).
	Hydrate(*Info) error

	// Read is Snapshot-of-one + Hydrate: the per-pid path used by liveness
	// sweeps and the hook ancestry walk.
	Read(pid int) (Info, error)

	// Watch calls onDeath exactly once when pid dies, from a kernel observation
	// (pidfd / kqueue NOTE_EXIT / WaitForSingleObject) — never inferred from
	// polling. Duplicate Watch for a watched pid is a no-op.
	Watch(ctx context.Context, pid int, onDeath func()) error
	Stop(pid int)
}
```

**Why this shape rather than today's `Enumerate`/`Read`/`AllPIDs`.** The existing
split (`osproc.go:56-67` plus the optional `pidLister` at
`discovery.go:289-291`) exists because on Linux the cheap thing is *a list of
bare pids* and everything else costs ~7 syscalls each. But on macOS **and**
Windows the cheap thing is *a list of already-half-populated records*:
`kern.proc.all` hands over comm and ppid for free, and `discovery.Classify`
filters on comm first (`discovery.go:43, 191`). Today's `AllPIDs` throws that
away and forces a second per-pid read. Splitting on *field cost* rather than
*record count* makes the fast path natural on all three platforms and dissolves
the optional-interface upgrade dance.

`Snapshot`+`Hydrate` also subsumes the Linux `AllPIDs` optimization exactly:
Linux's `Snapshot` reads only `comm` per pid, discovery filters, `Hydrate` pays
for the survivors. Same syscall count as today.

**Call sites that change:**

| Site | Change |
|---|---|
| `discovery.go:279-315` | `procSource`/`pidLister`/`osprocSource` — all three collapse into direct use of `Snapshot`+`Hydrate`. Net deletion. |
| `discovery.go:361-385` | `scanOnce`: `Snapshot` -> skip seen -> `Classify` on cheap fields -> `Hydrate` only candidates -> `Classify` again (argv-dependent predicates need it). |
| `cmd/switchboard/main.go:890-891` | `proc.State`/`proc.Suspended` -> `src.Read(pid).Suspended()`. **Removes a Linux-only bypass.** |
| `internal/rpc/rpc.go:237, 242` | `readProc func(int) (proc.Info, error)` -> `func(int) (osproc.Info, error)`, defaulting to the injected `Source.Read`. **Removes the second Linux-only bypass**, and deletes `osproc.FromProc` (`osproc.go:41`) with its stated reason. Test fakes at `rpc_test.go`/`provider_hook_test.go` change type only. |
| `main.go:333-342` | `sessionDead` gains the `Start` check: gone, **or** the token changed, **or** classifies as non-agent. |
| `internal/wezterm/wezterm.go:75` | `os.Stat("/proc/%d")` -> `unix.Kill(pid, 0)` per `hyprland.go:353-356`. |
| `cmd/switchboard-ctl/main.go:735-739` | `/proc/self/fd/N` readlink -> `unix.Ttyname(fd)`. |
| `cmd/switchboard-ctl/bottombar.go:327` | `/proc/<pid>/comm` -> `src.Read(pid).Comm`. |
| `internal/proc` | Becomes the Linux backend's private helper (add `//go:build linux`), or is absorbed into `source_linux.go`. Nothing above the seam should import it. |
| `discovery.go:42-50` | **Independently, per section 0: stop gating on `comm` alone.** Accept when comm matches **or** `filepath.Base(Args[0]) == "claude"` — the shape `IsCodex` already uses at `discovery.go:194`. |

---

## 6. Test strategy

**a. Fix the `Args` hole first (section 2g).** Add `Args []string` to
`conformance.ProcInfo` (`conformance.go:37-44`), propagate it in `toNeutral`
(`source_test.go:23-25`), and assert it in the interactive-child case:
`len(Args) > 0 && Args[0] != ""`. Until this lands, a green suite does not mean
Codex works. This is the highest-value change in the whole report after
section 0.

**b. Add contract cases for the new model:**

- *should report a stable start token across repeated reads of one live
  process* — two `Read`s of the same pid agree; `SpawnBareChild` twice yields
  different `(pid, Start)` pairs even in the (rare) reuse case.
- *should distinguish "no controlling terminal" from "cannot determine tty"* —
  the bare child must report `TTY == "" && Missing[FieldTTY] == NotApplicable`,
  **not** `Unsupported`. This is precisely the assertion a Windows implementer
  must legitimately fail, which tells them to declare `Unsupported` rather than
  quietly return `""`. Today's case (`conformance.go:105-114`) cannot make that
  distinction and would pass a broken backend.
- *should report suspended when a child is job-control-stopped* — `SIGSTOP` the
  child, assert `Suspended()`, `SIGCONT`. Requires a new `fx.SuspendChild`. This
  is currently untested on any platform.
- *should agree between the batch and per-pid paths* — for every pid in
  `Snapshot`, a `Read` of that pid returns the same cheap fields. This is the
  direct analogue of `runLocatorBatchContract` (`conformance.go:388-444`) and
  catches the drift that a `Snapshot`/`Read` split invites.

**c. Let a platform declare non-support without failing.** Extend
`SourceFixture` (`conformance.go:56-74`) with `Unsupported func(Field) bool`.
Cases for a field the fixture declares unsupported assert *the honest-absence
contract* (empty value **and** `Missing[f] == Unsupported`) instead of skipping.
That is the mechanism by which a Windows implementer proves conformance for a
backend that genuinely cannot supply `TTY`: they do not get to skip, they get to
declare — and the suite then holds them to declaring it consistently. Mirror the
existing precedent at `conformance.go:391-397`, where an absent optional path is
logged and skipped rather than failed.

**d. `MaskedExePID` needs a portable definition.** `pid 2 == kthreadd`
(`source_test.go:88-94`) has no macOS analogue. Replace with "a pid whose
path-bearing fields are unobtainable" and supply it per-platform: Linux keeps
kthreadd; **darwin should use pid 1 (launchd)**, where `cwd` returns `EPERM`
while `exe` returns `/sbin/launchd` (verified). Note this makes darwin's answer
*stronger* than Linux's — it exercises `Denied` specifically, not just "empty".

**e. `internal/testsupport` needs `child_darwin.go`.** `child_linux.go`
(`:25-43`) uses `/dev/ptmx` + `TIOCSPTLCK`/`TIOCGPTN` and hand-builds
`/dev/pts/N` — all Linux-specific. The darwin twin needs
`grantpt`/`unlockpt`/`ptsname`, which **`golang.org/x/sys v0.27.0` does not
export for darwin** (verified: `undefined: unix.Grantpt/Unlockpt/Ptsname`).
Three options: bump `x/sys` (check whether a later version adds them), a small
cgo helper, or `unix.IoctlGetInt(fd, TIOCPTYGRANT/TIOCPTYUNLK)` +
`TIOCPTYGNAME`. **Also required:
`cmd.SysProcAttr = &unix.SysProcAttr{Setsid: true, Setctty: true}`.** Without
it, macOS gives the child stdio on the pty but no *controlling* terminal, so
`Tdev` stays `NODEV` and the interactive-child case fails for a reason that has
nothing to do with the backend. `child_linux.go` gets away without it only
because it reads fds, not the ctty — which is leak 2d showing up as a
test-harness divergence. Add `SuspendChild` to `child.go` (portable
`unix.Kill(pid, SIGSTOP)`) alongside `Kill` (`child.go:48-54`).

**f. Untag `conformance/source_test.go`.** It is `//go:build linux` (`:1`)
purely because its fixtures are. Once the fixtures are portable, drop the tag so
`go test ./...` on a Mac runs the contract instead of silently compiling
nothing. Add a `darwin` job to the CI matrix
(`docs/portability-plan.md:114` currently stubs darwin as build-only).

**g. `procstatus.go` stays Linux-scoped.** It is a `/proc` text-format fixture
generator (leak 2e) feeding `parsePPID`/`parseState`. When `internal/proc` gains
`//go:build linux`, tag this alongside it. Darwin's equivalent unit-level
fixtures are `kinfo_proc` structs, not text — but they test *field extraction*,
which is thin enough that the conformance suite covers it. Do not build a
parallel fixture library.

---

## Open questions / where I am guessing

- **The Linux `comm` half of section 0 is inferred**, not measured — there was
  no Linux box available. The claim "Linux `comm` comes from the unresolved
  execve path, macOS `p_comm` from the resolved binary" is a reading of
  `setup_new_exec`. The macOS observation is direct and certain. Worth 30
  seconds on a Linux host with the versioned install to confirm whether this is
  a genuine platform divergence or a latent bug on both.
- **Whether the versioned-symlink layout is universal.** One install was
  observed (native installer, `~/.local/bin/claude` -> `versions/2.1.259`).
  Homebrew and npm layouts may differ, and a future installer could change it.
  This argues for making the predicate robust to *any* comm rather than
  enumerating install layouts.
- **`syscall.Syscall(336, ...)` durability.** Verified on 14.3.1. There is no
  evidence about macOS 15/26, and Apple documents no guarantee. The startup
  self-test in section 3.2 is the mitigation, not a claim that it will keep
  working.
- **`proc_vnodepathinfo` struct offsets are hand-computed** (`152 + 1024` per
  `vnode_info_path`) and validated only on arm64. amd64 needs re-verification;
  cgo would eliminate this class of risk entirely.
- **kqueue at scale.** Only one watched pid was tested. Whether one goroutine
  per pid (mirroring `source_linux.go:87-120`) or a single shared kqueue with a
  demux goroutine is better for ~20 sessions is unmeasured. The shared-kqueue
  design is the idiomatic BSD one and would be the starting point.
- `internal/panebind`, `internal/history`, and the bar renderers were not
  audited for process assumptions — outside the assigned dimension.
