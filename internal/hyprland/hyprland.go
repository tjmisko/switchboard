// Package hyprland talks to Hyprland's two IPC sockets: the request socket
// (commands like `clients`, `dispatch`) and socket2 (an async event stream).
//
// Event payloads are documented at https://wiki.hypr.land/IPC/. We only care
// about a small subset (openwindow, closewindow, movewindowv2, activewindowv2,
// windowtitlev2) — anything else is dropped at parse time.
package hyprland

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Client is one entry from `hyprctl clients -j`.
type Client struct {
	Address   string    `json:"address"` // "0x..."
	PID       int       `json:"pid"`
	Class     string    `json:"class"`
	Title     string    `json:"title"`
	Workspace Workspace `json:"workspace"`
	Monitor   int       `json:"monitor"`
	Mapped    bool      `json:"mapped"`
}

type Workspace struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// Clients runs `hyprctl clients -j` over the request socket and parses the
// JSON. We bypass the hyprctl binary to keep latency low and avoid spawning.
func Clients(ctx context.Context) ([]Client, error) {
	resp, err := request(ctx, "j/clients")
	if err != nil {
		return nil, err
	}
	var out []Client
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, fmt.Errorf("parse clients: %w", err)
	}
	return out, nil
}

// ActiveWindowAddress returns the address of the currently focused window, or
// "" if nothing is focused. Used at startup to set the initial Focused flag
// without waiting for the user to switch windows.
func ActiveWindowAddress(ctx context.Context) (string, error) {
	resp, err := request(ctx, "j/activewindow")
	if err != nil {
		return "", err
	}
	var aw struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(resp, &aw); err != nil {
		return "", fmt.Errorf("parse activewindow: %w", err)
	}
	return aw.Address, nil
}

// Dispatch sends a `dispatch` command (e.g. "focuswindow address:0x...") and
// returns the raw response bytes. Hyprland responds with "ok" on success.
func Dispatch(ctx context.Context, cmd string) error {
	resp, err := request(ctx, "/dispatch "+cmd)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(string(resp), "ok") {
		return fmt.Errorf("dispatch %q: %s", cmd, strings.TrimSpace(string(resp)))
	}
	return nil
}

// FocusWindow focuses the window at addr without warping the cursor.
//
// Hyprland's `focuswindow` dispatcher warps the cursor to the center of the
// target window when that window lives on another workspace (this is the
// default `cursor:no_warps = false` behavior). For click-driven focus — a chip
// click — that mouse jump is jarring. We want it to behave like clicking a
// waybar workspace button, which switches workspace without moving the cursor.
//
// So we temporarily enable `cursor:no_warps` for the duration of this one
// dispatch, batched into a single IPC request ([[BATCH]] runs the commands
// sequentially in one round-trip) so the toggle is atomic and never leaks to
// other, unrelated focus events. We restore the option to its prior value
// rather than hardcoding false, so a user who globally disabled warps stays
// disabled.
func FocusWindow(ctx context.Context, addr string) error {
	prev, err := noWarps(ctx)
	if err != nil {
		// If we can't read the option, fall back to a plain focus. The cursor
		// may warp, but focus still works — better than failing the jump.
		return Dispatch(ctx, "focuswindow address:"+addr)
	}
	cmd := fmt.Sprintf(
		"[[BATCH]]keyword cursor:no_warps true ; dispatch focuswindow address:%s ; keyword cursor:no_warps %t",
		addr, prev,
	)
	resp, err := request(ctx, cmd)
	if err != nil {
		return err
	}
	// A batch echoes "ok" once per sub-command. Any non-ok segment is a failure.
	for _, part := range strings.Split(strings.TrimSpace(string(resp)), "\n") {
		if part = strings.TrimSpace(part); part != "" && !strings.HasPrefix(part, "ok") {
			return fmt.Errorf("focus window %s: %s", addr, part)
		}
	}
	return nil
}

// noWarps reports the current effective value of the cursor:no_warps option.
func noWarps(ctx context.Context) (bool, error) {
	resp, err := request(ctx, "j/getoption cursor:no_warps")
	if err != nil {
		return false, err
	}
	var o struct {
		Int int `json:"int"`
	}
	if err := json.Unmarshal(resp, &o); err != nil {
		return false, fmt.Errorf("parse getoption cursor:no_warps: %w", err)
	}
	return o.Int != 0, nil
}

// Event is one line from socket2. Name is the event identifier
// (openwindow/closewindow/...); Data is the raw comma-separated payload.
type Event struct {
	Name string
	Data string
}

// Subscribe opens socket2 and streams events into the returned channel.
// Closes the channel when the context is cancelled or the socket dies.
func Subscribe(ctx context.Context) (<-chan Event, error) {
	sock, err := socket2Path()
	if err != nil {
		return nil, err
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, err
	}
	ch := make(chan Event, 32)
	go func() {
		defer close(ch)
		defer conn.Close()
		go func() {
			<-ctx.Done()
			conn.Close()
		}()
		parseEvents(ctx, conn, ch)
	}()
	return ch, nil
}

// parseEvents reads socket2 lines from r, splits each on the first ">>" into an
// Event, and forwards them to ch until r reaches EOF/error or ctx is cancelled
// (observed on the send). Lines without a ">>" delimiter are dropped. The token
// buffer is capped at 1 MiB. It does not close ch — the caller owns that, and
// is responsible for closing r (e.g. on ctx-cancel) to unblock a pending Read.
func parseEvents(ctx context.Context, r io.Reader, ch chan<- Event) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		name, data, ok := strings.Cut(line, ">>")
		if !ok {
			continue
		}
		select {
		case ch <- Event{Name: name, Data: data}:
		case <-ctx.Done():
			return
		}
	}
}

