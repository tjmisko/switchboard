# Phase 4 — macOS support (planning)

> **Status: NOT STARTED — gated on a Mac + a CI decision.** This directory is the
> implementation-ready plan for Phase 4 of the [portability pivot](../portability-plan.md).
> Every other phase (0–3, 5) is done on Linux; Phase 4 opens the second platform
> and is the one piece that cannot be written or verified honestly from the Linux
> dev box. These docs spell out exactly what to build, in what order, and the
> decisions that must be made first.

## Why it's blocked

1. **It needs real macOS hardware.** The Observe tier's headline fields (cwd, exe)
   are only available on macOS through `libproc` (`proc_pidpath`,
   `proc_pidinfo`), which requires **cgo**. cgo darwin cannot cross-compile from
   Linux (no macOS SDK/toolchain), so the backend can neither be compiled nor run
   here. Writing unverifiable cgo blind is exactly what we avoid.
2. **It changes CI topology.** The darwin CI job is currently a cross-compiled,
   build-only, non-blocking stub that passes *because* `source_darwin.go` is a
   pure-Go `ErrUnsupported` stub. The moment that file uses cgo, `GOOS=darwin go
   build` from Linux fails — so Phase 4 needs a **macOS CI runner** with
   `CGO_ENABLED=1`, which is a real infra decision.

## Decisions required before starting

| # | Decision | Recommendation | Detail |
|---|----------|----------------|--------|
| D1 | cgo vs cgo-free darwin enumerate | **cgo** — cgo-free can't supply cwd/exe (loses the Observe headline + weakens `IsClaude`) | [01](01-osproc-darwin-enumerate.md) §3, [03](03-ci-and-cgo.md) §1 |
| D2 | macOS CI runner (cost, blocking?) | **Add a blocking `test (darwin/arm64)` job on `macos-latest`**, in the same PR as the backend; arm64 only by default to limit minutes | [03](03-ci-and-cgo.md) §2,§5 |
| D3 | Navigate scope on macOS | **Observe-first** (works on any Mac once 4.1/4.2 land). Navigate is opt-in: tmux + wezterm already work; add **AeroSpace** as the WM backend | [04](04-navigate-matrix-macos.md) §3 |

## Work breakdown

| Item | Doc | cgo? | Verifiable on Linux? | Notes |
|------|-----|------|----------------------|-------|
| 4.1 enumerate (`Enumerate`/`Read`: pid/ppid/comm/exe/cwd/tty) | [01](01-osproc-darwin-enumerate.md) | **yes** (libproc) | no | sysctl `KERN_PROC_ALL` for the list + cheap fields; `proc_pidpath`/`proc_pidinfo` for exe/cwd; `devname_r` for tty |
| 4.2 death watch (`Watch`/`Stop`) | [02](02-osproc-darwin-deathwatch.md) | no (kqueue via `x/sys/unix`) | compiles/vets only | per-pid kqueue `EVFILT_PROC`/`NOTE_EXIT`, mirrors the Linux pidfd watcher 1:1 |
| CI + build/cgo | [03](03-ci-and-cgo.md) | — | n/a | replace the stub with a real blocking darwin test job; Linux jobs unaffected (build tags) |
| 4.3 Navigate matrix (WM/terminal) | [04](04-navigate-matrix-macos.md) | no | partially | tmux/wezterm work today; AeroSpace (no event stream → poll) recommended WM; yabai (events, SIP friction) second; stock macOS = Observe-only |
| discovery→osproc unification + `IsClaude` + `child_darwin.go` | [05](05-discovery-and-testsupport.md) | `child_darwin.go` cgo-free | the unification + `IsClaude` tweak: **yes**; the darwin pty child: no | repoint discovery onto `osproc.Source` (deferred from Phase 1); broaden `IsClaude` for macOS install paths; add a darwin pty test child |

## Recommended sequencing

Most of Phase 4 must land in **one PR on a Mac**, because the instant
`source_darwin.go` uses cgo, Linux can no longer build darwin — so the macOS
test job, the backend, and the darwin test-support must arrive together or CI
loses all darwin signal.

1. **Linux-doable prep (can be a separate, earlier PR):** repoint `discovery`
   onto `osproc.Source` and broaden `discovery.IsClaude` for macOS install paths
   ([05](05-discovery-and-testsupport.md) §1–§2). This compiles and tests on
   Linux and removes the dual process-reading path left over from Phase 1.
2. **On a Mac, one PR:**
   - `internal/testsupport/child_darwin.go` (pty child via `x/sys/unix`
     `PosixOpenpt`/`Grantpt`/`Unlockpt`/`Ptsname` — no cgo, no new dep)
     ([05](05-discovery-and-testsupport.md) §3).
   - `internal/osproc/source_darwin.go`: cgo enumerate (4.1) + kqueue watch (4.2).
   - `.github/workflows/ci.yml`: replace the stub with the blocking
     `test (darwin/arm64)` job ([03](03-ci-and-cgo.md) §2).
   - Run `conformance.RunSourceContract` + the darwin death tests on the Mac.
3. **Opt-in Navigate (later):** an `wm/aerospace.go` backend (and optionally
   `terminal/iterm2.go`); tmux+wezterm already give terminal Navigate for free.

## Highest-risk unknowns to confirm on hardware

- **Does `claude` appear as `comm == "claude"` on macOS, or as `node`?** Research
  says the CLI is a native signed binary since v2.1.113 (so `comm` should be
  `claude`), but this is the single load-bearing assumption for discovery —
  **verify first on a real Mac** ([05](05-discovery-and-testsupport.md) §1).
- **`devname_r(e_tdev, S_IFCHR)` resolves to a clean `/dev/ttysNNN`** for live
  pty sessions, and `NODEV` surfaces as `Tdev == -1` ([01](01-osproc-darwin-enumerate.md) §2,§6).
- **`Read(deadpid)` error code** (`ESRCH` vs `EIO` from `SysctlKinfoProc`) maps to
  `ErrGone` ([01](01-osproc-darwin-enumerate.md) §6).
- **The `SysProcAttr{Setctty, Ctty}` fd value** for the darwin pty test child
  ([05](05-discovery-and-testsupport.md) §3).

## What stays unchanged

The neutral `osproc.Source` / `wm.Manager` / `terminal.Locator` interfaces and
the `internal/conformance` contract suites do **not** change — the darwin
backend satisfies the exact same contracts as Linux, which is the entire point
of the Phase-1 seams. Assertions are written against neutral observables
(non-empty tty, `ErrGone`, onDeath-exactly-once), never against `/proc` or
`/dev/pts` literals, so the same suite validates macOS (`/dev/ttysNNN`, kqueue)
without modification.
