package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Config is the proxy configuration. It lives in one JSON file that is
// reread on mtime changes so edits apply without a restart.
type Config struct {
	Listen                string       `json:"listen"`
	Upstream              string       `json:"upstream"`
	SessionFallback       string       `json:"session_fallback"`
	Log                   LogConfig    `json:"log"`
	Models                ModelsConfig `json:"models"`
	Retry                 RetryConfig  `json:"retry"`
	Admin                 AdminConfig  `json:"admin"`
	ConnectTimeoutSeconds int          `json:"connect_timeout_seconds"`
}

type LogConfig struct {
	File         string        `json:"file"`
	Level        string        `json:"level"`
	MaxMB        int           `json:"max_mb"`
	Keep         int           `json:"keep"`
	Capture      CaptureConfig `json:"capture"`
	MaxBodyBytes int64         `json:"max_body_bytes"`
}

type CaptureConfig struct {
	RequestHeaders  bool `json:"request_headers"`
	RequestBody     bool `json:"request_body"`
	ResponseHeaders bool `json:"response_headers"`
	ResponseBody    bool `json:"response_body"`
}

type ModelsConfig struct {
	Default    string            `json:"default"`
	Background string            `json:"background"`
	Aliases    map[string]string `json:"aliases"`
}

type RetryConfig struct {
	MaxAttempts   int     `json:"max_attempts"`
	FirstWaitMS   int     `json:"first_wait_ms"`
	Multiplier    float64 `json:"multiplier"`
	MaxWaitMS     int     `json:"max_wait_ms"`
	TotalBudgetMS int     `json:"total_budget_ms"`
}

type AdminConfig struct {
	Enabled bool   `json:"enabled"`
	Token   string `json:"token"`
}

// Defaults returns the built-in configuration. Callers get a fresh value,
// including a fresh Aliases map.
func Defaults() Config {
	logFile := "proxy.log"
	if home, err := os.UserHomeDir(); err == nil {
		logFile = filepath.Join(home, "opencode-proxy", "proxy.log")
	}

	return Config{
		Listen:          "127.0.0.1:4000",
		Upstream:        "https://opencode.ai/inference/go/anthropic",
		SessionFallback: "my-session-1",
		Log: LogConfig{
			File:         logFile,
			Level:        "info",
			MaxMB:        10,
			Keep:         5,
			Capture:      CaptureConfig{},
			MaxBodyBytes: 65536,
		},
		Models: ModelsConfig{
			Default:    "deepseek-v4.1-flash",
			Background: "deepseek-v4.1-flash",
			Aliases: map[string]string{
				"claude-v4.1-flash": "deepseek-v4.1-flash",
				"claude-v151-flash": "deepseek-v4.1-flash",
				"claude-sonnet-5":   "deepseek-v4.1-flash",
			},
		},
		Retry: RetryConfig{
			MaxAttempts:   5,
			FirstWaitMS:   2000,
			Multiplier:    2,
			MaxWaitMS:     15000,
			TotalBudgetMS: 60000,
		},
		Admin: AdminConfig{
			Enabled: true,
			Token:   "",
		},
		ConnectTimeoutSeconds: 15,
	}
}

// ConfigLoader holds the current configuration and its file mtime. Get is
// safe to call from request goroutines while the file is being reloaded or
// updated.
type ConfigLoader struct {
	path  string
	mu    sync.RWMutex
	cfg   Config
	mtime time.Time
}

// NewConfigLoader reads path into a Config filled with Defaults, then keeps
// the result for concurrent readers.
func NewConfigLoader(path string) (*ConfigLoader, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("config: stat %s: %w", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	cfg, err := parseConfig(data)
	if err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return &ConfigLoader{path: path, cfg: cfg, mtime: info.ModTime()}, nil
}

// Defaults returns the built-in configuration values.
func (l *ConfigLoader) Defaults() Config {
	return Defaults()
}

// Get returns a snapshot that the caller may modify freely.
func (l *ConfigLoader) Get() *Config {
	l.mu.RLock()
	defer l.mu.RUnlock()
	cfg := l.cfg.clone()
	return &cfg
}

// ReloadIfChanged rereads the file when its mtime is newer than the loaded
// version. On error the last good configuration stays in place.
func (l *ConfigLoader) ReloadIfChanged() error {
	info, err := os.Stat(l.path)
	if err != nil {
		return fmt.Errorf("config: stat %s: %w", l.path, err)
	}

	l.mu.RLock()
	unchanged := !info.ModTime().After(l.mtime)
	l.mu.RUnlock()
	if unchanged {
		return nil
	}

	data, err := os.ReadFile(l.path)
	if err != nil {
		return fmt.Errorf("config: read %s: %w", l.path, err)
	}
	cfg, err := parseConfig(data)
	if err != nil {
		return fmt.Errorf("config: parse %s: %w", l.path, err)
	}

	l.mu.Lock()
	l.cfg = cfg
	l.mtime = info.ModTime()
	l.mu.Unlock()
	return nil
}

// Update applies fn to a copy of the configuration, then writes the result
// to disk atomically. The in-memory configuration changes only if both fn
// and the write succeed.
func (l *ConfigLoader) Update(fn func(*Config) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	next := l.cfg.clone()
	if err := fn(&next); err != nil {
		return err
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	data = append(data, '\n')
	if err := writeFileAtomic(l.path, data); err != nil {
		return fmt.Errorf("config: write %s: %w", l.path, err)
	}

	l.cfg = next
	info, err := os.Stat(l.path)
	if err != nil {
		return fmt.Errorf("config: stat %s after write: %w", l.path, err)
	}
	l.mtime = info.ModTime()
	return nil
}

// parseConfig decodes data over the defaults. A file-provided aliases table
// replaces the default one instead of merging into it; an omitted table
// keeps the defaults.
func parseConfig(data []byte) (Config, error) {
	cfg := Defaults()
	defaultAliases := cfg.Models.Aliases
	cfg.Models.Aliases = nil
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	if cfg.Models.Aliases == nil {
		cfg.Models.Aliases = defaultAliases
	}
	return cfg, nil
}

func (c Config) clone() Config {
	if c.Models.Aliases != nil {
		aliases := make(map[string]string, len(c.Models.Aliases))
		for k, v := range c.Models.Aliases {
			aliases[k] = v
		}
		c.Models.Aliases = aliases
	}
	return c
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
