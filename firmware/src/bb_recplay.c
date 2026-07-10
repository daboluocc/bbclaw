/**
 * bb_recplay.c — SD 录音段设备端回放（ADR-044 P1a 附属）。
 *
 * 流程：读整个 .opus 到 PSRAM → bb_ogg_opus 解码为 PCM（PSRAM）→
 * 扬声器播放（bb_audio_play_pcm_blocking,内建打断检查）。
 * 任务栈 40KB PSRAM——libopus USE_ALLOCA,解码路径同样吃深栈（栈账见
 * ADR-044 §3.7,勿削减）。
 *
 * 互斥：录音中(bb_recorder_active)或 TTS 播放中拒绝启动;回放中用户再点
 * 同一文件 = 停止。文件读取发生在本任务(与录音不并发——录音中拒绝启动,
 * FATFS 单用户约束见 2026-07-05 并发竞争实锤)。
 */
#include "bb_recplay.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "bb_audio.h"
#include "bb_config.h"
#include "bb_ogg_opus.h"
#include "bb_recorder.h"
#include "esp_heap_caps.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "freertos/task.h"

static const char* TAG = "bb_recplay";

#define RECPLAY_TASK_STACK 40960
#define RECPLAY_MAX_FILE   (2 * 1024 * 1024) /* 段文件护栏(正常 ~35KB/分钟) */

static volatile int s_active;
static volatile int s_stop_req;
static volatile int s_session_mode; /* 1 = s_path 是会话目录,连播其下所有段 */
static char s_path[128];
static SemaphoreHandle_t s_done_sem;

/* 读+解码+播放单个 .opus(playback 已 start)。返回 0=正常放完(或缺段/坏段跳过),
 * -1=停止请求/致命错(OOM)。段间逐段解码到 PSRAM 再播,复用同一 playback 会话。 */
