# ADR-040: 支持 Waveshare 零售开发板作为 BBClaw 拓展板

- 状态：已接受（2026-07-04）
- 关联：[AMOLED-2.06 硬件参考](../boards/waveshare-esp32-s3-touch-amoled-2.06.md)、
  [LCD-1.85 硬件参考](../boards/waveshare-esp32-s3-lcd-1.85.md)、ADR-021（固件 UI）

## 背景

已采购两块 Waveshare ESP32-S3 零售开发板，作为 BBClaw 固件的「拓展板」硬件载体：

1. **ESP32-S3-Touch-AMOLED-2.06** — 2.06" AMOLED 410×502（CO5300, QSPI）+
   FT3168 触摸 + ES8311 codec + AXP2101 PMIC，Octal PSRAM 8MB。
2. **ESP32-S3-LCD-1.85（无触摸）** — 1.85" LCD 360×360（ST77916, QSPI）+
   PCM5101/ICS-43434 纯 I2S 音频（TX/RX 分离引脚）+ TCA9554 扩展器。

两块都是 ESP32-S3R8，与现有固件同芯片系；差异集中在显示总线（QSPI vs SPI/i80）、
音频拓扑和电源管理。现有板级机制（`boards/<name>/board_config.h` + Kconfig choice +
`bb_config.h` 默认值回退）可以承载，但驱动栈有缺口。

## 决策

1. **沿用现有板级模型**，每板一个 `firmware/boards/<name>/`：
   - `waveshare-amoled-206`（本 ADR 先落地）
   - `waveshare-lcd-185`（后续）
2. **适配顺序**：先 AMOLED-2.06，跑通后再 LCD-1.85。
3. **显示**：`bb_panel.c` 新增第三种总线 `BBCLAW_DISPLAY_BUS_QSPI`，面板驱动用
   ESP Registry 组件 `waveshare/esp_lcd_sh8601`（CO5300）。LCD-1.85 复用同一
   QSPI 路径、换 `esp_lcd_st77916`。AMOLED 亮度走面板 `0x51` 命令（无背光 GPIO），
   LVGL 侧注册 2px 对齐 rounder（CO5300 硬性要求）。
4. **音频**：AMOLED-2.06 直接启用 `bb_audio` 既有 **ES8311** duplex 路径
   （代码已在、首次真机使用）。LCD-1.85 的 TX/RX 分离 I2S 是后续适配时的
   `bb_audio` 扩展点（S3 有两个 I2S 控制器）。
5. **输入分期**：
   - 第一阶段：BOOT 键（GPIO0）作 PTT，导航禁用——先打通「按住说话」主链路。
   - 第二阶段：FT3168 触摸接 LVGL indev（固件目前零触摸代码，greenfield）。
6. **电源分期**：AXP2101 免初始化即可点屏/出声（BSP 验证），第一阶段不写 PMU
   驱动、`bb_power` 禁用（ADC 分压不存在）；电量/充电状态/电源键属第二阶段。
7. **逐板 flash/PSRAM 配置**：`scripts/set_board.py` 从「只翻 Kconfig board choice」
   扩展为「翻 choice + 把 `boards/<name>/sdkconfig.board` 覆盖进 `sdkconfig`」，
   使 OCT/QUAD PSRAM、flash 大小、分区表等每板差异随 `make set-board` 一次切齐。
   这补上了多板支持最大的结构缺口（此前全局 `sdkconfig.defaults` 写死 8MB+QUAD）。
8. **发布不涉及**：拓展板固件不进 OTA 发布链路（`sdkconfig.bbclaw.latest` 仍是
   唯一发布配置），拓展板仅本地开发/体验用，不打 tag。

## 后果

- `bb_panel.c` 出现第三个 init 分支；QSPI 相关宏进入 `bb_config.h` 默认值层。
- ES8311 路径从「有代码没人用」变为「有真机在用」，问题会开始暴露（好事）。
- UI 在 410×502 竖屏上的布局适配是独立后续工作（ADR-021 的 UI 以 320×172 横屏
  为基准设计）。
- 触摸、AXP2101、QMI8658、PCF85063、SD 卡都是后续增量，不阻塞第一阶段。
