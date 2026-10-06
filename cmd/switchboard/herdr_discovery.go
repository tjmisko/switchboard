package main

import (
	"context"
	"slices"
	"time"

	"github.com/tjmisko/switchboard/internal/herdr"
	"github.com/tjmisko/switchboard/internal/osproc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/terminal"
)

// herdrDiscovery finds the agents herdr detects in a pane that have no provider
// adapter (Pi, OpenCode, Cursor, Gemini, Copilot, …). Most of them the process
// scanner does not classify. Pi it does, so a Pi in herdr is found by both and
// admitRoot merges the two into one session carrying the pane. Claude Code and
// Codex are left to the scanner and their provider adapters; herdr only decides
// their status (herdr_status.go).
//
// It also owns which herdr servers the status watcher follows. It reads the
// herdr backend alone, not the composed terminal chain: a chain snapshot drops
// the panes of a backend that errored, and following that set would stop
// watching every server on one bad read.
type herdrDiscovery struct {
	panes  terminal.Snapshotter
	status herdrStatusSource
	call   herdr.Caller
	procs  osproc.Source

	// pids caches each pane's agent process, so a tool the agent runs in the
	// foreground cannot take its place; it is re-validated every tick, by
	// lifetime and not by pid alone.
	pids map[herdr.PaneKey]herdrAgentProcess
}

type herdrAgentProcess struct {
	lifetime osproc.Lifetime
	agent    string
	tty      string
}

// herdrCandidate is one herdr-observed agent: its process and the pane it runs
// in.
type herdrCandidate struct {
	info  osproc.Info
	agent string
	pane  terminal.PaneRef
}

func newHerdrDiscovery(panes terminal.Snapshotter, status herdrStatusSource, call herdr.Caller, procs osproc.Source) *herdrDiscovery {
	return &herdrDiscovery{panes: panes, status: status, call: call, procs: procs, pids: make(map[herdr.PaneKey]herdrAgentProcess)}
}

// tick enumerates herdr's panes, points the watcher at their servers, and
// returns every herdr-observed agent. ok is false when the enumeration failed;
// the watched set is then kept as it was.
func (d *herdrDiscovery) tick(ctx context.Context) (candidates []herdrCandidate, ok bool) {
	panes, err := d.panes.Snapshot(ctx)
	if err != nil {
		return nil, false
	}
	d.status.SetServers(ctx, herdrSockets(panes))

	seen := make(map[herdr.PaneKey]bool)
	ttys := make([]string, 0, len(panes))
	for tty := range panes {
		ttys = append(ttys, tty)
	}
	slices.Sort(ttys) // deterministic order for logs and tests
	for _, tty := range ttys {
		pane := panes[tty]
		if pane.Backend != "herdr" {
			continue
		}
		key := herdr.PaneKey{Socket: pane.MuxSocket, PaneID: pane.Handle}
		status, live := d.status.Status(key)
		if !live || status.Agent == "" || state.IsProviderAgent(status.Agent) {
			continue
		}
		seen[key] = true
		info, ok := d.agentProcess(ctx, key, status.Agent, tty)
		if !ok {
			continue
		}
		candidates = append(candidates, herdrCandidate{info: info, agent: status.Agent, pane: pane})
	}
	for key := range d.pids {
		if !seen[key] {
			delete(d.pids, key)
		}
	}
	return candidates, true
}

// agentProcess returns the process running a pane's agent: the cached one while
// the same lifetime still runs on the pane's tty, else the pane's foreground
// process group leader from herdr's process_info. A cached pid that another
// process took over, even on the same tty, is not the cached agent (#97); nor
// is one whose birth token cannot be verified, so it is looked up afresh.
func (d *herdrDiscovery) agentProcess(ctx context.Context, key herdr.PaneKey, agent, tty string) (osproc.Info, bool) {
	if cached, ok := d.pids[key]; ok && cached.agent == agent && cached.tty == tty {
		info, err := d.procs.Read(cached.lifetime.PID)
		if err == nil && info.TTY == tty && osproc.CompareBirth(info.Birth, cached.lifetime.Birth) == osproc.BirthSame {
			return info, true
		}
	}
	var result struct {
		ProcessInfo struct {
			ForegroundProcessGroupID int `json:"foreground_process_group_id"`
			ForegroundProcesses      []struct {
				PID int `json:"pid"`
			} `json:"foreground_processes"`
		} `json:"process_info"`
	}
	if err := d.call(ctx, key.Socket, "pane.process_info", map[string]string{"pane_id": key.PaneID}, &result); err != nil {
		return osproc.Info{}, false
	}
	pid := 0
	for _, p := range result.ProcessInfo.ForegroundProcesses {
		if p.PID == result.ProcessInfo.ForegroundProcessGroupID {
			pid = p.PID // the group leader is the command the shell launched
			break
		}
		if pid == 0 {
			pid = p.PID
		}
	}
	if pid <= 0 {
		return osproc.Info{}, false
	}
	info, err := d.procs.Read(pid)
	if err != nil || info.TTY != tty {
		return osproc.Info{}, false // exited, or not the pane's: retry next tick
	}
	d.pids[key] = herdrAgentProcess{lifetime: info.Lifetime(), agent: agent, tty: tty}
	return info, true
}

// herdrDiscoveryInterval matches the process scanner's cadence, so an agent
// started in herdr appears as fast as one the scanner finds.
const herdrDiscoveryInterval = time.Second

// runHerdrDiscovery announces every herdr-observed agent lifetime once per
// daemon run. That includes one hydrated from state.json: like the scanner's
// survivors, it is announced again so its pidfd death watch exists in this
// process (appear keeps the hydrated lifetime when the birth token matches). A
// pid tracked as some other agent is left alone. Announcements are keyed by
// lifetime, so a pid reused by a new agent process is announced again, and
// admitRoot gives it nothing of the old one. A session ends like any other,
// when its process dies (the pidfd watch, or the liveness sweep's
// processIsSession check).
func runHerdrDiscovery(ctx context.Context, store *state.Store, d *herdrDiscovery, interval time.Duration, appear func(osproc.Info, string, *terminal.PaneRef)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	announced := make(map[osproc.Lifetime]string) // lifetime → agent, for this daemon run
	for {
		candidates, ok := d.tick(ctx)
		if ok {
			tracked := make(map[int]string)
			for _, sess := range store.Snapshot().Sessions {
				tracked[sess.PID] = sess.Agent
			}
			current := make(map[osproc.Lifetime]string, len(candidates))
			for _, c := range candidates {
				lifetime := c.info.Lifetime()
				current[lifetime] = c.agent
				if announced[lifetime] == c.agent {
					continue
				}
				if agent, ok := tracked[c.info.PID]; ok && agent != c.agent {
					continue
				}
				announced[lifetime] = c.agent
				pane := c.pane
				appear(c.info, c.agent, &pane)
			}
			for lifetime, agent := range announced {
				if current[lifetime] != agent {
					delete(announced, lifetime)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
