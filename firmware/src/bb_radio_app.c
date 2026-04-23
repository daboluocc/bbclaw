#include "bb_radio_app.h"

#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <strings.h>

#include "bb_adapter_client.h"
#include "bb_status.h"
#include "bb_audio.h"
#include "bb_config.h"
#include "bb_display.h"
#include "bb_gateway_node.h"
#include "bb_button_test.h"
#include "bb_led.h"
#include "bb_motor.h"
#include "bb_nav_input.h"
#include "bb_ogg_opus.h"
#include "bb_power.h"
#include "bb_ptt.h"
#include "bb_time.h"
#include "bb_transport.h"
#include "bb_wifi.h"
#include "bb_xl9555.h"
#include "bb_ota.h"
#include "esp_err.h"
#include "esp_heap_caps.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/queue.h"
#include "freertos/ringbuf.h"
#include "freertos/task.h"

static const char* TAG = "bb_radio_app";

#if BBCLAW_SPK_TEST_ON_BOOT
extern const uint8_t _binary_bbclaw_wav_start[] asm("_binary_bbclaw_wav_start");
extern const uint8_t _binary_bbclaw_wav_end[] asm("_binary_bbclaw_wav_end");
#endif

/** ESP_LOG 单行缓冲有限，长 UTF-8 回复分多条打印 */
#define BB_LOG_TEXT_CHUNK 400
#define BB_TTS_STREAM_QUEUE_DEPTH 128
#define BB_TTS_STREAM_TASK_STACK 6144
#define BB_STREAM_TASK_STACK 12288

#if BBCLAW_SPK_TEST_ON_BOOT
static uint16_t read_le16(const uint8_t* p) {
  return (uint16_t)p[0] | ((uint16_t)p[1] << 8);
}

static uint32_t read_le32(const uint8_t* p) {
  return (uint32_t)p[0] | ((uint32_t)p[1] << 8) | ((uint32_t)p[2] << 16) | ((uint32_t)p[3] << 24);
}

static esp_err_t bb_play_embedded_boot_wav(void) {
  const uint8_t* wav = _binary_bbclaw_wav_start;
  const size_t wav_len = (size_t)(_binary_bbclaw_wav_end - _binary_bbclaw_wav_start);
  const uint8_t* data = NULL;
  size_t data_len = 0U;
  uint16_t audio_format = 0U;
  uint16_t channels = 0U;
  uint16_t bits_per_sample = 0U;
  uint32_t sample_rate = 0U;

  if (wav_len < 44U) {
    ESP_LOGW(TAG, "boot wav too small len=%u", (unsigned)wav_len);
    return ESP_ERR_INVALID_SIZE;
  }
  if (memcmp(wav, "RIFF", 4) != 0 || memcmp(wav + 8, "WAVE", 4) != 0) {
    ESP_LOGW(TAG, "boot wav invalid riff header");
    return ESP_ERR_INVALID_RESPONSE;
  }

  for (size_t off = 12U; off + 8U <= wav_len;) {
    const uint8_t* chunk = wav + off;
    uint32_t chunk_size = read_le32(chunk + 4);
    size_t payload_off = off + 8U;
    size_t next_off = payload_off + chunk_size + (chunk_size & 1U);
    if (next_off > wav_len) {
      ESP_LOGW(TAG, "boot wav chunk truncated off=%u size=%u", (unsigned)off, (unsigned)chunk_size);
      return ESP_ERR_INVALID_SIZE;
    }
    if (memcmp(chunk, "fmt ", 4) == 0) {
      if (chunk_size < 16U) {
        ESP_LOGW(TAG, "boot wav fmt chunk too small size=%u", (unsigned)chunk_size);
        return ESP_ERR_INVALID_SIZE;
      }
      audio_format = read_le16(wav + payload_off);
      channels = read_le16(wav + payload_off + 2U);
      sample_rate = read_le32(wav + payload_off + 4U);
      bits_per_sample = read_le16(wav + payload_off + 14U);
    } else if (memcmp(chunk, "data", 4) == 0) {
      data = wav + payload_off;
      data_len = chunk_size;
    }
    off = next_off;
  }

  if (audio_format != 1U || channels != 1U || bits_per_sample != 16U || sample_rate == 0U || data == NULL || data_len == 0U) {
    ESP_LOGW(TAG,
             "boot wav unsupported format=%u ch=%u bits=%u rate=%u data_len=%u",
             (unsigned)audio_format,
             (unsigned)channels,
             (unsigned)bits_per_sample,
             (unsigned)sample_rate,
             (unsigned)data_len);
    return ESP_ERR_NOT_SUPPORTED;
  }
  if ((data_len % sizeof(int16_t)) != 0U) {
    ESP_LOGW(TAG, "boot wav payload not aligned len=%u", (unsigned)data_len);
    return ESP_ERR_INVALID_SIZE;
  }

  esp_err_t err = bb_audio_set_playback_sample_rate((int)sample_rate);
  if (err != ESP_OK) {
    ESP_LOGW(TAG, "set boot wav sample rate failed err=%s", esp_err_to_name(err));
    return err;
  }
  int expected_ms = 0;
  if (sample_rate > 0U && channels > 0U && bits_per_sample > 0U) {
    expected_ms = (int)(((uint64_t)data_len * 1000ULL) / (((uint64_t)sample_rate * (uint64_t)channels * (uint64_t)bits_per_sample) / 8ULL));
  }
  ESP_LOGI(TAG, "boot wav play len=%u rate=%u ch=%u bits=%u", (unsigned)data_len, (unsigned)sample_rate,
           (unsigned)channels, (unsigned)bits_per_sample);
  int64_t t0_ms = bb_now_ms();
  err = bb_audio_play_pcm_blocking(data, data_len);
  int actual_ms = (int)(bb_now_ms() - t0_ms);
  int ratio_x100 = expected_ms > 0 ? (actual_ms * 100) / expected_ms : 0;
  ESP_LOGI(TAG, "boot wav done expected_ms=%d actual_ms=%d ratio_x100=%d err=%s", expected_ms, actual_ms, ratio_x100,
           esp_err_to_name(err));
  esp_err_t restore_err = bb_audio_set_playback_sample_rate(BBCLAW_AUDIO_SAMPLE_RATE);
  if (restore_err != ESP_OK) {
    ESP_LOGW(TAG, "restore playback sample rate failed err=%s", esp_err_to_name(restore_err));
    return restore_err;
  }
  return err;
}
#endif

static void log_phase_text_chunks(const char* phase, const char* text) {
  if (text == NULL) {
    return;
  }
  size_t len = strlen(text);
  if (len == 0U) {
    ESP_LOGI(TAG, "%s (empty)", phase);
    return;
  }
  if (len <= BB_LOG_TEXT_CHUNK) {
    ESP_LOGI(TAG, "%s %s", phase, text);
    return;
  }
  size_t total_parts = (len + BB_LOG_TEXT_CHUNK - 1U) / BB_LOG_TEXT_CHUNK;
  size_t part = 0;
  for (size_t off = 0; off < len; off += BB_LOG_TEXT_CHUNK) {
    size_t n = BB_LOG_TEXT_CHUNK;
    if (off + n > len) {
      n = len - off;
    }
    char buf[BB_LOG_TEXT_CHUNK + 4];
    if (n > sizeof(buf) - 1U) {
      n = sizeof(buf) - 1U;
    }
    memcpy(buf, text + off, n);
    buf[n] = '\0';
    part++;
    ESP_LOGI(TAG, "%s part=%u/%u %s", phase, (unsigned)part, (unsigned)total_parts, buf);
  }
}
static volatile int s_ptt_pressed;
static volatile unsigned s_ptt_change_version;
static volatile unsigned s_nav_event_versions[BB_NAV_EVENT_COUNT];
static volatile int s_transport_health_ok;
static volatile int s_transport_ready;
static volatile int s_transport_audio_streaming_ready;
static volatile int s_transport_tts_ready;
static volatile int s_transport_display_ready;
static volatile int s_tts_playback_active;
static volatile int s_tts_interrupt_requested;
static int s_transport_http_status;
static char s_transport_detail[64];
static char s_transport_registration_code[16];
static char s_transport_registration_expires_at[40];
static bb_transport_pairing_status_t s_transport_pairing_status;
#if BBCLAW_ENABLE_TTS_PLAYBACK
static char s_spoken_registration_code[16];
#endif
static uint8_t s_stream_task_pcm_read_buf[1024];
static uint8_t s_stream_pcm_chunk_buf[(BBCLAW_AUDIO_SAMPLE_RATE * BBCLAW_STREAM_CHUNK_MS / 1000) * sizeof(int16_t) *
                                      BBCLAW_AUDIO_CHANNELS];
static uint8_t s_stream_encoded_chunk_buf[sizeof(s_stream_pcm_chunk_buf) + 64];

/*
 * Ring buffer for decoupling I2S capture from HTTP upload.
 * capture_task reads I2S at high priority and pushes PCM into the ring buffer.
 * stream_task pulls from the ring buffer and does (slow) HTTP uploads.
 * This prevents I2S DMA overflow when upload is slow (e.g. low bandwidth).
 *
 * Size: 8 chunks worth of PCM ≈ 80KB, covers ~2.5s of upload latency.
 */
#ifndef BBCLAW_CAPTURE_RINGBUF_CHUNKS
#define BBCLAW_CAPTURE_RINGBUF_CHUNKS 8
#endif
#define CAPTURE_RINGBUF_SIZE (sizeof(s_stream_pcm_chunk_buf) * BBCLAW_CAPTURE_RINGBUF_CHUNKS)

static RingbufHandle_t s_capture_rb;
static volatile int s_capture_active;  /* 1 = capture_task should read I2S and push to ring buffer */
static volatile int s_capture_stopped; /* 1 = capture_task has acknowledged stop */
static volatile bb_radio_app_state_t s_app_state = BBCLAW_STATE_UNLOCKED;
static uint8_t* s_voice_verify_pcm_buf;
static size_t s_voice_verify_pcm_len;
static size_t s_voice_verify_pcm_cap;
static int s_voice_verify_truncated;

/* VAD 触发后首帧原经 xRingbufferSend 进环；与 LVGL/SPI 异核时易在环缓冲自旋锁上触发 INT WDT，改为先内存再 ingest */
static volatile int s_capture_seed_pending;
static uint8_t s_capture_seed_buf[2048];
static size_t s_capture_seed_len;

static void log_heap_snapshot(const char* phase) {
  ESP_LOGI(TAG,
           "heap %s total_free=%u total_largest=%u internal_free=%u internal_largest=%u spiram_free=%u spiram_largest=%u",
           phase != NULL ? phase : "(unknown)", (unsigned)esp_get_free_heap_size(),
           (unsigned)heap_caps_get_largest_free_block(MALLOC_CAP_8BIT),
           (unsigned)heap_caps_get_free_size(MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT),
           (unsigned)heap_caps_get_largest_free_block(MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT),
           (unsigned)heap_caps_get_free_size(BBCLAW_MALLOC_CAP_PREFER_PSRAM),
           (unsigned)heap_caps_get_largest_free_block(BBCLAW_MALLOC_CAP_PREFER_PSRAM));
}

static void capture_seed_clear(void) {
  s_capture_seed_pending = 0;
  s_capture_seed_len = 0;
}

/** 公网 Cloud SaaS 模式下启用「密语」锁屏（ASR 比对），与声纹/生物特征无关。 */
static int passphrase_unlock_enabled(void) {
  return bb_transport_is_cloud_saas();
}

static int radio_app_is_locked(void) {
  return passphrase_unlock_enabled() && s_app_state == BBCLAW_STATE_LOCKED;
}

static void refresh_lock_screen_visibility(void) {
  bb_display_set_locked(radio_app_is_locked());
}

static void set_radio_app_state(bb_radio_app_state_t state) {
  if (s_app_state != state) {
    ESP_LOGI(TAG, "STATE_TRANSITION: %s -> %s",
             s_app_state == BBCLAW_STATE_LOCKED ? BB_STATUS_LOCKED : BB_STATUS_READY,
             state == BBCLAW_STATE_LOCKED ? BB_STATUS_LOCKED : BB_STATUS_READY);
    s_app_state = state;
    refresh_lock_screen_visibility();
  }
}

static size_t voice_verify_max_pcm_bytes(void) {
  return (size_t)((BBCLAW_AUDIO_SAMPLE_RATE * BBCLAW_VOICE_VERIFY_MAX_MS / 1000) * sizeof(int16_t) *
                  BBCLAW_AUDIO_CHANNELS);
}

static void voice_verify_capture_reset(void) {
  s_voice_verify_pcm_len = 0;
  s_voice_verify_truncated = 0;
}

static esp_err_t voice_verify_capture_append(const uint8_t* pcm, size_t pcm_len) {
  if (pcm == NULL || pcm_len == 0U) {
    return ESP_OK;
  }
  size_t cap = voice_verify_max_pcm_bytes();
  if (cap == 0U) {
    return ESP_ERR_INVALID_SIZE;
  }
  if (s_voice_verify_pcm_buf == NULL || s_voice_verify_pcm_cap != cap) {
    uint8_t* new_buf =
        (uint8_t*)heap_caps_realloc(s_voice_verify_pcm_buf, cap, BBCLAW_MALLOC_CAP_PREFER_PSRAM);
    if (new_buf == NULL) {
      return ESP_ERR_NO_MEM;
    }
    s_voice_verify_pcm_buf = new_buf;
    s_voice_verify_pcm_cap = cap;
  }
  if (s_voice_verify_pcm_len >= s_voice_verify_pcm_cap) {
    s_voice_verify_truncated = 1;
    return ESP_OK;
  }
  size_t remain = s_voice_verify_pcm_cap - s_voice_verify_pcm_len;
  size_t copy_len = pcm_len;
  if (copy_len > remain) {
    copy_len = remain;
    s_voice_verify_truncated = 1;
  }
  memcpy(s_voice_verify_pcm_buf + s_voice_verify_pcm_len, pcm, copy_len);
  s_voice_verify_pcm_len += copy_len;
  return ESP_OK;
}

