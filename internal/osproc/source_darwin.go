//go:build darwin

package osproc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func newSource() Source { return newDarwinSource() }

// newDarwinSource builds the concrete macOS source. Tests use it directly to
// reach Watched(), which is introspection the Source interface does not expose.
func newDarwinSource() *darwinSource {
	return &darwinSource{watched: make(map[int]context.CancelFunc)}
}

// darwinSource reads process metadata from sysctl + libproc and watches deaths
// with kqueue EVFILT_PROC/NOTE_EXIT — the structural mirror of the Linux
// backend's pidfd watcher, with the kernel again signalling death rather than
// the watcher inferring it from polled state.
//
// Everything here is pure Go. That is a deliberate constraint, not a
// coincidence: the moment this file needs cgo, `GOOS=darwin go build ./...`
// stops working from a Linux host and the darwin CI signal has to be rebuilt
// around a macOS runner (docs/phase4/03-ci-and-cgo.md).
type darwinSource struct {
	mu      sync.Mutex
	watched map[int]context.CancelFunc
}

// proc_info(2) is reached through libSystem's syscall() shim. Apple deprecated
// that shim in 10.12, so every call site degrades rather than failing hard: a
// process whose cwd or exe cannot be read still yields a usable Info (discovery
// tolerates both being empty), which keeps the Observe tier alive even if a
// future macOS removes the entry point. Verified working on 14.3.1/arm64.
const (
	sysProcInfo          = 336
	procInfoCallPidInfo  = 2
	procPidVnodePathInfo = 9
	procPidPathInfo      = 11
	procPidPathMaxSize   = 4096
)

// struct proc_vnodepathinfo is two struct vnode_info_path, each being a
// vnode_info (152 bytes) followed by path[MAXPATHLEN]. The cwd is the second
// half's path — offsets hand-computed and verified on arm64.
const (
	sizeofVnodeInfo     = 152
	sizeofVnodeInfoPath = sizeofVnodeInfo + 1024
)

func pidInfo(pid, flavor int, buf []byte) (int, error) {
	r1, _, errno := syscall.Syscall6(sysProcInfo,
		uintptr(procInfoCallPidInfo), uintptr(pid), uintptr(flavor),
		0, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if errno != 0 {
		return 0, errno
	}
	return int(r1), nil
}

// readCWD returns pid's working directory. It succeeds unprivileged for
// processes owned by the calling user and returns EPERM for others, so a failure
// is a normal outcome and never fatal.
func readCWD(pid int) string {
	buf := make([]byte, 2*sizeofVnodeInfoPath)
	n, err := pidInfo(pid, procPidVnodePathInfo, buf)
	if err != nil || n < sizeofVnodeInfoPath {
		return ""
	}
	return cstring(buf[sizeofVnodeInfo:sizeofVnodeInfoPath])
}

func readExe(pid int) string {
	buf := make([]byte, procPidPathMaxSize)
	if _, err := pidInfo(pid, procPidPathInfo, buf); err != nil {
		return ""
	}
	return cstring(buf)
}

// readArgs returns pid's argv. On macOS argv[0] is the load-bearing identity
// signal for discovery, because p_comm holds the RESOLVED binary's basename and
// so reads as a version string for a versioned-symlink install.
func readArgs(pid int) []string {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil
	}
	return parseArgs2(raw)
}

// parseArgs2 decodes the KERN_PROCARGS2 blob: a uint32 argc, the NUL-terminated
// executable path, NUL padding to alignment, then argc NUL-terminated argv
// strings (followed by the environment, which we deliberately do not read —
// it carries secrets and nothing above this seam needs it).
func parseArgs2(raw []byte) []string {
	if len(raw) < 4 {
		return nil
	}
	argc := int(*(*uint32)(unsafe.Pointer(&raw[0])))
	if argc <= 0 {
		return nil
	}
	rest := raw[4:]
	end := bytes.IndexByte(rest, 0) // skip the exec path
	if end < 0 {
		return nil
	}
	i := end
	for i < len(rest) && rest[i] == 0 {
		i++
	}
	argv := make([]string, 0, argc)
	start := i
	for i < len(rest) && len(argv) < argc {
		if rest[i] == 0 {
			argv = append(argv, string(rest[start:i]))
			start = i + 1
		}
		i++
	}
	return argv
}

// ptyMajor is the device major for macOS pty slaves (/dev/ttysNNN).
const ptyMajor = 16

