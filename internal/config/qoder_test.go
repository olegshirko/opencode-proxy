package config

import "testing"

func TestConfigQoderDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Qoder.OpenAPIBaseURL != "https://openapi.qoder.sh" {
		t.Fatalf("default openapi_base_url: %q", cfg.Qoder.OpenAPIBaseURL)
	}
	if cfg.Qoder.RefreshMarginMS != 60000 {
		t.Fatalf("default refresh_margin_ms: %d", cfg.Qoder.RefreshMarginMS)
	}
}

func TestConfigQoderOverride(t *testing.T) {
	cfg, err := Parse([]byte(`{"qoder":{"openapi_base_url":"https://test-openapi.qoder.sh","refresh_margin_ms":1000}}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Qoder.OpenAPIBaseURL != "https://test-openapi.qoder.sh" || cfg.Qoder.RefreshMarginMS != 1000 {
		t.Fatalf("override lost: %+v", cfg.Qoder)
	}
	// Unspecified fields keep their defaults even in an explicit section.
	if cfg.Qoder.InferenceBaseURL != "https://api1.qoder.sh" {
		t.Fatalf("default inference_base_url: %q", cfg.Qoder.InferenceBaseURL)
	}
}
