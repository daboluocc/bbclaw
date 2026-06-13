#include "bb_ptt.h"

#include <stddef.h>

#include "bb_config.h"
#include "driver/gpio.h"
#include "esp_log.h"
#include "esp_timer.h"

static const char* TAG = "bb_ptt";

/* Consecutive stable samples needed to confirm a level change. Poll runs every
 * BBCLAW_PTT_POLL_MS; the debounce window is BBCLAW_PTT_DEBOUNCE_MS. Clamp to
 * ≥2 so a single stray sample never flips state. */
#define PTT_STABLE_SAMPLES                                 \
  (((BBCLAW_PTT_DEBOUNCE_MS) / (BBCLAW_PTT_POLL_MS)) >= 2  \
       ? ((BBCLAW_PTT_DEBOUNCE_MS) / (BBCLAW_PTT_POLL_MS)) \
       : 2)

static int s_gpio_num;
static int s_pressed;
static bb_ptt_callback_t s_callback;
static esp_timer_handle_t s_timer;
static int s_last_raw;
static int s_stable_count;
static int s_inject_hold; /* while set, poll ignores the real GPIO (bb_ptt_inject) */

static int is_pressed_raw(void) {
  int level = gpio_get_level(s_gpio_num);
  return level == BBCLAW_PTT_ACTIVE_LEVEL ? 1 : 0;
}

static void ptt_poll_cb(void* arg) {
  (void)arg;
  if (s_inject_hold) {
    return; /* a host-injected hold owns the state; don't let the real pin fight it */
  }
  int raw = is_pressed_raw();
  if (raw == s_last_raw) {
    s_stable_count++;
  } else {
    s_last_raw = raw;
    s_stable_count = 0;
  }

  if (s_stable_count >= PTT_STABLE_SAMPLES && raw != s_pressed) {
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
  ESP_ERROR_CHECK(esp_timer_start_periodic(s_timer, BBCLAW_PTT_POLL_MS * 1000));

  ESP_LOGI(TAG, "ptt init gpio=%d active_level=%d pull=%s debounce_ms=%d poll_ms=%d samples=%d", s_gpio_num,
           BBCLAW_PTT_ACTIVE_LEVEL, BBCLAW_PTT_PULL_UP ? "up" : "down", BBCLAW_PTT_DEBOUNCE_MS, BBCLAW_PTT_POLL_MS,
           PTT_STABLE_SAMPLES);
  return ESP_OK;
}

int bb_ptt_is_pressed(void) {
  return s_pressed;
}

void bb_ptt_inject(int pressed) {
  pressed = pressed ? 1 : 0;
  if (pressed) {
    /* Take ownership: the poll task stops reading the real pin until release,
     * so an injected hold lasts as long as the host keeps it down. */
    s_inject_hold = 1;
  } else {
    /* Release: hand control back to the GPIO, seeded to the current physical
     * level so polling resumes without a spurious edge. */
    s_inject_hold = 0;
    s_last_raw = is_pressed_raw();
    s_stable_count = 0;
  }
  if (pressed != s_pressed) {
    s_pressed = pressed;
    ESP_LOGI(TAG, "ptt injected pressed=%d", s_pressed);
    if (s_callback != NULL) {
      s_callback(s_pressed);
    }
  }
}
