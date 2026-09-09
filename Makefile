# omagihu ships source only: bin/ is not committed, so the plugin is built once
# after install. Requires the Go toolchain and git.

VERSION ?= 0.1.0
LDFLAGS := -X main.version=$(VERSION)
BINARIES := omagihud omagihu omagihu-setup

PLUGIN_DIR ?= $(HOME)/.config/omarchy/plugins/karamble.omagihu
PLUGIN_FILES := manifest.json Panel.qml DashboardView.qml ReposView.qml AlertsView.qml \
		ArmForm.qml SettingsView.qml ListRow.qml Badge.qml README.md LICENSE preview.png

.PHONY: all build test clean install install-check

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
