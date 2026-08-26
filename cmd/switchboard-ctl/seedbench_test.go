package main

import (
	"testing"

	"github.com/tjmisko/switchboard/internal/history"
)

// The line-admission behavior seed-bench depends on (Python-spaced repair
// lines, content mentions of event types) is pinned where the fold lives:
// internal/history's seed tests. What is left to hold here is the bench's own
// target selection.
func TestSeedBenchOrderIsBusiestFirstAndDeterministic(t *testing.T) {
	index := history.SeedIndex{
		"light": {Spawned: map[string]bool{"a": true}, Stopped: map[string]bool{},
			WorkflowStarted: map[string]bool{}, WorkflowStopped: map[string]bool{}},
		"heavy": {Spawned: map[string]bool{"a": true, "b": true}, Stopped: map[string]bool{"a": true},
			WorkflowStarted: map[string]bool{"wf": true}, WorkflowStopped: map[string]bool{}},
		"also-light": {Spawned: map[string]bool{"z": true}, Stopped: map[string]bool{},
			WorkflowStarted: map[string]bool{}, WorkflowStopped: map[string]bool{}},
	}
	got := seedBenchOrder(index)
	if len(got) != 3 || got[0] != "heavy" {
		t.Fatalf("order = %v, want heavy first", got)
	}
	if got[1] != "also-light" || got[2] != "light" {
		t.Errorf("ties must break by id for run-to-run stability, got %v", got)
	}
}
