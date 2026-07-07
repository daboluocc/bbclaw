#pragma once

#include "esp_err.h"

/**
 * ambient 回网补传同步引擎（ADR-044 P1b）。
 *
 * 后台任务轮询：在网(cloud_saas + WiFi + WS) 且 录音不活跃 且 语音链路空闲时，
 * 扫 /sdcard/ambient/<session>/index.jsonl，把未上传段按序补传云端
 * (ambient.segment.start → 文件字节二进制帧 → segment.finish → 等 ack)。
 *
 * 上传进度记在每会话目录 sync.state（"v1 <acked_seq> <sent_bookmarks> <stop_sent>"），
 * 不改写 recorder 的 append-only index.jsonl（单写者纪律）。ack=云端已持久化，
 * 才推进高水位；掉电/掉线最多重传一段（云端 O_TRUNC 幂等）。
 *
 * SD 低水位（剩余 < 256MB）时从最旧的「已全部上传且已收尾」会话开始整目录回收。
 */

/** 创建后台同步任务（幂等）。SD/网络不可用时任务空转,无需前置条件。 */
esp_err_t bb_ambient_sync_start(void);

/** 状态快照(UI/日志用)。任意指针可 NULL。 */
typedef struct {
  int last_round_pending;   /* 上一轮扫描时待传段数 */
  int total_uploaded;       /* 本次开机以来已 ack 段数 */
  int last_error;           /* 1 = 上一轮有上传失败 */
} bb_ambient_sync_status_t;
void bb_ambient_sync_get_status(bb_ambient_sync_status_t* out);
