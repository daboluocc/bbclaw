/**
 * bb_led.c — state-driven 状态灯实现（v2：三色三波形）
 *
 * 见 design/firmware_status_led.md。LED 任务订阅 bb_state 并按优先级表
 * 自己合成效果。业务层只在需要"瞬时提示"时调 bb_led_pulse()。
 *
 * v2 变更（issue #168）：
 *   - 颜色仅用 红 / 绿 / 橙（或灭）—— 红绿灯配色
 *   - 动画仅用 SOLID / SLOW_BLINK(~1Hz) / FAST_BLINK(~3Hz)
 *   - 移除所有 BREATHE / PULSE / SINGLE_FLASH / TRIPLE_BLINK
 *   - 状态切换瞬时，无渐变
 */
#include "bb_led.h"

#include <stdbool.h>
#include <stdint.h>
#include <string.h>

#include "bb_config.h"
#include "bb_power.h"
#include "bb_state.h"
#include "bb_time.h"
#include "driver/gpio.h"
#include "driver/ledc.h"
#include "driver/rmt_tx.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "led_strip_encoder.h"

static const char* TAG = "bb_led";

#define BB_LED_TASK_PERIOD_MS 30

/* --- 三线 PWM 路径 --- */
#define BB_LED_LEDC_MODE LEDC_LOW_SPEED_MODE
#define BB_LED_LEDC_TIMER LEDC_TIMER_0
#define BB_LED_LEDC_DUTY_RES LEDC_TIMER_10_BIT
#define BB_LED_LEDC_FREQ_HZ 4000
#define BB_LED_LEDC_MAX_DUTY ((1U << 10) - 1U)

/* --- WS2812 --- */
#define BB_LED_WS2812_RESOLUTION_HZ 10000000 /* 1 tick = 0.1us */
#define BB_LED_WS2812_PIXEL_BYTES 3          /* GRB */

/* ================ 内部类型 ================ */

typedef struct {
  uint8_t r, g, b;
} rgb_t;

typedef enum {
  ANIM_SOLID = 0,       /* 常亮 */
  ANIM_SLOW_BLINK,      /* 慢闪 ~1Hz，period=1000ms，50% 方波 */
  ANIM_FAST_BLINK,      /* 快闪 ~3Hz，period=333ms，50% 方波 */
} anim_mode_t;

typedef struct {
  rgb_t       color;
  anim_mode_t mode;
  uint32_t    period_ms;
} effect_t;

typedef struct {
  bool           active;
  bb_led_pulse_t kind;
  int64_t        start_ms;
  uint32_t       duration_ms;
} pulse_state_t;

/* ================ 模块静态 ================ */

static portMUX_TYPE s_lock = portMUX_INITIALIZER_UNLOCKED;
static pulse_state_t s_pulse;

static bool    s_ready;
static bool    s_boot_anim_active;
static int64_t s_boot_anim_start_ms;
static volatile bool s_suspended; /* 息屏休眠时挂起=强制灭灯,见 bb_led_set_suspended */

#if BBCLAW_STATUS_LED_WS2812
static rmt_channel_handle_t s_rmt_chan;
static rmt_encoder_handle_t s_rmt_encoder;
static uint8_t              s_ws2812_pixel[BB_LED_WS2812_PIXEL_BYTES];
#else
static uint32_t s_led_on_duty;
static uint32_t s_led_off_duty;
#endif

#if BBCLAW_STATUS_LED_BOOT_ANIM_ENABLE && BBCLAW_STATUS_LED_BOOT_ANIM_LOOPS > 0
#define BB_LED_BOOT_TOTAL_MS (3U * BBCLAW_STATUS_LED_BOOT_ANIM_STEP_MS * BBCLAW_STATUS_LED_BOOT_ANIM_LOOPS)
#else
#define BB_LED_BOOT_TOTAL_MS 0U
#endif

static uint32_t clamp_brightness_pct(void) {
  if (BBCLAW_STATUS_LED_BRIGHTNESS_PCT <= 0) return 0U;
  if (BBCLAW_STATUS_LED_BRIGHTNESS_PCT >= 100) return 100U;
  return (uint32_t)BBCLAW_STATUS_LED_BRIGHTNESS_PCT;
}

/* ================ 硬件输出 ================ */

