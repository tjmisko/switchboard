# Phase 4.1 — macOS process enumeration (osproc darwin)

One-shot summary: implement the macOS `osproc.Source` enumeration tier (`Enumerate`/`Read`) — PID, PPID, Comm, Exe, CWD, TTY — using a `sysctl(KERN_PROC_ALL)` scan for the process list + per-pid `proc_pidinfo`/`proc_pidpath` (libproc, cgo) for the path-bearing fields, with `dev_t → /dev/ttysNNN` resolution via `devname(3)`.

> **Status: BLOCKED — needs a Mac to compile/verify (no macOS SDK on the Linux dev box).** Everything below is implementation-ready, but the cgo `import "C"` block, `proc_pidinfo` calls, and `RunSourceContract` run can only be compiled and exercised on a macOS host (arm64 primary, amd64 secondary). The Linux box can typecheck the non-cgo sysctl sketch via `GOOS=darwin go vet` only for the pure-Go portion; the cgo portion will not build here.

## Scope

This doc covers **4.1 enumeration only**: turning the stub `darwinSource` into a real backend whose `Enumerate()` and `Read(pid)` populate a fully-formed `osproc.Info` for every visible process. Death watching (`Watch`/`Stop` via kqueue `EVFILT_PROC`/`NOTE_EXIT`) is **out of scope** and is documented separately in `docs/phase4/02-osproc-darwin-watch.md`. The `Watch`/`Stop` methods remain the `ErrUnsupported`/no-op stubs after this phase.

The neutral contract (`internal/conformance.RunSourceContract`) is the acceptance oracle. It asserts, against *observables only*:
- `Enumerate()` surfaces an interactive child with **non-empty TTY and non-empty CWD** (it checks non-emptiness, never the `/dev/pts` vs `/dev/ttys` literal).
- A bare (non-tty) child has **empty TTY**.
- `Read(deadpid)` returns the `ErrGone` sentinel.
- `Read(masked)` (a process whose exe is unobtainable) returns **empty `Exe` with no error**.
- `Watch` fires `onDeath` exactly once (validated in 02, not here).

So Comm, PPID, Exe, CWD, and TTY must be populated equivalently to the Linux `/proc` backend, with the same "unobtainable ⇒ empty string, not error" discipline for Exe/CWD and the same `ErrGone` mapping for dead pids.

## 1. PID enumeration

Two candidate mechanisms:

| Mechanism | cgo? | Returns | Notes |
|---|---|---|---|
| `proc_listallpids(buf, size)` (libproc) | **yes** | bare `int32` pid array | Simple, but needs a sizing pre-call and races a growing table. |
| `sysctl({CTL_KERN, KERN_PROC, KERN_PROC_ALL})` → `[]kinfo_proc` | **no** | full `kinfo_proc` per process | One syscall delivers pid **+ ppid + comm + e_tdev** for *every* process. |

**Recommendation: use the sysctl `KERN_PROC_ALL` scan.** It is reachable cgo-free through `golang.org/x/sys/unix` (verified in v0.27.0): `unix.SysctlKinfoProcSlice` does the size-probe / read-into-buffer / size-mismatch retry loop for us and returns `[]unix.KinfoProc`. Crucially this *also* delivers three of our six fields (PPID, Comm, TTY-device) in the same pass, so even though we still need libproc/cgo for Exe and CWD, the enumeration loop is anchored on a single cheap syscall rather than `proc_listallpids` + N pid probes.

Confirmed available in the pinned dependency (`golang.org/x/sys@v0.27.0`):
- `func SysctlKinfoProcSlice(name string, args ...int) ([]KinfoProc, error)` — `internal/.../unix/syscall_darwin.go:519`.
- `const SizeofKinfoProc = 0x288`, `type KinfoProc struct { Proc ExternProc; Eproc Eproc }` — `ztypes_darwin_arm64.go`.
- `CTL_KERN = 0x1`. The `KERN_PROC`/`KERN_PROC_ALL` MIB selectors are passed by name to the string-MIB resolver, so we call by the textual form `"kern.proc.all"` rather than assembling numeric MIBs by hand (the helper resolves the string to a MIB via `sysctlnametomib`).

