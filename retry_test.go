package main

import (
	"math/rand"
	"net/http"
	"testing"
	"time"
)

// testRetryConfig mirrors the defaults shipped in config.json.
func testRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts:   5,
		FirstWaitMS:   2000,
		Multiplier:    2,
		MaxWaitMS:     15000,
		TotalBudgetMS: 60000,
	}
}

func TestShouldRetry(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{429, true},
		{500, true},
		{599, true},
		{400, false},
		{404, false},
		{401, false},
		{200, false},
	}
	for _, c := range cases {
		if got := ShouldRetry(c.status); got != c.want {
			t.Errorf("ShouldRetry(%d) = %v, want %v", c.status, got, c.want)
		}
	}
}

// x-should-retry is the upstream's own verdict and outranks the status-code
// heuristic; an absent or unparseable header falls back to the heuristic.
func TestShouldRetryResponseHonoursUpstreamHeader(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header string
		want   bool
	}{
		{"429 without header", 429, "", true},
		{"429 denied by upstream", 429, "false", false},
		{"500 denied by upstream", 500, "false", false},
		{"500 without header", 500, "", true},
		{"404 without header", 404, "", false},
		{"404 forced by upstream", 404, "true", true},
		{"400 forced by upstream", 400, "true", true},
		{"header is case-insensitive", 400, "TRUE", true},
		{"garbage falls back", 404, "maybe", false},
		{"garbage falls back retryable", 502, "maybe", true},
	}
	for _, c := range cases {
		h := http.Header{}
		if c.header != "" {
			h.Set("X-Should-Retry", c.header)
		}
		resp := &http.Response{StatusCode: c.status, Header: h}
		if got := ShouldRetryResponse(resp); got != c.want {
			t.Errorf("%s: ShouldRetryResponse(%d, %q) = %v, want %v", c.name, c.status, c.header, got, c.want)
		}
	}
}

func TestWaitForGrows(t *testing.T) {
	p := NewRetryPolicy(testRetryConfig(), rand.New(rand.NewSource(42)))

	check := func(attempt int, lo, hi time.Duration) {
		t.Helper()
		for i := 0; i < 200; i++ {
			got := p.WaitFor(attempt, 0)
			if got < lo || got > hi {
				t.Fatalf("attempt %d wait = %v, want within [%v, %v]", attempt, got, lo, hi)
			}
		}
	}
	// Base 2000ms at attempt 1, 2000*2^2 at attempt 3, each jittered by ±20%.
	check(1, 1600*time.Millisecond, 2400*time.Millisecond)
	check(3, 6400*time.Millisecond, 9600*time.Millisecond)
}

func TestWaitForCapsAtMax(t *testing.T) {
	p := NewRetryPolicy(testRetryConfig(), rand.New(rand.NewSource(7)))

	for i := 0; i < 200; i++ {
		// 2000*2^5 = 64000ms is capped to 15000ms, then jittered by ±20%.
		got := p.WaitFor(6, 0)
		if got < 12000*time.Millisecond || got > 18000*time.Millisecond {
			t.Fatalf("attempt 6 wait = %v, want within [12000ms, 18000ms]", got)
		}
	}
}

func TestWaitForHonoursRetryAfterWithinCap(t *testing.T) {
	p := NewRetryPolicy(testRetryConfig(), rand.New(rand.NewSource(99)))

	for i := 0; i < 200; i++ {
		// 40s is clamped to the 15s cap before jitter.
		if got := p.WaitFor(1, 40*time.Second); got < 12000*time.Millisecond || got > 18000*time.Millisecond {
			t.Fatalf("retryAfter 40s wait = %v, want within [12000ms, 18000ms]", got)
		}
		// A shorter upstream delay is used as-is, then jittered.
		if got := p.WaitFor(1, 3*time.Second); got < 2400*time.Millisecond || got > 3600*time.Millisecond {
			t.Fatalf("retryAfter 3s wait = %v, want within [2400ms, 3600ms]", got)
		}
	}
}

func TestParsePrefillHint(t *testing.T) {
	cases := []struct {
		name string
		body string
		want time.Duration
	}{
		{"seconds with space", "Try again (after 30 s)", 30 * time.Second},
		{"seconds without space", "(after 45s)", 45 * time.Second},
		{"no hint", "gateway is at capacity", 0},
		{"unparseable hint", "(after soon)", 0},
		{"empty body", "", 0},
	}
	for _, c := range cases {
		if got := ParsePrefillHint([]byte(c.body)); got != c.want {
			t.Errorf("%s: ParsePrefillHint(%q) = %v, want %v", c.name, c.body, got, c.want)
		}
	}
}

func TestNewRetryPolicy(t *testing.T) {
	p := NewRetryPolicy(testRetryConfig(), nil)
	if p.MaxAttempts != 5 || p.FirstMS != 2000 || p.MaxMS != 15000 || p.BudgetMS != 60000 || p.Multiplier != 2 {
		t.Fatalf("constructor did not map config fields: %+v", p)
	}
	if p.Jitter != 0.2 {
		t.Fatalf("Jitter = %v, want 0.2", p.Jitter)
	}
	if p.rand == nil {
		t.Fatal("nil rnd must be replaced with a seeded rand")
	}
	if got := p.WaitFor(1, 0); got < 1600*time.Millisecond || got > 2400*time.Millisecond {
		t.Fatalf("wait = %v, want within [1600ms, 2400ms]", got)
	}
}
