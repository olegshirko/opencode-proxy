# Cross-Platform Support (Linux first) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A stranger on Ubuntu-like Linux goes from `git clone` to a working proxy with a 30-model CLI picker without reading design docs; macOS behavior is unchanged.

**Architecture:** Per-OS path defaults and config self-init inside `internal/config`; packaging split into Makefile OS branches (launchd on macOS, systemd user unit on Linux); `genmodels -settings` emits the CLI settings block; statusline learns a CLI mode; GitHub Actions matrix adds CI.

**Tech Stack:** Go 1.27 (pure, cross-compiles today), systemd user units, GNU make, GitHub Actions.

**Spec:** `docs/superpowers/specs/2026-11-09-cross-platform-design.md`

## Global Constraints

- macOS install behavior is unchanged: existing `~/opencode-proxy` paths, launchd flow, `make install` result (spec anti-goal).
- No root requirement on Linux: systemd **user** unit, binaries in `~/.local/bin`.
- Module stays dependency-free: `go.mod` unchanged, CI runs `go test` with no downloads.
- `-settings` output: JSON to stdout only — never touches `models.json` (spec §4).
- `modelPicker` entries: only `available: true` models, `replaceBuiltInOptions: true` (spec §4).
- Tracked `config.json` stays the author's working file; no code or docs treat it as a template (spec §5).
- Windows is out of scope (phase 2): CI runs windows tests as `continue-on-error` signal only (spec §6).

## Review Focus

- Stranger's first run with no config file anywhere → proxy must self-create config from defaults and start; never fatal. Pinned by `TestNewConfigLoaderInitializesMissingFile` (Task 2).
- `models.json` missing on a fresh machine → proxy starts with empty registry + logged hint, live catalog keeps resolution working. Pinned by `TestLoadRegistryOrDefault` (Task 2).
- Statusline handed a bare CLI session id → must query `/admin/session-cost` with that id directly, never attempt the macOS `local_` glob. Pinned by `TestResolveSessionID` (Task 4).
- `-settings` block must not offer region-blocked models → picker options filtered to `available: true`. Pinned by `TestWriteSettings` (Task 3).
- `make models` on Linux must land where the running proxy reloads from (config dir, not repo checkout). Pinned by `TestDefaultPathsByOS` (Task 1) + Makefile `CONFIG_DIR` step in Task 5.

---

### Task 1: Per-OS default paths

**Files:**
- Create: `internal/config/paths.go`
- Modify: `internal/config/config.go` (Defaults, logFile computation ~line 140), `cmd/opencode-proxy/main.go` (defaultConfigPath)
- Test: `internal/config/paths_test.go`

**Interfaces:**
- Produces: `config.DefaultConfigPath() string`, `config.DefaultStateDir() string` (both read `os.UserHomeDir()` + `runtime.GOOS`); internal `defaultConfigPathFor(goos, home string) string` and `defaultStateDirFor(goos, home string) string` for table tests.
- Consumes: nothing new.

- [ ] **Step 1: Write the failing table test**

```go
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
```

- [ ] **Step 2: Run to verify it fails** — `go test ./internal/config/ -run TestDefaultPathsByOS` → FAIL (undefined).

- [ ] **Step 3: Implement `paths.go`** — the four functions; exported wrappers substitute `os.UserHomeDir()` and `runtime.GOOS`. Change `Defaults()` to build `logFile` from `DefaultStateDir()` so state follows the OS (darwin result identical to today).

- [ ] **Step 4: Verify** — `go test ./internal/config/ ./cmd/...` → PASS (existing `TestDefaults` and `TestLoaderDefaultsMatchesTheBuiltIns` must stay green: darwin values unchanged).

- [ ] **Step 5: Commit** — `git commit -m "feat: per-OS default paths (XDG on Linux)"`

### Task 2: Config self-init and registry-missing tolerance

