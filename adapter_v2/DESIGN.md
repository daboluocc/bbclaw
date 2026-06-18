# adapter_v2 — PTY-based universal CLI bridge

状态：**设计 / 骨架阶段（Phase 0）**。本目录与现有 `adapter/`（v1）完全并存，独立 `go.mod`，不影响 v1 的编译与发布。

## 1. 为什么有 v2

v1 的 `internal/agent/` 为每个 CLI（claude-code / codex / opencode / aider / ollama / openclaw）写了一个 driver，各自解析自家结构化输出（claude 走 `claude -p --output-format stream-json`），统一成 `agent.Event` 事件流喂给设备 UI / butler / TTS。

v2 推翻这条路线，原因（既定决策，不再讨论）：

1. **弃 `-p`** —— 不再依赖 headless / stream-json 接口。
2. **PTY 跑交互式 TUI** —— 把 CLI 当普通终端程序拉进伪终端，一根通道适配**所有** CLI，干掉 per-CLI driver 矩阵的冗余。
3. **adapter 做双适配**：
   - **通道①（终端视图）**：原样透传 stdout 字节流，给**手机 app / web** 用 xterm.js 直接渲染、接管真实终端。
   - **通道②（设备视图）**：服务端 VT 仿真器抽取出"干净的助手回复文本"，喂 **BBClaw 设备 / 语音**。

参考实现：`~/github/dinotty`（Rust，`src/pty.rs` / `src/vt_screen.rs` / `src/session.rs` / `src/ws.rs`）。v2 借鉴它的 **"Session 独立于连接而存活 + 重连补发屏幕快照"** 机制。

## 2. 已接受的取舍（重要）

走 PTY 抓 TUI 字节流，**拿不回 stream-json 的语义粒度**。能稳定得到的只有：

- ✅ 最新一段**助手回复纯文本**（给 TTS / 设备屏）
- ✅ 粗粒度**忙/闲状态**
- ❌ `thinking` 折叠块、精确 `tool_call` 审批事件、`dispatch_status` 子任务派发进度、`tokens` 统计 —— 这些 TUI 不以机器可读形式提供。

**决策（已确认）**：设备侧只要纯文本 / 语音，**接受**富 UI 降级（v1 的"对话页 thinking + 子任务下钻"在 v2 下不保留）。

## 3. 架构：单 PTY 会话，双视图消费

一个 PTY 会话 = 一个活着的交互式 CLI 进程。手机/web 看原始字节，设备看抽取文本，**两者盯同一个进程**——手机可实时旁观语音设备正在驱动的会话。

```
                 ┌────────────────────────────────────────────────┐
   PTT 语音 ─ASR─▶│  session.Session                               │
                 │   ├─ ptyhost.PTY ── 交互式 `claude`(无 -p)─┐   │
   手机/web 按键 ─▶│   │  (stdin)                              │   │
                 │   │                                        ▼   │
                 │   │                            ┌──────────────┐│
                 │   │  stdout 字节流 ───────────▶│ vtscreen     ││
                 │   │       │                    │ 快照/scrollbk││
                 │   │       │                    └──────┬───────┘│
                 │   │       ▼ (原样)                    ▼ (抽取) │
                 │   │  ① termchan                  ② extract     │
                 │   │  WS raw bytes            VT→纯文本+边界检测 │
                 └───┼───────┼───────────────────────────┼────────┘
                     │       ▼                            ▼
                  stdin  手机app/web (xterm.js)    deviceapi → TTS→设备
```

**输入两路、写同一个 stdin**：
- 手机/web：原样按键字节（含方向键、Ctrl-C）
- 设备语音：ASR 文本 → 注入 `transcript + "\r"` 当一个 user turn

## 4. 包结构

```
adapter_v2/
  cmd/bbclaw-adapter-v2/main.go   # 进程入口、HTTP/WS 路由装配
  internal/
    ptyhost/    # PTY 拉起 + 生命周期(对标 dinotty pty.rs);CLI 无关
    vtscreen/   # 服务端 VT 仿真 + snapshot/scrollback(对标 dinotty vt_screen.rs)
    session/    # Session 独立于连接而存活;SessionManager;重连补快照;detach GC
    termchan/   # 通道①: WS 原样字节 ↔ PTY,手机/web 接管 + 重连恢复
    extract/    # 通道②: VT screen → 干净助手文本(v2 最难、最脆的一层)
      boundary.go #   turn 边界检测(回到输入提示符 = 一轮结束)
    deviceapi/  # 抽取文本 ↔ 设备协议; ASR 入、TTS 出
```

