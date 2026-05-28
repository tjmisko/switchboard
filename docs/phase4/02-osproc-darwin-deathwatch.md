# Phase 4.2 — macOS death watching (osproc darwin / kqueue)

Phase 4.2 registers one kqueue `EVFILT_PROC`/`NOTE_EXIT` event per watched pid and fires `onDeath` exactly once when the kernel reports the process exited — the Darwin sibling of the Linux pidfd watcher.

**Status: implementable cgo-free, but compile/run needs a Mac.** `unix.Kqueue`, `unix.Kevent`, and `unix.Kevent_t` are all pure-syscall wrappers (no `import "C"`), so this file builds and vets from any host with `GOOS=darwin`; only *running* the death tests requires a macOS machine. This is why 4.2 is far less blocked than 4.1 (libproc enumeration, which does need cgo).

## 1. Goal and contract

Implement `darwinSource.Watch/Stop/Watched` so that the package-neutral `conformance.RunSourceContract` "watch fires onDeath exactly once on death" assertion and a darwin sibling of `internal/osproc/source_linux_test.go` both pass. The structure mirrors `source_linux.go` exactly: a `sync.Mutex` guarding `watched map[int]context.CancelFunc`, one goroutine per watched pid, `onDeath` fired exactly once, duplicate `Watch` a no-op, `Stop` cancelling without firing, an already-dead pid firing immediately and never entering the map, and `Watched()` draining after death (no goroutine leak).

This doc covers **death watching only**. `Enumerate`/`Read` (libproc) are a separate doc; `darwinSource` will gain those methods there. `Watch`/`Stop`/`Watched` here are self-contained and share only the struct.

## 2. kqueue EVFILT_PROC / NOTE_EXIT model

A kqueue is created with `unix.Kqueue()` (returns an `int` fd, cgo-free). To watch a process for exit, register a change event with:

| Field    | Value                            | Type / note                                  |
|----------|----------------------------------|----------------------------------------------|
| `Ident`  | `uint64(pid)`                    | `uint64` on darwin — must cast the `int` pid |
| `Filter` | `unix.EVFILT_PROC`               | `int16`, value `-0x5`                         |
| `Flags`  | `EV_ADD \| EV_ONESHOT \| EV_RECEIPT` | `uint16`                                  |
| `Fflags` | `unix.NOTE_EXIT`                 | `uint32`, value `0x80000000`                 |
| `Data`   | `0`                              | on the *returned* event, exit status         |
| `Udata`  | `nil`                            | unused                                       |

Confirmed against `x/sys@v0.27.0` `ztypes_darwin_arm64.go`: `Kevent_t{ Ident uint64; Filter int16; Flags uint16; Fflags uint32; Data int64; Udata *byte }`. Constants confirmed in `zerrors_darwin_arm64.go`: `EVFILT_PROC = -0x5`, `NOTE_EXIT = 0x80000000`, `EV_ADD = 0x1`, `EV_ONESHOT = 0x10`, `EV_RECEIPT = 0x40`, `EV_ERROR = 0x4000`.

**On the flags:**
- `EV_ADD` — register the filter.
- `EV_ONESHOT` — auto-remove after it fires once. `NOTE_EXIT` is intrinsically a one-time event and the kernel forces oneshot semantics anyway; setting `EV_ONESHOT` makes the intent explicit and guarantees the kqueue holds nothing after the exit event is read. This is the primary "exactly once" guard.
- `EV_RECEIPT` — forces an `EV_ERROR` result event to be returned for *every* change entry immediately, with `Data` carrying the errno (0 = success). This lets us do the registration and read its outcome in **one** `Kevent` call, so we can detect the already-dead `ESRCH` case synchronously inside `Watch` (matching the Linux `PidfdOpen` ESRCH path) instead of racing it on the wait loop.

**Note on `SetKevent`:** `unix.SetKevent(k, fd, mode, flags)` exists on darwin but sets only `Ident`/`Filter`/`Flags` — it does **not** set `Fflags`. Since we need `Fflags = NOTE_EXIT`, set the struct fields directly rather than using the helper.

**The `Kevent` call signature** (confirmed in `syscall_bsd.go`):
```go
func Kevent(kq int, changes, events []Kevent_t, timeout *Timespec) (n int, err error)
```
`changes` is the registration list, `events` is the output buffer, `timeout` is a `*unix.Timespec` (nil blocks forever; a non-nil value bounds the wait, which we use to stay cancellable).

