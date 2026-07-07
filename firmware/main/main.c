#include "esp_err.h"
#include "esp_log.h"
#include "nvs_flash.h"

#include "bb_chat_cache.h"
#include "bb_device_monitor.h"
#include "bb_identity.h"
#include "bb_ota.h"
#include "bb_radio_app.h"
#include "bb_session_store.h"
#include "bb_power_mgmt.h"

static const char* TAG = "bbclaw_main";

void app_main(void) {
  esp_err_t err = nvs_flash_init();
  if (err == ESP_ERR_NVS_NO_FREE_PAGES || err == ESP_ERR_NVS_NEW_VERSION_FOUND) {
    ESP_ERROR_CHECK(nvs_flash_erase());
    err = nvs_flash_init();
  }
  ESP_ERROR_CHECK(err);

  ESP_LOGI(TAG, "starting BBClaw firmware bootstrap");
  bbclaw_identity_init();

  /* ADR-014 Phase B: migrate legacy NVS session keys on first boot
   * after OTA from v0.4.x. Must run on internal-RAM stack before any
   * session_store_load calls. */
  bb_session_store_migrate();

  /* ADR-017: chat tail cache (in-RAM ring + NVS blob per driver). Init
   * before the chat overlay's first show, which hydrates from NVS. */
  bb_chat_cache_init();

  // Initialize OTA
  bb_ota_init();

  // ADR-015: device monitor (USB screenshot + key injection for dev tools).
  // No-op stub when CONFIG_BBCLAW_DEVICE_MONITOR=n.
  bb_device_monitor_init();

  /* ADR-046: Power management (IMU + 亮度 + 息屏)。2026-07-08 专项修复后
   * 重新启用:①定时器回调(Tmr Svc 小栈)废除,状态机改 stream_task 内 tick;
   * ②IMU/触摸/消息事件旗标化,转换单一上下文;③亮度 QSPI 写与 LVGL 刷屏
   * 互斥(lvgl_port_lock);④渐变任务代数化优雅退出,禁外部 vTaskDelete。 */
  bb_power_mgmt_init();

  ESP_ERROR_CHECK(bb_radio_app_start());
}
