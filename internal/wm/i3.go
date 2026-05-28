package wm

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"time"
)

// I3 is the sway/i3 backend. Both speak the same IPC over a Unix socket
// ($SWAYSOCK or $I3SOCK) with a magic-string binary framing; the only
// difference this package cares about is the reported Name. The opaque window
// ref is the container id (con_id); focus is `[con_id=N] focus`.
//
// Caveat: i3's GET_TREE does not expose a pid (only sway does), so under pure
// i3 the terminal<->window join (which keys on the terminal's mux pid) cannot
// resolve and such sessions stay Observe-only. Sway populates pid, so Navigate
// works there. See docs/portability-plan.md.
type I3 struct {
	name   string
	socket string
}

// NewI3 returns the sway/i3 backend, resolving the socket from $SWAYSOCK (sway)
// or $I3SOCK (i3). With neither set it is unavailable (name defaults to "i3").
func NewI3() *I3 {
	if s := os.Getenv("SWAYSOCK"); s != "" {
		return &I3{name: "sway", socket: s}
	}
	if s := os.Getenv("I3SOCK"); s != "" {
		return &I3{name: "i3", socket: s}
	}
	return &I3{name: "i3"}
}

func (i *I3) Name() string { return i.name }

func (i *I3) Available() bool {
	if i.socket == "" {
		return false
	}
	_, err := os.Stat(i.socket)
	return err == nil
}

func (i *I3) Clients(ctx context.Context) ([]Window, error) {
	reply, err := i.roundtrip(ctx, i3GetTree, nil)
	if err != nil {
		return nil, err
	}
	return parseI3Tree(reply)
}

func (i *I3) ActiveWindow(ctx context.Context) (string, error) {
	reply, err := i.roundtrip(ctx, i3GetTree, nil)
	if err != nil {
		return "", err
	}
	return parseI3Active(reply)
}

func (i *I3) Focus(ctx context.Context, ref string) error {
	reply, err := i.roundtrip(ctx, i3RunCommand, []byte(fmt.Sprintf("[con_id=%s] focus", ref)))
	if err != nil {
		return err
	}
	var res []struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(reply, &res); err != nil {
		return fmt.Errorf("i3 focus: parse reply: %w", err)
	}
	for _, r := range res {
		if !r.Success {
			return fmt.Errorf("i3 focus %s: %s", ref, r.Error)
		}
	}
	return nil
}

