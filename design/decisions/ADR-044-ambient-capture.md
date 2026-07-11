# ADR-044: 随身长录音（Ambient Capture）——设备第二形态

- 状态：**Accepted（2026-07-05 用户拍板：Track A，手表 + SD 本地优先 + 回网补传）**
- 日期：2026-07-05
- 组件：firmware + cloud（不经 adapter/home relay）
- 前置调研：三路并行（固件采集管线 / 云端基建 / 同类产品+法规），事实引用见各节

## 1. 目标与定位

设备一键切入**录音形态**：持续采集环境音，实时流式传云端保存，云端异步做
ASR/文本解读，为 AI 感知佩戴者日常提供真实语料。录音形态与对话形态**互斥**
（长录音状态下不做唤起/对话）。

产品定位锚定「**记录佩戴者自己的生活与对话**」——这是法规红线的定性基础
（见 §8），不是措辞技巧。

市场验证（Limitless/Bee/Omi/Plaud 调研结论）：
- 付费点在「云端转写+摘要+可问答」，不在本地全量记录（Rewind→Limitless 转型教训）；
- 「先存云 + 异步批转写」比流式转写便宜 40-50%，正是本设计的形态；
- 续航是第一翻车风险（friend.com 标称 15h 实测 4h 口碑崩盘）——我们标保守值。

## 2. 总体架构

```
[手表] ES7210双mic → capture_task(现有) → ring buffer(现有)
        → recorder_task(新, PSRAM栈): 能量VAD门控 → Opus编码(16kbps CBR+DTX)
        → WS 二进制帧, 60s 一段 (ambient.segment.start/finish)
        → 断网: PSRAM 环形缓冲 ≈40min, 丢最旧
[云端] WS handleDeviceWS 新增 ambient.* case
        → 边收边 append 到 data/ambient/<deviceId>/<date>/<segId>.opus (不解码,不进内存)
        → finish 登记 job (append-only jsonl)
        → ambient worker(voice-clone-worker 模式): Silero VAD 精切 → 火山录音文件
          异步 ASR(批价) → transcript.jsonl → 小时级摘要 → daily digest(LLM)
```

### 为什么复用设备 WS 通道而非独立 HTTPS 上传
- 设备对 HTTP 端点无可用凭证（`withHTTPAuth` 是共享管理 token）；WS 已有
  binding 鉴权 + 配对流程（cloud `server.go:1629-1633`）。
- `/ws` 的 nginx 已为长连接调好（86400s read timeout）；仓库 nginx conf 与线上
  已知不同步，少动网关少踩坑。
- WS 天然有序，断点续传退化为「掉线最多丢当前一段（≤60s）」。

### 为什么分段（60s/段）而非一条长流
绕开云端现有三个「整段进内存」硬约束：4MB 内存上限、90s 时长上限、整段
ffmpeg 解码（`audio.Manager`、`server.go:2059-2071`、`codec.go:36-68`）。
**现有对话 turn 流一律不改**，ambient 是并行的新路径。段小可重传，服务端
直接落盘（Ogg/Opus 原生可 append），云端零解码。

## 3. 固件设计

### 3.1 状态机：RECORDER 第四态
`bb_radio_app_state_t` 增 `BBCLAW_STATE_RECORDER = 3`（现有 LOCKED/CHAT/SETTINGS，
`bb_radio_app.h:19-22`）。

- **入口**：待机主菜单加「录音」行（与 ADR-021 §9 菜单结构一致）+ OK 二次确认
  （隐私敏感操作）。
- **态内键位/触摸语义**：
  - PTT 短按 = **打标记**（bookmark 帧，给云端解读一个高价值锚点），不再唤起对话；
  - PTT 长按（≥2s）或右滑/BACK = 弹「停止录音?」确认后退出；
  - 其余触摸手势禁用。
- **态内豁免**：双级空闲锁屏（120s+120s，`bb_radio_app.c:3711-3733`）与 miyu 锁
  不生效；OTA confirm 推迟到退出录音。
- **互斥**：进入前检查 `!streaming && !arming && !session_busy`；云端同时校验
  「ambient 流活跃时拒绝 voice.stream.start」（扩展现有单流校验 `server.go:1915`）。

