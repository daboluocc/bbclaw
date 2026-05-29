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
  - `adapter/` — Go Agent Bus daemon with pluggable drivers (Claude Code, OpenCode, Ollama, Aider, OpenClaw). Migrated from bbclaw-reference 2026-04-27 (ADR-011); module path `github.com/daboluocc/bbclaw/adapter`.
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

### AI 烧录权限（2026-05-12 起开放）

AI 可以执行 `make flash` 烧录固件。完整开发闭环（截图 → 改代码 → build → flash → 截图验证）由 AI 自主跑，不再需要用户中转。

**允许 AI 执行：**
- `make build` / `idf.py build` — 编译
- `make flash PORT=...` — 烧录（需要显式 PORT，避免误选 Flipper 等其他设备）
- `make boot-recover` — 修复 boot loop
- `make monitor-last` / `make monitor-log-filtered` / 读 `firmware/.cache/idf-monitor.latest.log` — 看日志
- 通过 device-monitor skill 跑截图、按键注入、UI 迭代

**仍然慎用 / 别用：**
- `make monitor` 直接前台运行 — 会**永久阻塞终端**，AI session 会卡死；要看实时日志用 `make monitor-log` 后台跑 + 读 cache 文件
- `make all` — 等价于 build+flash+monitor，会触发 monitor 阻塞
- 任何**破坏性**操作（`make fullclean` 之外的强制初始化、修改 bootloader 等）仍需用户确认

**TinyUSB 烧录流程**（参见 ADR-015 和 device-monitor skill）：

**正常情况（推荐）**：AI 通过协议命令远程触发设备进 ROM bootloader，全自动，0 按键：
```bash
PORT=$(ls /dev/cu.usbmodem*3 | head -1)
python3 firmware/scripts/devmon_reboot.py --port $PORT --wait-for-bootloader
make flash PORT=/dev/cu.usbmodem2124401   # AI 跑
# 设备自动重启进新固件，TinyUSB CDC 重新枚举
```

**异常恢复**（仅以下情况需要手动）：
- 首次安装 `REQ_REBOOT_TO_BOOTLOADER` 之前的旧固件 → 一次性 BOOT+RESET 升级
- 固件在 boot 早期就崩，TinyUSB 起不来 → BOOT+RESET 救回
- 极少数 chip 状态卡死

手动序列（仅恢复用）：
1. 按住 BOOT 键，短按 RESET，松开 BOOT
2. 等 `/dev/cu.usbmodem2124401` 出现
3. `make flash PORT=/dev/cu.usbmodem2124401`
4. 烧完单独按一下 RESET 启动新固件

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
- **OTA 版本**: `boards/bbclaw/partitions_ota.csv` (factory / ota_0 / ota_1 各 2.5MB)

OTA 分区表已默认启用在 `boards/bbclaw/sdkconfig.board`。`boards/bbclaw/partitions_ota.csv` 是真相来源，`make build` 会用 `cmp` 检测后自动同步到项目根目录（ESP-IDF 实际读的那份）。

**OTA 分区布局 (8MB Flash)**:
| 分区 | 大小 | 起始地址 |
|------|------|----------|
| factory | 2.5MB | 0x020000 |
| ota_0 | 2.5MB | 0x2A0000 |
| ota_1 | 2.5MB | 0x520000 |
| resources | 384KB | 0x7A0000 |

三个 app 槽尺寸一致，`make flash` 走 IDF 标准路径烧 factory，OTA 升级落到 ota_0 / ota_1。

### 烧录到指定分区
```bash
# 默认 `make flash` 已经走 factory（足够装 2.5MB 固件）。
# 仅当想手动测试 OTA 流程时才需要指定 ota_0：
python3 -m esptool --chip esp32s3 -b 460800 --before default_reset --after hard_reset \
  write_flash \
  --flash_mode dio --flash_size 8MB --flash_freq 80m \
  0x0 build/bootloader/bootloader.bin \
  0x8000 build/partition_table/partition-table.bin \
  0x2A0000 build/bbclaw_firmware.bin
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
- OTA 服务端 API 由 Cloud Backend 提供（不在本仓库内）

## Adapter 日志查看

```bash
cd adapter

