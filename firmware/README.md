# BBClaw Firmware (ESP32-S3 / ESP-IDF)

该目录为 ESP-IDF 固件工程，当前主线是：

- `ESP32-S3 + INMP441` 采集
- `PTT` 触发上传到外部 adapter 运行面（HTTP）
- 结束后将 ASR 文本回显到 LVGL 状态层（ST7789）

接入文档：

- [Adapter 对接说明](./docs/bbclaw_adapter_integration.md)
- [公共 UI 资源](./docs/ui_assets.md)
- [macOS Simulator 用法](./docs/lvgl_macos_simulator.md)
- [Cloud V1 架构图](../docs/cloud_v1_architecture.md)

## 已有结构

- `CMakeLists.txt`: ESP-IDF 工程入口
- `main/main.c`: 固件启动入口（NVS 初始化 + 应用启动）
- `src/`: 核心组件（当前为可编译 stub）
- `include/`: 组件头文件
- `sdkconfig.defaults`: 默认构建配置（含 `esp32s3`）

## 当前模块边界

- `bb_gateway_node.*`: 保留的 nodes stub（未删除）
- `bb_wifi.*`: Wi-Fi 连接、NVS 凭据管理、STA 失败后的 AP 配网
- `bb_adapter_client.*`: `healthz/start/chunk/finish` HTTP 客户端
- `bb_audio.*`: I2S 音频读写（默认 INMP441，可切 ES8311），读取 PCM 帧并输出 Opus 上传包
- `bb_motor.*`: 触觉反馈（马达）驱动与事件震动模式
- `bb_led.*`: 状态灯（独立 R/Y/G 或共阴 RGB 模块）驱动与动画
- `bb_ptt.*`: PTT 输入 + 软消抖轮询
- `bb_display.*`: ST7789 + LVGL；右侧为「ME / AI」一问一答（多轮历史与滚动见 [display_chinese_and_navigation.md](./docs/display_chinese_and_navigation.md)）
- `bb_ui_assets.*`: 公共 icon/logo 位图资源
- `bb_radio_app.*`: 主流程编排（PTT -> stream -> transcript）

## 快速开始

1. 安装并配置 ESP-IDF（确保 `$(HOME)/esp/esp-idf` 可用，或导出自定义 `IDF_PATH`）
2. 初始化目标芯片（ESP32-S3）

```bash
cd firmware
make init
```

3. 编译

```bash
make build
```

若出现 **`project was configured with ... python_env ... while ... is currently active`**（换过 IDF 自带 Python 版本或重建过 `python_env` 后常见），构建目录里 CMake 仍指向旧解释器。清掉构建缓存后重配即可：

```bash
cd firmware
make fullclean
make init
make build
```

4. 烧录并监控

```bash
make flash
make monitor
```

5. 本地 mock adapter 冒烟（主机侧）

```bash
cd /Volumes/1TB/github/bbclaw
make go-e2e
```

6. 本地 UI 预览 / 导图

```bash
cd /Volumes/1TB/github/bbclaw/firmware
make sim-build
make sim-run
make sim-export-feedback
```

说明：

- simulator 现在直接复用固件真实 UI，不再单独维护一套布局
- 批量预览图默认输出到 `.cache/sim-feedback/`
- 当前约定是“设备端可旋转，simulator 和导图保持正常阅读方向”

## 你接下来需要填的硬件参数

- Wi-Fi: 首次上电走 AP 配网；如需编译期覆盖，可显式设置 `BBCLAW_WIFI_SSID`, `BBCLAW_WIFI_PASSWORD`
- Adapter: `BBCLAW_ADAPTER_BASE_URL`
- 设备标识: `BBCLAW_DEVICE_ID`, `BBCLAW_SESSION_KEY`
- PTT/音频/屏幕引脚（均在 `bb_config.h`）

## Wi-Fi 配网策略

- 启动时优先读取 NVS 中保存的 `ssid/password`
- 默认不内置 Wi-Fi 凭据；若显式覆盖了 `BBCLAW_WIFI_SSID` / `BBCLAW_WIFI_PASSWORD`，才会作为编译期回退值
- STA 在 `BBCLAW_WIFI_STA_CONNECT_TIMEOUT_MS` 内无法连上时，自动切到 SoftAP 配网模式
- SoftAP 默认参数见 `BBCLAW_WIFI_AP_*`，起网后访问 `http://192.168.4.1/`
- 提交新凭据后会写回 NVS 并自动重启，下一次启动优先使用新配置

## 引脚文档（专门维护）

所有引脚与接线映射统一维护在：
- [Pin Map](./docs/pin_map.md)

