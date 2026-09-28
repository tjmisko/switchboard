package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/tjmisko/switchboard/internal/herdr"
)

// herdrFixture models one host: a /proc tree, the kernel's unix socket table,
// and the herdr API answers of each server socket.
type herdrFixture struct {
	proc    fakeProc
	sockets []unixSocket
	panes   map[string][]herdr.Pane // API socket → pane.list
	shells  map[string]int          // API socket + pane id → shell pid
	failing map[string]error        // API socket → error for every call
	calls   []string                // "method socket pane_id", in order
	inode   uint64
}

func newHerdrFixture(t *testing.T) *herdrFixture {
	return &herdrFixture{
		proc:    newFakeProc(t),
		panes:   make(map[string][]herdr.Pane),
		shells:  make(map[string]int),
		failing: make(map[string]error),
		inode:   9000,
	}
}

func (f *herdrFixture) nextInode() uint64 {
	f.inode++
	return f.inode
}

// socket gives pid an fd on a new socket inode and returns it.
func (f *herdrFixture) socket(pid int) uint64 {
	inode := f.nextInode()
	f.proc.fd(pid, int(inode), "socket:["+strconv.FormatUint(inode, 10)+"]", "")
	return inode
}

// stat writes a /proc/<pid>/stat with a controlling tty (0 for none) and start
// time. The comm deliberately holds a space and a paren, as real ones can.
func (f *herdrFixture) stat(pid int, ttyNr uint32, start uint64) {
	f.proc.t.Helper()
	fields := []string{"S", "1", strconv.Itoa(pid), strconv.Itoa(pid), strconv.FormatUint(uint64(ttyNr), 10)}
	for len(fields) < 19 {
		fields = append(fields, "0")
	}
	fields = append(fields, strconv.FormatUint(start, 10), "0", "0")
	body := strconv.Itoa(pid) + " (her dr) " + strings.Join(fields, " ") + "\n"
	if err := os.WriteFile(filepath.Join(f.proc.root, strconv.Itoa(pid), "stat"), []byte(body), 0o644); err != nil {
		f.proc.t.Fatal(err)
	}
}

// ptsDev encodes /dev/pts/index as the kernel's tty_nr (new_encode_dev).
func ptsDev(index int) uint32 {
	minor := uint32(index)
	return (minor & 0xff) | unix98PTYSlaveMajor<<8 | (minor&^0xff)<<12
}

// server starts a herdr server process listening on apiSocket and its derived
// client socket, driving one pty per pane.
func (f *herdrFixture) server(pid int, apiSocket string, panes ...herdrFixturePane) {
	f.proc.process(pid, "/home/u/.local/bin/herdr")
	f.stat(pid, 0, 100)
	f.sockets = append(f.sockets,
		unixSocket{Inode: f.socket(pid), State: unixStateListen, Name: apiSocket},
		unixSocket{Inode: f.socket(pid), State: unixStateListen, Name: herdrClientSocketFor(apiSocket)},
	)
	for _, p := range panes {
		f.proc.fd(pid, 100+p.pts, "/dev/ptmx", ptmxInfo(p.pts))
		f.proc.process(p.shell, "/usr/bin/bash")
		f.stat(p.shell, ptsDev(p.pts), 200)
		f.panes[apiSocket] = append(f.panes[apiSocket], herdr.Pane{
			PaneID: p.id, TerminalID: "term_" + p.id, CWD: "/pane", ForegroundCWD: p.fgCWD, TerminalTitle: p.title,
		})
		f.shells[apiSocket+" "+p.id] = p.shell
	}
}

type herdrFixturePane struct {
	id    string
	pts   int
	shell int
	fgCWD string
	title string
}

// client attaches a TUI client process running on /dev/pts/tty to the server
// listening on apiSocket.
func (f *herdrFixture) client(pid, serverPID int, apiSocket string, tty int, start uint64) {
	f.proc.process(pid, "/home/u/.local/bin/herdr")
	f.stat(pid, ptsDev(tty), start)
	clientEnd, serverEnd := f.socket(pid), f.socket(serverPID)
	f.sockets = append(f.sockets,
		unixSocket{Inode: clientEnd, Peer: serverEnd, State: unixStateEstablished},
		unixSocket{Inode: serverEnd, Peer: clientEnd, State: unixStateEstablished, Name: herdrClientSocketFor(apiSocket)},
	)
}