#if BBCLAW_STATUS_LED_WS2812
static void output_rgb(uint8_t r, uint8_t g, uint8_t b) {
  uint32_t br = clamp_brightness_pct();
  r = (uint8_t)((r * br) / 100U);
  g = (uint8_t)((g * br) / 100U);
  b = (uint8_t)((b * br) / 100U);
  s_ws2812_pixel[0] = g;
  s_ws2812_pixel[1] = r;
  s_ws2812_pixel[2] = b;
  rmt_transmit_config_t tx_cfg = { .loop_count = 0 };
  rmt_transmit(s_rmt_chan, s_rmt_encoder, s_ws2812_pixel, sizeof(s_ws2812_pixel), &tx_cfg);
  rmt_tx_wait_all_done(s_rmt_chan, portMAX_DELAY);
}
#else
static void ledc_apply(ledc_channel_t channel, int on) {
  uint32_t duty = on ? s_led_on_duty : s_led_off_duty;
  (void)ledc_set_duty(BB_LED_LEDC_MODE, channel, duty);
  (void)ledc_update_duty(BB_LED_LEDC_MODE, channel);
}

#if BBCLAW_STATUS_LED_KIND_RGB_MODULE
/* 共阴 RGB 模块只能 on/off：把目标 RGB 量化到阈值 */
static void output_rgb(uint8_t r, uint8_t g, uint8_t b) {
  ledc_apply(LEDC_CHANNEL_0, r > 64 ? 1 : 0);
  ledc_apply(LEDC_CHANNEL_1, g > 64 ? 1 : 0);
  ledc_apply(LEDC_CHANNEL_2, b > 64 ? 1 : 0);
}
#else
/* 三线 RYG：把目标 RGB 近似成 R / Y(=R+G) / G 中一个 */
static void output_rgb(uint8_t r, uint8_t g, uint8_t b) {
  int R = r > 64 ? 1 : 0;
  int Y = (r > 64 && g > 64) ? 1 : 0;
  int G = (g > 64 || b > 64) ? 1 : 0;  /* 蓝色退化为绿 */
  if (Y) { R = 0; G = 0; }
  ledc_apply(LEDC_CHANNEL_0, R);
  ledc_apply(LEDC_CHANNEL_1, Y);
  ledc_apply(LEDC_CHANNEL_2, G);
}
#endif
#endif

static void output_off(void) { output_rgb(0, 0, 0); }

/* ================ 初始化底层 ================ */

#if BBCLAW_STATUS_LED_WS2812
static esp_err_t hw_init_ws2812(void) {
  rmt_tx_channel_config_t tx_chan_cfg = {
      .clk_src = RMT_CLK_SRC_DEFAULT,
      .gpio_num = BBCLAW_STATUS_LED_R_GPIO,
      .mem_block_symbols = 64,
      .resolution_hz = BB_LED_WS2812_RESOLUTION_HZ,
      .trans_queue_depth = 4,
  };
  esp_err_t err = rmt_new_tx_channel(&tx_chan_cfg, &s_rmt_chan);
  if (err != ESP_OK) return err;
  led_strip_encoder_config_t enc_cfg = { .resolution = BB_LED_WS2812_RESOLUTION_HZ };
  err = rmt_new_led_strip_encoder(&enc_cfg, &s_rmt_encoder);
  if (err != ESP_OK) return err;
  return rmt_enable(s_rmt_chan);
}
#else
static esp_err_t hw_init_pwm_channels(void) {
  const uint32_t br = clamp_brightness_pct();
  const uint32_t active_duty = (BB_LED_LEDC_MAX_DUTY * br) / 100U;
  if (BBCLAW_STATUS_LED_GPIO_ON_LEVEL) {
    s_led_on_duty = active_duty;
    s_led_off_duty = 0U;
  } else {
    s_led_on_duty = BB_LED_LEDC_MAX_DUTY - active_duty;
    s_led_off_duty = BB_LED_LEDC_MAX_DUTY;
  }
  ledc_timer_config_t timer_cfg = {
      .speed_mode = BB_LED_LEDC_MODE,
      .duty_resolution = BB_LED_LEDC_DUTY_RES,
      .timer_num = BB_LED_LEDC_TIMER,
      .freq_hz = BB_LED_LEDC_FREQ_HZ,
      .clk_cfg = LEDC_AUTO_CLK,
  };
  esp_err_t err = ledc_timer_config(&timer_cfg);
  if (err != ESP_OK) return err;
#if BBCLAW_STATUS_LED_KIND_RGB_MODULE
  const int gpios[3] = {BBCLAW_STATUS_LED_R_GPIO, BBCLAW_STATUS_LED_RGB_G_GPIO, BBCLAW_STATUS_LED_RGB_B_GPIO};
#else
  const int gpios[3] = {BBCLAW_STATUS_LED_R_GPIO, BBCLAW_STATUS_LED_Y_GPIO, BBCLAW_STATUS_LED_G_GPIO};
#endif
  for (int i = 0; i < 3; ++i) {
    ledc_channel_config_t ch_cfg = {
        .gpio_num = gpios[i],
        .speed_mode = BB_LED_LEDC_MODE,
        .channel = (ledc_channel_t)i,
        .intr_type = LEDC_INTR_DISABLE,
        .timer_sel = BB_LED_LEDC_TIMER,
        .duty = s_led_off_duty,
        .hpoint = 0,
        .sleep_mode = LEDC_SLEEP_MODE_NO_ALIVE_NO_PD,
    };
    err = ledc_channel_config(&ch_cfg);
    if (err != ESP_OK) return err;
  }
  return ESP_OK;
}
#endif