Code sketch (cgo-free portion — lives in the same cgo file but uses no `C.` symbols):

```go
// scanProcs returns every process the current uid can see, as raw kinfo_proc
// records. One sysctl call; the slice is a point-in-time snapshot.
func scanProcs() ([]unix.KinfoProc, error) {
	// "kern.proc.all" == {CTL_KERN, KERN_PROC, KERN_PROC_ALL}.
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	return procs, nil
}
```

> Note: `KERN_PROC_ALL` returns *all* processes on the box, including other users'. We do not filter by uid here — discovery (`IsClaude`) filters by comm/exe downstream, and the per-pid libproc reads naturally fail closed for processes we cannot inspect (see §3, §6). If a uid filter is later wanted, `"kern.proc.uid"` + the current `unix.Getuid()` narrows the set kernel-side.

## 2. Per-field retrieval

### PPID — from the sysctl record (no extra call)

`KinfoProc.Eproc.Ppid` (`int32`). Verified layout in `ztypes_darwin_arm64.go`:

```go
type Eproc struct {
	Paddr  uintptr
	Sess   uintptr
	Pcred  Pcred
	Ucred  Ucred
	Vm     Vmspace
	Ppid   int32   // <-- PPID
	Pgid   int32
	Jobc   int16
	Tdev   int32   // <-- controlling tty dev_t (see TTY below)
	Tpgid  int32
	// ...
}
```

PID itself is `KinfoProc.Proc.P_pid` (`int32`).

### Comm — from the sysctl record (no extra call), with a libproc fallback

