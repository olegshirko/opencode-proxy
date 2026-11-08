package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// routedHarness builds a proxy whose config sends big-pickle to a second
// endpoint with its own token, the way models outside the subscription work.
func routedHarness(t *testing.T, zen http.HandlerFunc) (*harness, *httptest.Server) {
	t.Helper()
	zenSrv := httptest.NewServer(zen)
	t.Cleanup(zenSrv.Close)

	tokenPath := filepath.Join(t.TempDir(), "zen-token")
	if err := os.WriteFile(tokenPath, []byte("zen-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}, func(c *Config) {
		c.Models.Routes = []ModelRoute{{
			ID:        "big-pickle",
			Picker:    "claude-Big-Pickle",
			Label:     "Big Pickle",
			BaseURL:   zenSrv.URL,
			TokenFile: tokenPath,
		}}
	})
	return h, zenSrv
}

func TestRoutedModelGoesToItsOwnEndpoint(t *testing.T) {
	var (
		mu      sync.Mutex
		path    string
		auth    string
		model   string
		gotReqs int
	)
	zen := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotReqs++
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		model = resolveBodyModel(r)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}
	h, _ := routedHarness(t, zen)

	h.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-Big-Pickle","messages":[]}`,
		map[string]string{"Authorization": "Bearer subscription-token"})

	mu.Lock()
	defer mu.Unlock()
	if gotReqs != 1 {
		t.Fatalf("the routed endpoint saw %d requests, want 1", gotReqs)
	}
	if path != "/v1/messages" {
		t.Fatalf("path = %q, want /v1/messages", path)
	}
	if model != "big-pickle" {
		t.Fatalf("upstream model = %q, want big-pickle", model)
	}
	if auth != "Bearer zen-token" {
		t.Fatalf("authorization = %q, want the route's token, not the client's", auth)
	}

	rec := h.record(t)
	if rec.Why != "route" {
		t.Fatalf("resolution reason = %q, want route", rec.Why)
	}
	if rec.ModelOut != "big-pickle" {
		t.Fatalf("resolved to %q, want big-pickle", rec.ModelOut)
	}
}

func TestRoutedModelIsListed(t *testing.T) {
	h, _ := routedHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"glm-5.3","name":"GLM 5.3"}]}`))
	})

	rec := httptest.NewRecorder()
	buildRouter(h.proxy).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	ids := modelsOf(t, rec)
	found := false
	for _, id := range ids {
		if id == "claude-Big-Pickle" {
			found = true
		}
	}
	if !found {
		t.Fatalf("models = %v, want claude-Big-Pickle among them", ids)
	}
}

// resolveBodyModel reads the model field of a request body.
func resolveBodyModel(r *http.Request) string {
	data, _ := io.ReadAll(r.Body)
	var body struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(data, &body)
	return body.Model
}