static int using_es8311_input(void) {
  return strcasecmp(BBCLAW_AUDIO_INPUT_SOURCE, "es8311") == 0;
}

typedef struct {
  uint64_t total_samples;
  uint64_t nonzero_samples;
  uint64_t abs_sum;
} vad_stats_t;

static void refresh_power_display(void) {
  bb_power_state_t power = {0};
  bb_power_get_state(&power);
  bb_display_set_battery(power.supported, power.available, power.percent, power.low);
}

static void log_pin_summary(void) {
  if (using_es8311_input()) {
    ESP_LOGI(TAG,
             "pinmap ptt gpio=%d active=%d | es8311_i2c sda=%d scl=%d addr=0x%02X | es8311_i2s mck=%d bck=%d ws=%d "
             "do=%d di=%d",
             BBCLAW_PTT_GPIO, BBCLAW_PTT_ACTIVE_LEVEL, BBCLAW_ES8311_I2C_SDA_GPIO, BBCLAW_ES8311_I2C_SCL_GPIO,
             BBCLAW_ES8311_I2C_ADDR, BBCLAW_AUDIO_I2S_MCK_GPIO, BBCLAW_AUDIO_I2S_BCK_GPIO, BBCLAW_AUDIO_I2S_WS_GPIO,
             BBCLAW_AUDIO_I2S_DO_GPIO, BBCLAW_AUDIO_I2S_DI_GPIO);
  } else {
    ESP_LOGI(TAG, "pinmap ptt gpio=%d active=%d | inmp441_i2s bck=%d ws=%d do=%d di=%d", BBCLAW_PTT_GPIO,
             BBCLAW_PTT_ACTIVE_LEVEL, BBCLAW_AUDIO_I2S_BCK_GPIO, BBCLAW_AUDIO_I2S_WS_GPIO, BBCLAW_AUDIO_I2S_DO_GPIO,
             BBCLAW_AUDIO_I2S_DI_GPIO);
  }
  ESP_LOGI(TAG, "audio source=%s sample_rate=%d", BBCLAW_AUDIO_INPUT_SOURCE, BBCLAW_AUDIO_SAMPLE_RATE);
  ESP_LOGI(TAG, "pinmap st7789 sclk=%d mosi=%d cs=%d dc=%d rst=%d bl=%d", BBCLAW_ST7789_SCLK_GPIO,
           BBCLAW_ST7789_MOSI_GPIO, BBCLAW_ST7789_CS_GPIO, BBCLAW_ST7789_DC_GPIO, BBCLAW_ST7789_RST_GPIO,
           BBCLAW_ST7789_BL_GPIO);
  ESP_LOGI(TAG,
           "feature tts_playback=%d spk_test_on_boot=%d pa_en_gpio=%d pa_en_level=%d pa_probe=%d probe_gpio={%d,%d,%d} "
           "loopback_only=%d display_pull=%d motor_enable=%d motor_gpio=%d motor_level=%d power_enable=%d power_adc=%d "
           "nav_enable=%d nav={a:%d,b:%d,key:%d} led_enable=%d led_ryg={%d,%d,%d}",
           BBCLAW_ENABLE_TTS_PLAYBACK, BBCLAW_SPK_TEST_ON_BOOT, BBCLAW_PA_EN_GPIO, BBCLAW_PA_EN_ACTIVE_LEVEL,
           BBCLAW_PA_EN_PROBE_ON_BOOT, BBCLAW_PA_EN_PROBE_GPIO1, BBCLAW_PA_EN_PROBE_GPIO2, BBCLAW_PA_EN_PROBE_GPIO3,
           BBCLAW_LOCAL_LOOPBACK_ONLY, BBCLAW_ENABLE_DISPLAY_PULL, BBCLAW_MOTOR_ENABLE, BBCLAW_MOTOR_GPIO,
           BBCLAW_MOTOR_ACTIVE_LEVEL, BBCLAW_POWER_ENABLE, BBCLAW_POWER_ADC_GPIO, BBCLAW_NAV_ENABLE,
           BBCLAW_NAV_ENC_A_GPIO, BBCLAW_NAV_ENC_B_GPIO, BBCLAW_NAV_KEY_GPIO, BBCLAW_STATUS_LED_ENABLE,
           BBCLAW_STATUS_LED_R_GPIO, BBCLAW_STATUS_LED_Y_GPIO, BBCLAW_STATUS_LED_G_GPIO);
  if (using_es8311_input() && BBCLAW_STATUS_LED_ENABLE && BBCLAW_STATUS_LED_R_GPIO == BBCLAW_AUDIO_I2S_MCK_GPIO) {
    ESP_LOGW(TAG, "status led red gpio=%d conflicts with es8311 mck; remap one side before enabling both",
             BBCLAW_STATUS_LED_R_GPIO);
  }
  if (using_es8311_input() && BBCLAW_NAV_ENABLE &&
      (BBCLAW_NAV_ENC_A_GPIO == BBCLAW_ES8311_I2C_SCL_GPIO || BBCLAW_NAV_ENC_B_GPIO == BBCLAW_ES8311_I2C_SDA_GPIO)) {
    ESP_LOGW(TAG, "nav gpio conflicts with es8311 i2c; remap encoder or disable es8311 mode on breadboard");
  }
}

static void on_ptt_changed(int pressed) {
  s_ptt_pressed = pressed > 0 ? 1 : 0;
  if (s_ptt_pressed && s_tts_playback_active) {
    s_tts_interrupt_requested = 1;
    bb_audio_request_playback_interrupt();
  }
  s_ptt_change_version++;
}

static void on_nav_event(bb_nav_event_t event) {
  if ((int)event >= 0 && event < BB_NAV_EVENT_COUNT) {
    s_nav_event_versions[event]++;
  }
}

static void signal_error_haptic(void) {
  (void)bb_motor_trigger(BB_MOTOR_PATTERN_ERROR_ALERT);
}

/**
 * Cloud SaaS：当前不应发上行流、但属于「等门户 / 等云端 ASR 就绪」而非设备故障。
 * - 配对中（pending / binding_required）
 * - 已 approved 但 health 仍报 supports_audio_streaming=0
 * 此时长按 PTT 不得打 ERROR 长震，PTT 边沿也不做触觉（避免像报错一样狂震）。
 * 鉴权失败、云不可达等仍返回 0，走正常错误触觉。
 */
static int cloud_saas_tx_wait_is_benign(void) {
  if (!bb_transport_is_cloud_saas()) {
    return 0;
  }
  if (strcmp(s_transport_detail, "unauthorized") == 0 || s_transport_http_status == 401 ||
      s_transport_http_status == 403) {
    return 0;
  }
  if (strcmp(s_transport_detail, "cloud_unavailable") == 0 || strcmp(s_transport_detail, "request_failed") == 0) {
    return 0;
  }
  if (s_transport_pairing_status == BB_TRANSPORT_PAIRING_PENDING ||
      s_transport_pairing_status == BB_TRANSPORT_PAIRING_BINDING_REQUIRED) {
    return 1;
  }
  if (s_transport_ready && !s_transport_audio_streaming_ready) {
    return 1;
  }
  return 0;
}

static void shadow_transport_state_for_ui(bb_transport_state_t* state) {
  if (state == NULL) {
    return;
  }
  memset(state, 0, sizeof(*state));
  state->ready = s_transport_ready;
  state->supports_audio_streaming = s_transport_audio_streaming_ready;
  state->supports_tts = s_transport_tts_ready;
  state->supports_display = s_transport_display_ready;
  state->http_status = s_transport_http_status;
  state->pairing_status = s_transport_pairing_status;
  snprintf(state->detail, sizeof(state->detail), "%s", s_transport_detail);
  snprintf(state->cloud_registration_code, sizeof(state->cloud_registration_code), "%s",
           s_transport_registration_code);
  snprintf(state->cloud_registration_expires_at, sizeof(state->cloud_registration_expires_at), "%s",
           s_transport_registration_expires_at);
}

static void show_status_idle(const char* status) {
  bb_display_set_record_level(0, 0);
  (void)bb_display_show_status(status);
  (void)bb_led_set_status(BB_LED_IDLE);
}

static void show_status_recording(const char* status) {
  bb_display_set_record_level(0, 0);
  (void)bb_display_show_status(status);
  (void)bb_led_set_status(BB_LED_RECORDING);
}

static void show_status_processing(const char* status) {
  bb_display_set_record_level(0, 0);
  (void)bb_display_show_status(status);
  (void)bb_led_set_status(BB_LED_PROCESSING);
}

#if BBCLAW_ENABLE_DISPLAY_PULL
static void show_status_notification(const char* status) {
  bb_display_set_record_level(0, 0);
  (void)bb_display_show_status(status);
  (void)bb_led_set_status(BB_LED_NOTIFICATION);
}
#endif

static void show_status_error(const char* status) {
  bb_display_set_record_level(0, 0);
  (void)bb_display_show_status(status);
  (void)bb_led_set_status(BB_LED_ERROR);
}

static void pulse_success_on_idle(const char* status) {
  bb_display_set_record_level(0, 0);
  (void)bb_display_show_status(status);
  (void)bb_led_set_status(BB_LED_IDLE);
  (void)bb_led_set_status(BB_LED_SUCCESS);
}

static void show_idle_ready_or_locked(void) {
  if (radio_app_is_locked()) {
    show_status_idle(BB_STATUS_LOCKED);
  } else {
    show_status_idle(BB_STATUS_READY);
  }
}

typedef struct {
  char transcript[sizeof(((bb_finish_result_t*)0)->transcript)];
  char reply_text[sizeof(((bb_finish_result_t*)0)->reply_text)];
  QueueHandle_t tts_queue;
  TaskHandle_t tts_task;
  volatile int tts_task_done;
  volatile int tts_chunk_received;
  volatile int tts_done_received;
  volatile int tts_playback_started;
  volatile int tts_playback_failed;
  volatile int tts_playback_interrupted;
  volatile int tts_chunk_played;
  volatile size_t tts_total_pcm_bytes;
} bb_reply_stream_ui_ctx_t;

typedef enum {
  BB_TTS_QUEUE_EVT_CHUNK = 0,
  BB_TTS_QUEUE_EVT_DONE,
  BB_TTS_QUEUE_EVT_STOP,
} bb_tts_queue_evt_type_t;

typedef struct {
  bb_tts_queue_evt_type_t type;
  bb_tts_chunk_t* chunk;
} bb_tts_queue_evt_t;

static void free_single_tts_chunk(bb_tts_chunk_t* chunk) {
  if (chunk == NULL) {
    return;
  }
  chunk->next = NULL;
  bb_adapter_tts_chunks_free(chunk);
}

static void tts_stream_queue_drain(bb_reply_stream_ui_ctx_t* ui) {
  if (ui == NULL || ui->tts_queue == NULL) {
    return;
  }
  bb_tts_queue_evt_t evt = {0};
  while (xQueueReceive(ui->tts_queue, &evt, 0) == pdTRUE) {
    free_single_tts_chunk(evt.chunk);
  }
}

static void tts_playback_set_active(int active) {
  s_tts_playback_active = active ? 1 : 0;
  if (active) {
    s_tts_interrupt_requested = 0;
    bb_audio_clear_playback_interrupt();
  }
}

static void tts_request_interrupt(void) {
  s_tts_interrupt_requested = 1;
  bb_audio_request_playback_interrupt();
}

static int tts_interrupt_requested(void) {
  return s_tts_interrupt_requested || bb_audio_is_playback_interrupt_requested();
}

