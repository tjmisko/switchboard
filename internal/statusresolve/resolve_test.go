package statusresolve

import (
	"reflect"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/statusexplain"
)

func target(provider string) Target {
	return Target{Root: statusexplain.Root{PID: 7, StartedAt: lifetime, Provider: provider, SessionID: sessionID}, PaneID: "w1:p1"}
}

var (
	claude = target("claude")
	codex  = target("codex")
	pi     = target("pi")
)

func herdr(agent, status string, since time.Time, now time.Time) Candidate {
	return Herdr(HerdrReading{PaneID: "w1:p1", Agent: agent, Status: status, Live: true, Since: since}, now)
}

const (
	active = agentgraph.RuntimeActive
	idle   = agentgraph.RuntimeIdle
	none   = agentgraph.AttentionNone
	input  = agentgraph.AttentionUserInput
	ask    = agentgraph.AttentionApproval
)

func claudeSnap(at time.Time, lease time.Duration, r agentgraph.RuntimeState, a agentgraph.AttentionState, children ...agentgraph.Node) Candidate {
	return ClaudeGraph(lifetime, observation(agentgraph.ProviderClaude, agentgraph.SourceClaudeTranscript, at, lease, r, a, children...))
}

func claudeHook(at time.Time, lease time.Duration, r agentgraph.RuntimeState, a agentgraph.AttentionState) Candidate {
	return ClaudeHook(lifetime, observation(agentgraph.ProviderClaude, agentgraph.SourceHook, at, lease, r, a))
}

func codexSnap(at time.Time, lease time.Duration, r agentgraph.RuntimeState, a agentgraph.AttentionState, children ...agentgraph.Node) Candidate {
	return CodexAppServer(lifetime, observation(agentgraph.ProviderCodex, agentgraph.SourceCodexAppServer, at, lease, r, a, children...))
}

func codexHook(at time.Time, lease time.Duration, r agentgraph.RuntimeState, a agentgraph.AttentionState) Candidate {
	return CodexHook(lifetime, observation(agentgraph.ProviderCodex, agentgraph.SourceHook, at, lease, r, a))
}

func codexRollout(at time.Time, lease time.Duration, r agentgraph.RuntimeState) Candidate {
	return CodexRolloutTail(lifetime, observation(agentgraph.ProviderCodex, agentgraph.SourceCodexRollout, at, lease, r, none))
}

func piHook(at time.Time, lease time.Duration, r agentgraph.RuntimeState, a agentgraph.AttentionState) Candidate {
	return PiHook(lifetime, observation(agentgraph.ProviderPi, agentgraph.SourceHook, at, lease, r, a))
}

func restored(provider agentgraph.ProviderKind, at time.Time, lease time.Duration, r agentgraph.RuntimeState, a agentgraph.AttentionState) Candidate {
	return RestoredLastKnown(lifetime, observation(provider, agentgraph.SourceHook, at, lease, r, a))
}

// rejected returns the reason d rejected the candidate from source with
// status, "" when it holds none.
func rejected(d statusexplain.Decision, source agentgraph.SourceKind, status string) statusexplain.Reason {
	for _, c := range d.Rejected {
		if c.Source == string(source) && c.Status == status {
			return c.RejectReason
		}
	}
	return ""
}

func want(t *testing.T, name string, d statusexplain.Decision, status string, reason statusexplain.Reason) {
	t.Helper()
	if d.Status != status || d.Reason != reason {
		t.Errorf("%s: decided %q for %q, want %q for %q", name, d.Status, d.Reason, status, reason)
	}
}

func TestRankShouldOrderEveryKindWhenItsBuilderDeclaresIt(t *testing.T) {
	o := func(p agentgraph.ProviderKind) agentgraph.Observation {
		return observation(p, agentgraph.SourceHook, t0, time.Minute, active, none)
	}
	order := [][]Candidate{
		{PiHook(lifetime, o(agentgraph.ProviderPi))},
		{herdr("claude", "working", t0, t0)},
		{ClaudeGraph(lifetime, o(agentgraph.ProviderClaude)), CodexAppServer(lifetime, o(agentgraph.ProviderCodex))},
		{ClaudeHook(lifetime, o(agentgraph.ProviderClaude)), CodexHook(lifetime, o(agentgraph.ProviderCodex))},
		{CodexRolloutTail(lifetime, o(agentgraph.ProviderCodex)), PiSessionTail(lifetime, o(agentgraph.ProviderPi))},
		{RestoredLastKnown(lifetime, o(agentgraph.ProviderPi))},
		{CodexChildHooks(lifetime, o(agentgraph.ProviderCodex)), {Kind: statusexplain.EvidenceUsageLimit}, {Kind: "bogus"}},
	}
	for tier, cs := range order {
		for _, c := range cs {
			if Rank(c) != Rank(order[tier][0]) {
				t.Errorf("%s/%s ranks %d, want %d", c.Kind, c.Source, Rank(c), Rank(order[tier][0]))
			}
			if tier > 0 && Rank(c) >= Rank(order[tier-1][0]) {
				t.Errorf("%s/%s ranks %d, not below tier %d", c.Kind, c.Source, Rank(c), tier-1)
			}
		}
	}
	if Rank(order[len(order)-1][0]) != rankNone {
		t.Error("a partial edge ranks to select a root status")
	}
}

