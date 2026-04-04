# BBClaw Firmware — AI Agent 特性记录

> 本文件供 AI agent 读取，记录固件侧已实现 / 进行中的特性，便于后续对话快速获取上下文。

## 常用 Makefile 命令

| 命令 | 说明 |
|------|------|
| `make init` | 设置 esp32s3 目标 + reconfigure |
| `make build` | 编译固件 |
| `make flash` | 编译 + 烧录 |
| `make monitor` | 串口监控 |
| `make monitor-log` | 串口监控并写日志到 `.cache/idf-monitor.latest.log` |
| `make monitor-last` | 查看最近 120 行串口日志 |
| `make monitor-errors` | 从日志中过滤 E/W 级别报错 |
| `make menuconfig` | 打开 sdkconfig 菜单（transport/wifi/led/display 等） |
| `make fullclean` | 清 build 目录（Python 环境不一致时用） |
| `make all` | build + flash + monitor-log |
| `make gen` | 一键生成所有 LVGL 资源（字库 + 位图） |
| `make sim-build` | 构建 macOS LVGL 本地预览 |
| `make sim-run` | 运行本地 LVGL 预览 |
| `make sim-export-feedback` | 导出预览图到 `.cache/sim-feedback/` |

串口日志文件：`.cache/idf-monitor.latest.log`

## 设备信息上报 ✅

**场景**：不同板子、不同固件版本连上 Cloud 后，Cloud 需要知道该设备支持哪些能力（如 TTS、Display），以便差异化降级处理。

**接口**：`POST /v1/devices/info`

**上报 JSON**：
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
    "audioInput": "inmp441",
    "sampleRate": 16000,
    "codec": "opus",
    "screenWidth": 320,
    "screenHeight": 172
  }
}
```

**调用时机**：transport bootstrap 成功且配对通过（`ready && audio_streaming_ready`）后调用一次，失败不阻塞启动。

**涉及文件**：
- `include/bb_cloud_client.h` / `src/bb_cloud_client.c` — `bb_cloud_report_device_info()`
- `include/bb_transport.h` / `src/bb_transport.c` — `bb_transport_report_device_info()`（cloud_saas 转发，local_home no-op）
- `src/bb_radio_app.c` — 启动流程中调用
