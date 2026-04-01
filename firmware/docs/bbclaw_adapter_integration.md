# BBClaw Adapter 固件接入说明

本文用于固件侧对接本地 `bbclaw-adapter`（上游硬件仍在开发阶段时可直接用于联调）。

## 1. 链路目标

当前接入链路：

`ESP32(PTT+音频分片) -> bbclaw-adapter(HTTP) -> ASR -> OpenClaw Gateway(node.event)`

固件只需要完成 HTTP 三段流式协议：

- `POST /v1/stream/start`
- `POST /v1/stream/chunk`
- `POST /v1/stream/finish`

## 2. 主机侧先决条件

1. 启动 OpenClaw gateway（默认 `18789`）。
2. 在仓库 `src/.env` 配好：
   - `OPENCLAW_WS_URL=ws://127.0.0.1:18789`
   - `OPENCLAW_AUTH_TOKEN=<gateway token>`
   - `OPENCLAW_NODE_ID=bbclaw-adapter`
   - `ASR_PROVIDER=doubao_native`（或你指定的 provider）
3. 启动 adapter：

```bash
cd /Volumes/1TB/github/bbclaw
make go-build
cd src
set -a; source .env; set +a
./bin/bbclaw-adapter
```

健康检查：

```bash
curl -H "Authorization: Bearer $ADAPTER_AUTH_TOKEN" \
  http://127.0.0.1:18080/healthz
```

## 3. 固件侧请求协议

所有请求均为 `application/json`，并带：

`Authorization: Bearer <ADAPTER_AUTH_TOKEN>`

### 3.1 start

`POST /v1/stream/start`

```json
{
  "deviceId": "esp32s3-001",
  "sessionKey": "agent:main:main",
  "streamId": "ptt-1730000000",
  "codec": "opus",
  "sampleRate": 16000,
  "channels": 1
}
```

返回（成功）：

```json
{"ok":true,"data":{"streamId":"ptt-1730000000"}}
```

### 3.2 chunk

`POST /v1/stream/chunk`

```json
{
  "deviceId": "esp32s3-001",
  "sessionKey": "agent:main:main",
  "streamId": "ptt-1730000000",
  "seq": 1,
  "timestampMs": 200,
  "audioBase64": "<base64-bytes>"
}
```

返回（成功）：

```json
{"ok":true,"data":{"seq":1}}
```

### 3.3 finish

`POST /v1/stream/finish`

```json
{
  "deviceId": "esp32s3-001",
  "sessionKey": "agent:main:main",
  "streamId": "ptt-1730000000"
}
```

返回（成功）：

```json
{
  "ok": true,
  "data": {
    "streamId": "ptt-1730000000",
    "text": "识别文本",
    "replyText": "助手最终回复",
    "savedInputPath": "/tmp/..../ptt-1730000000.pcm"
  }
}
```

### 3.4 finish（stream 模式）

`POST /v1/stream/finish`

```json
{
  "deviceId": "esp32s3-001",
  "sessionKey": "agent:main:main",
  "streamId": "ptt-1730000000",
  "replyMode": "stream"
}
```

返回头：

- `Content-Type: application/x-ndjson`

返回体按行输出 NDJSON，事件顺序：

1. `{"type":"status","phase":"transcribing"}`
2. `{"type":"asr.final","text":"识别文本"}`
3. `{"type":"status","phase":"processing"}`
4. 零到多个 `{"type":"reply.delta","text":"当前完整 assistant 文本"}`
5. `{"type":"done", ... }`

错误时：

- 若尚未开始流式输出，服务端仍可返回普通 JSON 错误；
- 若已开始流式输出，则返回 `{"type":"error","error":"..."}` 后关闭连接。

## 4. 分片与重试建议

1. `streamId` 每次 PTT 录音唯一。
2. `seq` 必须从 `1` 递增，不能跳号、重复。
3. 建议每片时长 `100ms~300ms`。
4. 网络失败时：
   - `start` 可重试（最多 3 次）；
   - `chunk` 若超时，按同一 `seq` 重发；
   - `finish` 若失败，不再补发 chunk，记录本次失败并结束会话。

## 5. 常见错误码

- `UNAUTHORIZED`: 适配器鉴权 token 不匹配。
- `STREAM_NOT_FOUND`: 未先 `start` 或 `streamId` 错误。
- `INVALID_SEQUENCE`: `seq` 非递增。
- `AUDIO_TOO_LARGE` / `AUDIO_TOO_LONG`: 超过服务端限制。
- `ASR_FAILED`: ASR 侧失败（密钥、格式、上游故障）。
- `OPENCLAW_DELIVERY_FAILED`: 到 OpenClaw 投递失败（网关未启动、token 缺失、未配对等）。

## 6. 现阶段联调命令

仓库根目录：

```bash
# 仅 API 冒烟
ADAPTER_AUTH_TOKEN=你的token ./scripts/bbclaw_adapter_smoke.sh

# 全本地闭环（mock upstream）
./scripts/bbclaw_adapter_e2e.sh
```

## 7. 与后续硬件开发的边界

1. 固件阶段只需要稳定输出分片上传协议，以及 `finish sync/stream` 两种回包处理，不需要感知 OpenClaw 内部 agent 细节。
2. 即使后续真实 Opus 编码替换，HTTP 协议保持不变（只影响 `audioBase64` 内容）。
3. 上游硬件就绪后，直接复用这份协议即可接入。
