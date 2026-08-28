package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// versionLDFlag is the exact linker path scripts/deploy uses to stamp a build.
// It is spelled out here, rather than derived, so that renaming the package or
// the variable fails this test instead of silently turning every deploy's
// version stamp into a no-op.
const versionLDFlag = "-X github.com/tjmisko/switchboard/internal/buildinfo.Version="

func buildCtl(t *testing.T, ldflags string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds a binary with the Go toolchain")
	}
	bin := filepath.Join(t.TempDir(), "switchboard-ctl")
	args := []string{"build", "-o", bin}
	if ldflags != "" {
		args = append(args, "-ldflags", ldflags)
	}
	args = append(args, ".")
	cmd := exec.Command("go", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func runVersion(t *testing.T, bin string) string {
	t.Helper()
	out, err := exec.Command(bin, "-version").CombinedOutput()
	if err != nil {
		t.Fatalf("%s -version: %v\n%s", bin, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A deploy asserts that the process it just restarted reports the revision it
// intended to ship. That assert has nothing to read unless a plain `go build`
// binary can already name its own source tree.
func TestVersionFlag_shouldReportTheSourceRevisionWhenBuiltNormally(t *testing.T) {
	got := runVersion(t, buildCtl(t, ""))
	if got == "" || got == "unknown" {
		t.Fatalf("-version printed %q; a toolchain build must carry its VCS stamp", got)
	}
}

// The deploy script's whole guarantee rests on this injection path working.
func TestVersionFlag_shouldReportTheInjectedVersionWhenStamped(t *testing.T) {
	const want = "test-stamp-38127e0"
	got := runVersion(t, buildCtl(t, versionLDFlag+want))
	if got != want {
		t.Fatalf("-version printed %q, want %q; the -X path in scripts/deploy no longer resolves", got, want)
	}
}

// -version must answer without a daemon: it runs against a staged binary
// before anything has been restarted, and before the socket exists.
func TestVersionFlag_shouldNotRequireARunningDaemon(t *testing.T) {
	bin := buildCtl(t, "")
	out, err := exec.Command(bin, "-socket", filepath.Join(t.TempDir(), "absent.sock"), "-version").CombinedOutput()
	if err != nil {
		t.Fatalf("-version dialled the daemon or failed: %v\n%s", err, out)
	}
}
