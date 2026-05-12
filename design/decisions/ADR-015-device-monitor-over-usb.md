# ADR-015: Device Monitor over USB (screenshot + key injection)

- **日期**: 2026-05-12（v3，最终方案）
- **状态**: 已实现
- **关联**: ADR-001（adapter 作为 Agent Bus）；ADR-006（Flipper 6-button 事件）；ADR-011（adapter 开源）

## 背景

开发期闭环长期缺失"看屏 + 戳屏"能力：

- 改了 UI 代码，必须烧到设备后用眼睛看屏才知道效果，没法截图发 PR、写 issue、回归对比
- 调试某个 picker / settings 分支，必须物理按键复现，无法脚本化
- macOS/SDL 模拟器（`make sim-run`）只能验证设计期，**真机渲染管线、字体、面板时序的 bug 抓不到**

参考 qFlipper：USB 一插即可在上位机看屏 + 远程点击。本 ADR 提出 qFlipper 同款架构在 bbclaw 上的落地：**TinyUSB 双 CDC，单根 USB-C，零外置硬件**。

v1 范围：
1. **截图**：`devmon_screenshot.py` 一次抓一帧，存 PNG
2. **按键注入**：`devmon_key.py` 把虚拟事件喂给 LVGL

实时流、Web 渲染、录制回放、touch 注入等**不在范围**，留待后续 ADR 增量扩展。

## 决策

### 1. TinyUSB 双 CDC over chip native USB

固件启用 **TinyUSB** 在芯片 native USB 引脚（GPIO 19/20）上提供两个 CDC ACM 接口：

```
                                ┌─── CDC0  (预留 console，目前 silent)
USB-C ── CH334F Hub ── GPIO19/20 ──┤
                                └─── CDC1  (二进制帧协议)
        └── CH340  ── UART0       ── 烧录 + UART0 console（不变）
```

host 上单根 USB-C 同时看到 3 个 tty：

| 端口 | 用途 |
|---|---|
| `usbserial-XXXX` | CH340 → UART0：烧录 + 业务 console（自动复位电路） |
| `usbmodemNNNN1` (CDC0) | 预留 console，当前未启用 stdio 重定向 |
| `usbmodemNNNN3` (CDC1) | 二进制帧协议（截图、按键） |

> **CDC0 console redirect 已暂时移除**：实测 `esp_tusb_init_console(CDC0)` 静默失败（host 收不到任何字节），原因待查。CDC0 接口仍枚举出来给 host 留位置，stdio 维持在 UART0。当前看日志仍走 CH340 / `make monitor`。

### 2. 硬件依赖：必须有 native USB 引出

ADR 早期版本（v1, v2）讨论过两条备选方案，最终都被砍：

| 方案 | 状态 | 砍的原因 |
|---|---|---|
| **v1: TinyUSB 双 CDC**（本 ADR 最终方案） | ✅ 采用 | 生产 PCB 的 CH334F Hub 把 native USB 拉到 USB-C |
| v2 临时方案: UART1 + 外置 USB-UART 模块（GPIO 38/39） | ❌ 砍 | 旧面包板 native USB 没接出来时用过；生产 PCB 不需要 |
| v3 提议: WiFi 本地 HTTP | ❌ 未实施 | 需要 WiFi 在线；开发期"插上就用"体验不如 USB |
| v4 提议: USJ + 帧分离 | ❌ 砍 | 日志和帧混在同一通道脆弱 |

**硬性前提**：板子的 GPIO 19/20 必须物理连接到 USB-C 接口（直连或经 USB Hub）。
- ✅ 生产 BBClaw PCB（CH334F Hub 版本）：满足
- ❌ 早期面包板 / 仅 CH340 的简化板：**不满足，无法用本方案**

老板子的备份方案是 ADR-015 v2 的 UART1 + 外置模块；代码当前不保留这条路径（如未来需要可从 git 历史里恢复 commit）。

### 3. 帧协议

```
+--------+------+------+--------+-----+----------+-----+
| magic  | kind | flags| len    | seq | payload  | crc |
| 2 B    | 1 B  | 1 B  | 4 B    | 2 B | len B    | 2 B |
+--------+------+------+--------+-----+----------+-----+
| 0xBBC1 |      |  0   | u32 LE | u16 |          | u16 |
+--------+------+------+--------+-----+----------+-----+
```

- `magic = 0xBBC1`：固定 sync，跟 ESP_LOG 文本不冲突
- `kind`：枚举（见下表）
- `flags`：保留为 0
- `len`：u32（v1 是 u16，但 320×172 RGB565 = 110080 > 65535，必须 u32）
- `seq`：req/res 关联用，host 自增、device 在响应里回填
- `crc`：CRC16-CCITT (poly 0x1021, init 0xFFFF)，覆盖 `kind..end-of-payload`（magic 不算）