// --- #96: fresh unresolved provider attention survives terminal readings ---

func TestResolveShouldHoldAttentionWhenATerminalReadingSaysWorkingOrIdle(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target Target
		red    Candidate
		herdr  string
	}{
		{"codex input under herdr working", codex, codexSnap(t0, time.Minute, active, input), "working"},
		{"codex approval under herdr idle", codex, codexSnap(t0, time.Minute, idle, ask), "idle"},
		{"claude approval snapshot under herdr working", claude, claudeSnap(t0, time.Minute, active, ask), "working"},
		{"claude hook approval under herdr done", claude, claudeHook(t0, time.Minute, active, ask), "done"},
	} {
		now := t0.Add(time.Second)
		reading := herdr(tc.target.Root.Provider, tc.herdr, now, now)
		d := Resolve(tc.target, []Candidate{tc.red, reading}, statusexplain.Decision{}, now)
		want(t, tc.name, d, agentgraph.LegacyPermission, statusexplain.ReasonAttentionHeld)
		if d.Source != string(tc.red.Source) || d.EvidenceKind != tc.red.Kind {
			t.Errorf("%s: attributed to %s/%s", tc.name, d.Source, d.EvidenceKind)
		}
		if got := rejected(d, agentgraph.SourceHerdr, reading.Status); got != statusexplain.ReasonAttentionHeld {
			t.Errorf("%s: herdr rejected for %q, want attention_held", tc.name, got)
		}
	}
}

func TestResolveShouldHoldRedForAFreshPiDialogWhenHerdrReadsWorking(t *testing.T) {
	now := t0.Add(time.Second)
	d := Resolve(pi, []Candidate{piHook(t0, time.Hour, active, input), herdr("pi", "working", now, now)}, statusexplain.Decision{}, now)
	want(t, "pi dialog", d, agentgraph.LegacyPermission, statusexplain.ReasonEventAuthority)
	if got := rejected(d, agentgraph.SourceHerdr, agentgraph.LegacyWorking); got != statusexplain.ReasonSourceOutranked {
		t.Errorf("herdr rejected for %q", got)
	}
	// The dialog's writer withdrawing does not release it either, until its
	// deadline: herdr cannot resolve a request it cannot see.
	held := Resolve(pi, []Candidate{herdr("pi", "working", now, now)}, d, now.Add(time.Minute))
	want(t, "pi dialog after hook withdrawal", held, agentgraph.LegacyPermission, statusexplain.ReasonAttentionHeld)
}

// --- request resolution: only authorized evidence resolves attention ---

func TestResolveShouldReleaseAttentionWhenAuthorizedEvidenceReportsItResolved(t *testing.T) {
	t1 := t0.Add(time.Second)
	for _, tc := range []struct {
		name     string
		target   Target
		red      Candidate
		resolver Candidate
		want     string
	}{
		{"pi dialog closed by its own hook", pi, piHook(t0, time.Hour, active, input), piHook(t1, time.Hour, active, none), agentgraph.LegacyWorking},
		{"codex input answered by its own hook", codex, codexHook(t0, time.Hour, idle, input), codexHook(t1, time.Hour, active, none), agentgraph.LegacyWorking},
		{"codex hook request cleared by a newer snapshot", codex, codexHook(t0, time.Hour, idle, input), codexSnap(t1, time.Minute, active, none), agentgraph.LegacyWorking},
		{"claude hook request cleared by a newer transcript graph", claude, claudeHook(t0, time.Hour, idle, ask), claudeSnap(t1, time.Minute, active, none), agentgraph.LegacyWorking},
	} {
		now := t1.Add(time.Millisecond)
		reading := herdr(tc.target.Root.Provider, "working", t0, now)
		first := Resolve(tc.target, []Candidate{tc.red, reading}, statusexplain.Decision{}, t1)
		if first.Status != agentgraph.LegacyPermission {
			t.Fatalf("%s: setup decided %q", tc.name, first.Status)
		}
		d := Resolve(tc.target, []Candidate{tc.resolver, reading}, first, now)
		if d.Status != tc.want {
			t.Errorf("%s: decided %q for %q, want %q", tc.name, d.Status, d.Reason, tc.want)
		}
	}
}

