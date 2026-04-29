# BBClaw adapter — 配置从文件读入（通过 shell 导出为环境变量，供 Go 的 os.Getenv 使用）
#
#   make run       # 运行 bbclaw-adapter，默认读 $(PWD)/.env
#
# 覆盖配置文件路径：
#   make run ENV_FILE=/path/to/custom.env
#
# 运行模式由 .env 中的 ADAPTER_MODE 控制；省略时默认 auto：
#   ADAPTER_MODE=auto   HTTP server + （若配置了 CLOUD_WS_URL）WS relay
#   ADAPTER_MODE=local  仅 HTTP server + ASR/TTS
#   ADAPTER_MODE=cloud  仅 WS relay to BBClaw Cloud

SHELL := /bin/bash

GO       ?= go
BIN_DIR  ?= $(CURDIR)/bin
REPO_ROOT ?= $(abspath $(CURDIR)/..)

# 与仓库根 Makefile 一致的注入版本信息
BBCLAW_BUILD_TAG  ?= $(shell git -C "$(REPO_ROOT)" describe --tags --always --dirty 2>/dev/null || echo dev)
BBCLAW_BUILD_TIME ?= $(shell date -u +%Y%m%d-%H%M)
GO_LDFLAGS := -X github.com/daboluocc/bbclaw/adapter/internal/buildinfo.Tag=$(BBCLAW_BUILD_TAG) -X github.com/daboluocc/bbclaw/adapter/internal/buildinfo.BuildTime=$(BBCLAW_BUILD_TIME)

LOG_DIR  ?= $(CURDIR)/tmp
LOG_FILE ?= $(LOG_DIR)/adapter-runtime.log
ENV_FILE ?= $(CURDIR)/.env
ADAPTER_RUN_MATCH ?= go run .*./cmd/bbclaw-adapter|/bbclaw-adapter($$| )

.PHONY: help build test clean-run-procs run dev log sync-env sync-env-reset

help:
	@echo "BBClaw adapter"
	@echo ""
	@echo "  make sync-env       # 扫描本地 driver + 抽 ~/.openclaw 配置 → 合并写 .env"
	@echo "  make sync-env-reset # 同上，但放弃当前 .env，全新生成（自动备份）"
	@echo "  # 可选：从已有 .env 导入 Volcano ASR 密钥（不会读未指定文件）"
	@echo "  # make sync-env DOUBAO_ENV=<path-to-other-project-env>"
	@echo "  make run            # 运行 bbclaw-adapter，配置: $(ENV_FILE)"
	@echo "  make log            # tail -f 运行日志"
	@echo "  make build          # 构建到 $(BIN_DIR)/bbclaw-adapter"
	@echo "  make test           # go test ./..."
	@echo ""
	@echo "覆盖配置文件: make run ENV_FILE=/path/to/x.env"
	@echo "示例文件: .env.example"

sync-env:
	@$(GO) run -ldflags "$(GO_LDFLAGS)" ./cmd/bbclaw-adapter doctor --fix \
		$(if $(DOUBAO_ENV),--doubao-env "$(DOUBAO_ENV)")

sync-env-reset:
	@$(GO) run -ldflags "$(GO_LDFLAGS)" ./cmd/bbclaw-adapter doctor --fix --reset \
		$(if $(DOUBAO_ENV),--doubao-env "$(DOUBAO_ENV)")

build:
	@mkdir -p "$(BIN_DIR)"
	cd "$(CURDIR)" && $(GO) build -ldflags "$(GO_LDFLAGS)" -o "$(BIN_DIR)/bbclaw-adapter" ./cmd/bbclaw-adapter

test:
	cd "$(CURDIR)" && $(GO) test ./...

clean-run-procs:
	@user_id="$$(id -u)"; \
	pids="$$(pgrep -u "$$user_id" -f '$(ADAPTER_RUN_MATCH)' || true)"; \
	if [ -z "$$pids" ]; then \
		exit 0; \
	fi; \
	echo "stopping existing adapter processes: $$pids"; \
	pkill -TERM -u "$$user_id" -f '$(ADAPTER_RUN_MATCH)' || true; \
	sleep 1; \
	leftover="$$(pgrep -u "$$user_id" -f '$(ADAPTER_RUN_MATCH)' || true)"; \
	if [ -n "$$leftover" ]; then \
		echo "force stopping adapter processes: $$leftover"; \
		pkill -KILL -u "$$user_id" -f '$(ADAPTER_RUN_MATCH)' || true; \
	fi

run: clean-run-procs
	@test -f "$(ENV_FILE)" || { echo "missing $(ENV_FILE) — copy from .env.example"; exit 1; }; \
	mkdir -p "$(LOG_DIR)"; \
	set -a; \
	. "$(ENV_FILE)"; \
	set +a; \
	echo "=== adapter start $$(date -u +%Y-%m-%dT%H:%M:%SZ) ===" >> "$(LOG_FILE)"; \
	echo "log: $(LOG_FILE)"; \
	$(GO) run -ldflags "$(GO_LDFLAGS)" ./cmd/bbclaw-adapter 2>&1 | tee -a "$(LOG_FILE)"

dev: run

log:
	@tail -f "$(LOG_FILE)"
