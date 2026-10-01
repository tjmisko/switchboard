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

// sandboxDeploy runs scripts/deploy against a throwaway HOME, deploy root and
// link directories. A fake systemctl keeps it off the real user manager, and
// the real Go caches keep the build from starting cold.
type sandboxDeploy struct {
	home, root, binDir, moduleDir string
}

func newSandboxDeploy(t *testing.T) sandboxDeploy {
	t.Helper()
	if testing.Short() {
		t.Skip("builds a full release")
	}
	home := t.TempDir()
	fakeBin := filepath.Join(home, "fake-bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeBin, "systemctl"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return sandboxDeploy{
		home:      home,
		root:      filepath.Join(home, "deploy-root"),
		binDir:    filepath.Join(home, "bin"),
		moduleDir: filepath.Join(home, "lib"),
	}
}

func goEnv(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command("go", "env", name).Output()
	if err != nil {
		t.Fatalf("go env %s: %v", name, err)
	}
	return strings.TrimSpace(string(out))
}

func (s sandboxDeploy) run(t *testing.T, moduleMode string) (string, error) {
	t.Helper()
	repoDir, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "scripts/deploy", "--allow-dirty", "--no-restart")
	cmd.Dir = repoDir
	cmd.Env = append(os.Environ(),
		"HOME="+s.home,
		"GOCACHE="+goEnv(t, "GOCACHE"),
		"GOMODCACHE="+goEnv(t, "GOMODCACHE"),
		"GOPATH="+goEnv(t, "GOPATH"),
		"SWITCHBOARD_DEPLOY_ROOT="+s.root,
		"SWITCHBOARD_LINK_DIR="+s.binDir,
		"SWITCHBOARD_MODULE_DIR="+s.moduleDir,
		"SWITCHBOARD_WAYBAR_MODULE="+moduleMode,
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// The module needs a C toolchain, GLib/GIO headers and the GTK 3 runtime that
// Waybar itself loads. A host without them cannot build or load it.
func requireWaybarModuleToolchain(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"cc", "pkg-config", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	if err := exec.Command("pkg-config", "--exists", "gio-2.0").Run(); err != nil {
		t.Skip("gio-2.0 development files not installed")
	}
	if err := exec.Command("python3", "-c", "import ctypes; ctypes.CDLL('libgtk-3.so.0')").Run(); err != nil {
		t.Skip("GTK 3 runtime not installed")
	}
}

// Waybar's module_path must survive release pruning, so the module ships in
// every release and is published through `current` like the commands.
func TestDeploy_shouldShipTheWaybarModuleBehindAStableLinkWhenWaybarIsWanted(t *testing.T) {
	requireWaybarModuleToolchain(t)
	sandbox := newSandboxDeploy(t)
	out, err := sandbox.run(t, "1")
	if err != nil {
		t.Fatalf("deploy failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(sandbox.root, "current", "libswitchboard-waybar.so")); err != nil {
		t.Fatalf("current release has no module: %v\n%s", err, out)
	}
	link := filepath.Join(sandbox.moduleDir, "libswitchboard-waybar.so")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("module link not published: %v\n%s", err, out)
	}
	if want := filepath.Join(sandbox.root, "current", "libswitchboard-waybar.so"); target != want {
		t.Errorf("module link points at %s, want %s (through current, never a release)", target, want)
	}
	if !strings.Contains(out, "restart the top Waybar") {
		t.Errorf("deploy did not say a Waybar restart loads the new module:\n%s", out)
	}
}

func TestDeploy_shouldSkipTheWaybarModuleWhenDisabled(t *testing.T) {
	sandbox := newSandboxDeploy(t)
	out, err := sandbox.run(t, "0")
	if err != nil {
		t.Fatalf("deploy failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(sandbox.root, "current", "libswitchboard-waybar.so")); !os.IsNotExist(err) {
		t.Errorf("module built although SWITCHBOARD_WAYBAR_MODULE=0 (stat err: %v)", err)
	}
	if _, err := os.Lstat(filepath.Join(sandbox.moduleDir, "libswitchboard-waybar.so")); !os.IsNotExist(err) {
		t.Errorf("module link published although SWITCHBOARD_WAYBAR_MODULE=0 (lstat err: %v)", err)
	}
}

func TestDeploy_shouldRejectAnUnknownWaybarModuleMode(t *testing.T) {
	sandbox := newSandboxDeploy(t)
	out, err := sandbox.run(t, "yes")
	if err == nil {
		t.Fatalf("deploy accepted SWITCHBOARD_WAYBAR_MODULE=yes:\n%s", out)
	}
	if !strings.Contains(out, "must be auto, 1 or 0") {
		t.Errorf("deploy failed without naming the valid modes:\n%s", out)
	}
}

// A real file at the link path is a copy that would never follow a deploy.
func TestDeploy_shouldRefuseToReplaceACopiedModule(t *testing.T) {
	sandbox := newSandboxDeploy(t)
	if err := os.MkdirAll(sandbox.moduleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(sandbox.moduleDir, "libswitchboard-waybar.so")
	if err := os.WriteFile(copied, []byte("stale copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := sandbox.run(t, "1")
	if err == nil {
		t.Fatalf("deploy replaced a copied module:\n%s", out)
	}
	if b, _ := os.ReadFile(copied); string(b) != "stale copy" {
		t.Error("deploy overwrote the copied module instead of refusing")
	}
}

// Stow and similar tools make ~/.config files symlinks into a dotfiles repo; a
// release path hiding behind one is what broke the circles view.
func TestDeploy_shouldWarnAboutReleasePathsInSymlinkedConfig(t *testing.T) {
	sandbox := newSandboxDeploy(t)
	dotfiles := filepath.Join(sandbox.home, "dotfiles")
	if err := os.MkdirAll(dotfiles, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := `"module_path": "` + filepath.Join(sandbox.root, "releases", "4b09ce7", "libswitchboard-waybar.so") + `"`
	if err := os.WriteFile(filepath.Join(dotfiles, "config.jsonc"), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	waybarDir := filepath.Join(sandbox.home, ".config", "waybar")
	if err := os.MkdirAll(waybarDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dotfiles, "config.jsonc"), filepath.Join(waybarDir, "config.jsonc")); err != nil {
		t.Fatal(err)
	}
	out, err := sandbox.run(t, "0")
	if err != nil {
		t.Fatalf("deploy failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "name a release directory") || !strings.Contains(out, ".config/waybar/config.jsonc") {
		t.Errorf("deploy did not flag the symlinked config naming a release:\n%s", out)
	}
}

// A dry run builds nothing, so it must not claim the release lacks a module.
func TestDeploy_shouldNotReportAMissingModuleOnADryRun(t *testing.T) {
	sandbox := newSandboxDeploy(t)
	repoDir, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "scripts/deploy", "--dry-run", "--allow-dirty")
	cmd.Dir = repoDir
	cmd.Env = append(os.Environ(),
		"HOME="+sandbox.home,
		"SWITCHBOARD_DEPLOY_ROOT="+sandbox.root,
		"SWITCHBOARD_LINK_DIR="+sandbox.binDir,
		"SWITCHBOARD_MODULE_DIR="+sandbox.moduleDir,
		"SWITCHBOARD_WAYBAR_MODULE=1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dry run failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "would build: libswitchboard-waybar.so") {
		t.Errorf("dry run does not say it would build the module:\n%s", out)
	}
	if strings.Contains(string(out), "has no libswitchboard-waybar.so") {
		t.Errorf("dry run reported a missing module it never tried to build:\n%s", out)
	}
}

// deploy stamps the module with its own release version so the smoke test can
// prove the staged module is the one this deploy built.
func TestBuildWaybarCircles_shouldStampTheRevisionItIsGiven(t *testing.T) {
	requireWaybarModuleToolchain(t)
	module := filepath.Join(t.TempDir(), "libswitchboard-waybar.so")
	build := exec.Command("python3", "build-waybar-circles", module)
	build.Env = append(os.Environ(), "SWITCHBOARD_DISPLAY_REVISION=abc1234-dirty-20260930000000")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	read := exec.Command("python3", "-c", `import ctypes, sys
lib = ctypes.CDLL(sys.argv[1])
print(ctypes.string_at(ctypes.addressof(ctypes.c_char.in_dll(lib, "switchboard_display_revision"))).decode())`, module)
	out, err := read.Output()
	if err != nil {
		t.Fatalf("load module: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "abc1234-dirty-20260930000000" {
		t.Errorf("module reports %q, want the given revision", got)
	}
}