**复用 vs 重写**：
- 复用 v1（Phase 2 再接入）：`asr` `tts` `audio` `config` `settingsstore` `obs`
- 被取代：整个 `internal/agent/`（driver 矩阵 + Event 模型）→ `ptyhost` + `vtscreen` + `extract`

## 5. 两个决定成败的工程难点

1. **turn 边界检测**（`extract/boundary.go`）：stream-json 有 `turn_end`，PTY 没有。靠"输出停顿 + 光标回到 CLI 输入提示符"判定一轮结束。这是语音线"何时开始 TTS 播报"的命门，也最易出 bug。
2. **tool 审批**：交互式 CLI 的权限弹窗渲染在 TUI 里，设备的批准/拒绝需从屏幕 scrape 弹窗、注入 `1`/`2` 按键。能做，但启发式。

## 6. 落地分期

- **Phase 1**：`ptyhost` + `vtscreen` + `session` + `termchan` → 手机/web 接管真实终端 + 断线重连恢复（最稳，纯 Dinotty 思路，先拿价值）。
- **Phase 2**：`extract` + `deviceapi` + 复用 v1 asr/tts → 接语音/设备线，啃 turn 边界与文本提炼。
- **Phase 3**：tool 审批 scrape、多 tab/分屏 sync（可选）。

## 7. 与 v1 的关系 / 迁移

- v2 独立 `go.mod`、独立二进制 `bbclaw-adapter-v2`，开发期与 v1 并行运行（不同端口）。
- 设备固件先不动；Phase 2 打通后再决定设备端切到 v2 的灰度策略。
- 公共代码（asr/tts/audio）暂以"先复制、跑通后抽共享库"处理，避免开发期 v1/v2 编译耦合。

## 8. 设备接入（deviceapi）+ 端到端冒烟

`internal/deviceapi` 是唯一懂设备契约的包：ASR 文本进、TTS 音频出，中间一根 PTY
会话。一台设备绑一个 `Bridge`：

```
PTT 音频 ─Recognizer.Transcribe─▶ Bridge.SubmitVoicePTT ─┐
设备/测试直接注入文本 ───────────▶ Bridge.SubmitVoiceTurn ─┤
                                                          ▼  session.Write → PTY stdin
                            PTY stdout 字节 ─▶ Bridge.Run（作为一个原始字节 Client 挂到 session）
                                                ├─ 自己的 vtscreen 镜像 Feed
                                                ├─ extract.Extractor 抽干净回复文本
                                                └─ extract.Detector 判 turn 边界
                                                          ▼ （一轮结束）
                                       Synthesizer.Synthesize ─▶ DeviceSink.Play（设备喇叭）
```

**三个接口，把 v1 的真实 provider 留到后面再接：**

- `Recognizer`（ASR 入）—— 比 v1 `asr.Provider` 窄，设备线只要最终文本。
- `Synthesizer`（TTS 出）—— 形状对齐 v1 `tts.Provider`（`Synthesize` + `OutputFormat`），
  v1 provider 可直接塞进来。
- `DeviceSink`（设备传输）—— Phase 2 先 stub；真实实现走设备音频通道。

本地自带、可离线跑通的桩：`StaticRecognizer`（mock ASR）、`SilentSynthesizer`
/ 真机 macOS `SayTTS`（真 WAV）、`DiscardSink` / 测试用 `recordingSink`。把 v1
真实 provider 接到这三个接口背后，是文档化的下一步（见 §7）。

**in-flight 策略：打断（不是排队）。** `SubmitVoiceTurn` 注入文本前先发一个 ESC
（claude 等 TUI 把 ESC 绑成 "esc to interrupt"），中断上一轮再提交新一轮——语音
设备上"插话 = 停下听我这句"是自然的轮替。空白 transcript 一律 no-op，ASR 误触不会
打断在跑的一轮。回显策略：设备只播助手回复，不回播用户自己的话。

