package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const fixture = `{"object":"list","data":[
 {"id":"glm-5.3-flash","name":"GLM 5.3 Flash",
  "capabilities":{"reasoning":{"effort":["low","high","max"]}},
  "limit":{"context":1000000},
  "cost":{"text":{"input":{"price":0.15,"cache":{"read":0.03}},"output":{"price":0.5}}}}]}`

func TestParseCatalog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(fixture))
	}))
	defer srv.Close()

	items, err := FetchCatalog(context.Background(), srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d models, want 1", len(items))
	}
	m := toModel(items[0])
	if m.ID != "glm-5.3-flash" {
		t.Errorf("id = %q", m.ID)
	}
	if m.Label != "GLM 5.3 Flash" {
		t.Errorf("label = %q", m.Label)
	}
	if m.Context != 1000000 {
		t.Errorf("context = %d", m.Context)
	}
	if m.MaxEffort != "max" {
		t.Errorf("max_effort = %q, want max", m.MaxEffort)
	}
	if !m.Supports1M {
		t.Error("supports_1m = false, want true for a 1M window")
	}
	if m.Pricing.Input != 0.15 || m.Pricing.Output != 0.5 || m.Pricing.CacheRead != 0.03 {
		t.Errorf("pricing = %+v", m.Pricing)
	}
}

func TestPickerNameFromID(t *testing.T) {
	cases := []struct{ id, name, want string }{
		{"glm-5.3-flash", "GLM 5.3 Flash", "claude-GL-5.3-Flash"},
		{"kimi-k2.7-code", "Kimi K2.7 Code", "claude-K27-Code"},
		// The display name does not spell the id, so the id is used.
		{"claude-haiku-5-5", "Claude Haiku 5.5", "claude-claude-haiku-5-5"},
		// Explicit short spellings win over both the name and the id.
		{"deepseek-v4-pro", "DeepSeek V4 Pro", "claude-DPV4-Pro"},
	}
	for _, c := range cases {
		if got := PickerName(c.id, c.name); got != c.want {
			t.Errorf("PickerName(%q, %q) = %q, want %q", c.id, c.name, got, c.want)
		}
	}
}

func TestPickerNameRoundTripsThroughLowercase(t *testing.T) {
	// Non-overridden pickers must survive the proxy's prefix-strip-and-lowercase
	// rule: a picked model has to land on its own upstream id.
	cases := []struct{ id, name string }{
		{"hy4", "Hy4"},
		{"hy4-preview", "Hy4 preview"},
		{"step-5-preview", "Step 5 Preview"},
		{"beam-free", "Beam Free"},
		{"claude-haiku-5-5", "Claude Haiku 5.5"},
	}
	for _, c := range cases {
		picker := PickerName(c.id, c.name)
		back := strings.ToLower(picker[len("claude-"):])
		if back != c.id {
			t.Errorf("picker %q lowers to %q, want %q", picker, back, c.id)
		}
	}
}

// TestPickerNamesAvoidVendorWords pins the desktop app's validation: a
// gateway route may not carry another vendor's name, so no picker spelling
// may contain one.
func TestPickerNamesAvoidVendorWords(t *testing.T) {
	ids := []string{
		"deepseek-v4.1-flash", "deepseek-v4-pro", "glm-5.3", "glm-5.2",
		"glm-5.3-flash", "gemini-4-argon", "kimi-k3", "kimi-k3.1-pro",
		"kimi-k2.7-code", "longcat-2.0", "mimo-v2.6-flash", "mimo-v2.6-pro",
		"minimax-m3", "minimax-m3.1-flash", "muse-spark-1.2-contributor",
		"muse-spark-1.3-contributor", "qwen3.7-plus", "qwen3.8-flash",
		"qwen3.8-max", "grok-4.7", "gpt-6-luna", "gpt-5.6-luna",
	}
	vendors := []string{
		"deepseek", "glm", "gemini", "kimi", "mimo", "minimax",
		"qwen", "grok", "longcat", "muse", "gpt",
	}
	sort.Strings(ids)
	for _, id := range ids {
		picker := strings.ToLower(PickerName(id, id))
		for _, v := range vendors {
			if strings.Contains(picker, v) {
				t.Errorf("PickerName(%q) = %q still names the vendor %q", id, picker, v)
			}
		}
	}
}

