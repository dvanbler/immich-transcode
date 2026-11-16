package pipeline

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExistsAndMissing(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.mp4")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, err := exists(file); !ok || err != nil {
		t.Errorf("exists(file) = %v, %v", ok, err)
	}
	if ok, err := exists(filepath.Join(dir, "gone.mp4")); ok || err != nil {
		t.Errorf("exists(gone) = %v, %v", ok, err)
	}
	if gone, err := missing(""); !gone || err != nil {
		t.Errorf(`missing("") = %v, %v`, gone, err)
	}
	// A path through a regular file cannot be examined: that is an error,
	// not "missing", so nothing is re-created on the strength of it.
	if ok, err := exists(filepath.Join(file, "child")); ok || err == nil {
		t.Errorf("exists(under a file) = %v, %v; want an error", ok, err)
	}
}
