# BBClaw Adapter (Go)

`bbclaw-adapter` is the local agent-bridge daemon for BBClaw devices: it
takes the ESP32's PTT audio stream, runs ASR/TTS, and routes the transcript
to a chosen agent CLI (claude-code, opencode, openclaw, ollama, aider, …)
via a unified [Agent Bus](../design/agent_bus.md).

> **Open-source as of 2026-04-27.** The source moved here from
> `bbclaw-reference/adapter` (see [ADR-011](../design/decisions/ADR-011-adapter-open-source.md)).
> Module path is unchanged: `github.com/daboluocc/bbclaw/adapter`.
> Cloud backend and web portal stay closed.

## Build from source

```bash
cd adapter
make build           # binary at bin/bbclaw-adapter
make test            # full unit-test suite
make run             # reads ./.env, hot-reloadable via `make dev` (Air)
```

Or as a Go install for casual use:

```bash
go install github.com/daboluocc/bbclaw/adapter/cmd/bbclaw-adapter@latest
```

Pre-built binaries (recommended for non-developers): see
[the install script](../scripts/install-adapter.sh) or the
[GitHub Releases](https://github.com/daboluocc/bbclaw/releases) page.

## Adding an agent driver

Each driver wraps one CLI behind the `agent.Driver` interface
(`internal/agent/driver.go`). The full interface is six methods plus a
`Capabilities` declaration; see existing drivers as templates:

| Driver | File | Best as template if your CLI… |
|---|---|---|
| `opencode` | `internal/agent/opencode/driver.go` | …emits NDJSON, has `--session` resume |
| `claude-code` | `internal/agent/claudecode/driver.go` | …emits NDJSON with tool-use frames |
| `aider` | `internal/agent/aider/driver.go` | …emits plain text, resume via history file |
| `ollama` | `internal/agent/ollama/driver.go` | …is HTTP, not a subprocess |

Steps:
1. Create `internal/agent/<name>/driver.go` (≈400 lines, mirror a template above).
2. Add a `driver_test.go` covering: stdout/stream parser, session lifecycle, capabilities, `Approve→ErrUnsupported` if no approval.
3. Register in `cmd/bbclaw-adapter/main.go`'s `k_driver_registry` with an `autoEnable` predicate (PATH probe / cfg field / TCP probe) and an optional `forceEnv`.
4. `go test ./... && go build ./...`.
5. Open a PR — driver-only changes are reviewed quickly.

Pipeline:

`ESP32 -> HTTP stream upload -> ASR -> OpenClaw Gateway WS (connect -> node.event voice.transcript)`

Provider options:

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

## Single Adapter, Dual Transport

`bbclaw-adapter` 现在是单进程双能力：

- 本地 HTTP ingress：服务 `local_home` 固件
- Cloud home relay：服务 `cloud_saas` 固件

默认推荐：

- 不配置 `ADAPTER_MODE`（默认就是 `auto`）
- 始终启动本地 HTTP 服务
- 如果配置了 `CLOUD_WS_URL`，则自动同时连接 Cloud，作为 `home_adapter` relay

这意味着：

- 同一台家里的 Adapter，只跑一个进程
- `local_home` 设备直接打 `http://<adapter>:18080`
- `cloud_saas` 设备走 Cloud，但 Cloud 需要 Home Adapter 时仍由这个进程承接

调试开关仍保留：

- `ADAPTER_MODE=auto`：显式声明自动模式，效果与省略相同
- `ADAPTER_MODE=local`：只启本地 HTTP
- `ADAPTER_MODE=cloud`：只启 Cloud relay

健康检查：

- `GET /healthz` 返回聚合状态
- `status=ok` 表示本地能力正常
- `status=degraded` 表示本地仍可用，但 cloud relay 当前未连上