Kind 枚举：

| 值 | 名称 | payload |
|---|---|---|
| 0x01 | REQ_ECHO | 任意，echo 测试用 |
| 0x02 | RES_ECHO | 复制 REQ_ECHO payload |
| 0x03 | REQ_SCREENSHOT | 空 |
| 0x04 | RES_SCREENSHOT | `u16 width + u16 height + RGB565 LE pixels (w*h*2)` |
| 0x05 | REQ_INPUT | `u8 event_id`（对应 `bb_nav_event_t`） |
| 0x06 | RES_INPUT_ACK | `u8 status` |
| 0xFF | ERR | `u8 code`（见 device_monitor_err_t） |

### 4. 截图实现：`lv_snapshot_take` + 流式 TX

LVGL 9.5 的 `lv_snapshot_take(lv_screen_active(), LV_COLOR_FORMAT_RGB565)` 返回 `lv_draw_buf_t*`，包含整屏像素。释放用 `lv_draw_buf_destroy`。

关键 sdkconfig：
- `CONFIG_LV_USE_SNAPSHOT=y` — 默认关闭，必须开
- `CONFIG_LV_USE_CLIB_MALLOC=y` — 切到 libc malloc（自动用 PSRAM），不然 LVGL 默认 64KB 静态 pool 装不下 110KB 帧；提到 256KB 会撑爆 OTA 分区

TX 用 `tinyusb_cdcacm_write_queue` 分批 queue，buffer 满了才 flush（不是每 chunk flush），避免大帧传输时 flush 调用过多导致 timeout。

### 5. 关键修复：Worker task 解耦 TX 死锁

**最难调的一个坑**：早期实现把 dispatcher 直接放在 CDC RX callback 里。callback 由 TinyUSB 内部 task 调用 → 在 callback 里调 `tinyusb_cdcacm_write_flush` 等大数据 TX 完成 → 但 TX 完成事件需要同一个 TinyUSB task 处理 → **自死锁**。

症状：echo（payload < TX buffer）能工作 1-2 次然后挂；screenshot（110KB）一次就挂死整个 USB stack。

修复：引入独立 worker task + queue。RX callback 只入队立刻返回；worker task 在自己上下文里跑 LVGL lock、snapshot、大帧 TX。

```c
// CDC RX 回调（TinyUSB 上下文，必须快速返回）
static void on_frame(...) { xQueueSend(s_req_queue, &msg, 0); }

// Worker task（独立上下文，可以做重活）
static void devmon_worker_task(void* arg) {
  while (true) {
    xQueueReceive(s_req_queue, &msg, portMAX_DELAY);
    // ... 调用 lvgl_port_lock + lv_snapshot_take + devmon_send_*
  }
}
```

### 6. 按键注入：直调 `bb_nav_input_inject`

bbclaw 的输入**不走 LVGL `lv_indev`**，是 `esp_timer` 轮询 GPIO + 直接 callback 派发（见 `bb_nav_input.c`）。注入路径必须匹配：

```c
void bb_nav_input_inject(bb_nav_event_t event);
// 实现：直接调内部 emit_event()，绕开 GPIO/防抖
```

CLI 按键名映射到 enum：`up/down/left/right/ok/back/ok-long` → `BB_NAV_EVENT_*`。

### 7. menuconfig 开关

`CONFIG_BBCLAW_DEVICE_MONITOR`，默认 **`y`**（开发优先）。release sdkconfig 应显式置 `n` 编译剔除整块代码（包括 TinyUSB 栈）。

### 8. Host 端工具

| 脚本 | 命令 |
|---|---|
| [scripts/devmon_echo_test.py](../../firmware/scripts/devmon_echo_test.py) | `python3 ... --port /dev/cu.usbmodemNNNN3 --payload hello` |
| [scripts/devmon_screenshot.py](../../firmware/scripts/devmon_screenshot.py) | `python3 ... --port /dev/cu.usbmodemNNNN3 -o out.png` |
| [scripts/devmon_key.py](../../firmware/scripts/devmon_key.py) | `python3 ... --port /dev/cu.usbmodemNNNN3 ok` |

Python 依赖：`pyserial`、`Pillow`、`numpy`。截图脚本带 retry（默认 2 次），覆盖 UI 转场动画导致 snapshot 暂时失败的窗口。

### 9. 烧录流程（v3.1 起全自动）

最初的设计假设 TinyUSB 方案必须手动 BOOT+RESET（因为用户态 USB 不响应 DTR/RTS）。2026-05-12 加入 **REQ_REBOOT_TO_BOOTLOADER (kind=0x07)** 协议命令后实现全自动：

