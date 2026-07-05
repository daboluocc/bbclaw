/**
 * 显示屏亮度控制通用接口 — ST7789 / AMOLED / LCD 通用
 *
 * 支持不同显示芯片的亮度调节。实现无需改动此接口，仅在 board_config.h
 * 中配置启用相应驱动即可（BBCLAW_DISPLAY_BRIGHTNESS_CONTROL）。
 */
#pragma once

#include <stdint.h>
#include <esp_err.h>

#ifdef __cplusplus
extern "C" {
#endif

/* ── 亮度等级 ── */

#define BB_BRIGHTNESS_OFF          0    /* 0%   屏幕关闭 */
#define BB_BRIGHTNESS_MIN          1    /* 5%   最低可见 */
#define BB_BRIGHTNESS_LOW          2    /* 20%  低亮度 */
#define BB_BRIGHTNESS_MID          5    /* 50%  中等亮度 */
#define BB_BRIGHTNESS_HIGH         8    /* 80%  高亮度 */
#define BB_BRIGHTNESS_MAX          10   /* 100% 最高亮度 */

/* 原始亮度值（0-255，仅内部使用） */
#define BB_BRIGHTNESS_RAW_MIN      0x00
#define BB_BRIGHTNESS_RAW_MAX      0xFF

/* ── 亮度控制接口 ── */

/**
 * 初始化显示亮度控制驱动。
 *
 * @return ESP_OK 成功；ESP_ERR_NOT_SUPPORTED 硬件不支持亮度调节
 */
esp_err_t bb_display_brightness_init(void);

/**
 * 反初始化亮度控制。
 *
 * @return ESP_OK 成功
 */
esp_err_t bb_display_brightness_deinit(void);

/**
 * 检查亮度控制是否可用。
 *
 * @return true 支持，false 不支持或未初始化
 */
int bb_display_brightness_is_available(void);

/**
 * 设置亮度等级（0-10，易于使用）。
 *
 * @param level 亮度等级 (0=OFF, 10=MAX)
 * @return ESP_OK 成功，ESP_ERR_INVALID_ARG 等级无效
 */
esp_err_t bb_display_set_brightness_level(uint8_t level);

/**
 * 获取当前亮度等级。
 *
 * @return 亮度等级 (0-10)
 */
uint8_t bb_display_get_brightness_level(void);

/**
 * 设置原始亮度值（0-255，精细控制）。
 *
 * @param value 亮度值 (0=OFF, 255=MAX)
 * @return ESP_OK 成功，ESP_ERR_INVALID_ARG 值无效
 */
esp_err_t bb_display_set_brightness_raw(uint8_t value);

/**
 * 获取当前原始亮度值。
 *
 * @return 亮度值 (0-255)
 */
uint8_t bb_display_get_brightness_raw(void);

/**
 * 平滑过渡亮度（渐进调节，避免闪烁）。
 *
 * @param target_level 目标亮度等级
 * @param duration_ms 过渡时长（毫秒），0 表示立即切换
 * @return ESP_OK 成功
 */
esp_err_t bb_display_fade_brightness(uint8_t target_level, uint16_t duration_ms);

/**
 * 停止正在进行的亮度过渡。
 *
 * @return ESP_OK 成功
 */
esp_err_t bb_display_stop_fade(void);

/**
 * 检查是否正在进行亮度过渡。
 *
 * @return true 正在过渡，false 已完成或未开始
 */
int bb_display_is_fading(void);

/* ── 便利宏 ── */

#define bb_display_turn_on()       bb_display_set_brightness_level(BB_BRIGHTNESS_MAX)
#define bb_display_turn_off()      bb_display_set_brightness_level(BB_BRIGHTNESS_OFF)
#define bb_display_dim(pct)        bb_display_set_brightness_raw((uint8_t)((pct) * 255 / 100))

#ifdef __cplusplus
}
#endif

