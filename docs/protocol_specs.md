# BBClaw Runtime Protocol

本文档定义当前仓库内可运行的实际协议边界。

当前存在两种运行 profile：

- `local_home`：`Firmware -> local-adapter -> OpenClaw`
- `cloud_saas`：`Firmware -> Cloud -> home-adapter -> OpenClaw`

两种 profile 的固件 HTTP 面尽量保持一致，设备端不再为 Cloud 单独维护第二套 endpoint。

## 1. Firmware HTTP Surface

固件统一使用以下接口：

- `POST /v1/stream/start`
- `POST /v1/stream/chunk`
- `POST /v1/stream/finish`
- `POST /v1/tts/synthesize`
- `POST /v1/display/pull`
- `POST /v1/display/ack`

Cloud 额外提供：

- `POST /v1/display/task`

用于后台或运维入口向设备 display queue 入队。

## 2. Audio / Transcript Flow

### 2.1 Start

`POST /v1/stream/start`

```json
{
  "deviceId": "bbclaw-device-esp32s3",
  "sessionKey": "agent:main:main",
  "streamId": "stream-001",
  "codec": "pcm16",
  "sampleRate": 16000,
  "channels": 1
}
```

### 2.2 Chunk

`POST /v1/stream/chunk`

```json
{
  "deviceId": "bbclaw-device-esp32s3",
  "sessionKey": "agent:main:main",
  "streamId": "stream-001",
  "seq": 1,
  "timestampMs": 1711111111111,
  "audioBase64": "BASE64_AUDIO"
}
```

### 2.3 Finish

`POST /v1/stream/finish`

```json
{
  "deviceId": "bbclaw-device-esp32s3",
  "sessionKey": "agent:main:main",
  "streamId": "stream-001"
}
```

返回：

```json
{
  "ok": true,
  "data": {
    "streamId": "stream-001",
    "text": "帮我看下最新 build 状态",
    "replyText": "已经帮你看过了，main 分支当前是绿色。",
    "replyWaitTimedOut": false
  }
}
```

说明：

- `codec` 当前支持 `pcm16` / `pcm_s16le` / `opus`
- `streamId` 由固件侧生成并在整条流生命周期内保持稳定
- `local_home` 由 `local-adapter` 终止音频并做 ASR
- `cloud_saas` 由 Cloud 终止音频并通过豆包 API 做 ASR

## 3. TTS Flow

`POST /v1/tts/synthesize`

```json
{
  "text": "已经帮你看过了，main 分支当前是绿色。",
  "codec": "pcm16",
  "sampleRate": 16000,
  "channels": 1
}
```

返回：

```json
{
  "ok": true,
  "data": {
    "text": "已经帮你看过了，main 分支当前是绿色。",
    "audioBase64": "BASE64_AUDIO",
    "format": "pcm16",
    "sampleRate": 16000,
    "channels": 1
  }
}
```

说明：

- 固件主路径要求 `codec=pcm16`
- `local_home` 与 `cloud_saas` 都保持相同返回字段
- `cloud_saas` 由 Cloud 调用豆包 API 生成音频，并在需要时转成 PCM16

## 4. Display Queue

### 4.1 Enqueue

`POST /v1/display/task`

```json
{
  "deviceId": "bbclaw-device-esp32s3",
  "title": "Build Failed",
  "body": "CI red",
  "blocks": [
    { "type": "kv", "label": "repo", "value": "openclaw" }
  ],
  "actions": [
    { "id": "ack", "label": "Acknowledge" }
  ]
}
```

### 4.2 Pull

`POST /v1/display/pull`

```json
{
  "deviceId": "bbclaw-device-esp32s3"
}
```

返回为空队列：

```json
{
  "ok": true,
  "data": {
    "task": null
  }
}
```

返回单任务：

```json
{
  "ok": true,
  "data": {
    "task": {
      "taskId": "task-123",
      "deviceId": "bbclaw-device-esp32s3",
      "displayText": "Build Failed | CI red | repo: openclaw"
    }
  }
}
```

### 4.3 Ack

`POST /v1/display/ack`

```json
{
  "deviceId": "bbclaw-device-esp32s3",
  "taskId": "task-123",
  "actionId": "shown"
}
```

说明：

- v1 queue 为内存队列，不做持久化和跨实例共享
- `displayText` 由服务端渲染，优先保证小屏可直接展示
- `ack` 目前只做确认记录，不做复杂状态流转

## 5. OpenClaw Boundary

`local-adapter` / `home-adapter` 都不向 OpenClaw 发送原始音频。

对 OpenClaw 的边界统一是：

- official node `connect` / pairing
- `node.event` with `event: "voice.transcript"`
- optional reply wait / subscribe

典型 `voice.transcript` payload：

```json
{
  "text": "帮我看下最新 build 状态",
  "sessionKey": "agent:main:main",
  "streamId": "stream-001",
  "source": "bbclaw.cloud",
  "nodeId": "bbclaw-cloud"
}
```

## 6. Capability Discovery

`GET /healthz` 现在不仅返回存活状态，还返回 Cloud 能力快照：

- `asr.ready`
- `tts.ready`
- `display.enabled`

固件据此做能力降级：

- ASR 不可用：禁止进入录音链路
- TTS 不可用：仅显示文本，不阻塞问答
- display 不可用：停止轮询 display queue，不阻塞问答
