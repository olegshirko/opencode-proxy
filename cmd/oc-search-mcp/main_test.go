package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestHandleInitialize(t *testing.T) {
	resp := handle(rpcRequest{ID: json.RawMessage(`1`), Method: "initialize",
		Params: json.RawMessage(`{"protocolVersion":"2024-11-05"}`)})
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result = %T, want map", resp.Result)
	}
	if result["protocolVersion"] != "2024-11-05" {
		t.Errorf("protocolVersion = %v", result["protocolVersion"])
	}
	if _, ok := result["capabilities"].(map[string]any)["tools"]; !ok {
		t.Error("tools capability missing")
	}
}

func TestHandleUnknownMethod(t *testing.T) {
	resp := handle(rpcRequest{ID: json.RawMessage(`9`), Method: "nope"})
	if resp.Error == nil || resp.Error.Code != -32601 {
		t.Fatalf("error = %+v, want -32601", resp.Error)
	}
}

func TestTrimText(t *testing.T) {
	if got := trimText("  a\n\tb  ", 10); got != "a b" {
		t.Errorf("whitespace was not collapsed: %q", got)
	}
	got := trimText(strings.Repeat("я", 20), 5)
	if got != "яяяяя…" {
		t.Errorf("trim = %q", got)
	}
}

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

func TestSearchFormatsResults(t *testing.T) {
	fakeCLI(t, `{"data":{"providerID":"tinyfish","results":[
		{"url":"https://example.com/a","title":"A","content":"first hit"},
		{"url":"https://example.com/b","title":"B","content":"second hit"},
		{"url":"https://example.com/c","title":"C","content":"third hit"}]}}`)

	text, isErr := search("query", "", 2)
	if isErr {
		t.Fatalf("search failed: %s", text)
	}
	if !strings.Contains(text, "2 result(s)") {
		t.Errorf("limit not applied:\n%s", text)
	}
	if strings.Contains(text, "third hit") {
		t.Errorf("result beyond the limit leaked:\n%s", text)
	}
	if !strings.Contains(text, "https://example.com/a") || !strings.Contains(text, "first hit") {
		t.Errorf("result body missing:\n%s", text)
	}
}

func TestSearchReportsProviderError(t *testing.T) {
	fakeCLI(t, `{"_tag":"ServiceUnavailableError","message":"Web search request failed: exa","service":"exa"}`)

	text, isErr := search("query", "exa", 0)
	if !isErr {
		t.Fatalf("provider error was not reported: %s", text)
	}
	if !strings.Contains(text, "Web search request failed: exa") {
		t.Errorf("provider message missing: %s", text)
	}
}

func TestSearchRejectsEmptyQuery(t *testing.T) {
	if _, isErr := search("   ", "", 0); !isErr {
		t.Error("an empty query was accepted")
	}
}
