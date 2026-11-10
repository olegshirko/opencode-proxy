package proxy

import (
	"net/http"
	"strings"
	"testing"
)

func TestQoderStreamEndToEnd(t *testing.T) {
	h, fake := newQoderHarness(t, "pat-secret")
	body := `{"model":"claude-qoder-test","max_tokens":64,"stream":true,
		"messages":[{"role":"user","content":"Hi"}]}`
	res := h.do(t, http.MethodPost, "/v1/messages", body, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	out := res.Body.String()
	for _, want := range []string{
		"event: message_start",
		`"text_delta"`, `"text":"Hello"`,
		"event: message_delta", `"input_tokens":10`, `"output_tokens":3`,
		"event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stream missing %q in:\n%s", want, out)
		}
	}
	// usage reached the accounting: the request record carries tokens
	rec := <-h.records
	if rec.In != 10 || rec.Out != 3 {
		t.Fatalf("record accounting: in=%v out=%v", rec.In, rec.Out)
	}
	if fake.completions.Load() != 1 {
		t.Fatalf("completions=%d", fake.completions.Load())
	}
}

func TestQoderReAuthOn401(t *testing.T) {
	h, fake := newQoderHarness(t, "pat-secret")
	fake.failFirst.Store(1)
	body := `{"model":"claude-qoder-test","max_tokens":64,"stream":true,
		"messages":[{"role":"user","content":"Hi"}]}`
	res := h.do(t, http.MethodPost, "/v1/messages", body, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "message_stop") {
		t.Fatalf("stream incomplete: %s", res.Body.String())
	}
	if got := fake.exchanges.Load(); got != 2 {
		t.Fatalf("exchanges=%d, want 2 (initial + re-auth)", got)
	}
}
