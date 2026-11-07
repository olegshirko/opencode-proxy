package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newAdminHarness builds a Proxy whose ConfigLoader is backed by a real file,
// so tests can verify that admin writes reach the disk.
func newAdminHarness(t *testing.T, mutate ...func(*Config)) *Proxy {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	base := Defaults()
	base.Upstream = "http://127.0.0.1:1"
	base.Log.File = ""
	base.Retry = RetryConfig{MaxAttempts: 2, FirstWaitMS: 1, Multiplier: 2, MaxWaitMS: 5, TotalBudgetMS: 50}
	base.Models = ModelsConfig{
		Default:    "deepseek-v4.1-flash",
		Background: "deepseek-v4.1-flash",
		Aliases:    map[string]string{"claude-legacy": "glm-5.3"},
	}
	for _, fn := range mutate {
		fn(&base)
	}
	data, err := json.MarshalIndent(base, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loader, err := NewConfigLoader(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	logger := newLoggerWithWriter(&bytes.Buffer{}, loader.Get().Log)
	return NewProxy(loader, testRegistry(t), logger, &http.Client{})
}

func adminDo(t *testing.T, p *Proxy, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	p.AdminHandler().ServeHTTP(rec, req)
	return rec
}

func decodeJSON[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

func TestAdminStatusReportsCounters(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	p := newAdminHarness(t, func(c *Config) { c.Upstream = up.URL })
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"claude-GLM-5.3"}`))
		p.ServeHTTP(httptest.NewRecorder(), req)
	}

	rec := adminDo(t, p, http.MethodGet, "/admin/status", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := decodeJSON[map[string]any](t, rec)
	if got := body["requests"].(float64); got != 2 {
		t.Errorf("requests = %v, want 2", got)
	}
	if got := body["upstream"]; got != up.URL {
		t.Errorf("upstream = %v, want %v", got, up.URL)
	}
}

func TestAdminPutCaptureTogglesAndPersists(t *testing.T) {
	p := newAdminHarness(t)

	rec := adminDo(t, p, http.MethodPut, "/admin/log/capture", `{"request_headers":true,"response_body":true}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200: %s", rec.Code, rec.Body)
	}

	got := decodeJSON[CaptureConfig](t, adminDo(t, p, http.MethodGet, "/admin/log/capture", "", nil))
	if !got.RequestHeaders || !got.ResponseBody {
		t.Errorf("capture = %+v, want request_headers and response_body", got)
	}
	if got.RequestBody || got.ResponseHeaders {
		t.Errorf("capture = %+v, unset flags flipped on", got)
	}

	onDisk, err := os.ReadFile(p.cfg.path)
	if err != nil {
		t.Fatal(err)
	}
	var saved Config
	if err := json.Unmarshal(onDisk, &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.Log.Capture.RequestHeaders || !saved.Log.Capture.ResponseBody {
		t.Errorf("config.json capture = %+v, want the flags persisted", saved.Log.Capture)
	}
}

func TestAdminPatchConfigMergesNotReplaces(t *testing.T) {
	p := newAdminHarness(t)

	rec := adminDo(t, p, http.MethodPatch, "/admin/config", `{"models":{"default":"glm-5.3"}}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200: %s", rec.Code, rec.Body)
	}

	cfg := p.cfg.Get()
	if cfg.Models.Default != "glm-5.3" {
		t.Errorf("default = %q, want glm-5.3", cfg.Models.Default)
	}
	if cfg.Models.Background != "deepseek-v4.1-flash" {
		t.Errorf("background = %q, want it untouched", cfg.Models.Background)
	}
	if cfg.Models.Aliases["claude-legacy"] != "glm-5.3" {
		t.Errorf("aliases = %v, want the existing table kept", cfg.Models.Aliases)
	}
}

func TestAdminAliasesReplace(t *testing.T) {
	p := newAdminHarness(t)

	rec := adminDo(t, p, http.MethodPut, "/admin/models/aliases", `{"claude-new":"glm-5.3"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200: %s", rec.Code, rec.Body)
	}

	aliases := p.cfg.Get().Models.Aliases
	if _, ok := aliases["claude-legacy"]; ok {
		t.Errorf("aliases = %v, old entry survived a replace", aliases)
	}
	if aliases["claude-new"] != "glm-5.3" {
		t.Errorf("aliases = %v, want claude-new", aliases)
	}
}

func TestAdminRequiresTokenWhenSet(t *testing.T) {
	p := newAdminHarness(t)

	// Setting the token through the API needs no token yet.
	if rec := adminDo(t, p, http.MethodPatch, "/admin/config", `{"admin":{"token":"s3cret"}}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("set token: status = %d, want 200", rec.Code)
	}

	if rec := adminDo(t, p, http.MethodGet, "/admin/status", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("without token: status = %d, want 401", rec.Code)
	}
	rec := adminDo(t, p, http.MethodGet, "/admin/status", "", map[string]string{"X-Admin-Token": "s3cret"})
	if rec.Code != http.StatusOK {
		t.Errorf("with token: status = %d, want 200", rec.Code)
	}

	// The token must not leak through GET /admin/config.
	rec = adminDo(t, p, http.MethodGet, "/admin/config", "", map[string]string{"X-Admin-Token": "s3cret"})
	if body := rec.Body.String(); bytes.Contains([]byte(body), []byte("s3cret")) {
		t.Errorf("config response leaks the token: %s", body)
	}
}

func TestAdminDisabledReturns404(t *testing.T) {
	p := newAdminHarness(t, func(c *Config) { c.Admin.Enabled = false })

	rec := adminDo(t, p, http.MethodGet, "/admin/status", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestAdminSessionCostShape(t *testing.T) {
	p := newAdminHarness(t)
	now := time.Now()
	p.addSessionCost("sess-1", "deepseek-v4.1-flash", 0.0042, 0.0051, now)

	rec := adminDo(t, p, http.MethodGet, "/admin/session-cost?session=sess-1", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	body := decodeJSON[map[string]any](t, rec)
	for _, key := range []string{"session", "usd", "list_usd", "requests", "first_at", "last_at", "by_model"} {
		if _, ok := body[key]; !ok {
			t.Errorf("response is missing %q: %s", key, rec.Body)
		}
	}
	if body["usd"].(float64) != 0.0042 {
		t.Errorf("usd = %v, want 0.0042", body["usd"])
	}
	if body["requests"].(float64) != 1 {
		t.Errorf("requests = %v, want 1", body["requests"])
	}
	byModel := body["by_model"].(map[string]any)
	if _, ok := byModel["deepseek-v4.1-flash"]; !ok {
		t.Errorf("by_model = %v, want the model entry", byModel)
	}
}

func TestAdminSessionCostUnknownSessionReturnsZeros(t *testing.T) {
	p := newAdminHarness(t)

	rec := adminDo(t, p, http.MethodGet, "/admin/session-cost?session=nope", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (not 404)", rec.Code)
	}
	body := decodeJSON[map[string]any](t, rec)
	if body["usd"].(float64) != 0 || body["requests"].(float64) != 0 {
		t.Errorf("unknown session = %v, want zeros", body)
	}
	if got := body["session"]; got != "nope" {
		t.Errorf("session = %v, want the requested id", got)
	}
}
