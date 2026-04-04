# BBClaw Firmware — AI Agent 特性记录

> 本文件供 AI agent 读取，记录固件侧已实现 / 进行中的特性，便于后续对话快速获取上下文。

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
