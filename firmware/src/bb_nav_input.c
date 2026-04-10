#include "bb_nav_input.h"

#include <stdint.h>

#include "bb_config.h"
#include "driver/gpio.h"
#include "esp_log.h"
#include "esp_timer.h"

static const char* TAG = "bb_nav_input";

#if BBCLAW_NAV_ENABLE && (BBCLAW_NAV_ENC_A_GPIO >= 0) && (BBCLAW_NAV_ENC_B_GPIO >= 0) && (BBCLAW_NAV_KEY_GPIO >= 0)
static bb_nav_input_callback_t s_callback;
static esp_timer_handle_t s_timer;
static uint8_t s_last_ab;
static int8_t s_step_accum;
static int s_key_raw;
static int s_key_stable;
static int s_key_stable_count;
static int64_t s_key_press_start_ms;
static int s_long_press_sent;

static uint8_t read_ab_state(void) {
  uint8_t a = gpio_get_level(BBCLAW_NAV_ENC_A_GPIO) ? 1U : 0U;
  uint8_t b = gpio_get_level(BBCLAW_NAV_ENC_B_GPIO) ? 1U : 0U;
  return (uint8_t)((a << 1) | b);
}

static int read_key_pressed(void) {
  int level = gpio_get_level(BBCLAW_NAV_KEY_GPIO);
  return level == BBCLAW_NAV_KEY_ACTIVE_LEVEL ? 1 : 0;
}

static void emit_event(bb_nav_event_t event) {
  if (s_callback != NULL) {
    s_callback(event);
  }
}

static void nav_poll_cb(void* arg) {
  (void)arg;
  static const int8_t kQuadTable[16] = {
      0, -1, 1, 0,
      1, 0, 0, -1,
      -1, 0, 0, 1,
      0, 1, -1, 0,
  };

  uint8_t ab = read_ab_state();
  if (ab != s_last_ab) {
    int8_t delta = kQuadTable[((int)s_last_ab << 2) | ab];
    if (delta != 0) {
      s_step_accum += delta;
      if (s_step_accum >= 4) {
        s_step_accum = 0;
        emit_event(BB_NAV_EVENT_ROTATE_CW);
      } else if (s_step_accum <= -4) {
        s_step_accum = 0;
        emit_event(BB_NAV_EVENT_ROTATE_CCW);
      }
    }
    s_last_ab = ab;
  }

  int key_raw = read_key_pressed();
  if (key_raw == s_key_raw) {
    s_key_stable_count++;
  } else {
    s_key_raw = key_raw;
    s_key_stable_count = 0;
  }

  const int debounce_samples =
      (BBCLAW_NAV_KEY_DEBOUNCE_MS + BBCLAW_NAV_POLL_MS - 1) / BBCLAW_NAV_POLL_MS;
  if (s_key_stable_count >= (debounce_samples > 0 ? debounce_samples : 1) && key_raw != s_key_stable) {
    s_key_stable = key_raw;
    if (s_key_stable) {
      s_key_press_start_ms = esp_timer_get_time() / 1000;
      s_long_press_sent = 0;
    } else if (!s_long_press_sent) {
      emit_event(BB_NAV_EVENT_CLICK);
    }
  }

  if (s_key_stable && !s_long_press_sent) {
    int64_t held_ms = (esp_timer_get_time() / 1000) - s_key_press_start_ms;
    if (held_ms >= BBCLAW_NAV_LONG_PRESS_MS) {
      s_long_press_sent = 1;
      emit_event(BB_NAV_EVENT_LONG_PRESS);
    }
  }
}
#endif

esp_err_t bb_nav_input_init(bb_nav_input_callback_t callback) {
#if BBCLAW_NAV_ENABLE && (BBCLAW_NAV_ENC_A_GPIO >= 0) && (BBCLAW_NAV_ENC_B_GPIO >= 0) && (BBCLAW_NAV_KEY_GPIO >= 0)
  s_callback = callback;

  gpio_config_t io_conf = {
      .pin_bit_mask =
          (1ULL << BBCLAW_NAV_ENC_A_GPIO) | (1ULL << BBCLAW_NAV_ENC_B_GPIO) | (1ULL << BBCLAW_NAV_KEY_GPIO),
      .mode = GPIO_MODE_INPUT,
#if BBCLAW_NAV_PULL_UP
      .pull_up_en = GPIO_PULLUP_ENABLE,
      .pull_down_en = GPIO_PULLDOWN_DISABLE,
#else
      .pull_up_en = GPIO_PULLUP_DISABLE,
      .pull_down_en = GPIO_PULLDOWN_ENABLE,
#endif
      .intr_type = GPIO_INTR_DISABLE,
  };
  ESP_ERROR_CHECK(gpio_config(&io_conf));

  s_last_ab = read_ab_state();
  s_step_accum = 0;
  s_key_raw = read_key_pressed();
  s_key_stable = s_key_raw;
  s_key_stable_count = 0;
  s_key_press_start_ms = 0;
  s_long_press_sent = 0;

  const esp_timer_create_args_t timer_args = {
      .callback = nav_poll_cb,
      .name = "bb_nav_poll",
  };
  ESP_ERROR_CHECK(esp_timer_create(&timer_args, &s_timer));
  ESP_ERROR_CHECK(esp_timer_start_periodic(s_timer, BBCLAW_NAV_POLL_MS * 1000));

  ESP_LOGI(TAG,
           "nav init a=%d b=%d key=%d pull=%s key_active=%d poll_ms=%d debounce_ms=%d long_ms=%d",
           BBCLAW_NAV_ENC_A_GPIO, BBCLAW_NAV_ENC_B_GPIO, BBCLAW_NAV_KEY_GPIO, BBCLAW_NAV_PULL_UP ? "up" : "down",
           BBCLAW_NAV_KEY_ACTIVE_LEVEL, BBCLAW_NAV_POLL_MS, BBCLAW_NAV_KEY_DEBOUNCE_MS, BBCLAW_NAV_LONG_PRESS_MS);
  return ESP_OK;
#else
  (void)callback;
  ESP_LOGI(TAG, "nav disabled");
  return ESP_OK;
#endif
}
