# omagihu ships source only: bin/ is not committed, so the plugin is built once
# after install. Requires the Go toolchain and git.

VERSION ?= 0.1.0
LDFLAGS := -X main.version=$(VERSION)
BINARIES := omagihud omagihu omagihu-setup

PLUGIN_DIR ?= $(HOME)/.config/omarchy/plugins/karamble.omagihu
PLUGIN_FILES := manifest.json Panel.qml DashboardView.qml ReposView.qml AlertsView.qml \
		ArmForm.qml SettingsView.qml ListRow.qml Badge.qml README.md LICENSE preview.png

.PHONY: all build test clean install install-check skill skill-check

all: build

build:
	@mkdir -p bin
	@for b in $(BINARIES); do \
		echo "building bin/$$b"; \
		go build -ldflags "$(LDFLAGS)" -o bin/$$b ./cmd/$$b || exit 1; \
	done
	@echo
	@echo "built. next:"
	@echo "  ./bin/omagihu-setup init     seed an account from the gh CLI"
	@echo "  ./bin/omagihud               start the daemon"

test:
	go test -race ./...

# Omarchy refuses symlinks inside a plugin folder, so installing copies the
# QML, the manifest and the built binaries into place.
install: build
	@mkdir -p "$(PLUGIN_DIR)/bin"
	@cp --remove-destination $(PLUGIN_FILES) "$(PLUGIN_DIR)/"
	@# --remove-destination unlinks first, so a running daemon does not block the copy
	@cp --remove-destination bin/* "$(PLUGIN_DIR)/bin/"
	@echo "installed to $(PLUGIN_DIR)"
	@echo "enable it with: omarchy plugin enable karamble.omagihu left"

clean:
	rm -rf bin

install-check:
	@command -v go >/dev/null || { echo "go toolchain not found"; exit 1; }
	@command -v git >/dev/null || { echo "git not found"; exit 1; }
	@echo "toolchain ok"

# The skill's catalogue section is generated from alerts.Catalogue(), so the
# paths an agent is told about cannot drift from the ones the daemon offers.
# The skill is embedded, so regenerating it means rebuilding after.
skill: build
	./bin/omagihu-setup skill --generate skill/SKILL.md
	$(MAKE) build

# Fails when the committed skill is stale, which is the only way to notice that
# the catalogue moved and the skill did not.
skill-check: build
	@cp skill/SKILL.md .skill.orig
	@./bin/omagihu-setup skill --generate skill/SKILL.md >/dev/null
	@if ! cmp -s skill/SKILL.md .skill.orig; then \
		cp .skill.orig skill/SKILL.md; rm -f .skill.orig; \
		echo "skill/SKILL.md is stale: run make skill"; exit 1; \
	fi
	@rm -f .skill.orig
	@echo "skill catalogue is current"