当前默认硬件基线：
- `INMP441 + MAX98357A`
- `PTT=GPIO0`
- `RYG LED={GPIO2,GPIO4,GPIO5}`
- `MOTOR=GPIO13`

## Opus 说明

- 上传 `codec` 默认是 `opus`（`BBCLAW_STREAM_CODEC`）。
- 当前固件端使用带 `BBPCM16` 封套的过渡编码；`src/asr-adapter` 的 opus 分支已兼容该封套用于联调。
- 后续可将 `bb_audio_encode_opus()` 替换为真实 Opus 编码实现，无需改 HTTP 协议。

## 设计约束

- 以 OpenClaw `nodes` 协议为上位接入主线
- 固件侧只实现通用节点能力，不做 BBClaw 私有协议分叉

## 固件模式策略

固件后续推荐保持：

- 一个主线代码库
- 两种运行模式

不建议长期拆成两条分支分别维护：

- 家庭内网版
- SaaS / Cloud 版

推荐做法：

1. `local_home`
- 继续走当前 `bbclaw-adapter` HTTP 主线

2. `cloud_saas`
- 新增 Cloud transport
- 由 Cloud 终止音频/ASR/TTS，并通过 Home Adapter relay transcript/reply
- 当前已接入 Cloud 健康检查与简单配对请求
- 当前已打通 Cloud 音频上行、文本回复、TTS 与 display 回路

以下模块应尽量共用：

- UI
- 音频采集
- PTT 状态机
- LED / 马达反馈
- 文本展示

以下模块应抽象成 transport 层：

- 请求上行
- 回复下行
- 鉴权参数
- endpoint 配置
- 重连策略

结论：

- 单主线双模式
- 不做两条长期产品分支

## 当前 Cloud 固件边界

当前固件里的 `cloud_saas` 这一步先做了启动引导层：

- `BBCLAW_TRANSPORT_PROFILE="cloud_saas"`
- 启动后访问 Cloud `/healthz`
- 启动后发起 `POST /v1/pairings/request`
- 录音链路复用 `stream start/chunk/finish`
- Cloud 直接承接音频流并调用云上 ASR
- Home Adapter 只承接 transcript / reply 传话
- Cloud 直接承接 `tts/synthesize`
- Cloud 直接承接 `display/task|pull|ack`
- 配对待审批时显示 `PAIR`
- 配对已批准后允许进入录音
- Cloud healthz 会暴露 `asr/tts/display` 能力，固件按能力降级

## 用 sdkconfig 切模式

现在 transport 模式和 endpoint 参数已经可以走 `sdkconfig / menuconfig`，不必继续手改 [bb_config.h](/Volumes/1TB/github/bbclaw/firmware/include/bb_config.h:1)。

使用方式：

```bash
cd firmware
make menuconfig
```

菜单位置：

- `BBClaw -> Transport Profile`
- `BBClaw -> Transport Endpoints`

可选模式：

- `local_home`
- `cloud_saas`

常用配置项：

- `BBClaw -> Transport Endpoints -> Local Adapter Base URL`
- `BBClaw -> Transport Endpoints -> Cloud Base URL`

当前规则：

- `local_home` 直接走本地 `bbclaw-adapter`
- `cloud_saas` 走 `Cloud -> Home Adapter -> 本地 bbclaw-adapter`
- `bb_config.h` 里的 transport 相关值现在主要作为兜底默认值

## Board 适配

固件支持多板子通过 Kconfig 切换，硬件引脚、音频、屏幕参数统一收敛在 `boards/<board>/board_config.h`。

当前支持：

| Board | 说明 |
| --- | --- |
| `breadboard` | 面包板开发基线（INMP441 + MAX98357A + SPI ST7789 1.47"，无电池采样、无旋钮） |
| `bbclaw` | BBClaw 自研 PCB（在 breadboard 基础上增加电池采样与旋钮导航） |
| `atk-dnesp32s3-box` | 正点原子 ESP32-S3 开发盒（ES8311 / NS4168 + i80 并口 ST7789 320×240） |

切换方式：

```bash
make set-board BOARD=bbclaw
make reconfigure
make build
```

或通过 `make menuconfig → BBClaw → Board` 选择。

新增 board 只需：
1. 新建 `boards/<name>/board_config.h`
2. 在 `Kconfig.projbuild` 加一个 choice
3. 在 `bb_config.h` 顶部加 `#elif`

## 致谢

- [xiaozhi-esp32](https://github.com/78/xiaozhi-esp32) — 优秀的开源 ESP32 语音助手方案，BBClaw 的 board 适配体系受其多板子架构启发，ATK-DNESP32S3-BOX 的引脚映射和硬件初始化参考了该项目的 board 实现
