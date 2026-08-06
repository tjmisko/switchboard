package conformance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// toySampler builds a Sampler over one file on disk, with the apply phase's
// behaviour parameterized. It stands in for the real thing: a fixture that says
// something, a pre-lock read, and an under-lock phase that either honours the
// sample or cheats.
//
// The three shapes below are the three outcomes the contract has to tell apart,
// and they exist because a proof-by-removal test can fail in two directions —
// it can miss a reader that cheats, and it can pass for a fixture that never fed
// the answer at all.
func toySampler(name string, apply func(sampled string, path string) any) Sampler {
	return Sampler{
		Name: name,
		Build: func(t *testing.T) SamplerRun {
			dir := t.TempDir()
			path := filepath.Join(dir, "fixture")
			if err := os.WriteFile(path, []byte("answer"), 0o644); err != nil {
				t.Fatal(err)
			}
			var sampled string
			return SamplerRun{
				Sample: func() {
					b, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					sampled = string(b)
				},
				Detach: func() {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				},
				Apply: func() any { return apply(sampled, path) },
			}
		},
	}
}

// readFileOrEmpty is what a cheating apply phase does: go back to the disk.
func readFileOrEmpty(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func TestShouldAcceptASamplerWhoseApplyPhaseAnswersFromTheSample(t *testing.T) {
	honest := toySampler("honest", func(sampled, _ string) any { return sampled })

	if problems := samplerContractProblems(t, honest); len(problems) != 0 {
		t.Errorf("a sampler that answers purely from its sample must pass the contract; got %v", problems)
	}
}

func TestShouldRejectASamplerWhoseApplyPhaseReadsTheDiskAgain(t *testing.T) {
	cheat := toySampler("cheat", func(_, path string) any { return readFileOrEmpty(path) })

	problems := samplerContractProblems(t, cheat)
	if len(problems) == 0 {
		t.Fatal("the contract accepted an apply phase that re-reads the file it was handed a sample of; " +
			"it cannot detect the defect it exists for")
	}
	if !strings.Contains(problems[0], "went back to the disk") {
		t.Errorf("the rejection must name the defect; got %q", problems[0])
	}
}

// The vacuity case, and the reason the negative control is not optional: an apply
// phase that ignores the fixture entirely satisfies the equality assertion
// perfectly. A registration that drifts into this shape — the fixture stops
// mattering, the answer becomes a constant — would otherwise keep passing forever
// while proving nothing.
func TestShouldRejectASamplerWhoseApplyPhaseNeverConsultsTheSample(t *testing.T) {
	blind := toySampler("blind", func(_, _ string) any { return "constant" })

	problems := samplerContractProblems(t, blind)
	if len(problems) == 0 {
		t.Fatal("the contract accepted an apply phase whose answer does not depend on the fixture at all, " +
			"so its equality assertion can never fail")
	}
	if !strings.Contains(problems[0], "cannot fail") {
		t.Errorf("the rejection must name the vacuity; got %q", problems[0])
	}
}

// A baseline that does not reproduce makes both comparisons meaningless, and the
// symptom — an intermittent "went back to the disk" on a sampler that did not —
// sends the reader hunting a bug that is in the fixture. Diagnose it directly.
func TestShouldRejectAFixtureWhoseAnswerIsNotReproducible(t *testing.T) {
	calls := 0
	drifting := toySampler("drifting", func(sampled, _ string) any {
		calls++
		return sampled + string(rune('0'+calls))
	})

	problems := samplerContractProblems(t, drifting)
	if len(problems) != 1 || !strings.Contains(problems[0], "no stable baseline") {
		t.Fatalf("a non-reproducible fixture must be reported as such, once, before anything else is "+
			"compared; got %v", problems)
	}
}
