# adapter_v2 设备协议 bbwire/2（提案 — 待签字）

> 状态：**提案,待维护者签字后实施**。由 3 版独立设计(latency / firmware-simplicity / SaaS-relay)评审综合而来。
> 影响三方:**固件 / Cloud 中继 / adapter_v2**——签字后再动手。

## 核心原则

**一条设备主动拨出的 WebSocket;WS 帧的 opcode 即类型判别符。JSON 永不携带音频,二进制帧永不携带控制。**

- LAN 直连:`wss://<adapter-host>/v2/dev/ws`
- SaaS:`wss://bbclaw.daboluo.cc/v2/dev/ws`（仅 host 串不同,沿用 cloud_saas 出站姿态,家里不开入站口）

一条 socket 取代 v1 的四个端点(`/v1/stream/start|chunk|finish` + `/v1/tts/synthesize`)。

## 两种帧

- **TEXT 帧(opcode 0x1)**= 一个 JSON 控制对象(`t` 字段为类型)。
- **二进制帧(opcode 0x2)**= 8 字节小端头 + 原始编码音频(**无 base64**,省掉 v1 的 ~33% 开销):

```
byte 0    streamKind  0x01=上行麦克风  0x02=下行TTS
byte 1    codec       0x01=opus  0x02=pcm16
bytes 2-3 turnSeq     uint16,镜像 JSON 轮次的 u
bytes 4-5 frameSeq    uint16,每轮单调递增(gap/重排/断线续传)
bytes 6-7 flags       bit0=可播单元末帧  bit1=opus-config/关键帧
bytes 8.. 原始音频
```

(保留 8 字节头——断线续传 `ack upTo` 和中继路由都需要它;拒绝了无头方案和 16 字节过度设计。)

> **`flags.final` 语义**:标记**一个可播音频单元的末帧**——一次性 TTS 下是整段回复,逐句 TTS（Phase B）下是**一句**(每句一个 final 帧,设备收到即播该句、清累积缓冲)。**turn 结束由 `turn{idle}` 控制帧定,不是 `flags.final`**:设备必须在 `turn{idle}` 重新允许 PTT,绝不能在 `flags.final` 上结束(否则逐句 TTS 只会播第一句就停)。

## 帧 schema

**上行(设备→adapter)**
```
{"t":"hello","proto":2,"dev":"<id>","auth":"<token>","mic":{"codec":"opus","rate":16000,"ch":1},"spk":{"codec":"pcm16","rate":16000},"resume":"<lastTurnId|空>"}
{"t":"ptt.start","turnId":"u7","u":7}        // 按下PTT,开上行流 u=7
   (二进制上行帧 streamKind=0x01,u=7 在说话过程中边录边传)
{"t":"ptt.stop","turnId":"u7","u":7,"frames":42}
{"t":"ack","forTurn":"u7","upTo":120}        // 已播下行到 frameSeq=120(流控/barge-in截断点)
{"t":"ping"}
```

**下行(adapter→设备)**
```
{"t":"hello.ok","proto":2,"sessionId":"s_abc","spk":{...},"resumed":false}
{"t":"asr.final","turnId":"u7","text":"play some jazz"}   // 注入PTY的转写(屏幕回显)
{"t":"reply.start","turnId":"u7"}
{"t":"reply.delta","turnId":"u7","seq":3,"text":"Sure, "} // 增量回复文本,喂1.47"屏
{"t":"reply.end","turnId":"u7","text":"Sure, playing jazz now."} // 权威最终文本
   (二进制下行帧 streamKind=0x02,u=7 携带TTS,逐句合成即流式下发;flags.final=末帧)
{"t":"turn","turnId":"u7","state":"thinking|speaking|idle"} // 粗粒度忙/闲,idle=可PTT
{"t":"interrupted","turnId":"u7","atSeq":118}
{"t":"error","turnId":"u7","code":"ASR_EMPTY|ASR_TIMEOUT|TTS_FAILED|BUSY|UNAUTH","detail":"..."}
```

## PTT 一轮往返

1. 空闲:设备持 WS(开机一次 hello/hello.ok,ping 保活)。
2. 按 PTT → `ptt.start{u}`(若上一轮在回复 → 隐式打断,走现有 ESC 路径)。
3. 按住时麦克风 Opus 包**边编码边以二进制帧上传**(说话同时上传,无 3-POST)。
4. adapter 累积上行帧。
5. 松 PTT → 末帧置 `flags.final` + `ptt.stop`。adapter `Recognizer.Transcribe(buf)` → `asr.final` → `Bridge.SubmitVoiceTurn` 注入 PTY(与今天完全同路)。
6. PTY 渲染回复,extractor 抓增量 → 立即 `reply.delta` 下发(流式),小屏实时更新。
7. 回复文本按句切分,逐句 `Synthesizer.Synthesize` → 即合即以二进制下发,**第一句已在播,第二句还在抓/合**。
8. `Detector.TurnEnded` → `reply.end` + 末音频帧 + `turn{idle}`。

## 鉴权 & 会话

- **握手一次鉴权**(`hello.auth` = v1 的 Bearer),每连接一次,非每请求。
- **会话 = 这条 socket,1:1 绑一个 `deviceapi.Bridge`(一设备=一PTY会话)**,退掉 v1 的 deviceId+sessionKey+streamId 三元组。`sessionId` 由 adapter 返回,**与手机/web termchan 客户端可 attach 的是同一个 PTY 会话**(单 PTY 双视图)。
- **会话独立于连接存活**(Phase 1 已有的保证)。重连发 `hello{resume}`,adapter 补发未 ack 的下行,不重传音频。