### Decision: one kqueue fd per watched pid

Two designs:
- **Per-pid kqueue** (recommended): each `Watch` allocates its own kqueue fd and its own goroutine, exactly mirroring "one pidfd + one goroutine per pid" on Linux. `Stop`/cleanup close just that fd; no demux logic; the goroutine owns one identity end to end.
- **Shared kqueue + demux loop**: one kqueue, one wait goroutine, dispatch by `event.Ident`. Scales to thousands of pids with one fd and one goroutine, but needs a registration map, a self-pipe / `EVFILT_USER` wakeup to add/remove pids and to unblock on shutdown, and careful demux — materially more code and more failure modes.

**Recommendation: per-pid kqueue.** switchboard watches a handful of agent processes, not thousands; fd pressure is a non-issue. Matching the Linux structure one-to-one keeps the two backends reviewable side by side and lets the darwin test file be a near-verbatim copy of the linux one. (If a future profile shows fd/goroutine pressure from many watched pids, revisit the shared-kqueue design — it is the scalable option.)

## 3. Already-dead pid

Registering `EVFILT_PROC`/`NOTE_EXIT` for a pid with no live process fails with `ESRCH` ("no such process"). With `EV_RECEIPT` set, that error surfaces synchronously: either the `Kevent` call returns `err == ESRCH`, or it returns the error inside the receipt event (`events[0].Flags & EV_ERROR != 0` with `events[0].Data == ESRCH`). Handle both, exactly like the Linux ESRCH branch — schedule `onDeath` and do **not** track:

```go
if errors.Is(err, unix.ESRCH) {
    unix.Close(kq)
    s.mu.Unlock()
    go onDeath() // already dead
    return nil
}
```

## 4. Exactly-once + cancellation

`EV_ONESHOT` removes the filter after it fires, so the kernel never delivers a second `NOTE_EXIT` for the same registration — the structural exactly-once guarantee. The goroutine fires `onDeath` once and returns immediately after.

`Kevent` blocks while waiting for events. `Stop` must unblock it. Two options:
- **Timeout-poll loop** (recommended): pass a 1s `*unix.Timespec`; on timeout (`n == 0`) re-check `ctx.Err()` and loop. This is symmetric with the Linux pidfd watcher's `unix.Poll(pfd, 1000)` tick, so `Stop` responsiveness and the test's `waitWatchedEmpty` 3s drain budget carry over unchanged.
- **Close-the-fd**: have `Stop` close the kqueue fd to fault the blocked `Kevent`. Tighter latency, but closing an fd another goroutine is blocked in is a sharp edge (EBADF races, the closer must not also be the cleanup path) and diverges from the Linux model.

**Recommendation: timeout-poll loop**, for symmetry and reviewability. On exit the goroutine deletes the map entry and closes the kq fd via `defer`.

## 5. Code sketch (`source_darwin.go`)