static void tts_stream_task(void* arg) {
  bb_reply_stream_ui_ctx_t* ui = (bb_reply_stream_ui_ctx_t*)arg;
  int playback_started = 0;
  int playback_sample_rate = BBCLAW_AUDIO_SAMPLE_RATE;
  char last_sentence[256] = {0};

  for (;;) {
    bb_tts_queue_evt_t evt = {0};
    if (xQueueReceive(ui->tts_queue, &evt, portMAX_DELAY) != pdTRUE) {
      continue;
    }

    if (evt.type == BB_TTS_QUEUE_EVT_STOP) {
      free_single_tts_chunk(evt.chunk);
      break;
    }
    if (evt.type == BB_TTS_QUEUE_EVT_DONE) {
      ESP_LOGI(TAG, "phase=tts_queue_drain played_chunks=%d total_bytes=%u queue_left=%u", ui->tts_chunk_played,
               (unsigned)ui->tts_total_pcm_bytes, (unsigned)uxQueueMessagesWaiting(ui->tts_queue));
      break;
    }
    if (evt.type != BB_TTS_QUEUE_EVT_CHUNK || evt.chunk == NULL) {
      continue;
    }

    bb_tts_chunk_t* chunk = evt.chunk;
    if (!playback_started) {
      ESP_LOGI(TAG, "phase=tts_play_start mono_ms=%lld first_chunk=1 queue_depth=%u", (long long)bb_now_ms(),
               (unsigned)uxQueueMessagesWaiting(ui->tts_queue));
      (void)bb_display_show_status(BB_STATUS_SPEAK);
      (void)bb_led_set_status(BB_LED_REPLY);
      bb_display_set_tts_playing(1);
      esp_err_t start_err = bb_audio_start_playback();
      if (start_err != ESP_OK) {
        ui->tts_playback_failed = 1;
        ESP_LOGE(TAG, "bb_audio_start_playback failed err=%s (streamed TTS)", esp_err_to_name(start_err));
        signal_error_haptic();
        free_single_tts_chunk(chunk);
        break;
      }
      playback_started = 1;
      ui->tts_playback_started = 1;
      tts_playback_set_active(1);
    }

    playback_sample_rate = chunk->sample_rate > 0 ? chunk->sample_rate : BBCLAW_AUDIO_SAMPLE_RATE;
    if (playback_sample_rate != BBCLAW_AUDIO_SAMPLE_RATE) {
      (void)bb_audio_set_playback_sample_rate(playback_sample_rate);
    }
    int expected_ms = playback_sample_rate > 0 ? (int)(chunk->pcm_len / 2 * 1000 / playback_sample_rate) : 0;
    int seq = chunk->seq > 0 ? chunk->seq : (ui->tts_chunk_played + 1);
    int64_t chunk_start_ms = bb_now_ms();
    ESP_LOGI(TAG, "phase=tts_chunk_play seq=%d pcm_bytes=%u rate=%d ch=%d expected_ms=%d", seq,
             (unsigned)chunk->pcm_len, playback_sample_rate, chunk->channels, expected_ms);
    if (chunk->tts_text[0] != '\0' && strcmp(chunk->tts_text, last_sentence) != 0) {
      strncpy(last_sentence, chunk->tts_text, sizeof(last_sentence) - 1);
      last_sentence[sizeof(last_sentence) - 1] = '\0';
      bb_display_set_tts_sentence(last_sentence);
    }
    if (bb_audio_play_pcm_blocking(chunk->pcm_data, chunk->pcm_len) != ESP_OK) {
      if (tts_interrupt_requested()) {
        ui->tts_playback_interrupted = 1;
        ESP_LOGI(TAG, "phase=tts_chunk_interrupt seq=%d", seq);
      } else {
        ui->tts_playback_failed = 1;
        ESP_LOGE(TAG, "tts chunk play failed seq=%d", seq);
        signal_error_haptic();
      }
      free_single_tts_chunk(chunk);
      break;
    }
    ui->tts_chunk_played++;
    ui->tts_total_pcm_bytes += chunk->pcm_len;
    ESP_LOGI(TAG, "phase=tts_chunk_done seq=%d actual_ms=%lld expected_ms=%d ratio_x100=%lld", seq,
             (long long)(bb_now_ms() - chunk_start_ms), expected_ms,
             expected_ms > 0 ? (long long)((bb_now_ms() - chunk_start_ms) * 100 / expected_ms) : 0LL);
    free_single_tts_chunk(chunk);
  }

  tts_stream_queue_drain(ui);
  if (playback_started) {
    (void)bb_audio_stop_playback();
    (void)bb_audio_set_playback_sample_rate(BBCLAW_AUDIO_SAMPLE_RATE);
  }
  tts_playback_set_active(0);
  bb_display_set_tts_playing(0);
  ui->tts_task_done = 1;
  ui->tts_task = NULL;
  vTaskDelete(NULL);
}

static esp_err_t tts_stream_ui_init(bb_reply_stream_ui_ctx_t* ui) {
  if (ui == NULL) {
    return ESP_ERR_INVALID_ARG;
  }
  ui->tts_queue = xQueueCreate(BB_TTS_STREAM_QUEUE_DEPTH, sizeof(bb_tts_queue_evt_t));
  if (ui->tts_queue == NULL) {
    return ESP_ERR_NO_MEM;
  }
  if (xTaskCreate(tts_stream_task, "bb_tts_stream", BB_TTS_STREAM_TASK_STACK, ui, 5, &ui->tts_task) != pdPASS) {
    vQueueDelete(ui->tts_queue);
    ui->tts_queue = NULL;
    return ESP_FAIL;
  }
  return ESP_OK;
}

static void tts_stream_ui_shutdown(bb_reply_stream_ui_ctx_t* ui, int force_stop) {
  if (ui == NULL) {
    return;
  }
  if (ui->tts_queue != NULL && !ui->tts_task_done) {
    bb_tts_queue_evt_t evt = {
        .type = force_stop ? BB_TTS_QUEUE_EVT_STOP : BB_TTS_QUEUE_EVT_DONE,
    };
    (void)xQueueSend(ui->tts_queue, &evt, pdMS_TO_TICKS(20));
  }
  while (ui->tts_task != NULL && !ui->tts_task_done) {
    vTaskDelay(pdMS_TO_TICKS(20));
  }
  if (ui->tts_queue != NULL) {
    vQueueDelete(ui->tts_queue);
    ui->tts_queue = NULL;
  }
}

static void on_finish_stream_event(bb_finish_stream_event_t* event, void* user_ctx) {
  bb_reply_stream_ui_ctx_t* ui = (bb_reply_stream_ui_ctx_t*)user_ctx;
  if (event == NULL || ui == NULL) {
    return;
  }

  if (event->type == BB_FINISH_STREAM_EVENT_STATUS) {
    if (event->phase != NULL && strcmp(event->phase, "transcribing") == 0) {
      show_status_processing("TRANSCRIBING");
    } else if (event->phase != NULL && strcmp(event->phase, "processing") == 0) {
      show_status_processing("PROCESSING");
    }
    return;
  }

  if (event->type == BB_FINISH_STREAM_EVENT_ASR_FINAL) {
    const char* line_you = (event->text != NULL && event->text[0] != '\0') ? event->text : "(no speech)";
    strncpy(ui->transcript, line_you, sizeof(ui->transcript) - 1);
    ui->transcript[sizeof(ui->transcript) - 1] = '\0';
    ui->reply_text[0] = '\0';
    (void)bb_display_upsert_chat_turn(ui->transcript, "", 0);
    return;
  }

  if (event->type == BB_FINISH_STREAM_EVENT_THINKING && event->text != NULL && event->text[0] != '\0') {
    ESP_LOGI(TAG, "phase=thinking text=%.80s", event->text);
    size_t cur = strlen(ui->reply_text);
    snprintf(ui->reply_text + cur, sizeof(ui->reply_text) - cur, "%s[thinking...]\n", cur > 0 ? "\n" : "");
    (void)bb_display_upsert_chat_turn(ui->transcript[0] != '\0' ? ui->transcript : "...", ui->reply_text, 0);
    return;
  }

  if (event->type == BB_FINISH_STREAM_EVENT_TOOL_CALL && event->text != NULL && event->text[0] != '\0') {
    ESP_LOGI(TAG, "phase=tool_call name=%s", event->text);
    size_t cur = strlen(ui->reply_text);
    snprintf(ui->reply_text + cur, sizeof(ui->reply_text) - cur, "%s[tool: %s]\n", cur > 0 ? "\n" : "", event->text);
    (void)bb_display_upsert_chat_turn(ui->transcript[0] != '\0' ? ui->transcript : "...", ui->reply_text, 0);
    return;
  }

  if (event->type == BB_FINISH_STREAM_EVENT_REPLY_DELTA && event->text != NULL && event->text[0] != '\0') {
    /* reply delta replaces only the reply portion, keep process log prefix */
    size_t prefix_len = 0;
    const char* last_nl = strrchr(ui->reply_text, '\n');
    if (last_nl != NULL && ui->reply_text[0] == '[') {
      prefix_len = (size_t)(last_nl - ui->reply_text) + 1;
    }
    char prefix[BBCLAW_DISPLAY_CHAT_LINE_LEN];
    if (prefix_len > 0 && prefix_len < sizeof(prefix)) {
      memcpy(prefix, ui->reply_text, prefix_len);
      prefix[prefix_len] = '\0';
      snprintf(ui->reply_text, sizeof(ui->reply_text), "%s%s", prefix, event->text);
    } else {
      strncpy(ui->reply_text, event->text, sizeof(ui->reply_text) - 1);
    }
    ui->reply_text[sizeof(ui->reply_text) - 1] = '\0';
    (void)bb_display_upsert_chat_turn(ui->transcript[0] != '\0' ? ui->transcript : "(no speech)", ui->reply_text, 0);
    return;
  }

  if (event->type == BB_FINISH_STREAM_EVENT_TTS_CHUNK && event->tts_chunk != NULL) {
    int seq = event->tts_chunk->seq;
    if (seq <= 0) {
      seq = ui->tts_chunk_received + 1;
    }
    ui->tts_chunk_received++;
    ESP_LOGI(TAG, "phase=tts_chunk_recv seq=%d pcm_bytes=%u rate=%d ch=%d", seq, (unsigned)event->tts_chunk->pcm_len,
             event->tts_chunk->sample_rate, event->tts_chunk->channels);
    if (ui->tts_queue != NULL) {
      bb_tts_queue_evt_t evt = {
          .type = BB_TTS_QUEUE_EVT_CHUNK,
          .chunk = event->tts_chunk,
      };
      if (xQueueSend(ui->tts_queue, &evt, 0) == pdTRUE) {
        ESP_LOGI(TAG, "phase=tts_chunk_enqueue seq=%d queue_depth=%u", seq, (unsigned)uxQueueMessagesWaiting(ui->tts_queue));
        return;
      }
      ESP_LOGW(TAG, "phase=tts_chunk_drop seq=%d queue_full=1", seq);
    }
    free_single_tts_chunk(event->tts_chunk);
    return;
  }

  if (event->type == BB_FINISH_STREAM_EVENT_TTS_DONE) {
    ui->tts_done_received = 1;
    ESP_LOGI(TAG, "phase=tts_done_recv chunks=%d queue_depth=%u", ui->tts_chunk_received,
             ui->tts_queue != NULL ? (unsigned)uxQueueMessagesWaiting(ui->tts_queue) : 0U);
    if (ui->tts_queue != NULL) {
      bb_tts_queue_evt_t evt = {
          .type = BB_TTS_QUEUE_EVT_DONE,
      };
      (void)xQueueSend(ui->tts_queue, &evt, pdMS_TO_TICKS(20));
    }
    return;
  }

  if (event->type == BB_FINISH_STREAM_EVENT_ERROR) {
    ESP_LOGW(TAG, "phase=tts_stream_error reply_wait_timed_out=%d", event->reply_wait_timed_out);
  }
}

static void remember_transport_state(const bb_transport_state_t* state) {
  if (state == NULL) {
    return;
  }
  s_transport_http_status = state->http_status;
  s_transport_tts_ready = state->supports_tts;
  s_transport_display_ready = state->supports_display;
  snprintf(s_transport_detail, sizeof(s_transport_detail), "%s", state->detail);
  snprintf(s_transport_registration_code, sizeof(s_transport_registration_code), "%s", state->cloud_registration_code);
  snprintf(s_transport_registration_expires_at, sizeof(s_transport_registration_expires_at), "%s",
           state->cloud_registration_expires_at);
  s_transport_pairing_status = state->pairing_status;
}

#if BBCLAW_ENABLE_TTS_PLAYBACK
static void maybe_speak_pairing_code(const bb_transport_state_t* state) {
  if (state == NULL || !bb_transport_is_cloud_saas()) {
    return;
  }
  if (state->cloud_registration_code[0] == '\0') {
    return;
  }
  if (strcmp(state->detail, "claim_required") != 0) {
    return;
  }
  if (strcmp(s_spoken_registration_code, state->cloud_registration_code) == 0) {
    return;
  }

  const char* code = state->cloud_registration_code;
  size_t n = strlen(code);
  if (n == 0U || n > 12U) {
    return;
  }
  /* 中文逗号让 TTS 在数字间停顿，比空格更易听清（与 cloud speedRatio 无关） */
  char utter[192];
  int pos = snprintf(utter, sizeof(utter), "验证码");
  for (size_t i = 0; i < n && pos < (int)sizeof(utter) - 8; i++) {
    pos += snprintf(utter + (size_t)pos, sizeof(utter) - (size_t)pos, "，%c", code[i]);
  }

  bb_tts_audio_t tts = {0};
  esp_err_t syn = bb_adapter_tts_synthesize_pcm16(utter, &tts);
  if (syn != ESP_OK || tts.pcm_data == NULL || tts.pcm_len == 0U) {
    ESP_LOGW(TAG, "pairing code TTS failed err=%s pcm=%p len=%u", esp_err_to_name(syn), (void*)tts.pcm_data,
             (unsigned)tts.pcm_len);
    bb_adapter_tts_audio_free(&tts);
    return;
  }

  esp_err_t tx = bb_audio_start_playback();
  if (tx != ESP_OK) {
    ESP_LOGW(TAG, "pairing code TTS playback start failed err=%s", esp_err_to_name(tx));
    bb_adapter_tts_audio_free(&tts);
    return;
  }
  if (tts.sample_rate > 0 && tts.sample_rate != BBCLAW_AUDIO_SAMPLE_RATE) {
    (void)bb_audio_set_playback_sample_rate(tts.sample_rate);
  }
  if (bb_audio_play_pcm_blocking(tts.pcm_data, tts.pcm_len) != ESP_OK) {
    ESP_LOGW(TAG, "pairing code TTS pcm play failed");
  }
  (void)bb_audio_stop_playback();
  (void)bb_audio_set_playback_sample_rate(BBCLAW_AUDIO_SAMPLE_RATE);
  bb_adapter_tts_audio_free(&tts);

  snprintf(s_spoken_registration_code, sizeof(s_spoken_registration_code), "%s", state->cloud_registration_code);
  ESP_LOGI(TAG, "pairing code spoken via TTS code=%s", state->cloud_registration_code);
}
#else
static void maybe_speak_pairing_code(const bb_transport_state_t* state) {
  (void)state;
}
#endif

