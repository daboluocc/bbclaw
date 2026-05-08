# Hardware Errata (PCB 勘误)

记录当前 PCB 批次上发现的硬件问题，以及下一版打板时应当如何修正。
固件侧已有绕行方案的情况下，本版可直接使用；下版 PCB 按本表修线。

---

## ERR-001 — WS-003 拨轮 PUSH 触点接线错误

- **发现日期**: 2026-05-09
- **影响板**: bbclaw v1 (2026-04 批次)
- **症状**: 按下拨轮中心 PUSH 键无任何响应。GPIO1 电平持续保持 1，
  固件侧诊断日志（`bb_nav_input: snapshot key(gpio1)=...`）按压 15 秒无跳变。
  拨轮左右方向 (GPIO6/GPIO8) 工作正常。
- **根因**:
  PCB footprint 把 WS-003 的 **外壳固定脚** 当成了 PUSH 信号脚，
  接到了 GPIO1；而 PUSH 真正的信号触点 `T`（见规格书 §触点图
  `PUSH: C─o─T`）落在了被 PCB 标记为 "GND" 的焊盘之一，两端都被
  错误地接到了 GND，按下去两端都是 GND，GPIO 永远检测不到变化。
- **验证**: 万用表蜂鸣档测得：
  - PCB 走线 GPIO1 → ESP32 GPIO1 ✓ 通
  - 按 PUSH 时 焊盘 5 ↔ GND ✗ 不通（5 号是外壳）
  - 按 PUSH 时 焊盘 1 ↔ GND / 焊盘 4 ↔ GND ✗ 都不通
  - 结论：PUSH 触点落在当前标 GND 的 2/3 号焊盘中（需拆件物理确认）

### WS-003 正确接线表

| 规格书脚 | 作用 | 应接到 |
|---------|------|-------|
| `1` | CW 方向动触点 | GPIO (上拉输入) |
| `2` | CCW 方向动触点 | GPIO (上拉输入) |
| `C` | 方向公共端（同时也是 PUSH 一端）| **GND** |
| `T` | PUSH 的另一端 | **GPIO (上拉输入)** |
| `D` / 外壳固定脚 | 机械固定 | GND 或悬空（**不接信号**）|

### 下版 PCB 修正

1. 焊盘 1 → GPIO6（保持，`BBCLAW_NAV_ENC_B_GPIO`，DOWN 方向）
2. 焊盘 4 → GPIO8（保持，`BBCLAW_NAV_ENC_A_GPIO`，UP 方向）
3. 焊盘 2（或 3，以拆件物理确认的 T 端为准）→ **GPIO1**
4. 焊盘 2/3 中另一个（`C` 端）→ GND
5. 外壳所有固定脚 → GND（不当信号）
6. 重新确认 footprint：建议换用 datasheet 明确标注 T/C/1/2 触点位置的
   型号（或在库里用带丝印脚号的 3D 模型）

### 本版绕行方案（已生效）

固件切到 Flipper 6-button 模式，用 PCB 上另外两个独立按键顶替：

```c
// boards/bbclaw/board_config.h
#define BBCLAW_NAV_FLIPPER_6BUTTON   1
#define BBCLAW_NAV_BTN_UP_GPIO       8    // 拨轮 UP 方向
#define BBCLAW_NAV_BTN_DOWN_GPIO     6    // 拨轮 DOWN 方向
#define BBCLAW_NAV_BTN_OK_GPIO      42    // KEY1 → OK
#define BBCLAW_NAV_BTN_BACK_GPIO    41    // KEY2 → BACK
```

附带好处：Flipper 模式的 UP/DOWN 支持按住自动连发
（`BBCLAW_NAV_REPEAT_INITIAL_MS=400` / `INTERVAL_MS=80`），
拨到头住不动会持续滚动。

下版 PCB 修好 PUSH 后，可保留 Flipper 模式（KEY1/KEY2 继续作 OK/BACK，
PUSH 成为冗余），或切回 `BUTTONS_INSTEAD_OF_ENC` + 单 KEY，由设计决定。