// Subscribe opens a dedicated connection (i3 streams events on the connection
// that issued SUBSCRIBE), translates window/workspace events into neutral
// Events, and closes the channel on ctx-cancel or connection EOF.
func (i *I3) Subscribe(ctx context.Context) (<-chan Event, error) {
	if i.socket == "" {
		return nil, fmt.Errorf("i3: no socket ($SWAYSOCK/$I3SOCK unset)")
	}
	conn, err := net.Dial("unix", i.socket)
	if err != nil {
		return nil, err
	}
	if err := writeI3Message(conn, i3Subscribe, []byte(`["window","workspace"]`)); err != nil {
		conn.Close()
		return nil, err
	}
	if _, _, err := readI3Message(conn); err != nil { // the {"success":true} ack
		conn.Close()
		return nil, err
	}
	out := make(chan Event, 32)
	go func() {
		defer close(out)
		go func() {
			<-ctx.Done()
			conn.Close()
		}()
		for {
			rawType, payload, err := readI3Message(conn)
			if err != nil {
				return
			}
			if rawType&i3EventFlag == 0 {
				continue // a command reply, not an event
			}
			ev, ok := translateI3Event(rawType&^uint32(i3EventFlag), payload)
			if !ok {
				continue
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// --- conformance helpers (con_id has no prefix quirk → identity) ---

func (*I3) NormalizeEventAddress(raw string) string { return raw }

func (*I3) RawForm(address string) string { return address }

// i3RecognizedChanges maps the window/workspace `change` values the backend
// handles to their neutral kind ("workspace" is a synthetic name standing for
// any workspace event, which always re-reconciles).
var i3RecognizedChanges = map[string]EventKind{
	"close":     EventWindowClosed,
	"focus":     EventFocusChanged,
	"new":       EventLayoutChanged,
	"move":      EventLayoutChanged,
	"title":     EventLayoutChanged,
	"workspace": EventLayoutChanged,
}

func (*I3) CanonicalEvents() []string {
	return []string{"close", "focus", "new", "move", "title", "workspace"}
}

func (*I3) TranslateEvent(name string) (string, bool) {
	if _, ok := i3RecognizedChanges[name]; ok {
		return name, true
	}
	return "", false
}

// --- i3-IPC wire protocol ---

const (
	i3Magic     = "i3-ipc"
	i3HeaderLen = 14      // magic(6) + length(4) + type(4)
	i3EventFlag = 1 << 31 // high bit of the reply type marks an event
)

type i3MsgType uint32

const (
	i3RunCommand i3MsgType = 0
	i3Subscribe  i3MsgType = 2
	i3GetTree    i3MsgType = 4
)

// event type numbers (low bits, with i3EventFlag set on the wire)
const (
	i3EventWorkspace uint32 = 0
	i3EventWindow    uint32 = 3
)

// roundtrip dials the socket, sends one message, and reads exactly one reply.
func (i *I3) roundtrip(ctx context.Context, t i3MsgType, payload []byte) ([]byte, error) {
	if i.socket == "" {
		return nil, fmt.Errorf("i3: no socket ($SWAYSOCK/$I3SOCK unset)")
	}
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "unix", i.socket)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	} else {
		conn.SetDeadline(time.Now().Add(2 * time.Second))
	}
	if err := writeI3Message(conn, t, payload); err != nil {
		return nil, err
	}
	_, reply, err := readI3Message(conn)
	return reply, err
}

// writeI3Message frames and writes one message. Length and type use native byte
// order, as the i3 IPC spec requires.
func writeI3Message(w io.Writer, t i3MsgType, payload []byte) error {
	var hdr [i3HeaderLen]byte
	copy(hdr[0:6], i3Magic)
	binary.NativeEndian.PutUint32(hdr[6:10], uint32(len(payload)))
	binary.NativeEndian.PutUint32(hdr[10:14], uint32(t))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := w.Write(payload)
		return err
	}
	return nil
}

// readI3Message reads one framed message, returning its raw type (event flag
// still set if it is an event) and payload.
func readI3Message(r io.Reader) (uint32, []byte, error) {
	var hdr [i3HeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	if string(hdr[0:6]) != i3Magic {
		return 0, nil, fmt.Errorf("i3: bad magic %q", hdr[0:6])
	}
	n := binary.NativeEndian.Uint32(hdr[6:10])
	t := binary.NativeEndian.Uint32(hdr[10:14])
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return t, payload, nil
}

// --- GET_TREE parsing ---

type i3Node struct {
	ID            int64    `json:"id"`
	Type          string   `json:"type"`
	Name          string   `json:"name"`
	Focused       bool     `json:"focused"`
	Window        *int64   `json:"window"` // X11 window id; null on Wayland
	AppID         string   `json:"app_id"` // sway Wayland clients
	PID           int      `json:"pid"`    // sway only; i3 omits it
	Num           int      `json:"num"`    // workspace number (workspace nodes)
	Nodes         []i3Node `json:"nodes"`
	FloatingNodes []i3Node `json:"floating_nodes"`
	WindowProps   struct {
		Title string `json:"title"`
	} `json:"window_properties"`
}

// isWindow reports whether the node is an actual client window (X11 window id
// present, or a sway Wayland app_id), as opposed to a workspace/split container.
func (n *i3Node) isWindow() bool { return n.Window != nil || n.AppID != "" }

func (n *i3Node) title() string {
	if n.Name != "" {
		return n.Name
	}
	return n.WindowProps.Title
}

func parseI3Tree(data []byte) ([]Window, error) {
	var root i3Node
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("i3 tree: %w", err)
	}
	var out []Window
	var walk func(n *i3Node, wsName string, wsNum int)
	walk = func(n *i3Node, wsName string, wsNum int) {
		if n.Type == "workspace" {
			wsName, wsNum = n.Name, n.Num
		}
		if n.isWindow() {
			out = append(out, Window{
				Address:     strconv.FormatInt(n.ID, 10),
				PID:         n.PID,
				Title:       n.title(),
				Workspace:   wsName,
				WorkspaceID: wsNum,
			})
		}
		for k := range n.Nodes {
			walk(&n.Nodes[k], wsName, wsNum)
		}
		for k := range n.FloatingNodes {
			walk(&n.FloatingNodes[k], wsName, wsNum)
		}
	}
	walk(&root, "", 0)
	return out, nil
}

func parseI3Active(data []byte) (string, error) {
	var root i3Node
	if err := json.Unmarshal(data, &root); err != nil {
		return "", fmt.Errorf("i3 tree: %w", err)
	}
	var found string
	var walk func(n *i3Node)
	walk = func(n *i3Node) {
		if found != "" {
			return
		}
		if n.Focused && n.isWindow() {
			found = strconv.FormatInt(n.ID, 10)
			return
		}
		for k := range n.Nodes {
			walk(&n.Nodes[k])
		}
		for k := range n.FloatingNodes {
			walk(&n.FloatingNodes[k])
		}
	}
	walk(&root)
	return found, nil
}

// translateI3Event maps a window/workspace event payload to a neutral Event.
func translateI3Event(evType uint32, payload []byte) (Event, bool) {
	switch evType {
	case i3EventWindow:
		var e struct {
			Change    string `json:"change"`
			Container struct {
				ID int64 `json:"id"`
			} `json:"container"`
		}
		if json.Unmarshal(payload, &e) != nil {
			return Event{}, false
		}
		addr := strconv.FormatInt(e.Container.ID, 10)
		switch e.Change {
		case "close":
			return Event{Kind: EventWindowClosed, Address: addr}, true
		case "focus":
			return Event{Kind: EventFocusChanged, Address: addr}, true
		case "new", "move", "title":
			return Event{Kind: EventLayoutChanged}, true
		}
		return Event{}, false
	case i3EventWorkspace:
		return Event{Kind: EventLayoutChanged}, true
	}
	return Event{}, false
}
