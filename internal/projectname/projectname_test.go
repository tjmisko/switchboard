package projectname

import (
	"os"
	"path/filepath"
	"testing"
)

// ruleFor resolves a DefaultConfig rule by basename, for the pure prefix tests.
func ruleFor(t *testing.T, base string) ProjectRule {
	t.Helper()
	return DefaultConfig().ruleForRoot("/irrelevant/"+base, base)
}

func TestPrefix_appendsCanonicalWhenUnprefixed(t *testing.T) {
	cases := []struct {
		name        string
		base, input string
		want        string
	}{
		{"switchboard adds sb", "switchboard", "status-fix", "sb-status-fix"},
		{"arachne adds arachne", "Arachne", "assess", "arachne-assess"},
		{"sspi adds sspi", "sspi-data-webapp", "cleanup", "sspi-cleanup"},
		{"fallback sanitizes basename", "My_Cool.Repo", "notes", "my-cool-repo-notes"},
		{"preserves input case", "switchboard", "Foo", "sb-Foo"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ruleFor(t, c.base).Prefix(c.input); got != c.want {
				t.Errorf("Prefix(%q) under %q = %q, want %q", c.input, c.base, got, c.want)
			}
		})
	}
}

func TestPrefix_leavesAlreadyPrefixedNamesAlone(t *testing.T) {
	cases := []struct {
		name        string
		base, input string
	}{
		{"no arachne-arachne double", "Arachne", "arachne-assess"},
		{"canonical sb prefix", "switchboard", "sb-foo"},
		{"alias switch counts", "switchboard", "switch-foo"},
		{"alias switchboard counts", "switchboard", "switchboard-foo"},
		{"bare canonical", "switchboard", "sb"},
		{"sspi alias", "sspi-data-webapp", "sspi-cleanup"},
		{"sspi-data longer alias", "sspi-data-webapp", "sspi-data-cleanup"},
		{"sspi-data-webapp full alias", "sspi-data-webapp", "sspi-data-webapp-x"},
		{"case-insensitive dedup", "switchboard", "SB-Foo"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ruleFor(t, c.base).Prefix(c.input); got != c.input {
				t.Errorf("Prefix(%q) under %q = %q, want unchanged", c.input, c.base, got)
			}
		})
	}
}

func TestPrefix_boundaryAvoidsFalsePositives(t *testing.T) {
	// "arachnophobia-notes" must NOT be treated as already prefixed by "arachne".
	if got := ruleFor(t, "Arachne").Prefix("arachnophobia-notes"); got != "arachne-arachnophobia-notes" {
		t.Errorf("got %q, want arachne-arachnophobia-notes", got)
	}
	// "switchboarding" has no hyphen boundary after "switchboard".
	if got := ruleFor(t, "switchboard").Prefix("switchboarding"); got != "sb-switchboarding" {
		t.Errorf("got %q, want sb-switchboarding", got)
	}
}

func TestPrefix_emptyNameUnchanged(t *testing.T) {
	if got := ruleFor(t, "switchboard").Prefix("   "); got != "" {
		t.Errorf("Prefix(blank) = %q, want empty", got)
	}
}

func TestStripKnownPrefix_stripsLongestAlias(t *testing.T) {
	cases := []struct {
		base, input, want string
	}{
		{"sspi-data-webapp", "sspi-data-cleanup", "cleanup"},
		{"sspi-data-webapp", "sspi-cleanup", "cleanup"},
		{"sspi-data-webapp", "cleanup", "cleanup"},
		{"sspi-data-webapp", "sspi", "sspi"}, // exact alias, no hyphen part to strip
		{"Arachne", "arachne-assess", "assess"},
		{"Arachne", "assess", "assess"},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			if got := ruleFor(t, c.base).StripKnownPrefix(c.input); got != c.want {
				t.Errorf("StripKnownPrefix(%q) under %q = %q, want %q", c.input, c.base, got, c.want)
			}
		})
	}
}

