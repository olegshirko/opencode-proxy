package websearch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeCLI writes a script that stands in for the opencode binary.
func fakeCLI(t *testing.T, stdout string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake opencode CLI is a POSIX shell script; windows coverage lands with phase 2")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode")
	script := "#!/bin/sh\ncat <<'EOF'\n" + stdout + "\nEOF\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OC_CLI", path)
}

func TestSearchParsesResults(t *testing.T) {
	fakeCLI(t, `{"data":{"providerID":"tinyfish","results":[
		{"url":"https://example.com/a","title":" A ","content":" first hit "},
		{"url":"https://example.com/b","title":"B","content":"second hit"}]}}`)

	res, err := Search(context.Background(), "  query  ", "", 0)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if res.ProviderID != "tinyfish" {
		t.Errorf("provider = %q", res.ProviderID)
	}
	if len(res.Results) != 2 {
		t.Fatalf("got %d results, want 2", len(res.Results))
	}
	if res.Results[0].Title != "A" || res.Results[0].URL != "https://example.com/a" {
		t.Errorf("first result = %+v", res.Results[0])
	}
	if res.Results[0].Snippet != " first hit " {
		t.Errorf("snippet was rewritten: %q", res.Results[0].Snippet)
	}
}

func TestSearchAppliesLimit(t *testing.T) {
	fakeCLI(t, `{"data":{"providerID":"exa","results":[
		{"url":"https://example.com/a","title":"A","content":"one"},
		{"url":"https://example.com/b","title":"B","content":"two"},
		{"url":"https://example.com/c","title":"C","content":"three"}]}}`)

	res, err := Search(context.Background(), "query", "exa", 2)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(res.Results) != 2 {
		t.Fatalf("got %d results, want 2", len(res.Results))
	}
}

func TestSearchCapsLimit(t *testing.T) {
	var hits []string
	for i := 0; i < MaxResults+2; i++ {
		hits = append(hits, `{"url":"https://example.com/x","title":"X","content":"hit"}`)
	}
	fakeCLI(t, `{"data":{"providerID":"exa","results":[`+strings.Join(hits, ",")+`]}}`)

	res, err := Search(context.Background(), "query", "exa", 1000)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(res.Results) != MaxResults {
		t.Fatalf("got %d results, want %d", len(res.Results), MaxResults)
	}
}

func TestSearchReportsProviderError(t *testing.T) {
	fakeCLI(t, `{"_tag":"ServiceUnavailableError","message":"Web search request failed: exa","service":"exa"}`)

	_, err := Search(context.Background(), "query", "exa", 0)
	if err == nil {
		t.Fatal("provider error was not reported")
	}
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) {
		t.Fatalf("error = %T, want *ProviderError", err)
	}
	if !strings.Contains(err.Error(), "Web search request failed: exa") {
		t.Errorf("provider message missing: %v", err)
	}
}

func TestSearchRejectsEmptyQuery(t *testing.T) {
	if _, err := Search(context.Background(), "   ", "", 0); err == nil {
		t.Error("an empty query was accepted")
	}
}

func TestSearchReportsBrokenAnswer(t *testing.T) {
	fakeCLI(t, `not json`)

	if _, err := Search(context.Background(), "query", "", 0); err == nil {
		t.Error("a broken answer was accepted")
	}
}
