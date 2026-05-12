#pragma once

#include "esp_err.h"
#include "sdkconfig.h"

#ifdef __cplusplus
extern "C" {
#endif

/* Device Monitor — host-driven USB debug channel.
 *
 * Implements ADR-015: a binary frame protocol over a dedicated USB CDC
 * interface, used by the bbclaw-adapter host tool to (1) capture LVGL
 * screenshots on demand and (2) inject navigation key events as if from a
 * real button press.
 *
 * The whole subsystem is compiled out when CONFIG_BBCLAW_DEVICE_MONITOR=n;
 * the init function below becomes a no-op stub so callers can stay
 * #ifdef-free.
 */

#ifdef CONFIG_BBCLAW_DEVICE_MONITOR

/* Initialize the device monitor task and USB CDC interface.
 *
 * Must be called once during boot, AFTER nvs_flash_init() and BEFORE any
 * subsystem that takes the LVGL lock for prolonged stretches (so the
 * monitor task can preempt them to service screenshot requests).
 *
 * Returns ESP_OK on success. On failure the function logs the cause and
 * returns the underlying error; boot should continue regardless since the
 * monitor is a developer convenience, not a critical service.
 */
esp_err_t bb_device_monitor_init(void);

#else  /* !CONFIG_BBCLAW_DEVICE_MONITOR */

static inline esp_err_t bb_device_monitor_init(void) { return ESP_OK; }

#endif /* CONFIG_BBCLAW_DEVICE_MONITOR */

#ifdef __cplusplus
}
#endif