## 映射到 deviceapi(adapter_v2 实际改动)

**接口签名不变**,协议是包在现有接口外的传输壳:
- 上行二进制帧 → 每轮 buffer → `ptt.stop` 时 `Recognizer.Transcribe(audio)`(今天的批量接口)。
- 转写 → `SubmitVoiceTurn` → `session.Write` — **不变**。
- 隐式/显式 barge-in → 现有 `inFlight`/ESC/`interruptSettle`/`rearm` — **不变**。
- **仅两处真改动**:
  1. **流式 `reply.delta`**:今天 `maybeSpeak` 只在 `TurnEnded` 发一次;改为 `Run` 在每次 `OnOutput()` 推增量,`reply.end` 权威去重。**触及最脆的 extract 层**——增量是 cosmetic、`end` 兜底,关掉则退回 boundary-only 安全路径(flag 门控)。
  2. **`DeviceSink` 变二进制帧 writer**:桩 `DiscardSink` 换成 `Play` 加 8 字节头 + `ws.Write`。

## 固件改动(净更简单)

cloud_saas 模式已有大部分:
- **删**:三个 HTTP POST + 单独 TTS POST、麦克风 base64 编码、`audioBase64` 解码、seq/streamId/sessionKey 簿记、INVALID_SEQUENCE 重试。
- **留并泛化**:`esp_websocket_client`、二进制收发、`tts_audio_buf` 累积、`bb_ogg_opus_decoder`、ping、自动重连。LAN 直连与 SaaS 收敛到**同一条代码路径**(仅 host 不同)。
- 状态机缩到 3 态:IDLE/CAPTURING/BUSY。
- 新增小逻辑:`u` 计数器、`reply.delta` 上屏、barge-in 停本地播放+`ack upTo`。

## Cloud 改动(加性,且一举两得)

设备语音 over SaaS 骑**现有出站中继**,只是帧格式变:
- `router.Envelope` 加 `streamType`(`voice`/`terminal`)+ `seq`(加性,旧 peer 忽略)。**语音和 Phase-4 手机终端共用一条隧道**,按 `sessionId+streamType` 解复用——**这就是 saas-takeover §"帧 mux" 那条 ADR,语音和终端共享**。
- 二进制帧按小 streamRef 边表 O(1) 路由(不逐包解 JSON),verbatim 透传保住免 base64。
- ASR/TTS 位置做成**每设备能力位**(同一 schema):Cloud 终止(默认,今天的路径,base64→二进制)或 Home 终止(LAN 私有,Cloud 透传二进制给 HomeAdapter)。设备协议字节级一致。

## 分期

- **Phase A ✅（已实现）**:adapter_v2 起 `/v2/dev/ws`,必需路径——批量 ASR、`reply.end`、一次性 TTS。零 deviceapi 接口改动,签字门 e2e 锁契约。
- **Phase B ✅（已实现）**:**流式 `reply.delta`**（快照模型——每帧带当前完整回复,设备替换字幕,稳健于 TUI 重绘;默认开 `ADAPTER_V2_STREAM_DELTA=1`,`reply.end` 仍权威）+ **逐句 TTS**（append-only 分句 `nextSentence`,切句即合即播,turn 末播尾巴;opt-in `ADAPTER_V2_SEGMENT_TTS=1`,默认关=Phase A 一次性 TTS）。e2e 验 `reply.delta` 到达。
- **Phase C**:resume/`ack` + barge-in 截断。
- **Phase D**:接 Cloud 中继(`streamType:voice` + 边表),Cloud-ASR/Home-ASR 能力位;Phase-4 终端 mux 落同一 envelope。

## 首个里程碑(签字验收门)

一个**冒充 ESP32 的 Go/Python WS 客户端**打 Phase-A `/v2/dev/ws`,无真 agent、无网络(复用 `cmd/mockcli` + `e2e` 套路):
1. 拨 `/v2/dev/ws` → `hello` → 期望 `hello.ok`。
2. `ptt.start{u:1}` → 几个二进制上行帧(canned PCM16,codec=0x02 免 Opus 依赖)→ `ptt.stop`(末帧 flags.final)。
3. 背后 adapter 接 `StaticRecognizer`(转写="hello")→ `SubmitVoiceTurn` → `cmd/mockcli` PTY(渲染 `ANSWER: hello`)。
4. 断言:收到 `asr.final{"hello"}` → `reply.end` 含 `ANSWER: hello` → ≥1 个 `streamKind=0x02` 二进制下行帧(flags.final)→ `turn{idle}`。

`make -C adapter_v2 e2e` 可跑,无外部依赖。**这个测试锁死传输契约,在任何固件/Cloud 改动之前就能签字。**

## 接受的取舍

1. 流式 `reply.delta` 触及最脆的 extract 层(flag 门控,可退回 boundary-only)。
2. 逐句 TTS 需对进行中的回复切句(可能韵律变碎;支持一次性合成兜底)。
3. 二进制 framing 比 v1 无状态 POST 多点固件状态、不好 curl 调(但 cloud_saas 已付这成本)。
4. 长连是语音线单点(ping ~15s 检出;resume 让恢复便宜)。
5. Cloud 持每流边表状态(小,turn-end/掉线清理)。
6. 两条 ASR 位置代码路径要测(换来"SaaS 近白送"+"LAN 私有"同一 schema)。
7. 砍掉必需的 `asr.partial`(无接口改动;留作未来加性帧)。
8. 固件+Cloud+adapter 协同改动,靠 `hello.proto:2` 能力握手保证半灰度不炸。
