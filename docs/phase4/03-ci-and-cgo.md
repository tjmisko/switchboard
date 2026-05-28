# Phase 4 — CI, build, and cgo decision

Switchboard's darwin enumeration backend almost certainly needs cgo (`libproc`), which removes the ability to cross-compile darwin from Linux and forces all darwin validation onto a `macos-latest` runner.

**Status: decision required (cgo vs cgo-free); the CI change is mechanical once decided.**

---

### 1. The core decision: cgo vs cgo-free

The OS process layer is the only OS-specific code in the tree. It is build-tagged:

- `internal/osproc/source_linux.go` — `//go:build linux` — real Linux backend (`/proc` + `pidfd_open`).
- `internal/osproc/source_darwin.go` — `//go:build darwin` — currently a **pure-Go `ErrUnsupported` stub**.

Everything else cross-compiles to darwin already: the WM backends (hyprland / sway-i3 / x11 via pure-Go `github.com/jezek/xgb`) and the terminal backends (wezterm / tmux) are pure Go. This is why `GOOS=darwin GOARCH=arm64 go build ./...` passes cleanly from Linux today, and why the existing `darwin-build` stub job is currently GREEN.

Phase 4.1 (enumerate: cwd / exe / tty) needs to read per-process metadata that macOS only exposes through `libproc`:

- `proc_pidpath(pid, …)` → executable path (**exe**)
- `proc_pidinfo(pid, PROC_PIDVNODEPATHINFO, …)` → current working directory (**cwd**)
- tty can be obtained either via `proc_pidinfo(PROC_PIDTBSDINFO)` (cgo) or `sysctl` (cgo-free).

These `libproc` symbols are not reachable through `golang.org/x/sys/unix` syscall wrappers in a practical way; the supported path is a small cgo shim that `#include <libproc.h>`.

Phase 4.2 (death watch) uses **kqueue** (`EVFILT_PROC` / `NOTE_EXIT`) via `golang.org/x/sys/unix`, which is **cgo-free**. So the cgo question is scoped entirely to enumeration, not to the death-watch path.

**Two options:**

| | cgo (recommended) | cgo-free |
|---|---|---|
| pid / ppid / comm | yes (`sysctl KERN_PROC`) | yes |
| tty | yes | yes (`sysctl`) |
| **exe** (executable path) | **yes** (`proc_pidpath`) | **no** (not available without libproc) |
| **cwd** (working directory) | **yes** (`proc_pidinfo VNODEPATHINFO`) | **no** |
| Cross-compile darwin from Linux | **NO** — needs a macOS C toolchain/SDK | yes |
| Validate darwin in CI | macOS runner only | Linux cross-build OR macOS runner |

**Recommendation: cgo.** `cwd` is part of the Observe headline feature, and `discovery.IsClaude`'s heuristic wants the executable path (**exe**). A cgo-free backend cannot supply either, so it would ship a structurally degraded Observe on macOS. Take the cgo path and accept the build-topology consequence below.

**Consequence of choosing cgo:** once `source_darwin.go` imports a cgo shim, `GOOS=darwin go build ./...` from Linux **FAILS** (no macOS SDK / C toolchain on the Linux dev box or on standard ubuntu runners). From that point, the *only* way to build or test darwin is on a macOS runner with `CGO_ENABLED=1` (the default on a native mac). The Linux jobs are unaffected — see §3.

---

### 2. CI changes (`.github/workflows/ci.yml`)

Replace the build-only `darwin-build` stub with a real darwin **test** job that runs `go vet`, `go build`, and `go test -race ./...` under `CGO_ENABLED=1`. Make it **blocking** (drop `continue-on-error`) once the cgo backend lands — cgo darwin can only be validated on this runner, so a non-blocking job would let darwin regressions merge silently. The stale comment (which references the pre-Phase-1.1 "expected to fail" state) is removed.

`macos-latest` is Apple-silicon (`arm64`) by default now, which matches the primary target. An optional `macos-13` (Intel `amd64`) leg gives amd64 coverage; it is included below but commented as optional to keep runner-minute cost down.

#### New darwin job (replaces the `darwin-build` block, lines 32–47)

```yaml
  test-darwin:
    name: test (darwin/${{ matrix.arch }})
    strategy:
      fail-fast: false
      matrix:
        include:
          - arch: arm64
            runner: macos-latest    # Apple silicon; primary target
          # Optional Intel coverage. Uncomment to also exercise darwin/amd64.
          # - arch: amd64
          #   runner: macos-13
    runs-on: ${{ matrix.runner }}
    env:
      CGO_ENABLED: "1"  # darwin osproc backend uses libproc (cgo); Xcode CLT is preinstalled on the runner
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: go vet ./...
      - run: go build ./...
      - run: go test -race ./...
```

#### Full diff against the current file

```diff
-  darwin-build:
-    # Stub (non-blocking). The OS process layer (/proc + pidfd_open) is
-    # Linux-only and not yet behind build tags, so `go build ./...` is expected
-    # to fail on macOS until Phase 1.1 moves it behind osproc_linux.go and
-    # Phase 4 adds the darwin backend (libproc + kqueue). This slot exists now so
-    # the matrix is visible; it must not block the required checks. The darwin
-    # test job lands in Phase 4.
-    name: darwin build (stub, non-blocking)
-    runs-on: macos-latest
-    continue-on-error: true
-    steps:
-      - uses: actions/checkout@v4
-      - uses: actions/setup-go@v5
-        with:
-          go-version-file: go.mod
-      - run: go build ./...
+  test-darwin:
+    name: test (darwin/${{ matrix.arch }})
+    strategy:
+      fail-fast: false
+      matrix:
+        include:
+          - arch: arm64
+            runner: macos-latest    # Apple silicon; primary target
+          # Optional Intel coverage. Uncomment to also exercise darwin/amd64.
+          # - arch: amd64
+          #   runner: macos-13
+    runs-on: ${{ matrix.runner }}
+    env:
+      CGO_ENABLED: "1"  # darwin osproc backend uses libproc (cgo); Xcode CLT is preinstalled on the runner
+    steps:
+      - uses: actions/checkout@v4
+      - uses: actions/setup-go@v5
+        with:
+          go-version-file: go.mod
+      - run: go vet ./...
+      - run: go build ./...
+      - run: go test -race ./...
```

