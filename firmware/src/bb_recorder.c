/**
 * bb_recorder.c — 长录音引擎（ADR-044 P1a：SD 本地优先，回网补传在 P1b）。
 *
 * 数据流：capture_task(既有) → ring buffer → recorder_task(本模块,PSRAM 栈)
 *          → bb_ogg_opus (16kbps CBR+DTX) → /sdcard/ambient/<sid>/<seq>.opus
 *
 * 分段：60s 一段（绕开云端整段内存约束,断网重传粒度小）。每段收尾在
 * index.jsonl 追加一行 {"seq","t0","boot0","dur_ms","bytes","st":"pend"}；
 * 书签行 {"bm_ms":<会话内毫秒>,"t":<epoch>}。追加式索引:拔电最多丢当前
 * 未收尾段的索引（文件本体已在盘上,P1b 补传时可扫描恢复）。
 *
 * 任务栈在 PSRAM（xTaskCreateWithCaps）：本任务不碰 NVS/内部 flash（SD 走
 * SDMMC 外设,与内部 flash cache 无关）,符合 PSRAM 栈任务约束。
 */
#include "bb_recorder.h"

#include <errno.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>
#include <sys/stat.h>
#include <time.h>

#include "bb_config.h"
#include "bb_ogg_opus.h"
#include "bb_sdcard.h"
#include "bb_time.h"
#include "esp_heap_caps.h"
#include "esp_log.h"
#include "esp_attr.h"
#include "esp_system.h"
#include "freertos/semphr.h"
#include "freertos/task.h"

static const char* TAG = "bb_recorder";

#define REC_BITRATE_BPS   16000
#define REC_SEGMENT_MS    60000
#define REC_TASK_STACK    16384
#define REC_DIR           "/sdcard/ambient"
/* 16kHz mono PCM16: 32 bytes / ms */
#define PCM_BYTES_PER_MS  32

typedef struct {
  RingbufHandle_t rb;
  volatile int run;        /* 0 → 任务收尾退出 */
  volatile int active;     /* 会话进行中（含收尾前） */
  volatile int write_error;
  volatile int bookmark_pending; /* 待写书签数（PTT 短按累加） */
  volatile int bookmark_count;   /* 已落盘书签数（UI 展示） */
  char dir[64];            /* /sdcard/ambient/<sid> */
  int64_t session_boot_ms; /* 会话起点（boot 时钟） */
  int seg_seq;             /* 当前段号（从 1 起） */
  int seg_done;            /* 已完成段数 */
  int64_t seg_pcm_ms;      /* 当前段累计 PCM 时长 */
  int64_t seg_t0_epoch;    /* 当前段起点墙钟（秒,未同步时为小值） */
  int64_t seg_boot0_ms;    /* 当前段起点 boot 时钟 */
  int64_t seg_bytes;       /* 当前段已写字节 */
  FILE* seg_fp;
  bb_ogg_opus_encoder_t* enc;
  SemaphoreHandle_t done_sem; /* 任务收尾完成信号 */
} rec_state_t;

static rec_state_t s_rec;

/* 崩溃面包屑(RTC noinit,软复位存活):段轮转路径每步打点,重启后
 * session start 日志读出死亡位置——本板 panic 输出不可达的替代品。 */
RTC_NOINIT_ATTR static int s_rec_crumb;
#define CRUMB(n) do { s_rec_crumb = (n); } while (0)
/* 1=close:flush 2=close:write 3=close:destroy 4=close:fclose 5=close:index
 * 6=open:fopen 7=open:enc_create 8=running 0=idle */

/* 索引行追加（O_APPEND 语义;单写者=recorder_task,无并发） */
static void index_append(const char* line) {
  char path[96];
  snprintf(path, sizeof(path), "%s/index.jsonl", s_rec.dir);
  FILE* f = fopen(path, "a");
  if (f == NULL) {
    s_rec.write_error = 1;
    return;
  }
  fputs(line, f);
  fputc('\n', f);
  fclose(f);
}

static int segment_open(void) {
  char path[96];
  snprintf(path, sizeof(path), "%s/%06d.opus", s_rec.dir, s_rec.seg_seq);
  CRUMB(6);
  s_rec.seg_fp = fopen(path, "wb");
  if (s_rec.seg_fp == NULL) {
    ESP_LOGE(TAG, "segment open failed: %s", path);
    s_rec.write_error = 1;
    return -1;
  }
  CRUMB(7);
  s_rec.enc = bb_ogg_opus_encoder_create(BBCLAW_AUDIO_SAMPLE_RATE, 1, 60);
  if (s_rec.enc == NULL) {
    ESP_LOGE(TAG, "encoder create failed");
    fclose(s_rec.seg_fp);
    s_rec.seg_fp = NULL;
    return -1;
  }
  /* 排障:crumb=1=flush 内 PANIC。DTX 已排除;现去掉整个 set_bitrate
   * (与天天跑的 PTT 编码器完全同配置)验证 CBR/VBR(0) 是否元凶。
   * auto 码率 ≈19-25kbps,存储账仍可接受。 */
  /* (void)bb_ogg_opus_encoder_set_bitrate(s_rec.enc, REC_BITRATE_BPS, 0); */
  CRUMB(8);
  s_rec.seg_pcm_ms = 0;
  s_rec.seg_bytes = 0;
  s_rec.seg_t0_epoch = (int64_t)time(NULL);
  s_rec.seg_boot0_ms = bb_now_ms();
  return 0;
}

