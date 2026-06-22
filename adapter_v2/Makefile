# BBClaw adapter_v2 — PTY-based universal CLI bridge (see DESIGN.md).
#
# Independent of adapter/ (v1): its own module, its own binary, its own Makefile.
# Targets mirror v1's shape (build / test / e2e) so muscle memory carries over.
#
#   make build   # compile the adapter-v2 binary into bin/
#   make test    # go test ./...                 (fast, no external deps)
#   make e2e     # device end-to-end voice round-trip smoke (issue #212)
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

# `make run` knobs (all overridable):
#   ADDR          listen address           (default :18090)
#   CLI           CLI to drive under a PTY (default claude; auto-resolves the install)
#   CWD           working dir for that CLI (default: the butler workspace)
#   CLOUD_WS_URL  BBClaw Cloud relay URL   (default: SaaS prod; set empty for LAN-only)
ADDR         ?= :18090
CLI          ?= claude
CWD          ?=
CLOUD_WS_URL ?= wss://bbclaw.daboluo.cc/ws

.PHONY: help build run test e2e clean

help:
	@echo "BBClaw adapter_v2"
	@echo ""
	@echo "  make run     # build + start in SaaS mode (cloud relay), open http://localhost$(ADDR)"
	@echo "  make build   # build $(BIN_DIR)/$(BIN_NAME)"
	@echo "  make test    # go test ./... (fast, no external deps)"
	@echo "  make e2e     # device end-to-end voice round-trip smoke"
	@echo "  make clean   # remove build artifacts"
	@echo ""
	@echo "  CLOUD_WS_URL= make run                    # LAN-only (no cloud relay)"
	@echo "  make run CLI=bash CWD=~/proj ADDR=:9000   # drive a shell instead of claude"
	@echo "  Override the Go toolchain: make test GO=/path/to/go"

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
