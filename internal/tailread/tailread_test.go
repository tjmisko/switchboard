package tailread

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tail.jsonl")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLinesShouldReturnTheWholeFileWhenItFitsTheWindow(t *testing.T) {
	data, complete, err := Lines(write(t, "a\nb\n"), 64)
	if err != nil || !complete || string(data) != "a\nb\n" {
		t.Fatalf("Lines = %q, %t, %v", data, complete, err)
	}
}

func TestLinesShouldDropThePartialFirstLineWhenTheWindowStartsMidFile(t *testing.T) {
	data, complete, err := Lines(write(t, strings.Repeat("x", 100)+"\nlast\n"), 8)
	if err != nil || !complete || string(data) != "last\n" {
		t.Fatalf("Lines = %q, %t, %v", data, complete, err)
	}
}

func TestLinesShouldReportIncompleteWhenTheNewestRowIsStillBeingWritten(t *testing.T) {
	data, complete, err := Lines(write(t, "a\n{\"half"), 64)
	if err != nil || complete || string(data) != "a\n{\"half" {
		t.Fatalf("Lines = %q, %t, %v", data, complete, err)
	}
}

func TestLinesShouldReturnNothingWhenOneLineOverflowsTheWindow(t *testing.T) {
	data, complete, err := Lines(write(t, "a\n"+strings.Repeat("x", 100)), 8)
	if err != nil || !complete || len(data) != 0 {
		t.Fatalf("Lines = %q, %t, %v", data, complete, err)
	}
}

func TestLinesShouldFailWhenTheFileIsMissing(t *testing.T) {
	if _, _, err := Lines(filepath.Join(t.TempDir(), "absent"), 8); err == nil {
		t.Fatal("Lines on a missing file returned no error")
	}
}