static void segment_write(const uint8_t* data, size_t len) {
  if (s_rec.seg_fp == NULL || data == NULL || len == 0) return;
  if (fwrite(data, 1, len, s_rec.seg_fp) != len) {
    ESP_LOGE(TAG, "segment write failed (card full/removed?)");
    s_rec.write_error = 1;
  } else {
    s_rec.seg_bytes += (int64_t)len;
  }
}

static void segment_close(void) {
  if (s_rec.enc != NULL) {
    uint8_t* out = NULL;
    size_t out_len = 0;
    CRUMB(10);
    (void)heap_caps_check_integrity_all(true); /* 轮转前哨兵:crumb=10 崩=append 阶段已坏 */
    CRUMB(1);
    if (bb_ogg_opus_encoder_flush(s_rec.enc, &out, &out_len) == ESP_OK && out != NULL) {
      CRUMB(2);
      segment_write(out, out_len);
      bb_ogg_opus_free(out);
    }
    CRUMB(3);
    bb_ogg_opus_encoder_destroy(s_rec.enc);
    s_rec.enc = NULL;
  }
  if (s_rec.seg_fp != NULL) {
    CRUMB(4);
    fclose(s_rec.seg_fp);
    s_rec.seg_fp = NULL;
  }
  CRUMB(5);
  if (s_rec.seg_pcm_ms > 0) {
    char line[192];
    snprintf(line, sizeof(line),
             "{\"seq\":%d,\"t0\":%lld,\"boot0\":%lld,\"dur_ms\":%lld,\"bytes\":%lld,\"st\":\"pend\"}",
             s_rec.seg_seq, (long long)s_rec.seg_t0_epoch, (long long)s_rec.seg_boot0_ms,
             (long long)s_rec.seg_pcm_ms, (long long)s_rec.seg_bytes);
    index_append(line);
    s_rec.seg_done++;
  }
}

static void recorder_task(void* arg) {
  (void)arg;
  /* 崩溃排障仪表:本板 panic 输出不可达(UART0 关闭+TinyUSB 占 USB),
   * 复位原因+周期健康指标是唯一线索。 */
  ESP_LOGW(TAG, "session start dir=%s (last_reset_reason=%d last_crumb=%d)", s_rec.dir,
           (int)esp_reset_reason(), s_rec_crumb);
  CRUMB(8);
  if (segment_open() != 0) {
    s_rec.run = 0;
  }
  while (s_rec.run) {
    /* 书签：写入会话内相对时刻（毫秒） */
    while (s_rec.bookmark_pending > 0) {
      s_rec.bookmark_pending--;
      char line[96];
      snprintf(line, sizeof(line), "{\"bm_ms\":%lld,\"t\":%lld}",
               (long long)(bb_now_ms() - s_rec.session_boot_ms), (long long)time(NULL));
      index_append(line);
      s_rec.bookmark_count++;
      ESP_LOGI(TAG, "bookmark @%llds", (long long)(bb_now_ms() - s_rec.session_boot_ms) / 1000);
    }

    size_t item_size = 0;
    uint8_t* item = (uint8_t*)xRingbufferReceive(s_rec.rb, &item_size, pdMS_TO_TICKS(100));
    if (item == NULL) continue;

    uint8_t* out = NULL;
    size_t out_len = 0;
    esp_err_t err = bb_ogg_opus_encoder_append_pcm16(s_rec.enc, (const int16_t*)item,
                                                     item_size / sizeof(int16_t), &out, &out_len);
    vRingbufferReturnItem(s_rec.rb, item);
    if (err == ESP_OK && out != NULL) {
      segment_write(out, out_len);
      bb_ogg_opus_free(out);
    }
    s_rec.seg_pcm_ms += (int64_t)item_size / PCM_BYTES_PER_MS;

    /* 周期 fsync(每 ~5s):FATFS 无日志,录音中拔电/重启会留脏 FAT(真机踩过:
     * 中途烧录重启后整卡写路径 EIO)。fsync 提交 FAT,最多丢几秒尾巴。 */
    static int64_t s_last_sync_ms;
    if (s_rec.seg_fp != NULL && s_rec.seg_pcm_ms - s_last_sync_ms >= 5000) {
      s_last_sync_ms = s_rec.seg_pcm_ms;
      (void)fflush(s_rec.seg_fp);
      (void)fsync(fileno(s_rec.seg_fp));
    }
    if (s_rec.seg_pcm_ms < s_last_sync_ms) s_last_sync_ms = 0; /* 段轮转后复位 */

    /* 健康指标(每 10s):内部堆水位/最大块 + 本任务栈余量——崩溃前最后一组
     * 数字即嫌疑方向(OOM/栈溢出)。 */
    static int64_t s_last_health_ms;
    if (s_rec.seg_pcm_ms - s_last_health_ms >= 10000 || s_rec.seg_pcm_ms < s_last_health_ms) {
      s_last_health_ms = s_rec.seg_pcm_ms;
      ESP_LOGI(TAG, "health: pcm=%llds int_free=%u int_largest=%u stack_hwm=%u",
               (long long)s_rec.seg_pcm_ms / 1000,
               (unsigned)heap_caps_get_free_size(MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT),
               (unsigned)heap_caps_get_largest_free_block(MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT),
               (unsigned)uxTaskGetStackHighWaterMark(NULL));
      /* 堆完整性哨兵:crumb=9+第N次检查。崩在这=损坏发生于上一个 10s 窗口内
       * (append/fwrite 阶段),不在轮转;print_errors=true 但本板 panic 不可见,
       * 靠 crumb 值判断。 */
      static int s_check_n;
      CRUMB(90 + (s_check_n % 9));
      s_check_n++;
      (void)heap_caps_check_integrity_all(true);
      CRUMB(8);
    }

    /* 段轮转 */
    if (s_rec.seg_pcm_ms >= REC_SEGMENT_MS) {
      segment_close();
      s_rec.seg_seq++;
      if (segment_open() != 0) {
        s_rec.run = 0; /* 打不开新段（卡满/拔卡）→ 结束会话,已录内容保住 */
        break;
      }
    }
  }
  segment_close();
  index_append("{\"end\":1}");
  ESP_LOGI(TAG, "session end: %d segments, last_err=%d", s_rec.seg_done, s_rec.write_error);
  s_rec.active = 0;
  xSemaphoreGive(s_rec.done_sem);
  vTaskDeleteWithCaps(NULL);
}

