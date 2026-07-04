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

## UI：410×502 竖屏 + 圆角安全区规格（2026-07-05 增补）

> 布局 token 定义见 UI_DESIGN_LANGUAGE.md §2.1；代码真相源 `firmware/include/bb_ui_layout.h`。
> 全量适配点清单（P0/P1/P2 + file:line）由 6 路代码勘察产出，存档于本次适配的
> commit message 与 PR 说明；此处只记决策。

1. **安全区机制**：板级宏 `BBCLAW_DISPLAY_CORNER_RADIUS`（手表 60px 设防值，
   待实机角尺标定校准）→ 推导 `BB_UI_CORNER_INSET ≈ 0.293r` → 页面 root padding
   一次接入（helper），不逐坐标修补。两档语义：全宽贴边条两端收 CORNER_INSET，
   居中内容天然安全。
2. **竖屏形态**：
   - 聊天页三段式：顶栏 ~48px（字标/时钟簇内缩离角）→ transcript 弹性主体
     → 底部状态点阵带（列数由可用宽推导）。录音视图 flex 垂直居中，消灭空洞。
   - 待机表盘：点阵时钟为 hero，放大居中偏上（~40% 高度）；提醒徽标移至时钟
     下方居中；wordmark/版本/电池收成底部居中簇。
   - 两栏页（locked/apconfig）横版「图左文右」改竖排居中（图上文下），
     一次性消除左缘圆角风险。
   - 列表类（settings/task list/picker）行高升到 48-64px（顺势成为触摸目标），
     可见行数运行时由可用高度推导。
3. **字号**：手表 PPI ≈ 旧屏 1.6×，16px CJK 物理不可读。目标 24px 正文 CJK +
   20px 次级；**受当前 2.5MB app 分区约束（余量 ~220KB）分两步走**：先启用
   Montserrat 20/22（ASCII，代价小），CJK 24px 待手表专用分区表（16MB flash
   可放大 app 槽）落地后再生成。手表不进 OTA 发布链，分区表可独立于 bbclaw。
4. **主题**：仅适配 buddy-anim（在售默认）；buddy-ascii / text-only 两个 legacy
   主题暂不适配也不注销（保持注册，其它板不受影响），手表上如被 NVS 选中按
   现状显示——是否全局注销留待产品决策。
5. **触摸输入**：不做 LVGL 指针 indev；FT3168 手势翻译成既有导航事件注入
   （tap=OK / 上滑=DOWN / 下滑=UP / 右滑或长按=BACK），所有页面零改动即可
   触摸操作（`bb_touch_input.c`）。

## 后果

- `bb_panel.c` 出现第三个 init 分支；QSPI 相关宏进入 `bb_config.h` 默认值层。
- ES8311 路径从「有代码没人用」变为「有真机在用」，问题会开始暴露（好事）。
- UI 在 410×502 竖屏上的布局适配是独立后续工作（ADR-021 的 UI 以 320×172 横屏
  为基准设计）。
- 触摸、AXP2101、QMI8658、PCF85063、SD 卡都是后续增量，不阻塞第一阶段。