The `test` (linux/amd64 + linux/arm64) job is **unchanged**. It keeps the default `CGO_ENABLED` (`0` when cross-compiling, but native here so effectively `1` — irrelevant since no Linux code needs cgo). The Linux build never sees the darwin cgo file because of build tags (§3), so Linux CI stays green with no mac.

If `test-darwin` is a required status check in branch protection, note that the check name is `test (darwin/arm64)` (the rendered matrix `name`), and update the required-checks list accordingly when you flip it to blocking.

---

### 3. Build-tag matrix

Confirms the darwin cgo file never leaks into Linux builds and the Linux file never leaks into darwin builds. `go vet ./...` / `go build ./...` on Linux **skip** the darwin file by build tag, so Linux CI compiles and stays green on a runner with no macOS SDK.

| File | Build tag | Compiles on `GOOS=linux` | Compiles on `GOOS=darwin` | cgo? |
|---|---|---|---|---|
| `internal/osproc/source_linux.go` | `//go:build linux` | yes | no (skipped) | no |
| `internal/osproc/source_darwin.go` | `//go:build darwin` | no (skipped) | yes | **yes** (after Phase 4) |
| WM backends (hyprland / sway-i3 / x11, `jezek/xgb`) | none / pure-Go | yes | yes | no |
| Terminal backends (wezterm / tmux) | none / pure-Go | yes | yes | no |
| `internal/testsupport/child_linux.go` | `//go:build linux` | yes | no (skipped) | no |
| `internal/testsupport/child_darwin.go` (**to be added**) | `//go:build darwin` | no (skipped) | yes | TBD (see §4) |

Because the cgo `import "C"` and `#include <libproc.h>` live exclusively inside `source_darwin.go` (guarded by `//go:build darwin`), the Linux toolchain never parses the cgo preamble. The Linux jobs therefore neither require a C cross-toolchain nor a macOS SDK. Symmetrically, the darwin job never compiles `source_linux.go` (`/proc`, `pidfd_open`), so darwin builds don't pull in Linux-only syscalls.

---

### 4. testsupport dependency (hard prerequisite)

`internal/testsupport/child_linux.go` is `//go:build linux` and provides `SpawnTTYChild`, `SpawnSleep`, and `DeadPID`, which the osproc tests and the conformance live tests depend on. With only the Linux file present, the `internal/testsupport` package has **no buildable source** under `GOOS=darwin`, so the darwin test job would fail to compile every package that imports it — before any darwin logic is even exercised.

**A `internal/testsupport/child_darwin.go` (`//go:build darwin`) providing the same `SpawnTTYChild` / `SpawnSleep` / `DeadPID` symbols is a hard prerequisite for the `test-darwin` job.** It must land in the same PR as the cgo darwin backend (and as the CI change). Its detailed spec lives in the companion testsupport doc; this doc only flags the dependency and the ordering constraint.

---

### 5. Interim option and recommended sequencing

Until a mac (CI runner or local) is actually exercised, the pure-Go `ErrUnsupported` stub plus the existing non-blocking build-only `darwin-build` job is a valid status quo: CI stays green, darwin cross-builds from Linux, and nothing blocks.

**Recommended sequencing — do all of the following in one PR:**

1. Add the cgo `source_darwin.go` backend (libproc enumerate + kqueue death watch).
2. Add `internal/testsupport/child_darwin.go` (§4).
3. Replace the stub `darwin-build` job with the blocking `test-darwin` job (§2).

Doing these together avoids an intermediate state where either (a) the CI job is blocking but the backend/testsupport isn't ready (red CI), or (b) the cgo backend lands but no mac ever runs it (untested code merged behind a non-blocking stub). The moment the cgo file exists, Linux can no longer build darwin — so the macOS test job must arrive in the same change to preserve any darwin signal at all.

Do **not** flip the job to blocking before the backend lands; until then keep the non-blocking build-only stub.

---

### 6. Risks

- **Runner cost / availability.** `macos-latest` (and `macos-13`) runners consume GitHub Actions minutes at a higher multiplier than Linux (historically 10x on private repos) and can queue longer. Mitigate by keeping the darwin matrix to `arm64` only by default and leaving `macos-13`/amd64 commented out; enable it only if amd64 regressions appear.
- **`setup-go` on macOS.** `actions/setup-go@v5` with `go-version-file: go.mod` works on macOS runners; the runner images also ship a recent Go preinstalled, but pinning via `go.mod` keeps parity with the Linux jobs.
- **Xcode Command Line Tools / SDK.** The `macos-latest` and `macos-13` runner images come with Xcode and the Command Line Tools preinstalled, so `clang` and the macOS SDK (`<libproc.h>`) are available — `CGO_ENABLED=1` builds work out of the box with no extra install step. (Confirm against the current runner image release notes at PR time; this has been stable but is image-dependent.)
- **Virtualized-runner flakiness.** `kqueue` `EVFILT_PROC`/`NOTE_EXIT` death watch and `libproc` enumeration run inside a virtualized macOS runner; process timing and pid reuse can be flakier than on bare metal. Write the darwin live tests with generous timeouts and avoid asserting on exact scheduling, and keep `fail-fast: false` so an arm64 flake doesn't mask amd64 results.
