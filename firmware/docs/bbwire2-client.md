# 固件 bbwire/2 客户端 — 实施计划

把固件连到 **adapter_v2** 的 bbwire/2 协议(`/v2/dev/ws`)。协议规范见 `adapter_v2/docs/device-protocol.md`,服务端线格式见 `adapter_v2/internal/devicews/frames.go`。

## 设计前提（侦察确认）

- **设备-adapter 抽象边界是真的**:`bb_radio_app.c` 只调 `bb_adapter_*`(见 `bb_adapter_client.h`),transport 分支(`bb_transport_is_cloud_saas()`)全在 `bb_adapter_client.c` 内部。bbwire/2 实现塞进同一接口背后,**radio app / LVGL / PTT 状态机全不用改**——"adapter 对设备无感"在此层已成立。
- **复用 cloud_saas 的 WS 栈 ~80%**:`esp_websocket_client`、`ws_send_text_message`、`ws_send_binary_message`、`tts_audio_buf` 累积、`bb_ogg_opus` 解码、text/binary opcode 分流、`json_extract_string`、I2S→Opus 采集环——全部现成。
- **现有 v1(local_home)保留**:bbwire/2 是第三个 transport profile,与 v1/cloud 并存。

## 选型决策(已确认)

- **adapter 选择**:运行时设置页菜单,**列表来自 SaaS 后台接口**(复用现有 ADR-027 Adapter picker UX)。这是 cloud 中介的选择层,需 Phase D/cloud 配合;**bbwire/2 协议客户端(LAN 直连)先做**,SaaS 列表 picker 后续。
- **driver/模型**:从设备设置移除选择,**保留只读显示**(顶栏 driver / 底栏 model 仍显示 adapter 选定值)。
- **首验路径**:LAN 直连 + bench-serial(`ptt tap`),首 bring-up 可发 canned PCM16(codec=0x02)绕开 Opus/ffmpeg。

## 协议映射(bbwire/2 帧 ↔ bb_adapter_* ↔ 事件)

| bb_radio_app 调用 | bbwire/2 上行 | 下行 → bb_finish_stream_event |
|---|---|---|
| `bb_adapter_stream_start` | `hello`(首连一次)+ `ptt.start{u}` | `hello.ok` |
| `bb_adapter_stream_chunk_pcm` | 二进制帧(8字节头 streamKind=0x01 + opus/pcm16) | — |
| `bb_adapter_stream_finish_stream` | `ptt.stop{u,flags.final}` | `asr.final`→`ASR_FINAL`;`reply.delta`→`REPLY_DELTA`;`reply.end`→填 `reply_text`;二进制 TTS→`TTS_CHUNK`;`turn{idle}`→`VOICE_DONE`;`error`→`ERROR` |
| `bb_adapter_tts_synthesize_pcm16` | (不需要——TTS 走下行二进制帧) | — |

## 增量 1 ✅（本轮，已编译验证）

**线格式核心 + 脚手架**(最易出字节序 bug 的部分先隔离验证):

- `firmware/include/bb_bbwire2.h` — 8 字节头 pack/unpack(static inline,严格对齐 `frames.go`)+ streamKind/codec/flags 常量 + 客户端 API 声明。host-gcc round-trip + 精确小端字节布局测试通过。
- `firmware/src/Kconfig.projbuild` — 第三档 `BBCLAW_TRANSPORT_PROFILE_LOCAL_HOME_V2` + `BBCLAW_ADAPTER_V2_BASE_URL`。
- `firmware/include/bb_config.h` — `BBCLAW_TRANSPORT_PROFILE "local_home_v2"` + `BBCLAW_ADAPTER_V2_BASE_URL` 宏。
- `firmware/src/bb_transport.{c,h}` — `bb_transport_is_v2()` 谓词。

## 增量 2 ✅（已实现，本机编译通过，待 flash 验证）

