# BBClaw

![BBClaw](./bbclaw-blueprint.png)

BBClaw 是 OpenClaw 生态中的第一方硬件节点（Node），面向本地优先的低延迟语音与通知交互。

---

## 本仓开源内容

- **`firmware/`** — ESP32-S3 固件（Apache-2.0）
- **`docs/`** — 架构、协议、硬件与使用说明
- **`scripts/`**、**`tools/`** — 烧录、调试与本地辅助工具
- **路线图**：**开源外壳 / 结构** 与固件配套，面向 3D 打印与社区迭代（见 [ROADMAP.md](ROADMAP.md)）

设备侧音频经运行面（如 Adapter）完成 ASR，再以 OpenClaw 官方 **node** 文本/控制面接入 Gateway；拓扑与扩展链路见 `docs/architecture.md`。

#### 🔌 连接模式（当前）

| 模式 | 描述 | 状态 |
|------|------|------|
| **WiFi LAN** | 设备与运行面连同一 WiFi，通过 IP 通信 | ✅ 已验证 |

**当前方案**：设备烧录 WiFi 信息 + 运行面 IP 地址，局域网内直连。

---

## 🚀 v0.1.0 发布 (2026-03-21)

### 首次开源发布！🎉

BBClaw 极客版 v0.1.0 现已开源：对讲机与 BB 机是**体验灵感**（实时语音 + 推送通知），由同一套固件统一承载，而非两种可切换的「模式」。

#### ✅ 已实现功能

| 模块 | 功能 |
|------|------|
| **音频** | ASR 语音识别接入、VAD 语音活动检测 |
| **显示** | 1.47″ ST7789（172×320，横屏 320×172）、LVGL |
| **交互** | PTT 按键控制、触觉反馈 (马达) |
| **连接** | WiFi 入网、Adapter / Gateway 配对 |
| **节点** | Adapter 音频入口、OpenClaw 文本/事件交互 |

#### 📡 架构图（局域网主路径）

```
┌─────────────┐              ┌─────────────┐              ┌─────────────┐
│   BBClaw    │              │   Adapter   │              │   OpenClaw  │
│  (硬件设备)  │   WiFi LAN   │  (运行面)    │   HTTP/WS    │   Gateway   │
│              │◄────────────►│              │◄────────────►│   (服务端)  │
│  - ESP32-S3  │              │  - 固定IP    │              │             │
│  - ES8311    │              │  - 数据转发   │              │  - AI 处理   │
│  - ST7789    │              │  - ASR/TTS   │              │  - 节点文本入口 │
└─────────────┘              └─────────────┘              └─────────────┘
       │                            │                            │
       │  烧录配置:                  │                            │
       │  - WiFi SSID/密码           │                            │
       │  - Adapter IP 地址          │                            │
       ▼                            ▼                            ▼
   音频/控制                  流式音频/文本桥              Agent/LLM
```

#### 连接说明

| 链路 | 协议 | 说明 |
|------|------|------|
| BBClaw ↔ Adapter | WiFi LAN（同一内网） | 设备与 Adapter 在同一网络，通过 IP 通信 |
| Adapter ↔ Gateway | WebSocket | 以官方 node 方式注入 transcript / 订阅回复 |
| Gateway ↔ AI | OpenAI/本地模型 | 文本理解、回复生成 |

#### 📦 公开仓内容

- `firmware/` - ESP32-S3 全部固件源码，开源
- `docs/` - 架构、协议、硬件说明等文档，开源
- `scripts/` - 烧录/调试脚本，开源
- `tools/` - 本地 ASR/TTS 等可复用工具，开源

#### 🛠️ 硬件与外设（当前实现）

- **MCU**：ESP32-S3  
- **音频**：ES8311 CODEC（麦克风 / 扬声器链路）  
- **显示**：1.47″ ST7789，面板 172×320，横屏 320×172，LVGL  
- **交互**：PTT 按键、振动马达  
- **形态**：灵感上融合对讲与寻呼体验，固件一体承载（非模式切换）

