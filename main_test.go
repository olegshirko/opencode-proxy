package main

import (
	"net/http"
	"net/http/httptest"
	"opencode-proxy/internal/reqlog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRouterSendsAdminToAdmin(t *testing.T) {
	var upstreamCalls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	cfg := testConfig(up.URL)
	p := NewProxy(cfg, testRegistry(t), reqlog.NewWithWriter(&discard{}, cfg.Get().Log), &http.Client{})
	router := buildRouter(p)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Errorf("upstream was called %d times; /admin/ must never be forwarded", got)
	}
}

func TestRouterSendsV1Upstream(t *testing.T) {
	var gotPath atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()

	cfg := testConfig(up.URL)
	p := NewProxy(cfg, testRegistry(t), reqlog.NewWithWriter(&discard{}, cfg.Get().Log), &http.Client{})
	router := buildRouter(p)

	rec := httptest.NewRecorder()
	body := `{"model":"claude-GLM-5.3","messages":[]}`
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got, _ := gotPath.Load().(string); got != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages", got)
	}
}

func TestRouterAnswersHelloProbeLocally(t *testing.T) {
	var upstreamCalls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	cfg := testConfig(up.URL)
	p := NewProxy(cfg, testRegistry(t), reqlog.NewWithWriter(&discard{}, cfg.Get().Log), &http.Client{})
	router := buildRouter(p)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/api/hello", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Errorf("upstream was called %d times; the probe must be answered locally", got)
	}
}

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

type discard struct{}

func (*discard) Write(p []byte) (int, error) { return len(p), nil }