**独立 WS 客户端 `firmware/src/bb_bbwire2.c`**(~440 行)+ 集成。`idf.py build` 成功。三视角对抗 review:协议逐帧对齐服务端、正常路径并发安全;修了 BLOCKING（异常路径 finish-wait 无锁 TOCTOU → UAF,改为锁内调 callback,镜像 cloud）+ MAJOR（TTS chunk leak;v2 的 tts_synthesize/healthz 误打旧 adapter URL → `active_base_url()` 对 v2 返回 adapter_v2 的 HTTP origin、synthesize 对 v2 返回 NOT_SUPPORTED）。

**已知限制(首验可接受,后续硬化)**:
- 中途自动重连不重发 hello → 下一轮 `ensure_connected` 自愈(hello_ok 才是就绪门,非 BW_CONNECTED)。
- 下行只处理 PCM16(codec 0x02),Opus(0x01)留待后续;`flags.final` 未用于多帧可播单元拼接(Phase A 每句一帧,无需)。
- `turn_u`/`frame_seq` 跨任务无锁读(uint16 不撕裂 + 单任务串行写,实测安全)。

### 实现细节

**WS 客户端 + bb_adapter_* 集成**(`firmware/src/bb_bbwire2.c`):

1. **WS 生命周期**:`esp_websocket_client` 连 `<BBCLAW_ADAPTER_V2_BASE_URL>/v2/dev/ws`(镜像 `ws_client_ensure_connected`:TLS bundle、15s ping、自动重连)。连上发 `{"t":"hello","proto":2,"dev":<id>,"mic":{"codec":"opus","rate":16000,"ch":1},"spk":{"codec":"pcm16","rate":16000}}`,等 `hello.ok`。
2. **上行**:`bb_adapter_stream_start` → `ptt.start{u}`(u 单调递增);`bb_adapter_stream_chunk_pcm` → 现成 Ogg/Opus 编码帧前置 8 字节头(streamKind=0x01,codec=0x01)发二进制;`bb_adapter_stream_finish_stream` → `ptt.stop{u,frames}`,复用 WS 等待循环。
3. **下行分发**(在 WS event handler 按 opcode 分流,文本按 `"t"` 路由):`asr.final`/`reply.delta`/`reply.end`/`turn`/`error` → 对应 `bb_finish_stream_event_t`;二进制帧解 8 字节头,**按 codec 字节分支**(0x02 PCM16 直接进 TTS 播放;0x01 Opus 走 `bb_ogg_opus` 解码),按 `flags.final` 切可播单元。
4. **bb_adapter_client.c 集成**:5 个 stream 函数加 `bb_transport_is_v2()` 臂,delegate 到 `bb_bbwire2_*`。`bb_adapter_client.h` 签名不变,`bb_radio_app.c` 不动。

**首 bring-up 简化**:先用 canned PCM16(`mic.codec="pcm16"`,codec=0x02)绕开 Opus——adapter_v2 解码 Opus 需 ffmpeg(dev Mac 默认没有)。验通后再切真 Opus(`brew install ffmpeg`)。

### 硬骨头(增量 2 正视)

1. **下行 codec 分支**:Phase A 服务端回 PCM16,固件 `tts_audio_buf` 现假设 Opus——必须按下行头 codec 字节分支(真缺口,不只 framing)。
2. **mic 是 Opus**:adapter_v2 解码需 ffmpeg;首验用 canned PCM16 绕开。
3. **模拟器测不了**:首验只能 bench-serial(`ptt tap 1500`)对本地 `make -C adapter_v2 run`。

## 验收(增量 2)

bench-serial 对本地 `make -C adapter_v2 run`(ADDR=:18090):
1. 设备连 `/v2/dev/ws`,`hello`→`hello.ok`。
2. `ptt tap 1500` → `ptt.start` + 几个二进制 mic 帧 + `ptt.stop`。
3. 串口日志见 `asr.final` → `reply.end`(含 mockcli 的 `ANSWER:`)→ 二进制 TTS 帧 → `turn{idle}`,屏幕显示回复。
