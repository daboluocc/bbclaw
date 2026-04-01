#pragma once

#include "esp_err.h"

/**
 * 外接按键测试：单独轮询 BBCLAW_BUTTON_TEST_GPIO（默认 7），与 PTT 无关。
 * BBCLAW_BUTTON_TEST_GPIO < 0 时不启动任务。
 */
esp_err_t bb_button_test_start(void);