### 3.2 采集管线：旁路 PTT 状态机，不复用
`stream_task` 的 arming/VAD/finish 语义（等 ASR 往返、单回合）与连续录音不匹配，
且它兼任按键派发循环（阻塞发送会卡键，有前科——见 memory
[[stream-task-no-blocking-network]]）。方案：

- `capture_task` + ring buffer **原样复用**（已解耦，`bb_radio_app.c:380-404, 4102`）；
- 新增独立 `recorder_task`（PSRAM 栈 xTaskCreateWithCaps）：消费 ring → VAD 门控
  → Opus 编码 → WS 上行。RECORDER 态时 stream_task 不消费 ring（I2S RX 单消费者，
  互斥天然成立）；
- 编码器录音会话级长持有；`bb_ogg_opus` 25KB 静态内部缓冲是单实例
  （`bb_ogg_opus.cpp:491`），两形态互斥保证不并发建第二个。

### 3.3 编码与省流（成本第一杠杆）
- 显式 `OPUS_SET_BITRATE(16000)` CBR + `OPUS_SET_DTX(1)`（现在未设码率≈19-25kbps
  auto，DTX 关着，`bb_ogg_opus.cpp:601-602`）→ 7.2MB/h 封顶；
- **设备端能量 VAD 门控**：静音段不编码不上传（Limitless 同款设计）。调研数据：
  ambient 语料关键语音仅 ~14% 时长，VAD 预过滤可把下游 ASR 量砍到 1/4~1/7——
  同时省功耗、带宽、云端 ASR 费。静音间隙以时间戳标记（gap 帧），云端时间轴不断裂；
- 批量发送：攒 1-5s 再发，降低 WiFi 唤醒频次。

### 3.4 存储与同步（2026-07-05 修订：本地优先，回网补传）
**手表板载 Micro SD 卡槽（SDMMC 1-bit：CLK=2/CMD=1/D0=3，与现有外设零冲突）**，
评审时确认启用——架构从「实时流+PSRAM 兜底」升级为 **SD 本地优先**
（Limitless/Plaud 验证过的黄金架构，纯实时流断网即丢是已验证的坑）：

- 录音段**先写 SD**（`/sdcard/ambient/<sessionId>/<seq>.opus` + `index.jsonl`
  段索引），32GB 卡 ≈ 4000+ 小时，出门断网照录；
- **同步引擎**：WiFi+WS 可用时后台补传 pending 段（云端 ack → 本地标记
  uploaded；SD 空间低水位时从最旧 uploaded 段开始清理）；
- 实时性是 best-effort：在网时段落盘后即刻上传，离网时回网补传；
- PSRAM 不再承担缓冲职责（只作写 SD 的短暂中转），无 40 分钟丢失上限；
- 未来 Track B（专用随身硬件 + Cat-1 4G 物联网卡，合宙 Air780E ¥26 级模组 +
  ¥9.9/3GB 月卡）复用同一套「SD 缓冲 + 批量上传」引擎与云端契约，只换传输层。

### 3.5 功耗（第一风险）
- 粗估（无实测，需上电流表定标）：熄屏连续录音 80-120mA → 400mAh 电池
  **≈3.5-5 小时**；
- 必做优化：录音态强制熄屏（常显最小化指示后灭屏）、降频 160MHz（complexity=0
  编码余量足）、DTX、批量发送；
- 护栏：低电量（<15%?）自动停录并落最后一段 + 通知。
- 对外**标保守续航**（friend.com 教训）。

### 3.6 录音指示（合规刚性，见 §8）
屏幕常显红点 + 已录时长 + 缓冲/断网状态（极简暗屏形态，AMOLED 黑底只亮
几十个像素）；任意键先亮屏。**指示不可关闭**——这是「非窃听器材」的定性依据，
不是 UI 偏好。

### 3.7 libopus 栈账（P1a 实战教训，2026-07-05）
本仓库 libopus（78__esp-opus）以 `USE_ALLOCA` 编译：**一次完整 SILK 编码在
调用任务栈上 alloca ~24KB**（silk_encode_frame_FIX 一层 9.5KB+NSQ 动态）。
任何调用 opus_encode 的任务栈必须 ≥40KB（stream_task/recorder_task 均如此）。
PSRAM 栈溢出不触发 fault（写烂邻居堆数据、堆元数据无损），死点飘忽极难排查
——已加 FREERTOS_WATCHPOINT_END_OF_STACK 硬件观察点。结构性根治（libopus 改
NONTHREADSAFE_PSEUDOSTACK+PSRAM 伪栈，可为每个任务省 ~20KB）列 P3。