**v1.0 方向**：**开发板极客方案 + 3D 打印外壳** — 优先可复现与体验迭代，不把单独小型化定制 PCB 作为 1.0 必达目标。中长期以 **外壳 IP**、**开源外壳 + 开源固件** 为主线。

#### 📄 License

Apache License 2.0

---

## 📣 最新进度

### ✅ 已完成

- **音频 ASR 接入** - 成功接入 OpenClaw Gateway
- VAD 语音活动检测
- 1.47″ ST7789（横屏 320×172）显示屏驱动
- PTT 按键控制
- 触觉反馈 (马达)
- **WiFi LAN Adapter 模式**（已验证）

### 🔄 当前进行

- 语音与通知链路的端侧体验持续优化
- 适配器回路文本回显到设备
- 公网与多部署拓扑的工程化（见 `docs/`）

### ⏳ 下一步（与路线图一致）

1. 音频 ASR 与端侧体验持续优化  
2. 马达与 LVGL 界面打磨  
3. BLE 等连接模式（规划中）  
4. 极客套件文档：接线、BOM、**3D 打印外壳**说明  

## 定位

- 不是第三方聊天平台 channel  
- 是由 OpenClaw Gateway 直接管理的设备节点  
- 当前开发主线是接入官方 `nodes` 体系，并向上游提交通用、最小、可解释的 PR  

## 当前开发方向

本仓库不再继续维护独立插件型网关实现。

当前策略：

- 运行链路由独立 **Adapter** 负责流式音频与 ASR  
- 与 OpenClaw 的边界优先保持在官方 `nodes` 文本/控制面  
- 上游 PR 只考虑通用、最小、可解释的节点能力补充  

已确认的方向：

- 设备对外主入口是运行面（如 Adapter），而不是让 Gateway 直接承载原始流媒体  
- OpenClaw 继续作为官方 node 控制面与 transcript 入口  
- 展示/下行目前先走 Adapter 过渡桥，后续再评估通用上游能力  

## 与 OpenClaw 的关系

- BBClaw 不是第三方聊天平台 channel  
- BBClaw 作为硬件节点接入 OpenClaw 官方 `nodes` 体系  
- 上游协作以通用、最小、可解释的能力补充为原则  

## 形态

**对讲机与 BB 机是设计灵感**，用来描述「实时语音 + 异步通知」这两类交互；产品在一条叙事里同时具备，由固件与场景呈现，**不是**用户在对讲 / 通知两种模式之间切换。

1. **实时语音（PTT 链路）** — 灵感来自对讲机  
   - 按下 PTT 后采集音频并上传到 Adapter  
   - Adapter 完成 ASR，并把 transcript 文本送入 OpenClaw  
   - OpenClaw 生成回复；设备通过 Adapter 侧桥接接收结果  

2. **通知与摘要** — 灵感来自寻呼 / BB 机  
   - 接收高优先级异步通知  
   - 支持离线补投与轻量摘要展示  
   - 设备侧触发震动/提示  

## 项目结构

```text
bbclaw/
├── firmware/        # ESP32-S3 固件源码（C/C++），开源
├── docs/            # 架构、协议、硬件/使用文档，开源
├── scripts/         # 烧录、调试脚本
├── tools/           # 本地 ASR/TTS 等工具
├── CHANGELOG.md     # 版本历史
└── LICENSE          # Apache 2.0
```

## 常用命令

```bash
make -C firmware build
make -C firmware flash
make -C firmware monitor
```

## 相关文档

- 架构草案：[docs/architecture.md](docs/architecture.md)  
- SaaS 平台架构草案：[docs/saas_platform_architecture.md](docs/saas_platform_architecture.md)  
- 协议草案：[docs/protocol_specs.md](docs/protocol_specs.md)  
- V1 协议基线：[docs/protocol_specs_v1.md](docs/protocol_specs_v1.md)  
- OpenClaw 集成路线：[docs/openclaw_integration_plan.md](docs/openclaw_integration_plan.md)  
- Beta 路线：[docs/beta_roadmap.md](docs/beta_roadmap.md)  
- 产品路线图：[ROADMAP.md](ROADMAP.md)  
