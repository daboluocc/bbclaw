# BBClaw adapter — 两种运行模式，配置分别从文件读入（通过 shell 导出为环境变量，供 Go 的 os.Getenv 使用）
#
#   make run-adapter   # 本地 HTTP 服务 bbclaw-adapter，默认读 $(PWD)/.env
#   make run-home      # 家庭 relay bbclaw-home-adapter，默认读 $(PWD)/.env.home
#
# 覆盖配置文件路径：
#   make run-adapter ENV_FILE=/path/to/custom.env
#   make run-home    ENV_FILE=/path/to/custom.env

SHELL := /bin/bash

GO       ?= go
BIN_DIR  ?= $(CURDIR)/bin
REPO_ROOT ?= $(abspath $(CURDIR)/..)

# 与仓库根 Makefile 一致的注入版本信息
BBCLAW_BUILD_TAG  ?= $(shell git -C "$(REPO_ROOT)" describe --tags --always --dirty 2>/dev/null || echo dev)
BBCLAW_BUILD_TIME ?= $(shell date -u +%Y%m%d-%H%M)
GO_LDFLAGS := -X github.com/zhoushoujianwork/bbclaw/adapter/internal/buildinfo.Tag=$(BBCLAW_BUILD_TAG) -X github.com/zhoushoujianwork/bbclaw/adapter/internal/buildinfo.BuildTime=$(BBCLAW_BUILD_TIME)

LOG_DIR  ?= $(CURDIR)/tmp
LOG_ADAPTER ?= $(LOG_DIR)/adapter-runtime.log
LOG_HOME    ?= $(LOG_DIR)/home-adapter-runtime.log
ENV_ADAPTER ?= $(CURDIR)/.env
ENV_HOME    ?= $(CURDIR)/.env.home

.PHONY: help build test run run-adapter run-home dev dev-adapter dev-home log log-adapter log-home

help:
	@echo "BBClaw adapter — 单二进制，通过 ADAPTER_MODE 环境变量区分模式"
	@echo ""
	@echo "  make run-adapter   # ADAPTER_MODE=local（默认），配置: $(ENV_ADAPTER)"
	@echo "  make run-home      # ADAPTER_MODE=cloud，配置: $(ENV_HOME)"
	@echo "  make dev-adapter / dev-home  # 与 run-* 相同（别名）"
	@echo "  make log-home / log-adapter  # tail -f 运行日志"
	@echo "  make build         # 构建到 $(BIN_DIR)/bbclaw-adapter"
	@echo "  make test          # go test ./..."
	@echo ""
	@echo "覆盖配置文件: make run-home ENV_FILE=/path/to/x.env"
	@echo "示例文件: .env.example / .env.home.example"

build:
	@mkdir -p "$(BIN_DIR)"
	cd "$(CURDIR)" && $(GO) build -ldflags "$(GO_LDFLAGS)" -o "$(BIN_DIR)/bbclaw-adapter" ./cmd/bbclaw-adapter

test:
	cd "$(CURDIR)" && $(GO) test ./...

# 运行前从文件加载：set -a 使 sourced 变量自动 export；使用 go run 便于开发退出（见 homeadapter ctx 关闭 conn）
run-adapter:
	@_ENV="$(ENV_FILE)"; \
	if [ -n "$$_ENV" ]; then CFG="$$_ENV"; else CFG="$(ENV_ADAPTER)"; fi; \
	test -f "$$CFG" || { echo "missing $$CFG — copy from .env.example"; exit 1; }; \
	mkdir -p "$(LOG_DIR)"; \
	set -a; \
	. "$$CFG"; \
	set +a; \
	echo "=== adapter start $$(date -u +%Y-%m-%dT%H:%M:%SZ) ===" >> "$(LOG_ADAPTER)"; \
	echo "log: $(LOG_ADAPTER)"; \
	$(GO) run -ldflags "$(GO_LDFLAGS)" ./cmd/bbclaw-adapter 2>&1 | tee -a "$(LOG_ADAPTER)"

run-home:
	@_ENV="$(ENV_FILE)"; \
	if [ -n "$$_ENV" ]; then CFG="$$_ENV"; else CFG="$(ENV_HOME)"; fi; \
	test -f "$$CFG" || { echo "missing $$CFG — copy from .env.home.example"; exit 1; }; \
	mkdir -p "$(LOG_DIR)"; \
	set -a; \
	. "$$CFG"; \
	set +a; \
	export ADAPTER_MODE=cloud; \
	echo "=== home-adapter start $$(date -u +%Y-%m-%dT%H:%M:%SZ) ===" >> "$(LOG_HOME)"; \
	echo "log: $(LOG_HOME)"; \
	$(GO) run -ldflags "$(GO_LDFLAGS)" ./cmd/bbclaw-adapter 2>&1 | tee -a "$(LOG_HOME)"

dev-adapter: run-adapter
dev-home: run-home

log-adapter:
	@tail -f "$(LOG_ADAPTER)"

log-home:
	@tail -f "$(LOG_HOME)"

log: log-home

# 默认列出帮助
run:
	@$(MAKE) help
