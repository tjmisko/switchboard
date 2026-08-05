package state

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// armLockWarn turns the lock-hold warning on for one test and captures it.
func armLockWarn(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	oldWarn, oldOut := lockHoldWarn, lockWarnOut
	lockHoldWarn, lockWarnOut = time.Nanosecond, &buf // every Apply trips
	t.Cleanup(func() { lockHoldWarn, lockWarnOut = oldWarn, oldOut })
	return &buf
}

// applyFromAHelper exists to put a NAMED frame between the test and Apply, which
// is what the warning is supposed to identify.
func applyFromAHelper(s *Store) {
	s.Apply(func(m map[int]*Session) { m[1] = &Session{PID: 1, Agent: "claude"} })
}

// The warning's whole purpose is answering "which Apply?", and the first version
// of this got the stack depth wrong — it named state.(*Store).Apply for every
// caller, which is true, useless, and silent about being useless. A duration with
// a misleading attribution is worse than a duration alone, because it sends the
// audit at the wrong call site.
func TestApplyLockWarningNamesTheCallerNotApplyItself(t *testing.T) {
	buf := armLockWarn(t)
	s := New(filepath.Join(t.TempDir(), "state.json"))

	applyFromAHelper(s)

	got := buf.String()
	if !strings.Contains(got, "applyFromAHelper") {
		t.Errorf("warning must name the function that called Apply; got %q", got)
	}
	if strings.Contains(got, "caller=state.(*Store).Apply") {
		t.Errorf("warning named Apply's own frame — the stack skip is off by one; got %q", got)
	}
}

func TestApplyLockWarningStaysSilentBelowTheThreshold(t *testing.T) {
	var buf bytes.Buffer
	oldWarn, oldOut := lockHoldWarn, lockWarnOut
	lockHoldWarn, lockWarnOut = time.Hour, &buf // nothing should ever trip this
	t.Cleanup(func() { lockHoldWarn, lockWarnOut = oldWarn, oldOut })

	s := New(filepath.Join(t.TempDir(), "state.json"))
	applyFromAHelper(s)

	if buf.Len() != 0 {
		t.Errorf("a fast Apply must not warn; got %q", buf.String())
	}
}

// The zero value is the production default, and it must cost nothing and say
// nothing — the feature is opt-in via SWITCHBOARD_DEBUG_LOCK.
func TestApplyLockWarningIsOffWhenUnset(t *testing.T) {
	var buf bytes.Buffer
	oldWarn, oldOut := lockHoldWarn, lockWarnOut
	lockHoldWarn, lockWarnOut = 0, &buf
	t.Cleanup(func() { lockHoldWarn, lockWarnOut = oldWarn, oldOut })

	s := New(filepath.Join(t.TempDir(), "state.json"))
	applyFromAHelper(s)

	if buf.Len() != 0 {
		t.Errorf("a zero threshold must disable the check entirely; got %q", buf.String())
	}
}
