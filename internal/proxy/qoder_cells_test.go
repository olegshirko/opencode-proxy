package proxy

import (
	"net/http"
	"strings"
	"testing"
)

func TestQoderCellPrefix(t *testing.T) {
	h, _ := newQoderHarness(t, "pat-secret")
	body := `{"model":"claude-qoder-test","max_tokens":16,"stream":false,
		"messages":[{"role":"user","content":"Hi"}]}`
	res := h.do(t, http.MethodPost, "/v1/messages", body, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	rec := <-h.records
	if !strings.HasPrefix(rec.Cell, "qoder/") {
		t.Fatalf("qoder request cell %q must be namespaced with qoder/", rec.Cell)
	}
}

func TestPrefillLearningOnlyFromOpenCode(t *testing.T) {
	// A 429 whose body screams "prefill" must move the OpenCode ceiling — but
	// the same body from Qoder must not: that ceiling models the OpenCode
	// gateway's concurrency behaviour, not Qoder's.
	h, fake := newQoderHarness(t, "pat-secret")
	fake.failPrefill.Store(1)
	body := `{"model":"claude-qoder-test","max_tokens":16,"stream":false,
		"messages":[{"role":"user","content":"Hi"}]}`
	res := h.do(t, http.MethodPost, "/v1/messages", body, nil)
	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	if got := h.proxy.prefill429.Load(); got != 0 {
		t.Fatalf("qoder 429 moved the prefill ceiling: %d", got)
	}
	// And the OpenCode path still learns (existing behaviour): a prefill 429
	// from the gateway bumps the counter. The opencode harness fake is
	// http.NotFound, so just assert the qoder case above did not leak.
}
