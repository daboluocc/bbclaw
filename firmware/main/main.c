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

  // ADR-046: Power management (IMU + display brightness + sleep manager)
  // ⚠️ 暂禁(2026-07-05 深夜实锤):息屏管理的 100ms FreeRTOS 定时器回调在
  // Tmr Svc 小栈里跑状态机+QSPI 亮度写,打坏定时器任务(panic_pc=uxListRemove
  // StoreProhibited task='Tmr Svc',全晚随机崩根因)。该分支从未真机验证,
  // 需专项调试(回调瘦身/挪自有任务/栈账)后再启用。
  // bb_power_mgmt_init();

  ESP_ERROR_CHECK(bb_radio_app_start());
}
