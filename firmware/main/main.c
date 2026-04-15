#include "esp_err.h"
#include "esp_log.h"
#include "nvs_flash.h"

#include "bb_identity.h"
#include "bb_ota.h"
#include "bb_radio_app.h"

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

  // Initialize OTA
  bb_ota_init();

  ESP_ERROR_CHECK(bb_radio_app_start());
}
