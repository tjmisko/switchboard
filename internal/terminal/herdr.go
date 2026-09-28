package terminal

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/tjmisko/switchboard/internal/herdr"
)

// herdrLocator resolves ttys owned by herdr (herdr.dev), a terminal workspace
// manager in the tmux mould: a background server owns every pane's pty, and
// TUI clients attach to it from ordinary terminal windows. An agent in a herdr
// pane therefore has a tty driven by the herdr SERVER, and the window showing it
// belongs to whichever terminal runs an attached CLIENT.
//
// Both hops are exact:
//
//   - pane → tty. herdr's API lists panes without their tty, but
//     `pane.process_info` names the pane's shell pid, whose controlling tty is
//     the pane's pty. The result is only accepted when the server itself holds
//     that pty's master, so a recycled shell pid cannot lend a pane another
//     program's tty. A pane's tty is fixed for the life of its terminal_id, so
//     it is looked up once and cached.
//   - server → client → window. The kernel's unix socket table pairs each
//     client's connection with the server's accepted end, which carries the
//     server's client-socket path. The chosen client's controlling tty is set as
//     PaneRef.HostTTY; the chain resolves it through the outer terminal backends
//     (foot, wezterm, …) and copies that pane's window join onto this one.
//
// Focus is herdr's own `pane.focus`. A successful public focus moves EVERY
// attached client of that server to the pane's tab (herdr's
// focus_all_shell_clients_on_default_target), so raising any one client's
// window lands on the prompt; the client choice only has to be deterministic.
type herdrLocator struct {
	scan    *processScan
	sockets func() ([]unixSocket, error)
	call    herdr.Caller

	mu   sync.Mutex
	ttys map[herdrTerminalKey]string // pane terminal → its tty, see paneTTY
}

type herdrTerminalKey struct {
	apiSocket  string
	terminalID string
}

// herdrExe is the basename of the herdr binary; server, clients and one-shot CLI
// calls all run it.
const herdrExe = "herdr"

// NewHerdr returns the herdr terminal locator over the real /proc and socket
// table.
func NewHerdr() Locator { return newHerdr(newProcessScan("/proc", processScanTTL)) }

func newHerdr(scan *processScan) *herdrLocator {
	return newHerdrWith(scan, unixSocketTable, herdr.Call)
}

func newHerdrWith(scan *processScan, sockets func() ([]unixSocket, error), call herdr.Caller) *herdrLocator {
	return &herdrLocator{scan: scan, sockets: sockets, call: call, ttys: make(map[herdrTerminalKey]string)}
}

func (l *herdrLocator) Name() string { return "herdr" }

// Available reports whether any herdr process runs. Reading /proc without
// spawning keeps it cheap enough for auto to re-probe on every call.
func (l *herdrLocator) Available() bool {
	return len(l.scan.pidsRunning(herdrExe)) > 0
}

func (l *herdrLocator) Locate(ctx context.Context, tty string) (*PaneRef, error) {
	panes, err := l.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	pane, ok := panes[tty]
	if !ok {
		return nil, nil
	}
	return &pane, nil
}

// Snapshot lists every pane of every herdr server this user runs, keyed by tty.
// A server whose API fails makes the set incomplete, so it errors rather than
// report those panes' ttys as unowned.
func (l *herdrLocator) Snapshot(ctx context.Context) (map[string]PaneRef, error) {
	panes := make(map[string]PaneRef)
	topo, err := l.topology()
	if err != nil {
		return nil, err
	}
	seen := make(map[herdrTerminalKey]bool)
	for _, server := range topo.servers {
		list, err := herdr.ListPanes(ctx, l.call, server.apiSocket)
		if err != nil {
			return nil, fmt.Errorf("herdr %s: %w", server.apiSocket, err)
		}
		masters := make(map[int]bool)
		for _, index := range l.scan.ptyMasterIndexes(server.pid) {
			masters[index] = true
		}
		host := preferredHerdrClient(topo.clients[server.apiSocket])
		for _, p := range list {
			key := herdrTerminalKey{apiSocket: server.apiSocket, terminalID: p.TerminalID}
			seen[key] = true
			tty, ok := l.paneTTY(ctx, server, p, masters)
			if !ok {
				continue
			}
			// Two servers cannot drive one pty; first-wins only keeps the map
			// deterministic should the kernel ever say otherwise.
			if _, claimed := panes[tty]; claimed {
				continue
			}
			panes[tty] = herdrPaneRef(server, p, tty, host.tty)
		}
	}
	l.forgetTerminalsExcept(seen)
	return panes, nil
}

