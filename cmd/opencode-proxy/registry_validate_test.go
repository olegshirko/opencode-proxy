package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"opencode-proxy/internal/models"
)

func TestLoadAndValidateRegistryRejectsRoutePickerClash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	if err := os.WriteFile(path, []byte(
		`{"models":[{"id":"native1","picker":"claude-native1","label":"N"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := models.Settings{Routes: []models.Route{
		{ID: "q1", Picker: "claude-native1", Auth: "qoder"},
	}}
	if _, _, err := loadAndValidateRegistry(path, settings); err == nil ||
		!strings.Contains(err.Error(), "claude-native1") {
		t.Fatalf("want clash error, got %v", err)
	}

	settings.Routes[0].Picker = "claude-qoder-q1"
	reg, existed, err := loadAndValidateRegistry(path, settings)
	if err != nil || !existed || len(reg.All()) != 1 {
		t.Fatalf("clean load: existed=%v err=%v", existed, err)
	}
}

func TestLoadAndValidateRegistryMissingFile(t *testing.T) {
	reg, existed, err := loadAndValidateRegistry(filepath.Join(t.TempDir(), "nope.json"), models.Settings{})
	if err != nil || existed || reg == nil {
		t.Fatalf("missing file: existed=%v reg=%v err=%v", existed, reg != nil, err)
	}
}
