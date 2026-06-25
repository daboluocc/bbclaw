# 内部 RAM 碎片 → adapter WebSocket 任务建不起来（PTT 录到音发不出）

日期：`2026-06-25` · 板型：bbclaw（ESP32-S3, OCT PSRAM 8MB）· 状态：持续追踪中

> 这是一类**反复出现**的故障，不是一次性 bug。每次新增常驻 UI/任务、内部堆又贴近上限时就可能复发。
> 本文记录**现象签名 + 诊断口径 + 已验证的处置手段 + 仍待做的根治项**，方便后续快速定位。

## 现象签名（串口日志）

PTT 按下、录到音，但流发不出去，agent 转 DIZZY：

```
E (xxxxx) websocket_client: Error create websocket task
E (xxxxx) bb_adapter: ws start failed free_heap=8239448 min_heap=8224828
E (xxxxx) bb_adapter: ws_send_text_message(...): ws connect failed
E (xxxxx) bb_radio_app: bb_adapter_stream_start failed esp=ESP_FAIL(after VAD arm)
I (xxxxx) bb_audio: capture summary frames=15 bytes=7680 timeouts=0   ← 音采到了
```

关键 mem 快照（`bb_adapter: mem ...` 那几行）：

```
total_free=8244088  total_largest=8126464   ← 这俩是 PSRAM，大得很，会误导
internal_free=33979 internal_largest=7680   ← 真正的瓶颈在这
spiram_free=8214096 spiram_largest=8126464
```

## 根因（已验证）

**不是总内存不够，是内部 DRAM 碎片。** `internal_free` 还有 ~33KB，但 `internal_largest` 只有 **7680B**。

`esp_websocket_client` 用 `xTaskCreate` 在**内部 DRAM** 建它的任务栈，bbclaw 这边配的是 **8192B**（[`bb_adapter_client.c`](../../src/bb_adapter_client.c) `ws_client_ensure_connected` 里 `.task_stack = 8192`）。它要**一整块连续 8192B**，而最大连续块只有 7680B → `xTaskCreate` 失败 → ws 连不上 → PTT 音频发不出去 → 服务端无音频 → ASR 无输入 → agent DIZZY。

**为什么碎**：点阵 UI 数百个长驻小对象 + CJK 字体 + Sessions 菜单 + ADR-033 弹窗页等，长驻内部分配把内部堆打散；`total_free` 那 8MB 是 PSRAM，跟这事无关。

**为什么反复**：ws client 是**首次 PTT 才懒加载**（`ws_client_ensure_connected` 只在流/请求路径调，不在开机）——这时 UI 已经把内部堆打散了。一旦断线/超时它还会 `esp_websocket_client_destroy` 再重建，重建又撞碎片。

## 诊断口径（怎么快速确认是这个）

1. 看日志里 `internal_largest` 是否 **< 8192**，而 `total_free`/`spiram_free` 很大 → 就是本故障。
2. 在线测：`heap_caps_get_largest_free_block(MALLOC_CAP_INTERNAL)` < ws 任务栈（8192）即复现条件。
3. 别被 `free_heap=8.2M` 骗了——那是含 PSRAM 的总数。

## 已验证的处置（把常驻任务栈挪去 PSRAM，省内部 RAM）

“别让自测/常驻任务偷内部 RAM” 系列修复，逐个把不碰 NVS/flash 的任务栈改 `xTaskCreateWithCaps(..., MALLOC_CAP_SPIRAM / PREFER_PSRAM)`：

| 任务 | 内部栈 | 提交 | 备注 |
|------|--------|------|------|
| `bb_uart_cmd` | 6144 | `7899e19` | dev 串口自测任务 |
| `bb_button_test` | 3072 | `c3d9c1d` | bbclaw 正式板也常驻（GPIO1） |
| WiFi/LWIP 动态缓冲 | 几十 KB | `fdf6181` | `CONFIG_SPIRAM_TRY_ALLOCATE_WIFI_LWIP=y` |
| `bb_capture_task` | 4096 | （本次） | PTT 时正在跑、只读 I2S→ring，挪 PSRAM 在 PTT 时段腾出 4KB 内部 |

