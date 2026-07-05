#pragma once

#include <stdint.h>

#include "esp_err.h"
#include "freertos/FreeRTOS.h"
#include "freertos/ringbuf.h"

/**
 * 长录音引擎（ADR-044 P1a）：消费采集 ring buffer → Opus 编码（16kbps CBR+DTX）
 * → 60s 分段写 SD（/sdcard/ambient/<sessionId>/<seq>.opus + index.jsonl）。
 *
 * 生命周期由 bb_radio_app 的 RECORDER 态驱动：进入态时 start（radio_app 负责
 * s_capture_active=1 且 stream_task 不消费 ring），退出态时 stop。
 * PTT 短按打书签（bookmark 索引行，云端解读的高价值锚点）。
 */

/** 启动录音会话。前置：SD 已挂载、rb 有效。失败不留任务。 */
esp_err_t bb_recorder_start(RingbufHandle_t rb);

/** 停止录音：收尾当前段（flush+索引行）后任务退出。阻塞至落盘完成（秒级）。 */
void bb_recorder_stop(void);

/** 1 = 录音会话进行中。 */
int bb_recorder_active(void);

/** PTT 短按打书签（线程安全，可从 stream_task 调）。 */
void bb_recorder_bookmark(void);

/** UI 状态快照。任意指针可 NULL。 */
typedef struct {
  int active;
  int64_t elapsed_ms;   /* 会话累计时长 */
  int segment_count;    /* 已完成段数 */
  int bookmark_count;   /* 已打书签数 */
  uint64_t sd_free_kb;  /* SD 剩余空间 */
  int write_error;      /* 1 = 最近发生写失败（卡满/拔卡） */
} bb_recorder_status_t;
void bb_recorder_get_status(bb_recorder_status_t* out);