### 3.8 内部 DRAM 护栏
internal largest 实测仅 ~11KB，WS 断线重建（8KB 内部栈）碎片失败有前科
（`docs/debug/internal-ram-ws-task.md`）。进录音模式前检查
`internal_largest < 12KB 则拒绝进入`；录音会话开始时确保 WS 已连（预热）。

## 4. 协议（跨组件契约，新 kind）

```
设备→云（文本 envelope + 二进制帧,沿用现有 WS 编码习惯）:
  ambient.session.start   {sessionId, codec:"ogg_opus", bitrate}
  ambient.segment.start   {sessionId, segSeq, startedAtMs, gapMsBefore}
  <binary frames>          (Ogg/Opus, append 语义)
  ambient.segment.finish  {sessionId, segSeq, durationMs, bytes}
  ambient.bookmark        {sessionId, atMs}          # PTT 短按打标
  ambient.session.stop    {sessionId, reason}        # 用户停/低电/错误
云→设备:
  ambient.segment.ack     {segSeq}                   # 续传依据
  ambient.error           {code}                     # 拒绝/配额/存储满
```
不新增 adapter/relay 触点（录音不做对话，不经 home adapter）——协议同步表
只涉及 cloud 一侧。

## 5. 云端设计

### 5.1 接收与存储
- `handleDeviceWS` 加 ambient case：收帧即 append `data/ambient/<deviceId>/<date>/<segId>.opus`
  （0600 权限，参考 voice-clone 样本落盘先例 `portal_voices_clone.go:77-101`）；
- 元数据 **append-only jsonl**（`segments.jsonl`），不进 accounts.json/events.json
  （后者每条全量重写，会被每分钟一段打爆，`event/log.go:60`）；
- 存储账：16kbps → 7.2MB/h，全天佩戴 <180MB/天/设备；10 台常开 ≈50-80GB/月——
  V1 单 VPS 本地盘 + **生命周期策略**扛住，设备量上来再谈对象存储。

### 5.2 异步处理管线
复制 `runVoiceCloneWorker` 模式（落盘 job 状态 + 常驻轮询 goroutine，重启可恢复，
`portal_voices_clone.go:219-246`）：

```
pending → Silero VAD 精切(丢静音) → 火山「录音文件识别」异步 API(批价,比流式便宜)
        → transcript.jsonl(词级时间戳) → 小时级摘要 → daily digest(LLM)
```
- **不用现有对话链路的流式 ASR**（贵 40-50% 且整段进内存）；
- 成本量级：全天佩戴经 VAD 后 ~3h 语音/天/设备，批转写 API 路线 ≈¥3-7/天，
  设备端 VAD 门控再砍——不做 VAD 的 24/7 转写经济上不成立（调研结论）；
- 摘要层 LLM 成本相对 ASR 可忽略；
- 说话人分离（diarization）触发声纹=敏感个人信息（PIPL 单独同意），**V1 不做**。

### 5.3 安全前置（随本功能必须补的两件事）
1. **WSS/HTTPS 核实**：量产固件 base URL 是 `http://bbclaw.daboluo.cc`
   （`sdkconfig.bbclaw.latest:610`）——持续环境音**绝不能明文上行**，先核实线上
   网关实际行为，固件切 `https://`（WS 升 wss）；
2. **per-device secret**：现有设备 WS 鉴权=binding 存在即放行（`server.go:1629-1633`），
   伪造 device_id 即可灌音频/冒充设备。对话场景风险有限，环境音是敏感数据——
   配对时下发 device token，WS 握手校验。

### 5.4 隐私数据面（PIPL 最小必要）
- 保留策略：**原始音频默认 30 天自动删，转写/摘要长存**（Limitless 同款，业界验证）；
- 用户自助删除：portal「删除全部录音数据」端点（先例：`DELETE /v1/chat/history`）；
- 账户级「录音功能开关 + 知情同意」标志；
- 数据全链路留境内（现有部署即满足）；绝不进无鉴权调试页（`/debug/audio` 前车之鉴）。

## 6. 分期落地

