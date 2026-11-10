package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"opencode-proxy/internal/config"
	"opencode-proxy/internal/models"
)

// qoderFake is both the jobToken endpoint and the chat completions endpoint.
type qoderFake struct {
	exchanges   atomic.Int64
	completions atomic.Int64
	lastAuth    atomic.Value // string
	lastReqID   atomic.Value
	lastSessID  atomic.Value
	lastBody    atomic.Value // []byte
}

func (f *qoderFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/v1/jobToken/exchange":
		f.exchanges.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"token":"jobtok","refresh_token":"ref1","expires_in":3600}`))
	case r.URL.Path == "/model/v1/chat/completions":
		f.completions.Add(1)
		f.lastAuth.Store(r.Header.Get("Authorization"))
		f.lastReqID.Store(r.Header.Get("X-Request-ID"))
		f.lastSessID.Store(r.Header.Get("X-Session-ID"))
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		f.lastBody.Store(body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"qoder-test-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"}}]}\n\n" +
			"data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":3}}\n\n" +
			"data: [DONE]\n\n"))
	default:
		http.NotFound(w, r)
	}
}

func newQoderHarness(t *testing.T, pat string, mutate ...func(*config.Config)) (*harness, *qoderFake) {
	t.Helper()
	fake := &qoderFake{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	h := newHarness(t, http.NotFound, append(mutate, func(c *config.Config) {
		c.Qoder.TokenEnv = "QODER_PROXY_TEST_PAT"
		c.Qoder.OpenAPIBaseURL = srv.URL
		c.Qoder.InferenceBaseURL = srv.URL
		c.Models.Routes = []models.Route{{
			ID:      "qoder-test-model",
			Picker:  "claude-qoder-test",
			Auth:    "qoder",
			BaseURL: srv.URL,
		}}
	})...)
	if pat != "" {
		t.Setenv("QODER_PROXY_TEST_PAT", pat)
	}
	return h, fake
}

const anthropicSmall = `{"model":"claude-qoder-test","max_tokens":64,"stream":false,
	"messages":[{"role":"user","content":"Hi"}]}`

func TestQoderCountTokensLocal(t *testing.T) {
	h, fake := newQoderHarness(t, "pat-secret")
	res := h.do(t, http.MethodPost, "/v1/messages/count_tokens", anthropicSmall, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("body: %s", res.Body.String())
	}
	if out.InputTokens <= 0 {
		t.Fatalf("input_tokens: %d", out.InputTokens)
	}
	if fake.exchanges.Load() != 0 || fake.completions.Load() != 0 {
		t.Fatalf("count_tokens must not touch the network: exchanges=%d completions=%d",
			fake.exchanges.Load(), fake.completions.Load())
	}
}

func TestQoderNonStreamTranslation(t *testing.T) {
	h, fake := newQoderHarness(t, "pat-secret")
	res := h.do(t, http.MethodPost, "/v1/messages", anthropicSmall, map[string]string{
		"X-Api-Key": "client-key",
	})
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	var msg map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &msg); err != nil {
		t.Fatalf("body: %s", res.Body.String())
	}
	if msg["type"] != "message" || msg["role"] != "assistant" {
		t.Fatalf("message: %v", msg)
	}
	content := msg["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != "Hello" {
		t.Fatalf("content: %v", content)
	}

	if fake.completions.Load() != 1 {
		t.Fatalf("completions=%d", fake.completions.Load())
	}
	if auth, _ := fake.lastAuth.Load().(string); auth != "Bearer jobtok" {
		t.Fatalf("upstream auth: %q", auth)
	}
	if id, _ := fake.lastReqID.Load().(string); id == "" {
		t.Fatal("X-Request-ID missing")
	}
	if sid, _ := fake.lastSessID.Load().(string); sid == "" {
		t.Fatal("X-Session-ID missing")
	}
	var sent map[string]any
	if raw, _ := fake.lastBody.Load().([]byte); len(raw) == 0 {
		t.Fatal("upstream body empty")
	} else if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatalf("upstream body: %s", raw)
	}
	if sent["model"] != "qoder-test-model" || sent["stream"] != true {
		t.Fatalf("upstream body: %v", sent)
	}
}

func TestQoderRouteMissingToken(t *testing.T) {
	h, fake := newQoderHarness(t, "") // no PAT anywhere
	res := h.do(t, http.MethodPost, "/v1/messages", anthropicSmall, nil)
	if res.Code != http.StatusBadGateway {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "qoder") {
		t.Fatalf("error must name qoder config: %s", res.Body.String())
	}
	if fake.completions.Load() != 0 {
		t.Fatalf("completions attempted: %d", fake.completions.Load())
	}
}

func TestQoderUnsupportedFeature400(t *testing.T) {
	h, fake := newQoderHarness(t, "pat-secret")
	body := `{"model":"claude-qoder-test","max_tokens":64,"stream":false,
		"tool_choice":{"type":"hello"},
		"messages":[{"role":"user","content":"Hi"}]}`
	res := h.do(t, http.MethodPost, "/v1/messages", body, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	if fake.completions.Load() != 0 {
		t.Fatalf("completions attempted: %d", fake.completions.Load())
	}
}
