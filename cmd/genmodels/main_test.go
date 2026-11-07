package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		{"glm-5.3-flash", "GLM 5.3 Flash", "claude-GLM-5.3-Flash"},
		{"kimi-k2.7-code", "Kimi K2.7 Code", "claude-Kimi-K2.7-Code"},
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
	// The proxy strips the "claude-" prefix and lowercases; that must give the
	// native id back, otherwise a picked model lands on the wrong upstream id.
	cases := []struct{ id, name string }{
		{"glm-5.3-flash", "GLM 5.3 Flash"},
		{"deepseek-v4.1-flash", "DeepSeek V4.1 Flash"},
		{"qwen3.8-max", "Qwen3.8 Max"},
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