func TestRuleForRoot_pathBeatsBasename(t *testing.T) {
	cfg := Config{Rules: []ProjectRule{
		{MatchPaths: []string{"/home/u/special-switchboard"}, Canonical: "spc", Aliases: []string{"spc"}},
	}}
	cfg.Rules = append(cfg.Rules, DefaultConfig().Rules...)

	// A dir basename "switchboard" would hit the default (sb), but the explicit
	// path override must win for that exact root.
	if got := cfg.ruleForRoot("/home/u/special-switchboard", "special-switchboard").Canonical; got != "spc" {
		t.Errorf("path override Canonical = %q, want spc", got)
	}
	// A different switchboard dir still resolves to the basename default.
	if got := cfg.ruleForRoot("/elsewhere/switchboard", "switchboard").Canonical; got != "sb" {
		t.Errorf("basename default Canonical = %q, want sb", got)
	}
	// Unknown project falls back to its sanitized basename.
	if got := cfg.ruleForRoot("/tmp/Brand New Thing", "Brand New Thing").Canonical; got != "brand-new-thing" {
		t.Errorf("fallback Canonical = %q, want brand-new-thing", got)
	}
}

func TestProjectRoot_walksToGitRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ProjectRoot(nested); got != root {
		t.Errorf("ProjectRoot(nested) = %q, want %q", got, root)
	}
}

func TestProjectRoot_gitFileWorktree(t *testing.T) {
	root := t.TempDir()
	// Worktrees record .git as a file, not a directory.
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /somewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ProjectRoot(root); got != root {
		t.Errorf("ProjectRoot with .git file = %q, want %q", got, root)
	}
}

func TestProjectRoot_noGitReturnsDirItself(t *testing.T) {
	dir := t.TempDir()
	if got := ProjectRoot(dir); got != dir {
		t.Errorf("ProjectRoot(no .git) = %q, want %q", got, dir)
	}
}

func TestSetAbbrevAndLoad_roundTrip(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "projects.json")
	root := "/home/u/projects/sspi-data-webapp"

	if err := setAbbrevIn(cfgPath, root, "Sx"); err != nil {
		t.Fatalf("setAbbrevIn: %v", err)
	}
	cfg := loadFrom(cfgPath)

	// The user override (path-matched, sanitized to "sx") must win over the
	// basename default ("sspi").
	if got := cfg.ruleForRoot(root, "sspi-data-webapp").Canonical; got != "sx" {
		t.Errorf("after SetAbbrev, Canonical = %q, want sx", got)
	}
	// A second write updates in place rather than duplicating.
	if err := setAbbrevIn(cfgPath, root, "sy"); err != nil {
		t.Fatalf("setAbbrevIn 2: %v", err)
	}
	if got := loadFrom(cfgPath).ruleForRoot(root, "sspi-data-webapp").Canonical; got != "sy" {
		t.Errorf("after second SetAbbrev, Canonical = %q, want sy", got)
	}
}

func TestLoad_missingFileFallsBackToDefaults(t *testing.T) {
	cfg := loadFrom(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if got := cfg.ruleForRoot("/x/switchboard", "switchboard").Canonical; got != "sb" {
		t.Errorf("missing-file Canonical = %q, want sb", got)
	}
}

func TestFullForDir_returnsFullElseTitleCasedBasename(t *testing.T) {
	cfg := DefaultConfig()
	cases := []struct {
		name, dir, want string
	}{
		{"seeded switchboard full", "/irrelevant/switchboard", "Switchboard"},
		{"seeded arachne full", "/irrelevant/arachne", "Arachne"},
		{"seeded sspi full", "/irrelevant/sspi-data-webapp", "SSPI Data Webapp"},
		{"unknown title-cases the basename", "/irrelevant/my-cool-repo", "My Cool Repo"},
		{"unknown sanitizes mixed separators", "/irrelevant/Brand_New.thing", "Brand New Thing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FullForDir(cfg, c.dir); got != c.want {
				t.Errorf("FullForDir(%q) = %q, want %q", c.dir, got, c.want)
			}
		})
	}
}