func (f *herdrFixture) call(_ context.Context, socket, method string, params, result any) error {
	paneID := ""
	if p, ok := params.(map[string]string); ok {
		paneID = p["pane_id"]
	}
	f.calls = append(f.calls, strings.TrimSpace(method+" "+socket+" "+paneID))
	if err := f.failing[socket]; err != nil {
		return err
	}
	var body any
	switch method {
	case "pane.list":
		body = map[string]any{"panes": f.panes[socket]}
	case "pane.process_info":
		shell, ok := f.shells[socket+" "+paneID]
		if !ok {
			return errors.New("not_found")
		}
		body = map[string]any{"process_info": map[string]any{"pane_id": paneID, "shell_pid": shell}}
	case "pane.focus":
		return nil
	default:
		return errors.New("unexpected method " + method)
	}
	raw, _ := json.Marshal(body)
	return json.Unmarshal(raw, result)
}

func (f *herdrFixture) locator() *herdrLocator {
	return newHerdrWith(newProcessScan(f.proc.root, 0), func() ([]unixSocket, error) { return f.sockets, nil }, f.call)
}

func (f *herdrFixture) countCalls(method string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, method+" ") {
			n++
		}
	}
	return n
}

const testHerdrSocket = "/home/u/.config/herdr/herdr.sock"

func TestHerdrShouldLocateAPaneByItsShellsControllingTTYWhenTheServerDrivesIt(t *testing.T) {
	f := newHerdrFixture(t)
	f.server(4000, testHerdrSocket, herdrFixturePane{id: "w1:p2", pts: 7, shell: 4100, fgCWD: "/repo", title: "✳ Claude Code"})
	f.client(4200, 4000, testHerdrSocket, 3, 500)

	pane, err := f.locator().Locate(context.Background(), "/dev/pts/7")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	want := PaneRef{
		Backend: "herdr", Handle: "w1:p2", MuxSocket: testHerdrSocket,
		Title: "✳ Claude Code", TTY: "/dev/pts/7", CWD: "/repo", HostTTY: "/dev/pts/3",
	}
	if pane == nil || *pane != want {
		t.Fatalf("Locate = %+v, want %+v", pane, want)
	}
}

func TestHerdrShouldFallBackToThePaneCWDWhenTheForegroundCWDIsUnknown(t *testing.T) {
	f := newHerdrFixture(t)
	f.server(4000, testHerdrSocket, herdrFixturePane{id: "w1:p1", pts: 7, shell: 4100})

	pane, _ := f.locator().Locate(context.Background(), "/dev/pts/7")
	if pane == nil || pane.CWD != "/pane" {
		t.Fatalf("Locate = %+v, want cwd /pane", pane)
	}
}

func TestHerdrShouldHostOnTheMostRecentlyStartedClientWhenSeveralAttach(t *testing.T) {
	f := newHerdrFixture(t)
	f.server(4000, testHerdrSocket, herdrFixturePane{id: "w1:p1", pts: 7, shell: 4100})
	f.client(4300, 4000, testHerdrSocket, 3, 900) // newest
	f.client(4200, 4000, testHerdrSocket, 5, 500)

	pane, _ := f.locator().Locate(context.Background(), "/dev/pts/7")
	if pane == nil || pane.HostTTY != "/dev/pts/3" {
		t.Fatalf("Locate = %+v, want host /dev/pts/3 (the newest client)", pane)
	}
}

func TestHerdrShouldLocateThePaneWithNoHostWhenNoClientIsAttached(t *testing.T) {
	f := newHerdrFixture(t)
	f.server(4000, testHerdrSocket, herdrFixturePane{id: "w1:p1", pts: 7, shell: 4100})

	pane, err := f.locator().Locate(context.Background(), "/dev/pts/7")
	if err != nil || pane == nil {
		t.Fatalf("Locate = %+v, %v; want the pane", pane, err)
	}
	if pane.HostTTY != "" {
		t.Fatalf("HostTTY = %q, want empty for a detached server", pane.HostTTY)
	}
}

func TestHerdrShouldJoinEachClientToItsOwnServerWhenSessionsRunSideBySide(t *testing.T) {
	f := newHerdrFixture(t)
	work := "/home/u/.config/herdr/sessions/work/herdr.sock"
	f.server(4000, testHerdrSocket, herdrFixturePane{id: "w1:p1", pts: 7, shell: 4100})
	f.server(5000, work, herdrFixturePane{id: "w1:p1", pts: 8, shell: 5100})
	f.client(4200, 4000, testHerdrSocket, 3, 500)
	f.client(5200, 5000, work, 4, 900)

	panes, err := f.locator().Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := panes["/dev/pts/7"]; got.MuxSocket != testHerdrSocket || got.HostTTY != "/dev/pts/3" {
		t.Errorf("default pane = %+v, want default socket hosted on pts/3", got)
	}
	if got := panes["/dev/pts/8"]; got.MuxSocket != work || got.HostTTY != "/dev/pts/4" {
		t.Errorf("work pane = %+v, want work socket hosted on pts/4", got)
	}
}

