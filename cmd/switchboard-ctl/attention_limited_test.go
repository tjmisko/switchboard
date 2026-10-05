package main

import (
	"testing"

	"github.com/tjmisko/switchboard/internal/state"
)

func TestNextAttentionTargetShouldSkipALimitedSessionWhenAnIdleOneWaits(t *testing.T) {
	sessions := []state.Session{focusedSess(1, "working"), sess(2, state.StatusLimited), sess(3, "idle")}
	if got := nextAttentionTarget(sessions); got == nil || got.PID != 3 {
		t.Fatalf("attention target = %+v, want pid 3", got)
	}
}

func TestNextAttentionTargetShouldReturnNilWhenEverySessionIsLimited(t *testing.T) {
	sessions := []state.Session{sess(1, state.StatusLimited), sess(2, state.StatusLimited)}
	if got := nextAttentionTarget(sessions); got != nil {
		t.Fatalf("attention target = pid %d, want nil", got.PID)
	}
}

func TestCycleTargetShouldReachALimitedSession(t *testing.T) {
	sessions := []state.Session{focusedSess(1, "working"), sess(2, state.StatusLimited), sess(3, "idle")}
	if target, ok := cycleTargetSession(sessions, "next"); !ok || target.PID != 2 {
		t.Fatalf("cycle next = pid %d (ok=%t), want pid 2", target.PID, ok)
	}
}