// request sends one command to the Hyprland request socket and reads the
// response until EOF. The protocol is text: the client writes the command and
// the server writes the response, then closes the connection.
func request(ctx context.Context, cmd string) ([]byte, error) {
	sock, err := requestSocketPath()
	if err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	} else {
		conn.SetDeadline(time.Now().Add(2 * time.Second))
	}
	if _, err := conn.Write([]byte(cmd)); err != nil {
		return nil, err
	}
	return io.ReadAll(conn)
}

func requestSocketPath() (string, error) { return socketPath(".socket.sock") }
func socket2Path() (string, error)       { return socketPath(".socket2.sock") }

func socketPath(name string) (string, error) {
	xdg := os.Getenv("XDG_RUNTIME_DIR")
	if xdg == "" {
		return "", errors.New("XDG_RUNTIME_DIR not set")
	}
	sig, err := InstanceSignature()
	if err != nil {
		return "", err
	}
	return filepath.Join(xdg, "hypr", sig, name), nil
}

// InstanceSignature resolves the Hyprland instance signature used to locate the
// IPC sockets under $XDG_RUNTIME_DIR/hypr/<sig>/. It prefers
// $HYPRLAND_INSTANCE_SIGNATURE — the variable Hyprland exports into the session
// it launches — and falls back to discovering the live instance on disk when the
// variable is absent.
//
// This is a defense-in-depth complement to the systemd unit's ExecStart, which
// performs the same lock-file discovery before exec'ing the daemon. The Go
// fallback also covers daemons launched OUTSIDE that wrapper — a developer
// `go run`, a login shell, or any supervisor that did not inherit the
// compositor's imported environment after a user-manager (e.g. OOM) restart.
// Without it the daemon would silently drop to a WM-less degraded mode until the
// next full login.
func InstanceSignature() (string, error) {
	if sig := os.Getenv("HYPRLAND_INSTANCE_SIGNATURE"); sig != "" {
		return sig, nil
	}
	xdg := os.Getenv("XDG_RUNTIME_DIR")
	if xdg == "" {
		return "", errors.New("HYPRLAND_INSTANCE_SIGNATURE and XDG_RUNTIME_DIR both unset; not running under Hyprland?")
	}
	return discoverInstance(filepath.Join(xdg, "hypr"))
}

// discoverInstance returns the name of the live Hyprland instance directory
// under hyprDir, applying the same liveness test the systemd unit's ExecStart
// uses: each $XDG_RUNTIME_DIR/hypr/<sig>/hyprland.lock holds the running
// compositor's PID on line 1, and the live session is the one whose PID is still
// present (checked with unix.Kill(pid, 0), the portable equivalent of the unit's
// `[ -e /proc/<pid> ]`). Stale dirs from prior sessions linger until reboot, so a
// bare directory listing is ambiguous; the PID check disambiguates.
//
// Tie-breaking, in order:
//  1. a dir whose lock names a live PID wins (the current session);
//  2. if more than one is live (should not happen), the most recently modified
//     .socket.sock wins;
//  3. if no lock names a live PID (e.g. an older Hyprland that writes no lock),
//     fall back to the newest .socket.sock as a last resort.
func discoverInstance(hyprDir string) (string, error) {
	entries, err := os.ReadDir(hyprDir)
	if err != nil {
		return "", fmt.Errorf("discover Hyprland instance: %w", err)
	}
	var (
		liveBest    string
		liveBestMod time.Time
		liveFound   bool
		sockBest    string
		sockBestMod time.Time
		sockFound   bool
	)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(hyprDir, e.Name())
		sockMod, hasSock := socketModTime(filepath.Join(dir, ".socket.sock"))
		if hasSock && (!sockFound || sockMod.After(sockBestMod)) {
			sockBest, sockBestMod, sockFound = e.Name(), sockMod, true
		}
		if pid, ok := lockPID(filepath.Join(dir, "hyprland.lock")); ok && pidAlive(pid) {
			if !liveFound || sockMod.After(liveBestMod) {
				liveBest, liveBestMod, liveFound = e.Name(), sockMod, true
			}
		}
	}
	switch {
	case liveFound:
		return liveBest, nil
	case sockFound:
		return sockBest, nil
	default:
		return "", fmt.Errorf("no live Hyprland instance under %s", hyprDir)
	}
}

// lockPID parses the compositor PID from the first line of a hyprland.lock file,
// matching the unit's `read -r p _ < hyprland.lock`: the PID is the first
// whitespace-delimited token of line 1.
func lockPID(lockPath string) (int, bool) {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return 0, false
	}
	firstLine, _, _ := strings.Cut(string(data), "\n")
	fields := strings.Fields(firstLine)
	if len(fields) == 0 {
		return 0, false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// pidAlive reports whether a process with pid exists, the portable analogue of
// the unit's `[ -e /proc/<pid> ]`. Signal 0 performs no delivery, only the
// existence/permission check; EPERM means the process exists but is owned by
// another user (still alive).
func pidAlive(pid int) bool {
	err := unix.Kill(pid, 0)
	return err == nil || err == unix.EPERM
}

// socketModTime returns the modification time of the request socket at path and
// whether it exists. It is used only to break ties between live instances and as
// the last-resort selector when no lock names a live PID.
func socketModTime(path string) (time.Time, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}