/* ================ Boot marquee ================ */

static void render_boot_marquee(uint32_t elapsed_ms) {
  uint32_t step = (uint32_t)BBCLAW_STATUS_LED_BOOT_ANIM_STEP_MS;
  if (step == 0U) step = 1U;
  uint32_t idx = (elapsed_ms / step) % 3U;
  if (idx == 0U)      output_rgb(255, 0, 0);
  else if (idx == 1U) output_rgb(0, 255, 0);
  else                output_rgb(255, 165, 0); /* 橙，替代蓝（红绿灯配色） */
}

/* ================ Pulse overlay ================ */

static uint32_t pulse_duration_ms(bb_led_pulse_t kind) {
  switch (kind) {
    case BB_LED_PULSE_SUCCESS:   return 200U;
    case BB_LED_PULSE_ERROR:     return 600U;
    case BB_LED_PULSE_CELEBRATE: return 1000U;
    case BB_LED_PULSE_NOTIFY:    return 400U;
    default:                     return 0U;
  }
}

esp_err_t bb_led_pulse(bb_led_pulse_t kind) {
  if (!BBCLAW_STATUS_LED_ENABLE) return ESP_OK;
  if (!s_ready) return ESP_ERR_INVALID_STATE;
  uint32_t dur = pulse_duration_ms(kind);
  if (dur == 0U) return ESP_ERR_INVALID_ARG;
  int64_t now = bb_now_ms();
  /* pulse 内部优先级：ERROR > CELEBRATE > NOTIFY > SUCCESS
   * 新 pulse 低于当前正在播的则忽略 */
  static const uint8_t prio[BB_LED_PULSE__COUNT] = {
    [BB_LED_PULSE_SUCCESS]   = 1,
    [BB_LED_PULSE_NOTIFY]    = 2,
    [BB_LED_PULSE_CELEBRATE] = 3,
    [BB_LED_PULSE_ERROR]     = 4,
  };
  portENTER_CRITICAL(&s_lock);
  bool accept = true;
  if (s_pulse.active && prio[kind] < prio[s_pulse.kind]) accept = false;
  if (accept) {
    s_pulse.active      = true;
    s_pulse.kind        = kind;
    s_pulse.start_ms    = now;
    s_pulse.duration_ms = dur;
  }
  portEXIT_CRITICAL(&s_lock);
  return ESP_OK;
}

static bool take_pulse(pulse_state_t* out, int64_t now_ms) {
  bool has = false;
  portENTER_CRITICAL(&s_lock);
  if (s_pulse.active) {
    if (now_ms - s_pulse.start_ms >= (int64_t)s_pulse.duration_ms) {
      s_pulse.active = false;
    } else {
      *out = s_pulse;
      has = true;
    }
  }
  portEXIT_CRITICAL(&s_lock);
  return has;
}

/* ================ 状态 → 效果合成 ================ */

