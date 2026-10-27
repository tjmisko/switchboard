package main

import (
	"time"

	"github.com/tjmisko/switchboard/internal/provider"
	codexprovider "github.com/tjmisko/switchboard/internal/provider/codex"
	"github.com/tjmisko/switchboard/internal/state"
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

// codexTurnUsageLimitSource is the app-server's own view of a thread's newest
// usage-limit turn error. It is read alongside the rollout tails: the rollout
// is the durable witness, the app-server read is the same fact from the loaded
// thread and needs no file.
type codexTurnUsageLimitSource interface {
	TurnUsageLimit(provider.RootKey, string) (time.Time, *time.Time, bool)
}

// codexTurnFailureSource is the app-server's view of the root's newest turn
// when it ended on a network or auth error (turnfailure.Kind as a string).
type codexTurnFailureSource interface {
	TurnFailure(provider.RootKey, string) (time.Time, string, bool)
}

// codexTurnEndSource is the app-server's view of whether the root's own newest
// turn is terminal. It is the release edge the hook stream omits for a capped,
// interrupted or aborted turn.
type codexTurnEndSource interface {
	TurnEnd(provider.RootKey, string) (string, time.Time, bool)
}

// codexLimitScan is one root's usage-limit scan state: when it last ran. Each
// rollout's tail verdict is cached by its file identity in ReadRolloutState
// (internal/tailcache), so an unchanged rollout is stat'ed, not re-read.
type codexLimitScan struct {
	rootID    string
	scannedAt time.Time
}

type codexLimitEvidence struct {
	at     time.Time
	limit  *codexprovider.RolloutUsageLimit
	source string
	// rootTurnEnd marks evidence that the root's own turn ended. A subagent's
	// cap leaves the root waiting, so only a root turn end releases it.
	rootTurnEnd bool
}

// witnessBetter reports whether candidate a should replace the current best b.
// Reset quality decides first: an exact rate-limit epoch beats a reset parsed
// from the error message. For equal quality the declared source order decides,
// then recency within one source.
func witnessBetter(a, b *codexLimitEvidence) bool {
	if b == nil {
		return true
	}
	if a.limit.ResetFromEpoch != b.limit.ResetFromEpoch {
		return a.limit.ResetFromEpoch
	}
	if a.source != b.source {
		return codexUsageLimitSourceRank(a.source) < codexUsageLimitSourceRank(b.source)
	}
	return a.at.After(b.at)
}

// codexUsageLimitSourceRank orders two witnesses of equal reset quality. The
// rollout is the durable witness this scan is built on, so it wins the tie: an
// app_server source then means the durable witness was unavailable.
func codexUsageLimitSourceRank(source string) int {
	if source == state.UsageLimitSourceCodexAppServer {
		return 1
	}
	return 0
}

// rolloutLimitEvidence wraps a rollout cap so the scan can compare it against
// the app-server's witness.
func rolloutLimitEvidence(at time.Time, limit *codexprovider.RolloutUsageLimit, rootTurnEnd bool) *codexLimitEvidence {
	return &codexLimitEvidence{at: at, limit: limit, source: state.UsageLimitSourceCodexRollout, rootTurnEnd: rootTurnEnd}
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

	// The app-server's own turn error is the alongside path: the same fact the
	// rollout records, read from the loaded thread, so a cap is seen even when
	// no rollout path is bound yet.
	var newest *codexLimitEvidence
	consider := func(e *codexLimitEvidence) {
		if witnessBetter(e, newest) {
			newest = e
		}
	}
	// A network or auth failure is parked evidence like a cap. Only the root's own
	// turn end counts: a subagent's failure does not say the root is stuck.
	var failure *state.TurnFailure
	considerFailure := func(at time.Time, kind, source string) {
		if failure == nil || at.After(failure.ObservedAt) {
			failure = &state.TurnFailure{ObservedAt: at, Kind: kind, Source: source}
		}
	}
	if source, ok := c.codex.(codexTurnFailureSource); ok {
		if at, kind, ok := source.TurnFailure(ref.Key(), rootID); ok {
			considerFailure(at, kind, state.TurnFailureSourceCodexAppServer)
		}
	}
	if source, ok := c.codex.(codexTurnUsageLimitSource); ok {
		if at, resetsAt, ok := source.TurnUsageLimit(ref.Key(), rootID); ok {
			consider(&codexLimitEvidence{
				at:          at,
				limit:       &codexprovider.RolloutUsageLimit{ResetsAt: resetsAt},
				source:      state.UsageLimitSourceCodexAppServer,
				rootTurnEnd: true,
			})
		}
	}
	if rootPath != "" || len(childPaths) > 0 {
		c.codexLimitMu.Lock()
		if c.codexLimitScans == nil {
			c.codexLimitScans = make(map[provider.RootKey]*codexLimitScan)
		}
		scan := c.codexLimitScans[ref.Key()]
		if scan == nil || scan.rootID != rootID {
			scan = &codexLimitScan{rootID: rootID}
			c.codexLimitScans[ref.Key()] = scan
		}
		if !scan.scannedAt.IsZero() && now.Sub(scan.scannedAt) < codexUsageLimitScanInterval {
			c.codexLimitMu.Unlock()
			return
		}
		scan.scannedAt = now
		c.codexLimitMu.Unlock()

		// The reads run outside every coordinator lock; the claim on scannedAt
		// above is what keeps two ticks from scanning the same root at once.
		root := readCodexRolloutState(rootPath)
		if root.UsageLimit != nil {
			consider(rolloutLimitEvidence(root.At, root.UsageLimit, true))
		}
		if root.TurnFailure != "" {
			considerFailure(root.At, root.TurnFailure, state.TurnFailureSourceCodexRollout)
		}
		for path := range childPaths {
			child := readCodexRolloutState(path)
			if child.UsageLimit == nil || !child.At.After(root.At) {
				continue
			}
			consider(rolloutLimitEvidence(child.At, child.UsageLimit, false))
		}
	}

	if failure != nil && failure.ObservedAt.After(hookAt) {
		c.recordCodexTurnFailure(ref, rootID, *failure)
	}
	if newest == nil || !newest.at.After(hookAt) {
		return
	}
	c.recordCodexUsageLimit(ref, rootID, newest.at, newest.limit, newest.source)
	if newest.rootTurnEnd {
		// The cap proves the root's turn ended, so anything it still holds red
		// is stale: release the latches and retire the red now, not only from
		// the published overlay.
		c.clearCodexOnUsageLimit(ref, rootID, newest.at, now)
	}
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

// readCodexRolloutState returns the tail verdict for path, re-reading only
// when the file's identity moved (ReadRolloutState caches it). An unreadable
// or empty path reads as no evidence.
func readCodexRolloutState(path string) codexprovider.RolloutState {
	if path == "" {
		return codexprovider.RolloutState{}
	}
	state, err := codexprovider.ReadRolloutState(path)
	if err != nil {
		return codexprovider.RolloutState{}
	}
	return state
}

func (c *agentCoordinator) forgetCodexLimitScan(key provider.RootKey) {
	c.codexLimitMu.Lock()
	delete(c.codexLimitScans, key)
	c.codexLimitMu.Unlock()
}
