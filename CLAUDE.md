# BBClaw Project Rules

## ⚠️ 设计文档优先原则

**设计文档是开发决策的唯一真相来源（Source of Truth）**

- 设计文档位于仓库根目录 `design/` 下
- 所有代码实现必须符合设计文档
- **如有冲突，先解决设计问题再实现代码**，不能以"代码能跑"为由绕过设计
- 新功能设计必须先编写/更新设计文档，再写代码

## Release & Tag Policy

**One tag, one release, both artifacts.** Pushing a `v*` tag to this repo
triggers `.github/workflows/release.yml` which builds the firmware `.bin`
**and** the adapter binary across 5 platforms in parallel, then publishes
a single GitHub Release with all artifacts + SHA256SUMS, and pushes the
firmware to the OTA server in one go.

```bash
# Cut a release:
git tag v0.4.1
git push origin v0.4.1
# → workflow builds, publishes daboluocc/bbclaw releases v0.4.1
# → firmware auto-uploaded to OTA so devices can pull
```

**Only create git tags when:**
- There is a user-facing new feature in the **firmware**, OR
- There is a fix or new feature in the **adapter** binary

**Do NOT tag for:**
- Cloud-only fixes (deployed server-side from `bbclaw-reference`)
- Web portal-only fixes (deployed server-side)
- Internal refactors with no firmware/adapter user-visible change

**Why a single tag:** firmware and adapter ship as a coordinated pair —
device-side (firmware) and host-side (adapter) protocol changes need to
land together so devices and adapters at the same version are guaranteed
compatible. The closed-repo "two separate tag patterns" model
(`v*` for firmware, `adapter/v*` for adapter) was retired with the
adapter migration on 2026-04-27 (ADR-011).

## Project Layout

- `daboluocc/bbclaw` — main public repo:
  - `firmware/` — ESP32 firmware (C, ESP-IDF)
  - `adapter/` — local agent-bridge daemon (Go). Imported from bbclaw-reference 2026-04-27 (commit `bf24299`); module path `github.com/daboluocc/bbclaw/adapter` unchanged.
  - `docs/`, `design/`, `CHANGELOG.md` — public design docs and ADRs
  - GitHub releases ship the adapter binary alongside firmware OTA bins
- `bbclaw-reference` — private repo (gitignored in main repo at `references/bbclaw-reference/`):
  - `cloud/` — cloud backend (Go): auth, billing, multi-tenant routing, ASR/TTS upstream, OTA channels
  - `web/` — web portal (React): account, device dashboard, billing
  - `promo/` — landing/marketing site

## Versioning

- Bump version and add CHANGELOG entry for every meaningful change
- Tag + release only when adapter binary needs to ship (see policy above)

## ESP-IDF Build & Flash

### 环境准备
```bash
# 确保 IDF_PATH 正确
export IDF_PATH=~/esp/esp-idf
source $IDF_PATH/export.sh
```

### Makefile 入口 (cd firmware)
```bash
make help        # 显示所有可用命令

# 构建
make init        # 初始化目标 (首次或切换芯片)
make build       # 编译固件
make clean       # 清理构建
make fullclean   # 完全清理 (Python 环境不一致时使用)

# 烧录
make flash       # build + flash to ota_0 partition

# 监视
make monitor     # 串口监视
make monitor-log # 监视并保存到日志
make monitor-last    # 查看上次监视日志
make monitor-errors  # 只看错误

# 调试
make menuconfig  # ESP-IDF menuconfig
make size       # 查看固件大小

# LVGL 资源
make gen                # 生成所有 LVGL 资源
make gen-lv-font        # 生成字库
make gen-lvgl-assets    # 生成 SVG 位图
make gen-lvgl-elements  # 生成元素位图

# 本地预览
make sim-build          # 构建 macOS/SDL2 预览
make sim-run            # 运行本地预览
make sim-export-feedback  # 导出预览图

# 板子切换
make set-board BOARD=bbclaw  # 切换到 bbclaw 板子
```

### ⚠️ 禁止直接操作设备

**严禁执行以下命令：**
- `make all` — 禁止自动烧录和监视
- `make monitor` — 禁止直接打开串口监视

**允许的操作：**
- `make build` — 编译固件（测试用）
- `make flash` — 仅在明确授权时烧录
- 通过日志文件查看串口输出：`make monitor-last` 或 `tail -n 100 firmware/.cache/idf-monitor.latest.log`

### 日志查看
```bash
# 查看上次监视日志
make monitor-last

# 只看错误
make monitor-errors

# 手动查看日志文件
tail -n 100 firmware/.cache/idf-monitor.latest.log
```
日志文件位置: `firmware/.cache/idf-monitor.latest.log`

### 分区表
- **默认**: `partitions_bbclaw.csv` (factory 3MB, 无 OTA)
- **OTA 版本**: `boards/bbclaw/partitions_ota.csv` (factory 1MB + ota_0/ota_1 各 2.5MB)

OTA 分区表已默认启用在 `boards/bbclaw/sdkconfig.board`。

**OTA 分区布局 (8MB Flash)**:
| 分区 | 大小 | 起始地址 |
|------|------|----------|
| factory | 1MB | 0x10000 |
| ota_0 | 2.5MB | 0x110000 |
| ota_1 | 2.5MB | 0x390000 |
| resources | 1MB | 0x610000 |

### 烧录到指定分区
```bash
# 烧录 bootloader + 分区表 + 固件到 ota_0 (用于 OTA 测试)
python3 -m esptool --chip esp32s3 -b 460800 --before default_reset --after hard_reset \
  write_flash \
  --flash_mode dio --flash_size 8MB --flash_freq 80m \
  0x0 build/bootloader/bootloader.bin \
  0x8000 build/partition_table/partition-table.bin \
  0x110000 build/bbclaw_firmware.bin
```

### 常见构建问题

**1. Python 环境不一致**
```
project was configured with ... python_env ... while ... is currently active
```
解决: `make fullclean && make init && make build`

**2. 分区太小**
```
Image length X doesn't fit in partition length Y
```
解决: 确认使用的是 OTA 分区表 (`partitions_ota.csv`)，固件 2.2MB 需要 2.5MB+ 分区

**3. 组件找不到**
```
fatal error: xxx.h: No such file or directory
```
解决: 检查 `CMakeLists.txt` 的 `REQUIRES` 和 `INCLUDE_DIRS`

### 设备信息
- **芯片**: ESP32-S3 (QFN56) revision v0.2
- **Flash**: 8MB
- **USB Serial**: `/dev/tty.usbmodem2112401`
- **Chip MAC**: `3c:84:27:c7:eb:88`

### OTA 说明
- **仅支持 `cloud_saas` 模式**，`local_home` 不支持 OTA
- 固件启动后通过 `GET /v1/ota/check` 查询更新
- OTA API 在 `references/bbclaw-reference/cloud/internal/ota/`（cloud 仍在闭源仓）