// herdrPaneRef converts one listed pane into the neutral PaneRef. Locate and
// Snapshot share it (Locate is answered from Snapshot) so the paths agree.
//
// Mux stays 0: the herdr server draws no window, so the WM join comes only from
// the host client's terminal, which the chain copies in through HostTTY.
func herdrPaneRef(server herdrServer, p herdr.Pane, tty, hostTTY string) PaneRef {
	cwd := p.ForegroundCWD
	if cwd == "" {
		cwd = p.CWD
	}
	return PaneRef{
		Backend:   "herdr",
		Handle:    p.PaneID,
		MuxSocket: server.apiSocket,
		Title:     p.TerminalTitle,
		TTY:       tty,
		CWD:       cwd,
		HostTTY:   hostTTY,
	}
}

// Activate selects the pane in herdr, which also switches every attached client
// to its tab. Raising a client's window is the WM seam's job, and activating
// the outer tab hosting that client is the chain's (via HostTTY).
func (l *herdrLocator) Activate(ctx context.Context, ref *PaneRef) error {
	if ref.Handle == "" || ref.MuxSocket == "" {
		return fmt.Errorf("herdr: pane ref has no pane id or socket")
	}
	return l.call(ctx, ref.MuxSocket, "pane.focus", map[string]string{"pane_id": ref.Handle}, nil)
}

// paneTTY returns the pane's tty from the cache or, for a terminal not seen
// before, from its shell's controlling tty. Either way the answer counts only
// while the server holds that pty's master: a cached tty stays exact because a
// terminal_id never changes pty, and a fresh one cannot be a recycled pid's.
func (l *herdrLocator) paneTTY(ctx context.Context, server herdrServer, p herdr.Pane, masters map[int]bool) (string, bool) {
	if p.TerminalID == "" || p.PaneID == "" {
		return "", false
	}
	key := herdrTerminalKey{apiSocket: server.apiSocket, terminalID: p.TerminalID}
	l.mu.Lock()
	tty, cached := l.ttys[key]
	l.mu.Unlock()
	if cached {
		return tty, ownsPTY(masters, tty)
	}

	var info struct {
		ProcessInfo struct {
			ShellPID int `json:"shell_pid"`
		} `json:"process_info"`
	}
	if err := l.call(ctx, server.apiSocket, "pane.process_info", map[string]string{"pane_id": p.PaneID}, &info); err != nil {
		return "", false // an exited pane has no process; retry next tick
	}
	tty, ok := l.scan.controllingTTY(info.ProcessInfo.ShellPID)
	if !ok || !ownsPTY(masters, tty) {
		return "", false
	}
	l.mu.Lock()
	l.ttys[key] = tty
	l.mu.Unlock()
	return tty, true
}

func ownsPTY(masters map[int]bool, tty string) bool {
	index, err := strconv.Atoi(strings.TrimPrefix(tty, "/dev/pts/"))
	return err == nil && strings.HasPrefix(tty, "/dev/pts/") && masters[index]
}

// forgetTerminalsExcept drops cached ttys for terminals no server listed, so the
// cache is bounded by the panes that exist.
func (l *herdrLocator) forgetTerminalsExcept(seen map[herdrTerminalKey]bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key := range l.ttys {
		if !seen[key] {
			delete(l.ttys, key)
		}
	}
}

// herdrServer is one running herdr server: its pid and its two listening
// sockets, the JSON API one and the TUI clients' one.
type herdrServer struct {
	pid          int
	apiSocket    string
	clientSocket string
}

