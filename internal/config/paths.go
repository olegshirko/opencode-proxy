package config

import (
	"os"
	"path/filepath"
	"runtime"
)

// DefaultConfigPath is where the proxy looks for config.json when -config is
// not given. macOS keeps the historical ~/opencode-proxy layout (existing
// installs must not move); Windows uses %APPDATA%; everything else follows
// XDG.
func DefaultConfigPath() string {
	return defaultConfigPathFor(runtime.GOOS, home())
}

// DefaultStateDir is where the runtime state lives (log, quota ledger,
// session costs). macOS keeps the historical layout; Windows uses
// %LOCALAPPDATA%; everything else follows XDG.
func DefaultStateDir() string {
	return defaultStateDirFor(runtime.GOOS, home())
}

func defaultConfigPathFor(goos, home string) string {
	switch goos {
	case "darwin":
		return filepath.Join(home, "opencode-proxy", "config.json")
	case "windows":
		return filepath.Join(windowsDir(home, "APPDATA", filepath.Join("AppData", "Roaming")), "opencode-proxy", "config.json")
	default:
		return filepath.Join(home, ".config", "opencode-proxy", "config.json")
	}
}

func defaultStateDirFor(goos, home string) string {
	switch goos {
	case "darwin":
		return filepath.Join(home, "opencode-proxy", "var")
	case "windows":
		return filepath.Join(windowsDir(home, "LOCALAPPDATA", filepath.Join("AppData", "Local")), "opencode-proxy", "var")
	default:
		return filepath.Join(home, ".local", "state", "opencode-proxy", "var")
	}
}

// windowsDir prefers the shell-folder environment variable; a stripped
// environment falls back to the conventional layout under the home dir.
func windowsDir(home, env, fallback string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return filepath.Join(home, fallback)
}

func home() string {
	dir, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return dir
}
