# ESP-IDF build/flash/monitor helpers for BBClaw firmware.

SHELL := /bin/bash
IDF_PATH ?= $(HOME)/esp/esp-idf
IDF_SOURCE = source "$(IDF_PATH)/export.sh" 2>/dev/null &&
TARGET ?= esp32s3

LOG_DIR := .cache
MONITOR_LOG := $(LOG_DIR)/idf-monitor.latest.log
MONITOR_FILTER ?= ^$$
MONITOR_FILTER_CMD := rg -v "$(MONITOR_FILTER)" 2>/dev/null || true

SIM_DIR := $(CURDIR)/simulator
SIM_BUILD_DIR := $(SIM_DIR)/build
SIM_ARGS ?=
SIM_FEEDBACK_DIR := $(CURDIR)/.cache/sim-feedback

.PHONY: help idf-env init set-target reconfigure build flash flash-ota0 erase monitor monitor-log monitor-log-filtered monitor-last monitor-errors menuconfig size clean fullclean all gen gen-lv-font gen-lvgl-assets gen-lvgl-elements lvgl-kconfig-fix sim-config sim-build sim-run sim-export-feedback set-board

help:
	@echo "BBClaw firmware (ESP-IDF)"
	@echo "  make init          # set esp32s3 target + reconfigure"
	@echo "  make build         # idf.py build"
	@echo "  make flash         # build + flash to ota_0 partition"
	@echo "  make monitor       # idf.py monitor"
	@echo "  make all           # build + flash + monitor-log"
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

build:
	@# Ensure OTA partition table exists
	@if [ ! -f partitions_ota.csv ] && [ -f boards/bbclaw/partitions_ota.csv ]; then \
		cp boards/bbclaw/partitions_ota.csv .; \
		echo "[build] copied partitions_ota.csv to project root"; \
	fi
	@$(IDF_SOURCE) idf.py build

flash: flash-ota0

# Flash to ota_0 partition (firmware 2.2MB > factory 1MB)
flash-ota0: build
	@echo "Flashing to ota_0 partition (0x110000)..."
	@python3 -m esptool --chip esp32s3 -b 460800 --before default_reset --after hard_reset \
		write_flash \
		--flash_mode dio --flash_size 8MB --flash_freq 80m \
		0x0 build/bootloader/bootloader.bin \
		0x8000 build/partition_table/partition-table.bin \
		0x110000 build/bbclaw_firmware.bin

erase:
	@$(IDF_SOURCE) idf.py erase-flash

monitor:
	@pkill -f "idf_monitor.py" 2>/dev/null || true
	@sleep 1
	@mkdir -p "$(LOG_DIR)"
	@echo "[monitor] logging to $(MONITOR_LOG)"
	@$(IDF_SOURCE) idf.py monitor 2>&1 | tee "$(MONITOR_LOG)"

monitor-log:
	@pkill -f "idf_monitor.py" 2>/dev/null || true
	@sleep 1
	@mkdir -p "$(LOG_DIR)"
	@: > "$(MONITOR_LOG)"
	@echo "[monitor-log] overwrite -> $(MONITOR_LOG)"
	@$(IDF_SOURCE) idf.py monitor 2>&1 | tee "$(MONITOR_LOG)"

monitor-log-filtered:
	@mkdir -p "$(LOG_DIR)"
	@: > "$(MONITOR_LOG)"
	@echo "[monitor-log-filtered] overwrite -> $(MONITOR_LOG), hide regex=$(MONITOR_FILTER)"
	@$(IDF_SOURCE) idf.py monitor 2>&1 | tee "$(MONITOR_LOG)" | $(MONITOR_FILTER_CMD)

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

all: build flash-ota0 monitor-log

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