/* v2 状态→效果映射（issue #168）
 *
 * 优先级从高到低（第一个满足的条件胜出）：
 *   P1  错误/失联  → 红快闪（FAST_BLINK ~3Hz）
 *   P2  Worker 长任务 → 红慢闪（SLOW_BLINK ~1Hz）
 *   P3  AI 思考/生成  → 橙慢闪（SLOW_BLINK ~1Hz）
 *   P4  倾听中       → 橙常亮（SOLID）
 *   P5  空闲/默认    → 绿常亮（SOLID）
 *
 * 红绿灯配色（红/绿/橙），不再用蓝。RYG 三线板
 * （BBCLAW_STATUS_LED_KIND_RGB_MODULE==0）上橙(255,165,0)经 Y=R+G 近似为黄灯，
 * 正好对应交通灯的黄/琥珀；WS2812/RGB 模块上显示真橙色。
 */
static effect_t compose_from_state(const bb_state_t* st, const bb_power_state_t* bp) {
  (void)bp; /* 低电量不占常态，只通过 pulse overlay 提示 */
  effect_t e = { .color = {0, 255, 0}, .mode = ANIM_SOLID, .period_ms = 0 };

  /* P1: 错误 / 失联 — 红快闪 */
  if (st->agent == BB_AGENT_STATE_DIZZY ||
      st->net == BB_NET_OFFLINE || st->adapter_offline ||
      st->net == BB_NET_DEGRADED) {
    e.color     = (rgb_t){255, 0, 0};
    e.mode      = ANIM_FAST_BLINK;
    e.period_ms = 333;
    return e;
  }

  /* P2: Worker 长任务派发中 — 红慢闪 */
  if (st->worker_busy) {
    e.color     = (rgb_t){255, 0, 0};
    e.mode      = ANIM_SLOW_BLINK;
    e.period_ms = 1000;
    return e;
  }

  /* P3: AI 思考 / 生成中（含 ATTENTION、BUSY、SPEAKING、TTS 播放）— 橙慢闪 */
  if (st->agent == BB_AGENT_STATE_ATTENTION ||
      st->agent == BB_AGENT_STATE_BUSY      ||
      st->agent == BB_AGENT_STATE_SPEAKING  ||
      st->ptt   == BB_PTT_RELEASED_WAIT     ||
      st->tts_in_flight) {
    e.color     = (rgb_t){255, 165, 0};
    e.mode      = ANIM_SLOW_BLINK;
    e.period_ms = 1000;
    return e;
  }

  /* P4: 倾听中（PTT 按下 / agent LISTENING）— 橙常亮 */
  if (st->ptt == BB_PTT_ARMED || st->ptt == BB_PTT_STREAMING ||
      st->agent == BB_AGENT_STATE_LISTENING) {
    e.color     = (rgb_t){255, 165, 0};
    e.mode      = ANIM_SOLID;
    e.period_ms = 0;
    return e;
  }

  /* P5: 空闲 / 待机 — 绿常亮（默认） */
  return e;
}

/* v2 pulse overlay（issue #168）：颜色收敛到红/绿/橙，动画收敛到三波形 */
static effect_t compose_pulse(bb_led_pulse_t kind) {
  effect_t e = { .color = {0, 255, 0}, .mode = ANIM_SOLID, .period_ms = 0 };
  switch (kind) {
    case BB_LED_PULSE_SUCCESS:
      /* 绿常亮 200ms：操作成功 */
      e.color = (rgb_t){0, 255, 0};
      e.mode = ANIM_SOLID;
      break;
    case BB_LED_PULSE_ERROR:
      /* 红快闪 600ms：事件级错误 */
      e.color     = (rgb_t){255, 0, 0};
      e.mode      = ANIM_FAST_BLINK;
      e.period_ms = 333;
      break;
    case BB_LED_PULSE_CELEBRATE:
      /* 绿常亮 1000ms：快速响应庆祝（退化为 SUCCESS，符合"只用三色"原则） */
      e.color = (rgb_t){0, 255, 0};
      e.mode = ANIM_SOLID;
      break;
    case BB_LED_PULSE_NOTIFY:
      /* 橙常亮 400ms：新通知 */
      e.color = (rgb_t){255, 165, 0};
      e.mode = ANIM_SOLID;
      break;
    default:
      break;
  }
  return e;
}

/* ================ 动画 → 亮度因子 ================ */

