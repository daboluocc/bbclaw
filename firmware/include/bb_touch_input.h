#pragma once

#include "esp_err.h"

/**
 * 触摸输入（手表 FT3168 等 FT5x06 兼容电容触控）→ 手势映射为导航事件注入
 * bb_nav_input_inject（复用现有按键导航语义，UI 层零改动）：
 *   点按          → OK
 *   上滑 / 下滑   → DOWN / UP（内容跟手：上滑=看下面=选中下移）
 *   右滑 或 长按  → BACK
 *
 * 板级未启用（BBCLAW_TOUCH_FT5X06_ENABLE=0）时为 no-op。
 * 依赖共享 I2C 总线（bb_audio_shared_i2c_bus），须在 bb_audio_init 之后调用。
 */
esp_err_t bb_touch_input_init(void);
