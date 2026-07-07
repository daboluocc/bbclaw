/**
 * ambient 回网补传同步引擎（ADR-044 P1b）。见 bb_ambient_sync.h。
 *
 * 纪律（P1a 实战教训沿袭）：
 * - recorder 的 index.jsonl 是单写者 append-only——本模块只读，进度记独立的
 *   sync.state，拔电安全（最坏重传一段，云端 O_TRUNC 幂等）。
 * - 一切 SD 操作（读/删/f_getfree）只在 bb_recorder_active()==0 时做：
 *   f_getfree 是 FAT 全表扫描且与录音写并发有崩溃前科（bb_recorder.c:180）。
 * - 语音链路（PTT/agent 回合）活跃时整轮让路：补传与 voice 流不共享 WS
 *   二进制通道语义，云端双向互斥会拒绝并发方。
 * - 本任务不碰 NVS/内部 flash（PSRAM 栈任务约束，同 recorder_task）。
 */
#include "bb_ambient_sync.h"

#include <dirent.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

#include "esp_heap_caps.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

#include "bb_adapter_client.h"
#include "bb_config.h"
#include "bb_radio_app.h"
#include "bb_recorder.h"
#include "bb_sdcard.h"
#include "bb_transport.h"
#include "bb_wifi.h"

static const char* TAG = "bb_amb_sync";

#define SYNC_ROOT "/sdcard/ambient"
#define SYNC_POLL_MS 20000
#define SYNC_CHUNK_BYTES 4096
#define SYNC_ACK_TIMEOUT_MS 15000
#define SYNC_LOW_WATER_KB (256U * 1024U) /* 剩余 <256MB 开始回收已上传会话 */
#define SYNC_TASK_STACK 16384            /* 无 opus 编码,纯 IO;照 transport_probe */

/* 每会话上传进度（<dir>/sync.state,一行文本）。缺失/损坏按全未传处理。 */
typedef struct {
  int acked_seq;      /* 已收云端持久化 ack 的最高段号（顺序上传保证水位语义） */
  int sent_bookmarks; /* 已上传书签数（index 内出现序） */
  int stop_sent;      /* 1 = session.stop 已发（会话收尾完成,可回收） */
} sync_state_t;

static bb_ambient_sync_status_t s_status;
static int s_task_started;

static void state_path(char* out, size_t out_len, const char* sess) {
  snprintf(out, out_len, SYNC_ROOT "/%s/sync.state", sess);
}

static void state_load(const char* sess, sync_state_t* st) {
  memset(st, 0, sizeof(*st));
  char path[112];
  state_path(path, sizeof(path), sess);
  FILE* f = fopen(path, "r");
  if (f == NULL) {
    return;
  }
  char line[64] = {0};
  if (fgets(line, sizeof(line), f) != NULL) {
    int a = 0, b = 0, c = 0;
    if (sscanf(line, "v1 %d %d %d", &a, &b, &c) == 3) {
      st->acked_seq = a;
      st->sent_bookmarks = b;
      st->stop_sent = c;
    }
  }
  fclose(f);
}

static int state_save(const char* sess, const sync_state_t* st) {
  char path[112];
  state_path(path, sizeof(path), sess);
  FILE* f = fopen(path, "w");
  if (f == NULL) {
    return -1;
  }
  fprintf(f, "v1 %d %d %d\n", st->acked_seq, st->sent_bookmarks, st->stop_sent);
  fclose(f);
  return 0;
}

/* 补传让路检查：任何一票否决就本轮撤退。段间/块间都要查。 */
static int sync_must_yield(void) {
  return !bb_wifi_is_connected() || bb_recorder_active() || bb_radio_app_voice_busy();
}

/* 中途放弃时清掉云端飞行中的段 writer——否则用户紧接着按 PTT,
 * voice.stream.start 会被云端以 AMBIENT_TRANSFER_ACTIVE 拒掉。 */
