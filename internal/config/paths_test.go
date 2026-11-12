package config

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestDefaultPathsWindows(t *testing.T) {
	t.Setenv("APPDATA", `C:\Users\me\AppData\Roaming`)
	t.Setenv("LOCALAPPDATA", `C:\Users\me\AppData\Local`)
	wantCfg := filepath.Join(`C:\Users\me\AppData\Roaming`, "opencode-proxy", "config.json")
	if got := defaultConfigPathFor("windows", "/h"); got != wantCfg {
		t.Fatalf("windows config = %q, want %q", got, wantCfg)
	}
	wantState := filepath.Join(`C:\Users\me\AppData\Local`, "opencode-proxy", "var")
	if got := defaultStateDirFor("windows", "/h"); got != wantState {
		t.Fatalf("windows state = %q, want %q", got, wantState)
	}

	// A stripped environment (no shell-folder vars) falls back to the
	// conventional AppData layout under home.
	t.Setenv("APPDATA", "")
	t.Setenv("LOCALAPPDATA", "")
	wantCfg = filepath.Join("/h", "AppData", "Roaming", "opencode-proxy", "config.json")
	if got := defaultConfigPathFor("windows", "/h"); got != wantCfg {
		t.Fatalf("windows fallback config = %q, want %q", got, wantCfg)
	}
	wantState = filepath.Join("/h", "AppData", "Local", "opencode-proxy", "var")
	if got := defaultStateDirFor("windows", "/h"); got != wantState {
		t.Fatalf("windows fallback state = %q, want %q", got, wantState)
	}
}

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
