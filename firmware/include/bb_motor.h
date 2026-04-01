#pragma once

#include "esp_err.h"

typedef enum {
  /** PTT 按下：开始 TX / 录音 */
  BB_MOTOR_PATTERN_PTT_PRESS = 0,
  /** PTT 松开：结束按住（与 PRESS 成对，便于感知按键行程） */
  BB_MOTOR_PATTERN_PTT_RELEASE = 1,
  /** 拉取到新任务等通知：双短震 */
  BB_MOTOR_PATTERN_TASK_NOTIFY = 2,
  /** 错误：单长震 */
  BB_MOTOR_PATTERN_ERROR_ALERT = 3,
} bb_motor_pattern_t;

esp_err_t bb_motor_init(void);
esp_err_t bb_motor_trigger(bb_motor_pattern_t pattern);
