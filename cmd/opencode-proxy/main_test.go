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