// ttyName renders kinfo_proc's e_tdev as the same string tty(1), tmux's
// #{pane_tty} and wezterm report, because that string is the join key the
// terminal seam matches on. A process with no controlling terminal has tdev
// NODEV (-1) and yields "".
//
// Non-pty terminals return "" rather than a guess: naming them needs devname(3)
// (cgo), and they are never panes, so the join would fail anyway. NOTE that
// libproc's devname route yields a BARE "ttys001" with no /dev/ prefix — that
// string silently fails every join, which is why this builds the path itself.
func ttyName(tdev int32) string {
	if tdev == -1 {
		return ""
	}
	dev := uint32(tdev)
	if major := (dev >> 24) & 0xff; major != ptyMajor {
		return ""
	}
	return fmt.Sprintf("/dev/ttys%03d", dev&0xffffff)
}

func cstring(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

// infoFromKinfo projects the cheap sysctl-resident fields. The expensive
// per-pid reads (argv, cwd, exe) are layered on by hydrate.
func infoFromKinfo(kp *unix.KinfoProc) Info {
	return Info{
		PID:  int(kp.Proc.P_pid),
		PPID: int(kp.Eproc.Ppid),
		Comm: cstring(kp.Proc.P_comm[:]),
		TTY:  ttyName(kp.Eproc.Tdev),
	}
}

func hydrate(info *Info) {
	info.Args = readArgs(info.PID)
	info.CWD = readCWD(info.PID)
	info.Exe = readExe(info.PID)
}

// Enumerate returns every visible process. One kern.proc.all sysctl supplies
// pid, ppid, comm and tdev for all of them — cheaper and richer than the Linux
// equivalent, which returns bare pids and charges a read per field.
func (s *darwinSource) Enumerate() ([]Info, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(procs))
	for i := range procs {
		if procs[i].Proc.P_pid == 0 {
			continue
		}
		info := infoFromKinfo(&procs[i])
		hydrate(&info)
		out = append(out, info)
	}
	return out, nil
}

// AllPIDs is the pidLister fast path discovery prefers: it skips the per-pid
// hydration entirely, so the 1 Hz scan pays one sysctl and nothing else.
func (s *darwinSource) AllPIDs() ([]int, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	pids := make([]int, 0, len(procs))
	for i := range procs {
		if pid := int(procs[i].Proc.P_pid); pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func (s *darwinSource) Read(pid int) (Info, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		// A pid that died between enumeration and read is the common race.
		// Darwin reports it as EIO (measured on 14.3.1: the sysctl returns a
		// zero-length record and x/sys surfaces that as EIO, NOT the ESRCH the
		// Phase-4 plan predicted). ESRCH and EINVAL are accepted too rather
		// than betting the liveness of the whole session model on one errno.
		if errors.Is(err, unix.EIO) || errors.Is(err, unix.ESRCH) || errors.Is(err, unix.EINVAL) {
			return Info{PID: pid}, ErrGone
		}
		return Info{PID: pid}, err
	}
	// A successful sysctl with a zeroed record means the same thing.
	if kp.Proc.P_pid == 0 {
		return Info{PID: pid}, ErrGone
	}
	info := infoFromKinfo(kp)
	hydrate(&info)
	return info, nil
}

// Watch registers a kqueue EVFILT_PROC/NOTE_EXIT filter for pid. onDeath is
// called exactly once, from a background goroutine, when the kernel reports the
// exit — independent of how the process died. A duplicate Watch for the same pid
// returns nil without scheduling a second watcher.
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
	ev := unix.Kevent_t{
		Ident:  uint64(pid),
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD | unix.EV_ENABLE | unix.EV_ONESHOT,
		Fflags: unix.NOTE_EXIT,
	}
	if _, err := unix.Kevent(kq, []unix.Kevent_t{ev}, nil, nil); err != nil {
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
		// A bounded timeout keeps Stop responsive; the kevent itself is what
		// observes the death, so the timeout costs nothing but a ctx re-check.
		timeout := unix.NsecToTimespec(int64(time.Second))
		out := make([]unix.Kevent_t, 1)
		for {
			if ctx.Err() != nil {
				return
			}
			n, err := unix.Kevent(kq, nil, out, &timeout)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				return
			}
			if n == 0 {
				continue // timeout — re-check ctx and loop
			}
			if out[0].Fflags&unix.NOTE_EXIT != 0 {
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
