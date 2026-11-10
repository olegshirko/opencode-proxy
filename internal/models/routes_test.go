package models

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRouteIsQoder(t *testing.T) {
	var nilRoute *Route
	if nilRoute.IsQoder() {
		t.Fatal("nil route must not be qoder")
	}
	if !(&Route{Auth: "qoder"}).IsQoder() {
		t.Fatal("auth=qoder must report IsQoder")
	}
	if !(&Route{Auth: "QODER"}).IsQoder() {
		t.Fatal("auth is case-insensitive")
	}
	if (&Route{}).IsQoder() {
		t.Fatal("empty auth is not qoder")
	}
}

func TestLoadRegistryDuplicatePickerRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	data := `{"models":[
		{"id":"a","picker":"claude-dup","label":"A"},
		{"id":"b","picker":"claude-dup","label":"B"}
	]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRegistry(path); err == nil || !strings.Contains(err.Error(), "claude-dup") {
		t.Fatalf("want duplicate picker error, got %v", err)
	}
}

func TestSettingsValidateAgainstRegistry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	data := `{"models":[{"id":"native1","picker":"claude-native1","label":"N"}]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := LoadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}

	ok := Settings{Routes: []Route{{ID: "q1", Picker: "claude-qoder-q1", Auth: "qoder"}}}
	if err := ok.ValidateAgainst(reg); err != nil {
		t.Fatalf("clean settings rejected: %v", err)
	}

	clash := Settings{Routes: []Route{{ID: "q1", Picker: "claude-NATIVE1", Auth: "qoder"}}}
	if err := clash.ValidateAgainst(reg); err == nil || !strings.Contains(err.Error(), "claude-native1") {
		t.Fatalf("want clash error naming picker, got %v", err)
	}

	dupRoute := Settings{Routes: []Route{
		{ID: "q1", Picker: "claude-qoder-q1"},
		{ID: "q2", Picker: "Claude-Qoder-Q1"},
	}}
	if err := dupRoute.ValidateAgainst(reg); err == nil {
		t.Fatal("want duplicate route picker error")
	}
}
