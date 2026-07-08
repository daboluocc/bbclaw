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
#include <math.h>
#include <string.h>
#include <unistd.h>
#include <sys/stat.h>
#include <time.h>

#include "bb_audio.h"
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
/* 40KB(对齐 BB_STREAM_TASK_STACK):libopus 是 USE_ALLOCA 编译,一次完整
 * SILK 编码 alloca ~23-24KB 任务栈(反汇编实测,silk_encode_frame_FIX 一层
 * 就 9.5KB+动态)。24KB 曾差 400B 溢出——PSRAM 栈溢出不 fault,直接写烂
 * 邻居堆数据,死点飘忽(P1a 60s 崩溃全案根因之一)。PSRAM 栈不心疼。 */
#define REC_TASK_STACK    49152  /* 48KB:40KB 在安静音频下余 14.5KB,有人声
 * (voiced,SILK pitch stage-3 更深)仍见 crumb=8 随机 panic——再加 8KB 保险;
 * PSRAM 栈,成本可忽略。结构性根治=libopus 伪栈化(ADR-044 P3) */
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
  int64_t session_bytes;   /* 会话累计已写字节(推导 SD 剩余用) */
  uint64_t sd_free_kb0;    /* 会话起点 SD 剩余(仅此一查,避免与写入并发碰 FATFS) */
  FILE* seg_fp;
  bb_ogg_opus_encoder_t* enc;
  SemaphoreHandle_t done_sem; /* 任务收尾完成信号 */
} rec_state_t;

static rec_state_t s_rec;

/* ── 录音 AGC(向上式 + 噪声门 + 软限幅)────────────────────────────────
 * ambient 声源远近漂,固定增益无法兼顾(真机段间 RMS -41~-59dBFS)。向上式 AGC:
 * 只把安静段抬起来、不动已经足够响的瞬态(不引入新削顶);噪声门防静音段泵噪;
 * 软限幅 + clamp 兜底。参数在你拉回的真实录音离线原型上调定并验证
 * (000229 RMS -50→-29dB;000230 峰值 -0.4dB 原样保留、零硬削顶)。状态整会话持有,
 * 段轮转不重置——跨段增益连续,避免每段开头重新爬坡。作用在双麦均值之后。 */
#define REC_AGC_TARGET      (0.28f * 32768.0f)   /* 目标包络 ≈ -11 dBFS */
#define REC_AGC_NOISE_FLOOR (0.0018f * 32768.0f) /* 门限 ≈ -55 dBFS:以下视为噪声不抬 */
#define REC_AGC_MAX_GAIN    22.0f                 /* 增益上限 ≈ +27 dB */
#define REC_AGC_LIMIT       (0.95f * 32768.0f)    /* 软限幅 ≈ -0.4 dBFS */

static struct {
  float env;   /* 信号包络,跟随 |x| */
  float gain;  /* 平滑后的实际增益 */
  float env_atk, env_rel; /* 包络 attack/release 一阶系数 */
  float gain_up, gain_dn; /* 增益上爬(慢,抗泵)/ 下降(快,抗削)系数 */
} s_agc;

/* coef = expf(-1/(tau*fs));fs=16k。attack 3ms / release 250ms / 上爬 1.5s / 下降 10ms */
static void rec_agc_reset(void) {
  const float fs = (float)BBCLAW_AUDIO_SAMPLE_RATE;
  s_agc.env = 0.0f;
  s_agc.gain = 1.0f;
  s_agc.env_atk = expf(-1.0f / (0.003f * fs));
  s_agc.env_rel = expf(-1.0f / (0.250f * fs));
  s_agc.gain_up = expf(-1.0f / (1.500f * fs));
  s_agc.gain_dn = expf(-1.0f / (0.010f * fs));
}

