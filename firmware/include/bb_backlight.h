/**
 * bb_backlight.h — ST7789 背光统一开关（GPIO 电平 或 LEDC PWM，板级宏选择）.
 *
 * 实战派排障结论（2026-07-19）：其背光升压电路需要 PWM 开关信号,恒定 GPIO
 * 电平(0/1 都试过)永远点不亮;命令/参数/像素链路当时全部正常(RDDPM/MADCTL/
 * RAMRD 回读实证),黑屏唯一原因就是背光。故引入 BBCLAW_ST7789_BL_PWM 路径。
 */
#pragma once

#include <esp_err.h>

/** Idempotent-init + set. on=1 点亮（PWM 板= BBCLAW_ST7789_BL_PWM_ON_DUTY 占空）。 */
esp_err_t bb_backlight_set(int on);
