package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------- harness ----------

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	data := `{"models":[
		{"id":"deepseek-v4.1-flash","picker":"claude-DeepSeek-v4.1-Flash","available":true},
		{"id":"glm-5.3","picker":"claude-GLM-5.3","available":true}
	]}`
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := LoadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func testConfig(upstream string, mutate ...func(*Config)) *ConfigLoader {
	cfg := Defaults()
	cfg.Upstream = upstream
	cfg.Log.File = ""
	cfg.Log.MaxBodyBytes = 1 << 20
	cfg.SessionFallback = "fallback-session"
	cfg.Retry = RetryConfig{MaxAttempts: 5, FirstWaitMS: 1, Multiplier: 2, MaxWaitMS: 50, TotalBudgetMS: 5000}
	cfg.Models = ModelsConfig{
		Default:    "deepseek-v4.1-flash",
		Background: "deepseek-v4.1-flash",
		Aliases:    map[string]string{},
	}
	for _, fn := range mutate {
		fn(&cfg)
	}
	return &ConfigLoader{cfg: cfg}
}

func withRetry(rc RetryConfig) func(*Config) {
	return func(c *Config) { c.Retry = rc }
}

type harness struct {
	proxy    *Proxy
	upstream *httptest.Server
	logs     *bytes.Buffer
	records  chan RequestRecord
}

func newHarness(t *testing.T, handler http.HandlerFunc, mutate ...func(*Config)) *harness {
	t.Helper()
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)

	cfg := testConfig(up.URL, mutate...)
	logs := &bytes.Buffer{}
	logger := newLoggerWithWriter(logs, cfg.Get().Log)
	p := NewProxy(cfg, testRegistry(t), logger, &http.Client{})

	h := &harness{proxy: p, upstream: up, logs: logs, records: make(chan RequestRecord, 32)}
	p.onRecord = func(rec RequestRecord) { h.records <- rec }
	return h
}

func (h *harness) do(t *testing.T, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	h.proxy.ServeHTTP(rec, req)
	return rec
}

func (h *harness) record(t *testing.T) RequestRecord {
	t.Helper()
	select {
	case rec := <-h.records:
		return rec
	case <-time.After(3 * time.Second):
		t.Fatal("no request record logged")
		return RequestRecord{}
	}
}

func waitFor(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func closeEnough(a, b float64) bool {
	return math.Abs(a-b) < 1e-12
}

func masked(h map[string][]string, key string) bool {
	v, ok := h[key]
	return ok && len(v) == 1 && v[0] == "***"
}

// ---------- model rewrite and session header ----------

func TestRewritesModelBeforeSending(t *testing.T) {
	got := make(chan map[string]any, 1)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			got <- req
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})

	res := h.do(t, http.MethodPost, "/v1/messages", `{"model":"claude-GLM-5.3","messages":[]}`, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if gotBody := res.Body.String(); gotBody != `{"ok":true}` {
		t.Fatalf("client body = %q, want upstream body", gotBody)
	}
	select {
	case req := <-got:
		if req["model"] != "glm-5.3" {
			t.Fatalf("upstream model = %v, want glm-5.3", req["model"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("upstream never saw the request")
	}

	rec := h.record(t)
	if rec.ModelIn != "claude-GLM-5.3" || rec.ModelOut != "glm-5.3" || rec.Why != "picker" {
		t.Fatalf("record models = %q -> %q (%s)", rec.ModelIn, rec.ModelOut, rec.Why)
	}
}

func TestNonJSONBodyPassesThrough(t *testing.T) {
	got := make(chan []byte, 1)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- body
		w.Write([]byte("ok"))
	})

	const body = "not json"
	res := h.do(t, http.MethodPost, "/v1/messages", body, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	select {
	case b := <-got:
		if string(b) != body {
			t.Fatalf("upstream body = %q, want %q", b, body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("upstream never saw the request")
	}
}

func TestSessionHeaderSent(t *testing.T) {
	got := make(chan string, 1)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("x-opencode-session")
	})

	h.do(t, http.MethodPost, "/v1/messages", `{"model":"glm-5.3","messages":[]}`, map[string]string{
		"X-Claude-Code-Session-Id": "session-abc",
	})
	if s := <-got; s != "s:session-abc" {
		t.Fatalf("x-opencode-session = %q, want s:session-abc", s)
	}
}

func TestSessionFallbackWhenHeaderMissing(t *testing.T) {
	got := make(chan string, 1)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("x-opencode-session")
	}, func(c *Config) { c.SessionFallback = "fallback-1" })

	h.do(t, http.MethodPost, "/v1/messages", `{"model":"glm-5.3","messages":[]}`, nil)
	if s := <-got; s != "s:fallback-1" {
		t.Fatalf("x-opencode-session = %q, want s:fallback-1", s)
	}
}

