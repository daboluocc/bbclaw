# ESP-IDF build/flash/monitor helpers for BBClaw firmware.

SHELL := /bin/bash
IDF_PATH ?= $(HOME)/esp/esp-idf
IDF_SOURCE = source "$(IDF_PATH)/export.sh" 2>/dev/null &&
TARGET ?= esp32s3

# 本地快速发版从这台云端机器取 OTA admin key（CLOUD_OTA_ADMIN_KEY）。可覆盖：
#   make release-local VERSION=vX.Y.Z OTA_DEPLOY_HOST=root@other-host
OTA_DEPLOY_HOST ?= root@daboluo.cc

# Serial port for flash/monitor/erase.
# Auto-detect: newest USB serial device (macOS: cu.*/tty.*, Linux: ttyUSB*/ttyACM*).
# Precedence: command-line PORT=... > env BBCLAW_PORT > auto-detect.
# Override per-invocation: make flash PORT=/dev/cu.usbmodem12401
# Or export once:          export BBCLAW_PORT=/dev/cu.usbmodem12401
# Force esptool/idf.py auto-detect: make flash PORT=
# List currently detected USB serial devices: make list-ports
USB_PORT_GLOBS := /dev/cu.usbmodem* /dev/cu.usbserial* /dev/cu.wchusbserial* \
                  /dev/tty.usbmodem* /dev/tty.usbserial* \
                  /dev/ttyUSB* /dev/ttyACM*
DEFAULT_PORT := $(shell ls -t $(USB_PORT_GLOBS) 2>/dev/null | head -n 1)
PORT ?= $(if $(BBCLAW_PORT),$(BBCLAW_PORT),$(DEFAULT_PORT))
ESPTOOL_PORT_ARG := $(if $(PORT),--port $(PORT),)
IDF_PORT_ARG := $(if $(PORT),-p $(PORT),)

LOG_DIR := .cache
MONITOR_LOG := $(LOG_DIR)/idf-monitor.latest.log
MONITOR_FILTER ?= ^$$
MONITOR_FILTER_CMD := rg -v "$(MONITOR_FILTER)" 2>/dev/null || true

SIM_DIR := $(CURDIR)/simulator
SIM_BUILD_DIR := $(SIM_DIR)/build
SIM_ARGS ?=
SIM_FEEDBACK_DIR := $(CURDIR)/.cache/sim-feedback

.PHONY: help idf-env init set-target reconfigure build flash boot-recover erase monitor monitor-log monitor-log-filtered monitor-last monitor-errors menuconfig size clean fullclean all gen gen-lv-font gen-lvgl-assets gen-lvgl-elements lvgl-kconfig-fix sim-config sim-build sim-run sim-export-feedback set-board list-ports release-local

help:
	@echo "BBClaw firmware (ESP-IDF)"
	@echo "  make init          # set esp32s3 target + reconfigure"
	@echo "  make build         # idf.py build"
	@echo "  make flash         # build + flash to ota_0 partition (writes proper otadata)"
	@echo "  make boot-recover  # rewrite otadata only, recover from boot-loop after bad flash"
	@echo "  make monitor       # idf.py monitor"
	@echo "  make all           # build + flash + monitor-log"
	@echo "  make release-local                  # 本地快速发版：自动版本(OTA active+1)+取key+生产构建+推OTA"
	@echo "  make list-ports    # list USB serial devices (pick one for PORT=...)"
	@echo "  make fullclean     # 清 build；若 idf.py 报 Python 环境与 configure 不一致，再 make init && make build"
	@echo "  make set-board BOARD=bbclaw             # 切换 board（breadboard | bbclaw | atk-dnesp32s3-box）"
	@echo "  make gen             # 一键生成所有 LVGL 资源（字库 + SVG 位图 + 元素位图）"
	@echo "  make lvgl-kconfig-fix  # 打开压缩字库 + LARGE 索引（屏上全方框时执行一次后 reconfigure）"
	@echo "  make sim-build       # 构建 macOS/SDL2 本地 LVGL 预览"
	@echo "  make sim-run         # 运行本地 LVGL 预览（可配 SIM_ARGS='--mode speaking'）"
	@echo "  make sim-export-feedback # 导出几张长文本预览图到 .cache/sim-feedback/"

