package atomicio

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	if err := WriteFile(path, []byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte(`{"v":2}`)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != `{"v":2}` {
		t.Fatalf("read back %q, %v", got, err)
	}
	// A failed or interrupted write must never leave *.tmp-* behind on success.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "state.json" {
			t.Errorf("stray file left behind: %s", e.Name())
		}
	}
}

func TestWriteFileCreatesMissingDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "dir", "state.json")
	if err := WriteFile(path, []byte("x")); err != nil {
		t.Fatalf("missing parent dir: %v", err)
	}
}
