package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"opencode-proxy/internal/config"
	"opencode-proxy/internal/reqlog"
	"opencode-proxy/internal/retry"
	"opencode-proxy/internal/scheduler"
	"strings"
	"testing"

	"time"
)

func TestCellKeyVariants(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    string
		prio    int
	}{
		{"main", nil, "s:sess", scheduler.PriorityMain},
		{"agent", map[string]string{"X-Claude-Code-Agent-Id": "a1"}, "s:sess:a:a1", scheduler.PrioritySubagent},
		{"auxiliary", map[string]string{"X-Claude-Code-Request-Class": "auxiliary"}, "s:sess:aux", scheduler.PriorityAuxiliary},
		{"compaction", map[string]string{"X-Claude-Code-Compaction": "1", "X-Claude-Code-Agent-Id": "a1"}, "s:sess", scheduler.PriorityMain},
		{"agent wins over class", map[string]string{
			"X-Claude-Code-Agent-Id":      "a1",
			"X-Claude-Code-Request-Class": "auxiliary",
		}, "s:sess:aux", scheduler.PriorityAuxiliary},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		key, _, _, prio := cellKey(req, "sess")
		if key != c.want {
			t.Errorf("%s: key = %q, want %q", c.name, key, c.want)
		}
		if prio != c.prio {
			t.Errorf("%s: priority = %d, want %d", c.name, prio, c.prio)
		}
	}
}

func TestForwardsEveryAnthropicHeader(t *testing.T) {
	got := make(chan http.Header, 1)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	})

	h.do(t, http.MethodPost, "/v1/messages", `{"model":"glm-5.3","messages":[]}`, map[string]string{
		"anthropic-beta":           "context-1m-2025-08-07,interleaved-thinking-2025-05-14",
		"anthropic-version":        "2023-06-01",
		"anthropic-new-feature":    "yes",
		"X-Claude-Code-Session-Id": "s1",
	})

	upstream := <-got
	if v := upstream.Get("anthropic-beta"); !strings.Contains(v, "context-1m") {
		t.Errorf("anthropic-beta = %q, want it forwarded verbatim", v)
	}
	if v := upstream.Get("anthropic-new-feature"); v != "yes" {
		t.Errorf("unknown anthropic-* header dropped or altered: %q", v)
	}
	if v := upstream.Get("X-Claude-Code-Session-Id"); v != "" {
		t.Errorf("client-private header leaked upstream: %q", v)
	}
}

func TestModelsEndpointListsPickers(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:1")
	p := NewProxy(cfg, testRegistry(t), reqlog.NewWithWriter(&discard{}, cfg.Get().Log), &http.Client{})
	router := Router(p)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			Type        string `json:"type"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 2 {
		t.Fatalf("got %d models, want 2", len(out.Data))
	}
	if out.Data[0].Type != "model" || !strings.HasPrefix(out.Data[0].ID, "claude-") {
		t.Errorf("entry = %+v, want a claude- picker id", out.Data[0])
	}
}

func TestStreamRewritesModelInMessageStart(t *testing.T) {
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"m1","model":"glm-5.3","usage":{"input_tokens":1}}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	})

	rec := h.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-GLM-5.3","stream":true,"messages":[]}`, nil)

	body := rec.Body.String()
	if !strings.Contains(body, `"model":"claude-GLM-5.3"`) {
		t.Errorf("message_start model was not rewritten to the requested id:\n%s", body)
	}
	if strings.Contains(body, `"model":"glm-5.3"`) {
		t.Errorf("upstream model id leaked to the client:\n%s", body)
	}
}

func TestStreamSendsPingsWhileRetrying(t *testing.T) {
	var calls int
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"at capacity"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}, func(c *config.Config) {
		c.Retry = retry.Config{MaxAttempts: 3, FirstWaitMS: 120, Multiplier: 1, MaxWaitMS: 200, TotalBudgetMS: 5000}
		c.Scheduler = config.SchedulerConfig{
			MaxConcurrentCells:  8,
			QueueTimeoutMS:      1000,
			KeepaliveAfterMS:    10,
			KeepaliveIntervalMS: 20,
			FallbackAfterMS:     20000,
		}
	})

	rec := h.do(t, http.MethodPost, "/v1/messages",
		`{"model":"glm-5.3","stream":true,"messages":[]}`, nil)

	body := rec.Body.String()
	if !strings.Contains(body, "event: ping") {
		t.Errorf("client saw no keepalive while the proxy retried:\n%s", body)
	}
	if !strings.Contains(body, "message_stop") {
		t.Errorf("real stream never arrived after the retry:\n%s", body)
	}
	if h.proxy.Stats().Keepalives == 0 {
		t.Error("keepalive counter stayed at zero")
	}
}

func TestStreamErrorEventWhenRetriesExhausted(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"at capacity"}}`))
	}, func(c *config.Config) {
		c.Retry = retry.Config{MaxAttempts: 2, FirstWaitMS: 80, Multiplier: 1, MaxWaitMS: 100, TotalBudgetMS: 5000}
		c.Scheduler = config.SchedulerConfig{
			MaxConcurrentCells:  8,
			QueueTimeoutMS:      1000,
			KeepaliveAfterMS:    10,
			KeepaliveIntervalMS: 15,
		}
	})

	rec := h.do(t, http.MethodPost, "/v1/messages",
		`{"model":"glm-5.3","stream":true,"messages":[]}`, nil)

	body := rec.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Errorf("no error event after the retry budget ran out:\n%s", body)
	}
	if !strings.Contains(body, "at capacity") {
		t.Errorf("upstream error body was not carried through:\n%s", body)
	}
}

func TestNonStreamRetriesAreUnchanged(t *testing.T) {
	var calls int
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m1","model":"glm-5.3","usage":{"input_tokens":3,"output_tokens":1}}`))
	})

	rec := h.do(t, http.MethodPost, "/v1/messages", `{"model":"claude-GLM-5.3","messages":[]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"model":"claude-GLM-5.3"`) {
		t.Errorf("non-streaming model was not rewritten:\n%s", rec.Body.String())
	}
	time.Sleep(1 * time.Millisecond)
}
