# 密语解锁（Cloud SaaS）

## 行为

- 使用 **公网 Cloud（`cloud_saas`）** 时，设备上电后进入 **锁定** 态，需说出与账号/云端配置一致的 **密语** 后解锁（云端 ASR 转写后比对文字，非生物声纹）。
- 按住 PTT 采集 PCM，经 WebSocket `kind: voice.verify` 上送；收到 `voice.verify.result` 且 `match=true` 后进入 `UNLOCKED`。
- 采集时长上限见 `BBCLAW_VOICE_VERIFY_MAX_MS`（`bb_config.h`）。

## 云端

- 实现见 `bbclaw-reference`：`/v1/voiceprint/verify` 仍为 HTTP 路径名（兼容）；逻辑为密语比对。