func TestResolveShouldKeepAttentionWhenOnlyUnauthorizedEvidenceSaysOtherwise(t *testing.T) {
	t1 := t0.Add(time.Second)
	now := t1.Add(time.Millisecond)
	for _, tc := range []struct {
		name   string
		target Target
		red    Candidate
		others []Candidate
	}{
		{"codex snapshot request against a newer codex hook", codex, codexSnap(t0, time.Minute, idle, input),
			[]Candidate{codexHook(t1, time.Hour, active, none)}},
		{"codex request against a newer rollout idle", codex, codexHook(t0, time.Hour, idle, input),
			[]Candidate{codexRollout(t1, time.Minute, idle)}},
		{"claude hook request against an older transcript graph", claude, claudeHook(t1, time.Hour, idle, ask),
			[]Candidate{claudeSnap(t0, time.Minute, active, none)}},
		{"claude snapshot request against a newer claude hook", claude, claudeSnap(t0, time.Minute, idle, ask),
			[]Candidate{claudeHook(t1, time.Hour, active, none)}},
		{"pi request against restored idle", pi, piHook(t0, time.Hour, active, input),
			[]Candidate{restored(agentgraph.ProviderPi, t1, time.Hour, idle, none)}},
		{"codex request against herdr idle", codex, codexSnap(t0, time.Minute, idle, ask),
			[]Candidate{herdr("codex", "idle", t1, now)}},
	} {
		// The request held as the prior decision, its writer still present.
		prior := Resolve(tc.target, []Candidate{tc.red}, statusexplain.Decision{}, tc.red.ObservedAt)
		d := Resolve(tc.target, append([]Candidate{tc.red}, tc.others...), prior, now)
		if d.Status != agentgraph.LegacyPermission {
			t.Errorf("%s: decided %q for %q, want permission held", tc.name, d.Status, d.Reason)
		}
		// And with the writer withdrawn, held from the prior decision.
		d = Resolve(tc.target, tc.others, prior, now)
		if d.Status != agentgraph.LegacyPermission || d.Reason != statusexplain.ReasonAttentionHeld {
			t.Errorf("%s, writer withdrawn: decided %q for %q, want permission held", tc.name, d.Status, d.Reason)
		}
	}
}

// --- source withdrawal and expiry: previous evidence holds to its deadline ---

func TestResolveShouldHoldPriorAttentionOnlyUntilItsDeadlineWhenItsWriterWithdraws(t *testing.T) {
	first := Resolve(codex, []Candidate{codexSnap(t0, 30*time.Second, idle, input), herdr("codex", "working", t0, t0)}, statusexplain.Decision{}, t0)
	want(t, "open", first, agentgraph.LegacyPermission, statusexplain.ReasonAttentionHeld)

	at := t0.Add(10 * time.Second)
	unavailable := codexSnap(at, 30*time.Second, agentgraph.RuntimeUnknown, none)
	held := Resolve(codex, []Candidate{unavailable, herdr("codex", "working", t0, at)}, first, at)
	want(t, "writer unavailable", held, agentgraph.LegacyPermission, statusexplain.ReasonAttentionHeld)
	if !held.FreshUntil.Equal(t0.Add(30 * time.Second)) {
		t.Errorf("held attention renewed its deadline to %s", held.FreshUntil)
	}

	at = t0.Add(30 * time.Second)
	lapsed := Resolve(codex, []Candidate{herdr("codex", "working", t0, at)}, held, at)
	want(t, "deadline passed", lapsed, agentgraph.LegacyWorking, statusexplain.ReasonTerminalAuthority)
}

func TestResolveShouldContributeNothingWhenCandidatesAreUnknownAndHoldPriorOnlyThroughItsDeadline(t *testing.T) {
	first := Resolve(codex, []Candidate{codexSnap(t0, 30*time.Second, active, none)}, statusexplain.Decision{}, t0)
	want(t, "first", first, agentgraph.LegacyWorking, statusexplain.ReasonGraphAuthority)

	at := t0.Add(10 * time.Second)
	unknown := []Candidate{codexSnap(at, 30*time.Second, agentgraph.RuntimeUnknown, none), herdr("codex", "unknown", at, at)}
	held := Resolve(codex, unknown, first, at)
	want(t, "unknown readings", held, agentgraph.LegacyWorking, statusexplain.ReasonPriorHeld)
	if !held.ObservedAt.Equal(t0) || !held.FreshUntil.Equal(t0.Add(30*time.Second)) || !held.DecidedAt.Equal(at) {
		t.Errorf("held decision window %s..%s decided %s", held.ObservedAt, held.FreshUntil, held.DecidedAt)
	}
	for _, c := range held.Rejected {
		if c.RejectReason != statusexplain.ReasonCoverageUnsupported {
			t.Errorf("unknown candidate %s rejected for %q", c.Source, c.RejectReason)
		}
	}

	at = t0.Add(30 * time.Second)
	lapsed := Resolve(codex, unknown, held, at)
	want(t, "prior deadline passed", lapsed, "", statusexplain.ReasonCoverageUnsupported)
}

func TestResolveShouldNotHoldATerminalReadingWhenHerdrWithdraws(t *testing.T) {
	gemini := Target{Root: statusexplain.Root{PID: 9, StartedAt: lifetime, Provider: "gemini"}, PaneID: "w1:p1"}
	first := Resolve(gemini, []Candidate{herdr("gemini", "blocked", t0, t0)}, statusexplain.Decision{}, t0)
	want(t, "followed", first, agentgraph.LegacyPermission, statusexplain.ReasonTerminalAuthority)

	at := t0.Add(time.Second)
	withdrawn := Herdr(HerdrReading{PaneID: "w1:p1", Agent: "gemini", Status: "blocked", Live: false, Since: t0}, at)
	d := Resolve(gemini, []Candidate{withdrawn}, first, at)
	want(t, "withdrawn", d, "", statusexplain.ReasonObservationExpired)
}

