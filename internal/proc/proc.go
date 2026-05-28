// Package proc reads process metadata from /proc.
package proc

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Info struct {
	PID  int
	PPID int
	Comm string
	Exe  string
	CWD  string
	TTY  string // e.g. "/dev/pts/2" or "" if not a tty-attached process
}

// Read collects /proc/<pid>/{comm,exe,cwd,status,fd/0..2}. Returns ErrGone if
// the process disappeared mid-read (the most common race).
func Read(pid int) (Info, error) {
	out := Info{PID: pid}

	comm, err := readSmallFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return out, wrapGone(err)
	}
	out.Comm = strings.TrimRight(comm, "\n")

	if exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		out.Exe = exe
	}
	if cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid)); err == nil {
		out.CWD = cwd
	}

	status, err := readSmallFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return out, wrapGone(err)
	}
	out.PPID = parsePPID(status)

	out.TTY = readTTY(pid)
	return out, nil
}

// readTTY tries /proc/<pid>/fd/{0,1,2} for a /dev/pts/N link. Interactive TUIs
// like claude reliably have at least one of these attached to the controlling
// terminal. Returns "" if none of them point at a pts.
func readTTY(pid int) string {
	for _, fd := range []int{0, 1, 2} {
		link, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%d", pid, fd))
		if err != nil {
			continue
		}
		if strings.HasPrefix(link, "/dev/pts/") {
			return link
		}
	}
	return ""
}

// AllPIDs lists every numeric entry under /proc. Cheap (one getdents).
func AllPIDs() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	pids := make([]int, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// ErrGone means the process disappeared between when we listed it and when we
// tried to read it. Callers should treat this as benign.
var ErrGone = errors.New("process gone")

func wrapGone(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return ErrGone
	}
	return err
}

func readSmallFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func parsePPID(status string) int {
	for line := range strings.SplitSeq(status, "\n") {
		if rest, ok := strings.CutPrefix(line, "PPid:"); ok {
			ppid, _ := strconv.Atoi(strings.TrimSpace(rest))
			return ppid
		}
	}
	return 0
}
