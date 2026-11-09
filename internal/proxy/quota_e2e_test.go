package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"opencode-proxy/internal/config"
	"opencode-proxy/internal/quota"
)

func TestProxyEmitsQuotaHeadersOnSuccess(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"type":"message","usage":{"input_tokens":1,"output_tokens":1}}`))
	}, func(c *config.Config) {
		c.Limits = quota.Limits{MonthlyUSD: 100, WarningFraction: 0.85}
	})
	ledger, _ := quota.NewUsageLedger("")
	ledger.Add("glm-5.3", 18, time.Now())
	h.proxy.SetUsageLedger(ledger)

	rec := h.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-GLM-5.3","messages":[{"role":"user","content":"hi"}]}`, nil)

	if got := rec.Header().Get("anthropic-ratelimit-unified-5h-utilization"); got != "0.9000" {
		t.Errorf("5h utilization = %q, want 0.9000", got)
	}
	if got := rec.Header().Get("anthropic-ratelimit-unified-5h-status"); got != "allowed_warning" {
		t.Errorf("5h status = %q, want allowed_warning", got)
	}
	if got := rec.Header().Get("anthropic-ratelimit-unified-reset"); got == "" {
		t.Error("representative reset header missing")
	}
}

func TestProxyOmitsQuotaHeadersWithoutBudget(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"message"}`))
	})
	h.proxy.SetUsageLedger(mustLedger(t))

	rec := h.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-GLM-5.3","messages":[{"role":"user","content":"hi"}]}`, nil)

	if got := rec.Header().Get("anthropic-ratelimit-unified-5h-utilization"); got != "" {
		t.Errorf("quota header emitted without a budget: %q", got)
	}
}

func TestAuxiliaryRoutedToBackgroundModel(t *testing.T) {
	var seenModel string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var data map[string]any
		if err := json.Unmarshal(body, &data); err == nil {
			seenModel, _ = data["model"].(string)
		}
		w.Write([]byte(`{"type":"message"}`))
	}, func(c *config.Config) {
		c.Models.Background = "deepseek-v4.1-flash"
	})

	h.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-GLM-5.3","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"X-Claude-Code-Request-Class": "auxiliary"})
	rec := <-h.records

	if seenModel != "deepseek-v4.1-flash" {
		t.Errorf("upstream model = %q, want deepseek-v4.1-flash", seenModel)
	}
	if rec.Why != "background" {
		t.Errorf("why = %q, want background", rec.Why)
	}
}

func TestCompactionStaysOnRequestedModel(t *testing.T) {
	var seenModel string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var data map[string]any
		if err := json.Unmarshal(body, &data); err == nil {
			seenModel, _ = data["model"].(string)
		}
		w.Write([]byte(`{"type":"message"}`))
	}, func(c *config.Config) {
		c.Models.Background = "deepseek-v4.1-flash"
	})

	h.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-GLM-5.3","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"X-Claude-Code-Request-Class": "auxiliary", "X-Claude-Code-Compaction": "1"})

	if seenModel != "glm-5.3" {
		t.Errorf("upstream model = %q, want glm-5.3", seenModel)
	}
}

func TestProxyEmitsQuotaHeadersOnStream(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: {\"type\":\"message_start\"}\n\n")
	}, func(c *config.Config) {
		c.Limits = quota.Limits{MonthlyUSD: 100}
	})
	ledger, _ := quota.NewUsageLedger("")
	ledger.Add("glm-5.3", 18, time.Now())
	h.proxy.SetUsageLedger(ledger)

	front := httptest.NewServer(h.proxy)
	defer front.Close()
	resp, err := http.Post(front.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"glm-5.3","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("anthropic-ratelimit-unified-5h-utilization"); got != "0.9000" {
		t.Errorf("stream 5h utilization = %q, want 0.9000", got)
	}
	if got := resp.Header.Get("anthropic-ratelimit-unified-status"); got != "allowed_warning" {
		t.Errorf("stream status = %q, want allowed_warning", got)
	}
}

// mustLedger wires a memory-only ledger into the harness.
func mustLedger(t *testing.T) *quota.UsageLedger {
	t.Helper()
	l, err := quota.NewUsageLedger("")
	if err != nil {
		t.Fatal(err)
	}
	return l
}