/* v2 动画因子（issue #168）：仅三种波形，无 sinusoidal 运算 */
static float anim_factor(const effect_t* e, uint32_t elapsed_ms) {
  switch (e->mode) {
    case ANIM_SOLID:
      return 1.0f;
    case ANIM_SLOW_BLINK:
    case ANIM_FAST_BLINK:
      if (e->period_ms == 0) return 1.0f;
      return ((elapsed_ms % e->period_ms) < (e->period_ms / 2U)) ? 1.0f : 0.0f;
  }
  return 0.0f;
}

static void render_effect(const effect_t* e, uint32_t elapsed_ms) {
  float k = anim_factor(e, elapsed_ms);
  uint8_t r = (uint8_t)((float)e->color.r * k);
  uint8_t g = (uint8_t)((float)e->color.g * k);
  uint8_t b = (uint8_t)((float)e->color.b * k);
  output_rgb(r, g, b);
}

/* ================ bb_state listener — 事件触发 pulse ================ */

static void state_listener(const bb_state_t* prev, const bb_state_t* next,
                           const bb_event_payload_t* evt) {
  (void)prev;
  if (!evt) return;
  switch (evt->type) {
    case BB_EVT_AGENT_ERROR:
    case BB_EVT_ASR_ERROR:
      (void)bb_led_pulse(BB_LED_PULSE_ERROR);
      break;
    case BB_EVT_AGENT_TURN_END: {
      uint64_t now = (uint64_t)bb_now_ms();
      uint64_t elapsed = (next && next->turn_start_ms && now > next->turn_start_ms)
                           ? (now - next->turn_start_ms) : UINT64_MAX;
      (void)bb_led_pulse(elapsed < 5000U ? BB_LED_PULSE_CELEBRATE : BB_LED_PULSE_SUCCESS);
      break;
    }
    default:
      break;
  }
}

/* ================ Task ================ */

esp_err_t bb_led_set_suspended(int suspend) {
  if (!BBCLAW_STATUS_LED_ENABLE) return ESP_OK;
  s_suspended = suspend ? true : false;
  return ESP_OK;
}

static void led_task(void* arg) {
  (void)arg;
  bool suspend_off_latched = false; /* 已在挂起态灭过灯,避免每轮重发 RMT 拽醒 CPU */
  while (1) {
    int64_t now_ms = bb_now_ms();

    /* 息屏休眠：强制灭灯,压过 boot/pulse/状态合成。灭一次后拉长轮询(WS2812 自锁,
     * 无需持续重发),既省电又不拖住 CPU light-sleep;唤醒(~150ms 内)恢复正常。 */
    if (s_suspended) {
      if (!suspend_off_latched) {
        output_off();
        suspend_off_latched = true;
      }
      vTaskDelay(pdMS_TO_TICKS(150));
      continue;
    }
    suspend_off_latched = false;

    if (s_boot_anim_active) {
      uint32_t boot_elapsed = (uint32_t)(now_ms - s_boot_anim_start_ms);
      if (boot_elapsed >= BB_LED_BOOT_TOTAL_MS) {
        s_boot_anim_active = false;
      } else {
        render_boot_marquee(boot_elapsed);
        vTaskDelay(pdMS_TO_TICKS(BB_LED_TASK_PERIOD_MS));
        continue;
      }
    }

    pulse_state_t p;
    if (take_pulse(&p, now_ms)) {
      effect_t e = compose_pulse(p.kind);
      render_effect(&e, (uint32_t)(now_ms - p.start_ms));
      vTaskDelay(pdMS_TO_TICKS(BB_LED_TASK_PERIOD_MS));
      continue;
    }

    bb_state_t st = bb_state_get();
    bb_power_state_t bp;
    bb_power_get_state(&bp);
    effect_t e = compose_from_state(&st, &bp);
    render_effect(&e, (uint32_t)now_ms);

    vTaskDelay(pdMS_TO_TICKS(BB_LED_TASK_PERIOD_MS));
  }
}

/* ================ Init ================ */

