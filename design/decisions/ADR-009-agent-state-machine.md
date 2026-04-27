# ADR-009: Agent 9 态状态机 LISTENING / BUSY / SPEAKING（Phase 4.8.x）

- **日期**: 2026-04-27
- **状态**: 已接受
- **关联**: ADR-005（多 driver）、ADR-008（chat 生命周期）；
  Phase 4.5 voice bridge；Phase 4.6 主题状态机原始 7 态

## 背景

Phase 4.6 主题状态机定义了 7 个语义态：

```
SLEEP / IDLE / BUSY / ATTENTION / CELEBRATE / DIZZY / HEART
```

主题（buddy ASCII / text only / 未来更多）只需要消费这些 enum，画对
应表情或调整 topbar。Phase 4.6 跑下来的核心问题是 **BUSY 严重过载**：

| 实际场景 | Phase 4.6 状态 |
|---|---|
| 用户按 PTT 正在说话（ASR 录音中） | BUSY |
| ASR 完成、agent 在计算回复 | BUSY |
| Agent 回复完、TTS 正在播放 | BUSY |
| Agent 中途的 EvText / EvSession 流式更新 | BUSY |

用户反馈两类典型 bug：

1. **"我按着 PTT 在说，buddy 显示 thinking 表情"**
   —— 期望是 LISTENING，实际是 BUSY。语音输入和 AI 计算都用一个表
   情，体验上"AI 没在听我说话，自顾自在想"，违和感强烈。

2. **"我已经收到回复文本了，但 buddy 还卡在 thinking，TTS 也没播"**
   —— EvTurnEnd 后逻辑判定为 IDLE，但 TTS task 的入场状态切换有竞
   态：EvTurnEnd → IDLE → TTS task 启动 → 应该切 SPEAKING 但被刚才
   的 IDLE 覆盖。Phase 4.6 没有 SPEAKING 概念，TTS 期间继续 BUSY，
   状态机和音频生命周期脱节。

Phase 4.8.x（commit `bf7f228`）补全两个语义态，把 BUSY 缩窄到"agent
计算中"原意。

## 决策

### 1. 扩 enum 到 9 态，新态附加在末尾

```c
typedef enum {
  BB_AGENT_STATE_SLEEP = 0,
  BB_AGENT_STATE_IDLE,
  BB_AGENT_STATE_BUSY,
  BB_AGENT_STATE_ATTENTION,
  BB_AGENT_STATE_CELEBRATE,
  BB_AGENT_STATE_DIZZY,
  BB_AGENT_STATE_HEART,
  BB_AGENT_STATE_LISTENING,   /* 新增 */
  BB_AGENT_STATE_SPEAKING,    /* 新增 */
  BB_AGENT_STATE_COUNT,
} bb_agent_state_t;
```

**为什么附加在末尾**：保持原有 7 个值不变，避免任何可能依赖 enum
数值的现存代码（包括 NVS 中可能持久化的 last-state、日志 grep、
未来的远程遥测）出错。代价是 enum 不再"语义分组"，但语义有名字
压住，数值排序无所谓。

### 2. 状态转移完整规则

| 触发源 | 旧态 | 新态 | 说明 |
|---|---|---|---|
| boot | * | SLEEP | 主题模块初始化默认 |
| voice_listening(begin) | * | LISTENING | PTT 按下、ASR 开始录 |
| voice_listening(end) | LISTENING | LISTENING* | 仅恢复 topbar，状态保持，等待 agent_task post BUSY |
| agent_task spawn | LISTENING | BUSY | 录音完成、上行 AgentBus，开始等回复 |
| EvText (流式) | BUSY | BUSY | 幂等，不重复 post |
| EvSession | BUSY | BUSY | 同上，仅更新会话 id |
| EvTurnEnd（有 TTS 待播） | BUSY | BUSY | 不变；让 tts_playback_task 接管 |
| EvTurnEnd（无 TTS） | BUSY | IDLE 或 HEART | 按 turn 时长 vs `BB_CHAT_HEART_THRESHOLD_MS` 决定 |
| tts_playback_task entry | BUSY/IDLE/HEART | SPEAKING | 即便上面已切 IDLE，也覆盖 |
| tts_playback_task exit | SPEAKING | IDLE 或 HEART | 同 EvTurnEnd 的 turn-duration 规则 |
| EvError | * | DIZZY | 错误兜底；之后由超时回 IDLE |

`*` 意为该态本身不变。