idf-env:
	@echo "IDF_PATH=$(IDF_PATH)"
	@$(IDF_SOURCE) idf.py --version

init:
	@$(IDF_SOURCE) idf.py set-target $(TARGET)
	@$(IDF_SOURCE) idf.py reconfigure

set-target:
	@$(IDF_SOURCE) idf.py set-target $(TARGET)

reconfigure:
	@$(IDF_SOURCE) idf.py reconfigure

# Switch board: make set-board BOARD=bbclaw
# Then: make build (or make fullclean && make init && make build)
BOARD ?= breadboard
set-board:
	@python3 scripts/set_board.py $(BOARD)

# Show all USB serial devices currently visible to macOS.
# Use the printed path as PORT=... or export BBCLAW_PORT=... to pin it.
list-ports:
	@ports=$$(ls -t $(USB_PORT_GLOBS) 2>/dev/null); \
		if [ -z "$$ports" ]; then \
			echo "[list-ports] no USB serial devices found (plug in the board?)"; \
		else \
			echo "[list-ports] USB serial devices (newest first):"; \
			for p in $$ports; do echo "  $$p"; done; \
			echo "[list-ports] selected default: $(DEFAULT_PORT)"; \
			echo "[list-ports] override with: make flash PORT=/dev/cu.usbmodemXXXX"; \
		fi

build:
	@# Keep the project-root copy of partitions_ota.csv in sync with boards/bbclaw/.
	@# boards/bbclaw/ is the source of truth; the root copy is what ESP-IDF reads.
	@if [ -f boards/bbclaw/partitions_ota.csv ]; then \
		if ! cmp -s boards/bbclaw/partitions_ota.csv partitions_ota.csv 2>/dev/null; then \
			cp boards/bbclaw/partitions_ota.csv .; \
			echo "[build] synced partitions_ota.csv from boards/bbclaw/ to project root"; \
		fi; \
	fi
	@$(IDF_SOURCE) idf.py build

# 本地快速发版（默认发布路线）：本机用生产配置构建固件 + 生成 OTA bundle + 推到 OTA
# 服务器，跳过 GitHub Actions 的慢 round-trip。设备下次 /v1/ota/check 即升级（全量，非灰度）。
# 自动：① 内层 make build 自带 $(IDF_SOURCE) 自源 ESP-IDF（无需先 get_idf）；
#       ② 未设 OTA_ADMIN_KEY 时自动 ssh $(OTA_DEPLOY_HOST) 从生产 cloud.env 取；
#       ③ 不传 VERSION 时按 OTA 当前 active 版本 patch+1 + -g<短哈希>[-dirty] 自动定版。
# 约定：tag→CI 发布（push v* tag 触发 release.yml）只在显式要求时做；默认走本地快速发版这条。
#   make release-local                                # 自动版本（OTA active +1，如 v0.5.19-gabc123）
#   make release-local VERSION=v0.5.19                # 手动指定版本
#   make release-local RELEASE_ARGS=--skip-build      # 复用已构建产物
#   make release-local RELEASE_ARGS=--gh-release      # 顺带传 GitHub Release
#   OTA_ADMIN_KEY=xxx make release-local              # 手动给 key，跳过 ssh
release-local:
	@set -e; \
	if [ -z "$$OTA_ADMIN_KEY" ]; then \
		echo "[release-local] OTA_ADMIN_KEY 未设置，从 $(OTA_DEPLOY_HOST) 取 …"; \
		OTA_ADMIN_KEY=$$(ssh -o BatchMode=yes -o ConnectTimeout=8 $(OTA_DEPLOY_HOST) 'grep "^CLOUD_OTA_ADMIN_KEY=" /opt/bbclaw-cloud/config/cloud.env | cut -d= -f2-'); \
		export OTA_ADMIN_KEY; \
		if [ -n "$$OTA_ADMIN_KEY" ]; then echo "[release-local] key 已取 (len=$${#OTA_ADMIN_KEY})"; \
		else echo "::warning:: 取 key 失败 → 仅构建+stage、跳过 OTA 推送（可手动 export OTA_ADMIN_KEY 重跑）"; fi; \
	fi; \
	scripts/release_local.sh $(VERSION) $(RELEASE_ARGS)

