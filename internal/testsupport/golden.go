package testsupport

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// updateGolden reports whether golden files should be rewritten this run.
// Driven by the UPDATE_GOLDEN env var so it is opt-in and never trips CI.
func updateGolden() bool { return os.Getenv("UPDATE_GOLDEN") != "" }

// Golden reads the golden file at path, failing the test if it is missing
// (with a hint to regenerate). Use for fixtures that are also program input.
func Golden(t testing.TB, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with UPDATE_GOLDEN=1 to create)", path, err)
	}
	return b
}

// AssertGolden compares got against the golden file at path. With UPDATE_GOLDEN
// set it (re)writes the file and returns; otherwise it fails on any byte
// difference, printing both sides. The parent directory is created on update.
func AssertGolden(t testing.TB, path string, got []byte) {
	t.Helper()
	if updateGolden() {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for golden %s: %v", path, err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want := Golden(t, path)
	if !bytes.Equal(got, want) {
		t.Errorf("golden mismatch for %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}