static int recplay_play_file(const char* path) {
  FILE* f = fopen(path, "rb");
  if (f == NULL) {
    ESP_LOGW(TAG, "open failed (skip): %s", path);
    return 0;
  }
  fseek(f, 0, SEEK_END);
  long sz = ftell(f);
  fseek(f, 0, SEEK_SET);
  if (sz <= 0 || sz > RECPLAY_MAX_FILE) {
    ESP_LOGE(TAG, "bad size %ld (skip)", sz);
    fclose(f);
    return 0;
  }
  uint8_t* ogg = (uint8_t*)heap_caps_malloc((size_t)sz, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
  if (ogg == NULL) {
    fclose(f);
    return -1;
  }
  size_t rd = fread(ogg, 1, (size_t)sz, f);
  fclose(f);
  if (rd != (size_t)sz) {
    ESP_LOGE(TAG, "short read %u/%ld (skip)", (unsigned)rd, sz);
    heap_caps_free(ogg);
    return 0;
  }
  bb_ogg_opus_decoder_t* dec = bb_ogg_opus_decoder_create();
  if (dec == NULL) {
    heap_caps_free(ogg);
    return -1;
  }
  uint8_t* pcm = NULL;
  size_t pcm_len = 0;
  int src_rate = 0, src_ch = 0;
  esp_err_t err = bb_ogg_opus_decoder_decode_all(dec, ogg, (size_t)sz, BBCLAW_AUDIO_SAMPLE_RATE, 1,
                                                 &pcm, &pcm_len, &src_rate, &src_ch);
  bb_ogg_opus_decoder_destroy(dec);
  heap_caps_free(ogg);
  if (err != ESP_OK || pcm == NULL || pcm_len == 0) {
    ESP_LOGE(TAG, "decode failed (skip): %s", esp_err_to_name(err));
    if (pcm != NULL) bb_ogg_opus_free(pcm);
    return 0;
  }
  ESP_LOGI(TAG, "play %s: ogg=%ldB pcm=%uB (%us)", path, sz, (unsigned)pcm_len,
           (unsigned)(pcm_len / (BBCLAW_AUDIO_SAMPLE_RATE * 2)));
  (void)bb_audio_play_pcm_blocking(pcm, pcm_len); /* 打断请求由内部检查 */
  bb_ogg_opus_free(pcm);
  if (s_stop_req || bb_audio_is_playback_interrupt_requested()) return -1;
  return 0;
}

static void recplay_task(void* arg) {
  (void)arg;
  bb_audio_clear_playback_interrupt();
  if (bb_audio_set_playback_sample_rate(BBCLAW_AUDIO_SAMPLE_RATE) == ESP_OK &&
      bb_audio_start_playback() == ESP_OK) {
    if (s_session_mode) {
      /* 段号从 1 连续递增(bb_recorder 保证),缺号即到会话尾 */
      for (int seq = 1; !s_stop_req; seq++) {
        char path[176];
        snprintf(path, sizeof(path), "%s/%06d.opus", s_path, seq);
        FILE* probe = fopen(path, "rb");
        if (probe == NULL) break;
        fclose(probe);
        if (recplay_play_file(path) < 0) break;
      }
    } else {
      (void)recplay_play_file(s_path);
    }
    (void)bb_audio_stop_playback();
  } else {
    ESP_LOGE(TAG, "playback start failed");
  }
  ESP_LOGI(TAG, "done");
  s_active = 0;
  s_session_mode = 0;
  s_path[0] = '\0';
  xSemaphoreGive(s_done_sem);
  vTaskDeleteWithCaps(NULL);
}

esp_err_t bb_recplay_toggle(const char* path) {
  if (path == NULL || path[0] == '\0') return ESP_ERR_INVALID_ARG;
  if (s_active) {
    int same = (strcmp(path, s_path) == 0);
    bb_recplay_stop();
    if (same) return ESP_OK; /* 同一文件再点 = 停止 */
  }
  if (bb_recorder_active()) return ESP_ERR_INVALID_STATE;     /* 录音中不回放 */
  if (bb_audio_is_playback_active()) return ESP_ERR_INVALID_STATE; /* TTS 占用 */

  if (s_done_sem == NULL) s_done_sem = xSemaphoreCreateBinary();
  snprintf(s_path, sizeof(s_path), "%s", path);
  s_session_mode = 0;
  s_stop_req = 0;
  s_active = 1;
  if (xTaskCreateWithCaps(recplay_task, "bb_recplay", RECPLAY_TASK_STACK, NULL, 5, NULL,
                          BBCLAW_MALLOC_CAP_PREFER_PSRAM) != pdPASS) {
    s_active = 0;
    s_path[0] = '\0';
    return ESP_ERR_NO_MEM;
  }
  return ESP_OK;
}

esp_err_t bb_recplay_toggle_session(const char* dir) {
  if (dir == NULL || dir[0] == '\0') return ESP_ERR_INVALID_ARG;
  if (s_active) {
    int same = (s_session_mode && strcmp(dir, s_path) == 0);
    bb_recplay_stop();
    if (same) return ESP_OK; /* 同一会话再点 = 停止 */
  }
  if (bb_recorder_active()) return ESP_ERR_INVALID_STATE;
  if (bb_audio_is_playback_active()) return ESP_ERR_INVALID_STATE;

  if (s_done_sem == NULL) s_done_sem = xSemaphoreCreateBinary();
  snprintf(s_path, sizeof(s_path), "%s", dir);
  s_session_mode = 1;
  s_stop_req = 0;
  s_active = 1;
  if (xTaskCreateWithCaps(recplay_task, "bb_recplay", RECPLAY_TASK_STACK, NULL, 5, NULL,
                          BBCLAW_MALLOC_CAP_PREFER_PSRAM) != pdPASS) {
    s_active = 0;
    s_session_mode = 0;
    s_path[0] = '\0';
    return ESP_ERR_NO_MEM;
  }
  return ESP_OK;
}

void bb_recplay_stop(void) {
  if (!s_active) return;
  s_stop_req = 1; /* 会话模式:让段循环在当前段放完/被打断后立即收尾 */
  bb_audio_request_playback_interrupt();
  (void)xSemaphoreTake(s_done_sem, pdMS_TO_TICKS(5000));
}

int bb_recplay_active(void) { return s_active; }

const char* bb_recplay_current(void) { return s_active ? s_path : ""; }