// TestPickerNamesAreUnique guards the picker table: two models must never
// share a picker spelling, or one of them becomes unreachable.
func TestPickerNamesAreUnique(t *testing.T) {
	ids := []string{
		"deepseek-v4.1-flash", "deepseek-v4-pro", "glm-5.3", "glm-5.2",
		"glm-5.3-flash", "gemini-4-argon", "kimi-k3", "kimi-k3.1-pro",
		"kimi-k2.7-code", "longcat-2.0", "mimo-v2.6-flash", "mimo-v2.6-pro",
		"minimax-m3", "minimax-m3.1-flash", "muse-spark-1.2-contributor",
		"muse-spark-1.3-contributor", "qwen3.7-plus", "qwen3.8-flash",
		"qwen3.8-max", "grok-4.7", "gpt-6-luna", "gpt-5.6-luna",
		"hy3", "hy4", "hy4-preview", "step-5-preview", "beam-free",
		"crouton-alpha-free", "iceman-contributor-free", "claude-haiku-5-5",
	}
	seen := make(map[string]string, len(ids))
	for _, id := range ids {
		picker := strings.ToLower(PickerName(id, id))
		if prev, dup := seen[picker]; dup {
			t.Errorf("picker %q is shared by %q and %q", picker, prev, id)
		}
		seen[picker] = id
	}
	if len(seen) != len(ids) {
		t.Errorf("got %d unique pickers for %d models", len(seen), len(ids))
	}
}

func TestMaxEffortPicksHighestSupported(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"low", "high", "max"}, "max"},
		{[]string{"high", "max"}, "max"},
		{[]string{"max"}, "max"},
		{[]string{"low", "medium"}, "medium"},
		{[]string{"none", "low"}, "low"},
		{[]string{"none"}, "low"},
		{nil, "low"},
	}
	for _, c := range cases {
		if got := MaxEffort(c.in); got != c.want {
			t.Errorf("MaxEffort(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestProbeReportsUnavailableOnError(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusForbidden, http.StatusNotFound} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		ok := Probe(context.Background(), srv.URL, "token", model{ID: "glm-5.3"}, srv.Client())
		srv.Close()
		if ok {
			t.Errorf("Probe with HTTP %d reported available", status)
		}
	}
}

func TestProbeReportsAvailableOnSuccess(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode probe body: %v", err)
		}
		gotModel = body.Model
		if r.Header.Get("x-opencode-session") == "" {
			t.Error("probe must send x-opencode-session")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	if !Probe(context.Background(), srv.URL, "tok", model{ID: "glm-5.3"}, srv.Client()) {
		t.Fatal("Probe reported unavailable for a 200")
	}
	if gotModel != "glm-5.3" {
		t.Errorf("probe sent model %q", gotModel)
	}
}

func TestRunWritesTheCatalog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[
			{"id":"glm-5.3","name":"GLM 5.3","limit":{"context":1000000},
			 "capabilities":{"reasoning":{"effort":["none","high"]}},
			 "cost":{"text":{"input":{"price":0.7,"cache":{"read":0.07}},"output":{"price":2.8}}}},
			{"id":"small-model","name":"Small Model","limit":{"context":256000},
			 "capabilities":{"reasoning":{"effort":["none"]}},
			 "cost":{"text":{"input":{"price":0.1},"output":{"price":0.2}}}}
		]}`))
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "models.json")
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), options{out: out, catalogURL: srv.URL, probeURL: srv.URL},
		&http.Client{}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got modelFile
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode %s: %v", out, err)
	}
	if len(got.Models) != 2 {
		t.Fatalf("models = %d, want 2", len(got.Models))
	}
	if got.Models[0].ID != "glm-5.3" {
		t.Fatalf("models are not sorted by label: first is %q", got.Models[0].ID)
	}
	big := got.Models[0]
	if big.Picker != "claude-GL-5.3" {
		t.Fatalf("picker = %q, want claude-GL-5.3", big.Picker)
	}
	if big.BehavesAs != "claude-sonnet-5-5" || !big.Supports1M {
		t.Fatalf("1M model = %+v", big)
	}
	if big.MaxEffort != "high" {
		t.Fatalf("max effort = %q, want high", big.MaxEffort)
	}
	small := got.Models[1]
	if small.BehavesAs != "claude-sonnet-5" || small.Supports1M {
		t.Fatalf("256k model = %+v", small)
	}
	if !strings.Contains(stdout.String(), "2 models, 2 available") {
		t.Fatalf("summary = %q", stdout.String())
	}
}

// Without -probe the generator must keep the availability each model already
// has in models.json: the field comes from the regional probe, which an
// unprobed run cannot re-check. Marking everything available would put
// region-blocked models back into the picker.
func TestRunPreservesAvailabilityWithoutProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[
			{"id":"glm-5.3","name":"GLM 5.3","limit":{"context":256000},
			 "capabilities":{"reasoning":{"effort":["none"]}},
			 "cost":{"text":{"input":{"price":0.7},"output":{"price":2.8}}}},
			{"id":"brand-new","name":"Brand New","limit":{"context":256000},
			 "capabilities":{"reasoning":{"effort":["none"]}},
			 "cost":{"text":{"input":{"price":0.1},"output":{"price":0.2}}}}
		]}`))
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "models.json")
	existing := `{"models":[
		{"id":"glm-5.3","picker":"claude-GL-5.3","label":"GLM 5.3","behaves_as":"claude-sonnet-5","available":false}
	]}`
	if err := os.WriteFile(out, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), options{out: out, catalogURL: srv.URL, probeURL: srv.URL},
		&http.Client{}, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v", err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got modelFile
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 2 {
		t.Fatalf("models = %d, want 2", len(got.Models))
	}
	byID := map[string]model{}
	for _, m := range got.Models {
		byID[m.ID] = m
	}
	if m := byID["glm-5.3"]; m.Available {
		t.Fatalf("glm-5.3 available = %v, want false carried over from the previous file", m.Available)
	}
	if m := byID["brand-new"]; !m.Available {
		t.Fatalf("brand-new available = %v, want true (unknown models start available)", m.Available)
	}
}

