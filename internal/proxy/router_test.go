package proxy

import (
	"net/http"
	"net/http/httptest"
	"opencode-proxy/internal/reqlog"
	"strings"
	"sync/atomic"
	"testing"
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
	router := Router(p)

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
	router := Router(p)

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
	router := Router(p)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/api/hello", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Errorf("upstream was called %d times; the probe must be answered locally", got)
	}
}