// ---------- retries ----------

func TestRetriesOn429ThenSucceeds(t *testing.T) {
	var calls atomic.Int64
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte("busy"))
			return
		}
		w.Write([]byte("ok"))
	}, withRetry(RetryConfig{MaxAttempts: 5, FirstWaitMS: 10, Multiplier: 2, MaxWaitMS: 100, TotalBudgetMS: 5000}))

	res := h.do(t, http.MethodPost, "/v1/messages", `{"model":"glm-5.3","messages":[]}`, nil)
	if res.Code != http.StatusOK || res.Body.String() != "ok" {
		t.Fatalf("client got %d %q, want 200 ok", res.Code, res.Body.String())
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
	if rec := h.record(t); rec.Attempt != 2 {
		t.Fatalf("record attempt = %d, want 2", rec.Attempt)
	}
	stats := h.proxy.Stats()
	if stats.Requests != 1 || stats.Retries != 1 || stats.Upstream429 != 1 {
		t.Fatalf("stats = %+v, want requests=1 retries=1 upstream429=1", stats)
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	var calls atomic.Int64
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("busy"))
	}, withRetry(RetryConfig{MaxAttempts: 3, FirstWaitMS: 1, Multiplier: 1, MaxWaitMS: 5, TotalBudgetMS: 5000}))

	res := h.do(t, http.MethodPost, "/v1/messages", `{"model":"glm-5.3","messages":[]}`, nil)
	if res.Code != http.StatusTooManyRequests || res.Body.String() != "busy" {
		t.Fatalf("client got %d %q, want 429 busy", res.Code, res.Body.String())
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("upstream calls = %d, want 3", got)
	}
	if rec := h.record(t); rec.Attempt != 3 || rec.Status != http.StatusTooManyRequests {
		t.Fatalf("record = %+v, want attempt 3 status 429", rec)
	}
	stats := h.proxy.Stats()
	if stats.Retries != 2 || stats.Upstream429 != 3 {
		t.Fatalf("stats = %+v, want retries=2 upstream429=3", stats)
	}
}

func TestRetryHonoursTotalBudget(t *testing.T) {
	var calls atomic.Int64
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("busy"))
	}, withRetry(RetryConfig{MaxAttempts: 5, FirstWaitMS: 2000, Multiplier: 2, MaxWaitMS: 2000, TotalBudgetMS: 300}))

	start := time.Now()
	res := h.do(t, http.MethodPost, "/v1/messages", `{"model":"glm-5.3","messages":[]}`, nil)
	elapsed := time.Since(start)

	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", res.Code)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (budget exhausted before the first wait)", got)
	}
	if elapsed >= time.Second {
		t.Fatalf("request took %v; the 300ms budget should have stopped it immediately", elapsed)
	}
	if stats := h.proxy.Stats(); stats.Retries != 0 {
		t.Fatalf("retries = %d, want 0", stats.Retries)
	}
}

