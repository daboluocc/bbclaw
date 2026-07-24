/**
 * 体感导航 —— BMI270 倾斜 → 导航事件，见 bb_nav_imu.h。
 *
 * 交互（竖屏 stick 手持）：**左/右轻倾（roll 轴）→ 向上/向下滚动**（主手势），
 * 倾角越大滚得越快（变速自动重复）；前/后倾（pitch 轴）→ LEFT/RIGHT（单次，菜单横向）。
 * 配侧键（OK/长按 BACK）= 完整无触摸导航。
 *
 * 算法：50Hz 读 accel → EMA 低通 → 相对 neutral 的 roll/pitch → 每轴三态滞回
 * （低阈值，轻倾即触发）+ 主导轴仲裁 + 动态运动抑制 + confirm 去抖 → 边沿触发注入；
 * roll(上下)按倾角变速重复。方向符号/轴向需真机确认（用户倾一倾即可校准）。
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
#define POLL_MS         20     /* 50 Hz */
#define EMA_ALPHA       0.25f  /* 低通 */
#define ENTER_DEG       15.0f  /* 触发阈值（轻倾即触发）*/
#define EXIT_DEG        8.0f   /* 回中/死区（滞回间隙 7°）*/
#define DOMINANCE_DEG   6.0f   /* 主导轴需领先另一轴，避免斜倾双触发 */
#define CONFIRM_SAMPLES 2      /* 持续 2 帧(~40ms)才算数 */
#define MOTION_TOL_MG   350.0f /* |a|−1000mg 超出=动态,抑制触发 */
#define RAD2DEG         57.2958f
#define NEUTRAL_SAMPLES 12

/* ── 变速滚动：|roll 角| 从 ENTER 到 SPEED_MAX_DEG 线性映射重复间隔 SLOW→FAST ── */
#define SPEED_MAX_DEG   50.0f
#define REPEAT_SLOW_MS  450 /* 刚过阈值：慢 */
#define REPEAT_FAST_MS  70  /* 大幅倾斜：快 */
#define REPEAT_INITIAL_MS 260 /* 首次重复前的停顿（单次轻点=一步）*/

/* ── 方向符号（据官方 IMU 轴向图：X=长轴/Y=宽/Z=屏法线，roll=左右倾）。
 *    按推算取值，真机若反了翻符号即可。 ── */
#define ROLL_UPDOWN_SIGN (-1) /* 左倾→UP / 右倾→DOWN */
#define PITCH_LR_SIGN    (+1)

typedef enum { AX_UPDOWN, AX_LEFTRIGHT } axis_kind_t;

typedef struct {
  int state;   /* -1 / 0 / +1 已锁存 */
  int pending; /* 候选态 */
  int confirm; /* 连续帧计数 */
  int64_t press_ms, last_emit_ms;
} axis_t;

static float s_fx, s_fy, s_fz;      /* EMA 后 accel(mg) */
static float s_gx, s_gy, s_gz, s_gxy; /* neutral 重力向量(mg) + 其 XY 投影模长 */

static int hysteretic_state(int cur, float a) {
  if (cur == 0) {
    if (a > ENTER_DEG) return +1;
    if (a < -ENTER_DEG) return -1;
    return 0;
  }
  if (cur > 0) return (a < EXIT_DEG) ? 0 : +1;
  return (a > -EXIT_DEG) ? 0 : -1;
}

static void emit_for(axis_kind_t kind, int st) {
  bb_nav_event_t ev;
  if (kind == AX_UPDOWN) ev = (st > 0) ? BB_NAV_EVENT_DOWN : BB_NAV_EVENT_UP;
  else ev = (st > 0) ? BB_NAV_EVENT_RIGHT : BB_NAV_EVENT_LEFT;
  bb_nav_input_inject(ev);
}

/* |angle| → 重复间隔：倾角越大间隔越短（滚得越快）。 */
static int repeat_interval_ms(float angle_abs) {
  float t = (angle_abs - ENTER_DEG) / (SPEED_MAX_DEG - ENTER_DEG);
  if (t < 0) t = 0;
  if (t > 1) t = 1;
  return (int)(REPEAT_SLOW_MS + t * (REPEAT_FAST_MS - REPEAT_SLOW_MS));
}

