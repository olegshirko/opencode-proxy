package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// catalogServer answers the gateway's model list and counts the requests, so
// a test can tell a cached answer from a fresh fetch.
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

func TestURLForDerivesFromUpstream(t *testing.T) {
	const upstream = "https://opencode.ai/inference/go/anthropic"
	if got := URLFor("", upstream); got != "https://opencode.ai/inference/go/v1/models" {
		t.Fatalf("derived catalog URL = %q", got)
	}
	if got := URLFor("http://example.test/models", upstream); got != "http://example.test/models" {
		t.Fatalf("explicit catalog URL = %q", got)
	}
}

func TestLiveCatalogIsCached(t *testing.T) {
	srv, hits := catalogServer(t, "", `{"data":[{"id":"glm-5.3","name":"GLM 5.3"}]}`)
	c := New()
	for i := 0; i < 3; i++ {
		if _, err := c.Get(context.Background(), srv.URL, &http.Client{}, ""); err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream fetches = %d, want 1: the catalog must be cached", got)
	}
	if id, ok := c.Lookup("claude-GL-5.3"); !ok || id != "glm-5.3" {
		t.Fatalf("Lookup = (%q, %v), want glm-5.3", id, ok)
	}
}
