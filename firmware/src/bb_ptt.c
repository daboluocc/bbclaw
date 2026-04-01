#include "bb_ptt.h"

#include <stddef.h>

#include "bb_config.h"
#include "driver/gpio.h"
#include "esp_log.h"
#include "esp_timer.h"

static const char* TAG = "bb_ptt";
static int s_gpio_num;
static int s_pressed;
static bb_ptt_callback_t s_callback;
static esp_timer_handle_t s_timer;
static int s_last_raw;
static int s_stable_count;

static int is_pressed_raw(void) {
  int level = gpio_get_level(s_gpio_num);
  return level == BBCLAW_PTT_ACTIVE_LEVEL ? 1 : 0;
}

static void ptt_poll_cb(void* arg) {
  (void)arg;
  int raw = is_pressed_raw();
  if (raw == s_last_raw) {
    s_stable_count++;
  } else {
    s_last_raw = raw;
    s_stable_count = 0;
  }

  if (s_stable_count >= 2 && raw != s_pressed) {
    s_pressed = raw;
    ESP_LOGI(TAG, "ptt changed pressed=%d", s_pressed);
    if (s_callback != NULL) {
      s_callback(s_pressed);
    }
  }
}

esp_err_t bb_ptt_init(int gpio_num, bb_ptt_callback_t callback) {
  s_gpio_num = gpio_num;
  s_callback = callback;

  gpio_config_t io_conf = {
      .pin_bit_mask = 1ULL << s_gpio_num,
      .mode = GPIO_MODE_INPUT,
#if BBCLAW_PTT_PULL_UP
      .pull_up_en = GPIO_PULLUP_ENABLE,
      .pull_down_en = GPIO_PULLDOWN_DISABLE,
#else
      .pull_up_en = GPIO_PULLUP_DISABLE,
      .pull_down_en = GPIO_PULLDOWN_ENABLE,
#endif
      .intr_type = GPIO_INTR_DISABLE,
  };
  ESP_ERROR_CHECK(gpio_config(&io_conf));

  s_last_raw = is_pressed_raw();
  s_pressed = s_last_raw;
  s_stable_count = 0;

  const esp_timer_create_args_t timer_args = {
      .callback = ptt_poll_cb,
      .name = "bb_ptt_poll",
  };
  ESP_ERROR_CHECK(esp_timer_create(&timer_args, &s_timer));
  ESP_ERROR_CHECK(esp_timer_start_periodic(s_timer, BBCLAW_PTT_DEBOUNCE_MS * 1000));

  ESP_LOGI(TAG, "ptt init gpio=%d active_level=%d pull=%s debounce_ms=%d", s_gpio_num, BBCLAW_PTT_ACTIVE_LEVEL,
           BBCLAW_PTT_PULL_UP ? "up" : "down", BBCLAW_PTT_DEBOUNCE_MS);
  return ESP_OK;
}

int bb_ptt_is_pressed(void) {
  return s_pressed;
}