// Any herdr CLI call (`herdr pane list`, an agent's hook) connects to the API
// socket, not the client socket. Hosting a jump on its short-lived tty would
// raise whatever terminal ran the command.
func TestHerdrShouldNotTreatAnAPICallerAsAClientWhenItConnectsToTheAPISocket(t *testing.T) {
	f := newHerdrFixture(t)
	f.server(4000, testHerdrSocket, herdrFixturePane{id: "w1:p1", pts: 7, shell: 4100})
	f.proc.process(4400, "/home/u/.local/bin/herdr")
	f.stat(4400, ptsDev(9), 999)
	cli, srv := f.socket(4400), f.socket(4000)
	f.sockets = append(f.sockets,
		unixSocket{Inode: cli, Peer: srv, State: unixStateEstablished},
		unixSocket{Inode: srv, Peer: cli, State: unixStateEstablished, Name: testHerdrSocket},
	)

	pane, _ := f.locator().Locate(context.Background(), "/dev/pts/7")
	if pane == nil || pane.HostTTY != "" {
		t.Fatalf("Locate = %+v, want no host: the API caller is not a client", pane)
	}
}

// A remote bridge or a client started without a terminal has no window.
func TestHerdrShouldIgnoreAClientWithoutAControllingTTY(t *testing.T) {
	f := newHerdrFixture(t)
	f.server(4000, testHerdrSocket, herdrFixturePane{id: "w1:p1", pts: 7, shell: 4100})
	f.client(4200, 4000, testHerdrSocket, 3, 500)
	f.client(4300, 4000, testHerdrSocket, 0, 900)
	f.stat(4300, 0, 900) // detach the newest client from any terminal

	pane, _ := f.locator().Locate(context.Background(), "/dev/pts/7")
	if pane == nil || pane.HostTTY != "/dev/pts/3" {
		t.Fatalf("Locate = %+v, want host /dev/pts/3", pane)
	}
}

// process_info's shell pid can exit and be reused between the reply and the
// stat read. Only a pty the server itself drives can be the pane's.
func TestHerdrShouldSkipAPaneWhenItsShellTTYIsNotDrivenByTheServer(t *testing.T) {
	f := newHerdrFixture(t)
	f.server(4000, testHerdrSocket, herdrFixturePane{id: "w1:p1", pts: 7, shell: 4100})
	f.stat(4100, ptsDev(12), 200) // the pid now belongs to a process on another pty

	panes, err := f.locator().Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(panes) != 0 {
		t.Fatalf("Snapshot = %+v, want no pane for an undriven tty", panes)
	}
}

func TestHerdrShouldCacheAPanesTTYWhenItsTerminalWasSeenBefore(t *testing.T) {
	f := newHerdrFixture(t)
	f.server(4000, testHerdrSocket, herdrFixturePane{id: "w1:p1", pts: 7, shell: 4100})
	l := f.locator()

	for range 3 {
		if panes, err := l.Snapshot(context.Background()); err != nil || len(panes) != 1 {
			t.Fatalf("Snapshot = %+v, %v; want one pane", panes, err)
		}
	}
	if n := f.countCalls("pane.process_info"); n != 1 {
		t.Fatalf("pane.process_info calls = %d, want 1 (cached after the first)", n)
	}
}

func TestHerdrShouldDropACachedTTYWhenTheServerNoLongerDrivesIt(t *testing.T) {
	f := newHerdrFixture(t)
	f.server(4000, testHerdrSocket, herdrFixturePane{id: "w1:p1", pts: 7, shell: 4100})
	l := f.locator()
	if panes, _ := l.Snapshot(context.Background()); len(panes) != 1 {
		t.Fatalf("first Snapshot = %+v, want one pane", panes)
	}

	if err := os.Remove(filepath.Join(f.proc.root, "4000", "fd", "107")); err != nil {
		t.Fatal(err)
	}
	if panes, _ := l.Snapshot(context.Background()); len(panes) != 0 {
		t.Fatalf("Snapshot = %+v, want the cached pane gone once its pty master closed", panes)
	}
}

func TestHerdrShouldForgetACachedTTYWhenItsTerminalCloses(t *testing.T) {
	f := newHerdrFixture(t)
	f.server(4000, testHerdrSocket, herdrFixturePane{id: "w1:p1", pts: 7, shell: 4100})
	l := f.locator()
	_, _ = l.Snapshot(context.Background())

	f.panes[testHerdrSocket] = nil
	_, _ = l.Snapshot(context.Background())
	if len(l.ttys) != 0 {
		t.Fatalf("tty cache = %v, want empty after the terminal closed", l.ttys)
	}
}

