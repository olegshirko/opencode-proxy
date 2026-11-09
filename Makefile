SHELL := /bin/sh

BIN     := $(CURDIR)/bin/opencode-proxy
NEW     := $(CURDIR)/bin/opencode-proxy.new
SL      := $(CURDIR)/bin/statusline
CONFIG  := $(CURDIR)/config.json
PLIST   := $(HOME)/Library/LaunchAgents/ai.opencode.proxy.plist
LABEL   := ai.opencode.proxy
UID     := $(shell id -u)

# Only this module's packages: the worktree may hold other people's scratch
# directories, and they must not be able to fail this build.
PKGS := . ./cmd/... ./internal/... ./statusline

.PHONY: build statusline test smoke install restart uninstall verify models

statusline:
	@mkdir -p $(CURDIR)/bin
	go build -o $(SL) ./statusline

build:
	@mkdir -p $(CURDIR)/bin
	go build -o $(NEW) .

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