func TestResolveShouldFallToTheGraphWhenHerdrWithdraws(t *testing.T) {
	snap := claudeSnap(t0, time.Hour, idle, none)
	first := Resolve(claude, []Candidate{snap, herdr("claude", "working", t0, t0)}, statusexplain.Decision{}, t0)
	want(t, "followed", first, agentgraph.LegacyWorking, statusexplain.ReasonTerminalAuthority)
	at := t0.Add(time.Second)
	withdrawn := Herdr(HerdrReading{PaneID: "w1:p1", Agent: "claude", Status: "working", Live: false, Since: t0}, at)
	d := Resolve(claude, []Candidate{snap, withdrawn}, first, at)
	want(t, "withdrawn", d, agentgraph.LegacyIdle, statusexplain.ReasonGraphAuthority)
	if got := rejected(d, agentgraph.SourceHerdr, agentgraph.LegacyWorking); got != statusexplain.ReasonObservationExpired {
		t.Errorf("withdrawn herdr rejected for %q", got)
	}
}

// --- expired or identity-mismatched candidates cannot select ---

func TestResolveShouldRejectExpiredCandidatesWhenTheyWouldOtherwiseDecide(t *testing.T) {
	at := t0.Add(time.Minute)
	expired := piHook(t0, time.Minute, active, input)
	d := Resolve(pi, []Candidate{expired, herdr("pi", "idle", t0, at)}, statusexplain.Decision{}, at)
	want(t, "expired pi dialog", d, agentgraph.LegacyIdle, statusexplain.ReasonTerminalAuthority)
	if got := rejected(d, agentgraph.SourceHook, agentgraph.LegacyPermission); got != statusexplain.ReasonObservationExpired {
		t.Errorf("expired hook rejected for %q", got)
	}

	alone := Resolve(pi, []Candidate{expired}, statusexplain.Decision{}, at)
	want(t, "expired alone", alone, "", statusexplain.ReasonObservationExpired)

	future := Resolve(claude, []Candidate{claudeSnap(at.Add(time.Second), time.Minute, active, none)}, statusexplain.Decision{}, at)
	want(t, "observed after now", future, "", statusexplain.ReasonObservationExpired)
}

func TestResolveShouldRejectIdentityMismatchesWhenAnyIdentityFieldDiffers(t *testing.T) {
	fallback := claudeSnap(t0, time.Hour, idle, none)
	other := func(edit func(*Candidate)) Candidate {
		c := claudeSnap(t0.Add(time.Second), time.Hour, active, ask)
		edit(&c)
		return c
	}
	for _, tc := range []struct {
		name      string
		candidate Candidate
	}{
		{"herdr sees another agent", herdr("codex", "working", t0, t0)},
		{"herdr sees no agent", herdr("", "working", t0, t0)},
		{"herdr reads another pane", Herdr(HerdrReading{PaneID: "w2:p1", Agent: "claude", Status: "working", Live: true, Since: t0}, t0)},
		{"another session", other(func(c *Candidate) { c.Identity.SessionID = "sess-2" })},
		{"another process lifetime", other(func(c *Candidate) { c.Identity.StartedAt = lifetime.Add(time.Second) })},
		{"another provider", other(func(c *Candidate) { c.Identity.Provider = "codex" })},
		{"no lifetime at all", other(func(c *Candidate) { c.Identity.StartedAt = time.Time{} })},
	} {
		at := t0.Add(2 * time.Second)
		d := Resolve(claude, []Candidate{tc.candidate, fallback}, statusexplain.Decision{}, at)
		want(t, tc.name, d, agentgraph.LegacyIdle, statusexplain.ReasonGraphAuthority)
		if got := rejected(d, tc.candidate.Source, tc.candidate.Status); got != statusexplain.ReasonIdentityMismatch {
			t.Errorf("%s: rejected for %q", tc.name, got)
		}
		alone := Resolve(claude, []Candidate{tc.candidate}, statusexplain.Decision{}, at)
		want(t, tc.name+" alone", alone, "", statusexplain.ReasonIdentityMismatch)
	}

	unbound := claude
	unbound.Root.SessionID = ""
	d := Resolve(unbound, []Candidate{claudeSnap(t0, time.Hour, active, none)}, statusexplain.Decision{}, t0)
	want(t, "unbound root", d, "", statusexplain.ReasonIdentityMismatch)
}

func TestResolveShouldIgnoreAPriorDecisionWhenItIsAboutAnotherRoot(t *testing.T) {
	prior := Resolve(codex, []Candidate{codexSnap(t0, time.Hour, idle, input)}, statusexplain.Decision{}, t0)
	rotated := codex
	rotated.Root.SessionID = "sess-2"
	at := t0.Add(time.Second)
	d := Resolve(rotated, []Candidate{herdr("codex", "working", at, at)}, prior, at)
	want(t, "rotated session", d, agentgraph.LegacyWorking, statusexplain.ReasonTerminalAuthority)

	restarted := codex
	restarted.Root.StartedAt = lifetime.Add(time.Minute)
	d = Resolve(restarted, nil, prior, at)
	want(t, "new process lifetime", d, "", statusexplain.ReasonObservationPending)
}

