package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAdminStatusIncludesQoder(t *testing.T) {
	h, _ := newQoderHarness(t, "pat-secret")
	rec := adminDo(t, h.proxy, http.MethodGet, "/admin/status", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body: %s", rec.Body.String())
	}
	q, ok := payload["qoder"].(map[string]any)
	if !ok {
		t.Fatalf("admin status has no qoder block: %s", rec.Body.String())
	}
	if q["state"] != "empty" {
		t.Fatalf("qoder state: %v", q["state"])
	}
	// No secret material anywhere in the status payload.
	if strings.Contains(rec.Body.String(), "pat-secret") || strings.Contains(rec.Body.String(), "jobtok") {
		t.Fatalf("status leaks secrets: %s", rec.Body.String())
	}
}
