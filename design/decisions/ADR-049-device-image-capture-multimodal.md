# ADR-049: 设备摄像头拍照 → 多模态 Agent —— 图片信息类文本传输链路

- **状态**: **全链路真机打通（2026-07-22）**——设备拍照→cloud→adapter→claude 读图→回复→
  设备经云端 TTS 朗读，逐段真机验证；生产触发菜单已落地。设备播报采方案 B2（固件朗读云端反向路由的
  `voice.reply`，走已在产的 `/v1/tts/synthesize`，cloud/adapter 零改动零部署，见 §12 checklist）
- **日期**: 2026-07-19
- **组件**: firmware + adapter（+ cloud 纯 relay，MVP 零改动）
- **关联**: ADR-044（ambient 二进制上传模板）、ADR-035（adapter PTY 跑 claude，原生读图）、ADR-004（cloud_saas agent 代理）、ADR-027/048（cloud_saas 路由与 adapter 分配）
- **前置调研**: 两路并行（固件摄像头/上传通道 + 云端/adapter 载荷限制与喂图落点），事实引用见各节

## 1. 目标与定位

把嘉立创·实战派板（lichuang-szp）板载的 **OV2640 摄像头**利用起来：设备拍一张照片，
经现有 cloud_saas 链路传到终端 adapter，喂给跑在 adapter 里的 **claude 交互式 CLI**
做多模态理解，结果沿现有 agent 回路播报/显示。

一句话定位：**给对话形态加一只"眼睛"**——"看看这是什么""这段报错什么意思"
（把屏幕/实物拍给 AI）。图片是**伴随一次意图**的单帧,不是连续视频流（那是将来的事，见 §9）。

### 与 ambient（ADR-044）的区别（避免混淆两条数据路径）
- ambient 是**设备→cloud 落盘**的长录音，云端异步 ASR，**永不经 adapter**。
- 本 ADR 是**设备→cloud→adapter→claude** 的单帧图片，**必须到达 adapter**（claude 在那儿）。
- 两者只共享"复用设备 WS 通道 + 二进制上传"的工程手法，数据流向根本不同。

## 2. 总体架构

```
[实战派] OV2640 DVP → esp32-camera 采一帧 JPEG(VGA/SVGA, PSRAM fb)
         → base64 编码(PSRAM 连续缓冲)
         → 单个 WS 文本 envelope: kind="image.capture" (payload.dataBase64)
[云端]   handleDeviceWS 文本分支 → 未知 kind 默认 hub.RouteFromPeer 中继
         → 按设备 active binding relay 给对应 home adapter（cloud 零改动、不落盘）
[adapter] cloudrelay handleRequest 新增 image.capture case
         → 解 base64 → os.WriteFile 落到 claude workspace(~/.bbclaw-adapter/workspace)
         → 复用现有 SubmitVoiceTurn 注入一句带绝对路径的 prompt
         → claude 原生读图 → 回复沿现有 agent.event / voice.reply 回路 → 设备 TTS 播报
```

### 为什么图片走 base64 文本 envelope（relay），而不是 ambient 的二进制帧
调研确证的**硬约束**（决定了方案唯一性）：
- 设备→cloud 的**二进制帧永远本地消费**（ambient 落盘 / voice buffer），**从不 relay 给 adapter**
  （`cloud/internal/httpapi/server.go:2090-2115`）。claude 在 adapter，图片必须到 adapter，
  所以**二进制路线到不了目的地**。
- 只有 **JSON 文本 envelope 会被 relay**（`hub.RouteFromPeer` → `WriteEnvelope`，`hub.go:227/640`），
  且 hub 对 envelope payload **无大小检查**。
- 因此"到达 adapter"唯一简单路径 = 把 JPEG base64 塞进文本 envelope 的 payload，走 relay。

### 为什么 cloud 侧 MVP 零改动
`handleDeviceWS` 文本分支对**未识别的 kind 默认走 `hub.RouteFromPeer` 中继**。
`image.capture` 不在 cloud 的 switch 里 → 自动落入 default relay 分支 → 按设备 active binding
转给 adapter。cloud 既不解码也不落盘，**纯管道**。这也顺带满足隐私诉求（图片不在云端留存）。

## 3. 载荷大小与那道 1 MiB 悬崖

各环节实测上限（调研结论）：

