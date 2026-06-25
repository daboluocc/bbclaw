# ADR-038: 密语解锁失败时显示识别到的语音文本

- **日期**: 2026-06-25
- **状态**: 已接受（已实现，`make build` 通过；待真机验证 + owner 发布）。**纯固件改动——云端已返回 transcript，无需改云/adapter。**
- **关联**: ADR-037（固件密语开关）——同一密语(锁屏语音解锁)特性的 UX 改进。`voiceprint_api.go`（云端 voice.verify 已返回 transcript）。

## 背景

密语(miyu)= 锁屏语音口令解锁：用户在 LOCKED 页按住说口令 → 固件录音 → `voice.verify` → 云端 ASR 识别+比对 → 回 `voice.verify.result`。问题（owner 反馈）：**ASR 准确度不稳，解锁失败时用户不知道设备到底听成了什么**，没法调整发音/口令。

**关键勘查结论**：云端 `voice.verify.result` WS 回包**早已带 `transcript` 字段**（`cloud/internal/httpapi/voiceprint_api.go:79` `"transcript": result.Transcript`），识别文本**已经到固件**——但固件的解析器 `parse_voice_verify_result`（`bb_adapter_client.c`）只取了 `match`/`confidence`/`message`，**丢弃了 transcript**，锁屏也没显示。所以这是**纯固件改动**，云端/adapter 不用动（满足 CLAUDE.md「Cross-Component Protocol Sync」：消费已有字段，无新契约）。

## 决策

固件消费已有的 `transcript` 并在锁屏失败态显示「听到的内容」：

1. **`bb_voice_verify_result_t`** 加 `char transcript[128]`（`bb_adapter_client.h`）。
2. **`parse_voice_verify_result`** 解析 `transcript`（`bb_adapter_client.c`，照 `message` 的 `json_extract_string`）。
3. **`bb_page_locked_show_heard(const char* heard)`**（`bb_page_locked.c/.h` 新增）：把锁屏 hint 行改为 `听到「<heard>」请重说`；heard 为空则保持默认错误提示。
4. **reject 分支**（`bb_radio_app.c`）：`show_status_error(VERIFY_ERR)` 后调 `bb_page_locked_show_heard(verify_result.transcript)`，并把失败停留从 1200ms 调到 **2500ms**（口令短语要给用户时间读「听到了什么」）；同时把识别文本带进 `bb_display_show_chat_turn` 的详情。

成功(match)→ 立即解锁进 CHAT，锁屏一闪而过，不显示 transcript（无意义）；只在**失败态**显示，正是用户要调整的场景。

## 不做

- 不改云端/adapter（transcript 已在回包里）。
- 不显示 confidence 数值（对普通用户无意义；用「听到的文本」更直观）。

## 后果

- 密语解锁失败时，用户在锁屏看到「听到『xxx』」，立刻知道 ASR 把口令听成了什么 → 可调整发音或改口令。
- 纯固件、向后兼容：transcript 缺失（旧云端/异常）时降级为原错误提示，不回归。
- 经现有云端 OTA 渠道灰度发布；本仓不打 tag，由 owner 触发（同 ADR-037）。
