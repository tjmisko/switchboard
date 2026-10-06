package statusexplain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
)

var base = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func candidate(i int) Candidate {
	return Candidate{
		Source: string(agentgraph.SourceCodexRollout), EvidenceKind: EvidenceProviderSnapshot,
		Status: "working", ObservedAt: base.Add(time.Duration(i) * time.Second),
		FreshUntil: base.Add(time.Duration(i)*time.Second + time.Minute), RejectReason: ReasonSourceOutranked,
	}
}

func fullDecision() Decision {
	d := Decision{
		Root: Root{PID: 4242, StartedAt: base.Add(-time.Hour), Provider: "codex", SessionID: "019a-thread-id"},
		Choice: Choice{Status: "limited", Source: "codex_rollout", EvidenceKind: EvidenceUsageLimit,
			Reason: ReasonUsageLimitOverlay, ObservedAt: base, FreshUntil: base.Add(time.Hour), DecidedAt: base.Add(time.Second)},
		Underlying: &Choice{Status: "permission", Source: "codex_app_server", EvidenceKind: EvidenceProviderSnapshot,
			Reason: ReasonHerdrYieldAttention, ObservedAt: base, FreshUntil: base.Add(30 * time.Second), DecidedAt: base},
	}
	d.AddRejected(Candidate{Source: "herdr", EvidenceKind: EvidenceTerminal, Status: "working",
		ObservedAt: base.Add(-time.Minute), RejectReason: ReasonHerdrYieldAttention})
	return d
}

func TestAddRejectedShouldKeepTheNewestBoundedCandidatesWhenManyArrive(t *testing.T) {
	var d Decision
	for i := range MaxRejected + 5 {
		d.AddRejected(candidate(i))
	}
	if len(d.Rejected) != MaxRejected {
		t.Fatalf("rejected = %d, want %d", len(d.Rejected), MaxRejected)
	}
	if d.RejectedOmitted != 5 {
		t.Fatalf("omitted = %d, want 5", d.RejectedOmitted)
	}
	if got := d.Rejected[0].ObservedAt; !got.Equal(candidate(5).ObservedAt) {
		t.Fatalf("oldest kept = %v, want the sixth candidate", got)
	}
	if got := d.Rejected[MaxRejected-1].ObservedAt; !got.Equal(candidate(MaxRejected + 4).ObservedAt) {
		t.Fatalf("newest kept = %v, want the last candidate", got)
	}
}

func TestAddRejectedShouldIgnoreARepeatWhenTheDecisionIsUnchanged(t *testing.T) {
	var d Decision
	for range 20 {
		d.AddRejected(candidate(1))
	}
	if len(d.Rejected) != 1 || d.RejectedOmitted != 0 {
		t.Fatalf("rejected = %d omitted = %d, want one candidate", len(d.Rejected), d.RejectedOmitted)
	}
	d.AddRejected(Candidate{})
	if len(d.Rejected) != 1 {
		t.Fatalf("the zero candidate was recorded: %+v", d.Rejected)
	}
}

func TestRejectionsShouldCarryTheOmittedCountWhenMergedIntoADecision(t *testing.T) {
	var r Rejections
	for i := range MaxRejected + 3 {
		r.Add(candidate(i))
	}
	var d Decision
	r.Into(&d)
	if len(d.Rejected) != MaxRejected || d.RejectedOmitted != 3 {
		t.Fatalf("rejected = %d omitted = %d", len(d.Rejected), d.RejectedOmitted)
	}
	r.Reset()
	var empty Decision
	r.Into(&empty)
	if len(empty.Rejected) != 0 || empty.RejectedOmitted != 0 {
		t.Fatalf("reset rejections still carried %+v", empty)
	}
}

func TestSanitizeShouldReplaceAnythingOutsideItsShapeWhenACallerFillsInContent(t *testing.T) {
	prompt := "please delete the prod database"
	d := Decision{
		Root:   Root{PID: 1, Provider: "claude", SessionID: "id with " + prompt},
		Choice: Choice{Status: prompt, Source: "rm -rf /", EvidenceKind: "tool input", Reason: "because I said so"},
		Underlying: &Choice{Status: "working", Source: "hook", EvidenceKind: EvidenceHook,
			Reason: Reason(prompt)},
		Rejected: []Candidate{{Source: "transcript text", EvidenceKind: EvidenceHook, Status: "working",
			RejectReason: Reason(prompt)}},
	}
	got := d.Sanitize()
	out, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{prompt, "rm -rf", "tool input", "because", "transcript text", "id with"} {
		if strings.Contains(string(out), leaked) || strings.Contains(got.Text(), leaked) {
			t.Fatalf("sanitized output still carries %q:\n%s\n%s", leaked, out, got.Text())
		}
	}
	if got.Status != invalidValue || got.Reason != invalidValue || got.Root.SessionID != invalidValue {
		t.Fatalf("sanitized = %+v", got)
	}
	if got.Underlying.Status != "working" || got.Underlying.Reason != invalidValue {
		t.Fatalf("sanitized underlying = %+v", *got.Underlying)
	}
}