**Files:**
- Modify: `internal/config/config.go` (NewConfigLoader)
- Modify: `cmd/opencode-proxy/main.go` (registry load + hint log; modelsPath handling)
- Test: `internal/config/config_test.go`, `cmd/opencode-proxy/main_test.go`

**Interfaces:**
- Consumes: Task 1 paths.
- Produces: `NewConfigLoader(path)` creates a missing file from `Defaults()` instead of erroring; `loadRegistryOrDefault(path string) (*models.Registry, bool, error)` in cmd (bool = file existed).

- [ ] **Step 1: Failing tests**

```go
// config_test.go
func TestNewConfigLoaderInitializesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	l, err := NewConfigLoader(path)
	if err != nil {
		t.Fatalf("missing config must self-init: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("defaults not written: %v", err)
	}
	if l.Get().Listen != Defaults().Listen {
		t.Fatalf("listen = %q, want the default", l.Get().Listen)
	}
}
```

```go
// main_test.go
func TestLoadRegistryOrDefault(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "models.json")
	reg, existed, err := loadRegistryOrDefault(missing)
	if err != nil || existed || reg == nil {
		t.Fatalf("missing file: existed=%v err=%v, want empty registry, no error", existed, err)
	}
	// A corrupt existing file stays an error: silence must not hide breakage.
	bad := filepath.Join(t.TempDir(), "models.json")
	os.WriteFile(bad, []byte("{nope"), 0o644)
	if _, _, err := loadRegistryOrDefault(bad); err == nil {
		t.Fatal("corrupt models.json must error")
	}
}
```

- [ ] **Step 2: Verify they fail** — `go test ./internal/config/ -run TestNewConfigLoaderInitializesMissingFile` and `go test ./cmd/opencode-proxy/ -run TestLoadRegistryOrDefault` → FAIL.

- [ ] **Step 3: Implement** — in `NewConfigLoader`, when the initial read fails with `os.IsNotExist`, write `Defaults()` with `atomicio.WriteFile` (create parent dirs), then proceed as today; a present-but-broken file still errors. In cmd, `loadRegistryOrDefault` wraps `models.LoadRegistry`: `os.IsNotExist` on stat → empty `&models.Registry{}`, `false`, nil; `main` logs the hint ("models.json не найден — резолвинг через живой каталог; make install скопирует файл") and continues.

- [ ] **Step 4: Verify** — both packages PASS; `go test ./...` full suite green.

- [ ] **Step 5: Commit** — `git commit -m "feat: first run self-creates config, tolerates missing models.json"`

### Task 3: `genmodels -settings` — the CLI settings block

**Files:**
- Modify: `cmd/genmodels/main.go` (options struct, flag wiring, run)
- Test: `cmd/genmodels/main_test.go`

**Interfaces:**
- Consumes: existing `run()` and `model` fields (`Picker`, `Label`, `BehavesAs`, `Available`).
- Produces: `-settings` flag and `-proxy-url` (default `http://127.0.0.1:4000`); `writeSettings(w io.Writer, proxyURL string, models []model) error`.

- [ ] **Step 1: Failing test**

