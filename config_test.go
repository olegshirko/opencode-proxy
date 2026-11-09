package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultsFillMissingFields(t *testing.T) {
	cfg := Defaults()
	if cfg.Listen != "127.0.0.1:4000" {
		t.Fatalf("listen = %q", cfg.Listen)
	}
	if cfg.Retry.MaxAttempts != 5 {
		t.Fatalf("attempts = %d", cfg.Retry.MaxAttempts)
	}
	if cfg.Log.MaxBodyBytes != 65536 {
		t.Fatalf("max_body = %d", cfg.Log.MaxBodyBytes)
	}
}

func TestDefaultsMatchSpec(t *testing.T) {
	cfg := Defaults()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("user home: %v", err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"upstream", cfg.Upstream, "https://opencode.ai/inference/go/anthropic"},
		{"session_fallback", cfg.SessionFallback, "my-session-1"},
		{"log.file", cfg.Log.File, filepath.Join(home, "opencode-proxy", "var", "proxy.log")},
		{"log.level", cfg.Log.Level, "info"},
		{"log.max_mb", cfg.Log.MaxMB, 10},
		{"log.keep", cfg.Log.Keep, 5},
		{"log.capture.request_headers", cfg.Log.Capture.RequestHeaders, false},
		{"log.capture.request_body", cfg.Log.Capture.RequestBody, false},
		{"log.capture.response_headers", cfg.Log.Capture.ResponseHeaders, false},
		{"log.capture.response_body", cfg.Log.Capture.ResponseBody, false},
		{"log.max_body_bytes", cfg.Log.MaxBodyBytes, int64(65536)},
		{"models.default", cfg.Models.Default, "deepseek-v4.1-flash"},
		{"models.background", cfg.Models.Background, "deepseek-v4.1-flash"},
		{"models.aliases[claude-v4.1-flash]", cfg.Models.Aliases["claude-v4.1-flash"], "deepseek-v4.1-flash"},
		{"models.aliases[claude-v151-flash]", cfg.Models.Aliases["claude-v151-flash"], "deepseek-v4.1-flash"},
		{"models.aliases[claude-sonnet-5]", cfg.Models.Aliases["claude-sonnet-5"], "deepseek-v4.1-flash"},
		{"retry.first_wait_ms", cfg.Retry.FirstWaitMS, 2000},
		{"retry.multiplier", cfg.Retry.Multiplier, 2.0},
		{"retry.max_wait_ms", cfg.Retry.MaxWaitMS, 15000},
		{"retry.total_budget_ms", cfg.Retry.TotalBudgetMS, 60000},
		{"admin.enabled", cfg.Admin.Enabled, true},
		{"admin.token", cfg.Admin.Token, ""},
		{"connect_timeout_seconds", cfg.ConnectTimeoutSeconds, 15},
	}

	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func writeConfigFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestNewConfigLoaderMergesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfigFile(t, path, `{"listen":"127.0.0.1:5555"}`)

	loader, err := NewConfigLoader(path)
	if err != nil {
		t.Fatalf("NewConfigLoader: %v", err)
	}

	cfg := loader.Get()
	if cfg.Listen != "127.0.0.1:5555" {
		t.Fatalf("listen = %q, want override", cfg.Listen)
	}
	if cfg.Upstream != Defaults().Upstream {
		t.Fatalf("upstream = %q, want default", cfg.Upstream)
	}
	if cfg.Retry.MaxAttempts != 5 {
		t.Fatalf("attempts = %d, want default 5", cfg.Retry.MaxAttempts)
	}
	if cfg.Admin.Enabled != true {
		t.Fatalf("admin.enabled = %v, want default true", cfg.Admin.Enabled)
	}
}

func TestReloadIfChangedPicksUpEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfigFile(t, path, `{"log":{"level":"info"}}`)

	loader, err := NewConfigLoader(path)
	if err != nil {
		t.Fatalf("NewConfigLoader: %v", err)
	}
	if got := loader.Get().Log.Level; got != "info" {
		t.Fatalf("level before edit = %q", got)
	}

	writeConfigFile(t, path, `{"log":{"level":"debug"}}`)
	next := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, next, next); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if err := loader.ReloadIfChanged(); err != nil {
		t.Fatalf("ReloadIfChanged: %v", err)
	}
	if got := loader.Get().Log.Level; got != "debug" {
		t.Fatalf("level after edit = %q, want debug", got)
	}
}

func TestUpdatePersistsToDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfigFile(t, path, `{"listen":"127.0.0.1:4000"}`)

	loader, err := NewConfigLoader(path)
	if err != nil {
		t.Fatalf("NewConfigLoader: %v", err)
	}

	err = loader.Update(func(cfg *Config) error {
		cfg.Listen = "127.0.0.1:4999"
		cfg.Models.Aliases["claude-test"] = "deepseek-v4.1-flash"
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	if got := loader.Get().Listen; got != "127.0.0.1:4999" {
		t.Fatalf("in-memory listen = %q", got)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var onDisk Config
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("saved config is not valid JSON: %v", err)
	}
	if onDisk.Listen != "127.0.0.1:4999" {
		t.Fatalf("on-disk listen = %q", onDisk.Listen)
	}
	if onDisk.Models.Aliases["claude-test"] != "deepseek-v4.1-flash" {
		t.Fatalf("on-disk aliases = %v", onDisk.Models.Aliases)
	}

	reloaded, err := NewConfigLoader(path)
	if err != nil {
		t.Fatalf("NewConfigLoader after save: %v", err)
	}
	if got := reloaded.Get().Listen; got != "127.0.0.1:4999" {
		t.Fatalf("reloaded listen = %q", got)
	}
}

func TestUpdateErrorLeavesConfigUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfigFile(t, path, `{"listen":"127.0.0.1:4000"}`)

	loader, err := NewConfigLoader(path)
	if err != nil {
		t.Fatalf("NewConfigLoader: %v", err)
	}

	boom := os.ErrInvalid
	err = loader.Update(func(cfg *Config) error {
		cfg.Listen = "127.0.0.1:1"
		return boom
	})
	if err != boom {
		t.Fatalf("Update error = %v, want %v", err, boom)
	}
	if got := loader.Get().Listen; got != "127.0.0.1:4000" {
		t.Fatalf("listen changed on failed Update: %q", got)
	}
}

func TestInvalidConfigKeepsLastGood(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfigFile(t, path, `{"listen":"127.0.0.1:4111"}`)

	loader, err := NewConfigLoader(path)
	if err != nil {
		t.Fatalf("NewConfigLoader: %v", err)
	}

	writeConfigFile(t, path, `{"listen":`)
	next := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, next, next); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if err := loader.ReloadIfChanged(); err == nil {
		t.Fatal("ReloadIfChanged succeeded on broken JSON")
	}
	if got := loader.Get().Listen; got != "127.0.0.1:4111" {
		t.Fatalf("listen = %q, want last good config", got)
	}
}

func TestGetReturnsIsolatedCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfigFile(t, path, `{"listen":"127.0.0.1:4000"}`)

	loader, err := NewConfigLoader(path)
	if err != nil {
		t.Fatalf("NewConfigLoader: %v", err)
	}

	cfg := loader.Get()
	cfg.Listen = "mutated"
	cfg.Models.Aliases["claude-v4.1-flash"] = "mutated"

	fresh := loader.Get()
	if fresh.Listen != "127.0.0.1:4000" {
		t.Fatalf("loader state mutated through Get: listen = %q", fresh.Listen)
	}
	if got := fresh.Models.Aliases["claude-v4.1-flash"]; got != "deepseek-v4.1-flash" {
		t.Fatalf("loader alias mutated through Get: %q", got)
	}
}
