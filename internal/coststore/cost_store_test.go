package coststore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCostStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-cost.json")

	store, err := NewCostStore(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	store.Add("sess-1", "deepseek-v4.1-flash", 0.0042, 0.0051, at)
	store.Add("sess-1", "deepseek-v4.1-flash", 0.0008, 0.0010, at.Add(time.Minute))
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewCostStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.Get("sess-1")
	if got.Requests != 2 {
		t.Errorf("requests = %d, want 2", got.Requests)
	}
	if diff := got.USD - 0.005; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("usd = %v, want 0.005", got.USD)
	}
	if got.ByModel["deepseek-v4.1-flash"].Requests != 2 {
		t.Errorf("by_model = %v, want the model entry", got.ByModel)
	}
	if !got.FirstAt.Equal(at) {
		t.Errorf("first_at = %v, want %v", got.FirstAt, at)
	}
}

func TestCostStoreMissingFileIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.json")
	store, err := NewCostStore(path)
	if err != nil {
		t.Fatalf("missing file must not be an error: %v", err)
	}
	if got := store.Get("anything"); got.Requests != 0 || got.Session != "anything" {
		t.Errorf("Get = %+v, want an empty record", got)
	}
}

func TestCostStoreEvictsOldestByLastAt(t *testing.T) {
	store, _ := NewCostStore("")
	base := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)

	// Oldest first, then enough newer ones to overflow the cap.
	store.Add("oldest", "m", 1, 1, base)
	for i := 0; i < MaxSessions; i++ {
		store.Add("s"+string(rune('a'+i%26))+time.Duration(i).String(), "m", 1, 1, base.Add(time.Duration(i+1)*time.Second))
	}

	if store.Get("oldest").Requests != 0 {
		t.Errorf("the least recently active session should have been evicted")
	}
	store.mu.Lock()
	n := len(store.sessions)
	store.mu.Unlock()
	if n > MaxSessions {
		t.Errorf("store holds %d sessions, cap is %d", n, MaxSessions)
	}
}

func TestCostStoreFlushReportsErrorWithoutLosingData(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := NewCostStore(filepath.Join(dir, "session-cost.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Point the store at a path that cannot be written: its parent is a file.
	store.path = filepath.Join(blocker, "session-cost.json")
	store.Add("sess", "m", 1, 2, time.Now())
	if err := store.Flush(); err == nil {
		t.Fatal("Flush over a blocked path should report an error")
	}

	// The record is still in memory and the dirty flag is still set, so a
	// later flush can retry.
	if got := store.Get("sess"); got.Requests != 1 {
		t.Errorf("in-memory record lost after a failed flush: %+v", got)
	}
}

func TestCostStoreFlusherWritesOnlyOnChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-cost.json")
	store, _ := NewCostStore(path)

	// Seed the file so its mtime exists.
	store.Add("sess", "m", 1, 1, time.Now())
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	before := info.ModTime()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.StartFlusher(ctx, 5*time.Millisecond)
	time.Sleep(40 * time.Millisecond)

	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(before) {
		t.Errorf("flusher rewrote an unchanged store")
	}

	store.Add("sess-2", "m", 1, 1, time.Now())
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f costFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Sessions["sess-2"]; !ok {
		t.Errorf("sessions = %v, want sess-2 written", f.Sessions)
	}
	if f.Version != costFileVersion {
		t.Errorf("version = %d, want %d", f.Version, costFileVersion)
	}
}
