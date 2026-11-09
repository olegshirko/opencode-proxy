package config

import "testing"

func TestDefaultPathsByOS(t *testing.T) {
	// darwin keeps the historical layout; everything else gets XDG dirs.
	if got := defaultConfigPathFor("darwin", "/h"); got != "/h/opencode-proxy/config.json" {
		t.Fatalf("darwin config = %q", got)
	}
	if got := defaultConfigPathFor("linux", "/h"); got != "/h/.config/opencode-proxy/config.json" {
		t.Fatalf("linux config = %q", got)
	}
	if got := defaultStateDirFor("darwin", "/h"); got != "/h/opencode-proxy/var" {
		t.Fatalf("darwin state = %q", got)
	}
	if got := defaultStateDirFor("linux", "/h"); got != "/h/.local/state/opencode-proxy/var" {
		t.Fatalf("linux state = %q", got)
	}
}
