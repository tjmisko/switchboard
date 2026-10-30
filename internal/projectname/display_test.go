package projectname

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfiguredProjectParents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, parent := range []string{"Projects", "Tools"} {
		root := filepath.Join(home, parent, "中国地理")
		nested := filepath.Join(root, "src", "nested-repo")
		if err := os.MkdirAll(filepath.Join(nested, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		cfg := DefaultConfig()
		if got := cfg.ProjectRoot(nested); got != root {
			t.Errorf("ProjectRoot = %q, want %q", got, root)
		}
		if got := ResolveForDir(cfg, nested, "map-update"); got != "中国地理-map-update" {
			t.Errorf("ResolveForDir = %q", got)
		}
		cfg.ProjectDirs = []string{}
		if got := cfg.ProjectRoot(nested); got != nested {
			t.Errorf("disabled parents: got %q, want nearest git root %q", got, nested)
		}
	}
}

func TestProjectParentConfigSurvivesRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projects.json")
	for _, parents := range []string{`["~/Work"]`, `[]`} {
		if err := os.WriteFile(path, []byte(`{"project_dirs":`+parents+`,"projects":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := setAbbrevIn(path, "/work/MyProject", "mp"); err != nil {
			t.Fatal(err)
		}
		cfg := loadFrom(path)
		if cfg.ProjectDirs == nil {
			t.Fatal("explicit parent configuration was lost")
		}
		if parents == `[]` && len(cfg.ProjectDirs) != 0 {
			t.Fatal("empty parent list no longer disables defaults")
		}
		if parents != `[]` && (len(cfg.ProjectDirs) != 1 || cfg.ProjectDirs[0] != "~/Work") {
			t.Fatalf("parents = %v", cfg.ProjectDirs)
		}
	}
}

func TestCustomProjectParentsAreSpecificAndRespectPathBoundaries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := Config{ProjectDirs: []string{"~/Work", "~/Work/group"}}
	for _, tc := range []struct{ dir, root string }{
		{"Work/MyProject/src", "Work/MyProject"},
		{"Work/group/MyProject/src", "Work/group/MyProject"},
		{"Work-extra/MyProject/src", "Work-extra/MyProject/src"},
	} {
		dir, want := filepath.Join(home, tc.dir), filepath.Join(home, tc.root)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		requireNoAncestorGit(t, dir)
		if got := cfg.ProjectRoot(dir); got != want {
			t.Errorf("ProjectRoot(%q) = %q, want %q", dir, got, want)
		}
	}
}

func TestTaskTitle(t *testing.T) {
	rule := DefaultConfig().RuleForBase("switchboard")
	for _, tc := range []struct{ in, want string }{
		{"sb-fix_unicode__project-names in all providers", "Fix Unicode Project Names In"},
		{"Switchboard Fix Unicode", "Fix Unicode"},
		{"switchboard", ""},
		{"switchboarding-is-different", "Switchboarding Is Different"},
		{"修复-中国地理-Cafe\u0301", "修复 中国地理 Cafe\u0301"},
		{"---", ""},
	} {
		if got := rule.TaskTitle(tc.in); got != tc.want {
			t.Errorf("TaskTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := DefaultConfig().RuleForBase("MyProject").DisplayName(); got != "MyProject" {
		t.Errorf("directory spelling lost: %q", got)
	}
}
