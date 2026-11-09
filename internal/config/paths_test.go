package config

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestDefaultPathsByOS(t *testing.T) {
	// darwin keeps the historical layout; everything else gets XDG dirs.
	// Expectations are built with filepath.Join so the test passes on every
	// host separator.
	if got := defaultConfigPathFor("darwin", "/h"); got != filepath.Join("/h", "opencode-proxy", "config.json") {
		t.Fatalf("darwin config = %q", got)
	}
	if got := defaultConfigPathFor("linux", "/h"); got != filepath.Join("/h", ".config", "opencode-proxy", "config.json") {
		t.Fatalf("linux config = %q", got)
	}
	if got := defaultStateDirFor("darwin", "/h"); got != filepath.Join("/h", "opencode-proxy", "var") {
		t.Fatalf("darwin state = %q", got)
	}
	if got := defaultStateDirFor("linux", "/h"); got != filepath.Join("/h", ".local", "state", "opencode-proxy", "var") {
		t.Fatalf("linux state = %q", got)
	}
	// The exported wrappers follow the running OS.
	if DefaultConfigPath() != defaultConfigPathFor(runtime.GOOS, home()) {
		t.Fatal("DefaultConfigPath disagrees with defaultConfigPathFor")
	}
	if DefaultStateDir() != defaultStateDirFor(runtime.GOOS, home()) {
		t.Fatal("DefaultStateDir disagrees with defaultStateDirFor")
	}
}