esp_err_t bb_led_init(void) {
  if (!BBCLAW_STATUS_LED_ENABLE) {
    ESP_LOGI(TAG, "status led disabled by config");
    return ESP_OK;
  }
  if (s_ready) return ESP_OK;

#if BBCLAW_STATUS_LED_WS2812
  if (BBCLAW_STATUS_LED_R_GPIO < 0) {
    ESP_LOGW(TAG, "ws2812 enabled but data gpio not configured");
    return ESP_ERR_INVALID_ARG;
  }
  esp_err_t err = hw_init_ws2812();
  if (err != ESP_OK) {
    ESP_LOGE(TAG, "ws2812 init failed: %s", esp_err_to_name(err));
    return err;
  }
#elif BBCLAW_STATUS_LED_KIND_RGB_MODULE
  if (BBCLAW_STATUS_LED_R_GPIO < 0 || BBCLAW_STATUS_LED_RGB_G_GPIO < 0 ||
      BBCLAW_STATUS_LED_RGB_B_GPIO < 0) {
    ESP_LOGW(TAG, "rgb module gpio not fully configured");
    return ESP_ERR_INVALID_ARG;
  }
  uint64_t mask = (1ULL << BBCLAW_STATUS_LED_R_GPIO) |
                  (1ULL << BBCLAW_STATUS_LED_RGB_G_GPIO) |
                  (1ULL << BBCLAW_STATUS_LED_RGB_B_GPIO);
  gpio_config_t io = {
      .pin_bit_mask = mask,
      .mode = GPIO_MODE_OUTPUT,
      .pull_up_en = GPIO_PULLUP_DISABLE,
      .pull_down_en = GPIO_PULLDOWN_DISABLE,
      .intr_type = GPIO_INTR_DISABLE,
  };
  esp_err_t err = gpio_config(&io);
  if (err != ESP_OK) return err;
  err = hw_init_pwm_channels();
  if (err != ESP_OK) return err;
#else
  if (BBCLAW_STATUS_LED_R_GPIO < 0 || BBCLAW_STATUS_LED_Y_GPIO < 0 || BBCLAW_STATUS_LED_G_GPIO < 0) {
    ESP_LOGW(TAG, "status led gpio not fully configured");
    return ESP_ERR_INVALID_ARG;
  }
  uint64_t mask = (1ULL << BBCLAW_STATUS_LED_R_GPIO) |
                  (1ULL << BBCLAW_STATUS_LED_Y_GPIO) |
                  (1ULL << BBCLAW_STATUS_LED_G_GPIO);
  gpio_config_t io = {
      .pin_bit_mask = mask,
      .mode = GPIO_MODE_OUTPUT,
      .pull_up_en = GPIO_PULLUP_DISABLE,
      .pull_down_en = GPIO_PULLDOWN_DISABLE,
      .intr_type = GPIO_INTR_DISABLE,
  };
  esp_err_t err = gpio_config(&io);
  if (err != ESP_OK) return err;
  err = hw_init_pwm_channels();
  if (err != ESP_OK) return err;
#endif

  output_off();

#if BBCLAW_STATUS_LED_BOOT_ANIM_ENABLE && BBCLAW_STATUS_LED_BOOT_ANIM_LOOPS > 0
  s_boot_anim_start_ms = bb_now_ms();
  s_boot_anim_active = true;
  ESP_LOGI(TAG, "boot marquee total_ms=%u step_ms=%d loops=%d",
           (unsigned)BB_LED_BOOT_TOTAL_MS,
           BBCLAW_STATUS_LED_BOOT_ANIM_STEP_MS,
           BBCLAW_STATUS_LED_BOOT_ANIM_LOOPS);
#else
  s_boot_anim_active = false;
#endif

  BaseType_t ok = xTaskCreate(led_task, "bb_led_task", 3072, NULL, 3, NULL);
  if (ok != pdPASS) {
    s_boot_anim_active = false;
    return ESP_ERR_NO_MEM;
  }

  if (bb_state_subscribe(state_listener) != 0) {
    ESP_LOGW(TAG, "bb_state_subscribe failed; pulse-on-event disabled");
  }

  s_ready = true;
#if BBCLAW_STATUS_LED_WS2812
  ESP_LOGI(TAG, "status led ws2812 data_gpio=%d brightness_pct=%d",
           BBCLAW_STATUS_LED_R_GPIO, BBCLAW_STATUS_LED_BRIGHTNESS_PCT);
#else
  ESP_LOGI(TAG, "status led pwm brightness_pct=%d", BBCLAW_STATUS_LED_BRIGHTNESS_PCT);
#endif
  return ESP_OK;
}