static void rec_agc_process(int16_t* buf, size_t n) {
  float env = s_agc.env, gain = s_agc.gain;
  for (size_t i = 0; i < n; ++i) {
    float x = (float)buf[i];
    float a = fabsf(x);
    if (a > env) env = s_agc.env_atk * env + (1.0f - s_agc.env_atk) * a; /* fast attack */
    else         env = s_agc.env_rel * env + (1.0f - s_agc.env_rel) * a; /* slow release */
    float desired = 1.0f;
    if (env > REC_AGC_NOISE_FLOOR) {
      desired = REC_AGC_TARGET / env;
      if (desired < 1.0f) desired = 1.0f;              /* 只抬不压:已够响的不动 */
      if (desired > REC_AGC_MAX_GAIN) desired = REC_AGC_MAX_GAIN;
    }
    if (desired < gain) gain = s_agc.gain_dn * gain + (1.0f - s_agc.gain_dn) * desired; /* 降快 */
    else                gain = s_agc.gain_up * gain + (1.0f - s_agc.gain_up) * desired; /* 升慢 */
    float y = x * gain;
    if (y > REC_AGC_LIMIT) y = REC_AGC_LIMIT;
    else if (y < -REC_AGC_LIMIT) y = -REC_AGC_LIMIT;
    int32_t iy = (int32_t)lrintf(y);
    if (iy > 32767) iy = 32767;
    else if (iy < -32768) iy = -32768;
    buf[i] = (int16_t)iy;
  }
  s_agc.env = env;
  s_agc.gain = gain;
}

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
  /* ⚠️ 不设 CBR——真凶终审:OPUS_SET_VBR(0)(CBR)让 opus_encode 栈消耗
   * 暴涨 ~12KB(SILK CBR 多趟编码栈缓冲),16KB 栈直接溢出 PANIC(死点飘忽=
   * 栈被打烂);24KB 栈也仅剩 408B。默认 VBR 实测轮转栈深 ~12KB,稳定。
   * auto 码率 ≈20-25kbps(9-11MB/h),存储账可接受(ADR-044 §3.3 修订)。 */
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
    s_rec.session_bytes += (int64_t)len;
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
    ESP_LOGI(TAG, "seg %d closed dur=%llds bytes=%lld (%.1fkbps)", s_rec.seg_seq,
             (long long)s_rec.seg_pcm_ms / 1000, (long long)s_rec.seg_bytes,
             s_rec.seg_pcm_ms > 0 ? (double)s_rec.seg_bytes * 8.0 / (double)s_rec.seg_pcm_ms : 0.0);
  }
}

static void recorder_task(void* arg) {
  (void)arg;
  /* 崩溃排障仪表:本板 panic 输出不可达(UART0 关闭+TinyUSB 占 USB),
   * 复位原因+周期健康指标是唯一线索。 */
  ESP_LOGW(TAG, "session start dir=%s (last_reset_reason=%d last_crumb=%d)", s_rec.dir,
           (int)esp_reset_reason(), s_rec_crumb);
  CRUMB(8);
  /* SD 剩余只在会话起点查一次:f_getfree 是 FAT32 全表扫描(秒级)且
   * esp_vfs_fat_info 绕过 VFS 锁——录音中从 UI 任务并发调用会与写入
   * 竞争 FATFS(真机 7s 崩溃实锤)。之后 UI 显示用「初值-已写」推导。 */
  (void)bb_sdcard_space(NULL, &s_rec.sd_free_kb0);
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

    /* 录音 AGC:就地处理 ring 内 PCM(recorder 是本 ring 唯一消费者,返还前独占)。
     * 作用在双麦均值之后、Opus 编码之前。 */
    rec_agc_process((int16_t*)item, item_size / sizeof(int16_t));

    uint8_t* out = NULL;
    size_t out_len = 0;
    CRUMB(30); /* 30=append(opus_encode)中 31=写SD中 8=其它 */
    esp_err_t err = bb_ogg_opus_encoder_append_pcm16(s_rec.enc, (const int16_t*)item,
                                                     item_size / sizeof(int16_t), &out, &out_len);
    CRUMB(8);
    vRingbufferReturnItem(s_rec.rb, item);
    if (err == ESP_OK && out != NULL) {
      CRUMB(31);
      segment_write(out, out_len);
      CRUMB(8);
      bb_ogg_opus_free(out);
    } else if (err != ESP_OK) {
      /* 编码失败不可静默(P1a 教训:静默吞错让"整段只录 60ms"隐身了一整天) */
      ESP_LOGE(TAG, "append_pcm16 failed: %s", esp_err_to_name(err));
      s_rec.write_error = 1;
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
  rec_agc_reset();                 /* AGC 状态整会话持有,起点归零 */
  bb_audio_set_recorder_mix(1);    /* 采集侧切双麦均值(对话态退出后由 stop 复位) */
  s_rec.run = 1;
  s_rec.active = 1;
  BaseType_t ok = xTaskCreateWithCaps(recorder_task, "bb_recorder", REC_TASK_STACK, NULL, 6, NULL,
                                      BBCLAW_MALLOC_CAP_PREFER_PSRAM);
  if (ok != pdPASS) {
    s_rec.run = 0;
    s_rec.active = 0;
    bb_audio_set_recorder_mix(0);  /* 任务没起来,回滚采集模式 */
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
  bb_audio_set_recorder_mix(0);    /* 恢复对话/PTT 的挑响一路语义 */
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
  /* 推导值:起点一查+已写字节(禁止在此碰 FATFS——与录音写入并发会崩) */
  const uint64_t used_kb = (uint64_t)(s_rec.session_bytes / 1024);
  out->sd_free_kb = (s_rec.sd_free_kb0 > used_kb) ? (s_rec.sd_free_kb0 - used_kb) : 0;
}