func TestDoesNotRetry404(t *testing.T) {
	var calls atomic.Int64
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("nope"))
	}, withRetry(RetryConfig{MaxAttempts: 5, FirstWaitMS: 1, Multiplier: 2, MaxWaitMS: 50, TotalBudgetMS: 5000}))

	res := h.do(t, http.MethodPost, "/v1/messages", `{"model":"glm-5.3","messages":[]}`, nil)
	if res.Code != http.StatusNotFound || res.Body.String() != "nope" {
		t.Fatalf("client got %d %q, want 404 nope", res.Code, res.Body.String())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
	if stats := h.proxy.Stats(); stats.Retries != 0 {
		t.Fatalf("retries = %d, want 0", stats.Retries)
	}
}

func TestParseRetryAfter(t *testing.T) {
	future := time.Now().Add(3 * time.Second).UTC()
	past := time.Now().Add(-time.Minute).UTC()

	cases := []struct {
		name  string
		value string
		check func(time.Duration) bool
	}{
		{"seconds", "7", func(d time.Duration) bool { return d == 7*time.Second }},
		{"zero", "0", func(d time.Duration) bool { return d == 0 }},
		{"negative", "-2", func(d time.Duration) bool { return d == 0 }},
		{"empty", "", func(d time.Duration) bool { return d == 0 }},
		{"garbage", "soon", func(d time.Duration) bool { return d == 0 }},
		{"http date", future.Format(http.TimeFormat), func(d time.Duration) bool {
			return d > 1*time.Second && d <= 3*time.Second
		}},
		{"past date", past.Format(http.TimeFormat), func(d time.Duration) bool { return d == 0 }},
	}
	for _, c := range cases {
		if got := parseRetryAfter(c.value); !c.check(got) {
			t.Errorf("%s: parseRetryAfter(%q) = %v", c.name, c.value, got)
		}
	}
}

// ---------- streaming ----------

func TestStreamsAndFlushesChunks(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	releaseUpstream := func() { once.Do(func() { close(release) }) }

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		io.WriteString(w, "data: one\n\n")
		flusher.Flush()
		<-release
		io.WriteString(w, "data: two\n\n")
		flusher.Flush()
		io.WriteString(w, "data: three\n\n")
		flusher.Flush()
	})

	front := httptest.NewServer(h.proxy)
	defer front.Close()
	defer releaseUpstream()

	resp, err := http.Post(front.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	reader := bufio.NewReader(resp.Body)
	type lineResult struct {
		line string
		err  error
	}
	first := make(chan lineResult, 1)
	go func() {
		line, err := reader.ReadString('\n')
		first <- lineResult{line, err}
	}()

	select {
	case r := <-first:
		if r.err != nil {
			t.Fatalf("reading first chunk: %v", r.err)
		}
		if !strings.Contains(r.line, "data: one") {
			t.Fatalf("first chunk = %q, want data: one", r.line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first chunk was not delivered while the upstream was still blocked")
	}

	releaseUpstream()
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rest), "data: two") || !strings.Contains(string(rest), "data: three") {
		t.Fatalf("remaining stream = %q, want chunks two and three", rest)
	}
}

func TestSSECostParsedAcrossChunks(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		// The first event is deliberately cut in the middle of the JSON and
		// left without a newline, so the parser must buffer it.
		io.WriteString(w, `data: {"type":"message_start","usage":{"input_tokens":10,"opencode":{"list_cost":0.000`)
		flusher.Flush()
		time.Sleep(10 * time.Millisecond)
		io.WriteString(w, `1,"allowance":0.0009,"tokens":{"input":10,"output":1,"cache_read":0}}}}`+"\n\n")
		flusher.Flush()
		time.Sleep(10 * time.Millisecond)
		io.WriteString(w, `data: {"type":"message_delta","usage":{"output_tokens":7,"opencode":{"list_cost":0.002,"allowance":0.0018,"tokens":{"input":10,"output":7,"cache_read":0}}}}`+"\n\n")
		flusher.Flush()
	})

	res := h.do(t, http.MethodPost, "/v1/messages", `{"model":"glm-5.3","messages":[]}`, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	rec := h.record(t)
	if rec.In != 10 || rec.Out != 7 || rec.CacheRead != 0 {
		t.Fatalf("record tokens = in %v out %v cache %v, want 10/7/0", rec.In, rec.Out, rec.CacheRead)
	}
	// message_start carries 0.0001/0.0009; the later message_delta must win.
	if !closeEnough(rec.Cost, 0.002) || !closeEnough(rec.Allowance, 0.0018) {
		t.Fatalf("record cost = %v allowance = %v, want 0.002/0.0018", rec.Cost, rec.Allowance)
	}
}