```go
func TestWriteSettings(t *testing.T) {
	models := []model{
		{ID: "glm-5.3", Picker: "claude-GL-5.3", Label: "GLM 5.3", BehavesAs: "claude-sonnet-5", Available: true},
		{ID: "blocked", Picker: "claude-Blocked", Label: "Blocked", BehavesAs: "claude-sonnet-5", Available: false},
	}
	var out bytes.Buffer
	if err := writeSettings(&out, "http://127.0.0.1:4000", models); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Env         map[string]string `json:"env"`
		ModelPicker struct {
			ReplaceBuiltInOptions bool `json:"replaceBuiltInOptions"`
			Options               []struct {
				Model     string `json:"model"`
				Label     string `json:"label"`
				BehavesAs string `json:"behavesAs"`
			} `json:"options"`
		} `json:"modelPicker"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.String())
	}
	if doc.Env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:4000" {
		t.Fatalf("base url = %q", doc.Env["ANTHROPIC_BASE_URL"])
	}
	if !doc.ModelPicker.ReplaceBuiltInOptions {
		t.Fatal("replaceBuiltInOptions must be true")
	}
	if len(doc.ModelPicker.Options) != 1 || doc.ModelPicker.Options[0].Model != "claude-GL-5.3" {
		t.Fatalf("options = %+v, want only the available model", doc.ModelPicker.Options)
	}
}
```

- [ ] **Step 2: Verify it fails** — `go test ./cmd/genmodels/ -run TestWriteSettings` → FAIL (undefined).

- [ ] **Step 3: Implement** — `writeSettings` marshals `{"env":{"ANTHROPIC_BASE_URL":proxyURL,"ANTHROPIC_AUTH_TOKEN":"<your OpenCode Go token>"},"modelPicker":{"replaceBuiltInOptions":true,"options":[…]}}` with one option per `Available` model (`model: Picker`, `label: Label`, `behavesAs: BehavesAs`), indented, trailing newline. Flags: `-settings` (bool) makes `run` print the block to stdout and return before writing `models.json`; `-proxy-url` feeds it.

- [ ] **Step 4: Verify** — `go test ./cmd/genmodels/` PASS.

- [ ] **Step 5: Commit** — `git commit -m "feat: genmodels -settings emits the CLI model picker block"`

### Task 4: Statusline CLI mode

**Files:**
- Modify: `statusline/main.go` (session resolution in `main`)
- Test: `statusline/main_test.go` (new)

**Interfaces:**
- Consumes: existing `cliSessionID`, `fetchCost`.
- Produces: `resolveSessionID(id string) (string, error)` — `local_…` → existing glob lookup; non-empty bare id → returned as-is (CLI mode); empty → error.

- [ ] **Step 1: Live contract check (before any code).** Run the Claude Code CLI through the proxy (`ANTHROPIC_BASE_URL=http://127.0.0.1:4000 claude -p "hi"`), then `grep session= var/proxy.log`. If the logged session id equals the id the CLI passes to the statusline (`~/.claude` statusline debug), the assumption holds; if not, stop and re-scope this task.

- [ ] **Step 2: Failing test**

```go
func TestResolveSessionID(t *testing.T) {
	id := "6968d8fb-1a2b-3c4d-9e0f-112233445566"
	if got, err := resolveSessionID(id); err != nil || got != id {
		t.Fatalf("bare CLI id: got (%q, %v), want it passed through", got, err)
	}
	if _, err := resolveSessionID(""); err == nil {
		t.Fatal("empty id must error into the 'no requests' path")
	}
}
```

- [ ] **Step 3: Verify it fails** — `go test ./statusline/ -run TestResolveSessionID` → FAIL (undefined).

