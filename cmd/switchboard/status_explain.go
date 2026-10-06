package main

import (
	"fmt"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/provider"
	"github.com/tjmisko/switchboard/internal/remotestate"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statusexplain"
)

// admissionRecord is the landing path's part of one root's decision record
// (#95). The resolver's decision lives on the session (state.ExplainStatus);
// this holds what only the coordinator sees: the graph it last landed, the
// observations refused before they could become evidence (older than their
// kind's latest), and, while nothing has landed, why observation stopped
// short.
//
// It is in memory only, guarded by agentCoordinator.mu, keyed by the process
// lifetime (RootKey), and bound to one provider session id: a record for any
// other conversation is replaced, not merged.
type admissionRecord struct {
	sessionID string

	admittedSource agentgraph.SourceKind
	admittedAt     time.Time

	rejected statusexplain.Rejections
	outcome  statusexplain.Reason
}

func (c *agentCoordinator) admissionLocked(key provider.RootKey, sessionID string) *admissionRecord {
	if c.admissions == nil {
		c.admissions = make(map[provider.RootKey]*admissionRecord)
	}
	rec := c.admissions[key]
	if rec == nil || rec.sessionID != sessionID {
		rec = &admissionRecord{sessionID: sessionID}
		c.admissions[key] = rec
	}
	return rec
}

// recordRejectionLocked files an observation admission refused for reason.
// Its status is what it would have reduced to had it landed.
func (c *agentCoordinator) recordRejectionLocked(key provider.RootKey, observation agentgraph.Observation, reason statusexplain.Reason, now time.Time) {
	rec := c.admissionLocked(key, observation.RootID)
	rec.rejected.Add(statusexplain.Candidate{
		Source: string(observation.Source), EvidenceKind: statusexplain.EvidenceKindOf(observation.Source),
		Status:     agentgraph.Reduce(observation, agentgraph.Summary{}, now).LegacyStatus,
		ObservedAt: observation.ObservedAt, FreshUntil: observation.FreshUntil, RejectReason: reason,
	})
}

// recordAdmissionLocked files the graph that landed. A different graph than
// the one held starts a fresh rejected list, since those candidates lost to a
// graph that no longer decides; the same graph re-landing keeps it.
func (c *agentCoordinator) recordAdmissionLocked(key provider.RootKey, graph *state.AgentGraph) {
	rec := c.admissionLocked(key, graph.RootID)
	if rec.admittedSource != graph.Source || !rec.admittedAt.Equal(graph.ObservedAt) {
		rec.rejected.Reset()
	}
	rec.admittedSource, rec.admittedAt = graph.Source, graph.ObservedAt
	rec.outcome = ""
}

// recordObserveOutcome files why a tick observed nothing for the root:
// binding_missing or coverage_unsupported.
func (c *agentCoordinator) recordObserveOutcome(ref provider.RootRef, reason statusexplain.Reason) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.admissionLocked(ref.Key(), ref.ProviderSessionID).outcome = reason
}

// Explain explains the published status of the local root with pid as of
// snap, which must be the store's own (unpublished) snapshot: the usage-limit
// overlay is applied by the explanation, at snap.UpdatedAt.
func (c *agentCoordinator) Explain(snap state.Snapshot, pid int) (statusexplain.Decision, error) {
	var sess state.Session
	found := false
	for _, candidate := range snap.Sessions {
		if candidate.PID == pid {
			sess, found = candidate, true
			break
		}
	}
	if !found {
		return statusexplain.Decision{}, fmt.Errorf("no session with pid %d", pid)
	}
	d := sess.ExplainStatus(snap.UpdatedAt)

	c.mu.Lock()
	defer c.mu.Unlock()
	if rec := c.admissions[providerRootKey(sess)]; rec != nil && rec.sessionID == d.Root.SessionID {
		rec.mergeInto(&d)
	}
	return d.Sanitize(), nil
}

// mergeInto adds the landing path's part to the resolver's decision: the
// reason observation stopped short of one, and the observations refused
// before they became evidence.
func (rec *admissionRecord) mergeInto(d *statusexplain.Decision) {
	decided := &d.Choice
	if d.Underlying != nil {
		decided = d.Underlying
	}
	switch decided.Reason {
	case statusexplain.ReasonBindingMissing, statusexplain.ReasonObservationPending:
		if rec.outcome != "" {
			decided.Reason = rec.outcome
		}
	}
	rec.rejected.Into(d)
}

// ExplainLocal answers the explain RPC. It explains this machine's roots only:
// a hostname naming any other host is refused rather than answered from local
// state.
func (c *agentCoordinator) ExplainLocal(localHostname, hostname string, pid int) (statusexplain.Decision, error) {
	if hostname != "" {
		canonical, err := remotestate.CanonicalHostname(hostname)
		if err != nil || canonical != localHostname {
			return statusexplain.Decision{}, fmt.Errorf("explain is local only; run it on %s", hostname)
		}
	}
	return c.Explain(c.store.Snapshot(), pid)
}
