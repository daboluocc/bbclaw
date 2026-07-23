/**
 * 体感导航 —— BMI270 倾斜 → UP/DOWN/LEFT/RIGHT，见 bb_nav_imu.h。
 * 算法：50Hz 读 accel → EMA 低通 → 相对 neutral 的 pitch/roll → 每轴三态滞回
 * （ENTER 25°/EXIT 12°）+ 主导轴仲裁 + 动态运动抑制 + confirm 去抖 → 边沿触发注入；
 * UP/DOWN 长倾自动重复（滚动），LEFT/RIGHT 只单次。倾斜阈值/轴向/符号需真机标定。
 */
#include "bb_nav_imu.h"

#include "bb_config.h"

#if BBCLAW_IMU_BMI270_NAV && !defined(BBCLAW_SIMULATOR)

#include <math.h>

#include "esp_log.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

#include "bb_nav_input.h"
#include "bmi270.h"

static const char *TAG = "bb_nav_imu";

/* ── 可调常量（真机标定起点）── */
#define POLL_MS            20     /* 50 Hz */
#define EMA_ALPHA          0.20f  /* τ≈80ms 低通 */
#define ENTER_DEG          25.0f  /* 触发阈值 */
#define EXIT_DEG           12.0f  /* 回中/死区（滞回间隙 13°）*/
#define DOMINANCE_DEG      8.0f   /* 主导轴需领先另一轴，避免斜置双触发 */
#define CONFIRM_SAMPLES    2      /* 持续 2 帧（~40ms）才算数 */
#define MOTION_TOL_MG      300.0f /* |a|−1000mg 超出=动态,抑制触发 */
#define RAD2DEG            57.2958f
#define PITCH_SIGN         (+1)   /* TODO 真机标定 */
#define ROLL_SIGN          (+1)   /* TODO 真机标定 */
#define NEUTRAL_SAMPLES    12

typedef struct {
  int state;   /* -1 / 0 / +1 已锁存 */
  int pending; /* 候选态 */
  int confirm; /* 连续帧计数 */
  int64_t press_ms, last_emit_ms;
} axis_t;

static float s_fx, s_fy, s_fz;  /* EMA 后 accel(mg) */
static float s_pitch0, s_roll0; /* neutral 偏置(deg) */

/* 三态滞回：中立→±需 |a|>ENTER；±→中立需 |a|<EXIT。 */
static int hysteretic_state(int cur, float a) {
  if (cur == 0) {
    if (a > ENTER_DEG) return +1;
    if (a < -ENTER_DEG) return -1;
    return 0;
  }
  if (cur > 0) return (a < EXIT_DEG) ? 0 : +1;
  return (a > -EXIT_DEG) ? 0 : -1;
}

static void emit_dir(int is_pitch, int st) {
  bb_nav_event_t ev;
  if (is_pitch) ev = (st > 0) ? BB_NAV_EVENT_DOWN : BB_NAV_EVENT_UP;   /* 前倾/后仰 → 下/上 */
  else ev = (st > 0) ? BB_NAV_EVENT_RIGHT : BB_NAV_EVENT_LEFT;         /* 右倾/左倾 → 右/左 */
  bb_nav_input_inject(ev);
}

static void update_axis(axis_t *ax, float angle, int dominant, int moving, int is_pitch, int64_t now,
                        int allow_repeat) {
  /* 非主导轴不能 ENTER（传 0），但已锁存的轴仍用真实角度以便 EXIT 回中。 */
  int target = moving ? ax->state : hysteretic_state(ax->state, dominant ? angle : (ax->state ? angle : 0.0f));

  if (target != ax->state) {
    if (target == ax->pending) {
      if (++ax->confirm >= CONFIRM_SAMPLES) {
        ax->state = target;
        ax->confirm = 0;
        if (target != 0) {
          emit_dir(is_pitch, target);
          ax->press_ms = now;
          ax->last_emit_ms = now;
        }
      }
    } else {
      ax->pending = target;
      ax->confirm = 1;
    }
    return;
  }
  ax->pending = ax->state;
  ax->confirm = 0;
  /* 长倾自动重复（仅 UP/DOWN）：过 INITIAL 后每 INTERVAL 再发一次。 */
  if (allow_repeat && ax->state != 0 && !moving) {
    if ((now - ax->press_ms) >= BBCLAW_NAV_REPEAT_INITIAL_MS &&
        (now - ax->last_emit_ms) >= BBCLAW_NAV_REPEAT_INTERVAL_MS) {
      emit_dir(is_pitch, ax->state);
      ax->last_emit_ms = now;
    }
  }
}