// --- restored presentation has no renewed live authority ---

func TestResolveShouldGiveRestoredPresentationNoLiveAuthorityWhenLoaded(t *testing.T) {
	at := t0.Add(time.Minute)
	red := restored(agentgraph.ProviderPi, t0, 5*time.Minute, active, input)

	alone := Resolve(pi, []Candidate{red}, statusexplain.Decision{}, at)
	want(t, "restored alone", alone, agentgraph.LegacyPermission, statusexplain.ReasonLastKnown)
	if !alone.FreshUntil.Equal(t0.Add(5 * time.Minute)) {
		t.Errorf("restored deadline %s, want the persisted one", alone.FreshUntil)
	}

	live := Resolve(pi, []Candidate{red, herdr("pi", "working", at, at)}, alone, at)
	want(t, "restored red under herdr working", live, agentgraph.LegacyWorking, statusexplain.ReasonTerminalAuthority)
	if got := rejected(live, agentgraph.SourceRestoredLastKnown, agentgraph.LegacyPermission); got != statusexplain.ReasonSourceOutranked {
		t.Errorf("restored rejected for %q", got)
	}

	tail := PiSessionTail(lifetime, observation(agentgraph.ProviderPi, agentgraph.SourcePiSessionFile, at, time.Minute, idle, none))
	d := Resolve(pi, []Candidate{red, tail}, alone, at)
	want(t, "restored under session tail", d, agentgraph.LegacyIdle, statusexplain.ReasonTranscriptAuthority)

	lapsed := Resolve(pi, []Candidate{red}, alone, t0.Add(5*time.Minute))
	want(t, "restored past its persisted deadline", lapsed, "", statusexplain.ReasonObservationExpired)
}

// --- conflicting candidates ---

func TestResolveShouldRankKindsWhenCandidatesConflict(t *testing.T) {
	t1 := t0.Add(time.Second)
	now := t1.Add(time.Millisecond)
	for _, tc := range []struct {
		name       string
		target     Target
		a, b       Candidate // a should win
		loseReason statusexplain.Reason
	}{
		{"pi hook over herdr", pi, piHook(t0, time.Hour, active, none), herdr("pi", "idle", t1, now), statusexplain.ReasonSourceOutranked},
		{"herdr over claude hook", claude, herdr("claude", "idle", t0, now), claudeHook(t1, time.Hour, active, none), statusexplain.ReasonSourceOutranked},
		{"herdr over claude transcript graph", claude, herdr("claude", "working", t0, now), claudeSnap(t1, time.Hour, idle, none), statusexplain.ReasonSourceOutranked},
		{"herdr over codex app-server", codex, herdr("codex", "blocked", t0, now), codexSnap(t1, time.Hour, active, none), statusexplain.ReasonSourceOutranked},
		{"claude graph over newer claude hook", claude, claudeSnap(t0, time.Hour, idle, none), claudeHook(t1, time.Hour, active, none), statusexplain.ReasonSourceOutranked},
		{"claude hook over older same-rank hook", claude, claudeHook(t1, time.Hour, idle, none), claudeHook(t0, time.Hour, active, none), statusexplain.ReasonOlderThanCurrent},
		{"codex newer hook over older app-server", codex, codexHook(t1, time.Hour, idle, none), codexSnap(t0, time.Hour, active, none), statusexplain.ReasonOlderThanCurrent},
		{"codex newer app-server over older hook", codex, codexSnap(t1, time.Hour, idle, none), codexHook(t0, time.Hour, active, none), statusexplain.ReasonOlderThanCurrent},
		{"codex same instant falls back to rank", codex, codexSnap(t1, time.Hour, idle, none), codexHook(t1, time.Hour, active, none), statusexplain.ReasonSourceOutranked},
		{"codex newer rollout idle over older hook working", codex, codexRollout(t1, time.Minute, idle), codexHook(t0, time.Hour, active, none), statusexplain.ReasonOlderThanCurrent},
		{"pi session tail over restored", pi, PiSessionTail(lifetime, observation(agentgraph.ProviderPi, agentgraph.SourcePiSessionFile, t0, time.Hour, idle, none)),
			restored(agentgraph.ProviderPi, t1, time.Hour, active, none), statusexplain.ReasonSourceOutranked},
	} {
		for _, order := range [][]Candidate{{tc.a, tc.b}, {tc.b, tc.a}} {
			d := Resolve(tc.target, order, statusexplain.Decision{}, now)
			if d.Status != tc.a.Status || d.Source != string(tc.a.Source) || !d.ObservedAt.Equal(tc.a.ObservedAt) {
				t.Errorf("%s: decided %q from %s at %s, want %q from %s", tc.name, d.Status, d.Source, d.ObservedAt, tc.a.Status, tc.a.Source)
				continue
			}
			if got := rejected(d, tc.b.Source, tc.b.Status); got != tc.loseReason {
				t.Errorf("%s: loser rejected for %q, want %q", tc.name, got, tc.loseReason)
			}
		}
	}
}

