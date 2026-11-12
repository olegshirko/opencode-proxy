# opencode-proxy

English | [Русский](README.ru.md)

A local reverse proxy that opens the [OpenCode Go](https://opencode.ai/docs/console/go)
model catalog — 30 models: GLM, DeepSeek, Kimi, Qwen, MiniMax and friends — to Claude
Desktop and Claude Code, and keeps them working under real traffic: parallel agents get
their own upstream cells, gateway refusals are retried behind the scenes, and every cent
of the monthly allowance is accounted for. One Go binary, no dependencies.

| Platform | Support |
|---|---|
| macOS | Full: service, desktop-app profile, status line, search, nightly refresh |
| Linux | Full for Claude Code CLI: systemd user service, model picker, status line, search, nightly refresh |
| Windows | Coming (phase 2) |

## Why

Out of the box, Claude Desktop points at the OpenCode Go gateway but exposes a single
model, five parallel subagents collide on one upstream cell (the gateway answers every
request with `429 prefill_cap_refused` while the UI hangs in silence), and nothing tells
you how much of the plan is left. The proxy fixes exactly that:

| Without | With the proxy |
|---|---|
| One forced model | All 30 catalog models in the picker, each on its own native id |
| Parallel agents refuse each other | Every agent chain gets its own cell; the main thread always wins the queue |
| UI hangs while the gateway is busy | Retries with SSE keepalive pings — the client always sees progress |
| No idea what the plan allows | 5-hour / weekly / monthly windows, verified against the OpenCode panel |
| `WebSearch` not available behind a gateway | Search bridge: the desktop app connector and a Claude Code MCP tool |
| Cost invisible | Status line: real session cost plus W/D/M percentages, live |

<!-- screenshot: picker dropdown (docs/img/picker.png) -->

![generated model list in the app's gateway profile](docs/img/model-list.webp)

<!-- screenshot: status line (docs/img/statusline.png) -->

<!-- screenshot: make install output (docs/img/install.png) -->

## Quick start from zero

Requirements: [Go 1.27+](https://go.dev), git. Everything below runs on the user's
machine; nothing listens outside loopback. The first start creates its own config file
with working defaults — no hand-written configuration needed.

### macOS

```bash
brew install go                      # skip if Go is already there
git clone https://github.com/olegshirko/opencode-proxy.git
cd opencode-proxy
make install                         # tests → smoke run → launchd agent on 127.0.0.1:4000
make verify                          # the admin status answers; the proxy is alive
```

`make install` never touches the running service until the candidate passes its test
suite and answers on a spare port; in-flight requests drain on restart.

**The desktop app.** Claude Desktop talks to the proxy through a gateway profile:
`inferenceGatewayBaseUrl: http://127.0.0.1:4000`. The `claude-config` skill in this repo
generates the whole profile from `models.json` (picker entries, pricing, context
windows). Without the skill, a one-field profile pointing at the proxy works too —
the model list is then served live at `GET /v1/models`. The Claude Code CLI uses the
same proxy — see [Claude Code](#claude-code) below.

**Keep it fresh (optional).** `models.json` regenerates itself every night at 04:17 —
the launchd timer fetches the live catalog and probes availability; the running proxy
re-reads the file within seconds, no restart. Install it (it is not installed by
`make install`):

```bash
make timer                           # runs daily; log: var/genmodels.log
make timer-uninstall
```

### Linux

```bash
git clone https://github.com/olegshirko/opencode-proxy.git
cd opencode-proxy
make install                         # tests → smoke run → systemd user service on 127.0.0.1:4000
make verify                          # the admin status answers; the proxy is alive
```

The service runs as your user (`systemctl --user`), installs to `~/.local/bin`, keeps
config in `~/.config/opencode-proxy` and state in `~/.local/state/opencode-proxy`.
For a headless machine: `loginctl enable-linger`.

Then set up the CLI in [Claude Code](#claude-code) below — the steps are identical
on both platforms.

**Keep it fresh (optional).** `make timer` installs a systemd user timer — the same
nightly refresh as on macOS (daily 04:17, catches up after downtime). Log:
`journalctl --user -u ai.opencode-proxy.models.service`. `make timer-uninstall`
removes it.

**Update**: `git pull && make install`.

## Claude Code

The proxy speaks the Anthropic API, so Claude Code needs no patches or wrappers —
just two environment variables. Identical on macOS and Linux. Prerequisites: an
active OpenCode Go subscription and a running proxy (`make install` + `make verify`
from the quick start above).

### 1. Point Claude Code at the proxy

Install the CLI if you don't have it (`npm install -g @anthropic-ai/claude-code`),
then merge this into `~/.claude/settings.json` — keep whatever is already there:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:4000",
    "ANTHROPIC_AUTH_TOKEN": "<your OpenCode Go token>"
  }
}
```

The token comes from your [OpenCode Go](https://opencode.ai/docs/console/go)
subscription; the proxy forwards it to the gateway untouched, and no Claude.ai login
is involved. For a one-off run you can export the same two variables in the shell
instead of editing the file.

Restart Claude Code and send a message — it already goes through the proxy. Without
a picker, the built-in model names resolve to `models.default` from `config.json`,
and background tasks (`claude-haiku-*`) to `models.background`.

### 2. Add the model picker

The picker replaces Claude Code's built-in model list with the gateway catalog:

```bash
make models                          # probes availability with the token from step 1
make settings                        # prints env + modelPicker
```

Merge the printed block into `~/.claude/settings.json`, replace the token
placeholder, restart Claude Code — `/model` now lists the catalog, each entry on its
own native id. Models your region cannot serve are excluded; re-run both commands
any time to re-probe. The proxy picks up the regenerated `models.json` within
seconds, no restart.

### 3. Status line (optional)

Real session cost plus the 5-hour / weekly / monthly allowance percentages, live:

```bash
make statusline
```

Then in `~/.claude/settings.json`:

```json
{
  "statusLine": { "type": "command", "command": "/path/to/opencode-proxy/bin/statusline" }
}
```

### 4. Web search (optional)

`WebSearch` is not available behind a gateway; the MCP bridge restores it:

```bash
go build -o bin/oc-search-mcp ./cmd/oc-search-mcp
claude mcp add --scope user oc-search -- "$PWD/bin/oc-search-mcp"
```

The bridge calls a local OpenCode CLI (`opencode api`) — install
[OpenCode](https://opencode.ai) if you want search.

### Check that it works

```bash
make verify                          # the proxy is alive
tail -f var/proxy.log                # one line per Claude Code request
```

## What each part is for

```
Claude Desktop / Claude Code CLI
        │  Anthropic API (/v1/messages)
        ▼
opencode-proxy  127.0.0.1:4000
        │
        ▼
https://opencode.ai/inference/go/anthropic
```

Every box in that middle line is one package under `internal/`, each with a reason to
exist:

| Package | Why it exists |
|---|---|
| `proxy` | The request handler: resolves the model, streams SSE back, owns the retry loop, and serves the admin API, `/v1/models` discovery, `/search` and the local router |
| `models` | The registry: maps `claude-*` picker names to native gateway ids. The picked model is never silently swapped — every mapping is logged with its reason |
| `scheduler` | Per-chain cell keys and a priority queue: parallel subagents stop refusing each other by prefill, the main thread never waits behind background work, and the concurrency ceiling is learned from the gateway's own refusals |
| `retry` | The retry ladder with jitter: honors `Retry-After` and the gateway's `x-should-retry` verdict instead of guessing |
| `quota` | Turns each response's allowance into 5-hour / weekly / monthly windows and the `anthropic-ratelimit-*` headers — the percentages match the OpenCode panel to the whole percent |
| `coststore` | Per-session cost that survives restarts: a returned-to session never shows a zeroed counter |
| `catalog` | The live gateway model list behind `/v1/models`, so a model added upstream today works today, without regenerating anything |
| `reqlog` | One key=value line per request (models, why, status, duration, tokens, cost, cell) plus opt-in full traffic capture to a rotated `.jsonl` |
| `thinking` | Collapsible thinking plates: a one-line summary is injected into the stream, the raw text stays untouched |
| `config` | One JSON config with hot reload: edits and PATCHes apply without a restart, atomic save means a crash never corrupts it |
| `atomicio` | The crash-safe file writes the state stores share |

![live admin status and log tail](docs/img/admin-terminal.png)

## Configuration

Everything lives in `config.json`; edits apply on the fly (mtime watch), and `PATCH
/admin/config` works from scripts:

| Section | What it decides |
|---|---|
| `listen`, `upstream` | Where the proxy listens and which gateway it forwards to |
| `models` | Default and background models, the alias table, per-model routes, catalog URL |
| `retry` | Attempt count, wait ladder, per-attempt and total budget |
| `scheduler` | Cell ceiling, queue timeout, keepalive intervals, fallback rotation |
| `limits` | The plan's monthly limit and window fractions the quota headers are computed from |
| `log` | Log file, rotation, level, and the four capture switches |
| `admin` | Enable the REST surface, optional `X-Admin-Token` |

State lives in `var/` next to the config: the request log, the capture file, the quota
ledger and per-session costs. It is runtime data, not source.

## Admin API

| Method | Path | What it does |
|---|---|---|
| `GET`  | `/admin/status` | health, cells, queue, counters, quota windows |
| `GET` / `PATCH` | `/admin/config` | read / partially update the config |
| `GET` / `PUT` | `/admin/models/aliases` | inspect / replace the alias table |
| `POST` | `/admin/reload` | re-read `config.json` |
| `GET`  | `/admin/session-cost?session=…` | per-session cost, requests, per-model breakdown |

## Operations

```bash
make restart      # drain in-flight requests, relaunch
make verify       # admin status as JSON
git pull && make install   # update: tests gate the swap, the service never goes down untested
tail -f var/proxy.log       # one line per request
```

`launchctl kickstart -k gui/$(id -u)/ai.opencode.proxy` restarts the service the hard
way; `make restart` is the polite path.

## Design docs

- [`docs/2026-11-07-opencode-proxy-design.md`](docs/2026-11-07-opencode-proxy-design.md) — the full design: resolver order, retry policy, logging, admin API
- [`docs/2026-11-07-agent-cells-and-native-compat.md`](docs/2026-11-07-agent-cells-and-native-compat.md) — cell keys, scheduler, quota windows, thinking plates
