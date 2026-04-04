# BBClaw — Agent 指引

本仓库根目录的 **AGENTS.md** 面向 AI 编码助手：提供可执行的构建步骤、目录约定与关键行为说明，作为 [README.md](README.md) 的补充（人类优先读 README；agent 优先读本文件）。格式参考开放约定 [AGENTS.md](https://agents.md/)。

## 项目概览

BBClaw 是 OpenClaw 生态中的硬件节点：ESP32-S3 固件，承载语音（ASR/VAD）、LVGL 显示、WiFi 与 Adapter/Cloud 配对。主要源码在 **`firmware/`**；架构与协议见 **`docs/`**。

## 仓库结构

| 路径 | 说明 |
|------|------|
| `firmware/` | ESP-IDF 固件（C/C++），Makefile 与 `idf.py` 均在此目录下执行 |
| `docs/` | 架构、公网模式、协议、用户手册 |
| `scripts/`、`tools/` | 烧录、调试与本地 ASR/TTS 等工具 |
| `references/` | 公网 Cloud、Home Adapter、Web Portal 等源码与配置；**构建命令与组件关系见 [references/AGENTS.md](references/AGENTS.md)** |

修改固件时，涉及路径均相对于 **`firmware/`**（例如 `firmware/src/bb_radio_app.c`）。若任务涉及 **Cloud / Adapter / Web**，请先读 **`references/AGENTS.md`** 再打开子目录 README。

## 前置条件

- 已安装 **ESP-IDF**（Makefile 默认 `IDF_PATH=$(HOME)/esp/esp-idf`，可用环境变量覆盖）。
- 首次克隆或切换分支后，在 **`firmware/`** 下执行 `make init` 可设置 `esp32s3` 并 `reconfigure`。

## 构建与常用命令

在仓库**根目录**可用 `make -C firmware <目标>`；在 **`firmware/`** 目录内则直接 `make <目标>`。以下以根目录写法为准。

| 命令 | 说明 |
|------|------|
| `make -C firmware init` | 设置 esp32s3 目标 + reconfigure |
| `make -C firmware build` | 编译固件 |
| `make -C firmware flash` | 编译并烧录 |
| `make -C firmware monitor` | 串口监控 |
| `make -C firmware monitor-log` | 串口监控并写入 `firmware/.cache/idf-monitor.latest.log` |
| `make -C firmware monitor-last` | 查看上述日志最近约 120 行 |
| `make -C firmware monitor-errors` | 从日志中过滤 E/W 级别行 |
| `make -C firmware menuconfig` | sdkconfig 菜单（transport / WiFi / LED / display 等） |
| `make -C firmware fullclean` | 清理 build；Python/IDF 环境不一致导致 configure 报错时可配合 `init` 使用 |
| `make -C firmware all` | build + flash + monitor-log |
| `make -C firmware gen` | 一键生成 LVGL 资源（字库 + 位图等） |
| `make -C firmware sim-build` | 构建 macOS LVGL 本地预览 |
| `make -C firmware sim-run` | 运行本地 LVGL 预览 |
| `make -C firmware sim-export-feedback` | 导出预览图到 `firmware/.cache/sim-feedback/` |

串口日志路径（相对于 `firmware/`）：`.cache/idf-monitor.latest.log`。

## 验证与测试

- **编译通过**：`make -C firmware build` 应无错误；改动显示/LVGL 资源时必要时跑 `make -C firmware gen` 后再 build。
- **真机**：烧录后通过 `monitor` / `monitor-log` 观察行为；无自动化单测套件时，以构建 + 设备日志为主。
- 提交 PR 前在说明中写清验证方式（参见 [.github/PULL_REQUEST_TEMPLATE.md](.github/PULL_REQUEST_TEMPLATE.md)）。

## 代码约定（固件）

- 语言为 **C**（ESP-IDF），与现有模块风格保持一致；避免无关重构与大面积格式化。
- Kconfig / `sdkconfig` 相关能力以 `menuconfig` 与现有宏（如 `BBCLAW_*`）为准。

## 安全与敏感信息

- 勿将密钥、证书、个人 `sdkconfig` 中的敏感项提交入库；遵循仓库已有 `.gitignore` 与团队惯例。

## 已实现行为：设备信息上报（Cloud）

**场景**：不同板卡与固件版本接入 Cloud 后，云端需获知能力（TTS、Display 等）以便降级或差异化处理。

**接口**：`POST /v1/devices/info`

**上报 JSON 示例**（`audioInput` 等为运行时宏，随配置变化，例如 `es8311` 或 `inmp441`）：

```json
{
  "deviceId": "BBClaw-0.1.0-A1B2C3",
  "firmwareVersion": "0.1.0",
  "capabilities": {
    "audioStreaming": true,
    "tts": true,
    "display": true,
    "vad": true
  },
  "hardware": {
    "audioInput": "es8311",
    "sampleRate": 16000,
    "codec": "opus",
    "screenWidth": 320,
    "screenHeight": 172
  }
}
```

**调用时机**：transport bootstrap 成功且配对通过（`ready && audio_streaming_ready`）后调用一次；失败不阻塞启动。

**相关文件**：

- `firmware/include/bb_cloud_client.h`、`firmware/src/bb_cloud_client.c` — `bb_cloud_report_device_info()`
- `firmware/include/bb_transport.h`、`firmware/src/bb_transport.c` — `bb_transport_report_device_info()`（cloud_saas 转发，local_home 为 no-op）
- `firmware/src/bb_radio_app.c` — 启动流程中调用

## TODO

- **HTTPS 恢复**：ESP-IDF `crt_bundle` 对 Let's Encrypt R12 链有启动初期验证失败问题（~8s 后自愈）。方案：将 healthz 改为非阻塞延迟检查，或 bootstrap 时先 pair 再补 healthz。当前临时走 HTTP 38082（nginx 代理到 bbclaw-cloud 38081）。

## 延伸阅读

- [README.md](README.md) — 功能、架构图、快速开始
- [docs/](docs/) — 用户手册、协议、公网架构
- [references/AGENTS.md](references/AGENTS.md) — Cloud、Adapter、Web 的依赖、构建与联调入口