// --- an idle root with live descendants remains delegating; background work ---

func TestResolveShouldStayDelegatingWhenAnIdleRootHasWorkingDescendants(t *testing.T) {
	now := t0.Add(time.Second)
	running := agentgraph.Node{Lifecycle: agentgraph.LifecycleRunning}
	edge := CodexChildHooks(lifetime, observation(agentgraph.ProviderCodex, agentgraph.SourceHook, t0, time.Minute, agentgraph.RuntimeUnknown, none, workingChild))
	for _, tc := range []struct {
		name       string
		target     Target
		candidates []Candidate
		source     agentgraph.SourceKind
	}{
		{"claude graph child under herdr idle", claude, []Candidate{claudeSnap(t0, time.Minute, idle, none, workingChild), herdr("claude", "idle", now, now)},
			agentgraph.SourceClaudeTranscript},
		{"codex running child under herdr done", codex, []Candidate{codexSnap(t0, time.Minute, idle, none, running), herdr("codex", "done", now, now)},
			agentgraph.SourceCodexAppServer},
		{"codex child hook edge under a root hook idle", codex, []Candidate{edge, codexHook(t0, time.Hour, idle, none)}, agentgraph.SourceHook},
	} {
		d := Resolve(tc.target, tc.candidates, statusexplain.Decision{}, now)
		want(t, tc.name, d, agentgraph.LegacyDelegating, statusexplain.ReasonDescendantsLive)
		if d.Source != string(tc.source) {
			t.Errorf("%s: attributed to %s", tc.name, d.Source)
		}
	}
	if d := Resolve(codex, []Candidate{edge}, statusexplain.Decision{}, now); d.Status != "" {
		t.Errorf("a partial edge alone decided %q for the root", d.Status)
	}
}

func TestResolveShouldEndDelegationWhenBackgroundWorkEndsOrItsEvidenceExpires(t *testing.T) {
	edge := CodexChildHooks(lifetime, observation(agentgraph.ProviderCodex, agentgraph.SourceHook, t0, 10*time.Second, agentgraph.RuntimeUnknown, none, workingChild))
	reading := herdr("codex", "idle", t0, t0)
	if d := Resolve(codex, []Candidate{edge, reading}, statusexplain.Decision{}, t0.Add(5*time.Second)); d.Status != agentgraph.LegacyDelegating {
		t.Fatalf("background work decided %q", d.Status)
	}
	expired := Resolve(codex, []Candidate{edge, reading}, statusexplain.Decision{}, t0.Add(10*time.Second))
	want(t, "child evidence expired", expired, agentgraph.LegacyIdle, statusexplain.ReasonTerminalAuthority)

	at := t0.Add(2 * time.Second)
	done := codexSnap(at, time.Minute, idle, none, agentgraph.Node{Lifecycle: agentgraph.LifecycleCompleted})
	ended := Resolve(codex, []Candidate{edge, done, herdr("codex", "idle", t0, at)}, statusexplain.Decision{}, at)
	want(t, "newer snapshot shows the child done", ended, agentgraph.LegacyIdle, statusexplain.ReasonTerminalAuthority)
}

func TestResolveShouldKeepTheRootsOwnStatusWhenItIsNotIdle(t *testing.T) {
	now := t0.Add(time.Second)
	snap := codexSnap(t0, time.Minute, idle, none, workingChild)
	for raw, status := range map[string]string{"working": agentgraph.LegacyWorking, "blocked": agentgraph.LegacyPermission} {
		d := Resolve(codex, []Candidate{snap, herdr("codex", raw, now, now)}, statusexplain.Decision{}, now)
		want(t, "herdr "+raw+" with background work", d, status, statusexplain.ReasonTerminalAuthority)
	}
	// Restored presentation does not borrow live descendants either.
	r := restored(agentgraph.ProviderCodex, t0, time.Minute, idle, none)
	d := Resolve(codex, []Candidate{r, CodexChildHooks(lifetime, observation(agentgraph.ProviderCodex, agentgraph.SourceHook, t0, time.Minute, agentgraph.RuntimeUnknown, none, workingChild))},
		statusexplain.Decision{}, now)
	want(t, "restored idle with a live edge", d, agentgraph.LegacyIdle, statusexplain.ReasonLastKnown)
}

// --- the record ---

func TestResolveShouldReportWhyNothingDecidedWhenNoCandidateSelects(t *testing.T) {
	unbound := Target{Root: statusexplain.Root{PID: 7, StartedAt: lifetime, Provider: "claude"}}
	want(t, "nothing bound", Resolve(unbound, nil, statusexplain.Decision{}, t0), "", statusexplain.ReasonBindingMissing)
	want(t, "bound, nothing yet", Resolve(claude, nil, statusexplain.Decision{}, t0), "", statusexplain.ReasonObservationPending)
}