**挪 PSRAM 栈的硬约束**（务必逐个核对再挪）：
- 任务**自身不能调 NVS/flash**（写 NVS/擦 flash 会关 cache，期间访问自己的 PSRAM 栈 = 崩）。这就是 `bb_radio_app.c` 把 NVS 操作隔离在**内部栈**子任务里的原因。
- 实时高优先级任务可挪，但要确认 PSRAM 栈延迟不影响其时序（`capture_task` 16k 采集余量充裕、又靠 ring buffer 解耦上传，已验证 OK；纯显示/DMA 实时任务要谨慎）。
- 用 `WithCaps` 建的任务若会被删，必须用 `vTaskDeleteWithCaps`；常驻不删则无所谓。

## 同类已修：OTA apply 任务建不起来（2026-06-26，预热根治）

现象（真机日志，设备 v0.5.15 收到 v0.5.19 OTA）：
```
bb_page_ota_confirm: confirm shown cur=v0.5.15 new=v0.5.19-g181923d
bb_radio_app: OTA confirm: user accepted version=v0.5.19-g181923d
E bb_radio_app: OTA apply task create failed (internal RAM exhausted?)   ← 装不上
```
同一根因：`ota_apply` 任务栈 **12KB 必须在内部 RAM**（下载+刷 flash 会冻结 cache，PSRAM 栈
会触发 `s_task_stack_is_sane_when_cache_frozen()` panic → reflash loop，见 issue #179），但它
原本是**用户确认 OTA 时才懒加载**——那会儿 UI 早把内部堆打散，最大连续块 < 12KB →
`xTaskCreate` 失败 → 设备拿不到更新。**这比 ws 那条更致命：OTA 是所有其他修复的下发通道，
装不上 = 全卡死。**

**修法 = 上面「根治项 1」的预热**（注意：它必须留内部 RAM，不能挪 PSRAM）：把 `ota_apply`
任务**开机最早**（`bb_radio_app_start` 第一件事，UI/字体/菜单分配之前）就 `xTaskCreate` 建好，
任务起来立刻 `ulTaskNotifyTake(portMAX_DELAY)` 阻塞；用户确认时 `ota_confirm_cb` 改为
`xTaskNotifyGive(handle)` 唤醒它。12KB 内部栈在堆还干净时一次性占住、本次开机复用，确认时
**永不再 race 碎片**。代价：从开机起常驻 12KB 内部 RAM（值得——换 OTA 永远可应用）。
见 `bb_radio_app.c` `ota_apply_task` / `ota_confirm_cb` / `bb_radio_app_start`。

## 仍待做的根治项（治标 vs 治本）

上面都是“少偷内部 RAM”的治标。真正治本是让 ws 任务**不依赖开机后才凑出的 8192 连续内部块**：

1. **开机预热 ws**（推荐）：WiFi 起来、确定 cloud_saas 后，早早调一次 `ws_client_ensure_connected()`，在内部堆还干净（大连续块还在）时把 8192 任务建好并常驻（keepalive 15s ping 已支持长连）。后续 PTT 复用，绕开碎片。
   - 配套：断线时别 `destroy` client（靠 auto-reconnect 保活），否则重建又撞碎片。
2. **压 ws 任务栈**（谨慎）：把 `.task_stack` 8192 调小到当前最大块之下。**风险**：wss 的 mbedTLS 握手吃栈，压过头会**栈溢出崩溃**——把“可恢复的连不上”换成“崩”，更糟。要先量 TLS 实际高水位再压，不要盲调。

## 相关文件

- [`firmware/src/bb_adapter_client.c`](../../src/bb_adapter_client.c) — `ws_client_ensure_connected`（`.task_stack = 8192`、懒加载、断线重建）
- [`firmware/src/bb_radio_app.c`](../../src/bb_radio_app.c) — `capture_task` / `stream_task` 创建（PSRAM 栈范式）
- [`firmware/include/bb_config.h`](../../include/bb_config.h) — `BBCLAW_MALLOC_CAP_PREFER_PSRAM`
