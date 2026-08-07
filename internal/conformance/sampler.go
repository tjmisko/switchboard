package conformance

import (
	"fmt"
	"reflect"
	"testing"
)

// ---------------------------------------------------------------------------
// Seam 4 — the reconcile tick's pre-lock samplers
// ---------------------------------------------------------------------------
//
// Unlike the three seams above, this one is not about portability. It is about a
// rule that the code cannot state for itself:
//
//	The fast path does no I/O. A sampler that has a usable sample reads nothing
//	while the store lock is held.
//
// That rule is load-bearing — store.Apply holds an exclusive lock across its whole
// callback, so a read in there blocks every waybar subscriber, every hook RPC and
// every chip click for as long as it takes — and it was, until this suite, enforced
// by nothing. It was hoisted into place once, and six weeks later a feature put two
// reads back with no failing test, no lint and no type error to say otherwise. The
// contributor did nothing wrong by the code's own lights. That is the defect this
// suite exists to close.
//
// It cannot be closed by types. "The apply phase has no filesystem" is not
// expressible in Go, and it is not even true: the inline fallback is load-bearing
// for correctness, because a sample the session has moved past MUST be re-read
// under the lock or the tick silently drops. A static "does this closure
// transitively call os?" check would trip on exactly the fallback it has to permit.
// So the enforceable invariant is the narrower one above, and the enforcement is a
// test.

// Sampler registers one pre-lock reader of the reconcile tick with the contract
// below. One per function that reads the disk before the lock is taken and stages
// the result for the phase that runs under it.
type Sampler struct {
	// Name is the sampler's own function name (sampleFanout, sampleSignals, …). It
	// names the subtest and prefixes every message, so a failure says which reader
	// went back to the disk.
	Name string

	// Build lays out a fresh fixture and returns the three phases to drive it. It
	// is called several times per contract run and each call must be INDEPENDENT:
	// its own temp dirs, its own store, its own cursor state. The suite compares
	// the answers of separate runs, so a Build that shares durable state across
	// calls (an Observer whose seen-set survives, a sink whose file accumulates)
	// makes the second run's answer differ for reasons that have nothing to do
	// with I/O.
	Build func(t *testing.T) SamplerRun
}

// SamplerRun is one independent run of one sampler: the fixture on disk plus the
// three phases the suite drives it through.
type SamplerRun struct {
	// Sample is the PRE-LOCK read phase — the sampler itself. It runs with no
	// store lock held and stages whatever the apply phase will need.
	Sample func()

	// Detach makes the sources this sampler read stop supplying the answer, and
	// stands in for the thing the suite cannot do directly: prove a function did
	// not touch the disk. For a file-backed sampler that is a rename or a removal;
	// for one behind an interface (the process source) it is swapping the backend
	// for one that answers differently.
	//
	// It must NOT disturb whatever staleness stamp the apply phase re-checks. Every
	// sampler here guards its sample — signalSample.freshFor re-stats the
	// transcript, fanout.Sample.usableFor compares the cursor and generation — and
	// a Detach that trips the guard makes the apply phase fall back to an inline
	// read LEGITIMATELY. The contract would then be asserting that the fallback
	// works, which is true, already covered elsewhere, and not what this is for.
	Detach func()

	// Apply is the UNDER-LOCK phase, and the thing on trial. It must answer from
	// what Sample staged, with no read of its own.
	//
	// The returned value is the observable answer — events recorded, fields
	// assigned, whatever the phase produces — and it is compared across runs with
	// reflect.DeepEqual, so it must be DETERMINISTIC across Builds. Normalize away
	// wall clocks and file mtimes in the registration; the contract is about what
	// was read, never about when.
	Apply func() any
}

// RunSamplerContract asserts that one sampler's apply phase answers from the
// sample alone.
//
// The technique is proof by removal, and it is the one this codebase already used
// ad hoc before it was a contract: take the sample, delete what it read, then run
// the apply phase and require the same answer to come out. A phase that went back
// to the disk finds nothing there and answers differently.
//
// Three assertions, and the two that are not the headline are what keep the
// headline honest:
//
//	precondition — two intact runs agree, so the comparison has a stable baseline;
//	the contract  — a sampled run with its sources detached agrees with an intact one;
//	negative control — an UNSAMPLED run with its sources detached DISAGREES.
//
// Without the negative control the contract passes for a fixture that never fed
// the answer in the first place, which is the failure mode a proof-by-removal test
// degrades into as the code around it moves. A contract that cannot fail is worse
// than no contract.
//
// THE HONEST LIMIT: a new sampler that is never registered here is not caught.
// This makes the contract cheap and conventional, not automatic. What covers the
// unregistered case is the tick budget test in cmd/switchboard — it measures the
// whole Apply against an injected delay and does not care which function spent it.
// The two are complementary and neither replaces the other: the budget test says
// SOMETHING under the lock is slow, this says WHICH reader broke its contract.
func RunSamplerContract(t *testing.T, s Sampler) {
	t.Helper()

	t.Run(s.Name, func(t *testing.T) {
		for _, problem := range samplerContractProblems(t, s) {
			t.Error(problem)
		}
	})
}

// samplerContractProblems drives the phases and returns one message per contract
// violation, rather than reporting them itself.
//
// The split exists so the suite can be run against a deliberately cheating
// sampler and observed to REJECT it (see sampler_test.go). A contract nobody has
// ever watched fail is a contract nobody knows works — and this one is exactly the
// shape that goes quietly vacuous, since it asserts an equality that a sampler
// reading nothing at all would also satisfy.
func samplerContractProblems(t *testing.T, s Sampler) []string {
	t.Helper()

	intact := s.Build(t)
	intact.Sample()
	want := intact.Apply()

	// Precondition. A baseline that is not reproducible makes every comparison
	// below meaningless in both directions, so stop rather than report noise.
	repeat := s.Build(t)
	repeat.Sample()
	if again := repeat.Apply(); !reflect.DeepEqual(again, want) {
		return []string{fmt.Sprintf("two intact runs of %s disagree, so the removal comparisons have no "+
			"stable baseline — normalize the clocks and mtimes out of the answer, or give each Build its "+
			"own fixture:\n  first  = %+v\n  second = %+v", s.Name, want, again)}
	}

	var problems []string

	// The contract: sampled, then detached, must still answer.
	sampled := s.Build(t)
	sampled.Sample()
	sampled.Detach()
	if got := sampled.Apply(); !reflect.DeepEqual(got, want) {
		problems = append(problems, fmt.Sprintf("%s's apply phase answered differently once its sources "+
			"were detached, so it went back to the disk with the store lock held:\n  sampled  = %+v\n  detached = %+v",
			s.Name, want, got))
	}

	// The negative control: unsampled and detached must NOT answer.
	unsampled := s.Build(t)
	unsampled.Detach()
	if got := unsampled.Apply(); reflect.DeepEqual(got, want) {
		problems = append(problems, fmt.Sprintf("%s's apply phase produced the sampled answer with NO "+
			"sample taken and its sources detached, so the fixture never fed that answer and the contract "+
			"above cannot fail:\n  answer = %+v", s.Name, got))
	}
	return problems
}