func TestSSEParserSurvivesChunkSplit(t *testing.T) {
	data := `data: {"usage":{"input_tokens":3,"output_tokens":2,"cache_read_input_tokens":1,` +
		`"opencode":{"list_cost":0.5,"allowance":0.4,"tokens":{"input":3,"output":2,"cache_read":1}}}}` + "\n\n"

	var parser sseParser
	for i := 0; i < len(data); i += 7 {
		end := i + 7
		if end > len(data) {
			end = len(data)
		}
		parser.Feed([]byte(data[i:end]))
	}
	parser.Close()

	acc := parser.acc
	if !closeEnough(acc.cost, 0.5) || !closeEnough(acc.allowance, 0.4) ||
		acc.in != 3 || acc.out != 2 || acc.cache != 1 {
		t.Fatalf("accumulator = %+v", acc)
	}
}

// ---------- count_tokens cache ----------

func TestCountTokensCacheHit(t *testing.T) {
	var calls atomic.Int64
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"input_tokens":10}`))
	})

	body := `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`
	headers := map[string]string{"X-Claude-Code-Session-Id": "s"}
	first := h.do(t, http.MethodPost, "/v1/messages/count_tokens", body, headers)
	second := h.do(t, http.MethodPost, "/v1/messages/count_tokens", body, headers)

	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("statuses = %d/%d, want 200/200", first.Code, second.Code)
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("bodies differ: %q vs %q", first.Body.String(), second.Body.String())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (second request must hit the cache)", got)
	}
}

func TestCountTokensDoesNotWaitForACell(t *testing.T) {
	// The app counts tokens constantly. Those calls must not queue behind a
	// streaming answer: they prefill nothing, so they hold no cell at all.
	blocked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/count_tokens") {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"input_tokens":7}`))
			return
		}
		once.Do(func() { close(blocked) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}, func(c *Config) { c.Scheduler.QueueTimeoutMS = 2000 })
	h.proxy.SetScheduler(NewCellScheduler(1))

	mainDone := make(chan struct{})
	go func() {
		defer close(mainDone)
		req := httptest.NewRequest(http.MethodPost, "/v1/messages",
			strings.NewReader(`{"model":"claude-GLM-5.3","messages":[]}`))
		h.proxy.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-blocked

	start := time.Now()
	rec := h.do(t, http.MethodPost, "/v1/messages/count_tokens",
		`{"model":"claude-GLM-5.3","messages":[{"role":"user","content":"hi"}]}`, nil)
	took := time.Since(start)
	close(release)
	<-mainDone

	if rec.Code != http.StatusOK {
		t.Fatalf("count_tokens status = %d, want 200", rec.Code)
	}
	if took > 500*time.Millisecond {
		t.Fatalf("count_tokens waited %s behind a busy cell; it must not take one", took)
	}
}

