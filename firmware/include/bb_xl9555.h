/**
 * bb_xl9555.h — Minimal XL9555 I2C IO expander driver.
 */
#pragma once

#include "esp_err.h"

/** Initialise XL9555 on the shared I2C bus. Call after I2C master is up. */
esp_err_t bb_xl9555_init(void);

/** Set a single output bit (0-15). */
esp_err_t bb_xl9555_set_output(uint8_t bit, uint8_t level);
