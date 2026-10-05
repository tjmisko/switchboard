package main

import (
	"os"
	"time"

	"github.com/tjmisko/switchboard/internal/provider"
	codexprovider "github.com/tjmisko/switchboard/internal/provider/codex"
)

// codexUsageLimitScanInterval throttles the scan per root. Observe runs on
// every provider update, and the cap it looks for lasts hours.
const codexUsageLimitScanInterval = 5 * time.Second

// codexRolloutPathSource is the app-server's view of where each thread of a
// root writes its rollout (Thread.path). It needs no hook, so it is what finds
// a cap already on disk when the daemon restarts beneath an idle session.
type codexRolloutPathSource interface {
	RolloutPaths(provider.RootKey, string) map[string]string
}

// codexLimitScan is one root's usage-limit scan state: when it last ran and
// the tail verdict of every rollout it read, keyed by path.
type codexLimitScan struct {
	rootID    string
	scannedAt time.Time
	tails     map[string]codexRolloutTail
}

// codexRolloutTail caches one rollout's tail verdict against the file's size
// and mtime, so an unchanged rollout is stat'ed, not re-read.
type codexRolloutTail struct {
	size    int64
	modTime time.Time
	state   codexprovider.RolloutState
}

type codexLimitEvidence struct {
	at    time.Time
	limit *codexprovider.RolloutUsageLimit
}

// scanCodexUsageLimit looks for a usage-limit turn end in the root's rollout
// and in each subagent's. Codex fires no hook for either, and a capped
// subagent leaves its parent parked in a wait that the app-server reports as
// live work, so the rollouts are the only witness.
//
// A subagent's cap counts only when it is newer than the root rollout's newest
// turn marker: a root that moved on after its child was capped is not blocked
// by it. Evidence must also postdate the newest hook, as for the root.
func (c *agentCoordinator) scanCodexUsageLimit(ref provider.RootRef, now time.Time) {
	sess, ok := sessionForKey(c.store.Snapshot(), ref.Key())
	if !ok || sess.AgentGraph == nil || sess.AgentGraph.RootID == "" {
		return
	}
	rootID := sess.AgentGraph.RootID
	rootPath, childPaths, hookAt, quiet := c.codexHookRolloutPaths(ref.Key(), rootID, now)
	if !quiet {
		return
	}
	if source, ok := c.codex.(codexRolloutPathSource); ok {
		for threadID, path := range source.RolloutPaths(ref.Key(), rootID) {
			if threadID == rootID {
				if rootPath == "" {
					rootPath = path
				}
				continue
			}
			childPaths[path] = struct{}{}
		}
	}
	delete(childPaths, rootPath)
	if rootPath == "" && len(childPaths) == 0 {
		return
	}

	c.codexLimitMu.Lock()
	if c.codexLimitScans == nil {
		c.codexLimitScans = make(map[provider.RootKey]*codexLimitScan)
	}
	scan := c.codexLimitScans[ref.Key()]
	if scan == nil || scan.rootID != rootID {
		scan = &codexLimitScan{rootID: rootID, tails: make(map[string]codexRolloutTail)}
		c.codexLimitScans[ref.Key()] = scan
	}
	if !scan.scannedAt.IsZero() && now.Sub(scan.scannedAt) < codexUsageLimitScanInterval {
		c.codexLimitMu.Unlock()
		return
	}
	scan.scannedAt = now
	seen := make(map[string]struct{}, len(childPaths)+1)
	root := scan.read(rootPath, seen)
	var newest *codexLimitEvidence
	if root.UsageLimit != nil {
		newest = &codexLimitEvidence{at: root.At, limit: root.UsageLimit}
	}
	for path := range childPaths {
		child := scan.read(path, seen)
		if child.UsageLimit == nil || !child.At.After(root.At) {
			continue
		}
		if newest == nil || child.At.After(newest.at) {
			newest = &codexLimitEvidence{at: child.At, limit: child.UsageLimit}
		}
	}
	for path := range scan.tails {
		if _, ok := seen[path]; !ok {
			delete(scan.tails, path)
		}
	}
	c.codexLimitMu.Unlock()

	if newest == nil || !newest.at.After(hookAt) {
		return
	}
	c.recordCodexUsageLimit(ref, rootID, newest.at, newest.limit)
}

// codexHookRolloutPaths returns the rollouts hooks have named for rootID and
// the newest hook time. quiet is false while hooks are still arriving inside
// the quiet window, or while the hook reducer is bound to another thread. With
// no hook since the daemon started, the root is quiet by definition.
func (c *agentCoordinator) codexHookRolloutPaths(key provider.RootKey, rootID string, now time.Time) (rootPath string, childPaths map[string]struct{}, hookAt time.Time, quiet bool) {
	childPaths = make(map[string]struct{})
	c.codexHookMu.Lock()
	defer c.codexHookMu.Unlock()
	root := c.codexHookRoots[key]
	if root == nil {
		return "", childPaths, time.Time{}, true
	}
	if root.sessionID != "" && root.sessionID != rootID {
		return "", childPaths, time.Time{}, false
	}
	if now.Sub(root.latestAt) < codexTranscriptQuietWindow {
		return "", childPaths, time.Time{}, false
	}
	for _, path := range root.childTranscripts {
		childPaths[path] = struct{}{}
	}
	return root.transcript, childPaths, root.latestAt, true
}

// read returns the tail verdict for path, re-reading only when the file's size
// or mtime moved. An unreadable or empty path reads as no evidence.
func (s *codexLimitScan) read(path string, seen map[string]struct{}) codexprovider.RolloutState {
	if path == "" {
		return codexprovider.RolloutState{}
	}
	seen[path] = struct{}{}
	info, err := os.Stat(path)
	if err != nil {
		delete(s.tails, path)
		return codexprovider.RolloutState{}
	}
	if cached, ok := s.tails[path]; ok && cached.size == info.Size() && cached.modTime.Equal(info.ModTime()) {
		return cached.state
	}
	state, err := codexprovider.ReadRolloutState(path)
	if err != nil {
		delete(s.tails, path)
		return codexprovider.RolloutState{}
	}
	s.tails[path] = codexRolloutTail{size: info.Size(), modTime: info.ModTime(), state: state}
	return state
}

func (c *agentCoordinator) forgetCodexLimitScan(key provider.RootKey) {
	c.codexLimitMu.Lock()
	delete(c.codexLimitScans, key)
	c.codexLimitMu.Unlock()
}
