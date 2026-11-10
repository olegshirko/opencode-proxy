// Package qoderauth holds Qoder's service credential: it exchanges the user's
// personal access token for a short-lived jobToken, refreshes that token while
// it can, and re-exchanges when the upstream rejects the refresh. The PAT
// itself never leaves this package; Status() reports counters only.
package qoderauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Default hosts and lifetimes, from the app's own constants (the design spec
// lists them; the spike confirms).
const (
	DefaultOpenAPIBaseURL = "https://openapi.qoder.sh"
	DefaultRefreshMargin  = 60 * time.Second

	pathExchange = "/api/v1/jobToken/exchange"
	pathRefresh  = "/api/v1/jobToken/refresh"
)

// Config points at the PAT and the host that trades it for a jobToken.
type Config struct {
	OpenAPIBaseURL  string `json:"openapi_base_url"`
	TokenEnv        string `json:"token_env"`
	TokenFile       string `json:"token_file"`
	RefreshMarginMS int    `json:"refresh_margin_ms"`
}

// Status is the admin view of the manager. It carries no secret material.
type Status struct {
	State      string    `json:"state"` // "empty" | "ready" | "error"
	HasRefresh bool      `json:"has_refresh"`
	ExpiresAt  time.Time `json:"expires_at,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
	Exchanges  int64     `json:"exchanges"`
	Refreshes  int64     `json:"refreshes"`
	ReAuths    int64     `json:"reauths"`
}

// exchangeResponse mirrors the jobToken endpoints' JSON. The token is present
// under one of three names depending on deployment; pick in that order.
type exchangeResponse struct {
	Token      string `json:"token"`
	DeviceTOk  string `json:"device_token"`
	AccessTok  string `json:"access_token"`
	Refresh    string `json:"refresh_token"`
	ExpiresAt  int64  `json:"expires_at"`
	ExpireTime int64  `json:"expire_time"`
	ExpiresIn  int64  `json:"expires_in"`
}

func (r *exchangeResponse) token() string {
	for _, v := range []string{r.Token, r.DeviceTOk, r.AccessTok} {
		if v != "" {
			return v
		}
	}
	return ""
}

// Manager owns the jobToken lifecycle.
type Manager struct {
	cfg    Config
	client *http.Client
	now    func() time.Time

	mu         sync.Mutex
	token      string
	refresh    string
	expiresAt  time.Time
	lastErr    string
	exchanges  int64
	refreshes  int64
	reauths    int64
	everIssued bool
}

// New builds a manager. client and now are injectable for tests; nil picks
// sensible defaults.
func New(cfg Config, client *http.Client, now func() time.Time) *Manager {
	if cfg.OpenAPIBaseURL == "" {
		cfg.OpenAPIBaseURL = DefaultOpenAPIBaseURL
	}
	if cfg.RefreshMarginMS <= 0 {
		cfg.RefreshMarginMS = int(DefaultRefreshMargin / time.Millisecond)
	}
	if client == nil {
		client = http.DefaultClient
	}
	if now == nil {
		now = time.Now
	}
	return &Manager{cfg: cfg, client: client, now: now}
}

// Token returns a live jobToken, exchanging or refreshing as needed.
func (m *Manager) Token(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tokenLocked(ctx)
}

func (m *Manager) tokenLocked(ctx context.Context) (string, error) {
	now := m.now()
	if m.token != "" && now.Before(m.expiresAt.Add(-time.Duration(m.cfg.RefreshMarginMS)*time.Millisecond)) {
		return m.token, nil
	}
	if m.token != "" && m.refresh != "" {
		if tok, refresh, expiresAt, err := m.refreshToken(ctx); err == nil {
			m.set(tok, refresh, expiresAt)
			m.refreshes++
			return tok, nil
		}
		// Refresh refused (400/401/403 or network): fall through to a full
		// re-exchange from the PAT.
		m.token, m.refresh, m.expiresAt = "", "", time.Time{}
	}
	return m.exchangeLocked(ctx)
}

func (m *Manager) exchangeLocked(ctx context.Context) (string, error) {
	pat, err := m.personalToken()
	if err != nil {
		m.lastErr = err.Error()
		return "", err
	}
	var resp exchangeResponse
	if err := m.post(ctx, pathExchange, map[string]string{"personal_token": pat}, &resp); err != nil {
		m.lastErr = err.Error()
		return "", err
	}
	tok := resp.token()
	if tok == "" {
		err := fmt.Errorf("qoder: %s returned no credential", pathExchange)
		m.lastErr = err.Error()
		return "", err
	}
	m.set(tok, resp.Refresh, m.expiry(resp))
	m.exchanges++
	m.lastErr = ""
	return tok, nil
}

// Invalidate drops the cached credential after an upstream 401: the next
// Token() re-exchanges from the PAT.
func (m *Manager) Invalidate() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.token, m.refresh, m.expiresAt = "", "", time.Time{}
	m.reauths++
}

// Status snapshots the manager state.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{
		HasRefresh: m.refresh != "",
		ExpiresAt:  m.expiresAt,
		Exchanges:  m.exchanges,
		Refreshes:  m.refreshes,
		ReAuths:    m.reauths,
		LastError:  m.lastErr,
	}
	switch {
	case m.token != "":
		st.State = "ready"
	case m.everIssued || m.lastErr != "":
		st.State = "error"
	default:
		st.State = "empty"
	}
	return st
}

func (m *Manager) set(token, refresh string, expiresAt time.Time) {
	m.token, m.refresh, m.expiresAt = token, refresh, expiresAt
	m.everIssued = true
}

func (m *Manager) refreshToken(ctx context.Context) (string, string, time.Time, error) {
	var resp exchangeResponse
	if err := m.post(ctx, pathRefresh, map[string]string{"refresh_token": m.refresh}, &resp); err != nil {
		return "", "", time.Time{}, err
	}
	tok := resp.token()
	if tok == "" {
		return "", "", time.Time{}, fmt.Errorf("qoder: %s returned no credential", pathRefresh)
	}
	return tok, resp.Refresh, m.expiry(resp), nil
}

// expiry normalises the response's lifetime fields: an absolute epoch first
// (ms when it is large enough to be ms), otherwise expires_in seconds.
func (m *Manager) expiry(resp exchangeResponse) time.Time {
	for _, abs := range []int64{resp.ExpiresAt, resp.ExpireTime} {
		if abs > 0 {
			return timeFromEpoch(abs)
		}
	}
	if resp.ExpiresIn > 0 {
		return m.now().Add(time.Duration(resp.ExpiresIn) * time.Second)
	}
	return m.now().Add(10 * time.Minute)
}

func timeFromEpoch(v int64) time.Time {
	if v > 1e12 { // milliseconds
		return time.UnixMilli(v)
	}
	return time.Unix(v, 0)
}

func (m *Manager) post(ctx context.Context, path string, payload map[string]string, out *exchangeResponse) error {
	buf, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(m.cfg.OpenAPIBaseURL, "/")+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	httpResp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("qoder: %s: %w", path, err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return fmt.Errorf("qoder: %s: HTTP %d", path, httpResp.StatusCode)
	}
	if err := json.NewDecoder(httpResp.Body).Decode(out); err != nil {
		return fmt.Errorf("qoder: %s: bad JSON: %w", path, err)
	}
	return nil
}

// personalToken reads the PAT from the environment first, then a file
// (PathExpand ~/). The error names the config keys, per the spec.
func (m *Manager) personalToken() (string, error) {
	if m.cfg.TokenEnv != "" {
		if v := strings.TrimSpace(os.Getenv(m.cfg.TokenEnv)); v != "" {
			return v, nil
		}
	}
	if m.cfg.TokenFile != "" {
		path := m.cfg.TokenFile
		if strings.HasPrefix(path, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				path = filepath.Join(home, path[2:])
			}
		}
		if data, err := os.ReadFile(path); err == nil {
			if v := strings.TrimSpace(string(data)); v != "" {
				return v, nil
			}
		}
		return "", fmt.Errorf("qoder: token_file %s is empty or unreadable", m.cfg.TokenFile)
	}
	return "", fmt.Errorf("qoder: no personal access token: set qoder.token_env or qoder.token_file")
}