### 端到端冒烟（无外部依赖）

`make -C adapter_v2 e2e` 跑一轮完整语音问答，**不连网、不需要装真 agent CLI**：

1. `cmd/mockcli` 是一个**真正独立编译的上游进程**——模仿 agent TUI 的屏幕行为
   （空闲 `> ` 提示符 → 收到一行输入后画带 "esc to interrupt" 的 spinner →
   画 `ANSWER: <输入>` 回复 → 清掉 spinner、回到空闲提示符并安静下来）。它不是内联
   shell 片段，所以冒烟跑的是和生产一样的 spawn→inject→reply→boundary→TTS 全链路。
2. `internal/deviceapi/e2e_test.go`（`//go:build e2e`）把 `mockcli` 在真 PTY 下拉起，
   注入一段 ASR 文本，等边界检测触发，断言：TTS 被调用且文本含 `ANSWER: <transcript>`，
   合成音频按 `OutputFormat` 到达 `DeviceSink`。`TestE2ESayTTSRoundTrip` 进一步用真
   macOS `say` 验证产出真实 RIFF/WAVE（非 macOS 自动 skip）。

冒烟用 `e2e` build tag 隔离，默认 `go test ./...` 不会在测试期 `go build` 子进程、
保持快且零外部依赖；重的进程冒烟只在 `make e2e` 按需跑。

> 注意：冒烟用 ASCII transcript。服务端 VT 仿真器（`vtscreen` 底层 hinshun/vt10x）
> 目前不渲染双宽 CJK 字形到 `VisibleText`（"你好" 回复会抽成空）——宽字符渲染是
> `vtscreen` 的事，单独追踪，与本设备 e2e 解耦；round-trip 链路本身与文本无关，已被
> ASCII 用例完整覆盖。

### Makefile 入口

```bash
make -C adapter_v2 build   # 编 bin/bbclaw-adapter-v2
make -C adapter_v2 test    # go test ./...（快，无外部依赖）
make -C adapter_v2 e2e     # 设备端到端语音 round-trip 冒烟（本节）
```

Go 不在 PATH 时 `GO` 默认取 Homebrew 路径，可覆盖：`make -C adapter_v2 test GO=/path/to/go`。

## 9. Phase 3（记录，未实现）— tool 审批 scrape（issue #213）

> 状态：**仅记录**。Phase 1/2 已完成；按决策 Phase 3 暂不实现，先固化设计，待设备/语音线在真机上验证稳定后再开。

**目标**：交互式 CLI 的工具权限弹窗渲染在 TUI 里（claude 的 "Do you want to proceed? 1. Yes / 2. No…"）。让瘦设备/手机也能审批——从 VT 屏幕识别弹窗、把"批准/拒绝"翻成注入 `1`/`2` 等按键。

**已就位的地基**（Phase 1/2 产物，直接可复用）：
- `vtscreen.VisibleText()` 已能给出可见网格纯文本 → 弹窗识别的输入。
- `extract/noise.go` 已有 claude 的行分类器（`isPromptLine` / spinner 判定）→ 弹窗行识别可挂同一套，claude 改 UI 时一处修。
- `session.Write` / `deviceapi.Bridge.SubmitVoiceTurn` 已是注入 stdin 的通道 → 注入 `1`/`2` 复用之。
- `extract/boundary.go` 的三启发式里，**"等待审批态"不能被误判为 turn 结束**——已天然契合（spinner 在、idle 提示符未回时 `TurnEnded=false`）。

**待写**：
1. `extract/` 加一个权限弹窗识别器：扫 `VisibleText`，命中弹窗区域 → 抽出结构 `{tool, options:[{key,label}]}`。
2. `deviceapi/` 暴露 `Approve(once|deny)` → 注入对应按键序列。
3. 与 boundary 的交互细化：审批等待态显式建模（避免把"等用户按键"当成静默结束）。

**风险**：最脆的一层，claude 改审批 UI 即可能失效，必须配 fixture 回归护栏（同 #209 的 `testdata/*.vt` 思路）。验收见 issue #213。
