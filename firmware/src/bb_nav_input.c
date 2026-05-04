#include "bb_nav_input.h"

#include <stdint.h>

#include "bb_config.h"
#include "driver/gpio.h"
#include "esp_log.h"
#include "esp_timer.h"

static const char* TAG = "bb_nav_input";

/* Compile-time enablement:
 *   - Legacy modes (quadrature encoder OR 3-buttons-as-encoder): require all
 *     three of ENC_A / ENC_B / KEY GPIOs to be valid.
 *   - Flipper 6-button (Option A): require BBCLAW_NAV_FLIPPER_6BUTTON=1 and
 *     at least UP / DOWN / OK GPIOs to be valid (LEFT / RIGHT / BACK optional).
 */
#define BB_NAV_HAS_ENCODER_PINS                                                                       \
  ((BBCLAW_NAV_ENC_A_GPIO >= 0) && (BBCLAW_NAV_ENC_B_GPIO >= 0) && (BBCLAW_NAV_KEY_GPIO >= 0))

#define BB_NAV_HAS_FLIPPER_PINS                                                                       \
  (BBCLAW_NAV_FLIPPER_6BUTTON && (BBCLAW_NAV_BTN_UP_GPIO >= 0) && (BBCLAW_NAV_BTN_DOWN_GPIO >= 0) && \
   (BBCLAW_NAV_BTN_OK_GPIO >= 0))

#if BBCLAW_NAV_ENABLE && (BB_NAV_HAS_ENCODER_PINS || BB_NAV_HAS_FLIPPER_PINS)
static bb_nav_input_callback_t s_callback;
static esp_timer_handle_t s_timer;

#if BBCLAW_NAV_FLIPPER_6BUTTON
/* Flipper 6-button mode: each of UP/DOWN/LEFT/RIGHT/OK/BACK has its own
 * debounce state. Buttons whose GPIO macro is -1 are skipped at gpio_config
 * time and their poll branch is short-circuited.
 *
 * UP/DOWN additionally track press-hold timestamps so the polling loop can
 * emit auto-repeat events while the user holds the key — unused for the
 * other four buttons (see header in bb_config.h for rationale).
 */
typedef struct {
  int gpio;
  int raw;
  int stable;
  int count;
  int64_t press_started_ms; /* monotonic time of latest press edge (UP/DOWN only) */
  int64_t last_repeat_ms;   /* monotonic time of most recent emission while held */
} bb_nav_btn_t;

static bb_nav_btn_t s_btn_up = {.gpio = BBCLAW_NAV_BTN_UP_GPIO};
static bb_nav_btn_t s_btn_down = {.gpio = BBCLAW_NAV_BTN_DOWN_GPIO};
static bb_nav_btn_t s_btn_left = {.gpio = BBCLAW_NAV_BTN_LEFT_GPIO};
static bb_nav_btn_t s_btn_right = {.gpio = BBCLAW_NAV_BTN_RIGHT_GPIO};
static bb_nav_btn_t s_btn_ok = {.gpio = BBCLAW_NAV_BTN_OK_GPIO};
static bb_nav_btn_t s_btn_back = {.gpio = BBCLAW_NAV_BTN_BACK_GPIO};
#elif BBCLAW_NAV_BUTTONS_INSTEAD_OF_ENC
/* Two-buttons-as-encoder mode: each press on the A pin emits ROTATE_CCW,
 * each press on the B pin emits ROTATE_CW. Same debounce logic as the
 * KEY pin. Used when the breadboard has plain push-buttons wired to
 * GPIOs nominally reserved for the rotary encoder phases.
 */
static int s_a_raw, s_a_stable, s_a_stable_count;
static int s_b_raw, s_b_stable, s_b_stable_count;
#else
static uint8_t s_last_ab;
static int8_t s_step_accum;
#endif

#if !BBCLAW_NAV_FLIPPER_6BUTTON
static int s_key_raw;
static int s_key_stable;
static int s_key_stable_count;
static int64_t s_key_press_start_ms;
static int s_long_press_sent;
#endif