static void sync_abort_cloud_segment(const char* sess) {
  char env[224];
  snprintf(env, sizeof(env),
           "{\"type\":\"request\",\"kind\":\"ambient.session.stop\",\"messageId\":\"ambx-%s\","
           "\"deviceId\":\"%s\",\"payload\":{\"sessionId\":\"%s\",\"reason\":\"yield\"}}",
           sess, BBCLAW_DEVICE_ID, sess);
  (void)bb_adapter_client_send_text(env);
}

/* 上传一段:segment.start → 文件字节分块二进制 → arm ack → finish → 等 ack。
 * 返回 0 成功（已 ack）;非 0 失败（下轮重试,云端 O_TRUNC 幂等）。 */
static int upload_segment(const char* sess, int seq, long long t0, long long dur_ms, uint8_t* chunk) {
  char path[112];
  snprintf(path, sizeof(path), SYNC_ROOT "/%s/%06d.opus", sess, seq);
  struct stat stt;
  if (stat(path, &stt) != 0 || stt.st_size <= 0) {
    /* 段文件缺失(被手动删/索引撕裂):跳过并推进水位,别永久卡住整个会话 */
    ESP_LOGW(TAG, "seg missing, skip: %s", path);
    return 0;
  }
  long long bytes = (long long)stt.st_size;
  long long started_ms = (t0 > 1600000000LL) ? t0 * 1000LL : 0;

  char env[288];
  snprintf(env, sizeof(env),
           "{\"type\":\"request\",\"kind\":\"ambient.segment.start\",\"messageId\":\"ambs-%s-%d\","
           "\"deviceId\":\"%s\",\"payload\":{\"sessionId\":\"%s\",\"segSeq\":%d,\"startedAtMs\":%lld}}",
           sess, seq, BBCLAW_DEVICE_ID, sess, seq, started_ms);
  if (bb_adapter_client_send_text(env) != ESP_OK) {
    return -1;
  }

  FILE* f = fopen(path, "rb");
  if (f == NULL) {
    ESP_LOGW(TAG, "seg open failed: %s", path);
    sync_abort_cloud_segment(sess);
    return -1;
  }
  size_t n;
  while ((n = fread(chunk, 1, SYNC_CHUNK_BYTES, f)) > 0) {
    if (bb_adapter_client_send_bin(chunk, n) != ESP_OK) {
      fclose(f);
      sync_abort_cloud_segment(sess);
      return -1;
    }
    if (sync_must_yield()) {
      fclose(f);
      sync_abort_cloud_segment(sess);
      ESP_LOGI(TAG, "yield mid-segment %s/%06d (voice/recorder/offline)", sess, seq);
      return -1;
    }
  }
  int read_err = ferror(f);
  fclose(f);
  if (read_err) {
    sync_abort_cloud_segment(sess);
    return -1;
  }

  bb_adapter_client_ambient_arm_ack(sess, seq);
  snprintf(env, sizeof(env),
           "{\"type\":\"request\",\"kind\":\"ambient.segment.finish\",\"messageId\":\"ambf-%s-%d\","
           "\"deviceId\":\"%s\",\"payload\":{\"sessionId\":\"%s\",\"segSeq\":%d,\"durationMs\":%lld,\"bytes\":%lld}}",
           sess, seq, BBCLAW_DEVICE_ID, sess, seq, dur_ms, bytes);
  if (bb_adapter_client_send_text(env) != ESP_OK) {
    return -1;
  }
  char err[32] = {0};
  if (!bb_adapter_client_ambient_wait_ack(SYNC_ACK_TIMEOUT_MS, err, sizeof(err))) {
    ESP_LOGW(TAG, "seg %s/%06d not acked: %s", sess, seq, err);
    return -1;
  }
  ESP_LOGI(TAG, "seg uploaded %s/%06d bytes=%lld", sess, seq, bytes);
  return 0;
}

/* 处理一个会话目录:按 index.jsonl 顺序补传未 ack 段与书签,索引见 end 且
 * 全部传完则发 session.stop 收尾。返回 0=本会话本轮无失败。 */
