package wm

import (
	"context"
	"os"
	"strconv"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// X11 is the EWMH backend for any standards-compliant X11 window manager. It
// reads the window list and focus from root-window properties (_NET_CLIENT_LIST,
// _NET_ACTIVE_WINDOW), pids from _NET_WM_PID, and titles from _NET_WM_NAME
// (falling back to WM_NAME); it focuses by sending a _NET_ACTIVE_WINDOW
// ClientMessage and watches focus/layout changes via root PropertyNotify. The
// opaque window ref is the X11 window id (decimal). Pure-Go via jezek/xgb, so
// `go install` and cross-compilation stay clean.
//
// Detection treats X11 as the fallback after the native Wayland/i3 IPCs; under
// a Wayland compositor DISPLAY may still be set (XWayland), so the precedence
// table in internal/detect prefers the compositor.
type X11 struct{}

// NewX11 returns the X11/EWMH backend. It does not connect — connections are
// opened lazily per operation so Available() stays cheap and side-effect-free.
func NewX11() *X11 { return &X11{} }

func (*X11) Name() string { return "x11" }

// Available reports whether an X display is configured, without connecting.
func (*X11) Available() bool { return os.Getenv("DISPLAY") != "" }

func (*X11) Clients(ctx context.Context) ([]Window, error) {
	c, err := xgb.NewConn()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	root := rootWindow(c)
	at, err := internAtoms(c)
	if err != nil {
		return nil, err
	}

	reply, err := xproto.GetProperty(c, false, root, at.clientList, xproto.AtomWindow, 0, 1<<20).Reply()
	if err != nil {
		return nil, err
	}
	var wins []xproto.Window
	if reply != nil {
		wins = parseWindowIDs(reply.Value)
	}

	out := make([]Window, 0, len(wins))
	for _, w := range wins {
		pid, _ := getCardinal(c, w, at.wmPID)
		name := getString(c, w, at.netWMName)
		if name == "" {
			name = getString(c, w, xproto.AtomWmName) // legacy WM_NAME fallback
		}
		ws, wsID := desktopOf(c, w, at.wmDesktop)
		out = append(out, Window{
			Address:     formatX11Window(w),
			PID:         int(pid),
			Title:       name,
			Workspace:   ws,
			WorkspaceID: wsID,
		})
	}
	return out, nil
}

func (*X11) ActiveWindow(ctx context.Context) (string, error) {
	c, err := xgb.NewConn()
	if err != nil {
		return "", err
	}
	defer c.Close()
	root := rootWindow(c)
	active, err := internAtom(c, "_NET_ACTIVE_WINDOW")
	if err != nil {
		return "", err
	}
	val, ok := getCardinal(c, root, active)
	if !ok || val == 0 {
		return "", nil
	}
	return formatX11Window(xproto.Window(val)), nil
}

func (*X11) Focus(ctx context.Context, ref string) error {
	win, err := parseX11Window(ref)
	if err != nil {
		return err
	}
	c, err := xgb.NewConn()
	if err != nil {
		return err
	}
	defer c.Close()
	root := rootWindow(c)
	active, err := internAtom(c, "_NET_ACTIVE_WINDOW")
	if err != nil {
		return err
	}
	// _NET_ACTIVE_WINDOW client message: data[0]=source indication (2 = pager),
	// data[1]=timestamp (0 = CurrentTime). Sent to the root with substructure
	// redirect/notify so the WM acts on it.
	ev := xproto.ClientMessageEvent{
		Format: 32,
		Window: win,
		Type:   active,
		Data:   xproto.ClientMessageDataUnionData32New([]uint32{2, 0, 0, 0, 0}),
	}
	mask := uint32(xproto.EventMaskSubstructureNotify | xproto.EventMaskSubstructureRedirect)
	return xproto.SendEventChecked(c, false, root, mask, string(ev.Bytes())).Check()
}

// Subscribe selects PropertyChange on the root window and translates the two
// EWMH properties we track into neutral events: _NET_ACTIVE_WINDOW → focus,
// _NET_CLIENT_LIST → layout (open/close). EWMH does not name the specific
// closed window in a list change, so X11 never emits EventWindowClosed; a
// closed terminal's session is dropped when its claude process dies, and the
// layout event re-reconciles the rest.
func (x *X11) Subscribe(ctx context.Context) (<-chan Event, error) {
	c, err := xgb.NewConn()
	if err != nil {
		return nil, err
	}
	root := rootWindow(c)
	at, err := internAtoms(c)
	if err != nil {
		c.Close()
		return nil, err
	}
	if err := xproto.ChangeWindowAttributesChecked(c, root, xproto.CwEventMask,
		[]uint32{xproto.EventMaskPropertyChange}).Check(); err != nil {
		c.Close()
		return nil, err
	}

	out := make(chan Event, 32)
	go func() {
		defer close(out)
		defer c.Close()
		go func() {
			<-ctx.Done()
			c.Close()
		}()
		for {
			raw, err := c.WaitForEvent()
			if raw == nil && err == nil {
				return // connection closed
			}
			if err != nil {
				continue
			}
			pn, ok := raw.(xproto.PropertyNotifyEvent)
			if !ok {
				continue
			}
			var ev Event
			switch pn.Atom {
			case at.activeWindow:
				ev = Event{Kind: EventFocusChanged}
				if val, ok := getCardinal(c, root, at.activeWindow); ok && val != 0 {
					ev.Address = formatX11Window(xproto.Window(val))
				}
			case at.clientList:
				ev = Event{Kind: EventLayoutChanged}
			default:
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

// --- conformance helpers (window id has no prefix quirk → identity) ---

func (*X11) NormalizeEventAddress(raw string) string { return raw }

func (*X11) RawForm(address string) string { return address }

// x11RecognizedProps maps the root properties the backend reacts to (the X11
// analog of an event name) to their neutral kind.
var x11RecognizedProps = map[string]EventKind{
	"_NET_ACTIVE_WINDOW": EventFocusChanged,
	"_NET_CLIENT_LIST":   EventLayoutChanged,
}

func (*X11) CanonicalEvents() []string {
	return []string{"_NET_ACTIVE_WINDOW", "_NET_CLIENT_LIST"}
}

func (*X11) TranslateEvent(name string) (string, bool) {
	if _, ok := x11RecognizedProps[name]; ok {
		return name, true
	}
	return "", false
}

// --- pure helpers (no X connection; unit-testable) ---

// parseWindowIDs decodes a _NET_CLIENT_LIST property value (an array of 32-bit
// window ids in the connection's byte order) into window handles.
func parseWindowIDs(buf []byte) []xproto.Window {
	out := make([]xproto.Window, 0, len(buf)/4)
	for i := 0; i+4 <= len(buf); i += 4 {
		out = append(out, xproto.Window(xgb.Get32(buf[i:])))
	}
	return out
}

func formatX11Window(w xproto.Window) string { return strconv.FormatUint(uint64(w), 10) }

// parseX11Window parses an opaque address back into a window handle. Base 0
// accepts both decimal (our Clients form) and 0x-hex refs defensively.
func parseX11Window(addr string) (xproto.Window, error) {
	n, err := strconv.ParseUint(addr, 0, 32)
	if err != nil {
		return 0, err
	}
	return xproto.Window(n), nil
}

// --- X connection helpers ---

type x11Atoms struct {
	clientList   xproto.Atom
	activeWindow xproto.Atom
	wmPID        xproto.Atom
	wmDesktop    xproto.Atom
	netWMName    xproto.Atom
}

func internAtoms(c *xgb.Conn) (x11Atoms, error) {
	var at x11Atoms
	var err error
	if at.clientList, err = internAtom(c, "_NET_CLIENT_LIST"); err != nil {
		return at, err
	}
	if at.activeWindow, err = internAtom(c, "_NET_ACTIVE_WINDOW"); err != nil {
		return at, err
	}
	if at.wmPID, err = internAtom(c, "_NET_WM_PID"); err != nil {
		return at, err
	}
	if at.wmDesktop, err = internAtom(c, "_NET_WM_DESKTOP"); err != nil {
		return at, err
	}
	if at.netWMName, err = internAtom(c, "_NET_WM_NAME"); err != nil {
		return at, err
	}
	return at, nil
}

func internAtom(c *xgb.Conn, name string) (xproto.Atom, error) {
	r, err := xproto.InternAtom(c, true, uint16(len(name)), name).Reply()
	if err != nil {
		return 0, err
	}
	return r.Atom, nil
}

func rootWindow(c *xgb.Conn) xproto.Window {
	return xproto.Setup(c).DefaultScreen(c).Root
}

// getCardinal reads a single 32-bit property (CARDINAL/WINDOW). Missing or
// empty → (0, false). Type is left as Any so CARDINAL and WINDOW both match.
func getCardinal(c *xgb.Conn, w xproto.Window, prop xproto.Atom) (uint32, bool) {
	r, err := xproto.GetProperty(c, false, w, prop, xproto.GetPropertyTypeAny, 0, 1).Reply()
	if err != nil || r == nil || len(r.Value) < 4 {
		return 0, false
	}
	return xgb.Get32(r.Value), true
}

func getString(c *xgb.Conn, w xproto.Window, prop xproto.Atom) string {
	r, err := xproto.GetProperty(c, false, w, prop, xproto.GetPropertyTypeAny, 0, 1<<10).Reply()
	if err != nil || r == nil {
		return ""
	}
	return string(r.Value)
}

// desktopOf returns the human-facing (1-indexed) workspace name and id for a
// window from _NET_WM_DESKTOP. 0xFFFFFFFF means "all desktops" (sticky) and is
// reported unresolved so chip ordering leaves it last.
func desktopOf(c *xgb.Conn, w xproto.Window, wmDesktop xproto.Atom) (string, int) {
	val, ok := getCardinal(c, w, wmDesktop)
	if !ok || val == 0xFFFFFFFF {
		return "", 0
	}
	id := int(val) + 1
	return strconv.Itoa(id), id
}
