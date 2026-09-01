// Package scripts_test pins the couplings between scripts/deploy and the Go
// source it builds. The script is shell, so nothing else checks that the
// command list it deploys and the linker symbol it stamps still refer to
// things that exist.
package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readDeploy(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("deploy")
	if err != nil {
		t.Fatalf("read scripts/deploy: %v", err)
	}
	return string(b)
}

func deployCommands(t *testing.T) []string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^commands=\(([^)]*)\)`).FindStringSubmatch(readDeploy(t))
	if m == nil {
		t.Fatal("scripts/deploy no longer declares commands=(...)")
	}
	return strings.Fields(m[1])
}

// A command named here but absent from cmd/ makes the deploy fail late, in the
// middle of a build, after the guards have already passed.
func TestDeploy_shouldOnlyDeployCommandsThatExist(t *testing.T) {
	commands := deployCommands(t)
	if len(commands) == 0 {
		t.Fatal("scripts/deploy deploys nothing")
	}
	for _, cmd := range commands {
		if _, err := os.Stat("../cmd/" + cmd); err != nil {
			t.Errorf("scripts/deploy deploys %q, but cmd/%s does not exist", cmd, cmd)
		}
	}
}

// The unit that renders the bar and the daemon must both be deployed, or a
// flip moves only half the pair — the skew cmd/switchboard-ctl/bottombar.go
// carries a legacy fallback for.
func TestDeploy_shouldDeployTheDaemonAndItsClientTogether(t *testing.T) {
	commands := strings.Join(deployCommands(t), " ")
	for _, required := range []string{"switchboard", "switchboard-ctl"} {
		if !strings.Contains(commands+" ", required+" ") {
			t.Errorf("scripts/deploy does not deploy %q", required)
		}
	}
}

// If the linker path drifts from the real symbol, -X silently stamps nothing
// and every release reports its fallback VCS revision instead of the version
// the deploy intended — which the smoke test would then reject on every run.
func TestDeploy_shouldStampTheSymbolThatBuildinfoActuallyDeclares(t *testing.T) {
	const symbol = "github.com/tjmisko/switchboard/internal/buildinfo.Version"
	if !strings.Contains(readDeploy(t), symbol) {
		t.Fatalf("scripts/deploy no longer stamps %s", symbol)
	}
	src, err := os.ReadFile("../internal/buildinfo/buildinfo.go")
	if err != nil {
		t.Fatalf("read buildinfo: %v", err)
	}
	if !regexp.MustCompile(`(?m)^var Version string`).Match(src) {
		t.Error("internal/buildinfo no longer declares `var Version string`; the -X path in scripts/deploy resolves to nothing")
	}
}

// Desktop configuration is meant to reference the published links, so the
// script must keep publishing them and must keep letting a host opt out.
func TestDeploy_shouldPublishStableCommandLinksWithAnOverride(t *testing.T) {
	text := readDeploy(t)
	if !strings.Contains(text, "SWITCHBOARD_LINK_DIR") {
		t.Error("scripts/deploy no longer honours SWITCHBOARD_LINK_DIR")
	}
	if !strings.Contains(text, "install_command_links") {
		t.Error("scripts/deploy no longer publishes stable command links")
	}
}

// Deployment cleanup is recursive by necessity, but it must never use the
// force-recursive spelling forbidden by the repository's standing safety
// policy. The script validates its exact stage/release child before `rm -r`.
func TestDeploy_shouldNeverForceRecursivelyRemoveAPath(t *testing.T) {
	if strings.Contains(readDeploy(t), "rm -rf") {
		t.Fatal("scripts/deploy must not invoke rm -rf")
	}
}

// An interactive CDPATH makes bash's `cd` echo the directory it resolved, and
// makes a relative operand resolve against a foreign tree first. The script
// derives script_dir and repo_dir with $(cd … && pwd), so an inherited CDPATH
// took the deploy down with `cd: $'…/scripts\n…/scripts/..': No such file or
// directory` — and would have pointed it at the decoy tree had that tree been
// a real checkout. The script must resolve itself from the filesystem alone.
func TestDeploy_shouldResolveItsOwnTreeWhenCDPATHIsSet(t *testing.T) {
	repoDir, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	// A decoy that contains a `scripts` entry is what an interactive CDPATH
	// looks like when it points at a directory of checkouts.
	decoy := t.TempDir()
	if err := os.Mkdir(filepath.Join(decoy, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	deployRoot := t.TempDir()

	// Invoked by relative path from the repo root, so `dirname` yields the bare
	// `scripts` operand that the CDPATH lookup hijacks.
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("bash", append([]string{"scripts/deploy"}, args...)...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(),
			"CDPATH="+decoy,
			"SWITCHBOARD_DEPLOY_ROOT="+deployRoot,
			"SWITCHBOARD_LINK_DIR=",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("scripts/deploy %v under CDPATH: %v\n%s", args, err, out)
		}
		return string(out)
	}

	if got := run("--status"); !strings.Contains(got, "root:     "+deployRoot) {
		t.Errorf("--status did not report the configured root:\n%s", got)
	}
	// --dry-run is the only mode that names the tree it would build from, which
	// is the assertion that catches a silent misresolve rather than a crash.
	if got := run("--dry-run", "--allow-dirty"); !strings.Contains(got, "repo:     "+repoDir+"\n") {
		t.Errorf("--dry-run resolved a tree other than %s:\n%s", repoDir, got)
	}
}