func TestResolveShouldProduceAWireCleanRecordWhenEveryRuleFires(t *testing.T) {
	now := t0.Add(2 * time.Second)
	candidates := []Candidate{
		codexSnap(t0, time.Minute, idle, input, workingChild),
		codexHook(t0.Add(time.Second), time.Hour, active, none),
		codexRollout(t0.Add(time.Second), time.Minute, idle),
		CodexChildHooks(lifetime, observation(agentgraph.ProviderCodex, agentgraph.SourceHook, t0, time.Minute, agentgraph.RuntimeUnknown, none, workingChild)),
		restored(agentgraph.ProviderCodex, t0, time.Hour, idle, none),
		herdr("codex", "working", t0, now),
		herdr("claude", "idle", t0, now),
		codexSnap(t0.Add(-time.Hour), time.Second, active, none),
	}
	d := Resolve(codex, candidates, statusexplain.Decision{}, now)
	if d.Root != codex.Root || !d.DecidedAt.Equal(now) {
		t.Fatalf("root %+v decided %s", d.Root, d.DecidedAt)
	}
	if len(d.Rejected) != len(candidates)-1 {
		t.Fatalf("rejected %d of %d candidates", len(d.Rejected), len(candidates))
	}
	// Sanitize changes only an empty status, which the wire spells unknown.
	clean := d.Sanitize()
	for i := range d.Rejected {
		if d.Rejected[i].Status == "" {
			d.Rejected[i].Status = statusexplain.StatusUnknown
		}
	}
	if !reflect.DeepEqual(clean, d) {
		t.Fatalf("Sanitize changed the record:\n got %+v\nwant %+v", clean, d)
	}
	for _, c := range d.Rejected {
		if c.RejectReason == "" || !c.RejectReason.Known() || !c.EvidenceKind.Known() {
			t.Errorf("rejected %+v", c)
		}
	}
}

func TestResolveShouldLeaveItsInputsUntouchedWhenCalled(t *testing.T) {
	candidates := []Candidate{codexSnap(t0, time.Minute, idle, input), herdr("codex", "working", t0, t0)}
	before := append([]Candidate(nil), candidates...)
	prior := Resolve(codex, candidates, statusexplain.Decision{}, t0)
	priorBefore := prior
	priorBefore.Rejected = append([]statusexplain.Candidate(nil), prior.Rejected...)
	first := Resolve(codex, candidates, prior, t0.Add(time.Second))
	second := Resolve(codex, candidates, prior, t0.Add(time.Second))
	if !reflect.DeepEqual(candidates, before) || !reflect.DeepEqual(prior, priorBefore) {
		t.Fatal("Resolve changed its inputs")
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("Resolve gave two answers for one input")
	}
}

func TestResolveShouldReadTheDecisionBeneathAnOverlayWhenThePriorCarriesOne(t *testing.T) {
	under := Resolve(codex, []Candidate{codexSnap(t0, time.Minute, idle, input)}, statusexplain.Decision{}, t0)
	overlaid := under
	underlying := under.Choice
	overlaid.Underlying = &underlying
	overlaid.Choice = statusexplain.Choice{Status: "limited", EvidenceKind: statusexplain.EvidenceUsageLimit, Reason: statusexplain.ReasonUsageLimitOverlay}
	at := t0.Add(time.Second)
	d := Resolve(codex, []Candidate{herdr("codex", "working", at, at)}, overlaid, at)
	want(t, "attention beneath a limit overlay", d, agentgraph.LegacyPermission, statusexplain.ReasonAttentionHeld)
}

func TestTargetShouldMatchATerminalReadingByItsTerminalIDWhenThePaneMovedWorkspace(t *testing.T) {
	tracked := Target{Root: claude.Root, PaneID: "w1:p1", TerminalID: "term_1"}
	moved := Herdr(HerdrReading{PaneID: "w2:p4", TerminalID: "term_1", Agent: "claude", Status: "working", Live: true, Since: t0}, t0)
	if !tracked.Matches(moved) {
		t.Fatal("a reading of the tracked terminal under its new pane id did not match")
	}
	d := Resolve(tracked, []Candidate{moved}, statusexplain.Decision{}, t0)
	want(t, "moved pane", d, agentgraph.LegacyWorking, statusexplain.ReasonTerminalAuthority)

	reused := Herdr(HerdrReading{PaneID: "w1:p1", TerminalID: "term_2", Agent: "claude", Status: "working", Live: true, Since: t0}, t0)
	if tracked.Matches(reused) {
		t.Error("a reading of another terminal matched because it reused the tracked pane id")
	}
}

func TestTargetShouldFallBackToThePaneIDWhenEitherSideHasNoTerminalID(t *testing.T) {
	for _, tc := range []struct {
		name            string
		target, reading string // terminal ids
		readingPane     string
		match           bool
	}{
		{"neither side", "", "", "w1:p1", true},
		{"target only", "term_1", "", "w1:p1", true},
		{"reading only", "", "term_1", "w1:p1", true},
		{"neither side, other pane", "", "", "w1:p2", false},
		{"reading only, other pane", "", "term_1", "w1:p2", false},
	} {
		tracked := Target{Root: claude.Root, PaneID: "w1:p1", TerminalID: tc.target}
		c := Herdr(HerdrReading{PaneID: tc.readingPane, TerminalID: tc.reading, Agent: "claude", Status: "idle", Live: true, Since: t0}, t0)
		if got := tracked.Matches(c); got != tc.match {
			t.Errorf("%s: matches = %v, want %v", tc.name, got, tc.match)
		}
	}
}