static int sync_session(const char* sess, uint8_t* chunk, int* pending_out) {
  char ipath[112];
  snprintf(ipath, sizeof(ipath), SYNC_ROOT "/%s/index.jsonl", sess);
  FILE* f = fopen(ipath, "r");
  if (f == NULL) {
    return 0; /* 无索引(空目录/异物)——不算失败 */
  }
  sync_state_t st;
  state_load(sess, &st);

  int failed = 0;
  int ended = 0;
  int bm_seen = 0;
  char line[224];
  while (!failed && fgets(line, sizeof(line), f) != NULL) {
    const char* pseq = strstr(line, "\"seq\":");
    if (pseq != NULL) {
      int seq = atoi(pseq + 6);
      const char* pt0 = strstr(line, "\"t0\":");
      const char* pdur = strstr(line, "\"dur_ms\":");
      long long t0 = (pt0 != NULL) ? atoll(pt0 + 5) : 0;
      long long dur = (pdur != NULL) ? atoll(pdur + 9) : 0;
      if (seq <= st.acked_seq) {
        continue;
      }
      (*pending_out)++;
      if (sync_must_yield()) {
        failed = 1;
        break;
      }
      if (upload_segment(sess, seq, t0, dur, chunk) != 0) {
        failed = 1;
        break;
      }
      st.acked_seq = seq;
      (void)state_save(sess, &st);
      (*pending_out)--;
      s_status.total_uploaded++;
      continue;
    }
    const char* pbm = strstr(line, "\"bm_ms\":");
    if (pbm != NULL) {
      bm_seen++;
      if (bm_seen <= st.sent_bookmarks) {
        continue;
      }
      char env[224];
      snprintf(env, sizeof(env),
               "{\"type\":\"request\",\"kind\":\"ambient.bookmark\",\"messageId\":\"ambb-%s-%d\","
               "\"deviceId\":\"%s\",\"payload\":{\"sessionId\":\"%s\",\"atMs\":%lld}}",
               sess, bm_seen, BBCLAW_DEVICE_ID, sess, (long long)atoll(pbm + 8));
      if (bb_adapter_client_send_text(env) != ESP_OK) {
        failed = 1;
        break;
      }
      st.sent_bookmarks = bm_seen;
      (void)state_save(sess, &st);
      continue;
    }
    if (strstr(line, "\"end\":1") != NULL) {
      ended = 1;
    }
  }
  fclose(f);

  if (!failed && ended && !st.stop_sent) {
    char env[224];
    snprintf(env, sizeof(env),
             "{\"type\":\"request\",\"kind\":\"ambient.session.stop\",\"messageId\":\"ambe-%s\","
             "\"deviceId\":\"%s\",\"payload\":{\"sessionId\":\"%s\",\"reason\":\"sync_complete\"}}",
             sess, BBCLAW_DEVICE_ID, sess);
    if (bb_adapter_client_send_text(env) == ESP_OK) {
      st.stop_sent = 1;
      (void)state_save(sess, &st);
      ESP_LOGI(TAG, "session %s fully synced", sess);
    }
  }
  return failed;
}

/* 会话目录名 → 排序键。epoch 秒名直接数值;b<bootms>(墙钟未同步)数值小,
 * 排最旧先回收——反正没有可用的真实时间,先回收无损。 */
static unsigned long long session_age_key(const char* name) {
  const char* digits = (name[0] == 'b') ? name + 1 : name;
  return strtoull(digits, NULL, 10);
}

static void remove_session_dir(const char* sess) {
  char dpath[112];
  snprintf(dpath, sizeof(dpath), SYNC_ROOT "/%s", sess);
  DIR* d = opendir(dpath);
  if (d == NULL) {
    return;
  }
  struct dirent* e;
  while ((e = readdir(d)) != NULL) {
    if (strcmp(e->d_name, ".") == 0 || strcmp(e->d_name, "..") == 0) {
      continue;
    }
    char fpath[176];
    /* 目录内都是我们自己写的短名(%06d.opus/index.jsonl/sync.state),%.60s 只为
     * 压 format-truncation 告警 */
    snprintf(fpath, sizeof(fpath), "%s/%.60s", dpath, e->d_name);
    (void)remove(fpath);
  }
  closedir(d);
  (void)rmdir(dpath);
  ESP_LOGI(TAG, "reclaimed session dir %s", sess);
}