#if BBCLAW_NAV_FLIPPER_6BUTTON
/* Active-level read for any Flipper button. */
static int read_btn_pressed(int gpio) {
  if (gpio < 0) return 0;
  return gpio_get_level(gpio) == BBCLAW_NAV_KEY_ACTIVE_LEVEL ? 1 : 0;
}
#elif BBCLAW_NAV_BUTTONS_INSTEAD_OF_ENC
static int read_a_pressed(void) {
  /* Same active-level rule as the KEY: pressed = ACTIVE_LEVEL. */
  return gpio_get_level(BBCLAW_NAV_ENC_A_GPIO) == BBCLAW_NAV_KEY_ACTIVE_LEVEL ? 1 : 0;
}
static int read_b_pressed(void) {
  return gpio_get_level(BBCLAW_NAV_ENC_B_GPIO) == BBCLAW_NAV_KEY_ACTIVE_LEVEL ? 1 : 0;
}
#else
static uint8_t read_ab_state(void) {
  uint8_t a = gpio_get_level(BBCLAW_NAV_ENC_A_GPIO) ? 1U : 0U;
  uint8_t b = gpio_get_level(BBCLAW_NAV_ENC_B_GPIO) ? 1U : 0U;
  return (uint8_t)((a << 1) | b);
}
#endif

#if !BBCLAW_NAV_FLIPPER_6BUTTON
static int read_key_pressed(void) {
  int level = gpio_get_level(BBCLAW_NAV_KEY_GPIO);
  return level == BBCLAW_NAV_KEY_ACTIVE_LEVEL ? 1 : 0;
}
#endif

static void emit_event(bb_nav_event_t event) {
  if (s_callback != NULL) {
    s_callback(event);
  }
}

#if BBCLAW_NAV_FLIPPER_6BUTTON || BBCLAW_NAV_BUTTONS_INSTEAD_OF_ENC
/* Debounced edge detection: when raw input matches stable for at least
 * debounce_samples polls AND differs from the previously latched stable
 * state, we have a confirmed transition. Returns 1 on a press edge
 * (released → pressed), -1 on release, 0 otherwise.
 */
static int debounce_step(int* raw_var, int* stable_var, int* count_var, int new_raw, int debounce_samples) {
  if (new_raw == *raw_var) {
    (*count_var)++;
  } else {
    *raw_var = new_raw;
    *count_var = 0;
  }
  if (*count_var >= debounce_samples && new_raw != *stable_var) {
    int prev = *stable_var;
    *stable_var = new_raw;
    return new_raw && !prev ? 1 : (!new_raw && prev ? -1 : 0);
  }
  return 0;
}
#endif

#if BBCLAW_NAV_FLIPPER_6BUTTON
/* Convenience wrapper: poll one Flipper button, return debounce_step()
 * result (1 = press edge, -1 = release edge, 0 = no change). Buttons
 * whose GPIO is -1 yield 0.
 */
static int poll_btn(bb_nav_btn_t* btn, int debounce_samples) {
  if (btn->gpio < 0) return 0;
  return debounce_step(&btn->raw, &btn->stable, &btn->count, read_btn_pressed(btn->gpio), debounce_samples);
}
#endif

