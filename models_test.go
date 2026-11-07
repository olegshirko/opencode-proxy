package main

import (
	"os"
	"path/filepath"
	"testing"
)

func mustRegistry(t *testing.T, data string) *Registry {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	r, err := LoadRegistry(path)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	return r
}

func TestResolve(t *testing.T) {
	r := mustRegistry(t, `{"models":[
        {"id":"glm-5.3","picker":"claude-GLM-5.3","available":true},
        {"id":"claude-haiku-5-5","available":true},
        {"id":"deepseek-v4.1-flash","picker":"claude-DeepSeek","available":true}]}`)
	cfg := ModelsConfig{
		Default: "deepseek-v4.1-flash", Background: "glm-5.3-flash",
		Aliases: map[string]string{"claude-v4.1-flash": "glm-5.3"},
	}
	cases := []struct{ in, want, why string }{
		{"claude-v4.1-flash", "glm-5.3", "alias"},
		{"CLAUDE-V4.1-FLASH", "glm-5.3", "alias"},
		{"claude-GLM-5.3", "glm-5.3", "picker"},
		{"claude-glm-5.3", "glm-5.3", "picker"},
		{"claude-haiku-5-5", "glm-5.3-flash", "background"},
		{"claude-haiku-4-5", "glm-5.3-flash", "background"},
		{"claude-claude-haiku-5-5", "claude-haiku-5-5", "picker"},
		{"deepseek-v4.1-flash", "deepseek-v4.1-flash", "native"},
		{"claude-sonnet-5", "deepseek-v4.1-flash", "default"},
		{"claude-opus-5", "deepseek-v4.1-flash", "default"},
		// The desktop app marks 1M-context runs with a [1m] suffix.
		{"claude-GLM-5.3[1m]", "glm-5.3", "picker"},
		{"claude-glm-5.3[1M]", "glm-5.3", "picker"},
		{"deepseek-v4.1-flash[1m]", "deepseek-v4.1-flash", "native"},
		{"", "deepseek-v4.1-flash", "default"},
	}
	for _, c := range cases {
		got, why := r.Resolve(c.in, cfg)
		if got != c.want || why != c.why {
			t.Errorf("Resolve(%q) = (%q,%q), want (%q,%q)", c.in, got, why, c.want, c.why)
		}
	}
}

func TestAvailableFiltersAndPreservesOrder(t *testing.T) {
	r := mustRegistry(t, `{"models":[
        {"id":"glm-5.3","available":true},
        {"id":"kimi-k3","available":false},
        {"id":"deepseek-v4.1-flash","available":true}]}`)

	got := r.Available()
	if len(got) != 2 {
		t.Fatalf("Available() returned %d models, want 2: %+v", len(got), got)
	}
	if got[0].ID != "glm-5.3" || got[1].ID != "deepseek-v4.1-flash" {
		t.Errorf("Available() ids = [%q %q], want [glm-5.3 deepseek-v4.1-flash]", got[0].ID, got[1].ID)
	}
}

func TestNativeIsCaseInsensitive(t *testing.T) {
	r := mustRegistry(t, `{"models":[{"id":"glm-5.3","available":true}]}`)

	m, ok := r.Native("GLM-5.3")
	if !ok {
		t.Fatal(`Native("GLM-5.3") not found, want found`)
	}
	if m.ID != "glm-5.3" {
		t.Errorf("Native(\"GLM-5.3\").ID = %q, want %q", m.ID, "glm-5.3")
	}
	if _, ok := r.Native("kimi-k3"); ok {
		t.Error(`Native("kimi-k3") found, want missing`)
	}
}
