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
	"strings"
	"time"
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
	sig := os.Getenv("HYPRLAND_INSTANCE_SIGNATURE")
	if sig == "" {
		return "", errors.New("HYPRLAND_INSTANCE_SIGNATURE not set; not running under Hyprland?")
	}
	xdg := os.Getenv("XDG_RUNTIME_DIR")
	if xdg == "" {
		return "", errors.New("XDG_RUNTIME_DIR not set")
	}
	return filepath.Join(xdg, "hypr", sig, name), nil
}
