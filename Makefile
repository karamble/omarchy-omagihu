# omagihu ships source only: bin/ is not committed, so the plugin is built once
# after install. Requires the Go toolchain and git.

VERSION ?= 0.1.0
LDFLAGS := -X main.version=$(VERSION)

# Tools are named through variables so a packager can point them elsewhere.
GO ?= go

# Pin the compiler. Without this the go command will fetch a different
# toolchain over the network to satisfy the directive in go.mod; with it the
# build uses the toolchain that is installed, or fails and says so.
export GOTOOLCHAIN = local

# readonly refuses to edit go.mod or go.sum during a build, so every module is
# the one the committed checksums name. trimpath keeps the output free of local
# paths, so the same inputs give the same bytes.
BUILDFLAGS := -trimpath -mod=readonly
BINARIES := omagihud omagihu omagihu-setup

PLUGIN_DIR ?= $(HOME)/.config/omarchy/plugins/karamble.omagihu
PLUGIN_FILES := manifest.json Panel.qml Service.qml DashboardView.qml ReposView.qml \
		AlertsView.qml ArmForm.qml SettingsView.qml ListRow.qml Badge.qml \
		README.md LICENSE preview.png

.PHONY: all build test verify clean install install-check

all: build

build: verify
	@mkdir -p bin
	@for b in $(BINARIES); do \
		echo "building bin/$$b"; \
		$(GO) build $(BUILDFLAGS) -ldflags "$(LDFLAGS)" -o bin/$$b ./cmd/$$b || exit 1; \
	done
	@echo
	@echo "built. next:"
	@echo "  ./bin/omagihu-setup init     seed an account from the gh CLI"
	@echo "  ./bin/omagihud               start the daemon"

test:
	$(GO) test -race ./...

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

# Check every module against the committed checksums before anything compiles.
verify:
	@$(GO) mod verify >/dev/null || { echo "module verification failed"; exit 1; }

install-check: verify
	@command -v $(GO) >/dev/null || { echo "go toolchain not found"; exit 1; }
	@command -v git >/dev/null || { echo "git not found"; exit 1; }
	@echo "toolchain ok, modules verified"
