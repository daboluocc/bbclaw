# bbclaw v2 需求（从 v1 量产版提取 + 升级项）

> 提取方式：2026-07-16 用 easyeda-agent 读取 EasyEDA 工程「bbclaw 量产版本」
> （V1 / Schematic1_3 / PCB1_2）真实网表（60 器件 / 39 网络），逐网核对
> `design/hardware_errata.md` 后整理。v1 完整网表摘要见附录 A。

## 一句话

bbclaw v2 = v1 全功能保持不变 + **板载单 CH340 USB 自动下载**（免外接串口板、免手按时序）
+ 勘误修正（ERR-001 拨轮）+ 若干小优化。

---

## 必须保持的 v1 功能（复刻基线）

1. **主控**：ESP32-S3-WROOM-1U-N8R8（IPEX 外置天线版）。
2. **显示**：1.47″ ST7789 SPI，8P FPC 座（AFC07-S08ECA）：
   GND / 3V3 / SCL=IO9 / SDA=IO10 / RES=IO11 / DC=IO12 / CS=IO13 / BLK=IO14。
3. **音频输出**：I2S → H2（MX1.25 7P wafer）外接功放+喇叭：
   LRC=IO15 / BCLK=IO16 / DIN=IO17 / SD(静音)=IO4，SD 线上并联 SW2 拨动开关可硬件静音；
   功放供电 +5V。
4. **麦克风**：外接 I2S 麦（与功放共享时钟）：H3（2.54 3P：MICSD=IO18 / 3V3 / GND）
   + H4（2.54 3P：GND / LRC / BCLK）。
5. **PTT**：电容触摸弹簧（GT-TC072A-H060）→ IO7（S3 原生 TOUCH7），无外置触摸 IC。
6. **拨轮**：WS-003：UP=IO8 / DOWN=IO6 / PUSH=IO1 —— **必须按 ERR-001 勘误表修正接线**
   （v1 把外壳固定脚接成了 PUSH 信号，PUSH 完全无效；正确接法：T 触点→IO1、C 触点→GND、
   外壳脚只做机械固定接 GND）。
7. **按键**：KEY1=IO42、KEY2=IO41（各带 100nF 去抖到地）；RESET 按 EN
   （EN 上有 10k 上拉 + 100nF + 1µF RC）。
8. **RGB 指示**：SK6812MINI-HS，3V3 供电，数据 IO5 经串阻。
9. **振动马达**：MX1.25 3P wafer（GND / +5V / 信号=IO21），马达模块自带驱动。
10. **电源链**：
    - USB-C（16P，仅供电）→ ME4054 线性锂充（PROG=1.2k）+ 充电指示 LED（VBUS 侧上拉）；
    - 锂电池经 SH1 焊接端子接入 VBAT；
    - SW1 拨动开关 = 电源总开关（VBAT → VBAT_OUT）；
    - VBAT_OUT → SX1308 升压 → +5V（喇叭/马达）→ XC6210B332 LDO → +3V3（主控/屏/麦/RGB）；
    - 电池采样：VBAT_OUT 分压 → IO3（ADC）。
11. **USB-C CC1/CC2**：各 5.1k 下拉（UFP sink 正确取电姿势，保持）。
12. **工艺件**：TP1–TP4 测试点（VBUS/VBAT/+5V/+3V3）、4× M 螺丝孔、屏蔽罩接地件。

## 升级项（v2 新增/变更）

A. **板载单 CH340 USB 自动下载**（本次核心诉求）：
   - 现状痛点：v1 的 USB-C **数据脚 D± 悬空**，烧录必须拆开外壳接 H1 六针座
     （VBUS/GND/TXD0/RXD0/IO0/EN）+ 外接串口板，且板上无 BOOT 键，进下载模式要手工短接 IO0。
   - 目标：插上 USB-C 即可一键烧录/看日志——USB-C D± → CH340C → UART0（IO43/IO44），
     DTR/RTS 经双三极管自动下载电路 → EN / IO0，无需任何手按。
   - USB 数据线对加 ESD 保护（对外插拔口）。
B. **ERR-001 修正**（见上第 6 条）。
C. **小优化**（S0 方案书逐条摊开给用户拍板，见
   `bbclaw-v2-s0-proposal.md`）：IO0 上拉、电池采样分压功耗、H1 调试座去留、
   BOOT 键要不要、USB ESD/VBUS 防护等。

## 明确不做（v2 范围外）

- 不改音频架构（不上 ES8311 codec——那是极客/开发板方案的走向，量产版保持 I2S 分体）。
- 不动升压+LDO 的电源拓扑（效率优化留给 v3 评估，本版聚焦烧录链路，改动面最小化）。
- 不动结构（外壳/屏/按键布局），PCB 外形与安装孔位保持 v1。

---

## 附录 A：v1 网表摘要（提取证据）

| 网 | 成员（关键） | 判读 |
|---|---|---|
| VBUS | U8.A4/A9/B4/B9, U1.4, R4→LED1, H1.1, TP1 | USB-C 只供电，D± 悬空 |
| VBAT | U1.3, SH1.2, SW1.1, TP2 | 充电输出/电池 |
| VBAT_OUT | SW1.2, U2.4/5, L1, R7, C5 | 开关后系统电 |
| +5V | U2 输出(D1), U64.1/3, H2.7, U3.2, TP3 | 升压轨：功放+马达+LDO 入 |
| +3V3 | U64.5, U7.2, FPC1.2, H3.2, U6.3, R9, TP4 | LDO 轨 |
| EN | U7.3, SW3, R9(10k↑), C10(100n), C16(1µ), H1.6 | 复位 RC 齐全 |
| IO0 | U7.27, H1.5 | **无上拉、无按键**，只出烧录座 |
| TXD0/RXD0 | U7.37/36, H1.3/4 | UART0 只出烧录座 |
| LCD_* | FPC1.3–8 ↔ U7.17–22 | IO9–14 |
| LRC/BCLK/DIN | H2.1/2/3 + H4.2/3, U7.8/9/10 | IO15/16/17，麦与功放共钟 |
| MICSD | H3.1, U7.11 | IO18 |
| SD | H2.5, SW2.1, U7.4 | IO4 + 硬件静音开关 |
| PTT | U5.1, U7.7 | IO7 触摸弹簧 |
| GPIO1/6/8 | U4.3/1/4, U7.39/6/12 | 拨轮（ERR-001 待修） |
| KEY1/KEY2 | SW4/SW5(+100n), U7.35/34 | IO42/41 |
| RGB1 | R8, U6.2, U7.5 | IO5 串阻 |
| MOTOR | U3.3, U7.23 | IO21 |
| POWER_ADC | R6/R7(10k/10k), C9, U7.15 | IO3 采样 |
| CC1/CC2 | R21/R22(5.1k), U8.A5/B5 | UFP ✓ |

器件清单（v1，60 件）：U7 WROOM-1U-N8R8 / U8 USB-C 16P / U1 ME4054 / U2 SX1308+L1+D1 /
U64 XC6210B332 / U4 WS-003 / U5 GT-TC072A / U6 SK6812MINI / FPC1 8P / H1 6P / H2 7P /
H3+H4 2.54-3P / U3 3P / SW1/2 拨动 / SW3/4/5 轻触 / LED1+R4 / R×12 / C×18 / SH1 / TP×4 / SCREW×4。