func TestCountTokensSingleflight(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	releaseUpstream := func() { once.Do(func() { close(release) }) }
	defer releaseUpstream()

	secondCall := make(chan struct{})
	var secondOnce sync.Once
	var calls atomic.Int64
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 2 {
			secondOnce.Do(func() { close(secondCall) })
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"input_tokens":5}`))
	})

	body := `{"model":"glm-5.3","messages":[]}`
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
			rec := httptest.NewRecorder()
			h.proxy.ServeHTTP(rec, req)
			results <- rec
		}()
	}
	close(start)

	waitFor(t, func() bool { return h.proxy.Stats().InFlight == 2 }, 3*time.Second)
	select {
	case <-secondCall:
		t.Error("a second upstream count_tokens call was made; the requests were not collapsed")
	case <-time.After(200 * time.Millisecond):
	}

	releaseUpstream()
	for i := 0; i < 2; i++ {
		select {
		case rec := <-results:
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rec.Code)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("a request did not finish")
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}

func TestCountTokensCacheExpires(t *testing.T) {
	var calls atomic.Int64
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"input_tokens":1}`))
	})

	body := `{"model":"glm-5.3","messages":[]}`
	h.do(t, http.MethodPost, "/v1/messages/count_tokens", body, nil)
	h.do(t, http.MethodPost, "/v1/messages/count_tokens", body, nil)
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 before expiry", got)
	}

	h.proxy.tokens.mu.Lock()
	for key, entry := range h.proxy.tokens.entries {
		entry.expires = time.Now().Add(-time.Second)
		h.proxy.tokens.entries[key] = entry
	}
	h.proxy.tokens.mu.Unlock()

	h.do(t, http.MethodPost, "/v1/messages/count_tokens", body, nil)
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 after the cache entry expired", got)
	}
}

// ---------- cost extraction and session aggregation ----------

func TestExtractsCostAndTokens(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"type":"message","usage":{` +
			`"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":5,` +
			`"opencode":{"billing_source":"go","list_cost":0.00123,"allowance":0.0011,` +
			`"tokens":{"input":100,"output":20,"reasoning":10,"cache_read":5}}}}`))
	})

	res := h.do(t, http.MethodPost, "/v1/messages", `{"model":"glm-5.3","messages":[]}`, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}

	rec := h.record(t)
	if rec.In != 100 || rec.Out != 20 || rec.CacheRead != 5 {
		t.Fatalf("tokens = in %v out %v cache %v, want 100/20/5", rec.In, rec.Out, rec.CacheRead)
	}
	if !closeEnough(rec.Cost, 0.00123) || !closeEnough(rec.Allowance, 0.0011) {
		t.Fatalf("cost = %v allowance = %v, want 0.00123/0.0011", rec.Cost, rec.Allowance)
	}
	if line := h.logs.String(); !strings.Contains(line, "cost=0.001230") {
		t.Fatalf("log line %q does not contain cost=0.001230", line)
	}
}

func TestSessionCostAggregatesBySession(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Model {
		case "glm-5.3":
			w.Write([]byte(`{"usage":{"opencode":{"list_cost":0.002,"allowance":0.001}}}`))
		default:
			w.Write([]byte(`{"usage":{"opencode":{"list_cost":0.004,"allowance":0.003}}}`))
		}
	})

	headers := map[string]string{"X-Claude-Code-Session-Id": "s1"}
	h.do(t, http.MethodPost, "/v1/messages", `{"model":"claude-GLM-5.3","messages":[]}`, headers)
	first := h.proxy.SessionCost("s1")
	if first.Session != "s1" || first.Requests != 1 {
		t.Fatalf("first record = %+v, want session s1 with 1 request", first)
	}
	if !closeEnough(first.USD, 0.001) || !closeEnough(first.ListUSD, 0.002) {
		t.Fatalf("first cost = %v/%v, want 0.001/0.002", first.USD, first.ListUSD)
	}

	h.do(t, http.MethodPost, "/v1/messages", `{"model":"deepseek-v4.1-flash","messages":[]}`, headers)
	second := h.proxy.SessionCost("s1")

	if second.Requests != 2 {
		t.Fatalf("requests = %d, want 2", second.Requests)
	}
	if !closeEnough(second.USD, 0.004) || !closeEnough(second.ListUSD, 0.006) {
		t.Fatalf("totals = %v/%v, want 0.004/0.006", second.USD, second.ListUSD)
	}
	if !second.FirstAt.Equal(first.FirstAt) {
		t.Fatalf("FirstAt moved: %v -> %v", first.FirstAt, second.FirstAt)
	}
	if second.LastAt.Before(first.LastAt) {
		t.Fatalf("LastAt went backwards: %v -> %v", first.LastAt, second.LastAt)
	}

	glm := second.ByModel["glm-5.3"]
	if glm.Requests != 1 || !closeEnough(glm.USD, 0.001) || !closeEnough(glm.ListUSD, 0.002) {
		t.Fatalf("glm model cost = %+v", glm)
	}
	deepseek := second.ByModel["deepseek-v4.1-flash"]
	if deepseek.Requests != 1 || !closeEnough(deepseek.USD, 0.003) || !closeEnough(deepseek.ListUSD, 0.004) {
		t.Fatalf("deepseek model cost = %+v", deepseek)
	}
}

func TestSessionCostIgnoresRetriesAndFailures(t *testing.T) {
	var calls atomic.Int64
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"usage":{"opencode":{"list_cost":9,"allowance":9}}}`))
		case 2:
			w.Write([]byte(`{"usage":{"opencode":{"list_cost":0.002,"allowance":0.001}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"usage":{"opencode":{"list_cost":5,"allowance":5}}}`))
		}
	}, withRetry(RetryConfig{MaxAttempts: 3, FirstWaitMS: 1, Multiplier: 1, MaxWaitMS: 5, TotalBudgetMS: 5000}))

	headers := map[string]string{"X-Claude-Code-Session-Id": "s2"}
	if res := h.do(t, http.MethodPost, "/v1/messages", `{"model":"glm-5.3","messages":[]}`, headers); res.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", res.Code)
	}
	if res := h.do(t, http.MethodPost, "/v1/messages", `{"model":"glm-5.3","messages":[]}`, headers); res.Code != http.StatusNotFound {
		t.Fatalf("second request status = %d, want 404", res.Code)
	}

	sc := h.proxy.SessionCost("s2")
	if sc.Requests != 1 {
		t.Fatalf("requests = %d, want 1 (only the successful attempt)", sc.Requests)
	}
	if !closeEnough(sc.USD, 0.001) || !closeEnough(sc.ListUSD, 0.002) {
		t.Fatalf("cost = %v/%v, want 0.001/0.002", sc.USD, sc.ListUSD)
	}
}

