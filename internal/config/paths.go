package config

import (
	"os"
	"path/filepath"
	"runtime"
)

// DefaultConfigPath is where the proxy looks for config.json when -config is
// not given. macOS keeps the historical ~/opencode-proxy layout (existing
// installs must not move); everything else follows XDG.
func DefaultConfigPath() string {
	return defaultConfigPathFor(runtime.GOOS, home())
}

// DefaultStateDir is where the runtime state lives (log, quota ledger,
// session costs). macOS keeps the historical layout; everything else follows
// XDG.
func DefaultStateDir() string {
	return defaultStateDirFor(runtime.GOOS, home())
}

func defaultConfigPathFor(goos, home string) string {
	if goos == "darwin" {
		return filepath.Join(home, "opencode-proxy", "config.json")
	}
	return filepath.Join(home, ".config", "opencode-proxy", "config.json")
}

func defaultStateDirFor(goos, home string) string {
	if goos == "darwin" {
		return filepath.Join(home, "opencode-proxy", "var")
	}
	return filepath.Join(home, ".local", "state", "opencode-proxy", "var")
}

func home() string {
	dir, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return dir
}