```go
//go:build darwin

package osproc

import (
    "context"
    "errors"
    "sync"

    "golang.org/x/sys/unix"
)

func newSource() Source { return newDarwinSource() }

// newDarwinSource builds the concrete Darwin source. Tests use it directly to
// reach Watched(), which the Source interface does not expose.
func newDarwinSource() *darwinSource {
    return &darwinSource{watched: make(map[int]context.CancelFunc)}
}

// darwinSource watches deaths with kqueue EVFILT_PROC/NOTE_EXIT: one kqueue fd
// and one goroutine per watched pid. The kernel delivers NOTE_EXIT when the
// process exits regardless of how it died (Ctrl+C, /exit, kill -9), mirroring
// the Linux pidfd watcher. kqueue is cgo-free, so this builds from any host.
type darwinSource struct {
    mu      sync.Mutex
    watched map[int]context.CancelFunc
}

// Watch registers pid for NOTE_EXIT on a private kqueue. onDeath is called
// exactly once, from a background goroutine, when the process exits. A
// duplicate Watch for the same pid returns nil without a second watcher.
func (s *darwinSource) Watch(parent context.Context, pid int, onDeath func()) error {
    s.mu.Lock()
    if _, dup := s.watched[pid]; dup {
        s.mu.Unlock()
        return nil
    }

    kq, err := unix.Kqueue()
    if err != nil {
        s.mu.Unlock()
        return err
    }

    // EV_RECEIPT makes the registration's outcome come back in `events`
    // synchronously, so an already-dead pid (ESRCH) is detected here, not on
    // the wait loop — symmetric with the Linux PidfdOpen ESRCH path.
    change := unix.Kevent_t{
        Ident:  uint64(pid),
        Filter: unix.EVFILT_PROC,
        Flags:  unix.EV_ADD | unix.EV_ONESHOT | unix.EV_RECEIPT,
        Fflags: unix.NOTE_EXIT,
    }
    var receipt [1]unix.Kevent_t
    n, err := unix.Kevent(kq, []unix.Kevent_t{change}, receipt[:], nil)
    if err == nil && n > 0 && receipt[0].Flags&unix.EV_ERROR != 0 && receipt[0].Data != 0 {
        err = unix.Errno(receipt[0].Data) // receipt carried the errno
    }
    if err != nil {
        unix.Close(kq)
        s.mu.Unlock()
        if errors.Is(err, unix.ESRCH) {
            go onDeath() // already dead
            return nil
        }
        return err
    }

    ctx, cancel := context.WithCancel(parent)
    s.watched[pid] = cancel
    s.mu.Unlock()

    go func() {
        defer unix.Close(kq)
        defer func() {
            s.mu.Lock()
            delete(s.watched, pid)
            s.mu.Unlock()
        }()

        // 1s tick keeps Stop responsive without an indefinite block, matching
        // the Linux poll tick. Kevent with a timeout returns n==0 on expiry.
        timeout := unix.Timespec{Sec: 1, Nsec: 0}
        var events [1]unix.Kevent_t
        for {
            if ctx.Err() != nil {
                return
            }
            n, err := unix.Kevent(kq, nil, events[:], &timeout)
            if errors.Is(err, unix.EINTR) {
                continue // interrupted — do NOT treat as death
            }
            if err != nil {
                return
            }
            if n == 0 {
                continue // timeout — re-check ctx and loop
            }
            // EV_ONESHOT removed the filter; any delivered NOTE_EXIT is the
            // single death notification. (Defensive: confirm it's our pid.)
            if events[0].Ident == uint64(pid) {
                onDeath()
                return
            }
        }
    }()
    return nil
}

func (s *darwinSource) Stop(pid int) {
    s.mu.Lock()
    cancel, ok := s.watched[pid]
    s.mu.Unlock()
    if ok {
        cancel()
    }
}

// Watched returns the PIDs currently being watched. Test/introspection only.
func (s *darwinSource) Watched() []int {
    s.mu.Lock()
    defer s.mu.Unlock()
    out := make([]int, 0, len(s.watched))
    for pid := range s.watched {
        out = append(out, pid)
    }
    return out
}
```

Notes on the sketch:
- `unix.Errno(receipt[0].Data)` — `Data` is `int64`; the errno is small and positive, so the cast to `unix.Errno` is safe and `errors.Is(_, unix.ESRCH)` works on it.
- The `events[0].Ident == uint64(pid)` check is defensive (a per-pid kqueue only ever delivers this pid) but cheap and documents intent.
- The `Stop`/`Watched` bodies are byte-for-byte the Linux versions with `linuxSource`→`darwinSource`; only `Watch` differs in mechanism.

## 6. EINTR

`unix.Kevent` can return `EINTR` if a signal interrupts the blocked syscall. As on Linux (`unix.Poll` EINTR), `continue` the loop — re-check `ctx.Err()` and re-arm the wait. **Never** treat EINTR as death. Like the Linux suite, this is covered by code inspection rather than a deterministic test (it cannot be triggered reliably), and should carry the same comment in `docs/decisions.md`.

## 7. File plan & build tags

- **`internal/osproc/source_darwin.go`** (`//go:build darwin`) — holds `newSource`, `newDarwinSource`, the `darwinSource` struct, and `Watch`/`Stop`/`Watched`. The libproc `Enumerate`/`Read` methods (separate doc) attach to the same struct in this file or a sibling; if they land first, add only the watch methods here and drop the duplicate `newSource`/struct decl. If watch lands first, put `Enumerate`/`Read` stubs returning a not-implemented error so the package compiles for `GOOS=darwin`. Splitting watch into `watch_darwin.go` is fine too; keep one `//go:build darwin` constraint and one definition of the struct + `newSource`.
- **`internal/osproc/source_darwin_test.go`** (`//go:build darwin`) — a near-verbatim copy of `source_linux_test.go`: `watching`, `waitWatchedEmpty` (referencing `*darwinSource`), `TestWatchFiresOnDeathExactlyOnce`, `TestDuplicateWatchIsNoOp`, `TestStopCancelsWithoutFiringOnDeath`, `TestWatchAlreadyDeadFiresImmediately`. Substitute `newLinuxSource`→`newDarwinSource`. The 1s tick / 3s drain budgets transfer unchanged.