func TestSessionCostIgnoresCountTokens(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"input_tokens":10,"usage":{"opencode":{"list_cost":1,"allowance":1}}}`))
	})

	res := h.do(t, http.MethodPost, "/v1/messages/count_tokens", `{"model":"glm-5.3","messages":[]}`,
		map[string]string{"X-Claude-Code-Session-Id": "s3"})
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	sc := h.proxy.SessionCost("s3")
	if sc.Requests != 0 || sc.USD != 0 || sc.ListUSD != 0 {
		t.Fatalf("count_tokens contributed to session cost: %+v", sc)
	}
}

func TestSessionCostUnknownSessionIsZero(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	sc := h.proxy.SessionCost("missing")
	if sc.Session != "missing" || sc.Requests != 0 || sc.USD != 0 || sc.ListUSD != 0 || len(sc.ByModel) != 0 {
		t.Fatalf("unknown session = %+v", sc)
	}
}

func TestSessionCostEvictsOldestAboveLimit(t *testing.T) {
	p := NewProxy(nil, nil, nil, nil)
	base := time.Now().Add(-time.Hour)
	for i := 0; i < maxSessions+1; i++ {
		p.addSessionCost(fmt.Sprintf("s%d", i), "m", 1, 2, base.Add(time.Duration(i)*time.Millisecond))
	}
	if got := p.SessionCost("s0").Requests; got != 0 {
		t.Fatalf("oldest session s0 was not evicted")
	}
	if got := p.SessionCost("s1").Requests; got != 1 {
		t.Fatalf("second-oldest session s1 was evicted")
	}
}

// ---------- capture ----------

func TestCaptureMasksHeadersAndTruncates(t *testing.T) {
	upstreamBody := `{"ok":"0123456789abcdef"}`
	sentBody := make(chan []byte, 1)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sentBody <- body
		w.Header().Set("Authorization", "Bearer upstream-secret")
		w.Header().Set("x-api-key", "upstream-secret")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(upstreamBody))
	}, func(c *Config) {
		c.Log.MaxBodyBytes = 10
		c.Log.Capture = CaptureConfig{
			RequestHeaders: true, RequestBody: true,
			ResponseHeaders: true, ResponseBody: true,
		}
	})

	res := h.do(t, http.MethodPost, "/v1/messages", `{"model":"glm-5.3","messages":[]}`, map[string]string{
		"Authorization": "Bearer client-secret",
		"X-Api-Key":     "client-secret",
	})
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	sent := <-sentBody
	rec := h.record(t)
	if rec.Capture == nil {
		t.Fatal("capture record is nil")
	}
	for _, name := range []string{"Authorization", "X-Api-Key"} {
		if !masked(rec.Capture.RequestHeaders, name) {
			t.Errorf("request header %s = %v, want masked", name, rec.Capture.RequestHeaders[name])
		}
		if !masked(rec.Capture.ResponseHeaders, name) {
			t.Errorf("response header %s = %v, want masked", name, rec.Capture.ResponseHeaders[name])
		}
	}
	if len(rec.Capture.RequestBody) != 10 || !strings.HasPrefix(string(sent), rec.Capture.RequestBody) {
		t.Fatalf("captured request body = %q (sent %q), want a 10-byte prefix", rec.Capture.RequestBody, sent)
	}
	if rec.Capture.ResponseBody != upstreamBody[:10] {
		t.Fatalf("captured response body = %q, want %q", rec.Capture.ResponseBody, upstreamBody[:10])
	}
	if !rec.Capture.Truncated {
		t.Fatal("Truncated = false, want true")
	}

	raw, err := json.Marshal(rec.Capture)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "client-secret") || strings.Contains(string(raw), "upstream-secret") {
		t.Fatalf("capture leaked a credential: %s", raw)
	}
}

// ---------- aborts ----------

func TestClientDisconnectAborts(t *testing.T) {
	started := make(chan struct{})
	var startedOnce sync.Once
	upstreamCanceled := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		// Drain the body first: net/http does not watch for a client
		// disconnect while unread request bytes are still pending, so a
		// handler that never reads would never see the cancel.
		_, _ = io.Copy(io.Discard, r.Body)
		startedOnce.Do(func() { close(started) })
		<-r.Context().Done()
		close(upstreamCanceled)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`)).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		h.proxy.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request never started")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("proxy handler did not return after the client canceled")
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request was not canceled")
	}

	rec := h.record(t)
	if !rec.Aborted {
		t.Error("record Aborted = false, want true")
	}
	if got := h.proxy.Stats().InFlight; got != 0 {
		t.Errorf("InFlight = %d, want 0", got)
	}
}

