# Firmware ↔ Agent Bus 接入设计

> 状态：草案（Draft）
> 日期：2026-04-25
> 相关文档：[agent_bus.md](agent_bus.md)、[ADR-001](decisions/ADR-001-adapter-as-agent-bus.md)、[ADR-002](decisions/ADR-002-multi-turn-session-lifecycle.md)、[ADR-003](decisions/ADR-003-router-and-multi-driver.md)

## 1. 背景

adapter 侧 Phase 1–3 已落地：`POST /v1/agent/message` NDJSON、多轮 session、Router + 多 driver（claude-code / Ollama）。

**但设备端还没接**。bbclaw ESP32-S3 固件现在只走"老 PTT 通路"：

```
[PTT] → audio capture → POST /v1/stream/{start,chunk,finish}
                      ↓
          adapter 侧 ASR → openclaw sink → TTS → 设备播音频
```

agent bus 是**完全独立的第二条通路**，走纯文本：

```
[按键/菜单] → 文字 → POST /v1/agent/message
                   ↓
          adapter router → driver → NDJSON 流回
                                 ↓
                        设备屏幕流式显示 + 可选 TTS
```

**接上这条才算真正把 Phase 1–3 的工作交付给用户。**

## 2. 范围

### 做

- 固件新增 `bb_agent_client` 模块（只读文本、不涉及音频上传），对齐 adapter HTTP 协议
- LVGL 新增"Agent Chat"屏幕（文本对话视图）
- 长按某个键切到 Agent Chat 模式，带 driver 选择菜单
- session ID 存 NVS，设备重启后恢复会话
- 流式文本逐字渲染、`tool_call` toast、`tokens` 角标显示

### 不做（现阶段）

- **不替换**现有 `bb_adapter_client`（PTT/audio 通路保持原样，零风险）
- **不做 TTS**：Agent Chat 先纯文本阅读体验；语音回放是后续 ADR
- **不做 ASR → agent**：Agent Chat 先键盘/转盘/预置短语输入；"语音说话给 claude-code"属于后续融合
- **不做权限审批 UI**：和 Phase 2 一起做
- **不动 openclaw 通路**（见 ADR-001 的"暂不迁移"决策）

## 3. 现状盘点

### 固件已有基础设施（可复用）

- `bb_http_*` 封装（`http_post_json`、动态 accumulator、event handler）—— **注意：`bb_http_dyn_accum_t` 当前是 `bb_adapter_client.c` 文件内的 static 结构**，跨模块复用要么各自重定义一份（Phase 4.0 选这条，简单），要么以后抽到 `bb_http.h` 头里
- `bb_finish_stream_event_t` 结构里**已经定义**了 `THINKING` / `TOOL_CALL` / `REPLY_DELTA` 事件类型 —— Agent Bus 事件一一对齐，不需要新数据类型
- WiFi + NVS 基础完善
- LVGL 文本渲染组件（含中文字体）
- **cJSON**：ESP-IDF 自带 `json` 组件。`bb_adapter_client.c` 当前**没用**它（手写字符串解析），所以 `bb_agent_client.c` 是固件里第一个 cJSON 消费者；CMakeLists.txt 的 `REQUIRES` 需要加 `json`

### adapter 已有 endpoint

- `GET /v1/agent/drivers` → driver 列表 + capabilities
- `POST /v1/agent/message` → NDJSON 流（session / text / tool_call / tokens / turn_end / error）
- 所有这些都**无鉴权**（当 `ADAPTER_AUTH_TOKEN` 空时）

## 4. 模块设计

### 新增头文件 `firmware/include/bb_agent_client.h`

```c
#pragma once
#include "esp_err.h"

typedef enum {
    BB_AGENT_EVENT_SESSION = 0,   // 首帧：{sessionId, isNew, driver}
    BB_AGENT_EVENT_TEXT,          // assistant 文本片段
    BB_AGENT_EVENT_TOOL_CALL,     // display-only（Phase 2 再做审批）
    BB_AGENT_EVENT_TOKENS,        // in/out token 用量
    BB_AGENT_EVENT_TURN_END,
    BB_AGENT_EVENT_ERROR,
} bb_agent_event_type_t;

typedef struct {
    bb_agent_event_type_t type;
    const char* text;              // 文本类事件的内容
    const char* session_id;        // SESSION 事件
    const char* driver;            // SESSION / TOOL_CALL 的 driver 名
    int is_new;                    // SESSION 事件
    const char* tool;              // TOOL_CALL 的工具名（Bash/Edit/...）
    const char* hint;              // TOOL_CALL 的简短预览
    int tokens_in, tokens_out;     // TOKENS 事件
    const char* error_code;        // ERROR 事件
} bb_agent_stream_event_t;

typedef void (*bb_agent_stream_cb_t)(const bb_agent_stream_event_t*, void* user_ctx);

typedef struct {
    char name[24];
    int tool_approval;
    int resume;
    int streaming;
} bb_agent_driver_info_t;

/* 列 driver —— 启动 Agent Chat 之前或菜单里调 */
esp_err_t bb_agent_list_drivers(bb_agent_driver_info_t* out_list, int cap, int* out_count);

/* 发一次消息并流式接收事件；阻塞到 turn_end 或出错 */
esp_err_t bb_agent_send_message(
    const char* text,
    const char* session_id,        /* 可为空 */
    const char* driver_name,       /* 可为空（走 adapter 默认） */
    bb_agent_stream_cb_t on_event, /* NDJSON 每一行触发一次 */
    void* user_ctx);
```

