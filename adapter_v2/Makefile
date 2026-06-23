# BBClaw adapter_v2 — PTY-based universal CLI bridge (see DESIGN.md).
#
# Independent of adapter/ (v1): its own module, its own binary, its own Makefile.
# Targets mirror v1's shape (build / test / e2e) so muscle memory carries over.
#
#   make web     # build the Vue/Vite web SPA into web/dist (needs Node/npm)
#   make build   # compile the adapter-v2 binary into bin/
#   make test    # go test ./...                 (fast, no external deps)
#   make e2e     # device end-to-end voice round-trip smoke (issue #212)
#
# NOTE: web/dist is committed and embedded via `//go:embed all:dist` (web/web.go),
# so `make build` works on a normal checkout without Node. Run `make web` only
# after editing web/spa, then commit the regenerated web/dist.
#
# Go is not on PATH in the dev environment, so GO defaults to the Homebrew
# location and is overridable:  make test GO=/usr/local/go/bin/go

SHELL := /bin/bash

# Prefer an explicit GO; else the Homebrew install; else fall back to PATH.
GO ?= $(shell command -v go 2>/dev/null || echo /opt/homebrew/bin/go)

CURDIR  ?= $(shell pwd)
BIN_DIR ?= $(CURDIR)/bin
BIN_NAME ?= bbclaw-adapter-v2

# Version stamps injected via -ldflags -X (matches v1's adapter Makefile). The
# tag comes from `git describe` locally; release CI overrides BBCLAW_BUILD_TAG
# with the release version (the v* git tag).
REPO_ROOT         ?= $(abspath $(CURDIR)/..)
BBCLAW_BUILD_TAG  ?= $(shell git -C "$(REPO_ROOT)" describe --tags --always --dirty 2>/dev/null || echo dev)
BBCLAW_BUILD_TIME ?= $(shell date -u +%Y%m%d-%H%M)
GO_LDFLAGS := -X github.com/daboluocc/bbclaw/adapter_v2/internal/buildinfo.Tag=$(BBCLAW_BUILD_TAG) -X github.com/daboluocc/bbclaw/adapter_v2/internal/buildinfo.BuildTime=$(BBCLAW_BUILD_TIME)

# Web SPA build knobs. Node is often not on the default PATH; NPM defaults to the
# Homebrew install. Override with `make web NPM=/path/to/npm`.
WEB_DIR ?= $(CURDIR)/web/spa
NPM     ?= $(shell command -v npm 2>/dev/null || echo /opt/homebrew/bin/npm)

# `make run` knobs (all overridable):
#   ADDR          listen address           (default :18090)
#   CLI           CLI to drive under a PTY (default claude; auto-resolves the install)
#   CWD           working dir for that CLI (default: the butler workspace)
#   CLOUD_WS_URL  BBClaw Cloud relay URL   (default: SaaS prod; set empty for LAN-only)
ADDR         ?= :18090
CLI          ?= claude
CWD          ?=
CLOUD_WS_URL ?= wss://bbclaw.daboluo.cc/ws

.PHONY: help web build run test e2e clean

help:
	@echo "BBClaw adapter_v2"
	@echo ""
	@echo "  make run     # build + start in SaaS mode (cloud relay), open http://localhost$(ADDR)"
	@echo "  make web     # build the web SPA into web/dist (needs Node/npm)"
	@echo "  make build   # build $(BIN_DIR)/$(BIN_NAME)"
	@echo "  make test    # go test ./... (fast, no external deps)"
	@echo "  make e2e     # device end-to-end voice round-trip smoke"
	@echo "  make clean   # remove build artifacts"
	@echo ""
	@echo "  CLOUD_WS_URL= make run                    # LAN-only (no cloud relay)"
	@echo "  make run CLI=bash CWD=~/proj ADDR=:9000   # drive a shell instead of claude"
	@echo "  Override the Go toolchain: make test GO=/path/to/go"

# Build the Vue 3 + Vite + TypeScript SPA (web/spa) into web/dist, which is
# committed and embedded via `//go:embed all:dist` in web/web.go. This is a
# PREREQUISITE for a clean-checkout `go build` only when web/spa changed — the
# generated web/dist is committed, so a normal build does not run this. Node/npm
# must be available (NPM defaults to the Homebrew install).
web:
	cd "$(WEB_DIR)" && "$(NPM)" install && "$(NPM)" run build

build:
	@mkdir -p "$(BIN_DIR)"
	$(GO) build -trimpath -ldflags "$(GO_LDFLAGS)" -o "$(BIN_DIR)/$(BIN_NAME)" ./cmd/bbclaw-adapter-v2

# Build then run the server in the foreground (Ctrl-C to stop). Open
# http://localhost$(ADDR) in a browser to take over the CLI's real terminal;
# refresh the page to reconnect to the same live session.
#
# CLI is resolved to a real binary: PATH first, then the standard Claude Code
# local install (~/.claude/local/claude), else used verbatim — so `make run`
# works even though `claude` is often only a shell alias.
run: build
	@CLI_BIN="$$(command -v $(CLI) 2>/dev/null || true)"; \
	if [ -z "$$CLI_BIN" ] && [ -x "$$HOME/.claude/local/claude" ] && [ "$(CLI)" = "claude" ]; then \
		CLI_BIN="$$HOME/.claude/local/claude"; \
	fi; \
	[ -n "$$CLI_BIN" ] || CLI_BIN="$(CLI)"; \
	MODE="SaaS (relay → $(CLOUD_WS_URL))"; [ -n "$(CLOUD_WS_URL)" ] || MODE="LAN only (/v2/dev/ws)"; \
	echo "▶ adapter_v2  →  http://localhost$(ADDR)   mode: $$MODE"; \
	echo "  CLI=$$CLI_BIN   CWD=$${CWD:-<butler workspace>}"; \
	CLOUD_WS_URL="$(CLOUD_WS_URL)" ADAPTER_V2_ADDR="$(ADDR)" ADAPTER_V2_CLI="$$CLI_BIN" ADAPTER_V2_CWD="$(CWD)" "$(BIN_DIR)/$(BIN_NAME)"

# Fast suite: every package's unit + integration tests, no process-spawn smoke.
test:
	$(GO) test ./...

# Device end-to-end smoke (no external deps): compiles the mock upstream CLI
# (cmd/mockcli), spawns it under a real PTY, and drives one full voice turn —
# injected ASR text → mock CLI reply → boundary fires → TTS invoked → audio at
# the device sink. Gated behind the `e2e` build tag so it only runs here.
#
# -count=1 disables the test cache so the smoke really re-runs (it spawns a child
# process and is the thing we want to actually exercise on demand).
e2e:
	$(GO) test -tags e2e -count=1 -run 'TestE2E' ./...

clean:
	@rm -rf "$(BIN_DIR)"