/* SD 低水位回收:删最旧的「已收尾(stop_sent)」会话直到回到水位上。
 * 只在录音不活跃时调用(f_getfree 与录音写并发有崩溃前科)。 */
static void cleanup_low_water(void) {
  uint64_t free_kb = 0;
  if (bb_sdcard_space(NULL, &free_kb) != ESP_OK) {
    return;
  }
  while (free_kb < SYNC_LOW_WATER_KB) {
    if (bb_recorder_active()) {
      return;
    }
    char oldest[64] = {0};
    unsigned long long oldest_key = 0;
    DIR* d = opendir(SYNC_ROOT);
    if (d == NULL) {
      return;
    }
    struct dirent* e;
    while ((e = readdir(d)) != NULL) {
      if (e->d_name[0] == '.') {
        continue;
      }
      sync_state_t st;
      state_load(e->d_name, &st);
      if (!st.stop_sent) {
        continue; /* 未传完/未收尾的不回收 */
      }
      unsigned long long key = session_age_key(e->d_name);
      if (oldest[0] == '\0' || key < oldest_key) {
        snprintf(oldest, sizeof(oldest), "%.63s", e->d_name);
        oldest_key = key;
      }
    }
    closedir(d);
    if (oldest[0] == '\0') {
      return; /* 没有可回收的会话了 */
    }
    remove_session_dir(oldest);
    if (bb_sdcard_space(NULL, &free_kb) != ESP_OK) {
      return;
    }
  }
}

static void ambient_sync_task(void* arg) {
  (void)arg;
  uint8_t* chunk = (uint8_t*)heap_caps_malloc(SYNC_CHUNK_BYTES, BBCLAW_MALLOC_CAP_PREFER_PSRAM);
  if (chunk == NULL) {
    ESP_LOGE(TAG, "chunk buffer alloc failed — sync disabled");
    vTaskDeleteWithCaps(NULL);
    return;
  }
  ESP_LOGI(TAG, "ambient sync task up (poll=%ds)", SYNC_POLL_MS / 1000);
  for (;;) {
    vTaskDelay(pdMS_TO_TICKS(SYNC_POLL_MS));
    if (!bb_transport_is_cloud_saas() || !bb_sdcard_mounted() || sync_must_yield()) {
      continue;
    }
    DIR* d = opendir(SYNC_ROOT);
    if (d == NULL) {
      continue; /* 还没录过音 */
    }
    int pending = 0;
    int any_failed = 0;
    struct dirent* e;
    while ((e = readdir(d)) != NULL) {
      if (e->d_name[0] == '.') {
        continue;
      }
      if (sync_must_yield()) {
        any_failed = 1;
        break;
      }
      if (sync_session(e->d_name, chunk, &pending) != 0) {
        any_failed = 1;
      }
    }
    closedir(d);
    s_status.last_round_pending = pending;
    s_status.last_error = any_failed;
    if (!any_failed && !bb_recorder_active()) {
      cleanup_low_water();
    }
  }
}

esp_err_t bb_ambient_sync_start(void) {
  if (s_task_started) {
    return ESP_OK;
  }
#ifdef CONFIG_FREERTOS_UNICORE
  BaseType_t ok = xTaskCreateWithCaps(ambient_sync_task, "bb_amb_sync", SYNC_TASK_STACK, NULL, 4, NULL,
                                      BBCLAW_MALLOC_CAP_PREFER_PSRAM);
#else
  BaseType_t ok = xTaskCreatePinnedToCoreWithCaps(ambient_sync_task, "bb_amb_sync", SYNC_TASK_STACK, NULL, 4, NULL, 0,
                                                  BBCLAW_MALLOC_CAP_PREFER_PSRAM);
#endif
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "sync task create failed");
    return ESP_ERR_NO_MEM;
  }
  s_task_started = 1;
  return ESP_OK;
}

void bb_ambient_sync_get_status(bb_ambient_sync_status_t* out) {
  if (out == NULL) {
    return;
  }
  *out = s_status;
}
