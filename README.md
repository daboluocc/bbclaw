# BBClaw Adapter (Go)

`bbclaw-adapter` is a decoupled HTTP streaming adapter for BBClaw voice ingestion.

Pipeline:

`ESP32 -> HTTP stream upload -> ASR -> OpenClaw Gateway WS (connect -> node.event voice.transcript)`

Provider modes:

- `ASR_PROVIDER=local` (default): run `ASR_LOCAL_BIN` with optional `ASR_LOCAL_ARGS`; placeholder `{wav}` is the temp 16-bit PCM WAV. Transcript from stdout, else from `{wav}.txt` (override with `ASR_LOCAL_TEXT_PATH`).
- `ASR_PROVIDER=openai_compatible` (OpenAI-compatible HTTP `/v1/audio/transcriptions`)
- `ASR_PROVIDER=doubao_native` (ByteDance OpenSpeech WebSocket protocol)

Bilingual Chinese + English (e.g. FunASR SenseVoice with `-l auto`): see repo **`docs/local_asr_setup.md`** section **「启用双语支持（中英）」**.

TTS provider implementation is included for follow-up integration:

- `TTS_PROVIDER=doubao_native`

## Endpoints

- `GET /healthz`
- `POST /v1/stream/start`
- `POST /v1/stream/chunk`
- `POST /v1/stream/finish`
- `POST /v1/tts/synthesize`
- `POST /v1/display/task`
- `POST /v1/display/pull`
- `POST /v1/display/ack`

`/v1/stream/*` 支持的 `codec`：

- `pcm16` / `pcm_s16le`
- `opus`（真实 Opus 依赖 ffmpeg；同时兼容 BBClaw 固件联调封套）

## Environment Variables

See `.env.example` in this folder.

Required:

- `OPENCLAW_WS_URL` (or legacy `OPENCLAW_RPC_URL` for HTTP mock/e2e)
- ASR: for default `ASR_PROVIDER=local`, set `ASR_LOCAL_BIN`. For `openai_compatible`, set `ASR_BASE_URL`, `ASR_API_KEY`, `ASR_MODEL`. For `doubao_native`, set `ASR_WS_URL`, `ASR_APP_ID`, `ASR_API_KEY`, `ASR_RESOURCE_ID`.

Optional:

- `ADAPTER_ADDR` (default `:18080`)
- `ADAPTER_AUTH_TOKEN`
- `OPENCLAW_NODE_ID` (default `bbclaw-adapter`)
- `OPENCLAW_AUTH_TOKEN` (optional, used for gateway auth)
- `OPENCLAW_DEVICE_IDENTITY_PATH` (optional, default under OS config dir)
- `MAX_STREAM_SECONDS` (default `90`)
- `MAX_AUDIO_BYTES` (default `4194304`)
- `MAX_CONCURRENT_STREAMS` (default `16`)
- `HTTP_TIMEOUT_SECONDS` (default `30`)
- `SAVE_AUDIO` (default `false`)
- `SAVE_INPUT_ON_FINISH` (default `true`)
- `AUDIO_IN_DIR` (default `tmp/audio-in`)
- `AUDIO_OUT_DIR` (default `tmp/audio-out`)

## Local Commands

From repo root:

```bash
make go-init
make go-test
make go-build
make go-smoke
make go-smoke-opus
make go-e2e
```

Home Adapter to Cloud:

```bash
cd src
go build -o bin/bbclaw-home-adapter ./cmd/bbclaw-home-adapter
```

Dev run (Air hot reload):

```bash
cd src
air
```

Smoke script (manual):

```bash
ADAPTER_BASE_URL=http://127.0.0.1:18080 \
ADAPTER_AUTH_TOKEN=your-token \
./scripts/bbclaw_adapter_smoke.sh
```

## Closed Loop

### Real env (uses `src/.env`)

```bash
cd src && air
make go-smoke
```

### Fully local deterministic e2e

```bash
make go-e2e
```

`go-e2e` starts a local mock upstream, starts adapter with mock-safe env overrides, runs ASR and TTS API smoke tests, then cleans up processes automatically.

When `SAVE_INPUT_ON_FINISH=true` (default):

- ASR uploaded audio is saved to `AUDIO_IN_DIR`
- `/v1/stream/finish` response includes `savedInputPath` (absolute path)

When `SAVE_AUDIO=true`:

- TTS synthesized audio is saved to `AUDIO_OUT_DIR`
- API responses include `savedOutputPath`

## Display Task Bridge (Firmware Pull)

为了让固件尽快承接“回传任务并展示”，adapter 提供了一个过渡型的展示任务桥：

- 任务入队：`POST /v1/display/task`
- 设备拉取：`POST /v1/display/pull`
- 展示回执：`POST /v1/display/ack`

示例入队：

```json
{
  "deviceId": "bbclaw-device-esp32s3",
  "title": "Build Failed",
  "body": "main branch CI failed",
  "priority": "high",
  "blocks": [
    { "type": "kv", "label": "repo", "value": "openclaw" },
    { "type": "text", "text": "check test logs" }
  ],
  "actions": [{ "id": "ack", "label": "Acknowledge" }]
}
```

`displayText` 会由 adapter 从富格式字段自动渲染，固件可以直接展示该字段。

注意：这条链路用于当前 firmware/adaptor 联调；未来如果 OpenClaw 出现明确、通用、可上游的节点下行能力，再评估是否替换这条 adapter 展示桥。

## Home Adapter Cloud Mode

为首个“设备带出去也能用”的版本，仓库增加了一个独立的 `bbclaw-home-adapter` 进程。

它负责：

- 主动连接 BBClaw Cloud
- 以 `home_adapter` 身份保持出站 WebSocket
- 接收 Cloud 转发的 `voice.transcript`
- 调用本地 OpenClaw
- 将回复回传给 Cloud

当前设计里，公网版音频流与 ASR 终止在 Cloud；`bbclaw-home-adapter` 不再依赖本地 `bbclaw-adapter` 进程。

最小环境变量：

- `CLOUD_WS_URL=http://daboluo.cc:38081`
- `CLOUD_AUTH_TOKEN=...`
- `HOME_SITE_ID=your-home-site`
- `OPENCLAW_WS_URL=ws://127.0.0.1:18789`

运行：

```bash
cd src
set -a; source .env; set +a
./bin/bbclaw-home-adapter
```