**Dependency (flagged, owned by another doc): `internal/testsupport/child_darwin.go`.** `testsupport/child.go` (`SpawnSleep`, `Child.Kill`, `DeadPID`) is platform-neutral and reusable as-is. But `testsupport/child_linux.go` (`SpawnTTYChild`, which uses `/dev/ptmx` + `TIOCSPTLCK`/`TIOCGPTN`) is `//go:build linux` and will not build on darwin. A `child_darwin.go` is needed providing a darwin `SpawnTTYChild` (open `/dev/ptmx`, `unix.IoctlGetInt(..., TIOCPTYGNAME)`/`grantpt`/`unlockpt`-equivalent, or `posix_openpt`-style pty) and confirming `DeadPID()` is platform-neutral. The death tests in this doc need only `SpawnSleep`/`Kill`/`DeadPID` (all neutral), so they can land **before** the tty helper; the conformance suite's tty assertions need `child_darwin.go`. Track `child_darwin.go` as a cross-cutting dependency, resolved in the testsupport/conformance doc.

## 8. Definition of Done

- `GOOS=darwin go build ./internal/osproc/...` and `go vet` pass from CI (cgo-free, host-independent).
- On a Mac: killing a `claude` process being watched fires `onDeath` exactly once and promptly (well within the 3s contract budget).
- `source_darwin_test.go` passes on a Mac: exactly-once-on-death, duplicate-Watch no-op, Stop-without-firing, already-dead-fires-immediately, and `Watched()` drains (no goroutine leak).
- `conformance.RunSourceContract`'s "watch fires onDeath exactly once on death" assertion passes with a `SourceFixture` backed by `newDarwinSource()`.

## 9. Risks

- **NOTE_EXIT privilege**: monitoring a same-uid process for `NOTE_EXIT` requires **no special privilege** on macOS (the agent processes switchboard watches are the same user). Watching another user's / a root process can fail with `EPERM` (or `ESRCH` if hidden) — out of scope; same-uid agents are the only target. Confirm on first run.
- **pid reuse race**: a pid can be reaped and recycled onto a new process between `Read`/decision time and `Watch`. This race exists on Linux too (pidfd narrows it); kqueue's `EVFILT_PROC` binds at registration, so once registered we watch *that* process struct. The residual race is the gap between obtaining the pid and the `Kevent` registration — accept it as on Linux; it is bounded and benign for short-lived agents. `DeadPID()` in the already-dead test must stay well above the system pid ceiling to avoid recycling onto a live process.
- **`Ident` is `uint64` on darwin** (vs the `int` pid): always `uint64(pid)` on register and compare `events[0].Ident == uint64(pid)`. A forgotten cast would not compile, but the comparison cast is easy to miss.
- **Arch differences**: `Kevent_t` layout is identical for `darwin_amd64` and `darwin_arm64` (both have the same six fields; verified). `SetKevent` exists on both arches but omits `Fflags`, so we set fields directly regardless of arch. No arch-specific code needed.
- **`EV_RECEIPT` semantics**: relies on the receipt event being delivered for the change even on success (Data == 0). If a future macOS changed this, the synchronous ESRCH detection would degrade to detection-on-wait; harmless (the oneshot would simply fire `onDeath` on the next tick) but worth a comment.

## Sources

- [KQUEUE(2) — Apple/Xcode man pages](https://keith.github.io/xcode-man-pages/kqueue.2.html) — `EVFILT_PROC`/`NOTE_EXIT` semantics, `EV_ONESHOT`/`EV_RECEIPT` behavior, exit status in `data`.
- [apple/darwin-xnu `bsd/sys/event.h`](https://github.com/apple/darwin-xnu/blob/main/bsd/sys/event.h) — kevent struct field types and flag/note constant values.
- [kevent(2) — daemon-systems.org](https://www.daemon-systems.org/man/kevent.2.html) — `EV_RECEIPT` forces `EV_ERROR`, errno returned in `data`; `EVFILT_PROC` is self-clearing/oneshot.
- [golang.org/x/sys/unix — pkg.go.dev](https://pkg.go.dev/golang.org/x/sys/unix) — `Kqueue`, `Kevent`, `Kevent_t`, `SetKevent` API (cross-checked against the local `x/sys@v0.27.0` source).