| 期 | 内容 | 出口条件 |
|----|------|----------|
| **P1a 本地录音**(先行,无云端依赖) | SD 卡 bring-up(SDMMC 1-bit) + RECORDER 态(入口/常显指示/互斥/PTT=书签) + 60s 分段落 SD + 段索引 | 真机 4h+ 连续录音不丢段;拔电重启索引完好 |
| **P1b 回网补传** | 固件同步引擎(ambient.* WS kinds) + 云端落盘/ack + 30 天生命周期 + portal 列表/删除;**随包完成 wss 核实**(环境音不明文) | 断网数小时回网后自动补齐;云端可见可删 |
| **P2 解读** | ambient worker: VAD→批 ASR(火山录音文件异步)→transcript→daily digest;bookmark 锚点入摘要;**转写文本下发 adapter**(/admin 新增「录音记录」页,原始音频留云端短保留,文本轻量长价值——2026-07-05 用户补充) | 每天自动出 digest,成本 ≤¥7/设备/天;adapter 页可浏览转写 |
| **P3 增强** | per-device token、功耗定标(降频/批传/熄屏)、双麦立体声/波束、digest 接入对话上下文;Track B 专用硬件立项(4G);libopus 伪栈化(省每任务 ~20KB 栈);采集丢帧 gap 标记入索引(时间轴诚实) | 续航实测达标 |

## 7. 决策记录（2026-07-05）

1. **方向**：Track A 先行——手表 + SD 本地优先 + 回网补传（用户拍板）；
   Track B（专用 4G 随身硬件）列 P3 立项评估。
2. 原始音频保留窗口：默认 30 天（转写长存）——采用默认值,可后台调。
3. ASR 供应商：火山录音文件异步识别（批价,与现有 ASR 同一家）。
4. 安全：wss 核实并入 P1b（补传上线前必须）；per-device token 挪 P3。
5. 「只留转写」隐私档位：暂不做,P2 后视需要。
6. 转写展示端（2026-07-05 补充）：文本同步到本地 adapter 管理页展示;
   云端暂无对象存储(单 VPS 本地盘+30 天生命周期),设备量上来再迁 OSS。
7. **设备端回放页（2026-07-12 补充，P1a 附属）**：Recordings 列表点一条录音打开
   专属回放页（`bb_page_recplay`），不再「后台连播 / 再点停」。触屏手表 transport
   全触控：上一首 / 播放·暂停 / 下一首 / 停止 + 可拖动音量条;顶部标题、大号已播
   时长、`段号 N/M | 总时长`、会话进度条。
   - **「上一首/下一首」= 录音之间切换,不是段级**：一次录音（会话）对用户即「一首」,
     60s 分段是内部流式实现细节;段级跳被用户实测判「没用」。段内连播仍自动接续
     (natural seq++),回放页只暴露录音级导航。
   - **暂停**：`bb_audio` 播放循环在 chunk 边界设 pause gate——暂停期间持续喂静音
     (而非停写,避免 DMA 环形缓冲反复重播最后几 ms 的嗡鸣),位置原地冻结,恢复从
     同一样本继续。仅回放路径用,TTS 不置位。
   - **输入**：触屏走 LVGL 原生 CLICKED（indev v2）+ 右滑=BACK 退回列表;bench/按键板
     降级 OK=播放暂停、rotate=音量、LEFT/RIGHT=上/下一首。音量改动经既有 off-thread
     持久化路径(避免 stream_task PSRAM 栈上写 NVS 的 cache-freeze)。
   - 手表真机全项验证（渲染 / 播放 / 暂停冻结 / 多段连播 1/2→2/2 / 切换录音 / 音量 / 返回）。

## 8. 法规红线备忘（中国大陆，要点非法律意见）

- 单方同意（参与者）录音民事上基本可用（2019 年废止旧批复后按三条件判断）；
- **非参与者偷录他人私密对话是硬红线**（民法典 1032/1033 + 治安管理处罚法 42）；
- **窃听器材定性风险**：隐蔽录音硬件可涉刑（刑法 283/284）——不可关闭的录音
  指示 + 明显的模式切换 UI 是合规护身符；
- 声纹=敏感个人信息（PIPL），diarization/声纹识别需单独同意；
- 数据出境按敏感口径管制——全链路留境内。