| 环节 | 限制 | 位置 |
|---|---|---|
| 设备→cloud WS 传输层 | **无上限**（未设 SetReadLimit） | `server.go:150` upgrader |
| cloud→adapter relay envelope | 无大小检查 | `hub.go:227` RouteFromPeer |
| **adapter 收 cloud WS 单帧** | **1 MiB**（`SetReadLimit(1<<20)`） | `cloudrelay.go:330` ← 绑定约束 |

- **1 MiB / 1.33（base64 膨胀）≈ 768 KB 原始 JPEG 天花板**。
- OV2640 单帧 JPEG：VGA(640×480) ~30–60KB、SVGA(800×600) ~60–100KB、即便 UXGA 高分辨率
  通常也几百 KB —— **base64 后仍在 1 MiB 内**。
- **决策**：① 固件默认拍 **SVGA、JPEG quality 适中**，把 base64 后钉在 ~150KB 量级，
  远离悬崖；② 同时把 `cloudrelay.go:330` 的 `SetReadLimit` 从 1 MiB **提到 4 MiB**
  （一行改动、cloud 侧本就无限）——给高分辨率留舒适余量、消除"复杂画面偶发超限断连"的尾部风险。
  两个一起做，既省内存又稳。

## 4. 协议契约（device ↔ cloud ↔ adapter，逐字锁定）

单请求 + 单 ack，复用现有 envelope（`type/kind/messageId/deviceId/payload`）：

```jsonc
// 设备 → cloud →(relay)→ adapter
{ "type":"request", "kind":"image.capture", "messageId":"<seq>", "deviceId":"<id>",
  "payload":{
    "format":"jpeg",          // MVP 固定 jpeg
    "width":800, "height":600,
    "bytes":72413,            // 原始 JPEG 字节数（adapter 解码后校验）
    "dataBase64":"<...>",     // 原始 JPEG 的 base64
    "note":"这段报错什么意思"  // 可选：伴随意图/语音文本；空则用默认 prompt
  }}

// adapter → cloud →(relay)→ 设备：成功
{ "type":"reply", "kind":"image.capture.ack", "messageId":"<seq>",
  "payload":{ "ok":true } }
// 失败（顶层 error 稳定 code + payload.error 详情，沿用 ADR-027 双写约定）
{ "type":"error", "kind":"image.capture.ack", "messageId":"<seq>",
  "error":"IMAGE_TOO_LARGE", "payload":{"error":{"code":"IMAGE_TOO_LARGE","detail":"..."}} }
```

- 错误码：`IMAGE_TOO_LARGE`（超 adapter 限）/ `IMAGE_DECODE_FAILED`（base64 或 bytes 不符）/
  `AGENT_BUSY`（当前有 turn 在跑）/ `INTERNAL`。
- ack 语义 = "已落盘且已注入 agent"。设备收 ack 后即可回收 JPEG 缓冲；**回复内容不在 ack 里**，
  沿现有 agent 回路异步流回（与语音 turn 一致）。
- `image.capture` **触发一次完整 agent turn**：adapter 落盘后立即 `SubmitVoiceTurn` 注入
  `note`（或默认 "描述你看到的画面"）+ 图片绝对路径，claude 读图并回复 → 现有
  `agent.event`/`voice.reply`/TTS 回路播报。**下游零新增**。

## 5. 固件设计（本 ADR 的主要工程量与风险）

### 5.1 摄像头 bring-up（唯一硬骨头，但已被大幅去风险）
现状：板载 OV2640 存在，但固件**零适配**——无 `esp32-camera` 依赖、`board_config.h`
无 DVP 引脚、明写"本阶段不适配"。要做：

1. 引入托管组件 `espressif/esp32-camera`（新 `idf_component.yml` 依赖）。
2. `boards/lichuang-szp/board_config.h` 补 DVP 引脚：8 根数据线 Y2–Y9 + XCLK/PCLK/HREF/VSYNC。
   **引脚真相来源 = xiaozhi `main/boards/lichuang-dev/`**（与本板显示/音频同源，已验证共存）。
3. **两个子系统已现成、直接复用**（关键去风险点）：
   - 摄像头 **PWDN 挂 PCA9557 bit2**，而固件**已有 `bb_pca9557.c` 在驱动这颗芯片**
     （bit0=LCD_CS、bit1=PA_EN）——上电控制半成品已在。
   - 摄像头配置总线 **SCCB = 已初始化好的那条 I2C（GPIO1/2）**——配置通道现成。
4. 新模块 `bb_camera.c`：init（含 PCA9557 bit2 上电时序）+ `bb_camera_capture_jpeg()`
   返回一帧 JPEG（`esp_camera_fb_get`）。**帧缓冲 `CAMERA_FB_IN_PSRAM`**。
