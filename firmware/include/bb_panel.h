/**
 * bb_panel.h — Abstract LCD panel initialisation (SPI or i80).
 *
 * Both bb_lvgl_display.c and bb_display_bitmap.c call bb_panel_init()
 * instead of hard-coding SPI bus setup.
 */
#pragma once

#include "esp_err.h"
#include "esp_lcd_panel_io.h"
#include "esp_lcd_types.h"

/**
 * Initialise the LCD panel IO and panel handle according to the active board
 * config (BBCLAW_DISPLAY_BUS_SPI or BBCLAW_DISPLAY_BUS_I80).
 *
 * On success *panel_io and *panel are ready for esp_lcd_panel_draw_bitmap().
 */
esp_err_t bb_panel_init(esp_lcd_panel_io_handle_t *panel_io,
                        esp_lcd_panel_handle_t *panel);
