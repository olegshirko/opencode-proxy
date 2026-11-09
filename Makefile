SHELL := /bin/sh

BIN     := $(CURDIR)/bin/opencode-proxy
NEW     := $(CURDIR)/bin/opencode-proxy.new
SL      := $(CURDIR)/bin/statusline
GEN     := $(CURDIR)/bin/genmodels
CONFIG  := $(CURDIR)/config.json
PLIST   := $(HOME)/Library/LaunchAgents/ai.opencode.proxy.plist
TIMER   := $(HOME)/Library/LaunchAgents/ai.opencode.proxy.models.plist
LABEL   := ai.opencode.proxy
TIMER_LABEL := ai.opencode.proxy.models
UID     := $(shell id -u)

# Only this module's packages: the worktree may hold other people's scratch
# directories, and they must not be able to fail this build.
PKGS := ./cmd/... ./internal/... ./statusline

.PHONY: build statusline genmodels test smoke install restart uninstall verify models timer timer-uninstall

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

verify:
	@curl -sf http://127.0.0.1:4000/admin/status | python3 -m json.tool

# Regenerates models.json from the OpenCode Go catalog and probes availability.
models:
	go run ./cmd/genmodels -out models.json -probe -token-file $(HOME)/.claude/settings.json

# Installs the daily models.json refresh: every night the timer regenerates
# the file from the live catalog and probes availability; the running proxy
# picks the file up by mtime, no restart involved.
timer: genmodels
	@mkdir -p $(CURDIR)/var
	@sed -e 's|__HOME__|$(HOME)|g' deploy/ai.opencode.proxy.models.plist.template > $(TIMER)
	@launchctl bootout gui/$(UID)/$(TIMER_LABEL) 2>/dev/null || true
	@launchctl bootstrap gui/$(UID) $(TIMER)
	@echo "installed $(TIMER); runs daily 04:17; log: var/genmodels.log"

timer-uninstall:
	@launchctl bootout gui/$(UID)/$(TIMER_LABEL) 2>/dev/null || true
	@rm -f $(TIMER)
	@echo "removed $(TIMER_LABEL)"