func TestResolveShouldLetTheHookResolveItsOwnRequestWhenASnapshotOnlyCarriedIt(t *testing.T) {
	// A Codex app-server sample composed with a question the hooks hold open:
	// the request is the hook's, so the hook's own later answer resolves it.
	latched := HookLatched(codexSnap(t0.Add(time.Second), time.Minute, active, input))
	answered := codexHook(t0.Add(2*time.Second), time.Hour, active, none)
	at := t0.Add(3 * time.Second)

	d := Resolve(codex, []Candidate{latched}, statusexplain.Decision{}, at)
	if d.Status != agentgraph.LegacyPermission || d.EvidenceKind != statusexplain.EvidenceHook || d.Source != string(agentgraph.SourceHook) {
		t.Fatalf("latched request decided %q by %s/%s, want permission by the hook", d.Status, d.EvidenceKind, d.Source)
	}
	// The answer resolves both the carried request and the prior decision.
	after := Resolve(codex, []Candidate{latched, answered}, d, at)
	want(t, "hook answers its own request", after, agentgraph.LegacyWorking, statusexplain.ReasonEventAuthority)
	afterPrior := Resolve(codex, []Candidate{answered}, d, at)
	want(t, "hook answers the prior hold", afterPrior, agentgraph.LegacyWorking, statusexplain.ReasonEventAuthority)
}

func TestResolveShouldKeepTheAppServersOwnRequestWhenOnlyAHookSaysOtherwise(t *testing.T) {
	// Coordinator decision (1): a Codex hook does not clear app-server
	// attention; only the next snapshot (or the request's deadline) does.
	asked := codexSnap(t0.Add(time.Second), time.Minute, active, input)
	hook := codexHook(t0.Add(2*time.Second), time.Hour, active, none)
	at := t0.Add(3 * time.Second)
	d := Resolve(codex, []Candidate{asked, hook}, statusexplain.Decision{}, at)
	want(t, "hook over app-server request", d, agentgraph.LegacyPermission, statusexplain.ReasonAttentionHeld)
	held := Resolve(codex, []Candidate{hook}, d, at)
	want(t, "hook over the held request", held, agentgraph.LegacyPermission, statusexplain.ReasonAttentionHeld)
	cleared := Resolve(codex, []Candidate{codexSnap(t0.Add(4*time.Second), time.Minute, active, none), hook}, d, t0.Add(4*time.Second))
	want(t, "next snapshot clears it", cleared, agentgraph.LegacyWorking, statusexplain.ReasonGraphAuthority)
}

func TestResolveIndexShouldNameTheCandidateTheDecisionRestsOnWhenOneDoes(t *testing.T) {
	snap := claudeSnap(t0, time.Minute, idle, none)
	red := claudeHook(t0.Add(time.Second), time.Minute, active, ask)
	reading := herdr("claude", "working", t0, t0)
	at := t0.Add(2 * time.Second)
	for _, tc := range []struct {
		name       string
		candidates []Candidate
		prior      statusexplain.Decision
		want       int
	}{
		{"base", []Candidate{snap}, statusexplain.Decision{}, 0},
		{"terminal", []Candidate{snap, reading}, statusexplain.Decision{}, 1},
		{"attention", []Candidate{snap, reading, red}, statusexplain.Decision{}, 2},
		{"nothing", nil, statusexplain.Decision{}, -1},
	} {
		if _, got := ResolveIndex(claude, tc.candidates, tc.prior, at); got != tc.want {
			t.Errorf("%s: index %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestResolveShouldNotLetSupersededEvidenceDecideWhenTheNewerObservationLapses(t *testing.T) {
	// A Codex SessionStart hook (long lease) superseded by an app-server sample
	// that has since lapsed: the hook does not decide again.
	hook := codexHook(t0, 24*time.Hour, idle, none)
	hook.Superseded = true
	sample := codexSnap(t0.Add(time.Second), time.Second, idle, none, workingChild)
	at := t0.Add(5 * time.Second)
	d := Resolve(codex, []Candidate{hook, sample}, statusexplain.Decision{}, at)
	want(t, "lapsed sample over superseded hook", d, "", statusexplain.ReasonObservationExpired)
	if got := rejected(d, agentgraph.SourceHook, agentgraph.LegacyIdle); got != statusexplain.ReasonOlderThanCurrent {
		t.Errorf("superseded hook rejected for %q, want older_than_current", got)
	}

	// A superseded snapshot still holds its own request open (decision 1).
	asked := codexSnap(t0, time.Minute, active, input)
	asked.Superseded = true
	newer := codexHook(t0.Add(time.Second), time.Hour, active, none)
	d = Resolve(codex, []Candidate{asked, newer}, statusexplain.Decision{}, t0.Add(2*time.Second))
	want(t, "superseded request", d, agentgraph.LegacyPermission, statusexplain.ReasonAttentionHeld)
}