### 新增源文件 `firmware/src/bb_agent_client.c`

- 复用 `bb_http_dyn_accum_t` 做流式读取
- `esp_http_client_perform` 配合 `HTTP_EVENT_ON_DATA` 回调把字节流做"按 `\n` 切行 + `json_parse`"
- 每行解析成 `bb_agent_stream_event_t` 丢给 user 的 cb
- 参考现有 `bb_adapter_stream_finish_stream` 的 NDJSON 处理，几乎可以抄

规模预估：~400 行（含 JSON 解析）。

### 新增 LVGL 屏幕 `firmware/src/ui/bb_ui_agent_chat.c`（不存在就新建目录）

屏幕本体只管"接事件、维护状态、调当前主题渲染"。视觉**全部委托给主题**（见下面"主题系统"小节）。

骨架：
- 上：transcript 列表（用户消息 + 助手消息 + tool_call toast；具体长相由主题决定）
- 中：driver 标签 + session 剩余 TTL 提示（可选）
- 下：输入区（第一版用预置短语列表 "你好 / 今天天气 / /status"，后续加软键盘）

### 主题系统（核心抽象）

**为什么做主题**：参考 [`claude-desktop-buddy`](https://github.com/anthropics/claude-desktop-buddy)（同款 ST7789 1.47" 屏），它的"七态角色 + ASCII/GIF"渲染范式天然对齐 Agent Bus 事件流。把屏幕本体和具体视觉解耦，未来既能保持极简文字流，也能挂上 buddy 风格甚至自定义角色。

#### 七态映射（沿用 buddy 命名）

| State | 触发 | bbclaw 来源 |
|---|---|---|
| `sleep` | adapter 不可达 / 无活跃 session | bb_agent_client 探测失败 |
| `idle` | 有 session 但当前无 turn | 上一次 `turn_end` 之后 |
| `busy` | 流式生成中 | `EvText` 帧到达期间 |
| `attention` | 等审批 | `EvToolCall`（Phase 2 真审批前为 display-only 提示）|
| `celebrate` | token 累计达阈值 | `EvTokens` 累加（每 50K out tokens）|
| `dizzy` | 错误 | `EvError` |
| `heart` | 快速 turn_end / 成功结束 | `EvTurnEnd` 且响应 < 5s |

#### 接口（C）

```c
typedef enum {
    BB_AGENT_STATE_SLEEP, BB_AGENT_STATE_IDLE, BB_AGENT_STATE_BUSY,
    BB_AGENT_STATE_ATTENTION, BB_AGENT_STATE_CELEBRATE,
    BB_AGENT_STATE_DIZZY, BB_AGENT_STATE_HEART,
} bb_agent_state_t;

typedef struct bb_agent_theme {
    const char* name;                          /* "text-only", "buddy-ascii", … */
    void (*on_enter)(lv_obj_t* parent);         /* 建初始 UI（一次） */
    void (*on_exit)(void);                      /* 清理（一次） */
    void (*set_state)(bb_agent_state_t state);  /* 七态切换 */
    void (*append_user)(const char* text);
    void (*append_assistant_chunk)(const char* delta);  /* 流式 append */
    void (*append_tool_call)(const char* tool, const char* hint);
    void (*append_error)(const char* msg);
    void (*set_driver)(const char* driver_name); /* 顶部状态栏更新 */
    void (*set_session)(const char* sid_short);  /* 显示 session 前 8 位 */
} bb_agent_theme_t;

void bb_agent_theme_register(const bb_agent_theme_t* theme);
const bb_agent_theme_t* bb_agent_theme_get_active(void);
esp_err_t bb_agent_theme_set_active(const char* name);   /* 持久化到 NVS */
const char** bb_agent_theme_list(int* out_count);        /* 菜单用 */
```

#### 主题注册策略

- 主题对象用**链表注册**（启动时各主题在自己的 init 函数里 `bb_agent_theme_register`）
- 内置 `text-only` 永远第一个，作为 fallback（NVS 里没值或主题不存在时）
- NVS key：`agent/theme`（string，不超过 24 字符）
- 切换时机：从设置菜单选择 → 写 NVS → 重新 `on_exit`/`on_enter`

### 菜单集成（`bb_radio_app.c` 改动）

现有主菜单加一项 **"Agent Chat"**。选中 → 切屏 → 启动屏幕专用任务处理事件回调并渲染。

## 5. 数据流（关键路径）

```
┌──────────────┐         ┌────────────────────────────┐
│  设备按键/   │         │  bb_ui_agent_chat (LVGL)   │
│  短语选择    │─input──▶│  - 累积用户输入            │
└──────────────┘         │  - 拿 session_id / driver  │
                         │  - 调 bb_agent_send_message│
                         └──────┬─────────────────────┘
                                │ 阻塞在 FreeRTOS task
                                ▼
                    ┌────────────────────────────┐
                    │  bb_agent_client           │
                    │  POST /v1/agent/message    │
                    │  HTTP_EVENT_ON_DATA 回调   │
                    │  ─ 按 \n 切行              │
                    │  ─ cJSON 解析              │
                    │  ─ 逐帧触发 on_event cb    │
                    └──────┬─────────────────────┘
                           │ 每帧
                           ▼
                    ┌────────────────────────────┐
                    │  UI 回调（在 LVGL 锁里）    │
                    │  - SESSION → 存 session_id │
                    │  - TEXT → append 到气泡    │
                    │  - TOOL_CALL → 新 toast    │
                    │  - TOKENS → 角标           │
                    │  - TURN_END → 光标隐藏     │
                    └────────────────────────────┘
```

**线程模型**：agent client 跑在**独立 FreeRTOS task** 里（别占 LVGL task），回调用 `lv_async_call` 把渲染切回 LVGL task。和现有 `bb_adapter_client` 的 pattern 一致。

## 6. 配置

### NVS key

| key | 含义 | 缺省 |
|---|---|---|
| `agent/last_sid` | 上次 session id，启动时恢复 | "" |
| `agent/driver` | 上次用户选的 driver | "" = adapter 默认 |

### 复用现有 WiFi + adapter URL 配置

adapter URL 已在现有 `bb_config.h` 里。无需新增。

## 7. 分阶段落地

| Phase | 内容 | 验收 |
|---|---|---|
| 4.0 | `bb_agent_client.c/h` + 最简 CLI 测试（串口命令触发 send）| ✅ 模块已落地（commit `0535218`）；串口测试待补 |
| 4.1 | 主题接口 + `text-only` 默认主题 + LVGL Agent Chat 骨架 | 设备屏幕能流式显示助手回复（极简风格）|
| 4.2 | driver picker 菜单 + NVS 持久化 session + 主题切换菜单 | 切 driver / 切主题 / 重启后继续 |
| 4.3 | tool_call toast + tokens 角标（在 text-only 主题里实现）| UI 完整 |
| 4.4 | 软键盘或拼音输入法（如果硬件支持） | 用户能真·自由输入 |
| 4.5 | 语音输入桥：长按 PTT → ASR → agent_bus（复用 `bb_adapter_client` 的 ASR，拿到文字后喂给 `bb_agent_client`）| 一键说话给 claude-code |
| 4.6 | `buddy-ascii` 主题：移植 [claude-desktop-buddy](https://github.com/anthropics/claude-desktop-buddy) 的七态 ASCII 角色，状态机绑到 Agent Bus 事件 | 选 buddy 主题 → 屏幕变 buddy 风 |
| 4.7 | 角色包推送：adapter 实现 buddy 同款 `char_begin/file/chunk/file_end` folder push 协议，bbclaw LittleFS 存 GIF 包 + LVGL GIF 解码 | 拖拽角色包到 web playground / 桌面工具，设备上自动切到自定义角色 |

**先做 4.1 + 4.2 + 4.3**（MVP，纯文字流就足够交付一次完整 UX）。
**4.4–4.5** 按 UX 反馈节奏。
**4.6** 是"可选 polish"——先做完文字流主线，再考虑 buddy 风格。
**4.7** 是大头，等 4.6 验证 ASCII 角色 OK 之后再决定要不要再投入实现 GIF。

## 8. 测试策略

| 层 | 怎么测 |
|---|---|
| `bb_agent_client` 单元 | Host build 下做 HTTP mock（已有 `bb_adapter_client_test.c` 样板可参考）|
| 端到端（设备 + adapter） | 设备串口命令 `agent send hello` 观察流式输出；和 playground HTML 行为应一致 |
| UI | 本地 SDL2 预览（`make sim-run`）手工测，截图留底 |

## 9. 开放问题

- **输入方式**：预置短语能跑起来，但用户迟早要自由打字。做软键盘 vs 接外设（蓝牙键盘）vs 一直绑 ASR → ADR-004？
- **会话长度上限**：Ollama 的 history cap 50 轮在 adapter 侧，设备这边要不要也给 transcript 列表封顶（比如保留最新 20 条）？内存压力问题
- **离线/弱网**：adapter 不可达时的 UX —— 默认 "agent unavailable" 界面，还是降级回 PTT 模式？
- **TTS 复用**：Agent Chat 要不要把 assistant 文本也过 TTS 播出来？如果要，就得和 `bb_adapter_tts_synthesize_pcm16` 拼起来。属于 4.5 之后的增量
- **OTA 影响**：`bb_agent_client.c` 增 ~400 行，会不会超固件分区？当前 factory 1MB / ota 2.5MB，应该有富余，但要留意