func TestSanitizeShouldReadAnEmptyStatusAsUnknownWhenNothingDecided(t *testing.T) {
	d := Decision{Choice: Choice{EvidenceKind: EvidenceNone, Reason: ReasonBindingMissing}}.Sanitize()
	if d.Status != StatusUnknown {
		t.Fatalf("status = %q, want unknown", d.Status)
	}
}

func TestSanitizeShouldKeepRealIdsWhenTheyAreIds(t *testing.T) {
	for _, id := range []string{"019a2b3c-4d5e-7f00-8899-aabbccddeeff", "herdr:t-12", "herdr:/run/user/1000/herdr.sock#w1:p2"} {
		if got := idValue(id); got != id {
			t.Fatalf("idValue(%q) = %q", id, got)
		}
	}
}

func TestTextAndJSONShouldAgreeWhenRenderedFromOneDecision(t *testing.T) {
	d := fullDecision().Sanitize()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var back Decision
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Text() != d.Text() {
		t.Fatalf("text from JSON differs:\n%s\nvs\n%s", back.Text(), d.Text())
	}
	// Every value the JSON carries appears in the text, rendered the same way.
	text := d.Text()
	for _, want := range []string{
		"pid=4242", "provider=codex", "session_id=019a-thread-id",
		"status=limited", "reason=usage_limit_overlay", "evidence_kind=usage_limit",
		"underlying status=permission reason=herdr_yield_attention source=codex_app_server",
		"rejected status=working source=herdr evidence_kind=terminal",
		"reject_reason=herdr_yield_attention",
		"observed_at=" + base.Format(time.RFC3339Nano),
		"fresh_until=" + base.Add(time.Hour).Format(time.RFC3339Nano),
		"decided_at=" + base.Add(time.Second).Format(time.RFC3339Nano),
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text lacks %q:\n%s", want, text)
		}
	}
}

func TestOutputShouldStayBoundedWhenEveryFieldIsAtItsLimit(t *testing.T) {
	long := strings.Repeat("a", maxIDLen)
	d := fullDecision()
	d.Root.SessionID = long
	for i := range 100 {
		c := candidate(i)
		c.Source = strings.Repeat("s", maxEnumLen)
		d.AddRejected(c)
	}
	d = d.Sanitize()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Rejected) != MaxRejected {
		t.Fatalf("rejected = %d", len(d.Rejected))
	}
	const limit = 4096
	if len(raw) > limit || len(d.Text()) > limit {
		t.Fatalf("json = %d bytes, text = %d bytes, want both <= %d", len(raw), len(d.Text()), limit)
	}
}

func TestSanitizeShouldBoundARejectedListWhenACallerBypassesAddRejected(t *testing.T) {
	d := Decision{}
	for i := range 3 * MaxRejected {
		d.Rejected = append(d.Rejected, candidate(i))
	}
	got := d.Sanitize()
	if len(got.Rejected) != MaxRejected || got.RejectedOmitted != 2*MaxRejected {
		t.Fatalf("rejected = %d omitted = %d", len(got.Rejected), got.RejectedOmitted)
	}
}

func TestEvidenceKindOfShouldClassifyEveryGraphSourceWhenOneIsGiven(t *testing.T) {
	cases := map[agentgraph.SourceKind]EvidenceKind{
		agentgraph.SourceCodexAppServer:    EvidenceProviderSnapshot,
		agentgraph.SourceClaudeTranscript:  EvidenceProviderSnapshot,
		agentgraph.SourceCodexRollout:      EvidenceProviderSnapshot,
		agentgraph.SourcePiSessionFile:     EvidenceProviderSnapshot,
		agentgraph.SourceHook:              EvidenceHook,
		agentgraph.SourceHerdr:             EvidenceTerminal,
		agentgraph.SourceRestoredLastKnown: EvidenceRestored,
		agentgraph.SourceUnknown:           EvidenceNone,
	}
	for source, want := range cases {
		if got := EvidenceKindOf(source); got != want {
			t.Errorf("EvidenceKindOf(%q) = %q, want %q", source, got, want)
		}
	}
	for reason := range knownReasons {
		if enumValue(string(reason), "") != string(reason) {
			t.Errorf("reason %q is not a valid wire code", reason)
		}
	}
}