```c
// firmware 实现要点
SET_PERI_REG_MASK(RTC_CNTL_OPTION1_REG, RTC_CNTL_FORCE_DOWNLOAD_BOOT);
esp_rom_software_reset_system();  // ⚠️ 必须用这个，esp_restart() 不会让 ROM 检查 flag
```

完整 AI 自主闭环：

```bash
# 1. 协议触发设备进 ROM bootloader（替代物理按键）
python3 firmware/scripts/devmon_reboot.py --port $CDC1 --wait-for-bootloader

# 2. 烧
make flash PORT=/dev/cu.usbmodem2124401

# 3. 烧完 esptool 的 hard_reset 让新固件正常启动（这次工作了，可能因为芯片是 software 进来的没有遗留 BOOT 物理状态）

# 4. 等 TinyUSB CDC 重新枚举
until ls /dev/cu.usbmodem*3 2>/dev/null | grep -q .; do sleep 0.3; done
```

单次迭代 ~30-40 秒，无按键，AI 可自主执行。

仅以下场景仍需物理 BOOT+RESET：
- 首次部署带 REQ_REBOOT_TO_BOOTLOADER 的固件（之前固件没有该命令）
- 固件 boot 早期崩溃，TinyUSB 没起来
- 极少数 chip 状态异常

## 后果

### 正面

- 真正的"USB 一插即用"开发闭环：PR 可以附设备截图，bug 复现可脚本化
- 单根 USB-C，零外置硬件，跟 qFlipper 体验对齐
- 通道与云端解耦，开发期不依赖网络、不依赖 `bbclaw-reference` 私仓
- menuconfig 开关让 release 固件零 footprint
- 协议为未来实时流、touch、录制留好扩展位
- **意外收获**：注入按键 + 立刻截图能暴露 UI 状态机的边角 bug（v1 测试中即发现 OK→snapshot→BACK 触发 crash 的现有问题）

### 负面 / 风险

- **板子硬性依赖**：必须把 GPIO 19/20 拉到 USB 接口；老板子不能用本方案
- **烧录多一步**：BOOT+RESET 手动操作，无 auto-reset。可以加 "REQ_REBOOT_TO_BOOTLOADER" 协议指令优化，未来工作
- **CDC0 console 暂未通**：esp_tusb_init_console 失败原因未查，目前 stdio 只在 UART0/CH340 上看
- **截图阻塞 LVGL ~20ms**：单次操作峰值 110KB 分配 + 拷贝，对实时 UI 有微小卡顿
- TinyUSB 增加 ~30KB 固件大小（OTA 分区 2.5MB 富裕）

### 已完成（曾经的"未来工作"）

- ✅ CDC0 console（2026-05-12）：用自定义 `esp_log_set_vprintf` hook 把 ESP_LOG tee 到 CDC0；`esp_tusb_init_console` 在这块板上静默失败，自己写一个绕开。**注意**：panic / abort backtrace 走 ROM 直接到 UART0，不经 hook，仍看不到
- ✅ REQ_REBOOT_TO_BOOTLOADER（2026-05-12）：协议命令 kind=0x07，AI 全自主烧录无需按键

### 未来工作

- v2：实时屏幕流（流式 kind + 脏区编码 + 浏览器 canvas 渲染器）
- v2.5：录制 / 回放
- adapter Go 端集成 `adapter device monitor` 子命令，替代当前的 Python 脚本（Python 是 PoC，长期归 adapter）
- 把 macOS/SDL 模拟器（`make sim-run`）也实现同一协议，让上位机工具同时支持真机和模拟器
- panic backtrace 也走 CDC0（需要在 panic handler 里直接调 tinyusb_cdcacm_write 而非通过 vprintf）

## 版本演进（debug 历史）

| 版本 | 日期 | 状态 | 备注 |
|---|---|---|---|
| v1 | 2026-05-11 | 提议 | 假设 TinyUSB 双 CDC，但旧面包板 GPIO19/20 未拉出 → 不可行 |
| v2 | 2026-05-11 | 临时实施 | UART1 GPIO 38/39 + 外置 USB-UART 模块，跑通 echo / screenshot / key inject |
| v3 | 2026-05-12 | **最终** | 切到生产 PCB（CH334F Hub 拉出 native USB），回到 TinyUSB 双 CDC。代码全保留协议层，只换底层 transport |

## 触发何时撤销

- 如果 TinyUSB CDC 在 production 设备上长期不稳，可降级到 ADR-015 v2（UART1 + 外置模块），代码协议层完全复用
- 如果开发者实际工作中更习惯 `make sim-run` 模拟器迭代，真机截图需求低 → 砍掉本功能，把精力投到改善模拟器保真度
