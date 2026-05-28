package testsupport

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Touch creates an empty file at path (creating parent dirs), failing the test
// on error. Used to stand up presence-marker files (e.g. the bottombar's
// master-visibility marker).
func Touch(t testing.TB, path string) {
	t.Helper()
	mkParent(t, path)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("touch %s: %v", path, err)
	}
}

// WriteFile writes content to path (creating parent dirs), failing on error.
func WriteFile(t testing.TB, path, content string) {
	t.Helper()
	mkParent(t, path)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// WritePIDFile writes pid as a decimal string to path, mirroring how the
// bottombar records its child's pid.
func WritePIDFile(t testing.TB, path string, pid int) {
	t.Helper()
	WriteFile(t, path, strconv.Itoa(pid))
}

func mkParent(t testing.TB, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
}