5. **GPIO 冲突核对**（bring-up 必做）：DVP 数据线不得与现用引脚重叠——现占用为
   显示 SPI(40/41/39/42)、音频 I2S(38/14/13/45/12)、I2C(1/2)、PTT(0)。照 xiaozhi 抄即无冲突
   （量产参考设计硬件已保证共存），但仍要逐脚 diff 一遍。

### 5.2 内存预算（无压力）
- 8MB Octal PSRAM。JPEG fb(SVGA ~100KB) + base64 连续缓冲(~135KB) 均放 PSRAM
  （`MALLOC_CAP_SPIRAM`）；对照 LVGL framebuffer 已吃 150KB，量级毫无压力。
- **内部 DRAM 一律不碰**（它才是紧张资源，见 memory [[internal-dram-recurring-root-cause]]）。
- 上传 task 用 PSRAM 栈（`xTaskCreateWithCaps`，PREFER_PSRAM），别在 LVGL 线程做编码/发送。

### 5.3 触发交互（MVP：显式菜单，隐私可控）
- **待机主菜单加「拍照」行**（复用 ADR-044/ADR-021 的菜单 + OK 二次确认模式）。
- 选中 → `bb_camera_capture_jpeg()` → 屏上给"拍照中/上传中"反馈 → 发 `image.capture`
  → 等 ack → 回复经 TTS 播报。
- 复用现有 WS 发送原语 `bb_adapter_client_send_text()`（单帧文本 envelope）。
- **不做**静默/连续拍照（隐私红线，见 §7）。

## 6. adapter 设计（几乎零新增）
`internal/cloudrelay/cloudrelay.go` 的 `handleRequest` 加一个 `image.capture` case：
1. 解 `payload.dataBase64` → 校验长度==`bytes` → `os.WriteFile` 到 claude workspace
   `~/.bbclaw-adapter/workspace/`（claude 的 cwd，落这儿它直接能读；文件名带 deviceId +
   单调序号，避免并发覆盖）。
2. 调**现有** `bridge.SubmitVoiceTurn(prompt)`（`deviceapi.go:548`）注入 `note`（或默认）
   + 图片绝对路径。**claude 交互式 PTY 原生读图**（ADR-035），adapter 侧不需要任何多模态代码。
3. 回 `image.capture.ack`。
4. 清理：图片用完（turn 结束 / 达数量上限）删除，避免 workspace 堆积。

## 7. 隐私与安全边界
- **仅显式拍照**（菜单 + 确认），MVP 无静默/连续采集——摄像头比麦克风更敏感，定性上先立规矩。
- **明文传输是当前红线**：量产 base URL 仍是 `http://bbclaw.daboluo.cc`
  （`sdkconfig.bbclaw.latest`）。**图片上线前必须先切 wss/https**；per-device token 尚未落地
  （现为"binding 存在即放行"，见 [[ota-decision-backend-only]] 同源问题）。
- 图片落在 adapter 本地磁盘（用户自己的机器），用完即删；cloud 纯 relay 不留存。

## 8. 三端改动清单
### firmware（bbclaw，主要工程量）
- `idf_component.yml`：+ `espressif/esp32-camera`。
- `boards/lichuang-szp/board_config.h`：DVP 引脚 + `BBCLAW_CAMERA_ENABLE`。
- 新 `bb_camera.c`（init + capture，PCA9557 bit2 上电，PSRAM fb）。
- 主菜单「拍照」行 + capture→base64→`image.capture`→等 ack（PSRAM 栈 task）。
### adapter（bbclaw）
- `cloudrelay.go`：`SetReadLimit` 1→4 MiB；`handleRequest` 加 `image.capture` case
  （落盘 + `SubmitVoiceTurn` + ack + 清理）。
### cloud（bbclaw-reference）
- **MVP 零改动**（未知 kind 默认 relay）。仅需回归验证 `image.capture` 确实落入 default
  relay 分支、按 active binding 正确转发；确认无意外 size guard。
### design
- 本 ADR；README 表尾登记。

## 9. 分期
- **Phase 1（本 ADR MVP）**：显式菜单拍照 → base64 relay → adapter 落盘喂 claude → TTS 回复。
- **Phase 2（多模态同一 turn）**：PTT 按下抓帧、松手语音 transcript + 图片**同一 turn**
  喂 claude。需 adapter 按 `turnId` 关联 cloud ASR 来的 `voice.transcript` 与 `image.capture`
  两条消息（图片先到、缓存等 transcript）。协议加 `turnId` 字段即可，不推翻本设计。