- [ ] **Step 4: Implement** — extract `resolveSessionID`; `main` calls it instead of `cliSessionID` (the `local_` branch inside keeps today's glob). No behavior change for desktop ids.

- [ ] **Step 5: Verify** — `go test ./statusline/` PASS; `go build ./statusline` OK.

- [ ] **Step 6: Commit** — `git commit -m "feat: statusline CLI mode passes bare session ids through"`

### Task 5: systemd packaging and OS-aware Makefile

**Files:**
- Create: `deploy/opencode-proxy.service.template`
- Modify: `Makefile`

**Interfaces:**
- Consumes: Task 1 paths (Linux config dir `~/.config/opencode-proxy`), Task 2 self-init.
- Produces: `make install` / `restart` / `uninstall` work on both OSes; `CONFIG_DIR` variable; `make models` writes `$(CONFIG_DIR)/models.json`.

- [ ] **Step 1: Write the unit template** — standard systemd user unit with `__HOME__` placeholders:

```ini
[Unit]
Description=opencode-proxy: OpenCode Go gateway for Claude clients
After=network-online.target

[Service]
ExecStart=__HOME__/.local/bin/opencode-proxy -config __HOME__/.config/opencode-proxy/config.json
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
```

- [ ] **Step 2: Makefile OS branches** — `OS := $(shell uname -s)`; `CONFIG_DIR`/`STATE_DIR` per spec paths (darwin: `$(HOME)/opencode-proxy`, else `$(HOME)/.config/opencode-proxy` and `$(HOME)/.local/state/opencode-proxy/var`); `models:` target writes `-out $(CONFIG_DIR)/models.json` (macOS result unchanged); new `install-linux` (build → `mkdir -p` config/state dirs and `~/.local/bin` → install binary → copy repo `config.json`/`models.json` into `$(CONFIG_DIR)` only if absent → render unit → `systemctl --user daemon-reload && systemctl --user enable --now ai.opencode-proxy` → echo the linger hint `loginctl enable-linger`); `restart-linux`/`uninstall-linux` via `systemctl --user`; root `install`/`restart`/`uninstall` dispatch on `$(OS)` — the darwin branches keep today's commands verbatim.

- [ ] **Step 3: Verify macOS is untouched** — `make -n install` on this Mac shows exactly today's launchd flow; `make -n models` writes to `$(HOME)/opencode-proxy/models.json`.

- [ ] **Step 4: Verify Linux flow statically** — `make -n install-linux` shows the binary, config copy, unit render and `systemctl --user` sequence; if `systemd-analyze verify` exists locally, run it on the rendered unit (optional, skip on macOS without systemd).

- [ ] **Step 5: Commit** — `git commit -m "feat: systemd user unit and OS-aware make install"`

### Task 6: CI workflow

**Files:**
- Create: `.github/workflows/test.yml`

**Interfaces:**
- Consumes: nothing; runs the existing suite.
- Produces: green CI badge data on every push.

- [ ] **Step 1: Write the workflow**

```yaml
name: test
on:
  push:
  pull_request:
jobs:
  test:
    strategy:
      fail-fast: false
      matrix:
        os: [ubuntu-latest, macos-latest, windows-latest]
    continue-on-error: ${{ matrix.os == 'windows-latest' }}
    runs-on: ${{ matrix.os }}
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: go build ./...
      - run: go test ./...
      # -race needs cgo on windows; unix runners get it.
      - run: go test -race ./...
        if: runner.os != 'Windows'
```

- [ ] **Step 2: Commit and push, verify CI** — `git commit -m "ci: test matrix on ubuntu, macos, windows"`; push; `gh run watch` (or check the Actions page) → ubuntu and macos green; windows result is informational (may be green already — tests use `t.TempDir`/`filepath.Join` only).

### Task 7: README for two platforms

**Files:**
- Modify: `README.md`

**Interfaces:**
- Consumes: everything above (make targets, `-settings`, statusline CLI mode).

- [ ] **Step 1: Rework the platform sections** — intro line: supported matrix instead of "macOS only" (macOS: full desktop-app integration; Linux: Claude Code CLI; Windows: coming); Quick start splits into **macOS** (unchanged text) and **Linux** (`git clone` → `make install` → `genmodels -settings |` paste block into `~/.claude/settings.json` → optional `make models` re-probe for the region → statusline/search notes); "first run creates its config" called out once; the daily `make timer` section marked macOS-only for now.

- [ ] **Step 2: Verify by read-through** — every command in the Linux path exists in the Makefile at this commit; no reference to the tracked personal `config.json` as a source.

- [ ] **Step 3: Commit** — `git commit -m "docs: README for macOS and Linux"`

---

## Out of scope (deliberate)

- systemd **timer** for the nightly models refresh on Linux — a small follow-up task after the unit ships (spec §2).
- Windows phase 2: service manager choice, Windows desktop-app paths, PowerShell smoke — separate design (spec, last section).
