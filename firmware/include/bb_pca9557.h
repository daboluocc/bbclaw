/**
 * bb_pca9557.h — Minimal PCA9557 I2C IO expander driver（实战派板：LCD_CS + PA_EN）.
 */
#pragma once

#include <esp_err.h>
#include <stdint.h>

/** Idempotent. Configures LCD_CS/PA_EN bits as outputs, drives LCD_CS low
 *  (panel permanently selected) and PA_EN low (amp off until audio is up). */
esp_err_t bb_pca9557_init(void);
esp_err_t bb_pca9557_set_output(uint8_t bit, uint8_t level);