// A server whose API fails leaves its panes' ttys unknown, not unowned.
func TestHerdrShouldErrorWhenAServersAPIFails(t *testing.T) {
	f := newHerdrFixture(t)
	f.server(4000, testHerdrSocket, herdrFixturePane{id: "w1:p1", pts: 7, shell: 4100})
	f.failing[testHerdrSocket] = errors.New("connection refused")

	if panes, err := f.locator().Snapshot(context.Background()); err == nil {
		t.Fatalf("Snapshot = %+v, nil; want an error for an incomplete set", panes)
	}
}

func TestHerdrShouldOwnNoPanesWithoutErrorWhenNoHerdrProcessRuns(t *testing.T) {
	f := newHerdrFixture(t)
	f.proc.process(2001, "/usr/bin/foot")
	l := newHerdrWith(newProcessScan(f.proc.root, 0), func() ([]unixSocket, error) {
		t.Fatal("socket table read with no herdr process running")
		return nil, nil
	}, f.call)

	if l.Available() {
		t.Fatal("Available() = true with no herdr process")
	}
	panes, err := l.Snapshot(context.Background())
	if err != nil || panes == nil || len(panes) != 0 {
		t.Fatalf("Snapshot = %+v, %v; want an empty, non-nil set", panes, err)
	}
}

// A process listening on a socket that merely looks like herdr's, without the
// derived client socket beside it, is not a server.
func TestHerdrShouldNotTreatAListenerAsAServerWhenItHasNoClientSocket(t *testing.T) {
	f := newHerdrFixture(t)
	f.proc.process(4000, "/home/u/.local/bin/herdr")
	f.sockets = append(f.sockets, unixSocket{Inode: f.socket(4000), State: unixStateListen, Name: testHerdrSocket})

	panes, err := f.locator().Snapshot(context.Background())
	if err != nil || len(panes) != 0 || len(f.calls) != 0 {
		t.Fatalf("Snapshot = %+v, %v with calls %v; want nothing asked of a non-server", panes, err, f.calls)
	}
}

func TestHerdrShouldFocusThePaneOnItsOwnServerWhenActivated(t *testing.T) {
	f := newHerdrFixture(t)
	work := "/home/u/.config/herdr/sessions/work/herdr.sock"
	err := f.locator().Activate(context.Background(), &PaneRef{Backend: "herdr", Handle: "w2:p3", MuxSocket: work})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if len(f.calls) != 1 || f.calls[0] != "pane.focus "+work+" w2:p3" {
		t.Fatalf("calls = %v, want one pane.focus of w2:p3 on the work socket", f.calls)
	}
}

func TestHerdrShouldRefuseToActivateWhenTheRefHasNoPaneID(t *testing.T) {
	f := newHerdrFixture(t)
	if err := f.locator().Activate(context.Background(), &PaneRef{Backend: "herdr", MuxSocket: testHerdrSocket}); err == nil {
		t.Fatal("Activate = nil, want an error for a ref with no pane id")
	}
}

func TestHerdrClientSocketShouldInsertClientBeforeTheExtensionLikeHerdr(t *testing.T) {
	for api, want := range map[string]string{
		"/c/herdr/herdr.sock":               "/c/herdr/herdr-client.sock",
		"/c/herdr/sessions/work/herdr.sock": "/c/herdr/sessions/work/herdr-client.sock",
		"/run/custom.sock":                  "/run/custom-client.sock",
		"/run/noext":                        "/run/noext-client.sock",
		"/c/herdr/herdr-client.sock":        "/c/herdr/herdr-client-client.sock",
	} {
		if got := herdrClientSocketFor(api); got != want {
			t.Errorf("herdrClientSocketFor(%q) = %q, want %q", api, got, want)
		}
	}
}

func TestPtsIndexShouldDecodeTheKernelDevEncodingWhenTheIndexExceedsAByte(t *testing.T) {
	for _, index := range []int{0, 7, 255, 256, 4097} {
		got, ok := ptsIndexFromDev(ptsDev(index))
		if !ok || got != index {
			t.Errorf("ptsIndexFromDev(ptsDev(%d)) = %d, %v", index, got, ok)
		}
	}
	if _, ok := ptsIndexFromDev(4<<8 | 1); ok { // /dev/tty1
		t.Error("ptsIndexFromDev accepted a virtual console")
	}
}