static void show_cloud_transport_message(const bb_transport_state_t* state) {
  if (state == NULL) {
    return;
  }

  if (strcmp(state->detail, "unauthorized") == 0 || state->http_status == 401 || state->http_status == 403) {
    show_status_error(BB_STATUS_AUTH);
    (void)bb_display_show_chat_turn("Cloud auth required", "Set Cloud token in menuconfig");
    return;
  }

  if (state->pairing_status == BB_TRANSPORT_PAIRING_PENDING ||
      state->pairing_status == BB_TRANSPORT_PAIRING_BINDING_REQUIRED) {
    char line1[BBCLAW_DISPLAY_CHAT_LINE_LEN];
    char line2[BBCLAW_DISPLAY_CHAT_LINE_LEN];
    show_status_processing(BB_STATUS_PAIR);
    if (state->cloud_registration_code[0] != '\0') {
      snprintf(line1, sizeof(line1), "Enter 6-digit code in portal");
      snprintf(line2, sizeof(line2), "%s", state->cloud_registration_code);
      (void)bb_display_show_chat_turn(line1, line2);
      static char s_pairing_log_code[16];
      static char s_pairing_log_exp[40];
      if (strcmp(s_pairing_log_code, state->cloud_registration_code) != 0 ||
          strcmp(s_pairing_log_exp, state->cloud_registration_expires_at) != 0) {
        ESP_LOGI(TAG, "pairing claim_required code=%s detail=%s expires=%s", state->cloud_registration_code,
                 state->detail, state->cloud_registration_expires_at);
        snprintf(s_pairing_log_code, sizeof(s_pairing_log_code), "%s", state->cloud_registration_code);
        snprintf(s_pairing_log_exp, sizeof(s_pairing_log_exp), "%s", state->cloud_registration_expires_at);
      }
      maybe_speak_pairing_code(state);
    } else if (state->pairing_status == BB_TRANSPORT_PAIRING_BINDING_REQUIRED) {
      snprintf(line1, sizeof(line1), "Create binding in portal");
      snprintf(line2, sizeof(line2), "%s", BBCLAW_DEVICE_ID);
      (void)bb_display_show_chat_turn(line1, line2);
    } else {
      snprintf(line1, sizeof(line1), "Cloud pairing pending");
      snprintf(line2, sizeof(line2), "%s", BBCLAW_DEVICE_ID);
      (void)bb_display_show_chat_turn(line1, line2);
    }
    return;
  }

  if (!state->supports_audio_streaming) {
    if (state->ready) {
      /* 已通过配对但 health 仍报 ASR/WebSocket 未就绪 — 与 claim 阶段同属「稍等」 */
      show_status_processing("LINK");
      (void)bb_display_show_chat_turn("Cloud ASR starting", "Please wait");
    } else {
      show_status_error("CLOUD");
      (void)bb_display_show_chat_turn("Cloud ASR unavailable", "Check Cloud ASR or Home relay");
    }
    return;
  }

  if (strcmp(state->detail, "cloud_unavailable") == 0) {
    show_status_error("LINK ERR");
    (void)bb_display_show_chat_turn("Cloud unavailable", "Check network or cloud service");
    return;
  }

  if (state->ready) {
    pulse_success_on_idle(BB_STATUS_READY);
    char info[BBCLAW_DISPLAY_CHAT_LINE_LEN];
    const char* ssid = bb_wifi_get_active_ssid();
    if (!state->supports_tts && !state->supports_display) {
      snprintf(info, sizeof(info), "WiFi: %s | Text only", ssid);
      (void)bb_display_show_chat_turn("Cloud linked", info);
      return;
    }
    if (!state->supports_tts) {
      snprintf(info, sizeof(info), "WiFi: %s | ASR ready", ssid);
      (void)bb_display_show_chat_turn("Cloud linked", info);
      return;
    }
    if (!state->supports_display) {
      snprintf(info, sizeof(info), "WiFi: %s | ASR+TTS", ssid);
      (void)bb_display_show_chat_turn("Cloud linked", info);
      return;
    }
    snprintf(info, sizeof(info), "WiFi: %s | Full", ssid);
    (void)bb_display_show_chat_turn("Cloud linked", info);
  }
}

static void show_cloud_transport_or_locked(const bb_transport_state_t* state) {
  if (state != NULL && radio_app_is_locked() && state->ready && state->supports_audio_streaming) {
    show_status_idle(BB_STATUS_LOCKED);
    refresh_lock_screen_visibility();
    return;
  }
  show_cloud_transport_message(state);
  refresh_lock_screen_visibility();
}

static esp_err_t wait_for_transport_health(int* out_status) {
  int status = 0;
  esp_err_t last_err = ESP_FAIL;
  for (int attempt = 1; attempt <= BBCLAW_ADAPTER_BOOT_HEALTH_RETRIES; ++attempt) {
    bb_transport_state_t state = {0};
    last_err = bb_transport_bootstrap(&state);
    status = state.http_status;
    s_transport_ready = state.ready;
    s_transport_audio_streaming_ready = state.supports_audio_streaming;
    s_transport_tts_ready = state.supports_tts;
    s_transport_display_ready = state.supports_display;
    remember_transport_state(&state);
    if (last_err == ESP_OK) {
      if (out_status != NULL) {
        *out_status = status;
      }
      return ESP_OK;
    }
    ESP_LOGW(TAG, "transport bootstrap retry=%d/%d profile=%s err=%s", attempt, BBCLAW_ADAPTER_BOOT_HEALTH_RETRIES,
             bb_transport_profile_name(), esp_err_to_name(last_err));
    /* On first failure show error immediately so user doesn't wait 8 retries */
    if (attempt == 1) {
      if (bb_transport_is_cloud_saas()) {
        show_status_error("CLOUD ERR");
        (void)bb_display_show_chat_turn("Cloud unreachable", "Check " BBCLAW_CLOUD_BASE_URL);
      } else {
        show_status_error("LINK ERR");
        (void)bb_display_show_chat_turn("Local adapter unreachable", "Check " BBCLAW_ADAPTER_BASE_URL);
      }
    }
    if (attempt < BBCLAW_ADAPTER_BOOT_HEALTH_RETRIES) {
      vTaskDelay(pdMS_TO_TICKS(BBCLAW_ADAPTER_BOOT_HEALTH_DELAY_MS));
    }
  }
  if (out_status != NULL) {
    *out_status = status;
  }
  return last_err;
}

static esp_err_t flush_stream_chunk(bb_stream_ctx_t* stream, uint8_t* pcm_buf, size_t* pending_pcm_len) {
  if (stream == NULL || pcm_buf == NULL || pending_pcm_len == NULL) {
    return ESP_ERR_INVALID_ARG;
  }
  if (*pending_pcm_len == 0U) {
    return ESP_OK;
  }

  if (bb_transport_is_cloud_saas()) {
    esp_err_t err = bb_adapter_stream_chunk_pcm(stream, pcm_buf, *pending_pcm_len, bb_now_ms());
    if (err != ESP_OK) {
      return err;
    }
    *pending_pcm_len = 0U;
    return ESP_OK;
  }

  size_t encoded_len = 0;
  esp_err_t err = bb_audio_encode_opus(pcm_buf, *pending_pcm_len, s_stream_encoded_chunk_buf,
                                       sizeof(s_stream_encoded_chunk_buf), &encoded_len);
  if (err != ESP_OK) {
    return err;
  }
  err = bb_adapter_stream_chunk(stream, s_stream_encoded_chunk_buf, encoded_len, bb_now_ms());
  if (err != ESP_OK) {
    return err;
  }
  *pending_pcm_len = 0U;
  return ESP_OK;
}

static void vad_update_from_pcm(vad_stats_t* vad, const uint8_t* pcm_buf, size_t pcm_bytes) {
  if (vad == NULL || pcm_buf == NULL || pcm_bytes == 0U) {
    return;
  }
  const int16_t* samples = (const int16_t*)pcm_buf;
  size_t sample_count = pcm_bytes / sizeof(int16_t);
  for (size_t i = 0; i < sample_count; ++i) {
    int32_t v = samples[i];
    vad->total_samples++;
    if (v != 0) {
      vad->nonzero_samples++;
    }
    if (v < 0) {
      v = -v;
    }
    vad->abs_sum += (uint64_t)v;
  }
}

static int vad_passes_thresholds(const vad_stats_t* vad, uint32_t min_dur_ms, uint32_t min_nz, uint32_t min_mean) {
  if (vad == NULL || vad->total_samples == 0U) {
    return 0;
  }
  uint64_t duration_ms = (vad->total_samples * 1000ULL) / BBCLAW_AUDIO_SAMPLE_RATE;
  uint32_t nonzero_permille = (uint32_t)((vad->nonzero_samples * 1000ULL) / vad->total_samples);
  uint32_t mean_abs = (uint32_t)(vad->abs_sum / vad->total_samples);
  return duration_ms >= min_dur_ms && nonzero_permille >= min_nz && mean_abs >= min_mean;
}

static void update_record_level_from_pcm(const uint8_t* pcm_buf, size_t pcm_bytes) {
  if (pcm_buf == NULL || pcm_bytes < sizeof(int16_t)) {
    bb_display_set_record_level(0, 0);
    return;
  }

  const int16_t* samples = (const int16_t*)pcm_buf;
  size_t sample_count = pcm_bytes / sizeof(int16_t);
  uint64_t abs_sum = 0;
  uint32_t peak_abs = 0;
  for (size_t i = 0; i < sample_count; ++i) {
    int32_t v = samples[i];
    if (v < 0) {
      v = -v;
    }
    abs_sum += (uint32_t)v;
    if ((uint32_t)v > peak_abs) {
      peak_abs = (uint32_t)v;
    }
  }

  const uint32_t mean_abs = sample_count > 0U ? (uint32_t)(abs_sum / sample_count) : 0U;
  const uint32_t mean_pct = (mean_abs * 100U) / 600U;
  const uint32_t peak_pct = (peak_abs * 100U) / 6000U;
  uint32_t level_pct = mean_pct > peak_pct ? mean_pct : peak_pct;
  if (level_pct > 100U) {
    level_pct = 100U;
  } else if (level_pct == 0U && peak_abs > 0U) {
    level_pct = 2U;
  }

  const int voiced = mean_abs >= BBCLAW_VAD_ARM_MIN_MEAN_ABS || peak_abs >= 1000U;
  bb_display_set_record_level((uint8_t)level_pct, voiced);
}

static esp_err_t stream_ingest_pcm(bb_stream_ctx_t* stream, vad_stats_t* vad, const uint8_t* pcm_read_buf, size_t pcm_read,
                                   size_t* pending_pcm_len, int* streaming_ok) {
  if (stream == NULL || vad == NULL || pending_pcm_len == NULL || streaming_ok == NULL) {
    return ESP_ERR_INVALID_ARG;
  }
  if (pcm_read == 0U) {
    return ESP_OK;
  }
  vad_update_from_pcm(vad, pcm_read_buf, pcm_read);
  size_t offset = 0;
  while (offset < pcm_read) {
    size_t remain = pcm_read - offset;
    size_t space = sizeof(s_stream_pcm_chunk_buf) - *pending_pcm_len;
    size_t to_copy = remain;
    if (to_copy > space) {
      to_copy = space;
    }
    memcpy(s_stream_pcm_chunk_buf + *pending_pcm_len, pcm_read_buf + offset, to_copy);
    *pending_pcm_len += to_copy;
    offset += to_copy;

    if (*pending_pcm_len == sizeof(s_stream_pcm_chunk_buf)) {
      if (flush_stream_chunk(stream, s_stream_pcm_chunk_buf, pending_pcm_len) != ESP_OK) {
        ESP_LOGE(TAG, "stream chunk failed");
        *streaming_ok = 0;
        return ESP_FAIL;
      }
    }
  }
  return ESP_OK;
}

static void try_finish_stream_abort(bb_stream_ctx_t* stream) {
  if (stream == NULL) {
    return;
  }
  bb_finish_result_t r = {0};
  (void)bb_adapter_stream_finish(stream, &r);
}

static void capture_task(void* arg) {
  (void)arg;
  uint8_t pcm_read_buf[1024];
  while (1) {
    if (!s_capture_active) {
      s_capture_stopped = 1;
      vTaskDelay(pdMS_TO_TICKS(10));
      continue;
    }
    s_capture_stopped = 0;
    size_t pcm_read = 0;
    esp_err_t err = bb_audio_read_pcm_frame(pcm_read_buf, sizeof(pcm_read_buf), &pcm_read);
    if (err != ESP_OK || pcm_read == 0U) {
      if (err != ESP_OK) {
        ESP_LOGE(TAG, "capture_task pcm read err=%s", esp_err_to_name(err));
      }
      continue;
    }
    update_record_level_from_pcm(pcm_read_buf, pcm_read);
    /* Best-effort push; if ring buffer is full, drop oldest data is not supported
     * by FreeRTOS ring buffer, so we use a short timeout and log overflow. */
    if (xRingbufferSend(s_capture_rb, pcm_read_buf, pcm_read, 0) != pdTRUE) {
      ESP_LOGW(TAG, "capture_task ringbuf full, dropping %u bytes", (unsigned)pcm_read);
    }
  }
}

