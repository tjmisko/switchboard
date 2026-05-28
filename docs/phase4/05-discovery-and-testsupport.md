Phase 4 plan for making discovery find `claude` on macOS: Darwin-aware `IsClaude`, unifying the discovery scan onto `osproc.Source`, a `child_darwin.go` pty test helper, and daemon wiring cleanup.

> **Status: partially doable on Linux** — the discovery→`osproc` unification and the `IsClaude` broadening compile, run, and keep `discovery_test.go` green on Linux today. Two pieces genuinely need a Mac: the `child_darwin.go` controlling-pty helper (must be exercised on a real macOS host) and the **comm/`node` question** (whether a `claude` process ever shows up with comm `node` rather than `claude`) — that one is the highest-risk unknown and gates the correctness of `IsClaude` on Darwin.

## 1. `IsClaude` on macOS

### 1.1 Where the `claude` binary actually lives on macOS

Research (Anthropic's `setup` docs, May 2026) confirms a critical fact for our predicate: **since v2.1.113 `claude` is a native, signed binary — it does not run under Node.** Even the npm install pulls a per-platform native binary (`@anthropic-ai/claude-code-darwin-arm64`) via an optional dependency and links it into place; "the installed `claude` binary does not itself invoke Node." So in the common case the process's `comm` is `claude`, not `node`. (We still flag the residual ambiguity in §1.5 / Risks.)

Install methods and the resulting on-disk paths:

| Method | Real binary | Launcher on `$PATH` | Notes |
| --- | --- | --- | --- |
| Native installer (recommended) — `curl …install.sh \| bash` | `~/.local/bin/claude` (or `~/.claude/bin/claude` on some installs) | same path | Version data under `~/.local/share/claude/`. **The exe is a plain file `…/bin/claude`, not necessarily under a `/claude/` directory** — our current `/claude/` substring check would MISS the native install on macOS. |
| Homebrew cask `claude-code` | Cellar/Caskroom path | `/opt/homebrew/bin/claude` (Apple Silicon) or `/usr/local/bin/claude` (Intel) | symlink/launcher into the cask payload |
| npm global | `<npm prefix>/lib/node_modules/@anthropic-ai/claude-code/.../claude` (e.g. `/usr/local/lib/node_modules/...` or `/opt/homebrew/lib/node_modules/...`) | `<npm prefix>/bin/claude` | native binary linked in via postinstall |

The decisive observation: across all three macOS methods the **basename of the resolved exe is `claude`**, but only the npm and `~/.local/share/claude/` cases contain a `/claude/` *directory* substring. The Linux `~/.local/share/claude/claude` dev-build case keeps the `/claude/` substring. So the robust generalization is: accept the exe if it contains `/claude/` **OR** ends in `/claude` (basename match), while keeping the `comm == "claude"` gate as the primary, cheap filter.

Note on `comm` truncation: Linux `/proc/<pid>/comm` truncates to 15 chars (`TASK_COMM_LEN` 16). macOS `p_comm` (from `proc_pidinfo`/`kinfo_proc`) truncates similarly (`MAXCOMLEN` is 16). `claude` is 6 chars, so truncation is a non-issue for the literal `"claude"` match on both OSes — but it means we must never tighten the predicate to require a *longer* comm.

### 1.2 Should `IsClaude` stay on `proc.Info` or move to `osproc.Info`?

**Recommendation: move it to `osproc.Info`.** The §2 unification repoints the scanner onto `osproc.Source`, so the scan loop will hand `IsClaude` an `osproc.Info`. `proc.Info` and `osproc.Info` are field-identical (`PID,PPID int; Comm,Exe,CWD,TTY string`), so the migration is a pure type swap in the signature and the test table — no behavior change. Keeping `IsClaude` on `proc.Info` would force an `osproc.Info → proc.Info` conversion at every call site and keep the Linux-only `internal/proc` type wired into the OS-agnostic discovery path, defeating the unification.

### 1.3 Proposed `IsClaude`

```go
// internal/discovery/discovery.go

import (
	"strings"

	"github.com/tjmisko/switchboard/internal/osproc"
)

// IsClaude reports whether the given process snapshot looks like a Claude Code
// process. The primary filter is comm == "claude" (cheap, and the native
// binary runs under its own name on both Linux and macOS — it does NOT run
// under node since claude v2.1.113). The exe check is cheap insurance against
// name collisions and is deliberately broad enough to cover every documented
// install layout:
//
//   - native installer (macOS/Linux): exe is ~/.local/bin/claude or
//     ~/.claude/bin/claude — matched by the "/claude" basename suffix.
//   - dev build / native versioned payload: exe under ~/.local/share/claude/…
//     — matched by the "/claude/" substring.
//   - npm global: …/node_modules/@anthropic-ai/claude-code/…/claude —
//     matched by the basename suffix.
//   - Homebrew: /opt/homebrew/bin/claude or /usr/local/bin/claude — matched by
//     the basename suffix.
//
// A masked/empty exe is given the benefit of the doubt (the comm already
// matched). The basename suffix accepts a bare "claude" exe too (no leading
// slash) so a process exec'd with a relative path still matches.
func IsClaude(p osproc.Info) bool {
	if p.Comm != "claude" {
		return false
	}
	if p.Exe == "" {
		return true // kernel/sysctl masked the exe (rare); comm already matched
	}
	if strings.Contains(p.Exe, "/claude/") {
		return true
	}
	return p.Exe == "claude" || strings.HasSuffix(p.Exe, "/claude")
}
```

This keeps every existing `discovery_test.go` assertion true:
- `{Comm:"claude", Exe:""}` → true (masked).
- `{Comm:"claude", Exe:"/home/u/.local/share/claude/claude"}` → true (`/claude/` substring AND suffix).
- `{Comm:"claude", Exe:"/usr/bin/claude-impostor"}` → false (no `/claude/`, suffix is `claude-impostor` not `claude`). **Verified: the impostor still fails** because `claude-impostor` does not end in `/claude`.
- `{Comm:"bash", …}` → false; `{Comm:"Claude", …}` → false (case-sensitive).

Add macOS-layout rows to the test table (they live under the same predicate, no new behavior):

```go
{"native installer exe", osproc.Info{Comm: "claude", Exe: "/Users/u/.local/bin/claude"}, true},
{"homebrew launcher",     osproc.Info{Comm: "claude", Exe: "/opt/homebrew/bin/claude"}, true},
{"npm global",            osproc.Info{Comm: "claude", Exe: "/usr/local/lib/node_modules/@anthropic-ai/claude-code/cli/claude"}, true},
{"bare relative exe",     osproc.Info{Comm: "claude", Exe: "claude"}, true},
{"comm node (ambiguous)", osproc.Info{Comm: "node",   Exe: "/usr/local/bin/node"}, false}, // see §1.5 / Risks
```

### 1.4 Doc-comment fix

The current package doc-comment says discovery "scans /proc". After §2 it scans through `osproc.Source` (which is `/proc` on Linux, `libproc`/`proc_listallpids` on macOS). Update the package comment to say "scans the OS process source" so it is no longer Linux-specific.

### 1.5 The comm/`node` question (HIGHEST-RISK UNKNOWN — must verify on a Mac)

The docs strongly imply modern `claude` always runs as a native process named `claude`. But we have NOT confirmed on a real macOS host that:
1. The launcher at `/opt/homebrew/bin/claude` (a script/symlink) `exec`s the native binary so the *running* process keeps comm `claude` (vs. spawning a child and the parent surviving with a different comm).
2. No legacy/transitional install path still runs `node <…>/cli.js` with comm `node` and `claude` only in argv.

**If (2) ever holds**, `comm == "claude"` filters out the real session. The mitigation (only if a Mac confirms it is needed): add an args-based fallback — read `argv` from the process source and accept comm `node` when `argv[1]` basename is `claude` / resolves under a `claude-code` path. `osproc.Info` does not currently carry argv, so this would require widening the seam (a new `Args []string` field on `osproc.Info`, populated from `KERN_PROCARGS2` on Darwin and `/proc/<pid>/cmdline` on Linux). **Do not build this speculatively** — verify on hardware first; it is a meaningful seam change. Track as the top open question for the Mac milestone.

## 2. Repointing discovery onto `osproc`

### 2.1 The refactor

Today there are two process-reading paths: the proc-backed discovery scan and `osproc` for death-watch. Phase 1 deliberately deferred unifying them. We now point the scanner at `osproc.Source`.

Narrow the `procSource` seam so it is satisfied by a thin adapter over `osproc.Source`, keeping the injected-fake test seam intact. Two shape options:

**Option A — keep `AllPIDs()+Read` (cheap-scan preserving), recommended.** Preserves the current "Read only unseen pids" behavior (one `Read` per *new* pid per tick, not per *all* pids per tick). Requires `osproc.Source` to grow a cheap `AllPIDs()` method.

**Option B — drive from `Enumerate()`.** Simpler (no new Source method), but `Enumerate()` reads *every* process's exe/cwd/tty each tick. On Linux that is the same procfs cost we pay once at startup; per-second it is wasteful (hundreds of full `Read`s/sec) and on macOS `proc_pidinfo` per pid is comparatively expensive.

**Recommendation: Option A.** Add a cheap `AllPIDs() ([]int, error)` to `osproc.Source` (Linux: read `/proc` dirents — already implemented internally; macOS: `proc_listallpids`, which returns just the pid array with no per-pid syscalls). This keeps discovery's hot path at "enumerate pids cheaply, `Read` only the unseen ones," which is exactly the current Linux behavior and the reason the seen-set exists. Driving off `Enumerate()` would regress that and couple discovery to the heavy path the conformance suite uses.

`AllPIDs()` is additive to the `Source` interface; both backends already have the underlying call. The conformance `Source` interface in `internal/conformance` does NOT need `AllPIDs()` (the contract suite uses `Enumerate`/`Read`/`Watch`/`Stop`) — leave it unchanged.

### 2.2 New `osproc.Source` method

```go
// internal/osproc/osproc.go — add to the Source interface
type Source interface {
	// AllPIDs returns just the visible pids, cheaply (no per-pid exe/cwd/tty
	// reads). Used by the discovery scan loop, which then Reads only pids it has
	// not seen before.
	AllPIDs() ([]int, error)
	Enumerate() ([]Info, error)
	Read(pid int) (Info, error)
	Watch(ctx context.Context, pid int, onDeath func()) error
	Stop(pid int)
}
```

- `source_linux.go`: implement `AllPIDs()` by delegating to the existing pid-listing helper (the same dirent scan `Enumerate` already uses before per-pid reads) or to `internal/proc.AllPIDs`.
- `source_darwin.go` (still the stub until the Phase 4 Darwin backend lands): add `func (darwinSource) AllPIDs() ([]int, error) { return nil, ErrUnsupported }` so it keeps compiling.

### 2.3 New `procSource` seam + `realProcSource`

```go
// internal/discovery/discovery.go

// procSource is the seam between the scanner and the OS process layer. The
// default implementation adapts an osproc.Source; tests inject a fake so the
// seen-set state machine is exercised without a live process table.
type procSource interface {
	AllPIDs() ([]int, error)
	Read(pid int) (osproc.Info, error)
}

// osprocSource adapts an osproc.Source to the scanner's narrow procSource seam.
type osprocSource struct{ src osproc.Source }

func (a osprocSource) AllPIDs() ([]int, error)             { return a.src.AllPIDs() }
func (a osprocSource) Read(pid int) (osproc.Info, error)   { return a.src.Read(pid) }

// New builds a Scanner over the given OS process source.
func New(src osproc.Source) *Scanner {
	return &Scanner{seen: make(map[int]struct{}), src: osprocSource{src: src}}
}
```

`scanOnce`/`Run`/`Forget` are unchanged except the callback type: `onAppeared func(osproc.Info)`. The seen-set state machine (fire-once, Forget re-fires, errored/non-claude never remembered, recycled-pid shadowed, lock-free callback) is byte-for-byte identical.

Drop `realProcSource` (and the `internal/proc` import from discovery). `New()` now takes a `Source` argument instead of constructing `realProcSource{}`.

### 2.4 Adapting the test fake (signatures only, asserted behavior unchanged)

`discovery_test.go`'s `fakeProcSource` and `claudeInfo` swap `proc.Info` → `osproc.Info`; every assertion stays the same:

```go
import "github.com/tjmisko/switchboard/internal/osproc"

type fakeProcSource struct {
	pids  []int
	infos map[int]osproc.Info
	errs  map[int]error
	reads int
}

func (f *fakeProcSource) AllPIDs() ([]int, error)            { return f.pids, nil }
func (f *fakeProcSource) Read(pid int) (osproc.Info, error)  { /* unchanged body, osproc.Info{} on error */ }

func claudeInfo(pid int) osproc.Info { return osproc.Info{PID: pid, Comm: "claude"} }
```

`newWithSource(src procSource)` keeps taking the narrow `procSource` (the fake satisfies it directly), so the five scanner tests call it unchanged. The `fire := func(osproc.Info) {...}` callback type updates; counts/assertions are untouched. **The `procSource` seam keeps `AllPIDs()`+`Read` exactly so the fake's call-shape and the `reads` counter semantics are preserved.**

## 3. `internal/testsupport/child_darwin.go`

A `//go:build darwin` sibling of `child_linux.go` providing the identical API the conformance fixture and death tests already call: `SpawnTTYChild(t, dur) *Child`, plus the cross-platform `SpawnSleep`, `DeadPID`, and `Child` which already live in the untagged `child.go`/`proctree.go` and need no Darwin variant. Only `SpawnTTYChild` is OS-specific (it needs a controlling pty); `DeadPID` (a pid above the max) can stay cross-platform — on macOS use a constant well above the default `kern.maxproc` ceiling (e.g. `1 << 24`), mirroring the Linux helper.

### 3.1 macOS pty approach

Linux's helper opens `/dev/ptmx`, unlocks via `TIOCSPTLCK`, learns the slave number via `TIOCGPTN`, and opens `/dev/pts/N`. **macOS has neither `/dev/ptmx` nor `TIOCGPTN`.** The portable POSIX route is `posix_openpt` → `grantpt` → `unlockpt` → `ptsname`, then open the returned `/dev/ttysNNN` slave.

**Dependency decision: use raw `golang.org/x/sys/unix`, no cgo, no new dependency.** `x/sys/unix` exposes the full sequence on Darwin: `unix.PosixOpenpt(unix.O_RDWR | unix.O_NOCTTY)`, `unix.Grantpt(fd)`, `unix.Unlockpt(fd)`, and `unix.Ptsname(fd)`. `golang.org/x/sys` is already a dependency (the Linux helper imports `unix`). Adding `github.com/creack/pty` would pull a new module just to wrap these four calls and would still not, by itself, make the slave the child's *controlling* terminal — we still need `SysProcAttr`. So raw syscalls win on both the dependency and the control-precision axes.

Making the slave the child's **controlling** terminal (so the OS reports a non-empty tty for it — the only thing the conformance contract asserts) is done entirely through `os/exec`'s `SysProcAttr`, no manual fork:
- `Setsid: true` — the child starts a new session (required before it can acquire a controlling tty).
- `Setctty: true` — acquire a controlling terminal.
- `Ctty: <index of the slave fd in cmd.Stdin/Stdout/Stderr>` — on Darwin `Ctty` is the **fd number as the child will see it** (i.e. the slave wired in as fd 0/1/2). Wiring the slave as `cmd.Stdin = slave` makes it child-fd 0, so `Ctty: 0`.

This is cgo-free and works through `os/exec` + `x/sys/unix` exactly like the Linux helper.

### 3.2 Concrete `child_darwin.go`

```go
//go:build darwin

package testsupport

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// SpawnTTYChild starts `sleep` attached to a fresh pseudo-terminal as its std
// streams and as its controlling terminal, so the OS reports a non-empty tty
// (a /dev/ttysNNN) for the child — the "interactive child / non-empty tty"
// case. Mirrors the Linux helper's contract exactly; the conformance suite
// asserts only that the tty is non-empty, never the literal path, so this
// passes the same neutral contract. The pty master is closed on cleanup.
func SpawnTTYChild(t testing.TB, d time.Duration) *Child {
	t.Helper()

	master, err := unix.PosixOpenpt(unix.O_RDWR | unix.O_NOCTTY)
	if err != nil {
		t.Fatalf("posix_openpt: %v", err)
	}
	if err := unix.Grantpt(master); err != nil {
		unix.Close(master)
		t.Fatalf("grantpt: %v", err)
	}
	if err := unix.Unlockpt(master); err != nil {
		unix.Close(master)
		t.Fatalf("unlockpt: %v", err)
	}
	slavePath, err := unix.Ptsname(master)
	if err != nil {
		unix.Close(master)
		t.Fatalf("ptsname: %v", err)
	}
	slave, err := os.OpenFile(slavePath, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		unix.Close(master)
		t.Fatalf("open %s: %v", slavePath, err)
	}

	cmd := exec.Command("sleep", strconv.Itoa(sleepSeconds(d)))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0, // child-fd 0 == the slave we wired into Stdin
	}
	if err := cmd.Start(); err != nil {
		slave.Close()
		unix.Close(master)
		t.Fatalf("spawn tty child: %v", err)
	}
	slave.Close() // child holds its own dup of the slave

	c := &Child{PID: cmd.Process.Pid, cmd: cmd}
	t.Cleanup(func() {
		c.Kill(t)
		unix.Close(master)
	})
	return c
}
```

Notes / gotchas to verify on hardware:
- `Child.cmd` is an unexported field set via the struct literal in the same package — `child_darwin.go` is in `package testsupport`, so this compiles (the Linux helper does the same).
- If `Ctty: 0` does not yield a controlling tty on Darwin, fall back to leaving stdio on the slave and setting `Ctty` to the slave's parent-side fd — Go's Darwin runtime historically interpreted `Ctty` against the parent fd in some versions; the correct value is version-sensitive and is the single thing to confirm with the conformance run on a Mac.
- Keep `unix.O_NOCTTY` on the parent's slave open so the *parent* (test process) never accidentally acquires the slave as its own controlling tty.

### 3.3 What the conformance fixture needs

The Darwin osproc test file wires the same `SourceFixture` the Linux one does: `SpawnTTYChild` → `testsupport.SpawnTTYChild`, `SpawnBareChild` → `testsupport.SpawnSleep` (cross-platform, no tty), `KillChild` → `(*Child).Kill`, `MaskedExePID` → a Darwin kernel-thread/`kernel_task`-style pid or `(0,false)` to skip. None of these change the contract; they only need the Darwin pty helper above to exist.

## 4. Daemon wiring cleanup (`cmd/switchboard/main.go`)

Unify both process-reading paths onto `procSrc` (the `osproc.Source` from `detect.Detect`) and the `osproc.Info`-typed `IsClaude`.

Deltas:

1. **Drop the `internal/proc` import** from `main.go`.
2. **Wire the scanner over `osproc`:**
   ```go
   scanner := discovery.New(procSrc) // was discovery.New()
   ```
3. **`onClaudeAppeared` takes `osproc.Info`:**
   ```go
   onClaudeAppeared := func(info osproc.Info) {
       log.Printf("claude pid=%d cwd=%s tty=%s discovered", info.PID, info.CWD, info.TTY)
       sess := resolver.Resolve(ctx, info)
       // … unchanged: store.Apply, procSrc.Watch(ctx, info.PID, …) for death, scanner.Forget
   }
   ```
   This requires `mapping.Resolver.Resolve` to accept `osproc.Info`. Two choices:
   - **(preferred)** Migrate `Resolve`/`Reconcile`'s input from `proc.Info` to `osproc.Info` (field-identical; pure type swap, no logic change). This removes the last `internal/proc` reference from the live daemon path.
   - **(minimal)** Convert at the call site: `resolver.Resolve(ctx, proc.Info(info))` — works because the structs are field-identical, but keeps `internal/proc` in the daemon import set. Prefer the migration; the conversion is a stopgap only.
4. **`dropStaleSessions` uses `procSrc`:**
   ```go
   func dropStaleSessions(store *state.Store, procSrc osproc.Source) {
       store.Apply(func(m map[int]*state.Session) {
           for pid := range m {
               info, err := procSrc.Read(pid)
               if err != nil || !discovery.IsClaude(info) {
                   delete(m, pid)
               }
           }
       })
   }
   ```
   Call it AFTER `procSrc := stack.OSProc` is in scope (move the call below that line; today it runs before `procSrc` is assigned). `osproc.Read` returns `ErrGone`/`ErrUnsupported` on a missing pid, which the `err != nil` branch already drops — same behavior as `proc.Read`'s error.

After this, the daemon has exactly one process-reading backend (`procSrc`, an `osproc.Source`) feeding discovery scan, death-watch, and stale-drop.

## 5. Definition of Done

- [ ] `osproc.Source` gains `AllPIDs() ([]int, error)`; Linux impl delegates to the existing pid lister; Darwin stub returns `ErrUnsupported`.
- [ ] `discovery.IsClaude` takes `osproc.Info` and matches `/claude/` substring OR `/claude` basename suffix OR empty exe; the Linux-only `internal/proc` import is gone from `discovery`.
- [ ] `discovery.New(src osproc.Source)`; scanner driven by `AllPIDs()`+`Read` over the adapter; seen-set state machine unchanged.
- [ ] `discovery_test.go` updated to `osproc.Info` in the fake/seed only — **all five scanner tests and every `IsClaude` row keep their asserted outcomes**, plus new macOS-layout rows pass. `go test ./internal/discovery/...` green on Linux.
- [ ] `mapping.Resolver.Resolve`/`Reconcile` accept `osproc.Info`; `cmd/switchboard/main.go` wires `discovery.New(procSrc)`, `onClaudeAppeared(osproc.Info)`, and `dropStaleSessions(store, procSrc)`; no `internal/proc` import remains in `main.go`. `go build ./...` and `go vet ./...` green on Linux; `GOOS=darwin go build ./...` green.
- [ ] `internal/testsupport/child_darwin.go` (`//go:build darwin`) provides `SpawnTTYChild` with the same signature/contract as the Linux helper, via `x/sys/unix` `PosixOpenpt`/`Grantpt`/`Unlockpt`/`Ptsname` + `SysProcAttr{Setsid,Setctty,Ctty}`, no cgo, no new dependency.
- [ ] On a macOS host: the osproc conformance suite (`RunSourceContract`) reports a **non-empty tty** for `SpawnTTYChild` and an **empty tty** for `SpawnBareChild`; the osproc death test sees `onDeath` fire exactly once; discovery finds a real running `claude` (native, Homebrew, and npm installs all matched by the broadened predicate).
- [ ] The comm/`node` question (§1.5) is resolved by observation on a real Mac — recorded in DONE/memory; the `Args`-fallback is built only if a real install is observed running as comm `node`.

## 6. Risks

1. **comm/`node` ambiguity (HIGHEST).** Predicate gates on `comm == "claude"`. Docs say modern `claude` is a native process named `claude`, but no Mac has confirmed it (and a launcher script or legacy npm path could surface comm `node` with `claude` only in argv). If wrong, discovery finds nothing on that Mac. Mitigation: verify on hardware first; only then widen `osproc.Info` with `Args` and add a comm-`node` fallback. Do not build speculatively.
2. **pty test-child complexity (Mac-only).** The `Ctty` fd interpretation on Darwin is version-sensitive; `Setctty` may need the parent-side slave fd rather than child-fd 0. Cannot be validated on Linux. Mitigation: the conformance contract only asserts *tty non-empty*, giving latitude; iterate `Ctty` value against `RunSourceContract` on the Mac.
3. **`proc.Info` → `osproc.Info` migration churn.** Touches `discovery`, `discovery_test`, `mapping`, and `main.go`. The types are field-identical so each change is mechanical, but a missed call site is a compile error (caught by `go build ./...` on both `GOOS`). Mitigation: migrate `mapping.Resolve` in the same change, run `GOOS=darwin go build ./...`.
4. **Keeping Linux behavior identical.** The seen-set "Read only unseen pids" hot path must be preserved — driving off `Enumerate()` would regress it. Mitigation: Option A (cheap `AllPIDs()`), and the unchanged scanner tests are the regression guard. The broadened `IsClaude` must still reject `claude-impostor` (the `/claude` suffix check does, since `claude-impostor` does not end in `/claude`) — covered by the existing test row.
5. **Stale-drop ordering.** `dropStaleSessions` must move below `procSrc := stack.OSProc`; calling it before `procSrc` exists is a compile error and forgetting to pass `procSrc` silently reverts to the old path. Mitigation: the new signature forces the argument.