func TestPrefillRefusalTightensTheCeiling(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"prefill_cap_refused, retry after 30 s"}}`))
	}, withRetry(RetryConfig{MaxAttempts: 1, TotalBudgetMS: 1}))
	h.proxy.SetScheduler(NewCellScheduler(8))

	rec := h.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-GLM-5.3","messages":[]}`, nil)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := h.proxy.sched.Limit(); got != 7 {
		t.Fatalf("cell limit = %d, want 7 after one prefill refusal", got)
	}
	if got := h.proxy.Stats().PrefillRefusals; got != 1 {
		t.Fatalf("prefill refusals = %d, want 1", got)
	}
	// The body must still reach the client untouched after being inspected.
	if !strings.Contains(rec.Body.String(), "prefill_cap_refused") {
		t.Fatalf("upstream body was altered: %q", rec.Body.String())
	}
}

func TestAllowanceRefusalLeavesTheCeilingAlone(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"monthly allowance exhausted"}}`))
	}, withRetry(RetryConfig{MaxAttempts: 1, TotalBudgetMS: 1}))
	h.proxy.SetScheduler(NewCellScheduler(8))

	rec := h.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-GLM-5.3","messages":[]}`, nil)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := h.proxy.sched.Limit(); got != 8 {
		t.Fatalf("cell limit = %d, want 8: a non-prefill 429 says nothing about concurrency", got)
	}
}

