package projectname

import (
	"os"
	"path/filepath"
	"testing"
)

// benchRoot builds a repo root carrying a .git and returns (root, deepDir),
// where deepDir is three levels below it — a session cwd inside a repo rather
// than at its top. The two shapes bracket the real cost: ProjectRoot stops at
// the first .git it finds, so a session sitting at its repo root costs one stat
// and a session three deep costs four.
func benchRoot(b *testing.B) (string, string) {
	b.Helper()
	root := filepath.Join(b.TempDir(), "proj")
	deep := filepath.Join(root, "internal", "pkg", "sub")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		b.Fatal(err)
	}
	return root, deep
}

// The bar calls ResolveForDir 13x (via Chip) + 3x (via tooltip) per renderSlot,
// ten slot processes deep, per emission. Every call walks the tree twice: once
// in ProjectRoot and once more in ProjectBase, which calls ProjectRoot again.
func BenchmarkResolveForDirAtRepoRoot(b *testing.B) {
	root, _ := benchRoot(b)
	cfg := Config{}
	b.ResetTimer()
	for b.Loop() {
		_ = ResolveForDir(cfg, root, "some-session-name")
	}
}

func BenchmarkResolveForDirThreeDeep(b *testing.B) {
	_, deep := benchRoot(b)
	cfg := Config{}
	b.ResetTimer()
	for b.Loop() {
		_ = ResolveForDir(cfg, deep, "some-session-name")
	}
}

// A cwd that does not exist walks all the way to "/" before giving up, which is
// what the bar's own benchmark snapshot does today.
func BenchmarkResolveForDirMissing(b *testing.B) {
	cfg := Config{}
	b.ResetTimer()
	for b.Loop() {
		_ = ResolveForDir(cfg, "/home/u/Projects/Arachne", "some-session-name")
	}
}
