/**
 * BMI270 拿起/运动唤醒 —— 见 bb_imu_wake.h。
 *
 * 静止时加速度总量 |a|≈1000mg（仅重力，与朝向无关）；拿起/移动叠加线加速度使 |a|
 * 偏离 1000mg → 判为"在动" → bb_sleep_manager_debug_motion() 置 motion_pending，
 * sleep manager 的 tick 在 SLEEPING 态据此唤醒。纯朝向倾斜不改变 |a|，不会误唤醒。
 *
 * TODO(功耗优化)：改用 BMI270 硬件 any-motion 中断（经 M5PM1 PYG1_IRQ/PYG4）零 CPU 唤醒；
 * 现为软件轮询，够用且简单。
 */
#include "bb_imu_wake.h"

#include "bb_config.h"

#if BBCLAW_IMU_BMI270_WAKE && !defined(BBCLAW_SIMULATOR)

#include <math.h>

#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

#include "bb_sleep_manager.h"
#include "bmi270.h"

static const char *TAG = "bb_imu_wake";

#define POLL_MS   66      /* ~15Hz：够抓"拿起"，功耗低 */
#define MOTION_MG 180.0f  /* |a|−1000mg 偏离阈值(拿起的线加速度)；越小越灵敏，太小会被桌面震动/手抖误唤醒。真机可调 */
#define CONFIRM   2       /* 连续 2 帧才算，滤掉单帧毛刺 */

static void wake_task(void *arg) {
  (void)arg;
  int hits = 0;
  for (;;) {
    vTaskDelay(pdMS_TO_TICKS(POLL_MS));
    float x, y, z;
    if (bb_bmi270_read_accel_mg(&x, &y, &z) != ESP_OK) continue;
    float a = sqrtf(x * x + y * y + z * z);
    if (fabsf(a - 1000.0f) > MOTION_MG) {
      if (++hits >= CONFIRM) {
        hits = 0;
        bb_sleep_manager_debug_motion(); /* 置 motion_pending → SLEEPING 时唤醒 */
      }
    } else {
      hits = 0;
    }
  }
}

esp_err_t bb_imu_wake_init(void) {
  esp_err_t err = bb_bmi270_init();
  if (err != ESP_OK) {
    ESP_LOGW(TAG, "BMI270 init failed (%s) — 拿起唤醒禁用（其它功能不受影响）", esp_err_to_name(err));
    return err;
  }
  if (xTaskCreate(wake_task, "imuwake", 3072, NULL, 4, NULL) != pdPASS) {
    ESP_LOGE(TAG, "imuwake task create failed");
    return ESP_ERR_NO_MEM;
  }
  ESP_LOGI(TAG, "IMU wake started (BMI270 |a|偏离%.0fmg → sleep manager)", (float)MOTION_MG);
  return ESP_OK;
}

#else /* disabled / simulator */

esp_err_t bb_imu_wake_init(void) { return ESP_OK; }

#endif