static void stream_task(void* arg) {
  (void)arg;
  bb_stream_ctx_t stream = {0};
  int streaming = 0;
  int arming = 0;
  int verifying = 0;
  int session_busy = 0;
  int64_t stream_start_ms = 0;
  int64_t arm_started_ms = 0;
  unsigned ptt_handled_version = s_ptt_change_version;
  vad_stats_t vad = {0};
  vad_stats_t verify_vad = {0};
  size_t pending_pcm_len = 0;
#if BBCLAW_ENABLE_DISPLAY_PULL
  int64_t last_display_poll_ms = 0;
#endif
  int64_t last_power_poll_ms = 0;
  int64_t last_adapter_heartbeat_ms = 0;
  const int adapter_heartbeat_interval_ms =
      BBCLAW_ADAPTER_HEARTBEAT_INTERVAL_MS > 0 ? BBCLAW_ADAPTER_HEARTBEAT_INTERVAL_MS : 5000;
  const int adapter_heartbeat_fail_threshold =
      BBCLAW_ADAPTER_HEARTBEAT_FAIL_THRESHOLD > 0 ? BBCLAW_ADAPTER_HEARTBEAT_FAIL_THRESHOLD : 2;
  int adapter_health_is_up = s_transport_health_ok ? 1 : 0;
  int adapter_health_fail_streak = 0;
  /** 按住 PTT 且处于门户配对门闸时，降低 UI/日志刷新频率 */
  int64_t pairing_ptt_ui_last_ms = 0;
  int nav_focus_ai = 1;
  int nav_history_mode = 0;
  unsigned nav_handled_versions[BB_NAV_EVENT_COUNT] = {0};

  ESP_LOGI(TAG, "stream task ready stack_hw=%u", (unsigned)uxTaskGetStackHighWaterMark(NULL));
  for (int i = 0; i < BB_NAV_EVENT_COUNT; ++i) {
    nav_handled_versions[i] = s_nav_event_versions[i];
  }

  while (1) {
    for (int event = 0; event < BB_NAV_EVENT_COUNT; ++event) {
      while (nav_handled_versions[event] != s_nav_event_versions[event]) {
        nav_handled_versions[event]++;
        switch ((bb_nav_event_t)event) {
          case BB_NAV_EVENT_ROTATE_CCW:
            if (nav_history_mode) {
              (void)bb_display_chat_prev_turn();
            } else {
              (void)bb_display_chat_scroll_up();
            }
            break;
          case BB_NAV_EVENT_ROTATE_CW:
            if (nav_history_mode) {
              (void)bb_display_chat_next_turn();
            } else {
              (void)bb_display_chat_scroll_down();
            }
            break;
          case BB_NAV_EVENT_CLICK:
            nav_focus_ai = !nav_focus_ai;
            if (nav_focus_ai) {
              bb_display_chat_focus_ai();
            } else {
              bb_display_chat_focus_me();
            }
            ESP_LOGI(TAG, "nav click focus=%s", nav_focus_ai ? "ai" : "me");
            break;
          case BB_NAV_EVENT_LONG_PRESS:
            nav_history_mode = !nav_history_mode;
            ESP_LOGI(TAG, "nav long_press mode=%s", nav_history_mode ? "history" : "scroll");
            break;
          case BB_NAV_EVENT_COUNT:
          default:
            break;
        }
      }
    }

    unsigned ptt_version = s_ptt_change_version;
    if (ptt_version != ptt_handled_version) {
      ptt_handled_version = ptt_version;
      if (s_ptt_pressed) {
        if (!bb_wifi_is_connected()) {
          signal_error_haptic();
          show_status_error(BB_STATUS_NO_WIFI);
        } else if (s_tts_playback_active) {
          tts_request_interrupt();
          show_status_processing(BB_STATUS_SKIP);
        } else if (session_busy) {
          signal_error_haptic();
          (void)bb_display_show_status(BB_STATUS_BUSY);
        } else if (radio_app_is_locked()) {
          show_status_recording(BB_STATUS_VERIFY_TX);
          (void)bb_motor_trigger(BB_MOTOR_PATTERN_PTT_PRESS);
        } else if (cloud_saas_tx_wait_is_benign()) {
          show_status_processing(BB_STATUS_PAIR);
          /* 配对 / ASR 未就绪：不按 TX 成功震动，避免用户以为在报错 */
        } else {
          show_status_recording(BB_STATUS_TX);
          (void)bb_motor_trigger(BB_MOTOR_PATTERN_PTT_PRESS);
        }
      } else {
        if (bb_wifi_is_connected()) {
          if (verifying || radio_app_is_locked()) {
            show_status_processing(verifying ? BB_STATUS_VERIFY : BB_STATUS_LOCKED);
          } else {
            show_status_processing(BB_STATUS_RX);
          }
        }
        pairing_ptt_ui_last_ms = 0;
        if (!radio_app_is_locked() && !cloud_saas_tx_wait_is_benign()) {
          (void)bb_motor_trigger(BB_MOTOR_PATTERN_PTT_RELEASE);
        }
      }
      (void)bb_gateway_node_send_ptt_state(s_ptt_pressed);
    }

    if (!s_ptt_pressed && arming && !streaming) {
      (void)bb_audio_stop_tx();
      arming = 0;
      memset(&vad, 0, sizeof(vad));
      pending_pcm_len = 0;
      if (bb_wifi_is_connected()) {
        show_idle_ready_or_locked();
      } else {
        show_status_idle("NO WIFI");
      }
      vTaskDelay(pdMS_TO_TICKS(20));
      continue;
    }

    if (s_ptt_pressed && !streaming && !arming && !session_busy) {
      if (!bb_wifi_is_connected()) {
        show_status_error(BB_STATUS_NO_WIFI);
        signal_error_haptic();
        vTaskDelay(pdMS_TO_TICKS(250));
        continue;
      }
      if (radio_app_is_locked()) {
        if (!bb_transport_is_cloud_saas()) {
          show_status_error(BB_STATUS_VERIFY_ERR);
          (void)bb_display_show_chat_turn("Voice unlock unavailable", "cloud_saas transport required");
          signal_error_haptic();
          vTaskDelay(pdMS_TO_TICKS(250));
          continue;
        }
        if (!s_transport_ready || !s_transport_audio_streaming_ready) {
          bb_transport_state_t state = {0};
          shadow_transport_state_for_ui(&state);
          if (cloud_saas_tx_wait_is_benign()) {
            int64_t now_ms = bb_now_ms();
            if (pairing_ptt_ui_last_ms == 0 || (now_ms - pairing_ptt_ui_last_ms) >= 2000) {
              pairing_ptt_ui_last_ms = now_ms;
              show_cloud_transport_or_locked(&state);
            }
            vTaskDelay(pdMS_TO_TICKS(250));
            continue;
          }
          show_cloud_transport_or_locked(&state);
          signal_error_haptic();
          vTaskDelay(pdMS_TO_TICKS(250));
          continue;
        }
        voice_verify_capture_reset();
        memset(&verify_vad, 0, sizeof(verify_vad));
        {
          size_t rb_item_size = 0;
          void* rb_item;
          while ((rb_item = xRingbufferReceive(s_capture_rb, &rb_item_size, 0)) != NULL) {
            vRingbufferReturnItem(s_capture_rb, rb_item);
          }
        }
        esp_err_t tx_err = bb_audio_start_tx();
        if (tx_err == ESP_OK) {
          verifying = 1;
          session_busy = 1;
          s_capture_active = 1;
          show_status_recording(BB_STATUS_VERIFY_TX);
          ESP_LOGI(TAG, "phase=voice_verify_capture_start mono_ms=%lld", (long long)bb_now_ms());
        } else {
          ESP_LOGE(TAG, "bb_audio_start_tx failed err=%s (voice verify)", esp_err_to_name(tx_err));
          show_status_error(BB_STATUS_VERIFY_ERR);
          signal_error_haptic();
          vTaskDelay(pdMS_TO_TICKS(200));
        }
        continue;
      }
      if (bb_transport_is_cloud_saas()) {
        if (!s_transport_ready) {
          if (cloud_saas_tx_wait_is_benign()) {
            int64_t now_ms = bb_now_ms();
            if (pairing_ptt_ui_last_ms == 0 || (now_ms - pairing_ptt_ui_last_ms) >= 2000) {
              pairing_ptt_ui_last_ms = now_ms;
              bb_transport_state_t state = {0};
              shadow_transport_state_for_ui(&state);
              show_cloud_transport_or_locked(&state);
            }
            vTaskDelay(pdMS_TO_TICKS(250));
            continue;
          }
          bb_transport_state_t state = {0};
          shadow_transport_state_for_ui(&state);
          show_cloud_transport_or_locked(&state);
          signal_error_haptic();
          vTaskDelay(pdMS_TO_TICKS(250));
          continue;
        }
        if (!s_transport_audio_streaming_ready) {
          if (cloud_saas_tx_wait_is_benign()) {
            int64_t now_ms = bb_now_ms();
            if (pairing_ptt_ui_last_ms == 0 || (now_ms - pairing_ptt_ui_last_ms) >= 2000) {
              pairing_ptt_ui_last_ms = now_ms;
              bb_transport_state_t state = {0};
              shadow_transport_state_for_ui(&state);
              show_cloud_transport_or_locked(&state);
            }
            vTaskDelay(pdMS_TO_TICKS(250));
            continue;
          }
          bb_transport_state_t state = {0};
          shadow_transport_state_for_ui(&state);
          show_cloud_transport_or_locked(&state);
          signal_error_haptic();
          vTaskDelay(pdMS_TO_TICKS(250));
          continue;
        }
      } else if (!s_transport_ready) {
        show_status_error("LINK ERR");
        signal_error_haptic();
        (void)bb_display_show_chat_turn("Local adapter unavailable", "Check adapter health");
        vTaskDelay(pdMS_TO_TICKS(250));
        continue;
      }
      {
        esp_err_t tx_err = bb_audio_start_tx();
        if (tx_err == ESP_OK) {
          arming = 1;
          arm_started_ms = bb_now_ms();
          memset(&vad, 0, sizeof(vad));
          pending_pcm_len = 0;
          ESP_LOGI(TAG, "phase=arm_listen mono_ms=%lld (wait speech before stream/start)", (long long)bb_now_ms());
        } else {
          ESP_LOGE(TAG, "bb_audio_start_tx failed err=%s (PTT arm)", esp_err_to_name(tx_err));
          show_status_error(BB_STATUS_ERR);
          signal_error_haptic();
          vTaskDelay(pdMS_TO_TICKS(200));
        }
      }
    }

    if (!s_ptt_pressed && verifying) {
      s_capture_active = 0;
      (void)bb_audio_stop_tx();
      while (!s_capture_stopped) {
        vTaskDelay(pdMS_TO_TICKS(5));
      }
      {
        size_t rb_item_size = 0;
        void* rb_item;
        while ((rb_item = xRingbufferReceive(s_capture_rb, &rb_item_size, 0)) != NULL) {
          vad_update_from_pcm(&verify_vad, (const uint8_t*)rb_item, rb_item_size);
          if (voice_verify_capture_append((const uint8_t*)rb_item, rb_item_size) != ESP_OK) {
            ESP_LOGE(TAG, "voice verify capture append failed");
          }
          vRingbufferReturnItem(s_capture_rb, rb_item);
        }
      }

      show_status_processing(BB_STATUS_VERIFY);
      {
        uint64_t duration_ms = (verify_vad.total_samples * 1000ULL) / BBCLAW_AUDIO_SAMPLE_RATE;
        uint32_t nonzero_permille = 0;
        uint32_t mean_abs = 0;
        if (verify_vad.total_samples > 0U) {
          nonzero_permille = (uint32_t)((verify_vad.nonzero_samples * 1000ULL) / verify_vad.total_samples);
          mean_abs = (uint32_t)(verify_vad.abs_sum / verify_vad.total_samples);
        }
        ESP_LOGI(TAG,
                 "phase=voice_verify_capture_done mono_ms=%lld pcm_bytes=%u truncated=%d dur_ms=%u nonzero_permille=%u "
                 "mean_abs=%u",
                 (long long)bb_now_ms(), (unsigned)s_voice_verify_pcm_len, s_voice_verify_truncated,
                 (unsigned)duration_ms, (unsigned)nonzero_permille, (unsigned)mean_abs);
        if (duration_ms < BBCLAW_VAD_ARM_MIN_DURATION_MS || nonzero_permille < BBCLAW_VAD_ARM_MIN_NONZERO_PERMILLE ||
            mean_abs < BBCLAW_VAD_ARM_MIN_MEAN_ABS || s_voice_verify_pcm_len == 0U) {
          show_status_error(BB_STATUS_VERIFY_ERR);
          (void)bb_display_show_chat_turn("未检测到有效声音", "请重试");
          signal_error_haptic();
          verifying = 0;
          session_busy = 0;
          voice_verify_capture_reset();
          vTaskDelay(pdMS_TO_TICKS(1200));
          show_status_idle(BB_STATUS_LOCKED);
          continue;
        }
      }

      bb_voice_verify_result_t verify_result = {0};
      esp_err_t verify_err = bb_adapter_voice_verify_pcm16(s_voice_verify_pcm_buf, s_voice_verify_pcm_len, &verify_result);
      verifying = 0;
      session_busy = 0;
      voice_verify_capture_reset();
      if (verify_err != ESP_OK) {
        ESP_LOGE(TAG, "voice verify failed err=%s", esp_err_to_name(verify_err));
        show_status_error(BB_STATUS_VERIFY_ERR);
        (void)bb_display_show_chat_turn("密语验证失败",
                                        verify_result.message[0] != '\0' ? verify_result.message : "cloud verify error");
        signal_error_haptic();
        vTaskDelay(pdMS_TO_TICKS(1200));
        show_status_idle(BB_STATUS_LOCKED);
        continue;
      }

      if (verify_result.match) {
        ESP_LOGI(TAG, "phase=voice_verify_unlock confidence=%.3f", (double)verify_result.confidence);
        set_radio_app_state(BBCLAW_STATE_UNLOCKED);
        pulse_success_on_idle(BB_STATUS_READY);
        (void)bb_display_show_chat_turn("密语验证通过",
                                        verify_result.message[0] != '\0' ? verify_result.message : "设备已解锁");
      } else {
        ESP_LOGW(TAG, "phase=voice_verify_reject confidence=%.3f message=%s", (double)verify_result.confidence,
                 verify_result.message);
        show_status_error(BB_STATUS_VERIFY_ERR);
        (void)bb_display_show_chat_turn("密语未匹配",
                                        verify_result.message[0] != '\0' ? verify_result.message : "请重试");
        signal_error_haptic();
        vTaskDelay(pdMS_TO_TICKS(1200));
        show_status_idle(BB_STATUS_LOCKED);
      }
      continue;
    }

    if (!s_ptt_pressed && streaming) {
      /* Stop capture_task and drain remaining audio from ring buffer. */
      s_capture_active = 0;
      (void)bb_audio_stop_tx();
      while (!s_capture_stopped) {
        vTaskDelay(pdMS_TO_TICKS(5));
      }
      if (s_capture_seed_pending) {
        s_capture_seed_pending = 0;
        int ingest_ok = 1;
        (void)stream_ingest_pcm(&stream, &vad, s_capture_seed_buf, s_capture_seed_len, &pending_pcm_len, &ingest_ok);
      }
      {
        size_t rb_item_size = 0;
        void* rb_item;
        while ((rb_item = xRingbufferReceive(s_capture_rb, &rb_item_size, 0)) != NULL) {
          int ingest_ok = 1;
          (void)stream_ingest_pcm(&stream, &vad, (const uint8_t*)rb_item, rb_item_size, &pending_pcm_len, &ingest_ok);
          vRingbufferReturnItem(s_capture_rb, rb_item);
          if (!ingest_ok) {
            break;
          }
        }
      }

      int skip_finish = 0;
#if BBCLAW_VAD_ENABLE
      uint64_t duration_ms = (vad.total_samples * 1000ULL) / BBCLAW_AUDIO_SAMPLE_RATE;
      uint32_t nonzero_permille = 0;
      uint32_t mean_abs = 0;
      if (vad.total_samples > 0U) {
        nonzero_permille = (uint32_t)((vad.nonzero_samples * 1000ULL) / vad.total_samples);
        mean_abs = (uint32_t)(vad.abs_sum / vad.total_samples);
      }
      uint64_t ptt_hold_ms = stream_start_ms > 0 ? (uint64_t)(bb_now_ms() - stream_start_ms) : 0;
      ESP_LOGI(TAG,
               "phase=vad mono_ms=%lld stream=%s ptt_hold_ms=%u captured_ms=%u samples=%llu nonzero_permille=%u "
               "mean_abs=%u",
               (long long)bb_now_ms(), stream.stream_id, (unsigned)ptt_hold_ms, (unsigned)duration_ms,
               (unsigned long long)vad.total_samples, (unsigned)nonzero_permille, (unsigned)mean_abs);
      if (duration_ms < BBCLAW_VAD_MIN_DURATION_MS || nonzero_permille < BBCLAW_VAD_MIN_NONZERO_PERMILLE ||
          mean_abs < BBCLAW_VAD_MIN_MEAN_ABS) {
        skip_finish = 1;
        ESP_LOGW(TAG,
                 "vad blocked asr dur_ms=%u nonzero_permille=%u mean_abs=%u threshold={%u,%u,%u}",
                 (unsigned)duration_ms, (unsigned)nonzero_permille, (unsigned)mean_abs, BBCLAW_VAD_MIN_DURATION_MS,
                 BBCLAW_VAD_MIN_NONZERO_PERMILLE, BBCLAW_VAD_MIN_MEAN_ABS);
      }
#endif

      bb_finish_result_t* finish = (bb_finish_result_t*)heap_caps_calloc(1, sizeof(bb_finish_result_t), BBCLAW_MALLOC_CAP_PREFER_PSRAM);
      bb_reply_stream_ui_ctx_t* ui_stream = (bb_reply_stream_ui_ctx_t*)heap_caps_calloc(1, sizeof(bb_reply_stream_ui_ctx_t), BBCLAW_MALLOC_CAP_PREFER_PSRAM);
      int tts_interrupted = 0;
      if (finish == NULL || ui_stream == NULL) {
        ESP_LOGE(TAG, "finish/ui_stream heap alloc failed");
        show_status_error(BB_STATUS_ERR);
        signal_error_haptic();
        skip_finish = 1;
        free(finish);
        free(ui_stream);
        finish = NULL;
        ui_stream = NULL;
      } else if (tts_stream_ui_init(ui_stream) != ESP_OK) {
        ESP_LOGW(TAG, "tts stream ui init failed; keep compatibility playback path");
      }
#if BBCLAW_ENABLE_TTS_PLAYBACK
      const char* tts_text = NULL;
      int tts_streamed = 0; /* 1 if cloud sent tts.chunk events */
#endif
      if (!skip_finish) {
        if (flush_stream_chunk(&stream, s_stream_pcm_chunk_buf, &pending_pcm_len) != ESP_OK) {
          ESP_LOGE(TAG, "flush pending chunk failed before finish");
          show_status_error(BB_STATUS_ERR);
          signal_error_haptic();
          skip_finish = 1;
        }
      }
      if (!skip_finish) {
        int64_t t_cloud_req_ms = bb_now_ms();
        ESP_LOGI(TAG, "phase=cloud_wait mono_ms=%lld stream=%s (RX + PROCESSING LED)", (long long)t_cloud_req_ms,
                 stream.stream_id);
        ESP_LOGI(TAG, "phase=cloud_wait_stack stream=%s stack_hw=%u", stream.stream_id,
                 (unsigned)uxTaskGetStackHighWaterMark(NULL));
        esp_err_t finish_err =
            bb_adapter_stream_finish_stream(&stream, finish, on_finish_stream_event, ui_stream);
        if (finish_err == ESP_OK) {
          int64_t t_cloud_done_ms = bb_now_ms();
          unsigned cloud_latency_ms = (unsigned)(t_cloud_done_ms - t_cloud_req_ms);
          const char* asr_text = finish->transcript[0] != '\0' ? finish->transcript : "(empty)";
          const char* reply_text = finish->reply_text[0] != '\0' ? finish->reply_text : "";

          ESP_LOGI(TAG, "phase=cloud_done mono_ms=%lld latency_ms=%u stream=%s", (long long)t_cloud_done_ms,
                   cloud_latency_ms, stream.stream_id);
          if (finish->saved_input_path[0] != '\0') {
            ESP_LOGI(TAG, "phase=asr_saved path=%s", finish->saved_input_path);
          }
          log_phase_text_chunks("phase=asr text=", asr_text);
          if (reply_text[0] != '\0') {
            log_phase_text_chunks("phase=assistant text=", reply_text);
          }

          if (reply_text[0] != '\0') {
            (void)bb_display_show_status(BB_STATUS_RESULT);
            (void)bb_led_set_status(BB_LED_REPLY);
          } else if (finish->transcript[0] != '\0') {
            (void)bb_display_show_status(BB_STATUS_RESULT);
            (void)bb_led_set_status(BB_LED_SUCCESS);
          } else {
            (void)bb_display_show_status(BB_STATUS_RESULT);
            (void)bb_led_set_status(BB_LED_SUCCESS);
          }

          {
            const char* line_you = finish->transcript[0] != '\0' ? finish->transcript : "(no speech)";
            const char* line_ai = reply_text[0] != '\0' ? reply_text : "(no reply)";
            int tts_active = (ui_stream != NULL && ui_stream->tts_playback_started && !ui_stream->tts_task_done);
            (void)bb_display_upsert_chat_turn(line_you, line_ai, tts_active ? 0 : 1);
          }

          if (finish->transcript[0] != '\0' && reply_text[0] == '\0') {
            (void)bb_gateway_node_send_voice_transcript(finish->transcript, BBCLAW_SESSION_KEY, stream.stream_id);
          }
#if BBCLAW_ENABLE_TTS_PLAYBACK
          tts_text = reply_text[0] != '\0' ? reply_text : (finish->transcript[0] != '\0' ? finish->transcript : NULL);
          tts_streamed = (ui_stream != NULL && ui_stream->tts_chunk_received > 0) || (finish->tts_chunks != NULL);
#endif
        } else {
          ESP_LOGE(TAG,
                   "phase=finish_failed esp=%s http_status=%d error_code=%s stream=%s reply_wait_timed_out=%d",
                   esp_err_to_name(finish_err), finish->http_status,
                   finish->error_code[0] != '\0' ? finish->error_code : "(none)", stream.stream_id,
                   finish->reply_wait_timed_out);
          {
            const char* ecode = finish->error_code[0] != '\0' ? finish->error_code : BB_STATUS_ERR;
            show_status_error(ecode);
            (void)bb_display_upsert_chat_turn("(error)", ecode, 1);
          }
          signal_error_haptic();
        }
      } else {
        show_idle_ready_or_locked();
        (void)bb_display_show_chat_turn("(no voice)", "(no reply)");
      }
#if BBCLAW_ENABLE_TTS_PLAYBACK
      {
        int streamed_async_active = (ui_stream != NULL && ui_stream->tts_playback_started);
        if (!streamed_async_active) {
          (void)bb_audio_stop_tx(); /* already stopped above; safe no-op unless async TTS already took over TX */
        } else {
          ESP_LOGI(TAG, "phase=tts_async_keep_tx chunks=%d done=%d", ui_stream->tts_chunk_received,
                   ui_stream->tts_done_received);
        }
      }
#else
      (void)bb_audio_stop_tx(); /* already stopped above; safe no-op for error paths */
#endif
      stream_start_ms = 0;
      pending_pcm_len = 0;

#if BBCLAW_ENABLE_TTS_PLAYBACK
      if (!skip_finish && tts_text != NULL && tts_text[0] != '\0') {
        if (ui_stream != NULL && ui_stream->tts_chunk_received > 0) {
          /* Streamed TTS is already being played asynchronously by the queue task. */
          ESP_LOGI(TAG, "phase=tts_stream_async chunks=%d done=%d", ui_stream->tts_chunk_received,
                   ui_stream->tts_done_received);
        } else if (bb_transport_is_cloud_saas() && !s_transport_tts_ready) {
          ESP_LOGW(TAG, "cloud tts unavailable, skip playback");
        } else if (tts_streamed && finish != NULL && finish->tts_chunks != NULL) {
          /* Play pre-synthesized TTS chunks from streaming finish. */
          ESP_LOGI(TAG, "phase=tts_stream_play mono_ms=%lld (playing streamed chunks)", (long long)bb_now_ms());
          (void)bb_display_show_status(BB_STATUS_SPEAK);
          (void)bb_led_set_status(BB_LED_REPLY);
          bb_display_set_tts_playing(1);
          esp_err_t tts_tx = bb_audio_start_playback();
          if (tts_tx == ESP_OK) {
            tts_playback_set_active(1);
            int chunk_idx = 0;
            int64_t play_start_ms = bb_now_ms();
            size_t total_pcm_bytes = 0;
            for (bb_tts_chunk_t* c = finish->tts_chunks; c != NULL; c = c->next) {
              chunk_idx++;
              total_pcm_bytes += c->pcm_len;
              int effective_rate = (c->sample_rate > 0) ? c->sample_rate : BBCLAW_AUDIO_SAMPLE_RATE;
              int expected_ms = (int)(c->pcm_len / 2 * 1000 / effective_rate);
              if (c->sample_rate > 0 && c->sample_rate != BBCLAW_AUDIO_SAMPLE_RATE) {
                (void)bb_audio_set_playback_sample_rate(c->sample_rate);
              }
              int64_t chunk_start = bb_now_ms();
              ESP_LOGI(TAG, "phase=tts_chunk_play seq=%d pcm_bytes=%u rate=%d ch=%d expected_ms=%d",
                       chunk_idx, (unsigned)c->pcm_len, effective_rate, c->channels, expected_ms);
              if (bb_audio_play_pcm_blocking(c->pcm_data, c->pcm_len) != ESP_OK) {
                if (tts_interrupt_requested()) {
                  tts_interrupted = 1;
                  ESP_LOGI(TAG, "phase=tts_chunk_interrupt seq=%d", chunk_idx);
                } else {
                  ESP_LOGE(TAG, "tts chunk play failed seq=%d", chunk_idx);
                  signal_error_haptic();
                }
                break;
              }
              int64_t chunk_elapsed = bb_now_ms() - chunk_start;
              ESP_LOGI(TAG, "phase=tts_chunk_done seq=%d actual_ms=%lld expected_ms=%d ratio_x100=%lld",
                       chunk_idx, (long long)chunk_elapsed, expected_ms,
                       expected_ms > 0 ? (long long)(chunk_elapsed * 100 / expected_ms) : 0LL);
            }
            int64_t total_elapsed = bb_now_ms() - play_start_ms;
            int total_expected = (int)(total_pcm_bytes / 2 * 1000 / BBCLAW_AUDIO_SAMPLE_RATE);
            ESP_LOGI(TAG, "phase=tts_play_summary chunks=%d total_bytes=%u total_actual_ms=%lld total_expected_ms=%d ratio_x100=%lld",
                     chunk_idx, (unsigned)total_pcm_bytes, (long long)total_elapsed, total_expected,
                     total_expected > 0 ? (long long)(total_elapsed * 100 / total_expected) : 0LL);
            (void)bb_audio_stop_playback();
            /* Restore I2S clock to capture sample rate. */
            (void)bb_audio_set_playback_sample_rate(BBCLAW_AUDIO_SAMPLE_RATE);
            tts_playback_set_active(0);
          } else {
            ESP_LOGE(TAG, "bb_audio_start_tx failed err=%s (TTS stream playback)", esp_err_to_name(tts_tx));
            signal_error_haptic();
          }
          bb_display_set_tts_playing(0);
        } else if (bb_transport_is_cloud_saas()) {
          ESP_LOGW(TAG, "cloud_saas expected streamed TTS over ws, skip fallback synth");
        } else {
          /* Fallback: synthesize full text in one shot. */
          bb_tts_audio_t tts = {0};
          if (bb_adapter_tts_synthesize_pcm16(tts_text, &tts) == ESP_OK && tts.pcm_data != NULL && tts.pcm_len > 0U) {
            ESP_LOGI(TAG, "phase=tts_play mono_ms=%lld pcm_bytes=%u (REPLY pulse then SPEAK)", (long long)bb_now_ms(),
                     (unsigned)tts.pcm_len);
            (void)bb_display_show_status(BB_STATUS_SPEAK);
            (void)bb_led_set_status(BB_LED_REPLY);
            bb_display_set_tts_playing(1);
            esp_err_t tts_tx = bb_audio_start_playback();
            if (tts_tx == ESP_OK) {
              tts_playback_set_active(1);
              if (bb_audio_play_pcm_blocking(tts.pcm_data, tts.pcm_len) != ESP_OK) {
                if (tts_interrupt_requested()) {
                  tts_interrupted = 1;
                  ESP_LOGI(TAG, "phase=tts_interrupt_fallback");
                } else {
                  ESP_LOGE(TAG, "bb_audio_play_pcm_blocking failed during TTS");
                  signal_error_haptic();
                }
              }
              (void)bb_audio_stop_playback();
              tts_playback_set_active(0);
            } else {
              ESP_LOGE(TAG, "bb_audio_start_tx failed err=%s (TTS playback)", esp_err_to_name(tts_tx));
              signal_error_haptic();
            }
            bb_display_set_tts_playing(0);
          } else {
            signal_error_haptic();
          }
          bb_adapter_tts_audio_free(&tts);
        }
      }
#endif
      /* Release Opus encoder if skip_finish bypassed stream_finish_stream. */
      if (stream.ws_encoder != NULL) {
        bb_ogg_opus_encoder_destroy((bb_ogg_opus_encoder_t*)stream.ws_encoder);
        stream.ws_encoder = NULL;
      }
      if (finish != NULL) {
        bb_adapter_tts_chunks_free(finish->tts_chunks);
        finish->tts_chunks = NULL;
      }
      if (ui_stream != NULL) {
        tts_stream_ui_shutdown(ui_stream, ui_stream->tts_done_received ? 0 : 1);
        /* Finalize chat turn now that TTS is done — clears stream_turn_active */
        if (ui_stream->tts_playback_started && finish != NULL) {
          const char* line_you = finish->transcript[0] != '\0' ? finish->transcript : "(no speech)";
          const char* line_ai = finish->reply_text[0] != '\0' ? finish->reply_text : "(no reply)";
          (void)bb_display_upsert_chat_turn(line_you, line_ai, 1);
        }
        if (ui_stream->tts_playback_interrupted) {
          tts_interrupted = 1;
        }
      }
      free(finish);
      free(ui_stream);
      /* RESULT 停留：TTS 播放期间用户已经在听，只需短暂停留让屏幕文字可读 */
      if (!skip_finish && !tts_interrupted) {
        (void)bb_display_show_status(BB_STATUS_RESULT);
        vTaskDelay(pdMS_TO_TICKS(2000));
      }
      s_tts_interrupt_requested = 0;
      bb_audio_clear_playback_interrupt();
      show_idle_ready_or_locked();
      streaming = 0;
      session_busy = 0;
      capture_seed_clear();
      vTaskDelay(pdMS_TO_TICKS(120));
      continue;
    }

    if (arming && !streaming && s_ptt_pressed) {
      if (BBCLAW_VAD_ARM_MAX_WAIT_MS > 0 && (bb_now_ms() - arm_started_ms) > BBCLAW_VAD_ARM_MAX_WAIT_MS) {
        ESP_LOGW(TAG, "phase=arm_timeout mono_ms=%lld", (long long)bb_now_ms());
        arming = 0;
        (void)bb_audio_stop_tx();
        memset(&vad, 0, sizeof(vad));
        pending_pcm_len = 0;
        show_idle_ready_or_locked();
        (void)bb_display_show_chat_turn("(arm timeout)", "");
        continue;
      }
      size_t pcm_read = 0;
      esp_err_t read_err =
          bb_audio_read_pcm_frame(s_stream_task_pcm_read_buf, sizeof(s_stream_task_pcm_read_buf), &pcm_read);
      if (read_err != ESP_OK) {
        ESP_LOGE(TAG, "pcm read failed err=%s", esp_err_to_name(read_err));
        show_status_error(BB_STATUS_ERR);
        signal_error_haptic();
        arming = 0;
        (void)bb_audio_stop_tx();
        memset(&vad, 0, sizeof(vad));
        pending_pcm_len = 0;
        continue;
      }
      if (pcm_read > 0U) {
        update_record_level_from_pcm(s_stream_task_pcm_read_buf, pcm_read);
        vad_update_from_pcm(&vad, s_stream_task_pcm_read_buf, pcm_read);
        if (vad_passes_thresholds(&vad, BBCLAW_VAD_ARM_MIN_DURATION_MS, BBCLAW_VAD_ARM_MIN_NONZERO_PERMILLE,
                                  BBCLAW_VAD_ARM_MIN_MEAN_ABS)) {
          esp_err_t start_err = bb_adapter_stream_start(&stream);
          if (start_err != ESP_OK) {
            ESP_LOGE(TAG, "bb_adapter_stream_start failed esp=%s (after VAD arm)", esp_err_to_name(start_err));
            show_status_error(BB_STATUS_ERR);
            signal_error_haptic();
            arming = 0;
            (void)bb_audio_stop_tx();
            memset(&vad, 0, sizeof(vad));
            pending_pcm_len = 0;
            vTaskDelay(pdMS_TO_TICKS(200));
            continue;
          }
          arming = 0;
          streaming = 1;
          session_busy = 1;
          /* Keep arm-phase VAD stats — that audio is valid speech. */
          stream_start_ms = bb_now_ms();
          pending_pcm_len = 0;
          ESP_LOGI(TAG, "phase=record_start mono_ms=%lld stream=%s (after VAD arm)", (long long)bb_now_ms(),
                   stream.stream_id);
          /* 首帧不再 xRingbufferSend，避免与显示任务跨核争用环缓冲锁触发 INT WDT；见 s_capture_seed_* */
          if (pcm_read > sizeof(s_capture_seed_buf)) {
            pcm_read = sizeof(s_capture_seed_buf);
          }
          if (pcm_read > 0U) {
            memcpy(s_capture_seed_buf, s_stream_task_pcm_read_buf, pcm_read);
            s_capture_seed_len = pcm_read;
            s_capture_seed_pending = 1;
          } else {
            capture_seed_clear();
          }
          s_capture_active = 1;
        }
      }
    }

    if (verifying && s_ptt_pressed) {
      size_t rb_item_size = 0;
      void* rb_item = xRingbufferReceive(s_capture_rb, &rb_item_size, pdMS_TO_TICKS(50));
      if (rb_item != NULL && rb_item_size > 0U) {
        vad_update_from_pcm(&verify_vad, (const uint8_t*)rb_item, rb_item_size);
        if (voice_verify_capture_append((const uint8_t*)rb_item, rb_item_size) != ESP_OK) {
          /* Buffer alloc may fail on no-PSRAM boards; keep verifying so
           * VAD stats accumulate.  On PTT release the pcm_bytes==0 path
           * will show a user-friendly "no valid sound" error instead of
           * the rapid restart loop caused by aborting here. */
          if (s_voice_verify_pcm_buf == NULL) {
            static int s_verify_buf_warn_logged;
            if (!s_verify_buf_warn_logged) {
              s_verify_buf_warn_logged = 1;
              ESP_LOGW(TAG, "voice verify pcm buffer unavailable (no PSRAM?), VAD-only mode");
            }
          }
        }
        vRingbufferReturnItem(s_capture_rb, rb_item);
      }
    }

    if (streaming) {
      if (s_capture_seed_pending) {
        s_capture_seed_pending = 0;
        int ingest_ok = 1;
        esp_err_t ige =
            stream_ingest_pcm(&stream, &vad, s_capture_seed_buf, s_capture_seed_len, &pending_pcm_len, &ingest_ok);
        if (ige != ESP_OK || !ingest_ok) {
          ESP_LOGE(TAG, "stream seed ingest failed");
          show_status_error(BB_STATUS_ERR);
          signal_error_haptic();
          s_capture_active = 0;
          capture_seed_clear();
          try_finish_stream_abort(&stream);
          streaming = 0;
          session_busy = 0;
          pending_pcm_len = 0;
          (void)bb_audio_stop_tx();
          continue;
        }
      }
      size_t rb_item_size = 0;
      void* rb_item = xRingbufferReceive(s_capture_rb, &rb_item_size, pdMS_TO_TICKS(50));
      if (rb_item != NULL && rb_item_size > 0U) {
        int ingest_ok = 1;
        if (stream_ingest_pcm(&stream, &vad, (const uint8_t*)rb_item, rb_item_size, &pending_pcm_len, &ingest_ok) !=
                ESP_OK ||
            !ingest_ok) {
          ESP_LOGE(TAG, "stream ingest failed");
          show_status_error(BB_STATUS_ERR);
          signal_error_haptic();
          s_capture_active = 0;
          capture_seed_clear();
          try_finish_stream_abort(&stream);
          streaming = 0;
          session_busy = 0;
          pending_pcm_len = 0;
          (void)bb_audio_stop_tx();
          vRingbufferReturnItem(s_capture_rb, rb_item);
          continue;
        }
        vRingbufferReturnItem(s_capture_rb, rb_item);
      }
    }

    {
      int64_t now_ms = bb_now_ms();
      if (now_ms - last_power_poll_ms >= BBCLAW_POWER_POLL_INTERVAL_MS) {
        last_power_poll_ms = now_ms;
        if (bb_power_refresh() == ESP_OK) {
          refresh_power_display();
        }
      }
    }

#if BBCLAW_ENABLE_DISPLAY_PULL
    if (!streaming && !s_ptt_pressed) {
      int64_t now_ms = bb_now_ms();
      if (now_ms - last_display_poll_ms >= 1500) {
        bb_display_task_t task = {0};
        last_display_poll_ms = now_ms;
        if (bb_transport_is_cloud_saas() && !s_transport_display_ready) {
          /* Cloud health says display queue is unavailable; skip until next heartbeat refresh. */
        } else if (bb_adapter_display_pull(&task) == ESP_OK && task.has_task) {
          show_status_notification(BB_STATUS_TASK);
          (void)bb_display_show_chat_turn("Task", task.display_text[0] != '\0' ? task.display_text : "(empty)");
          if (!cloud_saas_tx_wait_is_benign()) {
            (void)bb_motor_trigger(BB_MOTOR_PATTERN_TASK_NOTIFY);
          }
          (void)bb_adapter_display_ack(task.task_id, "shown");
        }
      }
    }
#endif

    if (!streaming && !s_ptt_pressed) {
      /* If WiFi dropped and entered provisioning, show AP info and block PTT */
      if (bb_wifi_is_provisioning_mode()) {
        show_status_error(BB_STATUS_NO_WIFI);
        char ap_line[64];
        char hint_line[64];
        snprintf(ap_line, sizeof(ap_line), "AP %s", bb_wifi_get_ap_ssid());
        snprintf(hint_line, sizeof(hint_line), "%s PWD %s", bb_wifi_get_ap_ip(), bb_wifi_get_ap_password());
        (void)bb_display_show_chat_turn(ap_line, hint_line);
        s_transport_health_ok = 0;
        adapter_health_is_up = 0;
        vTaskDelay(pdMS_TO_TICKS(2000));
        continue;
      }

      int64_t now_ms = bb_now_ms();
      if (now_ms - last_adapter_heartbeat_ms >= adapter_heartbeat_interval_ms) {
        int health_status = 0;
        int prev_ready = s_transport_ready;
        int prev_audio_ready = s_transport_audio_streaming_ready;
        int prev_tts_ready = s_transport_tts_ready;
        int prev_display_ready = s_transport_display_ready;
        int prev_http_status = s_transport_http_status;
        char prev_detail[sizeof(s_transport_detail)];
        snprintf(prev_detail, sizeof(prev_detail), "%s", s_transport_detail);
        char prev_reg[sizeof(s_transport_registration_code)];
        snprintf(prev_reg, sizeof(prev_reg), "%s", s_transport_registration_code);
        last_adapter_heartbeat_ms = now_ms;
        bb_transport_state_t state = {0};
        esp_err_t health_err = bb_transport_refresh_state(&state);
        health_status = state.http_status;
        s_transport_ready = state.ready;
        s_transport_audio_streaming_ready = state.supports_audio_streaming;
        s_transport_tts_ready = state.supports_tts;
        s_transport_display_ready = state.supports_display;
        remember_transport_state(&state);
        if (health_err == ESP_OK && bb_transport_is_cloud_saas()) {
          if (state.cloud_volume_pct >= 0) {
            static int s_applied_cloud_volume_pct = -1;
            if (state.cloud_volume_pct != s_applied_cloud_volume_pct) {
              s_applied_cloud_volume_pct = state.cloud_volume_pct;
              bb_audio_set_volume_pct(state.cloud_volume_pct);
            }
          }
          if (state.cloud_speaker_enabled >= 0) {
            static int s_applied_cloud_speaker_enabled = -1;
            if (state.cloud_speaker_enabled != s_applied_cloud_speaker_enabled) {
              s_applied_cloud_speaker_enabled = state.cloud_speaker_enabled;
              bb_audio_set_speaker_enabled(state.cloud_speaker_enabled);
            }
          }
        }
        if (health_err == ESP_OK) {
          adapter_health_fail_streak = 0;
          s_transport_health_ok = 1;
          if (!adapter_health_is_up) {
            adapter_health_is_up = 1;
            ESP_LOGI(TAG, "transport heartbeat recovered profile=%s status=%d", bb_transport_profile_name(),
                     health_status);
            if (bb_transport_is_cloud_saas()) {
              show_cloud_transport_or_locked(&state);
            } else {
              pulse_success_on_idle(BB_STATUS_READY);
            }
          } else if (bb_transport_is_cloud_saas() &&
                     (prev_ready != state.ready || prev_audio_ready != state.supports_audio_streaming ||
                      prev_tts_ready != state.supports_tts || prev_display_ready != state.supports_display ||
                      prev_http_status != state.http_status || strcmp(prev_detail, state.detail) != 0 ||
                      strcmp(prev_reg, state.cloud_registration_code) != 0)) {
            ESP_LOGI(TAG, "cloud transport state updated status=%d ready=%d audio=%d tts=%d display=%d detail=%s",
                     health_status, state.ready, state.supports_audio_streaming, state.supports_tts,
                     state.supports_display, state.detail);
            show_cloud_transport_or_locked(&state);
          }
        } else {
          adapter_health_fail_streak++;
          if (adapter_health_fail_streak == 1 || (adapter_health_fail_streak % 5) == 0) {
            ESP_LOGW(TAG, "transport heartbeat failed profile=%s streak=%d err=%s status=%d",
                     bb_transport_profile_name(), adapter_health_fail_streak,
                     esp_err_to_name(health_err), health_status);
          }
          if (adapter_health_is_up && adapter_health_fail_streak >= adapter_heartbeat_fail_threshold) {
            /* Adapter was up but now gone down — mark as down and show error */
            adapter_health_is_up = 0;
            s_transport_health_ok = 0;
            if (bb_transport_is_cloud_saas()) {
              show_status_error("CLOUD ERR");
              (void)bb_display_show_chat_turn("Cloud unreachable", "Check " BBCLAW_CLOUD_BASE_URL);
            } else {
              show_status_error("LINK ERR");
              (void)bb_display_show_chat_turn("Local adapter unreachable", "Check " BBCLAW_ADAPTER_BASE_URL);
            }
          } else if (!bb_transport_is_cloud_saas() && !adapter_health_is_up && adapter_health_fail_streak == 1) {
            /* local_home: adapter never came up — show error immediately on first failure */
            show_status_error("LINK ERR");
            (void)bb_display_show_chat_turn("Local adapter unreachable", "Check " BBCLAW_ADAPTER_BASE_URL);
          } else if (bb_transport_is_cloud_saas() && !adapter_health_is_up && adapter_health_fail_streak == 1) {
            /* cloud_saas: cloud never came up — show error immediately on first failure */
            show_status_error("CLOUD ERR");
            (void)bb_display_show_chat_turn("Cloud unreachable", "Check " BBCLAW_CLOUD_BASE_URL);
          }
        }
      }
    }

    if (!streaming) {
      vTaskDelay(pdMS_TO_TICKS(20));
    }
  }
}

