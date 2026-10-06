package agentgraph

import (
	"errors"
	"testing"
)

func TestOutcomeOfShouldClassifyAnObserversAnswerWhenItDoesOrDoesNotStateOne(t *testing.T) {
	graph := Observation{RootID: "root", Nodes: []Node{{ID: "root"}}}
	failed := errors.New("scan failed")
	for _, tc := range []struct {
		name string
		o    Observation
		err  error
		want Outcome
	}{
		{"should be usable when a graph arrives without an outcome", graph, nil, OutcomeUsable},
		{"should be unavailable when nothing arrives without an outcome", Observation{}, nil, OutcomeUnavailable},
		{"should be unavailable when a root id arrives with no nodes", Observation{RootID: "root"}, nil, OutcomeUnavailable},
		{"should be unavailable when an error comes with an unstated graph", graph, failed, OutcomeUnavailable},
		{"should be unavailable when an error comes with a usable claim", withOutcome(graph, OutcomeUsable), failed, OutcomeUnavailable},
		{"should keep unsupported when the observer states it", withOutcome(Observation{}, OutcomeUnsupported), nil, OutcomeUnsupported},
		{"should keep reset when the observer states it with an error", withOutcome(Observation{RootID: "next"}, OutcomeReset), failed, OutcomeReset},
		{"should keep unavailable when the observer carries its prior graph", withOutcome(graph, OutcomeUnavailable), nil, OutcomeUnavailable},
		{"should infer when the stated outcome is unknown", withOutcome(graph, Outcome("bogus")), nil, OutcomeUsable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := OutcomeOf(tc.o, tc.err); got != tc.want {
				t.Fatalf("OutcomeOf = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCloneShouldCarryTheOutcomeWhenAnObservationIsDetached(t *testing.T) {
	o := Observation{RootID: "root", Outcome: OutcomeUnavailable, Nodes: []Node{{ID: "root"}}}
	if got := o.Clone().Outcome; got != OutcomeUnavailable {
		t.Fatalf("Clone().Outcome = %q", got)
	}
}

func withOutcome(o Observation, outcome Outcome) Observation {
	o.Outcome = outcome
	return o
}
