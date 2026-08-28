package buildinfo

import (
	"runtime/debug"
	"testing"
)

func stubRead(settings map[string]string, ok bool) func() (*debug.BuildInfo, bool) {
	return func() (*debug.BuildInfo, bool) {
		if !ok {
			return nil, false
		}
		build := &debug.BuildInfo{}
		for key, value := range settings {
			build.Settings = append(build.Settings, debug.BuildSetting{Key: key, Value: value})
		}
		return build, true
	}
}

func TestGet_shouldAbbreviateRevisionWhenVCSStampPresent(t *testing.T) {
	got := get(stubRead(map[string]string{
		"vcs.revision": "38127e013f1f8b8a2562e816508d991f1fe98de2",
		"vcs.modified": "false",
	}, true))
	if got.Revision != "38127e0" {
		t.Errorf("Revision = %q, want 38127e0", got.Revision)
	}
	if got.Modified {
		t.Error("Modified = true, want false")
	}
}

func TestGet_shouldReportModifiedWhenToolchainStampsIt(t *testing.T) {
	got := get(stubRead(map[string]string{
		"vcs.revision": "38127e013f1f8b8a2562e816508d991f1fe98de2",
		"vcs.modified": "true",
	}, true))
	if !got.Modified {
		t.Error("Modified = false, want true")
	}
}

func TestGet_shouldLeaveRevisionEmptyWhenBuiltOutsideAVCSTree(t *testing.T) {
	got := get(stubRead(map[string]string{}, true))
	if got.Revision != "" {
		t.Errorf("Revision = %q, want empty", got.Revision)
	}
}

func TestGet_shouldLeaveRevisionEmptyWhenBuildInfoUnavailable(t *testing.T) {
	got := get(stubRead(nil, false))
	if got.Revision != "" {
		t.Errorf("Revision = %q, want empty", got.Revision)
	}
}

func TestGet_shouldNotAbbreviateAnAlreadyShortRevision(t *testing.T) {
	got := get(stubRead(map[string]string{"vcs.revision": "abc123"}, true))
	if got.Revision != "abc123" {
		t.Errorf("Revision = %q, want abc123", got.Revision)
	}
}

func TestGet_shouldCarryInjectedVersionAlongsideTheStamp(t *testing.T) {
	Version = "38127e0-dirty"
	t.Cleanup(func() { Version = "" })

	got := get(stubRead(map[string]string{
		"vcs.revision": "38127e013f1f8b8a2562e816508d991f1fe98de2",
	}, true))
	if got.Version != "38127e0-dirty" {
		t.Errorf("Version = %q, want 38127e0-dirty", got.Version)
	}
	if got.Revision != "38127e0" {
		t.Errorf("Revision = %q, want the stamp to survive injection", got.Revision)
	}
}

func TestInfoString_shouldPreferInjectedVersionWhenPresent(t *testing.T) {
	got := Info{Version: "38127e0-dirty", Revision: "38127e0"}.String()
	if got != "38127e0-dirty" {
		t.Errorf("String() = %q, want the injected version to win", got)
	}
}

func TestInfoString_shouldFallBackToRevisionWhenVersionNotInjected(t *testing.T) {
	got := Info{Revision: "38127e0"}.String()
	if got != "38127e0" {
		t.Errorf("String() = %q, want 38127e0", got)
	}
}

// A clean linked git worktree is stamped Modified=true by the Go toolchain.
// Appending "-dirty" here would mark every worktree build dirty and make the
// suffix meaningless, so String must ignore Modified entirely.
func TestInfoString_shouldNotMarkDirtyWhenOnlyTheUnreliableStampSaysModified(t *testing.T) {
	got := Info{Revision: "38127e0", Modified: true}.String()
	if got != "38127e0" {
		t.Errorf("String() = %q, want 38127e0 with no dirty suffix", got)
	}
}

func TestInfoString_shouldReportUnknownWhenNothingIdentifiesTheBuild(t *testing.T) {
	got := Info{}.String()
	if got != "unknown" {
		t.Errorf("String() = %q, want unknown", got)
	}
}

// The toolchain stamps VCS information into `go build` and `go install`
// output only, never into a test binary, so Get() is legitimately "unknown"
// here. That a real command reports its revision is asserted end-to-end in
// cmd/switchboard-ctl, which builds an actual binary and runs it.
func TestGet_shouldReportUnknownFromAnUnstampedTestBinary(t *testing.T) {
	if got := Get(); got.String() != "unknown" {
		t.Logf("test binary carries a stamp (%q); harmless, but unexpected", got)
	}
}