static void nav_poll_cb(void* arg) {
  (void)arg;

  const int debounce_samples =
      (BBCLAW_NAV_KEY_DEBOUNCE_MS + BBCLAW_NAV_POLL_MS - 1) / BBCLAW_NAV_POLL_MS;
  const int eff_debounce = debounce_samples > 0 ? debounce_samples : 1;

#if BBCLAW_NAV_FLIPPER_6BUTTON
  /* Flipper 6-button mode (Phase 5 / Option B).
   *
   * Each direction now has its own dedicated event. UP/DOWN/LEFT/RIGHT/BACK
   * fire on the press edge; OK fires on the release edge so a quick tap is
   * treated as a click, matching the legacy KEY semantics that picker /
   * settings code was originally written against.
   *
   * The encoder/legacy modes still emit ROTATE_CCW / ROTATE_CW / CLICK /
   * LONG_PRESS — those names are aliases for UP / DOWN / OK / BACK, so the
   * same downstream switch/case handles both input families.
   */
  /* UP/DOWN: emit on press edge AND auto-repeat while held. The repeat path
   * uses the same emit_event hook so downstream handlers don't need to know
   * whether a given event came from a real key press or a hold-repeat. */
  int64_t now_ms = esp_timer_get_time() / 1000;
  int up_edge = poll_btn(&s_btn_up, eff_debounce);
  if (up_edge > 0) {
    s_btn_up.press_started_ms = now_ms;
    s_btn_up.last_repeat_ms = now_ms;
    emit_event(BB_NAV_EVENT_UP);
  } else if (up_edge < 0) {
    s_btn_up.press_started_ms = 0;
  } else if (s_btn_up.stable && s_btn_up.press_started_ms > 0) {
    int64_t held_ms = now_ms - s_btn_up.press_started_ms;
    if (held_ms >= BBCLAW_NAV_REPEAT_INITIAL_MS &&
        now_ms - s_btn_up.last_repeat_ms >= BBCLAW_NAV_REPEAT_INTERVAL_MS) {
      s_btn_up.last_repeat_ms = now_ms;
      emit_event(BB_NAV_EVENT_UP);
    }
  }

  int down_edge = poll_btn(&s_btn_down, eff_debounce);
  if (down_edge > 0) {
    s_btn_down.press_started_ms = now_ms;
    s_btn_down.last_repeat_ms = now_ms;
    emit_event(BB_NAV_EVENT_DOWN);
  } else if (down_edge < 0) {
    s_btn_down.press_started_ms = 0;
  } else if (s_btn_down.stable && s_btn_down.press_started_ms > 0) {
    int64_t held_ms = now_ms - s_btn_down.press_started_ms;
    if (held_ms >= BBCLAW_NAV_REPEAT_INITIAL_MS &&
        now_ms - s_btn_down.last_repeat_ms >= BBCLAW_NAV_REPEAT_INTERVAL_MS) {
      s_btn_down.last_repeat_ms = now_ms;
      emit_event(BB_NAV_EVENT_DOWN);
    }
  }

  if (poll_btn(&s_btn_left, eff_debounce) > 0) {
    emit_event(BB_NAV_EVENT_LEFT);
  }
  if (poll_btn(&s_btn_right, eff_debounce) > 0) {
    emit_event(BB_NAV_EVENT_RIGHT);
  }
  /* OK: release edge → click (so a quick tap doesn't fire on press too). */
  if (poll_btn(&s_btn_ok, eff_debounce) < 0) {
    emit_event(BB_NAV_EVENT_OK);
  }
  /* BACK: explicit dedicated key — press edge maps to the "exit overlay"
   * gesture that was previously a long-press hold on the encoder click. */
  if (poll_btn(&s_btn_back, eff_debounce) > 0) {
    emit_event(BB_NAV_EVENT_BACK);
  }
#elif BBCLAW_NAV_BUTTONS_INSTEAD_OF_ENC
  if (debounce_step(&s_a_raw, &s_a_stable, &s_a_stable_count, read_a_pressed(), eff_debounce) > 0) {
    emit_event(BB_NAV_EVENT_ROTATE_CCW);
  }
  if (debounce_step(&s_b_raw, &s_b_stable, &s_b_stable_count, read_b_pressed(), eff_debounce) > 0) {
    emit_event(BB_NAV_EVENT_ROTATE_CW);
  }
#else
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
#endif

#if !BBCLAW_NAV_FLIPPER_6BUTTON
  /* Legacy KEY (single click + long-press) path. Flipper mode handles its
   * OK / BACK keys above and skips this block entirely. */
  int key_raw = read_key_pressed();
  if (key_raw == s_key_raw) {
    s_key_stable_count++;
  } else {
    s_key_raw = key_raw;
    s_key_stable_count = 0;
  }

  if (s_key_stable_count >= eff_debounce && key_raw != s_key_stable) {
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
#endif
}
#endif

esp_err_t bb_nav_input_init(bb_nav_input_callback_t callback) {
#if BBCLAW_NAV_ENABLE && (BB_NAV_HAS_ENCODER_PINS || BB_NAV_HAS_FLIPPER_PINS)
  s_callback = callback;

#if BBCLAW_NAV_FLIPPER_6BUTTON
  /* Build pin_bit_mask from the (up to) 6 buttons whose GPIO macro is >= 0. */
  uint64_t pin_mask = 0;
  if (BBCLAW_NAV_BTN_UP_GPIO >= 0) pin_mask |= (1ULL << BBCLAW_NAV_BTN_UP_GPIO);
  if (BBCLAW_NAV_BTN_DOWN_GPIO >= 0) pin_mask |= (1ULL << BBCLAW_NAV_BTN_DOWN_GPIO);
  if (BBCLAW_NAV_BTN_LEFT_GPIO >= 0) pin_mask |= (1ULL << BBCLAW_NAV_BTN_LEFT_GPIO);
  if (BBCLAW_NAV_BTN_RIGHT_GPIO >= 0) pin_mask |= (1ULL << BBCLAW_NAV_BTN_RIGHT_GPIO);
  if (BBCLAW_NAV_BTN_OK_GPIO >= 0) pin_mask |= (1ULL << BBCLAW_NAV_BTN_OK_GPIO);
  if (BBCLAW_NAV_BTN_BACK_GPIO >= 0) pin_mask |= (1ULL << BBCLAW_NAV_BTN_BACK_GPIO);

  gpio_config_t io_conf = {
      .pin_bit_mask = pin_mask,
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

  /* Initialize debounce state for every button (skip those = -1). */
  bb_nav_btn_t* all_btns[] = {&s_btn_up, &s_btn_down, &s_btn_left, &s_btn_right, &s_btn_ok, &s_btn_back};
  for (size_t i = 0; i < sizeof(all_btns) / sizeof(all_btns[0]); ++i) {
    bb_nav_btn_t* b = all_btns[i];
    if (b->gpio < 0) continue;
    b->raw = read_btn_pressed(b->gpio);
    b->stable = b->raw;
    b->count = 0;
    b->press_started_ms = 0;
    b->last_repeat_ms = 0;
  }
#else
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

#if BBCLAW_NAV_BUTTONS_INSTEAD_OF_ENC
  s_a_raw = read_a_pressed();
  s_a_stable = s_a_raw;
  s_a_stable_count = 0;
  s_b_raw = read_b_pressed();
  s_b_stable = s_b_raw;
  s_b_stable_count = 0;
#else
  s_last_ab = read_ab_state();
  s_step_accum = 0;
#endif

  s_key_raw = read_key_pressed();
  s_key_stable = s_key_raw;
  s_key_stable_count = 0;
  s_key_press_start_ms = 0;
  s_long_press_sent = 0;
#endif /* BBCLAW_NAV_FLIPPER_6BUTTON */

  const esp_timer_create_args_t timer_args = {
      .callback = nav_poll_cb,
      .name = "bb_nav_poll",
  };
  ESP_ERROR_CHECK(esp_timer_create(&timer_args, &s_timer));
  ESP_ERROR_CHECK(esp_timer_start_periodic(s_timer, BBCLAW_NAV_POLL_MS * 1000));

#if BBCLAW_NAV_FLIPPER_6BUTTON
  ESP_LOGI(TAG,
           "nav init mode=flipper6 up=%d down=%d left=%d right=%d ok=%d back=%d pull=%s key_active=%d "
           "poll_ms=%d debounce_ms=%d",
           BBCLAW_NAV_BTN_UP_GPIO, BBCLAW_NAV_BTN_DOWN_GPIO, BBCLAW_NAV_BTN_LEFT_GPIO,
           BBCLAW_NAV_BTN_RIGHT_GPIO, BBCLAW_NAV_BTN_OK_GPIO, BBCLAW_NAV_BTN_BACK_GPIO,
           BBCLAW_NAV_PULL_UP ? "up" : "down", BBCLAW_NAV_KEY_ACTIVE_LEVEL, BBCLAW_NAV_POLL_MS,
           BBCLAW_NAV_KEY_DEBOUNCE_MS);
#else
  ESP_LOGI(TAG,
           "nav init mode=%s a=%d b=%d key=%d pull=%s key_active=%d poll_ms=%d debounce_ms=%d long_ms=%d",
#if BBCLAW_NAV_BUTTONS_INSTEAD_OF_ENC
           "buttons",
#else
           "quadrature",
#endif
           BBCLAW_NAV_ENC_A_GPIO, BBCLAW_NAV_ENC_B_GPIO, BBCLAW_NAV_KEY_GPIO, BBCLAW_NAV_PULL_UP ? "up" : "down",
           BBCLAW_NAV_KEY_ACTIVE_LEVEL, BBCLAW_NAV_POLL_MS, BBCLAW_NAV_KEY_DEBOUNCE_MS, BBCLAW_NAV_LONG_PRESS_MS);
#endif
  return ESP_OK;
#else
  (void)callback;
  ESP_LOGI(TAG, "nav disabled");
  return ESP_OK;
#endif
}
