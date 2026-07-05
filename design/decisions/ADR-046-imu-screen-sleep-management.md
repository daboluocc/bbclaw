# ADR-046：IMU 息屏管理与抬手唤醒

**状态**: Proposed  
**作者**: BBClaw 团队  
**日期**: 2026-07-05  
**相关**: ADR-040（Waveshare 拓展板）, 硬件/boards/waveshare-esp32-s3-touch-amoled-2.06.md

---

## 问题

Waveshare ESP32-S3-Touch-AMOLED-2.06 拓展板集成 **QMI8658 六轴 IMU**（加速度计 + 陀螺仪），但目前未被利用。AMOLED 屏幕常亮是功耗主源，尤其在待机/空闲状态。

**需求**：
1. **息屏管理** — 无用户交互一段时间后自动息屏降功耗
2. **抬手唤醒** — 用户抬起/晃动设备时自动亮屏
3. **电池优化** — 显著延长待机时间

---

## 方案

### 1. IMU 驱动集成（第一步）

**硬件**:
- QMI8658 @I2C 地址 0x6B/0x6A（双地址兼容），16-bit 加速度计 + 16-bit 陀螺仪
- 频率: 50Hz ~ 8kHz，取 100Hz（10ms 样本周期）
- 中断: INT1/INT2（板上已接 GPIO，拓展后使用）
- 功耗: 待机 <1mA，工作 ~2.5mA @ 100Hz

**驱动**:
- 创建 `firmware/drivers/qmi8658/` 目录
- 实现基础初始化、数据读取、中断处理
- I2C 接复用 bus（0x18/0x34/0x38/0x51 既有设备同总线）

### 2. 屏幕亮度控制（第二步）

**CO5300 AMOLED 指令**:
- 当前已在 init 中设置 `0x51 0xFF`（满亮）
- 可通过 `esp_lcd_panel_io_tx_param(panel_io, 0x51, brightness_byte, 1)` 动态调节
- 亮度: 0x00 = 息屏，0xFF = 满亮

**亮度曲线**:
- Level 0: 息屏（0x00）
- Level 1-3: 低亮（0x20-0x50）— 通知/时间显示
- Level 4-9: 正常（0x80-0xFF）— 实时交互

### 3. 状态机设计

```
                    ┌─────────────┐
                    │  ACTIVE     │ (屏幕全亮)
                    │  用户交互中 │
                    └──────┬──────┘
                      2min无操作
                           │
                    ┌──────▼──────┐
                    │  DIMMING    │ (亮度20%)
                    │  空闲过度   │
                    └──────┬──────┘
                      1min无操作
                           │
                    ┌──────▼──────┐
                    │  SLEEPING   │ (息屏)
                    │  低功耗待机 │
                    └──────┬──────┘
                       IMU动作
                       (抬手/晃)
                           │
                    ┌──────▼──────┐
                    │  WAKING     │ (亮度40%)
                    │  触发检查中 │
                    └──────┬──────┘
                      用户交互/超时
                           │
                    ┌──────▼──────┘
                    │  ACTIVE
                    │  (循环)
```

### 4. IMU 唤醒逻辑

**检测条件**（任一满足触发唤醒）:

1. **抬手** — 加速度瞬时变化 > 阈值 (e.g., a_total > 1.5g)
2. **晃动** — 连续 3 个 100ms 周期内加速度变化 > 阈值
3. **旋转** — 陀螺仪角速度 > 50°/s

**去抖**:
- 息屏 → 抬手 → 进 WAKING 亮度(40%) → 用户未继续交互 → 2s 后回 SLEEPING
- 防止频繁误触发（如震动、掉落）

### 5. 用户交互重置

事件源:
- **PTT 按下** → 重置为 ACTIVE
- **屏幕触摸** → 重置为 ACTIVE（需启用触摸中断）
- **网络消息到达** → 重置为 ACTIVE（设备标志，由 cloud relay 驱动）

### 6. 配置参数

```c
/* board_config.h */
#define BBCLAW_IMU_ENABLE                1
#define BBCLAW_IMU_QMI8658_I2C_ADDR     0x6B
#define BBCLAW_IMU_SAMPLE_RATE_HZ       100

/* 息屏超时（毫秒） */
#define BBCLAW_SLEEP_DIMMING_DELAY_MS   (2 * 60 * 1000)  /* 2 min */
#define BBCLAW_SLEEP_SLEEP_DELAY_MS     (3 * 60 * 1000)  /* 3 min total */

/* IMU 阈值 */
#define BBCLAW_IMU_ACCEL_THRESHOLD_MG   1500  /* 1.5g */
#define BBCLAW_IMU_GYRO_THRESHOLD_DPS   50    /* 50°/s */
#define BBCLAW_IMU_WAKE_COOLDOWN_MS     2000  /* 去抖延迟 */
```

---

## 实施路线图

### Phase 1: IMU 驱动 (2-3 days)
- [ ] QMI8658 I2C 初始化 & 数据读取
- [ ] 单元测试（UART 输出原始数据验证）
- [ ] 集成到 main 应用

### Phase 2: 屏幕控制 (1-2 days)
- [ ] CO5300 亮度指令接口
- [ ] 亮度 level 映射表
- [ ] 动画过渡（平滑淡出/淡入）

### Phase 3: 息屏状态机 (2-3 days)
- [ ] Timer + 事件驱动状态转换
- [ ] IMU 唤醒检测算法
- [ ] 功耗优化（IMU 低功耗模式）

### Phase 4: 集成与测试 (2-3 days)
- [ ] 真机功耗测试（息屏 vs 常亮）
- [ ] 唤醒灵敏度调优
- [ ] 用户体验微调

---

## 风险 & 缓解

| 风险 | 缓解 |
|------|------|
| IMU 假唤醒多（晃、掉落） | 阈值微调 + 去抖算法 |
| AMOLED 响应缓慢（亮屏延迟） | 预先从 WAKING 即刻转 ACTIVE |
| I2C 总线冲突（触摸/PMIC/codec 争用） | IMU 数据读取独立任务，100Hz 采样线程 |
| 固件体积增长 | QMI8658 驱动代码 ~2KB，接受范围 |

---

## 预期收益

- **待机功耗** — 屏幕占 60%+，息屏可降 80% 以上
- **续航时间** — 400mAh 电池：待机 ~2-3 天（vs 当前 8-12 小时）
- **用户体验** — 完全透明；用户只需抬手即亮，无额外交互

---

## 审批

- **Author**: BBClaw 团队
- **Reviewer**: 
- **Approved**: 

