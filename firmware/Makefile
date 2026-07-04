# ESP-IDF build/flash/monitor helpers for BBClaw firmware.

SHELL := /bin/bash
IDF_PATH ?= $(HOME)/esp/esp-idf
IDF_SOURCE = source "$(IDF_PATH)/export.sh" 2>/dev/null &&
TARGET ?= esp32s3

# esp_app_desc.version 覆盖钩子：CI 发版（release.yml）显式传 FW_VERSION=vX.Y.Z 钉死
# 发布版本。本地 `make build` 不设，由 stamp-version 从 VERSION 文件 + git 短哈希推导。
FW_VERSION ?=

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

.PHONY: help idf-env init set-target reconfigure stamp-version bump build flash boot-recover erase monitor monitor-log monitor-log-filtered monitor-last monitor-errors menuconfig size clean fullclean all gen gen-lv-font gen-lvgl-assets gen-lvgl-elements lvgl-kconfig-fix sim-config sim-build sim-run sim-export-feedback set-board list-ports

help:
	@echo "BBClaw firmware (ESP-IDF)"
	@echo "  make init          # set esp32s3 target + reconfigure"
	@echo "  make build         # idf.py build (版本 = VERSION 文件 + git 短哈希)"
	@echo "  make bump          # 递增 VERSION 基线并提交 (LEVEL=patch|minor|major)"
	@echo "  发版：make bump && git tag vX.Y.Z && git push origin vX.Y.Z  (CI 构建+推 OTA)"
	@echo "  make flash         # build + flash to ota_0 partition (writes proper otadata)"
	@echo "  make boot-recover  # rewrite otadata only, recover from boot-loop after bad flash"
	@echo "  make monitor       # idf.py monitor"
	@echo "  make all           # build + flash + monitor-log"
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

# Base version (esp_app_desc.version 的真相来源) 放在 git 跟踪的 `VERSION` 文件里，
# 用 `make bump` 递增。dev 构建 stamp 出 <VERSION>-g<短哈希>[-dirty]——每次构建可追溯
# 到 commit，而 OTA 比较的 M.m.p 基线固定在仓库里（不再依赖落后于 OTA 的 git tag，
# 也不再被 gitignore 的残留 version.txt 带偏，这是「版本错乱」的根因）。CI 发版
# （release.yml）用 FW_VERSION=vX.Y.Z 钉死发布版本，覆盖此默认。version.txt 是生成物
# (gitignore)，每次 stamp 从 VERSION 重算，不会残留。
VERSION_FILE := VERSION
stamp-version:
	@v="$(FW_VERSION)"; \
	if [ -z "$$v" ]; then \
		base="$$(cat $(VERSION_FILE) 2>/dev/null | tr -d '[:space:]')"; \
		if [ -n "$$base" ]; then \
			short="$$(git rev-parse --short HEAD 2>/dev/null || echo nogit)"; \
			dirty=""; git diff --quiet HEAD 2>/dev/null || dirty="-dirty"; \
			v="$${base}-g$${short}$${dirty}"; \
		else \
			v="$$(git describe --tags --dirty --always 2>/dev/null)"; \
		fi; \
	fi; \
	if [ -n "$$v" ]; then \
		if [ "$$(cat version.txt 2>/dev/null)" != "$$v" ]; then \
			printf '%s' "$$v" > version.txt; \
			echo "[version] esp_app_desc.version = $$v"; \
		else \
			echo "[version] esp_app_desc.version = $$v (unchanged)"; \
		fi; \
	else \
		echo "[version] WARN: 无法推导版本(VERSION 缺失且无 git)，沿用现有 version.txt"; \
	fi

# 递增 VERSION 基线并提交（不打 tag、不发布）。LEVEL=patch(默认)|minor|major。
#   make bump               # v0.6.0 → v0.6.1
#   make bump LEVEL=minor   # v0.6.0 → v0.7.0
bump:
	@level="$(LEVEL)"; [ -z "$$level" ] && level=patch; \
	cur="$$(cat $(VERSION_FILE) 2>/dev/null | tr -d '[:space:]' | sed 's/^v//')"; \
	[ -z "$$cur" ] && cur="0.0.0"; \
	IFS=. read -r maj min pat <<<"$$cur"; \
	maj=$${maj:-0}; min=$${min:-0}; pat=$${pat%%[!0-9]*}; pat=$${pat:-0}; \
	case "$$level" in \
		major) maj=$$((maj+1)); min=0; pat=0 ;; \
		minor) min=$$((min+1)); pat=0 ;; \
		patch) pat=$$((pat+1)) ;; \
		*) echo "[bump] LEVEL 必须是 patch|minor|major" >&2; exit 1 ;; \
	esac; \
	new="v$${maj}.$${min}.$${pat}"; \
	printf '%s\n' "$$new" > $(VERSION_FILE); \
	echo "[bump] $(VERSION_FILE): v$${cur} → $$new"; \
	git add $(VERSION_FILE) 2>/dev/null && \
		git commit -q -m "chore(firmware): bump version → $$new" -- $(VERSION_FILE) 2>/dev/null \
		&& echo "[bump] committed $$new" \
		|| echo "[bump] 已写入 $(VERSION_FILE)（未自动提交，手动 git add/commit 即可）"

build: stamp-version
	@# Keep the project-root copy of partitions_ota.csv in sync with boards/bbclaw/.
	@# boards/bbclaw/ is the source of truth; the root copy is what ESP-IDF reads.
	@if [ -f boards/bbclaw/partitions_ota.csv ]; then \
		if ! cmp -s boards/bbclaw/partitions_ota.csv partitions_ota.csv 2>/dev/null; then \
			cp boards/bbclaw/partitions_ota.csv .; \
			echo "[build] synced partitions_ota.csv from boards/bbclaw/ to project root"; \
		fi; \
	fi
	@$(IDF_SOURCE) idf.py build

# 发版统一走 tag → CI：`make bump` 递增 VERSION 并提交，再 `git tag vX.Y.Z && git push
# origin vX.Y.Z` 触发 .github/workflows/release.yml（用生产配置构建固件 + 5 平台 adapter，
# 建 GitHub Release，并用 CI 托管的 OTA_ADMIN_KEY secret 推 OTA）。本地不再直推 OTA——
# 已撤掉 release_local.sh / make release-local（无需在本机持有生产 OTA admin key）。

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
