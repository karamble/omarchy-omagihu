# omagihu ships source only: bin/ is not committed, so the plugin is built once
# after install. Requires the Go toolchain and git.

VERSION ?= 0.1.1
LDFLAGS := -X main.version=$(VERSION)

# Every tool is named through a variable, and the two that only ever do one
# thing are named absolutely, so a build does not depend on what PATH resolves
# them to. A packager can override any of them.
GO      ?= go
INSTALL ?= /usr/bin/install

# Pin the compiler. Without this the go command will fetch a different
# toolchain over the network to satisfy the directive in go.mod; with it the
# build uses the toolchain that is installed, or fails and says so.
export GOTOOLCHAIN = local

# Go turns cgo on whenever it finds a C compiler and off when it does not, so
# the same source silently produces different binaries on different machines,
# and picks a different DNS resolver with it. Pin it off: no C toolchain is
# needed, and the pure Go resolver is the one that reads resolv.conf directly.
export CGO_ENABLED = 0

# readonly refuses to edit go.mod or go.sum during a build, so every module is
# the one the committed checksums name. trimpath keeps the output free of local
# paths, and buildvcs=false keeps the git revision out, so the bytes depend on
# the source and the toolchain and nothing else.
BUILDFLAGS := -trimpath -mod=readonly -buildvcs=false
BINARIES := omagihud omagihu omagihu-setup

PLUGIN_DIR ?= $(HOME)/.config/omarchy/plugins/karamble.omagihu
PLUGIN_FILES := manifest.json Panel.qml Service.qml DashboardView.qml ReposView.qml \
		AlertsView.qml ArmForm.qml SettingsView.qml ListRow.qml Badge.qml \
		README.md LICENSE preview.png

.PHONY: all build test verify clean install install-check

all: build

build: verify
	@$(INSTALL) -d bin
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
	@$(INSTALL) -d "$(PLUGIN_DIR)/bin"
	@$(INSTALL) -m 0644 $(PLUGIN_FILES) "$(PLUGIN_DIR)/"
	@# install writes through a fresh inode, so a running daemon holding the old
	@# binary open does not block the replacement, which plain cp would.
	@$(INSTALL) -m 0755 bin/* "$(PLUGIN_DIR)/bin/"
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
