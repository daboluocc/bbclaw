#pragma once

#include "esp_err.h"

/**
 * BBClaw 状态灯 — state-driven 实现
 *
 * 设计：见 design/firmware_status_led.md
 *
 * 语义：LED 任务每 30ms 从 bb_state_get() 和 bb_power_get_state() 读快照，
 *       按优先级表合成颜色和动画。业务层不再主动 set_status，只在需要"瞬时
 *       提示"时调 bb_led_pulse()。
 *
 * 初始化顺序：bb_state_init() → bb_power_init() → bb_led_init()
 */

typedef enum {
  BB_LED_PULSE_SUCCESS = 0,   /* 绿色 200ms 常亮：turn 正常结束 */
  BB_LED_PULSE_ERROR,         /* 红色快闪 600ms：事件级错误 */
  BB_LED_PULSE_CELEBRATE,     /* 绿色 1s 常亮：快速响应（<5s），退化为 SUCCESS 色 */
  BB_LED_PULSE_NOTIFY,        /* 橙色 400ms 常亮：新通知 */
  BB_LED_PULSE__COUNT,
} bb_led_pulse_t;

/** 启动 LED 任务并注册 bb_state listener。必须在 bb_state_init() 之后调用。 */
esp_err_t bb_led_init(void);

/** 触发一次瞬时提示 overlay；期间覆盖状态合成出的基态。任意线程安全。 */
esp_err_t bb_led_pulse(bb_led_pulse_t kind);
