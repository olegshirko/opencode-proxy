package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"opencode-proxy/internal/config"
	"opencode-proxy/internal/reqlog"
)

// catalogServer answers the gateway's model list; a local copy of the helper
// that lives with the catalog package's own tests.
func catalogServer(t *testing.T, token string, body string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if token != "" && r.Header.Get("Authorization") != token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func modelsOf(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		ids = append(ids, m.ID)
	}
	return ids
}

func TestModelsEndpointUsesTheLiveCatalog(t *testing.T) {
	srv, _ := catalogServer(t, "Bearer tok", `{"data":[
		{"id":"glm-5.3","name":"GLM 5.3"},
		{"id":"brand-new","name":"Brand New"},
		{"id":"retired","name":"Retired"}]}`)

	cfg := testConfig("http://127.0.0.1:1", func(c *config.Config) {
		c.Models.CatalogURL = srv.URL + "/catalog"
	})
	reg := mustRegistry(t, `{"models":[
		{"id":"glm-5.3","picker":"claude-GLM-5.3","available":true},
		{"id":"retired","picker":"claude-Retired","available":false}]}`)
	p := NewProxy(cfg, reg, reqlog.NewWithWriter(&discard{}, cfg.Get().Log), &http.Client{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer tok")
	Router(p).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// The gateway list is served as it comes, including a model the snapshot
	// still marks unavailable: availability changes and the snapshot cannot
	// be trusted to hide a working model.
	got := modelsOf(t, rec)
	want := []string{"claude-GL-5.3", "claude-Brand-New", "claude-Retired"}
	if len(got) != len(want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("models = %v, want %v", got, want)
		}
	}
}

func TestModelsEndpointFallsBackToTheSnapshot(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:1", func(c *config.Config) {
		c.Models.CatalogURL = "http://127.0.0.1:1/unreachable"
	})
	p := NewProxy(cfg, testRegistry(t), reqlog.NewWithWriter(&discard{}, cfg.Get().Log), &http.Client{})

	rec := httptest.NewRecorder()
	Router(p).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even when the catalog is down", rec.Code)
	}
	if ids := modelsOf(t, rec); len(ids) != 2 {
		t.Fatalf("fallback models = %v, want the two from models.json", ids)
	}
}

func TestRequestForANewCatalogModelResolves(t *testing.T) {
	// A model the gateway added after models.json was generated must resolve
	// from the live catalog instead of falling through to the default.
	srv, _ := catalogServer(t, "", `{"data":[{"id":"brand-new","name":"Brand New"}]}`)

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}, func(c *config.Config) { c.Models.CatalogURL = srv.URL + "/catalog" })

	// Warm the catalog the way the app does, through the model list.
	rec := httptest.NewRecorder()
	Router(h.proxy).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("models status = %d", rec.Code)
	}

	h.do(t, http.MethodPost, "/v1/messages", `{"model":"claude-Brand-New","messages":[]}`, nil)
	got := h.record(t)
	if got.Why != "catalog" {
		t.Fatalf("resolution reason = %q, want catalog", got.Why)
	}
	if got.ModelOut != "brand-new" {
		t.Fatalf("resolved to %q, want brand-new", got.ModelOut)
	}
}