static void imu_task(void *arg) {
  (void)arg;
  /* neutral 标定：静置取均值（假定开机时握持在正常朝向）。 */
  float psum = 0, rsum = 0;
  int n = 0;
  for (int i = 0; i < NEUTRAL_SAMPLES; i++) {
    float x, y, z;
    if (bb_bmi270_read_accel_mg(&x, &y, &z) == ESP_OK) {
      psum += PITCH_SIGN * atan2f(z, x) * RAD2DEG;
      rsum += ROLL_SIGN * atan2f(y, x) * RAD2DEG;
      if (i == 0) { s_fx = x; s_fy = y; s_fz = z; }
      n++;
    }
    vTaskDelay(pdMS_TO_TICKS(POLL_MS));
  }
  s_pitch0 = n ? psum / n : 0.0f;
  s_roll0 = n ? rsum / n : 0.0f;
  ESP_LOGI(TAG, "tilt-nav neutral pitch0=%.1f roll0=%.1f (n=%d)", s_pitch0, s_roll0, n);

  axis_t pitch = {0}, roll = {0};
  int64_t last_dbg = 0;
  for (;;) {
    vTaskDelay(pdMS_TO_TICKS(POLL_MS));
    float x, y, z;
    if (bb_bmi270_read_accel_mg(&x, &y, &z) != ESP_OK) continue;
    s_fx += EMA_ALPHA * (x - s_fx);
    s_fy += EMA_ALPHA * (y - s_fy);
    s_fz += EMA_ALPHA * (z - s_fz);

    float mag = sqrtf(s_fx * s_fx + s_fy * s_fy + s_fz * s_fz);
    int moving = fabsf(mag - 1000.0f) > MOTION_TOL_MG;

    float p = PITCH_SIGN * atan2f(s_fz, s_fx) * RAD2DEG - s_pitch0;
    float r = ROLL_SIGN * atan2f(s_fy, s_fx) * RAD2DEG - s_roll0;

    int pitch_dom = fabsf(p) >= fabsf(r) + DOMINANCE_DEG;
    int roll_dom = fabsf(r) >= fabsf(p) + DOMINANCE_DEG;

    int64_t now = esp_timer_get_time() / 1000;
    update_axis(&pitch, p, pitch_dom, moving, 1, now, 1); /* UP/DOWN 可重复 */
    update_axis(&roll, r, roll_dom, moving, 0, now, 0);   /* LEFT/RIGHT 单次 */

    /* 标定调试：每 1s 打一次角度/态（真机整定阈值/符号用；量产可关）。 */
    if (now - last_dbg >= 1000) {
      last_dbg = now;
      ESP_LOGD(TAG, "pitch=%.0f(%d) roll=%.0f(%d) mag=%.0f%s", p, pitch.state, r, roll.state, mag,
               moving ? " MOVING" : "");
    }
  }
}

esp_err_t bb_nav_imu_init(void) {
  esp_err_t err = bb_bmi270_init();
  if (err != ESP_OK) {
    ESP_LOGW(TAG, "BMI270 init failed (%s) — 体感导航禁用（其它输入不受影响）", esp_err_to_name(err));
    return err;
  }
  BaseType_t ok = xTaskCreate(imu_task, "navimu", 4096, NULL, 5, NULL);
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "navimu task create failed");
    return ESP_ERR_NO_MEM;
  }
  ESP_LOGI(TAG, "tilt-nav started (50Hz, enter=%.0f° exit=%.0f°)", (float)ENTER_DEG, (float)EXIT_DEG);
  return ESP_OK;
}

#else /* disabled / simulator */

esp_err_t bb_nav_imu_init(void) { return ESP_OK; }

#endif