func TestQueueTimeoutAnswersOverloaded(t *testing.T) {
	blocked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(blocked) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}, func(c *Config) { c.Scheduler.QueueTimeoutMS = 50 })
	h.proxy.SetScheduler(NewCellScheduler(1))

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, "/v1/messages",
			strings.NewReader(`{"model":"claude-GLM-5.3","messages":[]}`))
		h.proxy.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-blocked

	rec := h.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-GLM-5.3","messages":[]}`, nil)
	close(release)
	<-done

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After = %q, want 5", got)
	}
	if !strings.Contains(rec.Body.String(), "overloaded_error") {
		t.Fatalf("body is not an Anthropic overloaded_error: %s", rec.Body.String())
	}
}

func TestSetCostStoreReplacesTheStore(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	store, err := NewCostStore("")
	if err != nil {
		t.Fatal(err)
	}
	h.proxy.SetCostStore(store)
	if h.proxy.costs != store {
		t.Fatal("SetCostStore did not install the store")
	}
}

func TestLimitedBufferTruncatesAndReports(t *testing.T) {
	lb := &limitedBuffer{max: 4}
	n, err := lb.Write([]byte("abcdefgh"))
	if err != nil || n != 8 {
		t.Fatalf("Write = (%d, %v), want (8, nil): it must report a full write", n, err)
	}
	if got := lb.String(); got != "abcd" {
		t.Fatalf("buffer = %q, want %q", got, "abcd")
	}
	if !lb.truncated {
		t.Fatal("truncation was not recorded")
	}
}

func TestPingWriterDelegatesTheResponseWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	pw := newPingWriter(rec, time.Hour, &atomic.Int64{})

	pw.Header().Set("X-Test", "1")
	pw.WriteHeader(http.StatusCreated)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if got := rec.Header().Get("X-Test"); got != "1" {
		t.Fatalf("header = %q, want 1", got)
	}
	pw.Flush()
	if pw.Committed() {
		t.Fatal("an uncommitted writer reported itself committed")
	}
}

func TestFillCapturedBodyRecordsTruncation(t *testing.T) {
	cfg := Defaults()
	cfg.Log.Capture.ResponseBody = true
	cfg.Log.MaxBodyBytes = 4

	rec := &CaptureRecord{}
	fillCapturedBody(rec, &cfg, []byte("abcdefgh"))
	if rec.ResponseBody != "abcd" {
		t.Fatalf("captured %q, want abcd", rec.ResponseBody)
	}
	if !rec.Truncated {
		t.Fatal("truncation was not recorded")
	}

	// A nil record and a disabled capture are both no-ops.
	fillCapturedBody(nil, &cfg, []byte("abc"))
	off := Defaults()
	off.Log.Capture.ResponseBody = false
	rec2 := &CaptureRecord{}
	fillCapturedBody(rec2, &off, []byte("abc"))
	if rec2.ResponseBody != "" {
		t.Fatalf("captured body %q with capture disabled", rec2.ResponseBody)
	}
}

func TestTruncateBodyWithoutALimit(t *testing.T) {
	text, truncated := truncateBody([]byte("anything"), 0)
	if text != "anything" || truncated {
		t.Fatalf("truncateBody = (%q, %v), want the whole body", text, truncated)
	}
}

func TestLoaderDefaultsMatchesTheBuiltIns(t *testing.T) {
	l := testConfig("http://127.0.0.1:1")
	if got := l.Defaults().Listen; got != Defaults().Listen {
		t.Fatalf("loader Defaults listen = %q", got)
	}
}
