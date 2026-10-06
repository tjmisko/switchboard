package main

import (
	"os"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/provider"
	piprovider "github.com/tjmisko/switchboard/internal/provider/pi"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statustune"
)

// piSessionFileLease is how long one read of a Pi session file's tail stays
// evidence: the Codex rollout read's window, since both are transcript
// evidence renewed by re-reading while no hook speaks for the root.
const piSessionFileLease = codexTranscriptQuietWindow

// piSessionTail caches one root's session-file verdict against the file's
// size and mtime, so an unchanged file is stat'ed, not re-read.
type piSessionTail struct {
	path    string
	size    int64
	modTime time.Time
	tail    piprovider.SessionTail
}

// piSessionFileCandidate is a Pi root that no hook has reached in this daemon
// but whose restored block names its session file.
type piSessionFileCandidate struct {
	key        provider.RootKey
	sessionID  string
	path       string
	freshUntil time.Time // the current session-file graph's lease, zero if none
}

// seedPiFromSessionFiles covers the gap a daemon restart leaves (phase-1 plan,
// 1.7). A restored Pi block has no live authority, and its hooks rebuild the
// reducer only when Pi next fires one, which for an idle Pi may be hours away.
// Until then the tail of Pi's own session file decides, read through the
// shared bounded tail reader. The newest message on the active branch decides:
// the user's (Pi appends it as a run starts) or an assistant message stopped
// for a tool → working; an assistant message stopped for anything else → idle.
//
// The read lands as a pi_session_file graph with piSessionFileLease and is
// renewed while the file stays readable. It yields to herdr outright
// (piStatusAuthority), so a root with a live herdr reading is not read at all,
// and it stops for good once a hook reaches the root: the reducer owns it from
// then on.
func (c *agentCoordinator) seedPiFromSessionFiles(now time.Time) {
	snap := c.store.Snapshot()
	c.piMu.Lock()
	var candidates []piSessionFileCandidate
	for _, sess := range snap.Sessions {
		key := providerRootKey(sess)
		if sess.Agent != state.AgentKindPi || sess.Pi == nil || sess.Pi.SessionID == "" || sess.Pi.Transcript == "" {
			continue
		}
		if c.piRoots[key] != nil || (sess.Herdr != nil && sess.Herdr.Live) {
			delete(c.piTails, key)
			continue
		}
		g := sess.AgentGraph
		bound := g != nil && g.RootID == sess.Pi.SessionID
		// The file records no dialog, so it cannot clear a red: a restored
		// one keeps its deadline unless a hook or herdr says otherwise, as a
		// Codex rollout read leaves a hook-owned wait alone.
		if bound && g.Fresh(now) && g.Summary.Attention != agentgraph.AttentionNone {
			continue
		}
		candidate := piSessionFileCandidate{key: key, sessionID: sess.Pi.SessionID, path: sess.Pi.Transcript}
		if bound && g.Source == agentgraph.SourcePiSessionFile {
			candidate.freshUntil = g.FreshUntil
		}
		candidates = append(candidates, candidate)
	}
	c.piMu.Unlock()

	for _, candidate := range candidates {
		c.seedPiFromSessionFile(candidate, now)
	}
}

// seedPiFromSessionFile reads one candidate's tail and lands it. The file is
// read outside every lock; the reducer lock is then retaken to confirm no hook
// claimed the root meanwhile.
func (c *agentCoordinator) seedPiFromSessionFile(candidate piSessionFileCandidate, now time.Time) {
	info, err := os.Stat(candidate.path)
	if err != nil {
		c.piMu.Lock()
		delete(c.piTails, candidate.key)
		c.piMu.Unlock()
		return
	}
	c.piMu.Lock()
	cached, ok := c.piTails[candidate.key]
	c.piMu.Unlock()
	unchanged := ok && cached.path == candidate.path && cached.size == info.Size() && cached.modTime.Equal(info.ModTime())
	var tail piprovider.SessionTail
	switch {
	case unchanged && now.Before(candidate.freshUntil.Add(-piSessionFileLease/2)):
		// An unchanged file renews its lease only once half of it has run, so
		// a quiet Pi republishes every lease/2 rather than every tick.
		return
	case unchanged:
		tail = cached.tail
	default:
		if tail, err = piprovider.ReadSessionTail(candidate.path); err != nil {
			return
		}
	}

	c.piMu.Lock()
	defer c.piMu.Unlock()
	if c.piRoots[candidate.key] != nil {
		return // a hook arrived during the read; the reducer owns the root
	}
	c.piTails[candidate.key] = &piSessionTail{path: candidate.path, size: info.Size(), modTime: info.ModTime(), tail: tail}
	if tail.Runtime == agentgraph.RuntimeUnknown {
		return // no evidence: whatever the restart restored keeps its deadline
	}
	updatedAt := tail.At
	if updatedAt.IsZero() || updatedAt.After(now) {
		updatedAt = now
	}
	observation := agentgraph.Observation{
		Provider: agentgraph.ProviderPi, RootID: candidate.sessionID, Source: agentgraph.SourcePiSessionFile,
		ObservedAt: now, FreshUntil: now.Add(piSessionFileLease),
		Nodes: []agentgraph.Node{{ID: candidate.sessionID, Runtime: tail.Runtime, UpdatedAt: updatedAt}},
	}

	applied := false
	before, after, cwd := "", "", ""
	var beforeSince time.Time
	c.store.Apply(func(sessions map[int]*state.Session) {
		s := sessions[candidate.key.PID]
		if s == nil || !s.StartedAt.Equal(candidate.key.StartedAt) || s.Agent != state.AgentKindPi ||
			s.Pi == nil || s.Pi.SessionID != candidate.sessionID {
			return
		}
		carryPiRootDetail(&observation, s.AgentGraph)
		projected, err := state.ProjectAgentGraph(observation, s.AgentGraph, now)
		if err != nil {
			return
		}
		beforeSince = s.Pi.StatusSince
		before, after = s.SetPiHookGraph(projected, now)
		cwd, applied = s.CWD, true
	})
	if !applied || before == after {
		return
	}
	c.sink.Record(history.Event{
		Ts: now, Type: history.EventTransition, SessionID: candidate.sessionID,
		PID: candidate.key.PID, Agent: state.AgentKindPi, CWD: cwd,
		From: before, To: after, Rule: statustune.RulePiSessionFileRead, DurPrevMs: history.HeldMs(beforeSince, now),
	})
	logPiDecision(candidate.key.PID, candidate.sessionID, before, after, statustune.RulePiSessionFileRead,
		"pi session file tail runtime="+string(tail.Runtime), beforeSince, now)
}

// carryPiRootDetail copies the usage and billing identity the current graph
// holds for the observation's root, so a status-only observation does not
// erase them.
func carryPiRootDetail(observation *agentgraph.Observation, current *state.AgentGraph) {
	if current == nil || current.RootID != observation.RootID || current.Source == agentgraph.SourceHerdr {
		return
	}
	for _, node := range current.Nodes {
		if node.ID != current.RootID {
			continue
		}
		root := &observation.Nodes[0]
		root.Billing = node.Billing
		root.Usage = agentgraph.Usage{
			InputTokens: node.Usage.InputTokens, CachedInputTokens: node.Usage.CachedInputTokens,
			CacheWriteInputTokens: node.Usage.CacheWriteInputTokens, OutputTokens: node.Usage.OutputTokens,
			ReasoningOutputTokens: node.Usage.ReasoningOutputTokens, TotalTokens: node.Usage.TotalTokens,
			ModelContextWindow: node.Usage.ModelContextWindow,
		}
		return
	}
}