// herdrClient is one attached TUI client and the tty it draws on.
type herdrClient struct {
	pid   int
	tty   string
	start uint64 // process start time, in clock ticks since boot
}

type herdrTopology struct {
	servers []herdrServer
	clients map[string][]herdrClient // by the server's API socket
}

// Unix socket states as sock_diag reports them (the TCP_* numbering).
const (
	unixStateEstablished = 1
	unixStateListen      = 10
)

// unixSocket is one row of the kernel's AF_UNIX socket table.
type unixSocket struct {
	Inode uint64
	Peer  uint64 // the connected peer's inode; 0 when unconnected
	State int
	Name  string // bound path; an accepted socket carries its listener's
}

// topology finds every herdr server and its attached clients from the herdr
// processes' fd tables joined with the kernel's socket table.
//
// A server is the process listening on both an API socket and the client socket
// herdr derives from it (`<dir>/<stem>-client.sock`): that pairing is herdr's
// own rule for every session and override, so no session naming is re-derived
// here. A client is a process whose socket's peer is the server's accepted end
// of the client socket.
func (l *herdrLocator) topology() (herdrTopology, error) {
	topo := herdrTopology{clients: make(map[string][]herdrClient)}
	pids := l.scan.pidsRunning(herdrExe)
	if len(pids) == 0 {
		return topo, nil
	}
	table, err := l.sockets()
	if err != nil {
		return topo, err
	}

	holders := make(map[uint64][]int)
	for _, pid := range pids {
		if !l.scan.runs(pid, herdrExe) {
			continue // the pid list is up to one TTL old
		}
		for _, inode := range l.scan.socketInodes(pid) {
			holders[inode] = append(holders[inode], pid)
		}
	}

	listening := make(map[int][]string)
	for _, s := range table {
		if s.State != unixStateListen || s.Name == "" {
			continue
		}
		for _, pid := range holders[s.Inode] {
			listening[pid] = append(listening[pid], s.Name)
		}
	}
	byClientSocket := make(map[string]herdrServer)
	for pid, names := range listening {
		for _, api := range names {
			client := herdrClientSocketFor(api)
			if client == api || !slices.Contains(names, client) {
				continue
			}
			server := herdrServer{pid: pid, apiSocket: api, clientSocket: client}
			topo.servers = append(topo.servers, server)
			byClientSocket[client] = server
		}
	}

	for _, s := range table {
		if s.State != unixStateEstablished || s.Peer == 0 {
			continue
		}
		server, ok := byClientSocket[s.Name]
		if !ok {
			continue
		}
		for _, pid := range holders[s.Peer] {
			if pid == server.pid {
				continue
			}
			tty, ok := l.scan.controllingTTY(pid)
			if !ok {
				continue // no terminal, so no window: e.g. a remote bridge
			}
			topo.clients[server.apiSocket] = append(topo.clients[server.apiSocket],
				herdrClient{pid: pid, tty: tty, start: l.scan.startTime(pid)})
		}
	}
	slices.SortFunc(topo.servers, func(a, b herdrServer) int { return strings.Compare(a.apiSocket, b.apiSocket) })
	return topo, nil
}

// herdrClientSocketFor mirrors herdr's derive_client_socket_from_api_socket:
// `<dir>/<stem>-client.sock`, stem being the file name minus its extension.
func herdrClientSocketFor(apiSocket string) string {
	base := filepath.Base(apiSocket)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	return filepath.Join(filepath.Dir(apiSocket), stem+"-client.sock")
}

// preferredHerdrClient picks the client whose window a jump raises: the most
// recently started, then the highest pid. Every attached client shows the
// focused pane, so any is correct; this only keeps repeated jumps on one
// window. herdr knows which client was used last but does not expose it.
func preferredHerdrClient(clients []herdrClient) herdrClient {
	var best herdrClient
	for _, c := range clients {
		if best.pid == 0 || c.start > best.start || (c.start == best.start && c.pid > best.pid) {
			best = c
		}
	}
	return best
}
