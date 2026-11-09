package main

import (
	"encoding/json"
	"fmt"
	"opencode-proxy/internal/reqlog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"opencode-proxy/internal/atomicio"
	"opencode-proxy/internal/quota"
	"opencode-proxy/internal/retry"
)

// Config is the proxy configuration. It lives in one JSON file that is
// reread on mtime changes so edits apply without a restart.
type Config struct {
	Listen                string          `json:"listen"`
	Upstream              string          `json:"upstream"`
	SessionFallback       string          `json:"session_fallback"`
	Log                   reqlog.Config   `json:"log"`
	Models                ModelsConfig    `json:"models"`
	Retry                 retry.Config    `json:"retry"`
	Admin                 AdminConfig     `json:"admin"`
	Scheduler             SchedulerConfig `json:"scheduler"`
	ConnectTimeoutSeconds int             `json:"connect_timeout_seconds"`
	SessionCostFile       string          `json:"session_cost_file"`
	UsageFile             string          `json:"usage_file"`
	Limits                quota.Limits    `json:"limits"`
}

// SchedulerConfig tunes cell assignment and the keepalive used while retrying.
type SchedulerConfig struct {
	MaxConcurrentCells  int  `json:"max_concurrent_cells"`
	QueueTimeoutMS      int  `json:"queue_timeout_ms"`
	KeepaliveAfterMS    int  `json:"keepalive_after_ms"`
	KeepaliveIntervalMS int  `json:"keepalive_interval_ms"`
	FallbackAfterMS     int  `json:"fallback_after_ms"`
	FallbackRotateMain  bool `json:"fallback_rotate_main"`
	// ThinkingSummaryWaitMS bounds how long a thinking block's start event is
	// held back while the proxy collects a one-line summary for it.
	ThinkingSummaryWaitMS int `json:"thinking_summary_wait_ms"`
}

// ModelRoute sends one model to a different upstream endpoint with its own
// credentials. The gateway is not one host: the subscription answers on
// /inference/go, while models outside the plan live on /inference with their
// own token, and a route is what tells the proxy which is which.
type ModelRoute struct {
	ID        string  `json:"id"`
	Picker    string  `json:"picker"`
	Label     string  `json:"label"`
	BehavesAs string  `json:"behaves_as"`
	MaxEffort string  `json:"max_effort"`
	Context   int     `json:"context"`
	BaseURL   string  `json:"base_url"`
	TokenEnv  string  `json:"token_env"`
	TokenFile string  `json:"token_file"`
	Pricing   Pricing `json:"pricing"`
}

// Matches reports whether the route owns this model name, which may be the
// gateway id or the picker the desktop app shows.
func (r *ModelRoute) Matches(name string) bool {
	in := strings.ToLower(strings.TrimSpace(name))
	return in != "" && (in == strings.ToLower(r.ID) || in == strings.ToLower(r.Picker))
}

// Token returns the route's credential, read from the environment first and
// from a file second. An empty result means the route runs unauthenticated.
func (r *ModelRoute) Token() string {
	if r == nil {
		return ""
	}
	if r.TokenEnv != "" {
		if v := strings.TrimSpace(os.Getenv(r.TokenEnv)); v != "" {
			return v
		}
	}
	if r.TokenFile != "" {
		path := r.TokenFile
		if strings.HasPrefix(path, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				path = filepath.Join(home, path[2:])
			}
		}
		if data, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}

type ModelsConfig struct {
	Default    string            `json:"default"`
	Background string            `json:"background"`
	Aliases    map[string]string `json:"aliases"`
	Routes     []ModelRoute      `json:"routes"`
	// CatalogURL is the gateway's model list that /v1/models serves. Empty
	// means derive it from upstream, so the picker follows the subscription
	// without a hand-maintained file.
	CatalogURL string `json:"catalog_url"`
}

// RouteFor returns the route that owns a resolved gateway id, or nil.
func (m ModelsConfig) RouteFor(id string) *ModelRoute {
	for i := range m.Routes {
		if strings.EqualFold(strings.TrimSpace(m.Routes[i].ID), id) {
			return &m.Routes[i]
		}
	}
	return nil
}

// RouteNamed returns the route owning a name the client sent, which may be
// the picker rather than the gateway id.
func (m ModelsConfig) RouteNamed(name string) *ModelRoute {
	for i := range m.Routes {
		if m.Routes[i].Matches(name) {
			return &m.Routes[i]
		}
	}
	return nil
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
		logFile = filepath.Join(home, "opencode-proxy", "var", "proxy.log")
	}

	return Config{
		Listen:          "127.0.0.1:4000",
		Upstream:        "https://opencode.ai/inference/go/anthropic",
		SessionFallback: "my-session-1",
		Log: reqlog.Config{
			File:         logFile,
			Level:        "info",
			MaxMB:        10,
			Keep:         5,
			Capture:      reqlog.Capture{},
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
		Retry: retry.Config{
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
		Scheduler: SchedulerConfig{
			MaxConcurrentCells:    8,
			QueueTimeoutMS:        60000,
			KeepaliveAfterMS:      3000,
			KeepaliveIntervalMS:   5000,
			FallbackAfterMS:       20000,
			FallbackRotateMain:    false,
			ThinkingSummaryWaitMS: 500,
		},
		SessionCostFile: filepath.Join(filepath.Dir(logFile), "session-cost.json"),
		UsageFile:       filepath.Join(filepath.Dir(logFile), "usage.json"),
		Limits: quota.Limits{
			MonthlyUSD:       0,
			FiveHourFraction: 0.2,
			WeeklyFraction:   0.5,
			WarningFraction:  0.85,
		},
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
	if err := atomicio.WriteFile(l.path, data); err != nil {
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
