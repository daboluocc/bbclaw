#include "bb_button_test.h"

#include "bb_config.h"
#include "driver/gpio.h"
#include "esp_heap_caps.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

#if BBCLAW_BUTTON_TEST_GPIO >= 0

static const char* TAG = "bb_button_test";

static void button_test_task(void* arg) {
  (void)arg;
  const int gpio = BBCLAW_BUTTON_TEST_GPIO;
  int last = gpio_get_level(gpio);
  ESP_LOGI(TAG, "gpio%d 初始 raw=%d（未按时参考；按下后看 edge）", gpio, last);

  uint32_t ticks_since_edge = 0;
  for (;;) {
    int level = gpio_get_level(gpio);
    if (level != last) {
      ESP_LOGI(TAG, "gpio%d edge %d→%d（按下后电平=%d → 作 PTT 时 BBCLAW_PTT_ACTIVE_LEVEL 填该值）", gpio, last, level,
               level);
      last = level;
      ticks_since_edge = 0;
    } else {
      ticks_since_edge++;
      if (ticks_since_edge >= 40) {
        ESP_LOGI(TAG, "gpio%d stable raw=%d", gpio, level);
        ticks_since_edge = 0;
      }
    }
    vTaskDelay(pdMS_TO_TICKS(BBCLAW_BUTTON_TEST_INTERVAL_MS));
  }
}

esp_err_t bb_button_test_start(void) {
  const int gpio = BBCLAW_BUTTON_TEST_GPIO;

  gpio_config_t io_conf = {
      .pin_bit_mask = 1ULL << (unsigned)gpio,
      .mode = GPIO_MODE_INPUT,
#if BBCLAW_BUTTON_TEST_PULL_UP
      .pull_up_en = GPIO_PULLUP_ENABLE,
      .pull_down_en = GPIO_PULLDOWN_DISABLE,
#else
      .pull_up_en = GPIO_PULLUP_DISABLE,
      .pull_down_en = GPIO_PULLDOWN_ENABLE,
#endif
      .intr_type = GPIO_INTR_DISABLE,
  };
  esp_err_t err = gpio_config(&io_conf);
  if (err != ESP_OK) {
    ESP_LOGE(TAG, "gpio_config %d failed: %s", gpio, esp_err_to_name(err));
    return err;
  }

  ESP_LOGI(TAG, "button test gpio=%d pull=%s interval_ms=%d（与 PTT 独立）", gpio,
           BBCLAW_BUTTON_TEST_PULL_UP ? "up" : "down", BBCLAW_BUTTON_TEST_INTERVAL_MS);

  /* PSRAM 栈：内部 RAM 零占用——别把内部堆最大连续块挤到 adapter ws 任务
   * 所需的 8192B 以下（对齐 bb_uart_cmd 的 7899e19 修复，避免 dev 自测任务
   * 偷走内部 RAM 导致 PTT 录音流的 WebSocket 建不起来）。 */
  BaseType_t ok = xTaskCreateWithCaps(button_test_task, "bb_button_test", 3072, NULL, 3, NULL, MALLOC_CAP_SPIRAM);
  return ok == pdPASS ? ESP_OK : ESP_ERR_NO_MEM;
}

#else

esp_err_t bb_button_test_start(void) { return ESP_OK; }

#endif
