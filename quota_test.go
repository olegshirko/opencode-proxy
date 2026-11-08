package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestUsageSnapshotWindows(t *testing.T) {
	// Thursday 19 Nov 2026, 12:00 UTC: the week started Monday the 16th.
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	ledger, err := NewUsageLedger("")
	if err != nil {
		t.Fatal(err)
	}
	ledger.Add("m", 1.0, now.Add(-8*time.Hour))  // opened a 5-hour window that closed at -3h
	ledger.Add("m", 2.0, now.Add(-2*time.Hour))  // opened the current one
	ledger.Add("m", 4.0, now.Add(-1*time.Hour))  // inside it
	ledger.Add("m", 8.0, now.AddDate(0, 0, -4))  // last week, inside the month
	ledger.Add("m", 16.0, now.AddDate(0, -2, 0)) // outside everything

	limits := LimitsConfig{MonthlyUSD: 100}
	s := ledger.Snapshot(limits, now)

	if s.FiveHourUSD != 6.0 {
		t.Errorf("five hour = %v, want 6 (the closed window's $1 must not count)", s.FiveHourUSD)
	}
	if want := now.Add(-2 * time.Hour).Add(fiveHourWindowLength); !s.FiveHourReset.Equal(want) {
		t.Errorf("five hour reset = %v, want %v", s.FiveHourReset, want)
	}
	if s.WeeklyUSD != 7.0 {
		t.Errorf("weekly = %v, want 7 (the 4-day-old charge is last week)", s.WeeklyUSD)
	}
	if want := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC); !s.WeeklyReset.Equal(want) {
		t.Errorf("weekly reset = %v, want %v", s.WeeklyReset, want)
	}
	if s.MonthlyUSD != 15.0 {
		t.Errorf("monthly = %v, want 15", s.MonthlyUSD)
	}
	if want := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC); !s.MonthReset.Equal(want) {
		t.Errorf("month reset = %v, want %v", s.MonthReset, want)
	}
	if s.FiveHourLimit != 20 || s.WeeklyLimit != 50 || s.MonthlyLimit != 100 {
		t.Errorf("limits = %v/%v/%v, want 20/50/100", s.FiveHourLimit, s.WeeklyLimit, s.MonthlyLimit)
	}
}

func TestMonthWindowAnchorAdvances(t *testing.T) {
	anchor := "2026-11-06T21:00:00+03:00"
	now := time.Date(2026, 12, 10, 12, 0, 0, 0, time.Local)
	start, reset := monthWindow(now, anchor)
	if want := time.Date(2026, 12, 6, 21, 0, 0, 0, time.FixedZone("MSK", 3*3600)); !start.Equal(want) {
		t.Errorf("start = %v, want %v", start, want)
	}
	if want := time.Date(2027, 1, 6, 21, 0, 0, 0, time.FixedZone("MSK", 3*3600)); !reset.Equal(want) {
		t.Errorf("reset = %v, want %v", reset, want)
	}

	// An anchor still ahead of now must not move the window backwards.
	early, _ := monthWindow(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), anchor)
	if !early.Equal(time.Date(2026, 11, 6, 21, 0, 0, 0, time.FixedZone("MSK", 3*3600))) {
		t.Errorf("early start = %v, want the anchor itself", early)
	}
}

func TestUsageSnapshotEmptyWindowsResetFromNow(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.Local)
	ledger, _ := NewUsageLedger("")
	s := ledger.Snapshot(LimitsConfig{MonthlyUSD: 10}, now)

	if !s.FiveHourReset.Equal(now.Add(fiveHourWindowLength)) {
		t.Errorf("five hour reset = %v, want now+5h", s.FiveHourReset)
	}
	_, weekReset := weekWindow(now)
	if !s.WeeklyReset.Equal(weekReset) {
		t.Errorf("weekly reset = %v, want %v", s.WeeklyReset, weekReset)
	}
}

func TestQuotaHeaders(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.Local)
	s := UsageSnapshot{
		MonthlyLimit:  100,
		FiveHourLimit: 20,
		WeeklyLimit:   50,
		FiveHourUSD:   18, // 90% of the 5-hour window
		WeeklyUSD:     10, // 20% of the week
		Warning:       0.85,
		FiveHourReset: now.Add(time.Hour),
		WeeklyReset:   now.Add(48 * time.Hour),
	}
	h := http.Header{}
	s.Apply(h)

	cases := map[string]string{
		"anthropic-ratelimit-unified-5h-utilization":         "0.9000",
		"anthropic-ratelimit-unified-5h-status":              "allowed_warning",
		"anthropic-ratelimit-unified-5h-surpassed-threshold": "true",
		"anthropic-ratelimit-unified-5h-reset":               strconv.FormatInt(now.Add(time.Hour).Unix(), 10),
		"anthropic-ratelimit-unified-7d-utilization":         "0.2000",
		"anthropic-ratelimit-unified-7d-status":              "allowed",
		"anthropic-ratelimit-unified-7d-surpassed-threshold": "false",
		"anthropic-ratelimit-unified-status":                 "allowed_warning",
		"anthropic-ratelimit-unified-representative-claim":   "five_hour",
	}
	for name, want := range cases {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestQuotaHeadersDisabledWithoutBudget(t *testing.T) {
	h := http.Header{}
	UsageSnapshot{FiveHourUSD: 5}.Apply(h)
	if len(h) != 0 {
		t.Errorf("headers emitted without a monthly budget: %v", h)
	}
}

func TestProxyEmitsQuotaHeadersOnSuccess(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"type":"message","usage":{"input_tokens":1,"output_tokens":1}}`))
	}, func(c *Config) {
		c.Limits = LimitsConfig{MonthlyUSD: 100, WarningFraction: 0.85}
	})
	ledger, _ := NewUsageLedger("")
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
	}, func(c *Config) {
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
	}, func(c *Config) {
		c.Models.Background = "deepseek-v4.1-flash"
	})

	h.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-GLM-5.3","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"X-Claude-Code-Request-Class": "auxiliary", "X-Claude-Code-Compaction": "1"})

	if seenModel != "glm-5.3" {
		t.Errorf("upstream model = %q, want glm-5.3", seenModel)
	}
}

func TestUsageLedgerPersistsAndPrunes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	ledger, err := NewUsageLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ledger.Add("m", 1.5, now)
	ledger.Add("m", 9.9, now.Add(-60*24*time.Hour)) // beyond retention
	if err := ledger.Flush(); err != nil {
		t.Fatal(err)
	}

	back, err := NewUsageLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	s := back.Snapshot(LimitsConfig{MonthlyUSD: 100}, now)
	if s.FiveHourUSD != 1.5 {
		t.Errorf("reloaded five hour = %v, want 1.5 (old entry must be pruned)", s.FiveHourUSD)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("ledger file missing: %v", err)
	}
}

func mustLedger(t *testing.T) *UsageLedger {
	t.Helper()
	l, err := NewUsageLedger("")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestProxyEmitsQuotaHeadersOnStream(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: {\"type\":\"message_start\"}\n\n")
	}, func(c *Config) {
		c.Limits = LimitsConfig{MonthlyUSD: 100}
	})
	ledger, _ := NewUsageLedger("")
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