int bb_recorder_debug_crumb(void) { return s_rec_crumb; }

esp_err_t bb_recorder_start(RingbufHandle_t rb) {
  if (s_rec.active) return ESP_ERR_INVALID_STATE;
  if (!bb_sdcard_mounted() || rb == NULL) return ESP_ERR_INVALID_STATE;

  SemaphoreHandle_t sem = s_rec.done_sem; /* 跨会话保留,防 memset 后重建泄漏 */
  memset(&s_rec, 0, sizeof(s_rec));
  s_rec.done_sem = sem;
  s_rec.rb = rb;
  s_rec.session_boot_ms = bb_now_ms();
  s_rec.seg_seq = 1;

  /* 会话目录：墙钟已同步用 epoch 秒,否则 boot 毫秒加 b 前缀（避免撞名） */
  const int64_t now_epoch = (int64_t)time(NULL);
  errno = 0;
  if (mkdir(REC_DIR, 0775) != 0 && errno != EEXIST) {
    ESP_LOGW(TAG, "mkdir %s: errno=%d(%s)", REC_DIR, errno, strerror(errno));
  }
  if (now_epoch > 1600000000LL) {
    snprintf(s_rec.dir, sizeof(s_rec.dir), REC_DIR "/%lld", (long long)now_epoch);
  } else {
    snprintf(s_rec.dir, sizeof(s_rec.dir), REC_DIR "/b%lld", (long long)s_rec.session_boot_ms);
  }
  errno = 0;
  if (mkdir(s_rec.dir, 0775) != 0) {
    ESP_LOGE(TAG, "mkdir %s failed errno=%d(%s)", s_rec.dir, errno, strerror(errno));
    return ESP_FAIL;
  }

  if (s_rec.done_sem == NULL) s_rec.done_sem = xSemaphoreCreateBinary();
  s_rec.run = 1;
  s_rec.active = 1;
  BaseType_t ok = xTaskCreateWithCaps(recorder_task, "bb_recorder", REC_TASK_STACK, NULL, 6, NULL,
                                      BBCLAW_MALLOC_CAP_PREFER_PSRAM);
  if (ok != pdPASS) {
    s_rec.run = 0;
    s_rec.active = 0;
    ESP_LOGE(TAG, "task create failed");
    return ESP_ERR_NO_MEM;
  }
  return ESP_OK;
}

void bb_recorder_stop(void) {
  if (!s_rec.active) return;
  s_rec.run = 0;
  /* 等任务收尾落盘（正常毫秒级;给 5s 兜底防卡死等待） */
  (void)xSemaphoreTake(s_rec.done_sem, pdMS_TO_TICKS(5000));
}

int bb_recorder_active(void) { return s_rec.active; }

void bb_recorder_bookmark(void) {
  if (s_rec.active) s_rec.bookmark_pending++;
}

void bb_recorder_get_status(bb_recorder_status_t* out) {
  if (out == NULL) return;
  out->active = s_rec.active;
  out->elapsed_ms = s_rec.active ? (bb_now_ms() - s_rec.session_boot_ms) : 0;
  out->segment_count = s_rec.seg_done;
  out->bookmark_count = s_rec.bookmark_count;
  out->write_error = s_rec.write_error;
  out->sd_free_kb = 0;
  (void)bb_sdcard_space(NULL, &out->sd_free_kb);
}