# Flash via the IDF default path: bootloader + partition table + ota_data_initial
# (all-0xFF, bootloader picks factory) + firmware → factory.
#
# Partition layout (actual, see boards/bbclaw/partitions_ota.csv):
#   0x00000 - 0x07FFF : bootloader     (~21 KB used)
#   0x08000 - 0x08FFF : partition table
#   0x09000 - 0x0EFFF : nvs            (24 KB)
#   0x0F000 - 0x0FFFF : phy_init       (4 KB)
#   0x10000 - 0x11FFF : otadata        (8 KB)
#   0x20000 - 0x29FFFF : factory app   (2.5 MB — `make flash` target)
#   0x2A0000 - 0x51FFFF: ota_0         (2.5 MB — OTA upgrade slot A)
#   0x520000 - 0x79FFFF: ota_1         (2.5 MB — OTA upgrade slot B)
#   0x7A0000 - 0x7FFFFF: resources     (384 KB)
flash: build
	@echo "Flashing factory partition (0x20000)... port=$(if $(PORT),$(PORT),auto)"
	@pkill -f "idf_monitor.py" 2>/dev/null || true
	@sleep 1
	@$(IDF_SOURCE) python3 -m esptool --chip esp32s3 $(ESPTOOL_PORT_ARG) -b 460800 --before default_reset --after hard_reset \
		write_flash \
		--flash_mode dio --flash_size 8MB --flash_freq 80m \
		0x0 build/bootloader/bootloader.bin \
		0x8000 build/partition_table/partition-table.bin \
		0x10000 build/ota_data_initial.bin \
		0x20000 build/bbclaw_firmware.bin

# Emergency: device boot-loops because otadata points at an invalid slot.
# Resets otadata to all-0xFF so the bootloader picks factory again.
boot-recover: build
	@echo "Recovering otadata (reset to factory)... port=$(if $(PORT),$(PORT),auto)"
	@pkill -f "idf_monitor.py" 2>/dev/null || true
	@sleep 1
	@$(IDF_SOURCE) python3 -m esptool --chip esp32s3 $(ESPTOOL_PORT_ARG) -b 460800 --before default_reset --after hard_reset \
		write_flash 0x10000 build/ota_data_initial.bin

erase:
	@pkill -f "idf_monitor.py" 2>/dev/null || true
	@sleep 1
	@echo "[erase] port=$(if $(PORT),$(PORT),auto)"
	@$(IDF_SOURCE) idf.py $(IDF_PORT_ARG) erase-flash

monitor:
	@pkill -f "idf_monitor.py" 2>/dev/null || true
	@sleep 1
	@mkdir -p "$(LOG_DIR)"
	@echo "[monitor] logging to $(MONITOR_LOG) port=$(if $(PORT),$(PORT),auto)"
	@$(IDF_SOURCE) idf.py $(IDF_PORT_ARG) monitor 2>&1 | tee "$(MONITOR_LOG)"

monitor-log:
	@pkill -f "idf_monitor.py" 2>/dev/null || true
	@sleep 1
	@mkdir -p "$(LOG_DIR)"
	@: > "$(MONITOR_LOG)"
	@echo "[monitor-log] overwrite -> $(MONITOR_LOG) port=$(if $(PORT),$(PORT),auto)"
	@$(IDF_SOURCE) idf.py $(IDF_PORT_ARG) monitor 2>&1 | tee "$(MONITOR_LOG)"

monitor-log-filtered:
	@mkdir -p "$(LOG_DIR)"
	@: > "$(MONITOR_LOG)"
	@echo "[monitor-log-filtered] overwrite -> $(MONITOR_LOG), hide regex=$(MONITOR_FILTER)"
	@$(IDF_SOURCE) idf.py $(IDF_PORT_ARG) monitor 2>&1 | tee "$(MONITOR_LOG)" | $(MONITOR_FILTER_CMD)

