#pragma once

#include "esp_err.h"

/**
 * 录音回放（ADR-044 P1a 附属）：设备端播放 SD 上的 .opus 录音段。
 * 设置页「Recordings」浏览器调用；与录音/TTS 互斥（调用方无需自查,
 * toggle 内部拒绝并返错）。
 */

/** 播放指定文件；若正在播同一文件则停止；正在播别的则切换。
 *  返回 ESP_ERR_INVALID_STATE = 录音中/TTS 播放中,不可回放。 */
esp_err_t bb_recplay_toggle(const char* path);

/** 停止回放（幂等；阻塞至播放任务收尾,毫秒级）。 */
void bb_recplay_stop(void);

/** 1 = 回放进行中。 */
int bb_recplay_active(void);

/** 当前播放文件路径（未播放返回 ""）。UI 标记用。 */
const char* bb_recplay_current(void);