- **Phase 3（大图/连拍/短视频）**：改走**仿 ambient 的二进制上传到 cloud 落盘 + adapter 主动
  HTTP fetch**（二进制不 relay，需新增 cloud HTTP 端点 + adapter fetch，类比固件拉 TTS）。
  仅当单帧 relay 不够用时才上，改动最大。

## 10. 备选方案
1. **图片走 ambient 同款二进制帧上传** — 否决：二进制永不 relay，到不了 adapter（§2）。
2. **cloud 落盘图片 + adapter fetch（Phase 3 方案）做 MVP** — 否决：为单帧小图引入 cloud
   HTTP 端点 + fetch 协调，过重；base64 relay 单帧足够且 cloud 零改动。
3. **adapter 侧解析图片做视觉（自己调多模态模型）** — 否决：claude CLI 已原生读图，
   落盘给路径最省，不重复造多模态管线（呼应 ADR-043 不抽公共 SDK 的思路）。
4. **设备端本地做视觉推理** — 否决：ESP32-S3 算力/内存不足以跑视觉模型；云端 claude 才是目的地。

## 11. 影响
- **正面**：对话形态升级为多模态，硬件既有摄像头被利用；后半链路（cloud/adapter/agent）
  近乎零成本，cloud MVP 零改动；claude 原生读图不引入新依赖。
- **负面/风险**：摄像头 bring-up 是实打实的固件工作（组件集成 + DVP 引脚 + GPIO 冲突核对），
  是唯一需要真机反复验证的环节；base64 单帧受 adapter 读限约束（已用限分辨率 + 提 limit 双保险化解）。
- **中性**：明文传输与 per-device token 是既有欠债，本功能让它更紧迫但非本 ADR 引入。