func TestFullForDir_knownRuleWithoutFullFallsBackToTitleCasedBasename(t *testing.T) {
	// A rule with an abbrev but no pretty name should not echo the abbrev — it
	// falls back to a title-cased basename, which reads better on a dashboard.
	cfg := Config{Rules: []ProjectRule{
		{Match: []string{"widget"}, Canonical: "wg", Aliases: []string{"wg"}},
	}}
	if got := FullForDir(cfg, "/irrelevant/widget"); got != "Widget" {
		t.Errorf("FullForDir(no-full rule) = %q, want Widget", got)
	}
}

func TestFullForAbbrev_resolvesKnownElseEchoesAbbrev(t *testing.T) {
	cfg := DefaultConfig()
	cases := []struct {
		name, abbrev, want string
	}{
		{"known sb -> Switchboard", "sb", "Switchboard"},
		{"known arachne -> Arachne", "arachne", "Arachne"},
		{"known sspi -> SSPI Data Webapp", "sspi", "SSPI Data Webapp"},
		{"unknown abbrev echoes itself", "zzz", "zzz"},
		{"empty maps to empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FullForAbbrev(cfg, c.abbrev); got != c.want {
				t.Errorf("FullForAbbrev(%q) = %q, want %q", c.abbrev, got, c.want)
			}
		})
	}
}

func TestFullForAbbrev_knownRuleWithoutFullEchoesAbbrev(t *testing.T) {
	cfg := Config{Rules: []ProjectRule{
		{Match: []string{"widget"}, Canonical: "wg", Aliases: []string{"wg"}},
	}}
	if got := FullForAbbrev(cfg, "wg"); got != "wg" {
		t.Errorf("FullForAbbrev(no-full rule) = %q, want wg", got)
	}
}

func TestSetFullAndLoad_roundTrip(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "projects.json")
	root := "/home/u/projects/widget"

	if err := setFullIn(cfgPath, root, "Widget Factory"); err != nil {
		t.Fatalf("setFullIn: %v", err)
	}
	if got := FullForDir(loadFrom(cfgPath), root); got != "Widget Factory" {
		t.Errorf("after SetFull, FullForDir = %q, want Widget Factory", got)
	}

	// Setting an abbrev afterwards must preserve the pretty name (the two fields
	// are edited independently), and a Full-only entry must never erase the abbrev.
	if err := setAbbrevIn(cfgPath, root, "wf"); err != nil {
		t.Fatalf("setAbbrevIn: %v", err)
	}
	cfg := loadFrom(cfgPath)
	if got := cfg.ruleForRoot(root, "widget").Canonical; got != "wf" {
		t.Errorf("after SetAbbrev, Canonical = %q, want wf", got)
	}
	if got := FullForDir(cfg, root); got != "Widget Factory" {
		t.Errorf("SetAbbrev clobbered Full: FullForDir = %q, want Widget Factory", got)
	}
	// The reverse lookup the timeline uses resolves through the loaded user rule.
	if got := FullForAbbrev(cfg, "wf"); got != "Widget Factory" {
		t.Errorf("FullForAbbrev(wf) = %q, want Widget Factory", got)
	}
}

func TestSetFull_preservesExistingAbbrev(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "projects.json")
	root := "/home/u/projects/widget"

	if err := setAbbrevIn(cfgPath, root, "wf"); err != nil {
		t.Fatalf("setAbbrevIn: %v", err)
	}
	if err := setFullIn(cfgPath, root, "Widget Factory"); err != nil {
		t.Fatalf("setFullIn: %v", err)
	}
	cfg := loadFrom(cfgPath)
	if got := cfg.ruleForRoot(root, "widget").Canonical; got != "wf" {
		t.Errorf("SetFull clobbered abbrev: Canonical = %q, want wf", got)
	}
	if got := FullForDir(cfg, root); got != "Widget Factory" {
		t.Errorf("FullForDir = %q, want Widget Factory", got)
	}
}