static void update_axis(axis_t *ax, float angle, int dominant, int moving, axis_kind_t kind, int64_t now,
                        int variable_repeat) {
  int target = moving ? ax->state : hysteretic_state(ax->state, dominant ? angle : (ax->state ? angle : 0.0f));

  if (target != ax->state) {
    if (target == ax->pending) {
      if (++ax->confirm >= CONFIRM_SAMPLES) {
        ax->state = target;
        ax->confirm = 0;
        if (target != 0) {
          emit_for(kind, target);
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
  /* 长倾自动重复（仅上下轴，按倾角变速）。 */
  if (variable_repeat && ax->state != 0 && !moving) {
    int interval = repeat_interval_ms(fabsf(angle));
    if ((now - ax->press_ms) >= REPEAT_INITIAL_MS && (now - ax->last_emit_ms) >= interval) {
      emit_for(kind, ax->state);
      ax->last_emit_ms = now;
    }
  }
}

static void imu_task(void *arg) {
  (void)arg;
  /* neutral 标定：静置取重力向量均值（不假定哪根轴竖直——存向量，后面用相对它的
   * in-plane 旋转 / out-of-plane 俯仰算倾斜，鲁棒于握持轴向）。 */
  float sx = 0, sy = 0, sz = 0;
  int n = 0;
  for (int i = 0; i < NEUTRAL_SAMPLES; i++) {
    float x, y, z;
    if (bb_bmi270_read_accel_mg(&x, &y, &z) == ESP_OK) {
      sx += x;
      sy += y;
      sz += z;
      if (i == 0) { s_fx = x; s_fy = y; s_fz = z; }
      n++;
    }
    vTaskDelay(pdMS_TO_TICKS(POLL_MS));
  }
  if (n) { s_gx = sx / n; s_gy = sy / n; s_gz = sz / n; }
  s_gxy = hypotf(s_gx, s_gy);
  ESP_LOGI(TAG, "tilt-nav neutral g=(%.0f,%.0f,%.0f) |xy|=%.0f (n=%d)", s_gx, s_gy, s_gz, s_gxy, n);

  axis_t roll = {0}, pitch = {0};
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

    /* 鲁棒倾斜量（相对 neutral 重力向量，不管哪根轴竖直，也不除零）：
     *   左右倾(→上下滚) = 重力在屏平面(XY)内相对 neutral 的有符号旋转角(cross,dot)；
     *   前后倾(→左右)   = 重力"离开屏平面"的俯仰角(Z vs XY 模)相对 neutral 的变化。
     * 竖握左右倾 = XY 内旋转 → r；前后倾 = 转出屏平面 → p，两者天然分离。 */
    float r = ROLL_UPDOWN_SIGN *
              atan2f(s_fx * s_gy - s_fy * s_gx, s_fx * s_gx + s_fy * s_gy) * RAD2DEG;
    float p = PITCH_LR_SIGN * (atan2f(s_fz, hypotf(s_fx, s_fy)) - atan2f(s_gz, s_gxy)) * RAD2DEG;

    int roll_dom = fabsf(r) >= fabsf(p) + DOMINANCE_DEG;
    int pitch_dom = fabsf(p) >= fabsf(r) + DOMINANCE_DEG;

    int64_t now = esp_timer_get_time() / 1000;
    update_axis(&roll, r, roll_dom, moving, AX_UPDOWN, now, 1);    /* 上下：变速重复 */
    update_axis(&pitch, p, pitch_dom, moving, AX_LEFTRIGHT, now, 0); /* 左右：单次 */

    if (now - last_dbg >= 1000) {
      last_dbg = now;
      ESP_LOGD(TAG, "roll=%.0f(%d) pitch=%.0f(%d) mag=%.0f%s", r, roll.state, p, pitch.state, mag,
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
  ESP_LOGI(TAG, "tilt-nav started (roll→上下变速滚, enter=%.0f°)", (float)ENTER_DEG);
  return ESP_OK;
}

#else /* disabled / simulator */

esp_err_t bb_nav_imu_init(void) { return ESP_OK; }

#endif
