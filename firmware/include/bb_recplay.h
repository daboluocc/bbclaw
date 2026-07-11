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

/** 连续播放整个录音会话：遍历会话目录下 000001.opus,000002.opus… 按序接续播放
 *  （每段独立完整 Ogg,逐段解码,段间极短解码间隙）。再点同一会话 = 停止。
 *  dir = 会话目录绝对路径(如 /sdcard/ambient/1783644997)。
 *  返回 ESP_ERR_INVALID_STATE = 录音中/TTS 播放中。 */
esp_err_t bb_recplay_toggle_session(const char* dir);

/** 停止回放（幂等；阻塞至播放任务收尾,毫秒级）。 */
void bb_recplay_stop(void);

/** 1 = 回放进行中。 */
int bb_recplay_active(void);

/** 当前播放文件路径（未播放返回 ""）。UI 标记用。 */
const char* bb_recplay_current(void);

/* ── 回放页 transport（暂停 / 上一段 / 下一段 / 状态）── */

/** 暂停 / 恢复当前回放。位置原地冻结,恢复从同一样本继续（bb_audio pause gate）。
 *  未在回放时 no-op。 */
void bb_recplay_set_paused(int paused);

/** 1 = 正在回放且已暂停。 */
int bb_recplay_is_paused(void);

/** 会话连播跳段：delta=+1 下一段 / -1 上一段（首段再上一=重放首段;越过末段=结束）。
 *  仅会话模式有效;打断当前段后由播放任务据此调整段号。 */
void bb_recplay_skip(int delta);

/** 回放状态快照（回放页轮询用）。 */
typedef struct {
  int active;    /* 1 = 回放中 */
  int paused;    /* 1 = 已暂停 */
  int session;   /* 1 = 会话连播模式 */
  int seg_cur;   /* 当前段(1-based);0=未知 */
  int seg_total; /* 会话总段数;0=未知/单文件 */
} bb_recplay_state_t;

void bb_recplay_get_state(bb_recplay_state_t* out);