`KinfoProc.Proc.P_comm` is `[17]byte` in `x/sys/unix` (NUL-terminated; the kernel's `MAXCOMLEN` is **16** chars + a slot). This matches the Linux truncation contract: `comm` is truncated to ~16 chars on **both** platforms, so `discovery.IsClaude`'s `comm == "claude"` exact match works identically. Trim at the first NUL:

```go
func goStr(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}
// comm := goStr(kp.Proc.P_comm[:])
```

Equivalent libproc field if we ever switch to the per-pid path: `proc_pidinfo(pid, PROC_PIDTBSDINFO, ...)` → `struct proc_bsdinfo`, fields `pbi_comm[MAXCOMLEN]` (16, truncated — same as sysctl) and `pbi_name[2*MAXCOMLEN]` (32, the *un*truncated accounting name). We prefer the sysctl `P_comm` to match Linux's truncation behavior exactly; `pbi_name` is intentionally **not** used (it would diverge from the Linux 15/16-char `comm` and break the cross-platform contract).

### Exe (full path) — `proc_pidpath` (libproc, cgo required)

```c
#include <libproc.h>
int proc_pidpath(int pid, void *buffer, uint32_t buffersize);
```

Buffer must be `PROC_PIDPATHINFO_MAXSIZE` = `4 * MAXPATHLEN` = `4 * 1024` = **4096** bytes. Returns the byte length on success (path is NUL-terminated within the buffer), `0` on failure with `errno` set (`ESRCH` if gone, `EPERM` if not permitted).

**There is no clean cgo-free equivalent.** The only pure-sysctl route to an executable path is `KERN_PROCARGS2`, which returns a raw blob (argc as `int`, then exec path, then NUL-padded argv/envp) that must be hand-parsed and is fragile across releases and for setuid/restricted targets. We deliberately do **not** go down that road; `proc_pidpath` is the supported API. On any failure we set `Exe = ""` (not an error) — this directly satisfies the contract's "`Read(masked)` ⇒ empty Exe, no error", and matches Linux returning `""` when `/proc/<pid>/exe` is unreadable.

### CWD — `proc_pidinfo(PROC_PIDVNODEPATHINFO)` (libproc, cgo required)

```c
#include <sys/proc_info.h>
#include <libproc.h>
#define PROC_PIDVNODEPATHINFO 9

struct vnode_info_path {
	struct vnode_info vip_vi;
	char              vip_path[MAXPATHLEN]; // MAXPATHLEN == 1024
};
struct proc_vnodepathinfo {
	struct vnode_info_path pvi_cdir; // current working directory  <-- this
	struct vnode_info_path pvi_rdir; // root directory
};
```

Call: `proc_pidinfo(pid, PROC_PIDVNODEPATHINFO, 0, &vpi, sizeof(vpi))`; success when the return value `== sizeof(struct proc_vnodepathinfo)`. CWD is the NUL-terminated `vpi.pvi_cdir.vip_path`. **No cgo-free path exists** for cwd. On failure ⇒ `CWD = ""` (matches Linux `/proc/<pid>/cwd` unreadable ⇒ `""`).

### TTY — the trickiest field: `e_tdev` (`dev_t`) → `/dev/ttysNNN`

The controlling-terminal device number is already in hand from the sysctl scan: `KinfoProc.Eproc.Tdev` (`int32`, this *is* the `dev_t`). It is also available via `proc_bsdinfo.e_tdev` (`uint32`) if using the per-pid path. The sentinel for "no controlling terminal" is **`NODEV == -1`** (i.e. `(dev_t)-1`); as the `int32` `Tdev` this is `-1`. When `Tdev == -1` ⇒ `TTY = ""` (the contract's bare-child case).

To turn a non-`NODEV` `dev_t` into the `/dev/ttysNNN` string, **use `devname(3)`** (cgo — it lives in libc, no extra link flags):

```c
#include <stdlib.h>      // devname
#include <sys/types.h>
#include <sys/stat.h>    // S_IFCHR
char *devname(dev_t dev, mode_t type);
```

`devname(tdev, S_IFCHR)` returns the bare device name **without** the `/dev/` prefix (e.g. `"ttys003"`); we prepend `/dev/`. This is exactly what `sudo` and Emacs's `process-attributes` do on macOS. Two caveats:
1. `devname` consults `/dev` and a name cache; it can return a synthesized `"#B/C"`-style string if the device is not found in `/dev`. Treat a result containing `/` (other than our own prefix) or starting with `#` as "no clean name" ⇒ `TTY = ""`. In practice for live login/pty sessions the lookup resolves to `ttysNNN`.
2. `devname` is **not** thread-safe (returns a static buffer). Use `devname_r(dev, type, buf, len)` instead, which writes into a caller buffer and is reentrant — important because `Enumerate` may be called concurrently with the watcher goroutines:

```c
char *devname_r(dev_t dev, mode_t type, char *buf, int len);
```

Sketch:

```go
// ttyPath maps a controlling-terminal dev_t to "/dev/ttysNNN", or "" when the
// process has no controlling terminal or the device cannot be named.
func ttyPath(tdev int32) string {
	const NODEV = -1
	if tdev == NODEV {
		return ""
	}
	var buf [128]C.char
	p := C.devname_r(C.dev_t(tdev), C.S_IFCHR, &buf[0], C.int(len(buf)))
	if p == nil {
		return ""
	}
	name := C.GoString(p)
	if name == "" || name[0] == '#' || strings.ContainsRune(name, '/') {
		return "" // synthesized "#major/minor" fallback — not a real tty
	}
	return "/dev/" + name
}
```

The resulting literal is `/dev/ttysNNN` (or `/dev/ttypN` on legacy ptys). The contract only checks non-emptiness and equality-as-join-key, so the exact spelling is irrelevant to the test — but it must be *stable* across `Enumerate`/`Read` so the join with the same process via its tty is consistent, which `devname_r` guarantees for a given `dev_t`.

> Alternative without `devname`: a hand-rolled `major(dev)`/`minor(dev)` → `fmt.Sprintf("/dev/ttys%03d", minor)` mapping. This is brittle (assumes the `ttys` cloning-device major and a `minor → NNN` identity that is **not** guaranteed) and is **not recommended**; since we are already paying for cgo (Exe/CWD), `devname_r` is the correct, supported resolver.

## 3. cgo vs cgo-free verdict

**Verdict: cgo is required.** The Observe tier headline includes **CWD**, and CWD has *no* cgo-free retrieval path on macOS (`PROC_PIDVNODEPATHINFO` is libproc-only). Exe is likewise libproc-only in any sane form. A cgo-free build therefore cannot satisfy the contract.

What a cgo-free subset *could* deliver, using only the `KERN_PROC_ALL` sysctl scan:

| Field | cgo-free? | Source |
|---|---|---|
| PID | yes | `Proc.P_pid` |
| PPID | yes | `Eproc.Ppid` |
| Comm | yes | `Proc.P_comm` (truncated, matches Linux) |
| TTY | yes | `Eproc.Tdev` + `devname_r`… *but* `devname_r` is cgo. A pure-Go `major/minor` formatter is possible but brittle (see §2). |
| **Exe** | **no** | needs `proc_pidpath` |
| **CWD** | **no** | needs `PROC_PIDVNODEPATHINFO` |

A cgo-free backend would lose **Exe and CWD** — and would fail `RunSourceContract`'s "interactive child has non-empty CWD" assertion, and would weaken `discovery.IsClaude` (which relies on an `/claude/` exe substring as one of its two signals). That is **not acceptable** for the Observe tier. Recommendation: **commit to cgo** for the darwin backend. (Implication for CI: a darwin builder with the macOS SDK and `CGO_ENABLED=1` — tracked in the separate CI doc.)

## 4. Concrete file plan

`source_darwin.go` becomes a cgo file. Keep the `//go:build darwin` tag; cgo is implied by `import "C"` but the tag is still required for the build constraint.

```go
//go:build darwin

package osproc

/*
#include <stdlib.h>
#include <sys/types.h>
#include <sys/stat.h>      // S_IFCHR
#include <sys/proc_info.h> // struct proc_bsdinfo, struct proc_vnodepathinfo, PROC_PID* flavors
#include <libproc.h>       // proc_pidpath, proc_pidinfo, PROC_PIDPATHINFO_MAXSIZE
*/
import "C"

import (
	"bytes"
	"context"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

func newSource() Source { return newDarwinSource() }

// newDarwinSource mirrors newLinuxSource so tests can reach introspection
// (Watched()) that the Source interface does not expose. Watch/Stop are stubs
// until Phase 4.2 (kqueue).
func newDarwinSource() *darwinSource {
	return &darwinSource{watched: make(map[int]context.CancelFunc)}
}

type darwinSource struct {
	mu      sync.Mutex
	watched map[int]context.CancelFunc
}
```

`Enumerate` — sysctl scan for the list + cheap fields, then libproc per pid for paths:

```go
func (s *darwinSource) Enumerate() ([]Info, error) {
	procs, err := scanProcs()
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(procs))
	for i := range procs {
		kp := &procs[i]
		pid := int(kp.Proc.P_pid)
		if pid <= 0 {
			continue
		}
		out = append(out, Info{
			PID:  pid,
			PPID: int(kp.Eproc.Ppid),
			Comm: goStr(kp.Proc.P_comm[:]),
			Exe:  procPath(pid),       // "" on failure — benign
			CWD:  procCWD(pid),        // "" on failure — benign
			TTY:  ttyPath(kp.Eproc.Tdev),
		})
	}
	return out, nil
}
```

> Mid-scan races are benign: a process that dies between the sysctl snapshot and the per-pid libproc reads simply yields `Exe=""`/`CWD=""`. We do **not** drop it (unlike Linux, where `proc.Read` failing drops the row) — but dropping is also acceptable. To match Linux's "skip disappeared rows" behavior more closely, detect `proc_pidpath` returning `ESRCH` and `continue`. Either is contract-conformant; **prefer skipping on `ESRCH`** for parity:

```go
func procPath(pid int) string {
	buf := make([]byte, C.PROC_PIDPATHINFO_MAXSIZE)
	n := C.proc_pidpath(C.int(pid), unsafe.Pointer(&buf[0]), C.uint32_t(len(buf)))
	if n <= 0 {
		return "" // ESRCH/EPERM/etc. — unobtainable, not an error
	}
	return string(buf[:n])
}

func procCWD(pid int) string {
	var vpi C.struct_proc_vnodepathinfo
	n := C.proc_pidinfo(C.int(pid), C.PROC_PIDVNODEPATHINFO, 0,
		unsafe.Pointer(&vpi), C.int(unsafe.Sizeof(vpi)))
	if int(n) != int(unsafe.Sizeof(vpi)) {
		return ""
	}
	return C.GoString(&vpi.pvi_cdir.vip_path[0])
}
```

`Read(pid)` — single-pid path, with `ErrGone` mapping. We re-fetch the one record via `"kern.proc.pid"`:

```go
func (s *darwinSource) Read(pid int) (Info, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return Info{PID: pid}, ErrGone
		}
		return Info{PID: pid}, err
	}
	// A live-but-empty result (n==0 from sysctl) surfaces as ESRCH/EIO from the
	// helper; treat the gone case explicitly:
	if kp == nil || kp.Proc.P_pid == 0 {
		return Info{PID: pid}, ErrGone
	}
	return Info{
		PID:  pid,
		PPID: int(kp.Eproc.Ppid),
		Comm: goStr(kp.Proc.P_comm[:]),
		Exe:  procPath(pid),
		CWD:  procCWD(pid),
		TTY:  ttyPath(kp.Eproc.Tdev),
	}, nil
}
```

> `SysctlKinfoProc` (singular) returns `EIO` when the returned size is not exactly one record (e.g. process gone ⇒ zero records). Map both `ESRCH` and the zero-record/`EIO` case to `ErrGone` so `Read(deadpid)` returns the sentinel. Verify the exact error on-device; if `EIO` is observed for a vanished pid, add it to the `ErrGone` branch.

`Watch`/`Stop` stay stubbed in this phase:

```go
func (s *darwinSource) Watch(context.Context, int, func()) error { return ErrUnsupported }
func (s *darwinSource) Stop(int)                                 {}
```

**Error-mapping rules (mirror Linux):**
- Enumerate list-fetch error ⇒ propagate.
- Per-pid Exe/CWD/TTY unobtainable (`EPERM`, `ESRCH` on the *secondary* probe, malformed device) ⇒ corresponding field `""`, **no error**.
- `Read` of a vanished pid (`ESRCH` / zero-record) ⇒ `Info{PID: pid}, ErrGone`.
- Comm/PPID come from the same record as the pid, so they are always present for an enumerated row.

## 5. Definition of Done

- [ ] `source_darwin.go` is a cgo `//go:build darwin` file implementing real `Enumerate`/`Read`; `darwinSource` carries the `watched` map mirroring `linuxSource` (Watch/Stop still stubbed → 4.2).
- [ ] `Enumerate()` on a Mac returns rows for claude processes with **non-empty CWD and non-empty TTY**; `discovery.IsClaude` matches them (comm `claude`, exe contains `/claude/`).
- [ ] An interactive child shows a `/dev/ttysNNN` TTY; a bare (no controlling terminal) child shows `TTY == ""`.
- [ ] `Read(deadpid)` returns `osproc.ErrGone`; `Read` of a process whose exe is unobtainable returns `Exe == ""` with **no error**.
- [ ] `internal/conformance.RunSourceContract` **passes** against the darwin `Source` on a macOS host (arm64; spot-check amd64).
- [ ] Comm is truncated to ≤16 chars (kernel `MAXCOMLEN`), consistent with Linux.
- [ ] `go build` / `go test ./...` succeed on macOS with `CGO_ENABLED=1`; the Linux dev box is not expected to build the cgo file.

## 6. Risks & unknowns

- **SIP / entitlements for other users' processes.** `proc_pidpath` / `proc_pidinfo(PROC_PIDVNODEPATHINFO)` for processes owned by *other* uids (or hardened-runtime / SIP-protected procs) return `EPERM`. We only need **our own uid's** claude processes, which we can always inspect, so this is a non-issue for the Observe tier — the failure mode for foreign procs is simply `Exe=""`/`CWD=""`, which is handled gracefully. Document that we do not attempt privileged inspection.
- **Truncated comm.** `P_comm`/`pbi_comm` is capped at `MAXCOMLEN` (16). Same as Linux; `IsClaude`'s exact `comm == "claude"` (6 chars) is safe. Do **not** switch to `pbi_name` to "fix" truncation — it would diverge from Linux and break the shared contract.
- **arm64 vs amd64 struct sizes.** `KinfoProc`/`Eproc`/`ExternProc` layouts differ by arch; we rely on `x/sys/unix`'s generated `ztypes_darwin_{arm64,amd64}.go` (`SizeofKinfoProc = 0x288` on arm64) rather than hand-declaring them — so no manual offset risk there. For the cgo structs (`proc_bsdinfo`, `proc_vnodepathinfo`) we use `C.struct_*` + `unsafe.Sizeof`, so the compiler computes correct per-arch sizes. Verify on both arches; arm64 is primary.
- **`devname_r` edge cases.** Returns synthesized `#major/minor` names when the device isn't in `/dev`; our guard maps those to `""`. Confirm on-device that a live `ttysNNN` resolves cleanly. Also confirm `NODEV` surfaces as `Tdev == -1` (it should; verify the sign since `Tdev` is `int32`).
- **`Read` gone-pid error code.** `SysctlKinfoProc` may surface a vanished pid as `EIO` (size mismatch) rather than `ESRCH`. Confirm on-device and ensure both map to `ErrGone`.
- **cgo cross-compile.** A cgo darwin build cannot be produced from the Linux box (no macOS SDK / linker). Requires a macOS builder with `CGO_ENABLED=1`; CI plumbing is a **separate doc**. Local dev verification of the cgo file is impossible here — flagged in the Status banner.
- **`KERN_PROC_ALL` returns all processes.** Larger scan than strictly necessary (we only care about claude procs). Acceptable for Observe-tier cardinality; if it becomes a hotspot, switch to `"kern.proc.uid"` + `unix.Getuid()` to narrow kernel-side.

## References

- [Apple darwin-xnu `bsd/sys/proc_info.h`](https://raw.githubusercontent.com/apple/darwin-xnu/main/bsd/sys/proc_info.h) — `proc_bsdinfo`, `proc_vnodepathinfo`, `vnode_info_path`, flavor constants (`PROC_PIDTBSDINFO=3`, `PROC_PIDVNODEPATHINFO=9`, `PROC_PIDPATHINFO_MAXSIZE=4*MAXPATHLEN`).
- [sudo `ttyname.c`](https://opensource.apple.com/source/sudo/sudo-83/sudo/src/ttyname.c.auto.html) — canonical `e_tdev` + `devname(dev, S_IFCHR)` → `/dev/...` pattern on macOS.
- [Emacs bug#48548 — process attributes on macOS](https://lists.gnu.org/archive/html/bug-gnu-emacs/2021-05/msg01652.html) — corroborates `kp_eproc.e_tdev` + `devname` for controlling tty.
- [Counting open FDs on macOS (proc_pidinfo usage)](https://zameermanji.com/blog/2021/8/1/counting-open-file-descriptors-on-macos/) — `proc_pidinfo` calling convention reference.