esp_err_t bb_radio_app_start(void) {
  bb_gateway_node_config_t node_cfg = {
      .node_id = BBCLAW_NODE_ID,
      .gateway_url = BBCLAW_GATEWAY_URL,
      .pairing_token = BBCLAW_PAIRING_TOKEN,
  };
  int health_status = 0;
  esp_err_t audio_err = ESP_OK;

  ESP_LOGI(TAG, "boot node_id=%s", BBCLAW_NODE_ID);
  ESP_LOGI(TAG, "transport=%s adapter=%s cloud=%s codec=%s", bb_transport_profile_name(), BBCLAW_ADAPTER_BASE_URL,
           BBCLAW_CLOUD_BASE_URL, BBCLAW_STREAM_CODEC);
  log_pin_summary();

  ESP_ERROR_CHECK(bb_display_init());
  bb_display_set_cloud_mode(bb_transport_is_cloud_saas());
  refresh_power_display();
  set_radio_app_state(passphrase_unlock_enabled() ? BBCLAW_STATE_LOCKED : BBCLAW_STATE_UNLOCKED);
  {
    esp_err_t power_err = bb_power_init();
    if (power_err != ESP_OK) {
      ESP_LOGW(TAG, "power init failed err=%s (continue without battery sensing)", esp_err_to_name(power_err));
    } else if (bb_power_refresh() == ESP_OK) {
      refresh_power_display();
    }
  }
  esp_err_t led_err = bb_led_init();
  if (led_err != ESP_OK) {
    ESP_LOGW(TAG, "status led init failed err=%s (continue without led)", esp_err_to_name(led_err));
  } else {
    show_status_processing(BB_STATUS_BOOT);
  }
  esp_err_t motor_err = bb_motor_init();
  if (motor_err != ESP_OK) {
    ESP_LOGW(TAG, "motor init failed err=%s (continue without haptics)", esp_err_to_name(motor_err));
  } else {
    /* boot self-test: one strong pulse so user knows motor works */
    (void)bb_motor_trigger(BB_MOTOR_PATTERN_PTT_PRESS);
  }
  audio_err = bb_audio_init();
  if (audio_err != ESP_OK) {
    ESP_LOGE(TAG, "audio init failed err=%s", esp_err_to_name(audio_err));
    show_status_error("AUDIO ERR");
    return audio_err;
  }
#if BBCLAW_XL9555_ENABLE
  {
    esp_err_t xl_err = bb_xl9555_init();
    if (xl_err != ESP_OK) {
      ESP_LOGW(TAG, "xl9555 init failed err=%s", esp_err_to_name(xl_err));
    } else {
      (void)bb_xl9555_set_output(BBCLAW_XL9555_SPK_EN_BIT, 1);
      (void)bb_xl9555_set_output(BBCLAW_XL9555_AMP_EN_BIT, 1);
    }
  }
#endif
#if BBCLAW_SPK_TEST_ON_BOOT
  show_status_processing("SPK TEST");
  if (bb_audio_start_playback() == ESP_OK) {
    esp_err_t spk_err = bb_play_embedded_boot_wav();
    if (spk_err != ESP_OK) {
      ESP_LOGW(TAG, "boot wav playback failed err=%s, fallback to tone", esp_err_to_name(spk_err));
      (void)bb_audio_play_test_tone(1000, 350, 5000);
    }
    (void)bb_audio_stop_playback();
  }
#endif
  ESP_ERROR_CHECK(bb_ptt_init(BBCLAW_PTT_GPIO, on_ptt_changed));
  ESP_ERROR_CHECK(bb_nav_input_init(on_nav_event));
  if (bb_button_test_start() != ESP_OK) {
    ESP_LOGW(TAG, "button test start failed (optional)");
  }
  ESP_ERROR_CHECK(bb_gateway_node_init(&node_cfg));
  ESP_ERROR_CHECK(bb_gateway_node_connect());

  show_status_processing(BB_STATUS_WIFI);
  esp_err_t wifi_err = bb_wifi_init_and_connect();
  if (wifi_err != ESP_OK) {
    ESP_LOGE(TAG, "wifi init failed err=%s", esp_err_to_name(wifi_err));
    show_status_error(BB_STATUS_WIFI_ERR);
    return wifi_err;
  }
  if (bb_wifi_is_provisioning_mode()) {
    char ap_line[BBCLAW_DISPLAY_CHAT_LINE_LEN];
    char hint_line[BBCLAW_DISPLAY_CHAT_LINE_LEN];
    snprintf(ap_line, sizeof(ap_line), "AP %s", bb_wifi_get_ap_ssid());
    if (bb_wifi_get_ap_password()[0] != '\0') {
      snprintf(hint_line, sizeof(hint_line), "%s PWD %s", bb_wifi_get_ap_ip(), bb_wifi_get_ap_password());
    } else {
      snprintf(hint_line, sizeof(hint_line), "%s OPEN", bb_wifi_get_ap_ip());
    }
    show_status_processing(BB_STATUS_WIFI_AP);
    (void)bb_display_show_chat_turn(ap_line, hint_line);
    ESP_LOGW(TAG, "wifi provisioning mode active ssid=%s ip=%s", bb_wifi_get_ap_ssid(), bb_wifi_get_ap_ip());
    return ESP_OK;
  }
  bb_sntp_start();
  show_status_processing("LINK...");
  esp_err_t health_err = wait_for_transport_health(&health_status);
  if (health_err == ESP_OK) {
    s_transport_health_ok = 1;
    if (bb_transport_is_cloud_saas()) {
      bb_transport_state_t state = {
          .ready = s_transport_ready,
          .supports_audio_streaming = s_transport_audio_streaming_ready,
          .supports_tts = s_transport_tts_ready,
          .supports_display = s_transport_display_ready,
          .http_status = s_transport_http_status != 0 ? s_transport_http_status : health_status,
          .pairing_status = s_transport_pairing_status,
      };
      snprintf(state.detail, sizeof(state.detail), "%s", s_transport_detail);
      snprintf(state.cloud_registration_code, sizeof(state.cloud_registration_code), "%s",
               s_transport_registration_code);
      snprintf(state.cloud_registration_expires_at, sizeof(state.cloud_registration_expires_at), "%s",
               s_transport_registration_expires_at);
      if (s_transport_ready && s_transport_audio_streaming_ready) {
        ESP_LOGI(TAG, "cloud transport ready status=%d", health_status);
        esp_err_t rpt = bb_transport_report_device_info();
        if (rpt != ESP_OK) {
          ESP_LOGW(TAG, "device info report failed err=%s (non-fatal)", esp_err_to_name(rpt));
        }
        /* Silent OTA check: download and flash if update available */
        if (bb_transport_is_cloud_saas()) {
          ota_update_info_t ota_info = {0};
          esp_err_t ota_err = bb_ota_check(&ota_info);
          if (ota_err == ESP_OK && ota_info.has_update) {
            ESP_LOGI(TAG, "OTA update available: version=%s size=%u", ota_info.version, ota_info.size);
            (void)bb_display_show_chat_turn("Updating...", ota_info.version);
            esp_err_t dl_err = bb_ota_download_and_flash(&ota_info, NULL);
            if (dl_err == ESP_OK) {
              ESP_LOGI(TAG, "OTA download+flash success, rebooting...");
              (void)bb_ota_apply_update();
              /* Never returns */
            } else {
              ESP_LOGW(TAG, "OTA download+flash failed err=%s", esp_err_to_name(dl_err));
            }
          }
        }
      } else {
        ESP_LOGI(TAG, "cloud pairing requested and pending approval status=%d detail=%s", health_status,
                 state.detail);
      }
      show_cloud_transport_or_locked(&state);
    } else {
      ESP_LOGI(TAG, "transport health ok status=%d", health_status);
      pulse_success_on_idle(BB_STATUS_READY);
      (void)bb_display_show_chat_turn("", "");
    }
  } else {
    s_transport_health_ok = 0;
    ESP_LOGW(TAG, "transport unavailable after boot retries profile=%s err=%s status=%d", bb_transport_profile_name(),
             esp_err_to_name(health_err), health_status);
    if (bb_transport_is_cloud_saas()) {
      bb_transport_state_t state = {
          .ready = s_transport_ready,
          .supports_audio_streaming = s_transport_audio_streaming_ready,
          .supports_tts = s_transport_tts_ready,
          .supports_display = s_transport_display_ready,
          .http_status = s_transport_http_status != 0 ? s_transport_http_status : health_status,
          .pairing_status = s_transport_pairing_status,
      };
      snprintf(state.detail, sizeof(state.detail), "%s", s_transport_detail);
      snprintf(state.cloud_registration_code, sizeof(state.cloud_registration_code), "%s",
               s_transport_registration_code);
      snprintf(state.cloud_registration_expires_at, sizeof(state.cloud_registration_expires_at), "%s",
               s_transport_registration_expires_at);
      show_cloud_transport_or_locked(&state);
    } else {
      show_status_error("LINK ERR");
      (void)bb_display_show_chat_turn("Local adapter unreachable", "Check " BBCLAW_ADAPTER_BASE_URL);
    }
  }

  /* Show OTA celebration if we just updated */
  if (bb_ota_was_just_updated()) {
    ESP_LOGI(TAG, "OTA celebration: just updated!");
    (void)bb_display_show_chat_turn("更新成功!", "新版本已安装");
    vTaskDelay(pdMS_TO_TICKS(2000));
  }

  log_heap_snapshot("before ringbuf");
  s_capture_rb = xRingbufferCreateWithCaps(CAPTURE_RINGBUF_SIZE, RINGBUF_TYPE_BYTEBUF,
                                           MALLOC_CAP_8BIT);
  if (s_capture_rb == NULL) {
    ESP_LOGE(TAG, "capture ring buffer alloc failed size=%u", (unsigned)CAPTURE_RINGBUF_SIZE);
    return ESP_ERR_NO_MEM;
  }
  ESP_LOGI(TAG, "capture ringbuf created size=%u chunks=%d mem=psram", (unsigned)CAPTURE_RINGBUF_SIZE,
           BBCLAW_CAPTURE_RINGBUF_CHUNKS);
  log_heap_snapshot("after ringbuf");

  /* 采集与 stream 同核，减少环缓冲跨核自旋锁与 LVGL(SPI) 争用导致的 INT WDT */
#ifdef CONFIG_FREERTOS_UNICORE
  BaseType_t ok = xTaskCreate(capture_task, "bb_capture_task", 4096, NULL, 7, NULL);
#else
  BaseType_t ok = xTaskCreatePinnedToCore(capture_task, "bb_capture_task", 4096, NULL, 7, NULL, 0);
#endif
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "capture task create failed stack=4096 free=%u largest=%u", (unsigned)esp_get_free_heap_size(),
             (unsigned)heap_caps_get_largest_free_block(MALLOC_CAP_8BIT));
    vRingbufferDeleteWithCaps(s_capture_rb);
    s_capture_rb = NULL;
    return ESP_ERR_NO_MEM;
  }
  log_heap_snapshot("after capture task");
  /* Stream task drives Opus encode plus cloud finish/JSON parsing. The first
   * encode burst can go materially deeper than the steady-state watermark,
   * so keep a larger PSRAM-backed stack here. */
#ifdef CONFIG_FREERTOS_UNICORE
  ok = xTaskCreateWithCaps(stream_task, "bb_stream_task", BB_STREAM_TASK_STACK, NULL, 5, NULL, BBCLAW_MALLOC_CAP_PREFER_PSRAM);
#else
  ok = xTaskCreatePinnedToCoreWithCaps(stream_task, "bb_stream_task", BB_STREAM_TASK_STACK, NULL, 5, NULL, 0,
                                       BBCLAW_MALLOC_CAP_PREFER_PSRAM);
#endif
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "stream task create failed stack=%u free=%u largest=%u", (unsigned)BB_STREAM_TASK_STACK,
             (unsigned)esp_get_free_heap_size(), (unsigned)heap_caps_get_largest_free_block(MALLOC_CAP_8BIT));
    vRingbufferDeleteWithCaps(s_capture_rb);
    s_capture_rb = NULL;
    return ESP_ERR_NO_MEM;
  }
  log_heap_snapshot("after stream task");
  return ESP_OK;
}