## 12. 实现 checklist
- [x] firmware: `esp32-camera` 组件 + `board_config.h` DVP 引脚（照 xiaozhi lichuang-dev）
- [x] firmware: `bb_camera.c` init（PCA9557 bit2 上电）+ capture JPEG（PSRAM fb）+ boot 抓帧串口自测（打 size + FFD8/FFD9 magic + 分辨率）
- [x] firmware: GPIO 冲突逐脚 diff（DVP 12 线 vs 显示/音频/I2C/PTT 零重叠；XCLK LEDC 避开 bb_led/背光）
- [x] firmware: 16MB 专属分区表（app 槽 4MB）——带 camera 固件 2.65MB 装不下 8MB 板 2.5MB 槽；`idf.py build` 通过，槽内余 37%
- [x] firmware: **真机验证完成**（2026-07-20）——采到完整 320x240 帧，DVP+SCCB+PSRAM 链路全通。**重要修正：板上 sensor 是 GC0308（PID=0x9b），不是 OV2640**（VGA 上限、无硬件 JPEG）
- [x] firmware: 拍完 `esp_camera_deinit` 释放内部 DRAM（否则 cam_hal 占内部堆→WiFi 起不来 WIFI ERR；deinit 后 31744→38912，WiFi/cloud 恢复）
- [x] **JPEG 上行软件编码**：GC0308 RGB565 VGA 采集 → esp32-camera `frame2jpg()` 软编（quality=10 → VGA ~7KB，base64 ~9.4KB，远在 1MiB 内）
- [x] firmware: 拍照走「按需 init→拍→deinit」（`bb_camera_shoot_and_send`），不常驻，与 WiFi 共存
- [x] firmware: `CAMERA_DMA_BUFFER_SIZE_MAX` 32K→8K——运行时内部 DRAM 仅 ~19KB，默认 30KB DMA 缓冲 malloc 失败，降到 ~7.6KB 可塞下（帧缓冲仍 PSRAM）
- [x] firmware: `bb_adapter_send_image_capture`（base64 → image.capture envelope → 云 WS，PSRAM 缓冲）
- [x] adapter: `cloudrelay.go` SetReadLimit 1→4 MiB
- [x] adapter: `handleImageCapture`（解码 → 落 workspace/inbox → `runTurn` 让 claude 读图 → voice.reply）
- [x] cloud: 未知 kind default relay 正确转发 `image.capture`（零代码改动，真机确认）
- [x] **端到端真机验证（2026-07-20）**：设备 VGA JPEG 上行 → cloud relay → adapter → **claude 读图并准确描述画面**（inbox 落图可视化确认）
- [x] firmware: 生产触发——设置菜单「拍照」行（`bb_ui_settings.c`：`MAIN_ROW_CAMERA`，cloud_saas-only 可见，点击→PSRAM 任务 `bb_camera_shoot_and_send`→行状态 拍摄中/已发送/失败）；测试钩子 `BBCLAW_CAMERA_TEST_UPLOAD`、boot `BBCLAW_CAMERA_SELFTEST` 均已关（生产按需 init→拍→deinit，无需 boot 自测）
- [x] 设备侧播报——**B2 方案真机验证通过（2026-07-22，实战派 BBClaw-A844E1）**。排查历程：先真机
  证伪了 §4"下游零新增复用 voice.reply"的假设——点「拍照」上行/relay/adapter/claude 读图/生成回复
  **全通**（claude 准确描述多张画面含自拍），但**设备不出声、屏幕停在设置页无回复 UI**。根因（已定位）：
  1. cloud_saas 设备**无本地 TTS**，播报音频只能由云端合成后流下来；
  2. 云端**只在 `voice.stream.finish` 语音回合里做 TTS**（PTT→云 ASR→adapter→回复→云 TTS→回流）；
     `image.capture` 是旁路 relay 文本，**从不触发 finish 回合**→云端不会 TTS 这条回复；
  3. 固件 `bb_adapter_client.c:1615`：reply/TTS 事件在 `finish_result==NULL` 时**直接丢弃**，
     拍照没建立该上下文→即便云端回文本也被丢。
  → §4"复用 voice.reply 回路、下游零新增"**不成立**（该回路只在设备发起的语音回合里活着）。
  修复必须让云端参与 TTS。候选方案：
  (A) Phase 2：照片挂进 PTT 语音回合（走 `voice.stream.finish`，云端本就 TTS）；
  (B1) cloud 给 image.capture 单独开一个 TTS-stream 回合（`server.go` 加 `image.capture` case，
      走 `SendRequestToApprovedHomeAdapterStream`+`deviceTTSChunkStreamer`→`tts.chunk`）+ 固件武装接收上下文；
  (B2) **【已选·已实现】** 固件朗读云端反向路由回来的 `voice.reply` 文本。
  → **决策 B2**（用户 2026-07-22 拍板"走云端"，取最低风险的云端方案）：
  - 关键事实：`image.capture` 走 default relay，adapter 的 `voice.reply` 因无在途 `pending` 被
    cloud **反向路由回设备**（`hub.go` RouteFromPeer→`WriteEnvelope`）——设备**本就收到**该文本 reply，
    只是原先在 `bb_adapter_client.c` type=reply 分支丢弃。
  - 实现（`414b226`，firmware only）：type=reply 里拦截 `kind=voice.reply` 且 `finish_result==NULL`
    （无在途语音回合，区别于正常 PTT turn 的流式 `tts.chunk` 播放，避免重复念），提取 `payload.text`
    调既有 `bb_adapter_speak_notification`→`POST /v1/tts/synthesize`→播 PCM16（与提醒播报同一条链路）。
  - **cloud/adapter 零改动、零部署**：云端 TTS 仍出力（`/v1/tts/synthesize` 已在产），是"走云端"的最低风险切法。
  - **真机验证通过（2026-07-22，实战派 BBClaw-A844E1）**：点「拍照」→ 串口实锤
    `bb_adapter: image.capture reply → speak (293 chars)` → `bb_adapter: notif-tts: play 540678 pcm bytes`
    （claude 293 字描述 → 云端合成 ~540KB PCM ≈17s → `bb_audio_play_pcm_blocking` 出声）。
    时序对齐：image sent(71589) → reply 回(82760,对上 adapter `elapsed=10.992s`) → 合成+播放(91020)。
    **整链 设备拍照→cloud→adapter→claude 读图→voice.reply→cloud 反向路由→设备 /v1/tts/synthesize→出声 全通。**
- [ ] 观察：真机长跑后 CDC0 被 `esp-x509-crt-bundle: Certificate validated` 洪水刷屏（~1–2/s），
  疑似 camera init/deinit 内部 DRAM 碎片→TLS 反复握手；回合仍能送达，但值得回头查（见 [[internal-dram-recurring-root-cause]]）。
- [ ] 安全: 上线前确认 wss/https（图片不走明文）；per-device token
- [ ] design: README 已登记（本 ADR）