# 实时追踪运行日志
make log
# 等价于 tail -f tmp/adapter-runtime.log
```

日志文件位置: `adapter/tmp/adapter-runtime.log`

## Cross-Component Protocol Sync

BBClaw's Adapter defines the canonical protocol (WebSocket envelopes, HTTP API contracts, audio streaming format). A Cloud Backend exists as a relay between remote devices and the Adapter. When changing protocol-level contracts in this repo, the Cloud side must stay in sync.

**Check the following when modifying:**

| Change in this repo | Also verify |
|------|------|
| Adapter HTTP API (`/v1/agent/*`, `/v1/ota/*`) | Cloud relay proxy passes new fields correctly |
| WebSocket envelope format (`homeadapter/`) | Cloud hub routing handles new envelope kinds |
| Notification payload fields (`notifications.go`) | Cloud hub forwarding + Firmware WS handler |
| Session API changes (`agent.go`) | Cloud agent proxy + Firmware session client |
| New agent proxy request kind | Cloud `handleRequest` dispatch + route registration |

When in doubt, search for the envelope `kind` string or HTTP path across both repos to find all touch points.

<!-- gitnexus:start -->
# GitNexus — Code Intelligence

This project is indexed by GitNexus as **bbclaw** (77623 symbols, 130014 relationships, 281 execution flows). Use the GitNexus MCP tools to understand code, assess impact, and navigate safely.

> If any GitNexus tool warns the index is stale, run `npx gitnexus analyze` in terminal first.

## Always Do

- **MUST run impact analysis before editing any symbol.** Before modifying a function, class, or method, run `gitnexus_impact({target: "symbolName", direction: "upstream"})` and report the blast radius (direct callers, affected processes, risk level) to the user.
- **MUST run `gitnexus_detect_changes()` before committing** to verify your changes only affect expected symbols and execution flows.
- **MUST warn the user** if impact analysis returns HIGH or CRITICAL risk before proceeding with edits.
- When exploring unfamiliar code, use `gitnexus_query({query: "concept"})` to find execution flows instead of grepping. It returns process-grouped results ranked by relevance.
- When you need full context on a specific symbol — callers, callees, which execution flows it participates in — use `gitnexus_context({name: "symbolName"})`.

## Never Do

- NEVER edit a function, class, or method without first running `gitnexus_impact` on it.
- NEVER ignore HIGH or CRITICAL risk warnings from impact analysis.
- NEVER rename symbols with find-and-replace — use `gitnexus_rename` which understands the call graph.
- NEVER commit changes without running `gitnexus_detect_changes()` to check affected scope.

## Resources

| Resource | Use for |
|----------|---------|
| `gitnexus://repo/bbclaw/context` | Codebase overview, check index freshness |
| `gitnexus://repo/bbclaw/clusters` | All functional areas |
| `gitnexus://repo/bbclaw/processes` | All execution flows |
| `gitnexus://repo/bbclaw/process/{name}` | Step-by-step execution trace |

## CLI

| Task | Read this skill file |
|------|---------------------|
| Understand architecture / "How does X work?" | `.claude/skills/gitnexus/gitnexus-exploring/SKILL.md` |
| Blast radius / "What breaks if I change X?" | `.claude/skills/gitnexus/gitnexus-impact-analysis/SKILL.md` |
| Trace bugs / "Why is X failing?" | `.claude/skills/gitnexus/gitnexus-debugging/SKILL.md` |
| Rename / extract / split / refactor | `.claude/skills/gitnexus/gitnexus-refactoring/SKILL.md` |
| Tools, resources, schema reference | `.claude/skills/gitnexus/gitnexus-guide/SKILL.md` |
| Index, status, clean, wiki CLI commands | `.claude/skills/gitnexus/gitnexus-cli/SKILL.md` |

<!-- gitnexus:end -->
