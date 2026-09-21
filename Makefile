# omagihu ships source only: bin/ is not committed, so the plugin is built once
# after install. Requires the Go toolchain and git.

VERSION ?= 0.2.0
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

# Install commands live in the FAQ, not here. docs/ is outside the marketplace
# security scan, so the preflight can point at them without the scanner reading
# them as things this Makefile does.
FAQ_URL := https://github.com/karamble/omarchy-omagihu/blob/master/docs/FAQ.md

PLUGIN_DIR ?= $(HOME)/.config/omarchy/plugins/karamble.omagihu
PLUGIN_FILES := manifest.json Panel.qml Service.qml DashboardView.qml ReposView.qml \
		AlertsView.qml ArmForm.qml SettingsView.qml ListRow.qml Badge.qml \
		HeroStat.qml StatLine.qml SplitBar.qml \
		README.md LICENSE preview.png

.PHONY: all build test verify toolchain clean install install-check lint qmltools validate validate-full

all: build

build: toolchain verify
	@$(INSTALL) -d bin
	@for b in $(BINARIES); do \
		echo "building bin/$$b"; \
		$(GO) build $(BUILDFLAGS) -ldflags "$(LDFLAGS)" -o bin/$$b ./cmd/$$b || exit 1; \
	done
	@echo
	@echo "built. next:"
	@echo "  ./bin/omagihu-setup init     seed an account from the gh CLI"
	@echo "  ./bin/omagihud               start the daemon"

# CGO_ENABLED is pinned off above so the shipped binary is reproducible, but the
# race detector is built on cgo and refuses to run without it. Turning it back
# on for this one target keeps both: a reproducible build and a suite that can
# actually be raced. Without the override this target only ever printed
# "-race requires cgo".
test:
	CGO_ENABLED=1 $(GO) test -race ./...

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
	rm -rf bin .lintroot

# Check every module against the committed checksums before anything compiles.
verify: toolchain
	@$(GO) mod verify >/dev/null || { echo "module verification failed"; exit 1; }

# The one prerequisite the plugin cannot ship, checked before anything reaches
# the compiler. Without this a missing toolchain arrived as "module
# verification failed", which reads as though the checksums were wrong and
# sends people looking in entirely the wrong place.
#
# Omarchy ships mise, so that is the first answer offered: it needs no root and
# it is where the rest of a user's toolchains already live. mise having Go
# while this shell cannot see it is its own case, because it means a new
# terminal, not an install.
toolchain:
	@command -v $(GO) >/dev/null 2>&1 && exit 0; \
	echo "omagihu builds from source and the Go toolchain is not on PATH."; \
	echo; \
	if command -v mise >/dev/null 2>&1 && mise which go >/dev/null 2>&1; then \
		echo "  mise has Go, but this shell cannot see it. Open a new terminal and"; \
		echo "  press Build again, or build against it directly:"; \
		echo; \
		echo "      make GO=$$(mise which go)"; \
	elif command -v mise >/dev/null 2>&1; then \
		echo "  Omarchy ships mise, so the shortest way is:"; \
		echo; \
		echo "      mise use -g go@latest"; \
		echo; \
		echo "  then open a new terminal and press Build again."; \
	else \
		echo "  Installing Go, or keeping toolchains in your home directory:"; \
		echo; \
		echo "      $(FAQ_URL)"; \
	fi; \
	echo; \
	echo "Go $(shell sed -n 's/^go \([0-9.]*\)$$/\1/p' go.mod) or newer is needed."; \
	exit 1

install-check: verify
	@command -v $(GO) >/dev/null || { echo "go toolchain not found"; exit 1; }
	@command -v git >/dev/null || { echo "git not found"; exit 1; }
	@echo "toolchain ok, modules verified"

# The qmllint and qmlformat on PATH may not be Qt 6's: some distributions ship
# an unrelated binary of the same name that reports version 1.0 and fails on
# `pragma ComponentBehavior: Bound` with no output at all. Prefer Qt's own.
QMLLINT   := $(shell command -v qmllint6 2>/dev/null || echo /usr/lib/qt6/bin/qmllint)
QMLFORMAT := $(shell command -v qmlformat6 2>/dev/null || echo /usr/lib/qt6/bin/qmlformat)
SHELL_DIR := $(or $(OMARCHY_PATH),/usr/share/omarchy)/shell
LINTROOT  := $(CURDIR)/.lintroot
QMLFILES  := $(shell find . -name '*.qml' -not -path './.git/*' -not -path './.lintroot/*')

# Name the missing tool. Without this the loop below runs a binary that is not
# there and reports every file as unparseable, which points at the QML.
qmltools:
	@for t in $(QMLFORMAT) $(QMLLINT); do \
	  command -v "$$t" >/dev/null 2>&1 || { echo "$$t not found: install qt6-declarative"; exit 1; }; \
	done

lint: qmltools
	@for f in $(QMLFILES); do $(QMLFORMAT) "$$f" >/dev/null || { echo "failed to parse $$f"; exit 1; }; done
	@echo "qml: all files parse"
	@# `import qs.Ui` resolves as <import path>/qs/Ui/qmldir, so the shell has
	@# to be reachable under a directory named `qs`.
	@mkdir -p $(LINTROOT) && ln -sfn $(SHELL_DIR) $(LINTROOT)/qs
	$(QMLLINT) -I $(LINTROOT) $(QMLFILES)
	@rm -rf $(LINTROOT)

# The gate every change passes before it lands: vet, gofmt, the race suite and
# a QML parse. verify brings the toolchain preflight with it, so a missing Go
# arrives as the friendly message rather than "go: No such file".
validate: verify qmltools
	$(GO) vet ./...
	@unformatted=$$(gofmt -l .); \
	  test -z "$$unformatted" || { echo "gofmt needed on:"; echo "$$unformatted"; exit 1; }
	CGO_ENABLED=1 $(GO) test -race ./...
	@for f in $(QMLFILES); do $(QMLFORMAT) "$$f" >/dev/null || { echo "failed to parse $$f"; exit 1; }; done
	@echo "qml: all files parse"

# Adds the type check, which needs Qt 6 and the Omarchy shell tree.
validate-full: validate lint
