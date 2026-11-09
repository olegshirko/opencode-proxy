package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanupStaleTempFiles(t *testing.T) {
	dir := t.TempDir()
	stale := func(name string, mtime time.Time) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := time.Now().Add(-2 * time.Hour)

	stale("session-cost.json.tmp-111", old)
	stale("usage.json.tmp-222", old)
	stale("not-a-temp.json", old)
	stale("session-cost.json.tmp-fresh", time.Now())

	CleanupStaleTempFiles(dir, time.Hour)

	for _, name := range []string{"session-cost.json.tmp-111", "usage.json.tmp-222"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("stale %s survived cleanup", name)
		}
	}
	for _, name := range []string{"session-cost.json.tmp-fresh", "not-a-temp.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was removed; only stale *.tmp-* go", name)
		}
	}
}

// loadRegistryIfChanged feeds the mtime ticker: a regenerated models.json must
// produce a fresh registry exactly once per change.
func TestLoadRegistryIfChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	write := func(data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		// Give the file a distinctly new mtime even on coarse filesystems.
		future := time.Now().Add(2 * time.Second)
		if err := os.Chtimes(path, future, future); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"models":[{"id":"glm-5.3","picker":"claude-GL-5.3","available":true}]}`)

	reg, changed, err := loadRegistryIfChanged(path, time.Time{})
	if err != nil || !changed || reg == nil {
		t.Fatalf("first load: changed=%v err=%v, want a fresh registry", changed, err)
	}
	mtime := regModTime(path)

	if _, changed, err := loadRegistryIfChanged(path, mtime); changed || err != nil {
		t.Fatalf("unchanged file: changed=%v err=%v, want nothing to do", changed, err)
	}

	write(`{"models":[{"id":"brand-new","picker":"claude-Brand-New","available":true}]}`)
	reg, changed, err = loadRegistryIfChanged(path, mtime)
	if err != nil || !changed {
		t.Fatalf("changed file: changed=%v err=%v, want a fresh registry", changed, err)
	}
	if _, ok := reg.Native("brand-new"); !ok {
		t.Fatal("the fresh registry does not know the new model")
	}
}
