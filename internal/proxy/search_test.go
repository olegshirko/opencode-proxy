package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSearchCLI writes a script that stands in for the opencode binary.
func fakeSearchCLI(t *testing.T, stdout string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode")
	script := "#!/bin/sh\ncat <<'EOF'\n" + stdout + "\nEOF\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OC_CLI", path)
}

func TestSearchHandlerReturnsAppShape(t *testing.T) {
	fakeSearchCLI(t, `{"data":{"providerID":"tinyfish","results":[
		{"url":"https://example.com/a","title":"A","content":"first hit"}]}}`)

	req := httptest.NewRequest(http.MethodPost, "/search", strings.NewReader(`{"q":"even realities"}`))
	rec := httptest.NewRecorder()
	SearchHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Provider string      `json:"provider"`
		Results  []searchHit `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body.Provider != "tinyfish" {
		t.Errorf("provider = %q", body.Provider)
	}
	if len(body.Results) != 1 || body.Results[0].URL != "https://example.com/a" {
		t.Fatalf("results = %+v", body.Results)
	}
	if body.Results[0].Snippet != "first hit" {
		t.Errorf("snippet = %q", body.Results[0].Snippet)
	}
}

func TestSearchHandlerAcceptsQuerySpelling(t *testing.T) {
	fakeSearchCLI(t, `{"data":{"providerID":"exa","results":[]}}`)

	req := httptest.NewRequest(http.MethodPost, "/search", strings.NewReader(`{"query":"x"}`))
	rec := httptest.NewRecorder()
	SearchHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestSearchHandlerRejectsEmptyQuery(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/search", strings.NewReader(`{"q":"  "}`))
	rec := httptest.NewRecorder()
	SearchHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestSearchHandlerRejectsBrokenBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/search", strings.NewReader(`{`))
	rec := httptest.NewRecorder()
	SearchHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestSearchHandlerReportsProviderFailure(t *testing.T) {
	fakeSearchCLI(t, `{"_tag":"ServiceUnavailableError","message":"web search failed"}`)

	req := httptest.NewRequest(http.MethodPost, "/search", strings.NewReader(`{"q":"x"}`))
	rec := httptest.NewRecorder()
	SearchHandler(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
}