func TestRunFailsOnACatalogError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "models.json")
	err := run(context.Background(), options{out: out, catalogURL: srv.URL},
		&http.Client{}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("run accepted a failing catalog")
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Fatal("run wrote a file despite the catalog error")
	}
}

func TestRunRequiresATokenForProbing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"glm-5.3","name":"GLM 5.3"}]}`))
	}))
	defer srv.Close()
	t.Setenv("OPENCODE_TOKEN", "")

	err := run(context.Background(), options{
		out: filepath.Join(t.TempDir(), "models.json"), catalogURL: srv.URL, probe: true,
	}, &http.Client{}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "needs OPENCODE_TOKEN") {
		t.Fatalf("err = %v, want a missing-token error", err)
	}
}

func TestReadToken(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(nested, []byte(`{"env":{"ANTHROPIC_AUTH_TOKEN":"secret"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readToken(nested); got != "secret" {
		t.Fatalf("readToken(env) = %q, want secret", got)
	}

	flat := filepath.Join(dir, "flat.json")
	if err := os.WriteFile(flat, []byte(`{"ANTHROPIC_AUTH_TOKEN":"flat"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readToken(flat); got != "flat" {
		t.Fatalf("readToken(top level) = %q, want flat", got)
	}

	if got := readToken(filepath.Join(dir, "missing.json")); got != "" {
		t.Fatalf("readToken(missing) = %q, want an empty token", got)
	}
}

func TestRunMainWritesTheFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"glm-5.3","name":"GLM 5.3","limit":{"context":1000000}}]}`))
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "models.json")
	var stdout, stderr bytes.Buffer
	code := runMain([]string{"-out", out, "-catalog", srv.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("no output written: %v", err)
	}
}

func TestRunMainReportsACatalogFailure(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runMain([]string{"-out", filepath.Join(t.TempDir(), "m.json"),
		"-catalog", "http://127.0.0.1:1/none"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "genmodels:") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunMainRejectsAnUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runMain([]string{"-nope"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}