**关键点 1**：EvTurnEnd 不直接判 SPEAKING。EvTurnEnd 来自 AgentBus
事件流，发生在文本完成时；TTS 是否开播取决于（a）TTS toggle 是否
打开（b）reply_buf 是否非空。这两件事 EvTurnEnd 自己不知道，只有
TTS task 启动时才知道——所以 SPEAKING 的 ownership 在 TTS task。

**关键点 2**：tts_playback_task entry 时无条件 post SPEAKING，能
覆盖 EvTurnEnd 那条路径中可能错误 post 的 IDLE/HEART。这是 Phase 4.6
"卡在 thinking" bug 的修复点。

**关键点 3**：HEART 不是 SPEAKING 的对立，是 IDLE 的"庆祝色"变体。
HEART vs IDLE 仅取决于本轮总时长是否 < `BB_CHAT_HEART_THRESHOLD_MS`
（短 = HEART = 高效完成、长 = IDLE = 普通完成）。两处出口（EvTurnEnd
无 TTS 路径、tts exit 路径）共享这个判定。

### 3. 文件归属

- `firmware/include/bb_agent_theme.h` —— enum 定义、`set_state` API、
  `state_name()` debug helper、`k_glyphs[]` 表情索引扩到 9
- `firmware/src/bb_ui_agent_chat.c` —— 所有 `post_state(...)` 调用、
  EvText/EvSession/EvTurnEnd/EvError 路径
- `firmware/src/bb_theme_buddy_ascii.c` —— ASCII 主题对 9 态的映射
- `firmware/src/bb_theme_text_only.c` —— 文本主题对 9 态的映射
  （多数态共用同一个 topbar 标签，但 LISTENING/SPEAKING 单独标）

### 4. Driver-aware buddy face（附录）

ASCII 主题对 "中性态" 引入 driver 维度：IDLE / BUSY / HEART 这三个
"AI 不在主动表达" 的状态下，buddy 脸由当前 active driver 决定：

| Driver | 中性态脸 |
|---|---|
| `claude-code` | `(^_^)` 默认友好 |
| `openclaw` | `(O_O)` 大眼，"omni" 暗示 |
| `ollama` | `(v_v)` 放松/眯眼 |
| `opencode` | `(._.)` 小眼专注 |
| 未知 | `(^_^)` fallback |

LISTENING / SPEAKING / DIZZY / CELEBRATE / ATTENTION / SLEEP 这 6
个态保留 **跨 driver 不变** 的语义专用脸——动作态的语义清晰度优先于
driver 品牌识别。例如 LISTENING 的"耳朵竖起脸"在所有 driver 下都一
样，用户不会误以为是别的状态。

驱动这个分歧的判断标准是："这个态用户在意的是 AI 在干什么，还是
AI 是谁？"——动作态在意"干什么"（统一），中性态在意"是谁"（分
化）。

## 后果

### 正面

- BUSY 回归原意（"agent 计算中"），用户看到 BUSY 就知道是在等回复
- LISTENING 让"AI 在听"有视觉确认，PTT 体验闭环
- SPEAKING 让"AI 在说话"和"AI 在想"有区分，TTS 播放期间表情对得上
- Driver-aware 中性态让多 driver 在视觉上有"个性"，OLED 一瞥即知是
  谁在答
- enum 附加策略保证向后兼容，未来加新态也走相同模式

### 负面 / 风险

- 9 态比 7 态复杂，新主题作者需查阅本表
- TTS task 的 entry/exit 必须可靠 post 状态，否则会卡 BUSY/SPEAKING
  不出来；目前用 `tts_playback_task` 的 entry 第一行 + exit cleanup
  保证
- "无 TTS 时 EvTurnEnd 直接判 IDLE/HEART" 与 "有 TTS 时让 TTS task
  覆盖" 这条分叉是隐式的，要在代码注释里讲清

### 未来工作

- ATTENTION：留给"工具调用待批准"场景（Claude tool use）；目前未
  接入，状态机预留
- CELEBRATE：留给"达到某 token 阈值 / 完成某 milestone"；目前未触发
- 把 driver-aware 脸做成主题级配置（JSON / kconfig），而不是在
  `bb_theme_buddy_ascii.c` 里硬编码 switch
- 给状态切换加 ring buffer 日志，便于复盘 "为什么卡 BUSY"

## 触发何时撤销

- 如果实际跑下来 LISTENING 和 BUSY 用户分不出区别（脸不够区分），
  考虑把 LISTENING 合并回 BUSY 但 topbar 显式带文字。但优先调表情
  不动状态机
- 如果 SPEAKING 的 ownership 在 TTS task 反复出竞态，考虑把 TTS 启
  动也通过 AgentBus 事件触发，统一在事件路径上 post 状态