monitor-last:
	@test -f "$(MONITOR_LOG)" || (echo "[monitor-last] log not found: $(MONITOR_LOG)" && exit 1)
	@tail -n 120 "$(MONITOR_LOG)"

monitor-errors:
	@test -f "$(MONITOR_LOG)" || (echo "[monitor-errors] log not found: $(MONITOR_LOG)" && exit 1)
	@rg -n "^[EW] \\([0-9]+\\)" "$(MONITOR_LOG)" 2>/dev/null || true

menuconfig:
	@$(IDF_SOURCE) idf.py menuconfig

size:
	@$(IDF_SOURCE) idf.py size

clean:
	@$(IDF_SOURCE) idf.py clean

fullclean:
	@$(IDF_SOURCE) idf.py fullclean

all: build flash monitor-log

# 已有 sdkconfig 时 defaults 不会覆盖「is not set」；一键打开 lv_font_conv 所需选项
lvgl-kconfig-fix:
	@python3 -c "import pathlib; p=pathlib.Path('sdkconfig'); t=p.read_text(); \
t=t.replace('# CONFIG_LV_USE_FONT_COMPRESSED is not set','CONFIG_LV_USE_FONT_COMPRESSED=y'); \
t=t.replace('# CONFIG_LV_FONT_FMT_TXT_LARGE is not set','CONFIG_LV_FONT_FMT_TXT_LARGE=y'); \
p.write_text(t); print('lvgl kconfig: patched sdkconfig (run: make reconfigure build)')"

# 一键生成所有 LVGL 资源（字库 + SVG 位图 + 元素位图）
gen: gen-lv-font gen-lvgl-assets gen-lvgl-elements

# 与 lvgl.io Font Converter 同源引擎（lv_font_conv），无需手搓网页
gen-lv-font:
	@python3 "$(CURDIR)/scripts/gen_lvgl_cjk_font.py"

gen-lvgl-assets:
	@python3 "$(CURDIR)/scripts/gen_lvgl_assets_from_svg.py"

gen-lvgl-elements:
	@python3 "$(CURDIR)/scripts/gen_lvgl_screen_assets_from_png.py" \
		--images-dir "$(CURDIR)/ui/lvgl/elements" \
		--manifest "$(CURDIR)/ui/lvgl/elements_manifest.json" \
		--allow-any-size \
		--out-c "$(CURDIR)/generated/bb_lvgl_element_assets.c" \
		--out-h "$(CURDIR)/generated/bb_lvgl_element_assets.h"
	@echo "[gen-lvgl-elements] If CMake was configured before generated/*.c existed, run: make reconfigure && make build"

sim-config:
	@cmake -S "$(SIM_DIR)" -B "$(SIM_BUILD_DIR)"

sim-build: sim-config
	@cmake --build "$(SIM_BUILD_DIR)" -j

sim-run: sim-build
	@"$(SIM_BUILD_DIR)/bbclaw_lvgl_sim" $(SIM_ARGS)

sim-export-feedback: sim-build
	@mkdir -p "$(SIM_FEEDBACK_DIR)"
	@"$(SIM_BUILD_DIR)/bbclaw_lvgl_sim" --mode standby --export "$(SIM_FEEDBACK_DIR)/01-standby.png"
	@"$(SIM_BUILD_DIR)/bbclaw_lvgl_sim" --mode notification --status TASK --reply "这是一段较长的回复文本，用来测试纯文本区域的自动换行和滚动效果。" --export "$(SIM_FEEDBACK_DIR)/02-task-long.png"
	@"$(SIM_BUILD_DIR)/bbclaw_lvgl_sim" --mode speaking --status TX --export "$(SIM_FEEDBACK_DIR)/03-listening.png"
	@"$(SIM_BUILD_DIR)/bbclaw_lvgl_sim" --mode speaking --status RESULT --reply "The audio decode is still running. This preview exists mainly to validate long text blocks and overall spacing." --export "$(SIM_FEEDBACK_DIR)/04-result.png"
	@echo "[sim-export-feedback] exported previews -> $(SIM_FEEDBACK_DIR)"
