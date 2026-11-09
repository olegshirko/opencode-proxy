SHELL := /bin/sh

OS := $(shell uname -s)

BIN     := $(CURDIR)/bin/opencode-proxy
NEW     := $(CURDIR)/bin/opencode-proxy.new
SL      := $(CURDIR)/bin/statusline
GEN     := $(CURDIR)/bin/genmodels
CONFIG  := $(CURDIR)/config.json
PLIST   := $(HOME)/Library/LaunchAgents/ai.opencode.proxy.plist
TIMER   := $(HOME)/Library/LaunchAgents/ai.opencode.proxy.models.plist
LABEL   := ai.opencode.proxy
TIMER_LABEL := ai.opencode.proxy.models
SERVICE := ai.opencode-proxy
UID     := $(shell id -u)

# Where the running proxy keeps its config and state. macOS keeps the
# historical layout; everything else follows XDG (internal/config/paths.go).
ifeq ($(OS),Darwin)
CONFIG_DIR := $(HOME)/opencode-proxy
STATE_DIR  := $(HOME)/opencode-proxy/var
else
CONFIG_DIR := $(HOME)/.config/opencode-proxy
STATE_DIR  := $(HOME)/.local/state/opencode-proxy/var
endif
LIVE_CONFIG := $(CONFIG_DIR)/config.json

# Only this module's packages: the worktree may hold other people's scratch
# directories, and they must not be able to fail this build.
PKGS := ./cmd/... ./internal/... ./statusline

.PHONY: build statusline genmodels test smoke install install-linux restart restart-linux uninstall uninstall-linux verify models timer timer-uninstall

statusline:
	@mkdir -p $(CURDIR)/bin
	go build -o $(SL) ./statusline

genmodels:
	@mkdir -p $(CURDIR)/bin
	go build -o $(GEN) ./cmd/genmodels

build:
	@mkdir -p $(CURDIR)/bin
	go build -o $(NEW) ./cmd/opencode-proxy

test:
	go test -race $(PKGS)

# Starts the candidate on a spare port and leaves the running proxy alone.
smoke: build
	@sh ./scripts/smoke.sh $(NEW) $(CONFIG)

# Nothing is swapped unless the tests pass and the candidate answers.
ifeq ($(OS),Darwin)
install: test smoke
	@mv $(NEW) $(BIN)
	@sed -e 's|__HOME__|$(HOME)|g' -e 's|__BIN__|$(BIN)|g' \
		deploy/ai.opencode.proxy.plist.template > $(PLIST).new
	@if cmp -s $(PLIST).new $(PLIST); then rm -f $(PLIST).new; \
	else mv $(PLIST).new $(PLIST); touch $(PLIST).changed; fi
	@sh ./scripts/restart.sh
	@echo "installed $(BIN); run: make verify"

# Relaunches the installed binary. In-flight requests drain first.
restart:
	@sh ./scripts/restart.sh

uninstall:
	@launchctl bootout gui/$(UID)/$(LABEL) 2>/dev/null || true
	@rm -f $(PLIST)
	@echo "removed $(LABEL)"
else
install: install-linux

restart: restart-linux

uninstall: uninstall-linux
endif

# Linux: systemd user unit, no root. The config file self-creates from
# defaults on first start; models.json ships from the repo.
install-linux: test smoke
	@mv $(NEW) $(BIN)
	@mkdir -p $(HOME)/.local/bin $(CONFIG_DIR) $(STATE_DIR)
	@install -m 0755 $(BIN) $(HOME)/.local/bin/opencode-proxy
	@if [ ! -f "$(CONFIG_DIR)/models.json" ]; then cp $(CURDIR)/models.json $(CONFIG_DIR)/models.json; fi
	@mkdir -p $(HOME)/.config/systemd/user
	@sed -e 's|__HOME__|$(HOME)|g' deploy/opencode-proxy.service.template \
		> $(HOME)/.config/systemd/user/$(SERVICE).service
	@systemctl --user daemon-reload
	@systemctl --user enable --now $(SERVICE).service
	@echo "installed $(HOME)/.local/bin/opencode-proxy; run: make verify"
	@echo "for headless machines: loginctl enable-linger"

restart-linux:
	@systemctl --user restart $(SERVICE).service

uninstall-linux:
	@systemctl --user disable --now $(SERVICE).service 2>/dev/null || true
	@rm -f $(HOME)/.config/systemd/user/$(SERVICE).service
	@systemctl --user daemon-reload
	@echo "removed $(SERVICE)"

verify:
	@curl -sf http://127.0.0.1:4000/admin/status | python3 -m json.tool

# Regenerates models.json in the directory the running proxy reads, then
# probes availability. The proxy re-reads the file by mtime.
models: genmodels
	go run ./cmd/genmodels -out $(CONFIG_DIR)/models.json -probe -token-file $(HOME)/.claude/settings.json

# Installs the daily models.json refresh (macOS launchd timer for now; Linux
# gets a systemd timer as a follow-up). Every night the timer regenerates the
# file from the live catalog and probes availability; the running proxy picks
# the file up by mtime, no restart involved.
timer: genmodels
	@if [ "$(OS)" != "Darwin" ]; then echo "make timer is macOS-only for now (systemd timer is a follow-up)"; exit 1; fi
	@mkdir -p $(CURDIR)/var
	@sed -e 's|__HOME__|$(HOME)|g' deploy/ai.opencode.proxy.models.plist.template > $(TIMER)
	@launchctl bootout gui/$(UID)/$(TIMER_LABEL) 2>/dev/null || true
	@launchctl bootstrap gui/$(UID) $(TIMER)
	@echo "installed $(TIMER); runs daily 04:17; log: var/genmodels.log"

timer-uninstall:
	@launchctl bootout gui/$(UID)/$(TIMER_LABEL) 2>/dev/null || true
	@rm -f $(TIMER)
	@echo "removed $(TIMER_LABEL)"
