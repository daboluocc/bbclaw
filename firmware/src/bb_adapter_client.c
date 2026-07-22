#include "bb_adapter_client.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>

#include "bb_audio.h" /* ADR-042 §3.2: play notification TTS (synth+blocking play) */
#include "bb_bbwire2.h"
#include "bb_prompt.h"
#include "bb_device_config.h"
#include "bb_notification.h"
#include "bb_power_mgmt.h" /* ADR-046 §5: 消息到达唤醒息屏 */
#include "bb_ui_agent_chat.h" /* ADR-040: reconcile turn.committed/superseded into chat UI */
#include "bb_ogg_opus.h"
#include "bb_config.h"
#include "bb_time.h"
#include "bb_transport.h"
#include "esp_check.h"
#include "esp_crt_bundle.h"
#include "esp_heap_caps.h"
#include "esp_http_client.h"
#include "esp_log.h"
#include "esp_system.h"
#include "esp_websocket_client.h"
#include "freertos/event_groups.h"
#include "freertos/queue.h"
#include "freertos/semphr.h"
#include "freertos/task.h"
#include "mbedtls/base64.h"

static const char* TAG = "bb_adapter";

static void log_mem_snapshot(const char* phase) {
  ESP_LOGI(TAG,
           "mem %s total_free=%u total_largest=%u internal_free=%u internal_largest=%u spiram_free=%u "
           "spiram_largest=%u",
           phase != NULL ? phase : "(unknown)", (unsigned)esp_get_free_heap_size(),
           (unsigned)heap_caps_get_largest_free_block(MALLOC_CAP_8BIT),
           (unsigned)heap_caps_get_free_size(MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT),
           (unsigned)heap_caps_get_largest_free_block(MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT),
           (unsigned)heap_caps_get_free_size(BBCLAW_MALLOC_CAP_PREFER_PSRAM),
           (unsigned)heap_caps_get_largest_free_block(BBCLAW_MALLOC_CAP_PREFER_PSRAM));
}

static const char* active_base_url(void) {
  if (strcasecmp(BBCLAW_TRANSPORT_PROFILE, "cloud_saas") == 0) {
    return BBCLAW_CLOUD_BASE_URL;
  }
  if (bb_transport_is_v2()) {
    /* HTTP origin of adapter_v2 (for /healthz readiness): map ws→http, wss→https
     * from BBCLAW_ADAPTER_V2_BASE_URL. adapter_v2 serves /healthz on the same
     * origin as /v2/dev/ws. Computed once into a static buffer. */
    static char v2_http[200];
    if (v2_http[0] == '\0') {
      const char* b = BBCLAW_ADAPTER_V2_BASE_URL;
      if (strncmp(b, "wss://", 6) == 0) {
        snprintf(v2_http, sizeof(v2_http), "https://%s", b + 6);
      } else if (strncmp(b, "ws://", 5) == 0) {
        snprintf(v2_http, sizeof(v2_http), "http://%s", b + 5);
      } else {
        snprintf(v2_http, sizeof(v2_http), "%s", b);
      }
    }
    return v2_http;
  }
  return BBCLAW_ADAPTER_BASE_URL;
}

/** Embedded ISRG Root X1 CA for Let's Encrypt RSA chain verification. */
extern const uint8_t isrg_root_x1_pem_start[] asm("_binary_isrg_root_x1_pem_start");
extern const uint8_t isrg_root_x1_pem_end[]   asm("_binary_isrg_root_x1_pem_end");

/** Populate common esp_http_client_config_t fields; auto-attach TLS for https URLs. */
static inline void bb_http_cfg_init(esp_http_client_config_t* cfg, const char* url, int timeout_ms,
                                    esp_http_client_method_t method, http_event_handle_cb handler, void* user_data) {
  memset(cfg, 0, sizeof(*cfg));
  cfg->url = url;
  cfg->timeout_ms = timeout_ms > 0 ? timeout_ms : BBCLAW_HTTP_TIMEOUT_MS;
  cfg->method = method;
  cfg->transport_type = HTTP_TRANSPORT_OVER_TCP;
  cfg->event_handler = handler;
  cfg->user_data = user_data;
  if (strncasecmp(url, "https", 5) == 0) {
    cfg->crt_bundle_attach = esp_crt_bundle_attach;
  }
}

typedef struct {
  int status_code;
  char body[1024];
} bb_http_resp_t;

typedef struct {
  bb_http_resp_t* resp;
  size_t offset;
} bb_http_accum_t;

typedef struct {
  int status_code;
  char* body;
  size_t body_len;
} bb_http_dyn_resp_t;

typedef struct {
  char* buf;
  size_t len;
  size_t cap;
} bb_http_dyn_accum_t;

typedef struct {
  bb_finish_result_t* result;
  bb_finish_stream_event_cb_t on_event;
  void* user_ctx;
  char* buf;
  size_t len;
  size_t cap;
  int saw_done;
  int saw_error;
} bb_finish_stream_accum_t;

/* ADR-027: in-flight state for a synchronous sites.list / sites.activate WS
 * request. Filled by ws_handle_text_message on the matching reply/error. */
typedef struct {
  int kind;              /* 0 = sites.list, 1 = sites.activate */
  bb_site_info_t* sites; /* caller buffer (list only) */
  int max;
  int count;             /* out: parsed site count */
  char active_id[40];    /* out: activate's activeHomeSiteId */
  char err_code[32];     /* out: error code, empty on success */
  int ok;                /* out: 1 = success */
} bb_sites_req_t;

typedef struct {
  esp_websocket_client_handle_t client;
  SemaphoreHandle_t lock;
  EventGroupHandle_t events;
  int initialized;
  int connected;
  bb_finish_result_t* finish_result;
  bb_finish_stream_event_cb_t finish_on_event;
  void* finish_user_ctx;
  int finish_waiting;
  /* 本回合最后一次流式活动（guard 通过的任何回合事件帧,含二进制 TTS）。
   * 等待循环据此做空闲超时;s_ws.lock 保护(S3 上 int64 读写非原子)。 */
  int64_t finish_last_activity_ms;
  int finish_saw_done;
  int finish_saw_error;
  char finish_stream_id[64];
  bb_voice_verify_result_t* verify_result;
  int verify_waiting;
  char verify_message_id[64];
  bb_sites_req_t* sites_req;
  int sites_waiting;
  char sites_message_id[64];
  /* ADR-044 P1b ambient 回网补传:单飞行段的 ack 等待槽(bb_ambient_sync 任务
   * 独占使用;arm 后发 segment.finish,云端 ambient.segment.ack/ambient.error
   * 落到这里)。 */
  char ambient_session[48];
  int ambient_seq;
  volatile int ambient_waiting;
  int ambient_ok;
  char ambient_err[32];
  uint8_t* text_buf;
  size_t text_len;
  size_t text_cap;
  uint8_t text_opcode;
  uint8_t* tts_audio_buf;
  size_t tts_audio_len;
  size_t tts_audio_cap;
  char tts_stream_id[64];
} bb_ws_state_t;

static bb_ws_state_t s_ws;

#define BB_WS_EVENT_CONNECTED BIT0
#define BB_WS_EVENT_DONE BIT1
#define BB_WS_EVENT_ERROR BIT2
#define BB_WS_EVENT_DISCONNECTED BIT3
#define BB_WS_EVENT_VERIFY_DONE BIT4
#define BB_WS_EVENT_SITES_DONE BIT5
#define BB_WS_EVENT_ABORT BIT6 /* ADR-028: PTT barge-in 本地中断 finish 等待 */
#define BB_WS_EVENT_AMBIENT_DONE BIT7 /* ADR-044 P1b: ambient 段 ack/错误已到 */

static int body_contains_ok_true(const char* body);
static int json_extract_string(const char* body, const char* key, char* out, size_t out_len);
static int json_extract_bool(const char* body, const char* key, int fallback);
static float json_extract_float(const char* body, const char* key, float fallback);
static int json_extract_alloc_string(const char* body, const char* key, char** out_ptr, size_t* out_len);
static char* json_escape_alloc(const char* src);
static void parse_finish_result(const char* body, bb_finish_result_t* out_result);
static void parse_voice_verify_result(const char* body, bb_voice_verify_result_t* out_result);
static void parse_finish_stream_line(const char* line, bb_finish_stream_accum_t* accum);
static void ws_finish_reset_locked(void);
static void ws_verify_reset_locked(void);
static void ws_sites_reset_locked(void);
static int parse_sites_array(const char* body, bb_site_info_t* out, int max);
static void ws_reset_client_locked(void);
static esp_err_t ws_client_ensure_connected(void);
static esp_err_t ws_send_text_message(const char* payload);
static esp_err_t ws_send_binary_message(const uint8_t* data, size_t len);
static void ws_handle_text_message(const char* msg);
static void ws_handle_binary_message(const uint8_t* data, size_t len);
static void ws_event_handler(void* handler_args, esp_event_base_t base, int32_t event_id, void* event_data);

static void emit_finish_stream_event(bb_finish_stream_event_cb_t cb, void* user_ctx, bb_finish_stream_event_type_t type,
                                     const char* phase, const char* text, bb_tts_chunk_t* tts_chunk,
                                     int reply_wait_timed_out) {
  if (cb == NULL) {
    return;
  }
  bb_finish_stream_event_t event = {
      .type = type,
      .phase = phase,
      .text = text,
      .tts_chunk = tts_chunk,
      .reply_wait_timed_out = reply_wait_timed_out,
      .dispatch = NULL,
  };
  cb(&event, user_ctx);
}

/* ADR-021-firmware-ui §1.2: emit a dispatch_status event from a parsed
 * bb_dispatch_status_t. Separate helper to avoid touching every call site of
 * emit_finish_stream_event. */
static void emit_dispatch_event(bb_finish_stream_event_cb_t cb, void* user_ctx,
                                const bb_dispatch_status_t* ds) {
  if (cb == NULL || ds == NULL) return;
  bb_finish_stream_event_t event = {
      .type = BB_FINISH_STREAM_EVENT_DISPATCH_STATUS,
      .phase = ds->phase,
      .text = NULL,
      .tts_chunk = NULL,
      .reply_wait_timed_out = 0,
      .dispatch = ds,
  };
  cb(&event, user_ctx);
}

/* ADR-030: emit a TOOL_CALL step carrying both the tool name and a short hint
 * (command / file path). Separate helper so the hint field doesn't ripple
 * through every emit_finish_stream_event call site. */
static void emit_tool_call_event(bb_finish_stream_event_cb_t cb, void* user_ctx,
                                 const char* name, const char* hint) {
  if (cb == NULL || name == NULL || name[0] == '\0') return;
  bb_finish_stream_event_t event = {
      .type = BB_FINISH_STREAM_EVENT_TOOL_CALL,
      .phase = NULL,
      .text = name,
      .hint = hint,
      .tts_chunk = NULL,
      .reply_wait_timed_out = 0,
      .dispatch = NULL,
  };
  cb(&event, user_ctx);
}

/* ADR-033: emit a PROMPT_OPEN/PROMPT_CLOSE event carrying the parsed menu. */
static void emit_prompt_event(bb_finish_stream_event_cb_t cb, void* user_ctx,
                              bb_finish_stream_event_type_t type, const bb_prompt_t* prompt) {
  if (cb == NULL || prompt == NULL) return;
  bb_finish_stream_event_t event = {
      .type = type,
      .phase = NULL,
      .text = NULL,
      .tts_chunk = NULL,
      .reply_wait_timed_out = 0,
      .dispatch = NULL,
      .prompt = prompt,
  };
  cb(&event, user_ctx);
}

/* ADR-033: send the device's answer to a forwarded blocking menu over the cloud
 * WS — a prompt.select request the cloud routes to the home adapter (same
 * fire-and-forget envelope shape as agent.sessions.activate). */
esp_err_t bb_adapter_send_prompt_select(const char* prompt_id, const char* option_key) {
  if (prompt_id == NULL || option_key == NULL || prompt_id[0] == '\0' || option_key[0] == '\0') {
    return ESP_ERR_INVALID_ARG;
  }
  char body[224];
  snprintf(body, sizeof(body),
           "{\"type\":\"request\",\"kind\":\"prompt.select\",\"messageId\":\"psel-%lld\","
           "\"deviceId\":\"%s\",\"payload\":{\"promptId\":\"%s\",\"optionKey\":\"%s\"}}",
           (long long)bb_now_ms(), BBCLAW_DEVICE_ID, prompt_id, option_key);
  esp_err_t err = ws_client_ensure_connected();
  if (err == ESP_OK) {
    err = ws_send_text_message(body);
  }
  ESP_LOGI(TAG, "prompt.select sent err=%s id=%s key=%s", esp_err_to_name(err), prompt_id, option_key);
  return err;
}

/* ADR-049: send a device photo to the home adapter over the cloud WS. The JPEG is
 * base64'd into an image.capture request envelope; the cloud relays it to the bound
 * adapter, which stashes the file and runs a multimodal turn (claude reads the
 * image). Big payload → PSRAM buffers (base64 ≈ 4/3 × jpeg, envelope a bit more). */
esp_err_t bb_adapter_send_image_capture(const uint8_t* jpeg, size_t jpeg_len, uint16_t width,
                                        uint16_t height, const char* note) {
  if (jpeg == NULL || jpeg_len == 0) {
    return ESP_ERR_INVALID_ARG;
  }
  size_t b64_cap = 4 * ((jpeg_len + 2) / 3) + 1;
  char* b64 = heap_caps_malloc(b64_cap, MALLOC_CAP_SPIRAM);
  if (b64 == NULL) {
    ESP_LOGE(TAG, "image.capture: b64 alloc %u failed", (unsigned)b64_cap);
    return ESP_ERR_NO_MEM;
  }
  size_t b64_len = 0;
  int rc = mbedtls_base64_encode((unsigned char*)b64, b64_cap, &b64_len, jpeg, jpeg_len);
  if (rc != 0) {
    free(b64);
    ESP_LOGE(TAG, "image.capture: base64 encode failed rc=%d", rc);
    return ESP_FAIL;
  }
  size_t body_cap = b64_len + 512;
  char* body = heap_caps_malloc(body_cap, MALLOC_CAP_SPIRAM);
  if (body == NULL) {
    free(b64);
    ESP_LOGE(TAG, "image.capture: body alloc %u failed", (unsigned)body_cap);
    return ESP_ERR_NO_MEM;
  }
  /* note 仅用受控文案（无引号/反斜杠），故直接内联不做 JSON 转义。 */
  int n = snprintf(body, body_cap,
                   "{\"type\":\"request\",\"kind\":\"image.capture\",\"messageId\":\"img-%lld\","
                   "\"deviceId\":\"%s\",\"payload\":{\"format\":\"jpeg\",\"width\":%u,\"height\":%u,"
                   "\"bytes\":%u,\"note\":\"%s\",\"dataBase64\":\"%.*s\"}}",
                   (long long)bb_now_ms(), BBCLAW_DEVICE_ID, (unsigned)width, (unsigned)height,
                   (unsigned)jpeg_len, note ? note : "", (int)b64_len, b64);
  free(b64);
  esp_err_t err = (n > 0 && (size_t)n < body_cap) ? ESP_OK : ESP_ERR_INVALID_SIZE;
  if (err == ESP_OK) {
    err = ws_client_ensure_connected();
  }
  if (err == ESP_OK) {
    err = ws_send_text_message(body);
  }
  free(body);
  ESP_LOGI(TAG, "image.capture sent err=%s jpeg=%u b64=%u %ux%u", esp_err_to_name(err),
           (unsigned)jpeg_len, (unsigned)b64_len, (unsigned)width, (unsigned)height);
  return err;
}

static int json_extract_int(const char* body, const char* key, int fallback) {
  if (body == NULL || key == NULL || key[0] == '\0') {
    return fallback;
  }
  char pattern[48] = {0};
  snprintf(pattern, sizeof(pattern), "\"%s\":", key);
  const char* p = strstr(body, pattern);
  if (p == NULL) {
    return fallback;
  }
  p += strlen(pattern);
  while (*p == ' ' || *p == '\t') {
    p++;
  }
  return atoi(p);
}

static void append_result_tts_chunk(bb_finish_result_t* result, bb_tts_chunk_t* chunk) {
  if (result == NULL || chunk == NULL) {
    return;
  }
  chunk->next = NULL;
  if (result->tts_chunks_tail != NULL) {
    result->tts_chunks_tail->next = chunk;
  } else {
    result->tts_chunks = chunk;
  }
  result->tts_chunks_tail = chunk;
}

/* TTS chunk structs + PCM payloads are plain data (no DMA, not executable):
 * playback copies them into the I2S DMA buffer via i2s_channel_write, so the
 * source can live in PSRAM. Force them there. With
 * CONFIG_SPIRAM_MALLOC_ALWAYSINTERNAL=16384 a bare malloc() of a <16KB PCM
 * chunk lands in INTERNAL RAM; a long reply churns dozens of ~10KB chunks and
 * fragments internal DRAM down to a ~256B largest block, which then starves
 * TLS/lwip and FreeRTOS task creation (barge-in cancel "no mem"). PSRAM has
 * megabytes free; fall back to internal only if PSRAM is exhausted. */
static void* tts_alloc(size_t n) {
  void* p = heap_caps_malloc(n, BBCLAW_MALLOC_CAP_PREFER_PSRAM);
  if (p == NULL) {
    p = heap_caps_malloc(n, MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT);
  }
  return p;
}

static void* tts_calloc(size_t n) {
  void* p = tts_alloc(n);
  if (p != NULL) {
    memset(p, 0, n);
  }
  return p;
}

static bb_tts_chunk_t* clone_tts_chunk(const bb_tts_chunk_t* src) {
  if (src == NULL || src->pcm_data == NULL || src->pcm_len == 0U) {
    return NULL;
  }
  bb_tts_chunk_t* copy = (bb_tts_chunk_t*)tts_calloc(sizeof(bb_tts_chunk_t));
  if (copy == NULL) {
    return NULL;
  }
  copy->pcm_data = (uint8_t*)tts_alloc(src->pcm_len);
  if (copy->pcm_data == NULL) {
    free(copy);
    return NULL;
  }
  memcpy(copy->pcm_data, src->pcm_data, src->pcm_len);
  copy->pcm_len = src->pcm_len;
  copy->sample_rate = src->sample_rate;
  copy->channels = src->channels;
  copy->seq = src->seq;
  memcpy(copy->tts_text, src->tts_text, sizeof(copy->tts_text));
  return copy;
}

static bb_tts_chunk_t* decode_tts_chunk_json(const char* body) {
  char* audio_b64 = NULL;
  size_t audio_b64_len = 0;
  if (!json_extract_alloc_string(body, "audioBase64", &audio_b64, &audio_b64_len) || audio_b64_len == 0U) {
    ESP_LOGW(TAG, "tts.chunk missing audioBase64");
    free(audio_b64);
    return NULL;
  }
  size_t pcm_cap = 0;
  int ret = mbedtls_base64_decode(NULL, 0, &pcm_cap, (const unsigned char*)audio_b64, audio_b64_len);
  if (ret != MBEDTLS_ERR_BASE64_BUFFER_TOO_SMALL || pcm_cap == 0U) {
    ESP_LOGW(TAG, "tts.chunk base64 size probe failed");
    free(audio_b64);
    return NULL;
  }
  bb_tts_chunk_t* chunk = (bb_tts_chunk_t*)tts_calloc(sizeof(bb_tts_chunk_t));
  if (chunk == NULL) {
    free(audio_b64);
    return NULL;
  }
  chunk->pcm_data = (uint8_t*)tts_alloc(pcm_cap);
  if (chunk->pcm_data == NULL) {
    free(chunk);
    free(audio_b64);
    return NULL;
  }
  size_t pcm_len = 0;
  ret = mbedtls_base64_decode(chunk->pcm_data, pcm_cap, &pcm_len, (const unsigned char*)audio_b64, audio_b64_len);
  free(audio_b64);
  if (ret != 0 || pcm_len == 0U) {
    free(chunk->pcm_data);
    free(chunk);
    return NULL;
  }
  chunk->pcm_len = pcm_len;
  chunk->sample_rate = json_extract_int(body, "sampleRate", 16000);
  chunk->channels = json_extract_int(body, "channels", 1);
  chunk->seq = json_extract_int(body, "seq", 0);
  json_extract_string(body, "text", chunk->tts_text, sizeof(chunk->tts_text));
  return chunk;
}

static void dispatch_tts_chunk_event(bb_finish_result_t* result, bb_finish_stream_event_cb_t cb, void* user_ctx,
                                     bb_tts_chunk_t* chunk) {
  if (chunk == NULL) {
    return;
  }
  if (cb != NULL) {
    bb_tts_chunk_t* copy = clone_tts_chunk(chunk);
    if (copy != NULL) {
      if (result != NULL) {
        append_result_tts_chunk(result, copy);
      } else {
        bb_adapter_tts_chunks_free(copy);
      }
    }
    emit_finish_stream_event(cb, user_ctx, BB_FINISH_STREAM_EVENT_TTS_CHUNK, NULL, NULL, chunk, 0);
    return;
  }
  /* No event callback (agent-bus path / abort path) → no TTS consumer.
   * Previously we still appended to result->tts_chunks, which the cloud's
   * 25 s reply piles into a multi-hundred-KB linked list of malloc'd PCM
   * data, freed only after cloud_wait returns. Drop instead. */
  bb_adapter_tts_chunks_free(chunk);
}

static esp_err_t http_event_handler(esp_http_client_event_t* evt) {
  bb_http_accum_t* accum = (bb_http_accum_t*)evt->user_data;
  if (accum == NULL || accum->resp == NULL) {
    return ESP_OK;
  }

  if (evt->event_id == HTTP_EVENT_ON_DATA && evt->data != NULL && evt->data_len > 0) {
    size_t cap = sizeof(accum->resp->body) - 1;
    if (accum->offset < cap) {
      size_t remain = cap - accum->offset;
      size_t n = (size_t)evt->data_len;
      if (n > remain) {
        n = remain;
      }
      memcpy(accum->resp->body + accum->offset, evt->data, n);
      accum->offset += n;
      accum->resp->body[accum->offset] = '\0';
    }
  }
  return ESP_OK;
}

static esp_err_t http_event_handler_dyn(esp_http_client_event_t* evt) {
  bb_http_dyn_accum_t* accum = (bb_http_dyn_accum_t*)evt->user_data;
  if (accum == NULL) {
    return ESP_OK;
  }
  if (evt->event_id != HTTP_EVENT_ON_DATA || evt->data == NULL || evt->data_len <= 0) {
    return ESP_OK;
  }

  size_t need = accum->len + (size_t)evt->data_len + 1;
  if (need > accum->cap) {
    size_t new_cap = accum->cap == 0 ? 2048 : accum->cap;
    while (new_cap < need) {
      new_cap *= 2;
    }
    char* new_buf = (char*)realloc(accum->buf, new_cap);
    if (new_buf == NULL) {
      return ESP_ERR_NO_MEM;
    }
    accum->buf = new_buf;
    accum->cap = new_cap;
  }

  memcpy(accum->buf + accum->len, evt->data, (size_t)evt->data_len);
  accum->len += (size_t)evt->data_len;
  accum->buf[accum->len] = '\0';
  return ESP_OK;
}

static esp_err_t http_post_json_with_timeout(const char* path, const char* payload, bb_http_resp_t* out_resp,
                                             int timeout_ms) {
  char url[256] = {0};
  snprintf(url, sizeof(url), "%s%s", active_base_url(), path);
  memset(out_resp, 0, sizeof(*out_resp));
  bb_http_accum_t accum = {.resp = out_resp, .offset = 0};
  if (timeout_ms <= 0) {
    timeout_ms = BBCLAW_HTTP_TIMEOUT_MS;
  }

  esp_http_client_config_t cfg;
  bb_http_cfg_init(&cfg, url, timeout_ms, HTTP_METHOD_POST, http_event_handler, &accum);

  esp_http_client_handle_t client = esp_http_client_init(&cfg);
  if (client == NULL) {
    return ESP_ERR_NO_MEM;
  }

  esp_http_client_set_header(client, "Content-Type", "application/json");

  esp_http_client_set_post_field(client, payload, (int)strlen(payload));

  esp_err_t err = esp_http_client_perform(client);
  if (err != ESP_OK) {
    esp_http_client_cleanup(client);
    return err;
  }

  out_resp->status_code = esp_http_client_get_status_code(client);
  esp_http_client_cleanup(client);
  return ESP_OK;
}

static esp_err_t http_post_json(const char* path, const char* payload, bb_http_resp_t* out_resp) {
  return http_post_json_with_timeout(path, payload, out_resp, BBCLAW_HTTP_TIMEOUT_MS);
}

static esp_err_t http_get(const char* path, bb_http_resp_t* out_resp) {
  char url[256] = {0};
  snprintf(url, sizeof(url), "%s%s", active_base_url(), path);
  memset(out_resp, 0, sizeof(*out_resp));
  bb_http_accum_t accum = {.resp = out_resp, .offset = 0};

  esp_http_client_config_t cfg;
  bb_http_cfg_init(&cfg, url, BBCLAW_HTTP_TIMEOUT_MS, HTTP_METHOD_GET, http_event_handler, &accum);

  esp_http_client_handle_t client = esp_http_client_init(&cfg);
  if (client == NULL) {
    return ESP_ERR_NO_MEM;
  }


  esp_err_t err = esp_http_client_perform(client);
  if (err != ESP_OK) {
    esp_http_client_cleanup(client);
    return err;
  }

  out_resp->status_code = esp_http_client_get_status_code(client);
  esp_http_client_cleanup(client);
  return ESP_OK;
}

static esp_err_t http_post_json_dynamic(const char* path, const char* payload, bb_http_dyn_resp_t* out_resp) {
  if (out_resp == NULL) {
    return ESP_ERR_INVALID_ARG;
  }
  memset(out_resp, 0, sizeof(*out_resp));

  char url[256] = {0};
  snprintf(url, sizeof(url), "%s%s", active_base_url(), path);
  bb_http_dyn_accum_t accum = {0};

  esp_http_client_config_t cfg;
  bb_http_cfg_init(&cfg, url, BBCLAW_HTTP_TIMEOUT_MS, HTTP_METHOD_POST, http_event_handler_dyn, &accum);

  esp_http_client_handle_t client = esp_http_client_init(&cfg);
  if (client == NULL) {
    return ESP_ERR_NO_MEM;
  }

  esp_http_client_set_header(client, "Content-Type", "application/json");

  esp_http_client_set_post_field(client, payload, (int)strlen(payload));

  esp_err_t err = esp_http_client_perform(client);
  if (err != ESP_OK) {
    esp_http_client_cleanup(client);
    free(accum.buf);
    return err;
  }

  out_resp->status_code = esp_http_client_get_status_code(client);
  out_resp->body = accum.buf;
  out_resp->body_len = accum.len;
  esp_http_client_cleanup(client);
  return ESP_OK;
}

static esp_err_t http_post_json_with_timeout_dynamic(const char* path, const char* payload, bb_http_dyn_resp_t* out_resp,
                                                     int timeout_ms) {
  if (out_resp == NULL) {
    return ESP_ERR_INVALID_ARG;
  }
  memset(out_resp, 0, sizeof(*out_resp));

  char url[256] = {0};
  snprintf(url, sizeof(url), "%s%s", active_base_url(), path);
  bb_http_dyn_accum_t accum = {0};

  if (timeout_ms <= 0) {
    timeout_ms = BBCLAW_HTTP_TIMEOUT_MS;
  }

  esp_http_client_config_t cfg;
  bb_http_cfg_init(&cfg, url, timeout_ms, HTTP_METHOD_POST, http_event_handler_dyn, &accum);

  esp_http_client_handle_t client = esp_http_client_init(&cfg);
  if (client == NULL) {
    return ESP_ERR_NO_MEM;
  }

  esp_http_client_set_header(client, "Content-Type", "application/json");

  esp_http_client_set_post_field(client, payload, (int)strlen(payload));

  esp_err_t err = esp_http_client_perform(client);
  if (err != ESP_OK) {
    esp_http_client_cleanup(client);
    free(accum.buf);
    return err;
  }

  out_resp->status_code = esp_http_client_get_status_code(client);
  out_resp->body = accum.buf;
  out_resp->body_len = accum.len;
  esp_http_client_cleanup(client);
  return ESP_OK;
}

static const char* active_ws_base_url(void) {
  return BBCLAW_CLOUD_BASE_URL;
}

static void build_cloud_ws_url(char* out, size_t out_len) {
  if (out == NULL || out_len == 0U) {
    return;
  }
  out[0] = '\0';
  const char* base = active_ws_base_url();
  const char* rest = base;
  const char* scheme = "ws://";
  if (strncmp(base, "https://", 8) == 0) {
    scheme = "wss://";
    rest = base + 8;
  } else if (strncmp(base, "http://", 7) == 0) {
    scheme = "ws://";
    rest = base + 7;
  } else if (strncmp(base, "wss://", 6) == 0) {
    scheme = "wss://";
    rest = base + 6;
  } else if (strncmp(base, "ws://", 5) == 0) {
    scheme = "ws://";
    rest = base + 5;
  }

  char path_part[128] = "/ws";
  const char* slash = strchr(rest, '/');
  char host_part[192] = {0};
  if (slash != NULL) {
    size_t host_len = (size_t)(slash - rest);
    if (host_len >= sizeof(host_part)) {
      host_len = sizeof(host_part) - 1U;
    }
    memcpy(host_part, rest, host_len);
    host_part[host_len] = '\0';
    if (strcmp(slash, "/ws") == 0 || strcmp(slash, "/ws/") == 0) {
      snprintf(path_part, sizeof(path_part), "/ws");
    } else {
      snprintf(path_part, sizeof(path_part), "%s/ws", slash);
    }
  } else {
    snprintf(host_part, sizeof(host_part), "%s", rest);
  }

  size_t host_len = strlen(host_part);
  while (host_len > 0U && host_part[host_len - 1U] == '/') {
    host_part[host_len - 1U] = '\0';
    host_len--;
  }

  snprintf(out, out_len, "%s%s%s?role=device&device_id=%s", scheme, host_part, path_part, BBCLAW_DEVICE_ID);
}

static void ws_finish_reset_locked(void) {
  s_ws.finish_result = NULL;
  s_ws.finish_last_activity_ms = 0;
  s_ws.finish_on_event = NULL;
  s_ws.finish_user_ctx = NULL;
  s_ws.finish_waiting = 0;
  s_ws.finish_saw_done = 0;
  s_ws.finish_saw_error = 0;
  s_ws.finish_stream_id[0] = '\0';
  s_ws.tts_stream_id[0] = '\0';
  s_ws.tts_audio_len = 0;
}

static void ws_verify_reset_locked(void) {
  s_ws.verify_result = NULL;
  s_ws.verify_waiting = 0;
  s_ws.verify_message_id[0] = '\0';
}

static void ws_sites_reset_locked(void) {
  s_ws.sites_req = NULL;
  s_ws.sites_waiting = 0;
  s_ws.sites_message_id[0] = '\0';
}

static void ws_reset_client_locked(void) {
  if (s_ws.client != NULL) {
    esp_websocket_client_destroy(s_ws.client);
    s_ws.client = NULL;
  }
  s_ws.connected = 0;
}

static esp_err_t ws_client_ensure_connected(void) {
  if (!bb_transport_is_cloud_saas()) {
    return ESP_ERR_NOT_SUPPORTED;
  }

  if (!s_ws.initialized) {
    memset(&s_ws, 0, sizeof(s_ws));
    log_mem_snapshot("ws init begin");
    s_ws.lock = xSemaphoreCreateMutex();
    s_ws.events = xEventGroupCreate();
    if (s_ws.lock == NULL || s_ws.events == NULL) {
      ESP_LOGE(TAG, "ws init alloc failed lock=%p events=%p", s_ws.lock, s_ws.events);
      log_mem_snapshot("ws init alloc failed");
      return ESP_ERR_NO_MEM;
    }
    s_ws.initialized = 1;
    log_mem_snapshot("ws init ready");
  }

  if (s_ws.connected && s_ws.client != NULL && esp_websocket_client_is_connected(s_ws.client)) {
    return ESP_OK;
  }

  xSemaphoreTake(s_ws.lock, portMAX_DELAY);
  if (s_ws.connected && s_ws.client != NULL && esp_websocket_client_is_connected(s_ws.client)) {
    xSemaphoreGive(s_ws.lock);
    return ESP_OK;
  }
  int need_start = 0;
  if (s_ws.client == NULL) {
    char ws_url[320] = {0};
    build_cloud_ws_url(ws_url, sizeof(ws_url));
    esp_websocket_client_config_t cfg = {
        .uri = ws_url,
        .buffer_size = 1024,
        .network_timeout_ms = BBCLAW_HTTP_TIMEOUT_MS,
        .reconnect_timeout_ms = 2000,
        .task_stack = 8192,
        .disable_auto_reconnect = false,
        .task_name = "bbclaw_ws",
        /* Keepalive: cloud closes the device WS after 35s of upstream silence
         * (cloud wsReadTimeout). During long agent turns the device only
         * receives TTS and sends nothing, so without periodic pings the relay
         * reaps an actively-served connection. Ping every 15s (well under 35s);
         * cloud replies pong and resets its read deadline. disable_pingpong_discon
         * keeps a late/dropped pong from triggering a *local* disconnect — dead
         * connections are still caught by the read-error path + auto-reconnect. */
        .ping_interval_sec = 15,
        .disable_pingpong_discon = true,
        .crt_bundle_attach = strncmp(ws_url, "wss", 3) == 0 ? esp_crt_bundle_attach : NULL,
    };
    s_ws.client = esp_websocket_client_init(&cfg);
    if (s_ws.client == NULL) {
      xSemaphoreGive(s_ws.lock);
      return ESP_ERR_NO_MEM;
    }
    esp_websocket_register_events(s_ws.client, WEBSOCKET_EVENT_ANY, ws_event_handler, NULL);
    need_start = 1;
  }
  xEventGroupClearBits(s_ws.events, BB_WS_EVENT_CONNECTED | BB_WS_EVENT_DISCONNECTED);
  ESP_LOGI(TAG, "ws ensure connect free_heap=%" PRIu32 " min_heap=%" PRIu32, esp_get_free_heap_size(),
           esp_get_minimum_free_heap_size());
  if (need_start && esp_websocket_client_start(s_ws.client) != ESP_OK) {
    ESP_LOGE(TAG, "ws start failed free_heap=%" PRIu32 " min_heap=%" PRIu32, esp_get_free_heap_size(),
             esp_get_minimum_free_heap_size());
    ws_reset_client_locked();
    xSemaphoreGive(s_ws.lock);
    return ESP_FAIL;
  }
  xSemaphoreGive(s_ws.lock);

  EventBits_t bits = xEventGroupWaitBits(s_ws.events, BB_WS_EVENT_CONNECTED | BB_WS_EVENT_DISCONNECTED, pdFALSE, pdFALSE,
                                         pdMS_TO_TICKS(BBCLAW_HTTP_TIMEOUT_MS));
  if ((bits & BB_WS_EVENT_CONNECTED) != 0U) {
    return ESP_OK;
  }
  xSemaphoreTake(s_ws.lock, portMAX_DELAY);
  ESP_LOGW(TAG, "ws connect timeout/disconnect free_heap=%" PRIu32 " min_heap=%" PRIu32, esp_get_free_heap_size(),
           esp_get_minimum_free_heap_size());
  ws_reset_client_locked();
  xSemaphoreGive(s_ws.lock);
  return ESP_FAIL;
}

/* Keep the cloud device WS up while idle so SERVER-INITIATED pushes (proactive
 * reminders / session.notification, ADR-042) can reach the device. The WS is
 * otherwise established lazily only when the device itself starts an op (PTT /
 * voice), so after boot — or after any drop — it stays disconnected until the
 * user speaks, and a notification firing in that window is buffered by the cloud
 * and never delivered (verified live: device did HTTP heartbeats but held no WS
 * → notifications enqueued offline). Called from the stream loop's heartbeat so
 * the device holds a persistent WS whenever it is awake + network-healthy. No-op
 * off cloud_saas; returns fast when already connected. */
esp_err_t bb_adapter_client_keep_ws_alive(void) {
  if (!bb_transport_is_cloud_saas()) {
    return ESP_ERR_NOT_SUPPORTED;
  }
  /* Fast path: already connected → nothing to do. Cheap + non-blocking. */
  if (s_ws.initialized && s_ws.client != NULL && esp_websocket_client_is_connected(s_ws.client)) {
    return ESP_OK;
  }
  /* Lazy one-time init of lock/events (mirror ws_client_ensure_connected). */
  if (!s_ws.initialized) {
    memset(&s_ws, 0, sizeof(s_ws));
    s_ws.lock = xSemaphoreCreateMutex();
    s_ws.events = xEventGroupCreate();
    if (s_ws.lock == NULL || s_ws.events == NULL) {
      return ESP_ERR_NO_MEM;
    }
    s_ws.initialized = 1;
  }
  /* CRUCIAL (fix for "重连后 UI 卡顿"): this runs on the stream/UI task's heartbeat.
   * ws_client_ensure_connected BLOCKS up to HTTP_TIMEOUT(5s) waiting for CONNECTED
   * (and destroys the client on timeout → re-blocks next tick), which freezes nav/UI
   * after a reconnect. Here we only START the client once if missing and return
   * IMMEDIATELY — esp_websocket auto-reconnects in its OWN task, so a single kick is
   * enough to keep it alive. If the client exists but is momentarily disconnected,
   * we leave it: it is already reconnecting on its own. Never wait, never destroy. */
  xSemaphoreTake(s_ws.lock, portMAX_DELAY);
  esp_err_t err = ESP_OK;
  if (s_ws.client == NULL) {
    char ws_url[320] = {0};
    build_cloud_ws_url(ws_url, sizeof(ws_url));
    esp_websocket_client_config_t cfg = {
        .uri = ws_url,
        .buffer_size = 1024,
        .network_timeout_ms = BBCLAW_HTTP_TIMEOUT_MS,
        .reconnect_timeout_ms = 2000,
        .task_stack = 8192,
        .disable_auto_reconnect = false,
        .task_name = "bbclaw_ws",
        .ping_interval_sec = 15,
        .disable_pingpong_discon = true,
        .crt_bundle_attach = strncmp(ws_url, "wss", 3) == 0 ? esp_crt_bundle_attach : NULL,
    };
    s_ws.client = esp_websocket_client_init(&cfg);
    if (s_ws.client == NULL) {
      xSemaphoreGive(s_ws.lock);
      return ESP_ERR_NO_MEM;
    }
    esp_websocket_register_events(s_ws.client, WEBSOCKET_EVENT_ANY, ws_event_handler, NULL);
    if (esp_websocket_client_start(s_ws.client) != ESP_OK) {
      ws_reset_client_locked();
      err = ESP_FAIL;
    }
  }
  xSemaphoreGive(s_ws.lock);
  return err;
}

static esp_err_t ws_send_text_message(const char* payload) {
  if (payload == NULL) {
    return ESP_ERR_INVALID_ARG;
  }
  ESP_RETURN_ON_ERROR(ws_client_ensure_connected(), TAG, "ws connect failed");
  int sent = esp_websocket_client_send_text(s_ws.client, payload, (int)strlen(payload), pdMS_TO_TICKS(5000));
  return sent >= 0 ? ESP_OK : ESP_FAIL;
}

esp_err_t bb_adapter_client_send_text(const char* payload) {
  return ws_send_text_message(payload);
}

/* ── ADR-044 P1b: ambient 回网补传原语（bb_ambient_sync 任务专用） ────── */

esp_err_t bb_adapter_client_send_bin(const uint8_t* data, size_t len) {
  return ws_send_binary_message(data, len);
}

void bb_adapter_client_ambient_arm_ack(const char* session_id, int seg_seq) {
  if (s_ws.lock == NULL || s_ws.events == NULL || session_id == NULL) {
    return;
  }
  xEventGroupClearBits(s_ws.events, BB_WS_EVENT_AMBIENT_DONE);
  xSemaphoreTake(s_ws.lock, portMAX_DELAY);
  snprintf(s_ws.ambient_session, sizeof(s_ws.ambient_session), "%s", session_id);
  s_ws.ambient_seq = seg_seq;
  s_ws.ambient_ok = 0;
  s_ws.ambient_err[0] = '\0';
  s_ws.ambient_waiting = 1;
  xSemaphoreGive(s_ws.lock);
}

int bb_adapter_client_ambient_wait_ack(int timeout_ms, char* err_out, size_t err_len) {
  if (err_out != NULL && err_len > 0U) {
    err_out[0] = '\0';
  }
  if (s_ws.lock == NULL || s_ws.events == NULL) {
    return 0;
  }
  EventBits_t bits =
      xEventGroupWaitBits(s_ws.events, BB_WS_EVENT_AMBIENT_DONE, pdFALSE, pdFALSE, pdMS_TO_TICKS(timeout_ms));
  xEventGroupClearBits(s_ws.events, BB_WS_EVENT_AMBIENT_DONE);
  xSemaphoreTake(s_ws.lock, portMAX_DELAY);
  int ok = 0;
  if ((bits & BB_WS_EVENT_AMBIENT_DONE) == 0U) {
    /* 超时(含掉线):解除等待,让迟到的 ack 不再动槽位 */
    s_ws.ambient_waiting = 0;
    if (err_out != NULL && err_len > 0U) {
      snprintf(err_out, err_len, "TIMEOUT");
    }
  } else {
    ok = s_ws.ambient_ok;
    if (!ok && err_out != NULL && err_len > 0U) {
      snprintf(err_out, err_len, "%s", s_ws.ambient_err);
    }
  }
  xSemaphoreGive(s_ws.lock);
  return ok;
}

void bb_adapter_abort_finish_wait(void) {
  /* 只在确有 finish 等待在飞时才设位,避免给下一轮 finish 留下 stale ABORT。
   * events 为 NULL(WS 未初始化)时静默返回。 */
  if (s_ws.events == NULL) {
    return;
  }
  if (s_ws.finish_waiting) {
    ESP_LOGI(TAG, "phase=finish_abort_local (barge-in)");
    xEventGroupSetBits(s_ws.events, BB_WS_EVENT_ABORT);
  }
}

/* ── ADR-028 §2.5.1 barge-in: fire-and-forget turn cancel ─────────────────
 * PTT 在任何状态按下都可能触发取消；调用点可能在 esp_timer 回调里，所以
 * 网络 IO 必须挪到一次性后台任务。同一时刻只允许一个 cancel 在飞。 */

static volatile int s_turn_cancel_inflight;

static void turn_cancel_task(void* arg) {
  char* played_text = (char*)arg; /* heap copy, may be NULL */
  char* escaped = json_escape_alloc(played_text != NULL ? played_text : "");
  char body[512];
  esp_err_t err;
  if (bb_transport_is_cloud_saas()) {
    snprintf(body, sizeof(body),
             "{\"type\":\"request\",\"kind\":\"turn.cancel\",\"messageId\":\"cancel-%lld\","
             "\"deviceId\":\"%s\",\"payload\":{\"playedText\":\"%s\"}}",
             (long long)bb_now_ms(), BBCLAW_DEVICE_ID, escaped != NULL ? escaped : "");
    err = ws_client_ensure_connected();
    if (err == ESP_OK) {
      err = ws_send_text_message(body);
    }
  } else {
    snprintf(body, sizeof(body), "{\"deviceId\":\"%s\",\"playedText\":\"%s\"}", BBCLAW_DEVICE_ID,
             escaped != NULL ? escaped : "");
    bb_http_resp_t resp;
    err = http_post_json("/v1/agent/cancel", body, &resp);
    if (err == ESP_OK && resp.status_code >= 400) {
      err = ESP_FAIL;
    }
  }
  ESP_LOGI(TAG, "phase=turn_cancel_sent err=%s played_chars=%u", esp_err_to_name(err),
           (unsigned)(played_text != NULL ? strlen(played_text) : 0U));
  free(escaped);
  free(played_text);
  s_turn_cancel_inflight = 0;
  /* 必须与 xTaskCreateWithCaps 配对,否则 caps 分配的 PSRAM 栈+TCB 不回收,
   * 每次打断泄漏 ~16KB(与 session_activate/new_task 一致)。 */
  vTaskDeleteWithCaps(NULL);
}

esp_err_t bb_adapter_request_turn_cancel(const char* played_text) {
  if (s_turn_cancel_inflight) {
    return ESP_OK; /* one in-flight cancel is enough — adapter keys by device */
  }
  s_turn_cancel_inflight = 1;
  char* copy = NULL;
  if (played_text != NULL && played_text[0] != '\0') {
    copy = strdup(played_text);
  }
  /* 栈走 PSRAM:真机 internal RAM 高度碎片化(largest 常 <8KB),internal 栈
   * xTaskCreate 会静默失败 → cancel 根本发不出去(2026-06-13 真机日志实锤)。
   * 网络 IO 在 PSRAM 栈上没问题(stream_task 40KB PSRAM 栈同样跑 HTTP/WS)。 */
  if (xTaskCreateWithCaps(turn_cancel_task, "bb_turn_cancel", 16384, copy, 5, NULL,
                          BBCLAW_MALLOC_CAP_PREFER_PSRAM) != pdPASS) {
    ESP_LOGE(TAG, "turn_cancel: task spawn failed (no mem) — cancel NOT sent");
    free(copy);
    s_turn_cancel_inflight = 0;
    return ESP_ERR_NO_MEM;
  }
  return ESP_OK;
}

/* ── ADR-042 §3.2: proactive notification TTS ─────────────────────────────────
 * A reminder/notification arrives over the control WS carrying ttsText (see the
 * session.notification branch). Speaking it means: POST /v1/tts/synthesize to the
 * cloud (active_base_url() is the cloud in cloud_saas; bb_adapter_tts_synthesize_
 * pcm16 already targets it) and play the PCM16 — both BLOCKING for seconds, so
 * they run on a dedicated worker, NOT the WS task. The stack lives in PSRAM
 * because internal RAM is too fragmented for xTaskCreate on real hardware (same
 * lesson as turn_cancel / the ws task). One speak at a time; overlaps are dropped.
 */
static volatile int s_notif_tts_inflight = 0;

static void notif_tts_task(void* arg) {
  char* text = (char*)arg;
  /* Don't talk over a live voice turn's playback — drop this notification's
   * speech if audio is already going (the toast still showed it). */
  if (text != NULL && text[0] != '\0' && !bb_audio_is_playback_active()) {
    bb_tts_audio_t audio;
    esp_err_t err = bb_adapter_tts_synthesize_pcm16(text, &audio, 0);
    if (err == ESP_OK && audio.pcm_data != NULL && audio.pcm_len > 0) {
      ESP_LOGI(TAG, "notif-tts: play %u pcm bytes", (unsigned)audio.pcm_len);
      /* 必须 start_playback 才会开 I2S TX + 功放门控(PA)：直接 play_pcm_blocking 会把
       * PCM 写进关着的 TX/功放 → 无声（实战派等 PA 门控板尤其如此，见 bb_audio.c 播放门控）。
       * 与语音回合/配对码 TTS 的播放包法一致：start→(设采样率)→play→stop→复位采样率。 */
      if (bb_audio_start_playback() == ESP_OK) {
        if (audio.sample_rate > 0 && audio.sample_rate != BBCLAW_AUDIO_SAMPLE_RATE) {
          (void)bb_audio_set_playback_sample_rate(audio.sample_rate);
        }
        bb_audio_play_pcm_blocking(audio.pcm_data, audio.pcm_len);
        (void)bb_audio_stop_playback();
        (void)bb_audio_set_playback_sample_rate(BBCLAW_AUDIO_SAMPLE_RATE);
      } else {
        ESP_LOGW(TAG, "notif-tts: start_playback failed");
      }
    } else {
      ESP_LOGW(TAG, "notif-tts: synth empty/failed err=%d", (int)err);
    }
    bb_adapter_tts_audio_free(&audio);
  }
  free(text);
  s_notif_tts_inflight = 0;
  /* 与 xTaskCreateWithCaps 配对,否则每条朗读通知泄漏 ~16KB PSRAM 栈+TCB。 */
  vTaskDeleteWithCaps(NULL);
}

void bb_adapter_speak_notification(const char* tts_text) {
  if (tts_text == NULL || tts_text[0] == '\0') {
    return;
  }
  /* On-demand synth is only available in cloud_saas (the cloud serves
   * /v1/tts/synthesize; local_home_v2 has no synth RPC — see synthesize guard). */
  if (!bb_transport_is_cloud_saas()) {
    return;
  }
  if (s_notif_tts_inflight) {
    return; /* one spoken notification at a time; drop overlaps */
  }
  s_notif_tts_inflight = 1;
  char* copy = strdup(tts_text);
  if (copy == NULL) {
    s_notif_tts_inflight = 0;
    return;
  }
  if (xTaskCreateWithCaps(notif_tts_task, "bb_notif_tts", 16384, copy, 5, NULL,
                          BBCLAW_MALLOC_CAP_PREFER_PSRAM) != pdPASS) {
    ESP_LOGE(TAG, "notif-tts: task spawn failed (no mem)");
    free(copy);
    s_notif_tts_inflight = 0;
  }
}

/* ── PTT edge observability: report EVERY physical PTT button edge ─────────
 * Otherwise the adapter only learns about PTT indirectly (voice.transcript
 * after VAD/ASR succeeds, or turn.cancel on barge-in) — an empty tap, a press
 * over a railed mic, or any release is invisible server-side. This reports
 * every raw down/up edge plus the firmware's classified action so the adapter
 * log shows each button operation and its recognized event type.
 *
 * A single persistent worker drains a queue: rapid down/up pairs are never
 * dropped (vs. the turn_cancel single-inflight guard) and we never create a
 * task per edge (internal-RAM fragmentation would make that intermittently
 * fail — see [[project_firmware_internal_ram_ws_task]]). Enqueue is
 * non-blocking, safe from the esp_timer / console-inject context. */

typedef struct {
  int pressed;       /* 1 = down, 0 = up */
  char action[16];   /* listen | barge_in | settings_exit | release */
  uint32_t seq;
} bb_ptt_evt_t;

static QueueHandle_t s_ptt_evt_queue;
static uint32_t s_ptt_evt_seq;
static uint32_t s_ptt_evt_dropped;

static void ptt_report_task(void* arg) {
  (void)arg;
  bb_ptt_evt_t evt;
  for (;;) {
    if (xQueueReceive(s_ptt_evt_queue, &evt, portMAX_DELAY) != pdTRUE) {
      continue;
    }
    const char* edge = evt.pressed ? "down" : "up";
    char body[256];
    esp_err_t err;
    if (bb_transport_is_cloud_saas()) {
      /* Observe-only: NEVER drive the WS connection from the telemetry path —
       * that is the voice/main path's job. If the WS isn't already up, drop this
       * event rather than take s_ws.lock / trigger a (up-to-5s) connect that
       * could contend with an in-flight voice stream. A healthy cloud_saas WS is
       * persistently connected, so this sends in the common case. */
      if (s_ws.client == NULL || !esp_websocket_client_is_connected(s_ws.client)) {
        err = ESP_ERR_INVALID_STATE;
      } else {
        snprintf(body, sizeof(body),
                 "{\"type\":\"request\",\"kind\":\"ptt.event\",\"messageId\":\"ptt-%u\","
                 "\"deviceId\":\"%s\",\"payload\":{\"edge\":\"%s\",\"action\":\"%s\",\"seq\":%u}}",
                 (unsigned)evt.seq, BBCLAW_DEVICE_ID, edge, evt.action, (unsigned)evt.seq);
        err = ws_send_text_message(body);
      }
    } else {
      snprintf(body, sizeof(body), "{\"deviceId\":\"%s\",\"edge\":\"%s\",\"action\":\"%s\",\"seq\":%u}",
               BBCLAW_DEVICE_ID, edge, evt.action, (unsigned)evt.seq);
      bb_http_resp_t resp;
      err = http_post_json("/v1/agent/ptt", body, &resp);
      if (err == ESP_OK && resp.status_code >= 400) {
        err = ESP_FAIL;
      }
    }
    ESP_LOGI(TAG, "phase=ptt_event_sent edge=%s action=%s seq=%u err=%s", edge, evt.action, (unsigned)evt.seq,
             esp_err_to_name(err));
  }
}

esp_err_t bb_adapter_ptt_report_init(void) {
  if (s_ptt_evt_queue != NULL) {
    return ESP_OK;
  }
  s_ptt_evt_queue = xQueueCreate(8, sizeof(bb_ptt_evt_t));
  if (s_ptt_evt_queue == NULL) {
    ESP_LOGE(TAG, "ptt report: queue create failed");
    return ESP_ERR_NO_MEM;
  }
  if (xTaskCreateWithCaps(ptt_report_task, "bb_ptt_report", 8192, NULL, 5, NULL, BBCLAW_MALLOC_CAP_PREFER_PSRAM) !=
      pdPASS) {
    ESP_LOGE(TAG, "ptt report: task spawn failed (no mem)");
    vQueueDelete(s_ptt_evt_queue);
    s_ptt_evt_queue = NULL;
    return ESP_ERR_NO_MEM;
  }
  return ESP_OK;
}

void bb_adapter_report_ptt_event(int pressed, const char* action) {
  if (s_ptt_evt_queue == NULL) {
    return; /* init not run / failed — observability is best-effort, never blocks PTT */
  }
  bb_ptt_evt_t evt = {.pressed = pressed ? 1 : 0, .seq = ++s_ptt_evt_seq};
  snprintf(evt.action, sizeof(evt.action), "%s", action != NULL ? action : "");
  if (xQueueSend(s_ptt_evt_queue, &evt, 0) != pdTRUE) {
    /* Queue full (worker stuck on a slow WS/HTTP send). Drop rather than block
     * the PTT edge handler; count so the gap is visible in the log. */
    if ((++s_ptt_evt_dropped % 8U) == 1U) {
      ESP_LOGW(TAG, "ptt report: queue full, dropped=%u", (unsigned)s_ptt_evt_dropped);
    }
  }
}

/* ── adapter_v2 P2: device-driven session switch / create (cloud_saas only) ──
 * Voice always runs the adapter's DEFAULT session, so when the device selects
 * or creates a conversation it must tell the adapter to respawn to that session
 * via a WS request (the adapter handles agent.sessions.activate / .create and
 * the cloud relays them pass-through). Same fire-and-forget pattern as
 * turn.cancel: build the JSON, ws_send on a one-shot PSRAM-stack task, one
 * in-flight at a time per kind. */

static volatile int s_session_activate_inflight;
static volatile int s_session_new_inflight;

static void session_activate_task(void* arg) {
  char* session_id = (char*)arg; /* heap copy, non-NULL */
  char* escaped = json_escape_alloc(session_id != NULL ? session_id : "");
  char body[256];
  snprintf(body, sizeof(body),
           "{\"type\":\"request\",\"kind\":\"agent.sessions.activate\",\"messageId\":\"sact-%lld\","
           "\"deviceId\":\"%s\",\"payload\":{\"sessionId\":\"%s\"}}",
           (long long)bb_now_ms(), BBCLAW_DEVICE_ID, escaped != NULL ? escaped : "");
  esp_err_t err = ws_client_ensure_connected();
  if (err == ESP_OK) {
    err = ws_send_text_message(body);
  }
  ESP_LOGI(TAG, "phase=session_activate_sent err=%s sid='%s'", esp_err_to_name(err),
           session_id != NULL ? session_id : "");
  free(escaped);
  free(session_id);
  s_session_activate_inflight = 0;
  vTaskDeleteWithCaps(NULL);
}

esp_err_t bb_adapter_request_session_activate(const char* session_id) {
  if (!bb_transport_is_cloud_saas()) {
    return ESP_OK; /* HTTP path switches sessions per-turn, not via WS */
  }
  if (session_id == NULL || session_id[0] == '\0') {
    return ESP_ERR_INVALID_ARG;
  }
  if (s_session_activate_inflight) {
    return ESP_OK; /* one in-flight activate is enough */
  }
  s_session_activate_inflight = 1;
  char* copy = strdup(session_id);
  if (copy == NULL) {
    s_session_activate_inflight = 0;
    return ESP_ERR_NO_MEM;
  }
  /* PSRAM stack — internal DRAM is fragmented in Settings/chat; network IO
   * runs fine on a PSRAM stack (same rationale as turn_cancel). */
  if (xTaskCreateWithCaps(session_activate_task, "bb_sess_act", 8192, copy, 5, NULL,
                          BBCLAW_MALLOC_CAP_PREFER_PSRAM) != pdPASS) {
    ESP_LOGE(TAG, "session_activate: task spawn failed (no mem) — not sent");
    free(copy);
    s_session_activate_inflight = 0;
    return ESP_ERR_NO_MEM;
  }
  return ESP_OK;
}

static void session_new_task(void* arg) {
  (void)arg;
  char body[192];
  snprintf(body, sizeof(body),
           "{\"type\":\"request\",\"kind\":\"agent.sessions.create\",\"messageId\":\"snew-%lld\","
           "\"deviceId\":\"%s\",\"payload\":{}}",
           (long long)bb_now_ms(), BBCLAW_DEVICE_ID);
  esp_err_t err = ws_client_ensure_connected();
  if (err == ESP_OK) {
    err = ws_send_text_message(body);
  }
  ESP_LOGI(TAG, "phase=session_new_sent err=%s", esp_err_to_name(err));
  s_session_new_inflight = 0;
  vTaskDeleteWithCaps(NULL);
}

esp_err_t bb_adapter_request_session_new(void) {
  if (!bb_transport_is_cloud_saas()) {
    return ESP_OK;
  }
  if (s_session_new_inflight) {
    return ESP_OK;
  }
  s_session_new_inflight = 1;
  if (xTaskCreateWithCaps(session_new_task, "bb_sess_new", 8192, NULL, 5, NULL,
                          BBCLAW_MALLOC_CAP_PREFER_PSRAM) != pdPASS) {
    ESP_LOGE(TAG, "session_new: task spawn failed (no mem) — not sent");
    s_session_new_inflight = 0;
    return ESP_ERR_NO_MEM;
  }
  return ESP_OK;
}

static esp_err_t ws_send_binary_message(const uint8_t* data, size_t len) {
  if (data == NULL || len == 0U) {
    return ESP_ERR_INVALID_ARG;
  }
  ESP_RETURN_ON_ERROR(ws_client_ensure_connected(), TAG, "ws connect failed");
  int sent = esp_websocket_client_send_bin(s_ws.client, (const char*)data, (int)len, pdMS_TO_TICKS(5000));
  return sent >= 0 ? ESP_OK : ESP_FAIL;
}

static void append_tts_chunk_from_ogg_locked(void) {
  if (s_ws.finish_result == NULL || s_ws.tts_audio_len == 0U) {
    return;
  }
  log_mem_snapshot("tts decode begin");
  ESP_LOGI(TAG, "tts.stop decode ogg_bytes=%u cap=%u", (unsigned)s_ws.tts_audio_len, (unsigned)s_ws.tts_audio_cap);
  bb_ogg_opus_decoder_t* decoder = bb_ogg_opus_decoder_create();
  if (decoder == NULL) {
    return;
  }
  uint8_t* pcm = NULL;
  size_t pcm_len = 0;
  int source_sample_rate = 0;
  int source_channels = 0;
  esp_err_t err = bb_ogg_opus_decoder_decode_all(decoder, s_ws.tts_audio_buf, s_ws.tts_audio_len, BBCLAW_TTS_SAMPLE_RATE,
                                                 BBCLAW_TTS_CHANNELS, &pcm, &pcm_len, &source_sample_rate,
                                                 &source_channels);
  bb_ogg_opus_decoder_destroy(decoder);
  if (err != ESP_OK || pcm == NULL || pcm_len == 0U) {
    ESP_LOGW(TAG, "tts.stop decode failed err=%s ogg_bytes=%u", esp_err_to_name(err), (unsigned)s_ws.tts_audio_len);
    bb_ogg_opus_free(pcm);
    return;
  }
  bb_tts_chunk_t* chunk = (bb_tts_chunk_t*)tts_calloc(sizeof(bb_tts_chunk_t));
  if (chunk == NULL) {
    bb_ogg_opus_free(pcm);
    return;
  }
  chunk->pcm_data = pcm;
  chunk->pcm_len = pcm_len;
  chunk->sample_rate = source_sample_rate > 0 ? source_sample_rate : BBCLAW_TTS_SAMPLE_RATE;
  chunk->channels = source_channels > 0 ? source_channels : BBCLAW_TTS_CHANNELS;
  s_ws.tts_audio_len = 0;
  ESP_LOGI(TAG, "tts.stop decoded pcm_bytes=%u sample_rate=%d channels=%d", (unsigned)pcm_len, chunk->sample_rate,
           chunk->channels);
  dispatch_tts_chunk_event(s_ws.finish_result, s_ws.finish_on_event, s_ws.finish_user_ctx, chunk);
  log_mem_snapshot("tts decode done");
}

static void ws_handle_text_message(const char* msg) {
  if (msg == NULL) {
    return;
  }
  char type[24] = {0};
  char kind[48] = {0};
  char phase[32] = {0};
  char stream_id[64] = {0};

  (void)json_extract_string(msg, "type", type, sizeof(type));
  if (strcmp(type, "welcome") == 0) {
    // Extract and apply config from welcome message
    const char* config_start = strstr(msg, "\"config\"");
    if (config_start != NULL) {
      const char* brace = strchr(config_start, '{');
      if (brace != NULL) {
        int depth = 0;
        const char* end = brace;
        for (; *end != '\0'; end++) {
          if (*end == '{') depth++;
          else if (*end == '}') {
            depth--;
            if (depth == 0) break;
          }
        }
        if (depth == 0 && end > brace) {
          size_t len = (size_t)(end - brace + 1);
          char* config_json = (char*)malloc(len + 1);
          if (config_json != NULL) {
            memcpy(config_json, brace, len);
            config_json[len] = '\0';
            bb_device_config_apply_welcome(config_json);
            free(config_json);
          }
        }
      }
    }
    return;
  }
  if (strcmp(type, "config.update") == 0) {
    // Extract and apply config update
    const char* config_start = strstr(msg, "\"config\"");
    if (config_start != NULL) {
      const char* brace = strchr(config_start, '{');
      if (brace != NULL) {
        int depth = 0;
        const char* end = brace;
        for (; *end != '\0'; end++) {
          if (*end == '{') depth++;
          else if (*end == '}') {
            depth--;
            if (depth == 0) break;
          }
        }
        if (depth == 0 && end > brace) {
          size_t len = (size_t)(end - brace + 1);
          char* config_json = (char*)malloc(len + 1);
          if (config_json != NULL) {
            memcpy(config_json, brace, len);
            config_json[len] = '\0';
            // Extract version from config
            int version = 0;
            const char* ver_str = strstr(config_json, "\"version\"");
            if (ver_str != NULL) {
              const char* colon = strchr(ver_str, ':');
              if (colon != NULL) {
                version = atoi(colon + 1);
              }
            }
            bb_device_config_apply_update(version, config_json);
            free(config_json);
          }
        }
      }
    }
    return;
  }
  if (strcmp(type, "ack") == 0 || strcmp(type, "pong") == 0) {
    return;
  }
  if (strcmp(type, "error") == 0) {
    /* ADR-027: sites.* failures arrive as type:"error" with a stable top-level
     * `error` code (mirrored in payload.error.code). Route to the in-flight
     * sites request before the generic finish/verify error handling. */
    char ekind[48] = {0};
    (void)json_extract_string(msg, "kind", ekind, sizeof(ekind));
    if (strncmp(ekind, "sites.", 6) == 0) {
      xSemaphoreTake(s_ws.lock, portMAX_DELAY);
      if (s_ws.sites_req != NULL) {
        s_ws.sites_req->ok = 0;
        if (!json_extract_string(msg, "error", s_ws.sites_req->err_code,
                                 sizeof(s_ws.sites_req->err_code)) ||
            s_ws.sites_req->err_code[0] == '\0') {
          (void)json_extract_string(msg, "code", s_ws.sites_req->err_code,
                                    sizeof(s_ws.sites_req->err_code));
        }
        s_ws.sites_waiting = 0;
        xSemaphoreGive(s_ws.lock);
        xEventGroupSetBits(s_ws.events, BB_WS_EVENT_SITES_DONE);
        return;
      }
      xSemaphoreGive(s_ws.lock);
    }
    /* ADR-044 P1b: ambient 补传错误只交给 ambient 等待槽,绝不落到下面的
     * finish/verify 通用错误路径(补传是后台任务,不许惊动语音回合状态)。 */
    if (strncmp(ekind, "ambient.", 8) == 0) {
      xSemaphoreTake(s_ws.lock, portMAX_DELAY);
      if (s_ws.ambient_waiting) {
        s_ws.ambient_ok = 0;
        if (!json_extract_string(msg, "error", s_ws.ambient_err, sizeof(s_ws.ambient_err)) ||
            s_ws.ambient_err[0] == '\0') {
          snprintf(s_ws.ambient_err, sizeof(s_ws.ambient_err), "AMBIENT_ERROR");
        }
        s_ws.ambient_waiting = 0;
        xSemaphoreGive(s_ws.lock);
        xEventGroupSetBits(s_ws.events, BB_WS_EVENT_AMBIENT_DONE);
        return;
      }
      xSemaphoreGive(s_ws.lock);
      return;
    }
    xSemaphoreTake(s_ws.lock, portMAX_DELAY);
    if (s_ws.finish_result != NULL) {
      (void)json_extract_string(msg, "error", s_ws.finish_result->error_code, sizeof(s_ws.finish_result->error_code));
      s_ws.finish_saw_error = 1;
      emit_finish_stream_event(s_ws.finish_on_event, s_ws.finish_user_ctx, BB_FINISH_STREAM_EVENT_ERROR, NULL,
                               s_ws.finish_result->error_code, NULL, 0);
    }
    if (s_ws.verify_result != NULL) {
      s_ws.verify_result->match = 0;
      s_ws.verify_result->confidence = 0.0f;
      if (!json_extract_string(msg, "error", s_ws.verify_result->message, sizeof(s_ws.verify_result->message))) {
        snprintf(s_ws.verify_result->message, sizeof(s_ws.verify_result->message), "voice.verify failed");
      }
      s_ws.verify_waiting = 0;
    }
    xSemaphoreGive(s_ws.lock);
    xEventGroupSetBits(s_ws.events, BB_WS_EVENT_ERROR);
    return;
  }
  /* ADR-027: cloud-terminated sites.list / sites.activate success replies. */
  if (strcmp(type, "reply") == 0) {
    char rkind[48] = {0};
    (void)json_extract_string(msg, "kind", rkind, sizeof(rkind));
    /* ADR-044 P1b: 段持久化确认。匹配飞行中的 (session, seq);容忍 sessionId
     * 未回显(seq 已足够界定单飞行段)。 */
    if (strcmp(rkind, "ambient.segment.ack") == 0) {
      int ack_seq = json_extract_int(msg, "segSeq", -1);
      char ack_sess[48] = {0};
      (void)json_extract_string(msg, "sessionId", ack_sess, sizeof(ack_sess));
      xSemaphoreTake(s_ws.lock, portMAX_DELAY);
      if (s_ws.ambient_waiting && ack_seq == s_ws.ambient_seq &&
          (ack_sess[0] == '\0' || strcmp(ack_sess, s_ws.ambient_session) == 0)) {
        s_ws.ambient_ok = 1;
        s_ws.ambient_waiting = 0;
        xSemaphoreGive(s_ws.lock);
        xEventGroupSetBits(s_ws.events, BB_WS_EVENT_AMBIENT_DONE);
        return;
      }
      xSemaphoreGive(s_ws.lock);
      return;
    }
    /* ADR-049: 设备主动发 image.capture(拍照)后,adapter 让 claude 读图并回一条
     * voice.reply;云端因该回复没有在途 voice 回合(finish 流)可匹配,便把它反向路由
     * 到设备这里作为文本 reply。此处把它朗读出来(走 cloud /v1/tts/synthesize,与提醒
     * 播报同一条链路)。正常语音回合里 finish_result 已武装、音频经流式 tts.chunk 播放,
     * 故仅在 finish_result==NULL(无在途回合)时朗读,避免把语音回合的回复重复念一遍。 */
    if (strcmp(rkind, "voice.reply") == 0) {
      int have_turn;
      xSemaphoreTake(s_ws.lock, portMAX_DELAY);
      have_turn = (s_ws.finish_result != NULL);
      xSemaphoreGive(s_ws.lock);
      if (!have_turn) {
        char reply_text[512] = {0};
        const char* payload = strstr(msg, "\"payload\"");
        const char* src = (payload != NULL) ? strchr(payload, '{') : NULL;
        if (src == NULL) {
          src = msg;
        }
        if (json_extract_string(src, "text", reply_text, sizeof(reply_text)) && reply_text[0] != '\0') {
          ESP_LOGI(TAG, "image.capture reply → show+speak (%d chars)", (int)strlen(reply_text));
          /* 显示到聊天界面：与 turn.committed 的问题气泡成对，让用户既能听也能看到
           * claude 的回答文字（正常语音回合走 finish-stream 显示，这里补的是图片回合）。 */
          bb_ui_agent_chat_post_reply_delta(reply_text);
          bb_ui_agent_chat_post_reply_done();
          bb_adapter_speak_notification(reply_text);
        }
      }
      return;
    }
    if (strncmp(rkind, "sites.", 6) != 0) {
      return;
    }
    char mid[64] = {0};
    (void)json_extract_string(msg, "messageId", mid, sizeof(mid));
    xSemaphoreTake(s_ws.lock, portMAX_DELAY);
    /* Match on messageId; tolerate a missing echo (kind already scoped to the
     * in-flight sites request) so a non-echoing cloud build still resolves. */
    if (s_ws.sites_req != NULL &&
        (mid[0] == '\0' || s_ws.sites_message_id[0] == '\0' ||
         strcmp(mid, s_ws.sites_message_id) == 0)) {
      if (strcmp(rkind, "sites.list") == 0) {
        s_ws.sites_req->count = parse_sites_array(msg, s_ws.sites_req->sites, s_ws.sites_req->max);
        s_ws.sites_req->ok = 1;
      } else { /* sites.activate */
        char res[16] = {0};
        (void)json_extract_string(msg, "result", res, sizeof(res));
        (void)json_extract_string(msg, "activeHomeSiteId", s_ws.sites_req->active_id,
                                  sizeof(s_ws.sites_req->active_id));
        s_ws.sites_req->ok = (strcmp(res, "ok") == 0) ? 1 : 0;
        if (!s_ws.sites_req->ok) {
          (void)json_extract_string(msg, "error", s_ws.sites_req->err_code,
                                    sizeof(s_ws.sites_req->err_code));
        }
      }
      s_ws.sites_waiting = 0;
      xSemaphoreGive(s_ws.lock);
      xEventGroupSetBits(s_ws.events, BB_WS_EVENT_SITES_DONE);
      return;
    }
    xSemaphoreGive(s_ws.lock);
    return;
  }
  if (strcmp(type, "event") != 0) {
    return;
  }

  (void)json_extract_string(msg, "kind", kind, sizeof(kind));
  (void)json_extract_string(msg, "streamId", stream_id, sizeof(stream_id));

  /* Phase S3: session.notification — no finish context needed, handle before
   * taking the semaphore. The payload is nested: extract from the "payload"
   * sub-object. */
  if (strcmp(kind, "session.notification") == 0) {
    const char* payload_start = strstr(msg, "\"payload\"");
    if (payload_start != NULL) {
      const char* brace = strchr(payload_start, '{');
      if (brace != NULL) {
        char sid[64] = {0};
        char drv[24] = {0};
        char ntype[16] = {0};
        char preview[48] = {0};
        char tts_text[256] = {0};
        json_extract_string(brace, "sessionId", sid, sizeof(sid));
        json_extract_string(brace, "driver", drv, sizeof(drv));
        json_extract_string(brace, "type", ntype, sizeof(ntype));
        json_extract_string(brace, "preview", preview, sizeof(preview));
        json_extract_string(brace, "ttsText", tts_text, sizeof(tts_text));
        /* Record into the notification store for the 提醒 page + unread badge
         * (ADR-021 §9). Reminders are SESSION-LESS (empty sessionId), so do NOT
         * gate on sid — that dropped every reminder from the store (badge stayed
         * 0, 已提醒 empty). Record whenever there's something to show (sid or a
         * preview); on_ws_event tolerates an empty sid. */
        if (sid[0] != '\0' || preview[0] != '\0') {
          bb_notification_on_ws_event(sid, drv, ntype, preview);
        }
        /* ADR-046 §5:消息/提醒到达唤醒息屏(此前该通路无人接线=死代码,息屏时
         * 来通知不亮屏)。只置旗标,tick 在 stream_task 消费,WS 任务上下文安全。 */
        bb_power_mgmt_on_message_arrived();
        /* ADR-042 §3.2: a notification carrying ttsText (e.g. a reminder) is
         * spoken aloud, not just toasted. Gated on ttsText presence (the adapter
         * sets it with speak=true); fires regardless of sid so a session-less
         * reminder still speaks. */
        if (tts_text[0] != '\0') {
          bb_adapter_speak_notification(tts_text);
        }
      }
    }
    return;
  }

  /* ADR-040 — authoritative turn lifecycle from the adapter (relayed by cloud).
   * Reconcile the device's optimistic, PTT-driven chat UI against ground truth.
   * Handled BEFORE the finish/stream_id guards below so a prior turn's reconcile
   * (which can arrive after the device already moved to a newer stream) is not
   * dropped. No finish context needed; the chat-UI funcs post via lv_async.
   * seq/text/reason are read top-level (json_extract_* searches the whole frame,
   * so the cloud's nested "payload" is fine — same as voice.reply.delta). */
  if (strcmp(kind, "turn.committed") == 0) {
    char committed_text[256] = {0};
    int seq = json_extract_int(msg, "seq", 0);
    (void)json_extract_string(msg, "text", committed_text, sizeof(committed_text));
    bb_ui_agent_chat_note_committed(seq, committed_text);
    return;
  }
  if (strcmp(kind, "turn.superseded") == 0) {
    char reason[40] = {0};
    int seq = json_extract_int(msg, "seq", 0);
    (void)json_extract_string(msg, "reason", reason, sizeof(reason));
    bb_ui_agent_chat_note_superseded(seq, reason);
    return;
  }

  xSemaphoreTake(s_ws.lock, portMAX_DELAY);
  if (strcmp(kind, "voice.verify.result") == 0 && s_ws.verify_result != NULL) {
    parse_voice_verify_result(msg, s_ws.verify_result);
    s_ws.verify_waiting = 0;
    xSemaphoreGive(s_ws.lock);
    xEventGroupSetBits(s_ws.events, BB_WS_EVENT_VERIFY_DONE);
    return;
  }
  if (s_ws.finish_result == NULL) {
    xSemaphoreGive(s_ws.lock);
    return;
  }
  if (stream_id[0] != '\0' && s_ws.finish_stream_id[0] != '\0' && strcmp(stream_id, s_ws.finish_stream_id) != 0) {
    xSemaphoreGive(s_ws.lock);
    return;
  }

  /* 唯一咽喉点：所有回合内事件（status/delta/thinking/tool_call/prompt/tts/done）
   * 都从这里过——打「最后活动」时间戳,喂空闲超时。 */
  s_ws.finish_last_activity_ms = bb_now_ms();

  if (strcmp(kind, "voice.session") == 0) {
    /* Issue #146: cloud voice (butler) path now forwards the resolved session
     * id + driver so the device can persist it (NVS + chat cache bind) and
     * replay history on CHAT re-entry. Payload is nested under "payload". */
    char sid[80] = {0};
    char drv[24] = {0};
    const char* payload_start = strstr(msg, "\"payload\"");
    const char* src = (payload_start != NULL) ? strchr(payload_start, '{') : NULL;
    if (src == NULL) src = msg;
    (void)json_extract_string(src, "sessionId", sid, sizeof(sid));
    (void)json_extract_string(src, "driver", drv, sizeof(drv));
    if (s_ws.finish_on_event != NULL && sid[0] != '\0') {
      emit_finish_stream_event(s_ws.finish_on_event, s_ws.finish_user_ctx, BB_FINISH_STREAM_EVENT_SESSION, drv, sid,
                               NULL, 0);
    }
  } else if (strcmp(kind, "voice.reply.status") == 0) {
    (void)json_extract_string(msg, "phase", phase, sizeof(phase));
    if (s_ws.finish_on_event != NULL) {
      emit_finish_stream_event(s_ws.finish_on_event, s_ws.finish_user_ctx, BB_FINISH_STREAM_EVENT_STATUS, phase, NULL,
                               NULL, 0);
    }
  } else if (strcmp(kind, "asr.final") == 0) {
    (void)json_extract_string(msg, "text", s_ws.finish_result->transcript, sizeof(s_ws.finish_result->transcript));
    if (s_ws.finish_on_event != NULL) {
      emit_finish_stream_event(s_ws.finish_on_event, s_ws.finish_user_ctx, BB_FINISH_STREAM_EVENT_ASR_FINAL, NULL,
                               s_ws.finish_result->transcript, NULL, 0);
    }
  } else if (strcmp(kind, "voice.reply.delta") == 0) {
    (void)json_extract_string(msg, "text", s_ws.finish_result->reply_text, sizeof(s_ws.finish_result->reply_text));
    if (s_ws.finish_on_event != NULL) {
      emit_finish_stream_event(s_ws.finish_on_event, s_ws.finish_user_ctx, BB_FINISH_STREAM_EVENT_REPLY_DELTA, NULL,
                               s_ws.finish_result->reply_text, NULL, 0);
    }
  } else if (strcmp(kind, "thinking") == 0) {
    char text[128] = {0};
    (void)json_extract_string(msg, "text", text, sizeof(text));
    if (s_ws.finish_on_event != NULL && text[0] != '\0') {
      emit_finish_stream_event(s_ws.finish_on_event, s_ws.finish_user_ctx, BB_FINISH_STREAM_EVENT_THINKING, NULL,
                               text, NULL, 0);
    }
  } else if (strcmp(kind, "tool_call") == 0) {
    char name[64] = {0};
    char hint[256] = {0}; /* full-ish tool command (adapter caps hint at 240) */
    (void)json_extract_string(msg, "name", name, sizeof(name));
    (void)json_extract_string(msg, "hint", hint, sizeof(hint)); /* ADR-030 */
    if (s_ws.finish_on_event != NULL && name[0] != '\0') {
      emit_tool_call_event(s_ws.finish_on_event, s_ws.finish_user_ctx, name, hint);
    }
  } else if (strcmp(kind, "voice.prompt.open") == 0) {
    /* ADR-033: blocking permission/confirm menu forwarded for on-device approval. */
    bb_prompt_t p;
    if (bb_prompt_parse_open(msg, &p) && s_ws.finish_on_event != NULL) {
      emit_prompt_event(s_ws.finish_on_event, s_ws.finish_user_ctx, BB_FINISH_STREAM_EVENT_PROMPT_OPEN, &p);
    }
  } else if (strcmp(kind, "voice.prompt.close") == 0) {
    bb_prompt_t p;
    memset(&p, 0, sizeof(p));
    (void)json_extract_string(msg, "promptId", p.prompt_id, sizeof(p.prompt_id));
    (void)json_extract_string(msg, "reason", p.reason, sizeof(p.reason));
    if (s_ws.finish_on_event != NULL) {
      emit_prompt_event(s_ws.finish_on_event, s_ws.finish_user_ctx, BB_FINISH_STREAM_EVENT_PROMPT_CLOSE, &p);
    }
  } else if (strcmp(kind, "dispatch_status") == 0) {
    /* ADR-021-firmware-ui §1.2: butler dispatch progress via WS relay */
    if (s_ws.finish_on_event != NULL) {
      bb_dispatch_status_t ds = {0};
      const char* dp = strstr(msg, "\"dispatch\"");
      const char* src = (dp != NULL) ? strchr(dp, '{') : msg;
      if (src == NULL) src = msg;
      (void)json_extract_string(src, "phase",   ds.phase,   sizeof(ds.phase));
      (void)json_extract_string(src, "taskId",  ds.task_id, sizeof(ds.task_id));
      (void)json_extract_string(src, "cwd",     ds.cwd,     sizeof(ds.cwd));
      ds.elapsed_ms = (int64_t)json_extract_int(src, "elapsedMs", 0);
      if (ds.phase[0] != '\0') {
        emit_dispatch_event(s_ws.finish_on_event, s_ws.finish_user_ctx, &ds);
      }
    }
  } else if (strcmp(kind, "tts.chunk") == 0) {
    /* Agent-bus path / abort path don't want TTS — skip base64 decode + alloc
     * entirely. dispatch_tts_chunk_event would also drop the chunk, but we
     * may as well not spend CPU on the JSON parse. */
    if (s_ws.finish_on_event != NULL) {
      bb_tts_chunk_t* chunk = decode_tts_chunk_json(msg);
      if (chunk != NULL) {
        dispatch_tts_chunk_event(s_ws.finish_result, s_ws.finish_on_event, s_ws.finish_user_ctx, chunk);
      }
    }
  } else if (strcmp(kind, "tts.start") == 0) {
    (void)json_extract_string(msg, "streamId", s_ws.tts_stream_id, sizeof(s_ws.tts_stream_id));
    s_ws.tts_audio_len = 0;
  } else if (strcmp(kind, "tts.stop") == 0) {
    append_tts_chunk_from_ogg_locked();
    if (s_ws.finish_on_event != NULL) {
      emit_finish_stream_event(s_ws.finish_on_event, s_ws.finish_user_ctx, BB_FINISH_STREAM_EVENT_TTS_DONE, NULL, NULL,
                               NULL, 0);
    }
  } else if (strcmp(kind, "tts.done") == 0) {
    if (s_ws.finish_on_event != NULL) {
      emit_finish_stream_event(s_ws.finish_on_event, s_ws.finish_user_ctx, BB_FINISH_STREAM_EVENT_TTS_DONE, NULL, NULL,
                               NULL, 0);
    }
  } else if (strcmp(kind, "voice.session.done") == 0) {
    parse_finish_result(msg, s_ws.finish_result);
    if (s_ws.finish_on_event != NULL) {
      emit_finish_stream_event(s_ws.finish_on_event, s_ws.finish_user_ctx, BB_FINISH_STREAM_EVENT_VOICE_DONE, NULL, NULL,
                               NULL, s_ws.finish_result->reply_wait_timed_out);
    }
    s_ws.finish_saw_done = 1;
    xEventGroupSetBits(s_ws.events, BB_WS_EVENT_DONE);
  }
  xSemaphoreGive(s_ws.lock);
}

static void ws_handle_binary_message(const uint8_t* data, size_t len) {
  if (data == NULL || len == 0U) {
    return;
  }
  xSemaphoreTake(s_ws.lock, portMAX_DELAY);
  if (s_ws.finish_result == NULL || s_ws.tts_stream_id[0] == '\0') {
    xSemaphoreGive(s_ws.lock);
    return;
  }
  /* No TTS consumer (agent-bus path / abort path) → drop OGG without
   * accumulating. Otherwise a multi-second cloud reply piles ~1 MB of
   * OGG into tts_audio_buf (sticky cap, never shrinks) and the ensuing
   * tts.stop decode further inflates PCM into finish->tts_chunks, both
   * of which sit in PSRAM until cloud_wait returns — squeezing internal
   * RAM via fragmentation pressure to the point xTaskCreate fails. */
  if (s_ws.finish_on_event == NULL) {
    xSemaphoreGive(s_ws.lock);
    return;
  }
  s_ws.finish_last_activity_ms = bb_now_ms(); /* TTS OGG 帧也是回合活动 */
  size_t need = s_ws.tts_audio_len + len;
  if (need > s_ws.tts_audio_cap) {
    size_t new_cap = s_ws.tts_audio_cap == 0U ? 4096U : s_ws.tts_audio_cap;
    while (new_cap < need) {
      new_cap *= 2U;
    }
    uint8_t* new_buf = (uint8_t*)heap_caps_realloc(s_ws.tts_audio_buf, new_cap, BBCLAW_MALLOC_CAP_PREFER_PSRAM);
    if (new_buf == NULL) {
      xSemaphoreGive(s_ws.lock);
      return;
    }
    s_ws.tts_audio_buf = new_buf;
    s_ws.tts_audio_cap = new_cap;
  }
  memcpy(s_ws.tts_audio_buf + s_ws.tts_audio_len, data, len);
  s_ws.tts_audio_len += len;
  xSemaphoreGive(s_ws.lock);
}

static void ws_event_handler(void* handler_args, esp_event_base_t base, int32_t event_id, void* event_data) {
  (void)handler_args;
  (void)base;
  if (!s_ws.initialized) {
    return;
  }

  if (event_id == WEBSOCKET_EVENT_CONNECTED) {
    s_ws.connected = 1;
    xEventGroupSetBits(s_ws.events, BB_WS_EVENT_CONNECTED);
    ESP_LOGI(TAG, "ws connected");
    log_mem_snapshot("ws connected");
    return;
  }
  if (event_id == WEBSOCKET_EVENT_DISCONNECTED || event_id == WEBSOCKET_EVENT_CLOSED) {
    s_ws.connected = 0;
    xEventGroupSetBits(s_ws.events, BB_WS_EVENT_DISCONNECTED);
    if (s_ws.finish_waiting) {
      xEventGroupSetBits(s_ws.events, BB_WS_EVENT_ERROR);
    }
    ESP_LOGW(TAG, "ws disconnected");
    return;
  }
  if (event_id != WEBSOCKET_EVENT_DATA) {
    return;
  }

  esp_websocket_event_data_t* evt = (esp_websocket_event_data_t*)event_data;
  if (evt == NULL || evt->data_ptr == NULL || evt->data_len <= 0) {
    return;
  }

  xSemaphoreTake(s_ws.lock, portMAX_DELAY);
  if (evt->payload_offset == 0) {
    s_ws.text_len = 0;
    s_ws.text_opcode = evt->op_code;
  }
  size_t need = s_ws.text_len + (size_t)evt->data_len + 1U;
  if (need > s_ws.text_cap) {
    size_t new_cap = s_ws.text_cap == 0U ? 4096U : s_ws.text_cap;
    while (new_cap < need) {
      new_cap *= 2U;
    }
    uint8_t* new_buf = (uint8_t*)heap_caps_realloc(s_ws.text_buf, new_cap, BBCLAW_MALLOC_CAP_PREFER_PSRAM);
    if (new_buf == NULL) {
      xSemaphoreGive(s_ws.lock);
      return;
    }
    s_ws.text_buf = new_buf;
    s_ws.text_cap = new_cap;
  }
  memcpy(s_ws.text_buf + s_ws.text_len, evt->data_ptr, (size_t)evt->data_len);
  s_ws.text_len += (size_t)evt->data_len;
  s_ws.text_buf[s_ws.text_len] = '\0';
  int complete = evt->fin && (evt->payload_offset + evt->data_len >= evt->payload_len);
  uint8_t opcode = s_ws.text_opcode;
  size_t msg_len = s_ws.text_len;
  xSemaphoreGive(s_ws.lock);

  if (!complete) {
    return;
  }
  if (opcode == 0x1U) {
    ws_handle_text_message((const char*)s_ws.text_buf);
  } else if (opcode == 0x2U) {
    ws_handle_binary_message(s_ws.text_buf, msg_len);
  }
}

static int body_contains_ok_true(const char* body) {
  if (body == NULL) {
    return 0;
  }
  if (strstr(body, "\"ok\":true") != NULL) {
    return 1;
  }
  return strstr(body, "\"ok\": true") != NULL;
}

/* Parse exactly 4 hex digits at s into *out. Returns 1 on success. */
static int json_parse_hex4(const char* s, uint32_t* out) {
  uint32_t cp = 0U;
  for (int i = 0; i < 4; i++) {
    char c = s[i];
    uint32_t n;
    if (c >= '0' && c <= '9') {
      n = (uint32_t)(c - '0');
    } else if (c >= 'a' && c <= 'f') {
      n = (uint32_t)(c - 'a' + 10);
    } else if (c >= 'A' && c <= 'F') {
      n = (uint32_t)(c - 'A' + 10);
    } else {
      return 0;
    }
    cp = (cp << 4) | n;
  }
  *out = cp;
  return 1;
}

/* Encode a Unicode code point as UTF-8 into buf (max avail bytes).
 * Returns bytes written, or 0 if it doesn't fit. */
static size_t json_utf8_encode(uint32_t cp, char* buf, size_t avail) {
  if (cp < 0x80U) {
    if (avail < 1U) return 0;
    buf[0] = (char)cp;
    return 1;
  }
  if (cp < 0x800U) {
    if (avail < 2U) return 0;
    buf[0] = (char)(0xC0U | (cp >> 6));
    buf[1] = (char)(0x80U | (cp & 0x3FU));
    return 2;
  }
  if (cp < 0x10000U) {
    if (avail < 3U) return 0;
    buf[0] = (char)(0xE0U | (cp >> 12));
    buf[1] = (char)(0x80U | ((cp >> 6) & 0x3FU));
    buf[2] = (char)(0x80U | (cp & 0x3FU));
    return 3;
  }
  if (avail < 4U) return 0;
  buf[0] = (char)(0xF0U | (cp >> 18));
  buf[1] = (char)(0x80U | ((cp >> 12) & 0x3FU));
  buf[2] = (char)(0x80U | ((cp >> 6) & 0x3FU));
  buf[3] = (char)(0x80U | (cp & 0x3FU));
  return 4;
}

static int json_extract_string(const char* body, const char* key, char* out, size_t out_len) {
  if (body == NULL || key == NULL || out == NULL || out_len == 0U) {
    return 0;
  }
  /* 兼容 "key":"val" 和 "key": "val" */
  char p1[64] = {0};
  char p2[64] = {0};
  snprintf(p1, sizeof(p1), "\"%s\":\"", key);
  snprintf(p2, sizeof(p2), "\"%s\": \"", key);
  const char* start = strstr(body, p1);
  if (start != NULL) {
    start += strlen(p1);
  } else {
    start = strstr(body, p2);
    if (start != NULL) {
      start += strlen(p2);
    }
  }
  if (start == NULL) {
    out[0] = '\0';
    return 0;
  }
  const char* p = start;
  size_t j = 0;
  while (*p != '\0' && j + 1 < out_len) {
    if (*p == '"') {
      break;
    }
    if (*p == '\\' && p[1] != '\0') {
      p++;
      switch (*p) {
        case '"':
          out[j++] = '"';
          break;
        case '\\':
          out[j++] = '\\';
          break;
        case '/':
          out[j++] = '/';
          break;
        case 'b':
          out[j++] = '\b';
          break;
        case 'f':
          out[j++] = '\f';
          break;
        case 'n':
          out[j++] = '\n';
          break;
        case 'r':
          out[j++] = '\r';
          break;
        case 't':
          out[j++] = '\t';
          break;
        case 'u': {
          /* \uXXXX — decode to a Unicode code point and emit UTF-8.
           * Go's encoding/json HTML-escapes '<' '>' '&' to the 6-char
           * sequences backslash-u-003c / 003e / 0026, so tool hints like
           * "2>&1" arrive escaped; without this they printed as literal
           * "u003e"/"u0026" on the serial console. */
          uint32_t cp;
          if (json_parse_hex4(p + 1, &cp)) {
            p += 4; /* consume the 4 hex digits (trailing p++ skips the last) */
            if (cp >= 0xD800U && cp <= 0xDBFFU && p[1] == '\\' && p[2] == 'u') {
              uint32_t lo;
              if (json_parse_hex4(p + 3, &lo) && lo >= 0xDC00U && lo <= 0xDFFFU) {
                cp = 0x10000U + ((cp - 0xD800U) << 10) + (lo - 0xDC00U);
                p += 6; /* consume the low surrogate "\uXXXX" */
              }
            }
            j += json_utf8_encode(cp, out + j, out_len - 1U - j);
          } else {
            out[j++] = 'u'; /* malformed \u — preserve prior behaviour */
          }
          break;
        }
        default:
          out[j++] = (unsigned char)*p;
          break;
      }
      p++;
    } else {
      out[j++] = (unsigned char)*p++;
    }
  }
  out[j] = '\0';
  return 1;
}

static int json_extract_bool(const char* body, const char* key, int fallback) {
  if (body == NULL || key == NULL) {
    return fallback;
  }
  char p1[64], p2[64];
  snprintf(p1, sizeof(p1), "\"%s\":true", key);
  snprintf(p2, sizeof(p2), "\"%s\": true", key);
  if (strstr(body, p1) != NULL || strstr(body, p2) != NULL) {
    return 1;
  }
  snprintf(p1, sizeof(p1), "\"%s\":false", key);
  snprintf(p2, sizeof(p2), "\"%s\": false", key);
  if (strstr(body, p1) != NULL || strstr(body, p2) != NULL) {
    return 0;
  }
  return fallback;
}

static float json_extract_float(const char* body, const char* key, float fallback) {
  if (body == NULL || key == NULL || key[0] == '\0') {
    return fallback;
  }
  char pattern[48] = {0};
  snprintf(pattern, sizeof(pattern), "\"%s\":", key);
  const char* p = strstr(body, pattern);
  if (p == NULL) {
    return fallback;
  }
  p += strlen(pattern);
  while (*p == ' ' || *p == '\t') {
    p++;
  }
  return (float)strtod(p, NULL);
}

static int json_extract_alloc_string(const char* body, const char* key, char** out_ptr, size_t* out_len) {
  if (body == NULL || key == NULL || out_ptr == NULL || out_len == NULL) {
    return 0;
  }
  *out_ptr = NULL;
  *out_len = 0;
  char p1[64] = {0};
  char p2[64] = {0};
  snprintf(p1, sizeof(p1), "\"%s\":\"", key);
  snprintf(p2, sizeof(p2), "\"%s\": \"", key);
  const char* start = strstr(body, p1);
  if (start != NULL) {
    start += strlen(p1);
  } else {
    start = strstr(body, p2);
    if (start != NULL) {
      start += strlen(p2);
    }
  }
  if (start == NULL) {
    return 0;
  }
  const char* end = strchr(start, '"');
  if (end == NULL) {
    return 0;
  }
  size_t n = (size_t)(end - start);
  char* buf = (char*)malloc(n + 1);
  if (buf == NULL) {
    return 0;
  }
  memcpy(buf, start, n);
  buf[n] = '\0';
  *out_ptr = buf;
  *out_len = n;
  return 1;
}

static char* json_escape_alloc(const char* src) {
  if (src == NULL) {
    return NULL;
  }
  size_t in_len = strlen(src);
  size_t cap = in_len * 2 + 1;
  char* out = (char*)malloc(cap);
  if (out == NULL) {
    return NULL;
  }
  size_t j = 0;
  for (size_t i = 0; i < in_len; ++i) {
    unsigned char c = (unsigned char)src[i];
    if (j + 3 >= cap) {
      size_t new_cap = cap * 2;
      char* new_out = (char*)realloc(out, new_cap);
      if (new_out == NULL) {
        free(out);
        return NULL;
      }
      out = new_out;
      cap = new_cap;
    }
    if (c == '"' || c == '\\') {
      out[j++] = '\\';
      out[j++] = (char)c;
    } else if (c == '\n') {
      out[j++] = '\\';
      out[j++] = 'n';
    } else if (c == '\r') {
      out[j++] = '\\';
      out[j++] = 'r';
    } else if (c == '\t') {
      out[j++] = '\\';
      out[j++] = 't';
    } else {
      out[j++] = (char)c;
    }
  }
  out[j] = '\0';
  return out;
}

static void parse_finish_result(const char* body, bb_finish_result_t* out_result) {
  out_result->transcript[0] = '\0';
  out_result->reply_text[0] = '\0';
  out_result->saved_input_path[0] = '\0';
  out_result->reply_wait_timed_out = 0;
  out_result->error_code[0] = '\0';
  (void)json_extract_string(body, "text", out_result->transcript, sizeof(out_result->transcript));
  (void)json_extract_string(body, "replyText", out_result->reply_text, sizeof(out_result->reply_text));
  (void)json_extract_string(body, "savedInputPath", out_result->saved_input_path, sizeof(out_result->saved_input_path));
  out_result->reply_wait_timed_out = json_extract_bool(body, "replyWaitTimedOut", 0);
  (void)json_extract_string(body, "error", out_result->error_code, sizeof(out_result->error_code));
}

static void parse_voice_verify_result(const char* body, bb_voice_verify_result_t* out_result) {
  if (out_result == NULL) {
    return;
  }
  out_result->match = json_extract_bool(body, "match", 0);
  out_result->confidence = json_extract_float(body, "confidence", 0.0f);
  out_result->message[0] = '\0';
  (void)json_extract_string(body, "message", out_result->message, sizeof(out_result->message));
  /* ADR-038: the cloud already returns the ASR-recognized passphrase text; surface
   * it so the lock screen can show "听到「…」" on a failed unlock. */
  out_result->transcript[0] = '\0';
  (void)json_extract_string(body, "transcript", out_result->transcript, sizeof(out_result->transcript));
}

static void parse_finish_stream_line(const char* line, bb_finish_stream_accum_t* accum) {
  if (line == NULL || accum == NULL || accum->result == NULL) {
    return;
  }

  char type[24] = {0};
  if (!json_extract_string(line, "type", type, sizeof(type))) {
    return;
  }

  if (strcmp(type, "status") == 0) {
    char phase[32] = {0};
    (void)json_extract_string(line, "phase", phase, sizeof(phase));
    if (accum->on_event != NULL) {
      emit_finish_stream_event(accum->on_event, accum->user_ctx, BB_FINISH_STREAM_EVENT_STATUS, phase, NULL, NULL, 0);
    }
    return;
  }

  if (strcmp(type, "asr.final") == 0) {
    char text[sizeof(accum->result->transcript)] = {0};
    (void)json_extract_string(line, "text", text, sizeof(text));
    /* snprintf 而非 strncpy：必空终止、绝不触发新 toolchain 的 stringop-truncation
     * （text 与目标同尺寸，strncpy(...,size-1) 在 GCC14 被当 error）。 */
    snprintf(accum->result->transcript, sizeof(accum->result->transcript), "%s", text);
    if (accum->on_event != NULL) {
      emit_finish_stream_event(accum->on_event, accum->user_ctx, BB_FINISH_STREAM_EVENT_ASR_FINAL, NULL, text, NULL, 0);
    }
    return;
  }

  if (strcmp(type, "reply.delta") == 0) {
    char text[sizeof(accum->result->reply_text)] = {0};
    (void)json_extract_string(line, "text", text, sizeof(text));
    snprintf(accum->result->reply_text, sizeof(accum->result->reply_text), "%s", text);
    if (accum->on_event != NULL) {
      emit_finish_stream_event(accum->on_event, accum->user_ctx, BB_FINISH_STREAM_EVENT_REPLY_DELTA, NULL, text, NULL,
                               0);
    }
    return;
  }

  if (strcmp(type, "prompt.open") == 0) {
    /* ADR-033: blocking menu over the HTTP NDJSON stream (short type). */
    bb_prompt_t p;
    if (bb_prompt_parse_open(line, &p) && accum->on_event != NULL) {
      emit_prompt_event(accum->on_event, accum->user_ctx, BB_FINISH_STREAM_EVENT_PROMPT_OPEN, &p);
    }
    return;
  }

  if (strcmp(type, "prompt.close") == 0) {
    bb_prompt_t p;
    memset(&p, 0, sizeof(p));
    (void)json_extract_string(line, "promptId", p.prompt_id, sizeof(p.prompt_id));
    (void)json_extract_string(line, "reason", p.reason, sizeof(p.reason));
    if (accum->on_event != NULL) {
      emit_prompt_event(accum->on_event, accum->user_ctx, BB_FINISH_STREAM_EVENT_PROMPT_CLOSE, &p);
    }
    return;
  }

  if (strcmp(type, "done") == 0 || strcmp(type, "voice.session.done") == 0) {
    parse_finish_result(line, accum->result);
    if (accum->on_event != NULL) {
      emit_finish_stream_event(accum->on_event, accum->user_ctx, BB_FINISH_STREAM_EVENT_VOICE_DONE, NULL, NULL, NULL,
                               accum->result->reply_wait_timed_out);
    }
    accum->saw_done = 1;
    return;
  }

  if (strcmp(type, "tts.chunk") == 0) {
    bb_tts_chunk_t* chunk = decode_tts_chunk_json(line);
    if (chunk == NULL) {
      return;
    }
    ESP_LOGI("bb_adapter", "tts.chunk decoded seq=%d pcm_bytes=%u", chunk->seq, (unsigned)chunk->pcm_len);
    dispatch_tts_chunk_event(accum->result, accum->on_event, accum->user_ctx, chunk);
    return;
  }

  if (strcmp(type, "tts.done") == 0) {
    if (accum->on_event != NULL) {
      emit_finish_stream_event(accum->on_event, accum->user_ctx, BB_FINISH_STREAM_EVENT_TTS_DONE, NULL, NULL, NULL, 0);
    }
    return;
  }

  if (strcmp(type, "thinking") == 0) {
    char text[128] = {0};
    (void)json_extract_string(line, "text", text, sizeof(text));
    if (accum->on_event != NULL && text[0] != '\0') {
      emit_finish_stream_event(accum->on_event, accum->user_ctx, BB_FINISH_STREAM_EVENT_THINKING, NULL, text, NULL, 0);
    }
    return;
  }

  if (strcmp(type, "tool_call") == 0) {
    char name[64] = {0};
    char hint[256] = {0}; /* full-ish tool command (adapter caps hint at 240) */
    (void)json_extract_string(line, "name", name, sizeof(name));
    (void)json_extract_string(line, "hint", hint, sizeof(hint)); /* ADR-030 */
    if (accum->on_event != NULL && name[0] != '\0') {
      emit_tool_call_event(accum->on_event, accum->user_ctx, name, hint);
    }
    return;
  }

  /* ADR-021-firmware-ui §1.2: butler dispatch progress frame */
  if (strcmp(type, "dispatch_status") == 0) {
    if (accum->on_event != NULL) {
      bb_dispatch_status_t ds = {0};
      /* dispatch object is nested under "dispatch" key */
      const char* dp = strstr(line, "\"dispatch\"");
      const char* src = (dp != NULL) ? strchr(dp, '{') : line;
      if (src == NULL) src = line;
      (void)json_extract_string(src, "phase",   ds.phase,   sizeof(ds.phase));
      (void)json_extract_string(src, "taskId",  ds.task_id, sizeof(ds.task_id));
      (void)json_extract_string(src, "cwd",     ds.cwd,     sizeof(ds.cwd));
      ds.elapsed_ms = (int64_t)json_extract_int(src, "elapsedMs", 0);
      if (ds.phase[0] != '\0') {
        emit_dispatch_event(accum->on_event, accum->user_ctx, &ds);
      }
    }
    return;
  }

  if (strcmp(type, "error") == 0) {
    (void)json_extract_string(line, "error", accum->result->error_code, sizeof(accum->result->error_code));
    if (accum->on_event != NULL) {
      emit_finish_stream_event(accum->on_event, accum->user_ctx, BB_FINISH_STREAM_EVENT_ERROR, NULL,
                               accum->result->error_code, NULL, 0);
    }
    accum->saw_error = 1;
  }
}

static esp_err_t http_event_handler_finish_stream(esp_http_client_event_t* evt) {
  bb_finish_stream_accum_t* accum = (bb_finish_stream_accum_t*)evt->user_data;
  if (accum == NULL) {
    return ESP_OK;
  }
  if (evt->event_id == HTTP_EVENT_ON_HEADER) {
    ESP_LOGI("bb_adapter", "finish_stream hdr: %s: %s", evt->header_key, evt->header_value);
    return ESP_OK;
  }
  if (evt->event_id == HTTP_EVENT_ON_FINISH) {
    ESP_LOGI("bb_adapter", "finish_stream HTTP_EVENT_ON_FINISH");
    return ESP_OK;
  }
  if (evt->event_id != HTTP_EVENT_ON_DATA || evt->data == NULL || evt->data_len <= 0) {
    return ESP_OK;
  }
  ESP_LOGI("bb_adapter", "finish_stream ON_DATA len=%d", evt->data_len);

  size_t need = accum->len + (size_t)evt->data_len + 1;
  if (need > accum->cap) {
    size_t new_cap = accum->cap == 0 ? 2048 : accum->cap;
    while (new_cap < need) {
      new_cap *= 2;
    }
    char* new_buf = (char*)realloc(accum->buf, new_cap);
    if (new_buf == NULL) {
      return ESP_ERR_NO_MEM;
    }
    accum->buf = new_buf;
    accum->cap = new_cap;
  }

  memcpy(accum->buf + accum->len, evt->data, (size_t)evt->data_len);
  accum->len += (size_t)evt->data_len;
  accum->buf[accum->len] = '\0';

  while (1) {
    char* nl = (char*)memchr(accum->buf, '\n', accum->len);
    if (nl == NULL) {
      break;
    }
    *nl = '\0';
    if (nl > accum->buf && nl[-1] == '\r') {
      nl[-1] = '\0';
    }
    if (accum->buf[0] != '\0') {
      parse_finish_stream_line(accum->buf, accum);
    }
    size_t consumed = (size_t)(nl - accum->buf) + 1;
    memmove(accum->buf, accum->buf + consumed, accum->len - consumed);
    accum->len -= consumed;
    accum->buf[accum->len] = '\0';
  }
  return ESP_OK;
}

esp_err_t bb_adapter_healthz(int* http_status) {
  bb_http_resp_t resp = {0};
  ESP_RETURN_ON_ERROR(http_get("/healthz", &resp), TAG, "healthz request failed");

  if (http_status != NULL) {
    *http_status = resp.status_code;
  }
  if (resp.status_code < 200 || resp.status_code >= 300) {
    ESP_LOGE(TAG, "healthz failed status=%d body=%s", resp.status_code, resp.body);
    return ESP_FAIL;
  }
  return ESP_OK;
}

esp_err_t bb_adapter_stream_start(bb_stream_ctx_t* ctx) {
  if (ctx == NULL) {
    return ESP_ERR_INVALID_ARG;
  }

  /* bbwire/2 (local_home_v2): dial /v2/dev/ws + hello + ptt.start. No Ogg
   * encoder — v2 sends raw PCM16 mic frames. */
  if (bb_transport_is_v2()) {
    return bb_bbwire2_stream_start(ctx);
  }

  memset(ctx, 0, sizeof(*ctx));
  snprintf(ctx->stream_id, sizeof(ctx->stream_id), "esp-%lld", (long long)bb_now_ms());
  ctx->next_seq = 1;
  ctx->ws_chunk_count = 0;

  /* Create Ogg/Opus encoder for both transport modes.
   * local_home: encoder output is base64-encoded and POSTed to /v1/stream/chunk.
   * cloud_saas: encoder output is sent as binary WebSocket frames. */
  log_mem_snapshot("stream start before encoder");
  ctx->ws_encoder = bb_ogg_opus_encoder_create(BBCLAW_AUDIO_SAMPLE_RATE, BBCLAW_AUDIO_CHANNELS, BBCLAW_STREAM_CHUNK_MS);
  if (ctx->ws_encoder == NULL) {
    ESP_LOGE(TAG, "encoder create failed stream=%s", ctx->stream_id);
    log_mem_snapshot("stream start encoder failed");
    return ESP_ERR_NO_MEM;
  }
  log_mem_snapshot("stream start after encoder");

  if (bb_transport_is_cloud_saas()) {
    char body[384] = {0};
    snprintf(body, sizeof(body),
             "{\"type\":\"request\",\"messageId\":\"start-%s\",\"deviceId\":\"%s\",\"kind\":\"voice.stream.start\","
             "\"payload\":{\"sessionKey\":\"%s\",\"streamId\":\"%s\",\"codec\":\"ogg_opus\",\"sampleRate\":%d,"
             "\"channels\":%d,\"frameDurationMs\":%d}}",
             ctx->stream_id, BBCLAW_DEVICE_ID, BBCLAW_SESSION_KEY, ctx->stream_id, BBCLAW_AUDIO_SAMPLE_RATE,
             BBCLAW_AUDIO_CHANNELS, BBCLAW_STREAM_CHUNK_MS);
    esp_err_t err = ws_send_text_message(body);
    if (err != ESP_OK) {
      bb_ogg_opus_encoder_destroy((bb_ogg_opus_encoder_t*)ctx->ws_encoder);
      ctx->ws_encoder = NULL;
      return err;
    }
    ESP_LOGI(TAG, "ws voice.stream.start stream=%s", ctx->stream_id);
    return ESP_OK;
  }

  char body[512] = {0};
  snprintf(body, sizeof(body),
           "{\"deviceId\":\"%s\",\"sessionKey\":\"%s\",\"streamId\":\"%s\",\"codec\":\"%s\",\"sampleRate\":%d,\"channels\":%d}",
           BBCLAW_DEVICE_ID, BBCLAW_SESSION_KEY, ctx->stream_id, BBCLAW_STREAM_CODEC, BBCLAW_AUDIO_SAMPLE_RATE,
           BBCLAW_AUDIO_CHANNELS);

  bb_http_resp_t resp = {0};
  esp_err_t http_err = http_post_json("/v1/stream/start", body, &resp);
  if (http_err != ESP_OK) {
    bb_ogg_opus_encoder_destroy((bb_ogg_opus_encoder_t*)ctx->ws_encoder);
    ctx->ws_encoder = NULL;
    ESP_LOGE(TAG, "stream start request failed err=%s", esp_err_to_name(http_err));
    return http_err;
  }

  if (resp.status_code < 200 || resp.status_code >= 300 || !body_contains_ok_true(resp.body)) {
    ESP_LOGE(TAG, "stream start failed status=%d body=%s", resp.status_code, resp.body);
    bb_ogg_opus_encoder_destroy((bb_ogg_opus_encoder_t*)ctx->ws_encoder);
    ctx->ws_encoder = NULL;
    return ESP_FAIL;
  }
#if BBCLAW_ADAPTER_STREAM_CHUNK_DIAG
  ESP_LOGI(TAG, "stream start ok streamId=%s (chunk diag on: wall_span_ms vs adapter MAX_STREAM_SECONDS)", ctx->stream_id);
#else
  ESP_LOGI(TAG, "stream start ok streamId=%s", ctx->stream_id);
#endif
  return ESP_OK;
}

esp_err_t bb_adapter_stream_chunk(bb_stream_ctx_t* ctx, const uint8_t* data, size_t len, int64_t ts_ms) {
  if (bb_transport_is_cloud_saas() || bb_transport_is_v2()) {
    return bb_adapter_stream_chunk_pcm(ctx, data, len, ts_ms);
  }
  if (ctx == NULL || data == NULL || len == 0U) {
    return ESP_ERR_INVALID_ARG;
  }

  size_t base64_len = 0;
  int ret = mbedtls_base64_encode(NULL, 0, &base64_len, data, len);
  if (ret != MBEDTLS_ERR_BASE64_BUFFER_TOO_SMALL) {
    return ESP_FAIL;
  }

  char* base64_buf = (char*)malloc(base64_len + 1);
  if (base64_buf == NULL) {
    return ESP_ERR_NO_MEM;
  }

  ret = mbedtls_base64_encode((unsigned char*)base64_buf, base64_len, &base64_len, data, len);
  if (ret != 0) {
    free(base64_buf);
    return ESP_FAIL;
  }
  base64_buf[base64_len] = '\0';

  size_t body_cap = base64_len + 512;
  char* body = (char*)malloc(body_cap);
  if (body == NULL) {
    free(base64_buf);
    return ESP_ERR_NO_MEM;
  }

  snprintf(body, body_cap,
           "{\"deviceId\":\"%s\",\"sessionKey\":\"%s\",\"streamId\":\"%s\",\"seq\":%d,\"timestampMs\":%lld,\"audioBase64\":\"%s\"}",
           BBCLAW_DEVICE_ID, BBCLAW_SESSION_KEY, ctx->stream_id, ctx->next_seq, (long long)ts_ms, base64_buf);

  if (ctx->first_ts_ms == 0) {
    ctx->first_ts_ms = ts_ms;
  }
  int64_t gap_prev_ms = (ctx->last_ts_ms != 0) ? (ts_ms - ctx->last_ts_ms) : 0;
  int64_t wall_span_ms = ts_ms - ctx->first_ts_ms;

  bb_http_resp_t resp = {0};
  int64_t http_t0 = bb_now_ms();
  esp_err_t err = http_post_json("/v1/stream/chunk", body, &resp);
  int64_t http_ms = bb_now_ms() - http_t0;

  free(body);
  free(base64_buf);

  if (err != ESP_OK) {
#if BBCLAW_ADAPTER_STREAM_CHUNK_DIAG
    ESP_LOGE(TAG,
             "stream chunk transport err=%s seq=%d ts=%lld wall_span_ms=%lld gap_prev_ms=%lld http_ms=%lld payload=%u",
             esp_err_to_name(err), ctx->next_seq, (long long)ts_ms, (long long)wall_span_ms, (long long)gap_prev_ms,
             (long long)http_ms, (unsigned)len);
#endif
    return err;
  }

  if (resp.status_code < 200 || resp.status_code >= 300 || !body_contains_ok_true(resp.body)) {
    ESP_LOGE(TAG,
             "stream chunk failed status=%d seq=%d body=%s wall_span_ms=%lld gap_prev_ms=%lld http_ms=%lld payload=%u",
             resp.status_code, ctx->next_seq, resp.body, (long long)wall_span_ms, (long long)gap_prev_ms,
             (long long)http_ms, (unsigned)len);
#if BBCLAW_ADAPTER_STREAM_CHUNK_DIAG
    ESP_LOGE(TAG, "stream chunk diag hint: server compares last_ts-first_ts to MAX_STREAM_SECONDS (default 90s)");
#endif
    return ESP_FAIL;
  }

  ctx->last_ts_ms = ts_ms;
  ctx->next_seq++;
#if BBCLAW_ADAPTER_STREAM_CHUNK_DIAG
  ESP_LOGI(TAG,
           "stream chunk ok seq=%d ts=%lld wall_span_ms=%lld gap_prev_ms=%lld http_ms=%lld payload=%u",
           ctx->next_seq - 1, (long long)ts_ms, (long long)wall_span_ms, (long long)gap_prev_ms, (long long)http_ms,
           (unsigned)len);
#endif
  return ESP_OK;
}

esp_err_t bb_adapter_stream_chunk_pcm(bb_stream_ctx_t* ctx, const uint8_t* pcm, size_t pcm_len, int64_t ts_ms) {
  (void)ts_ms;
  if (ctx == NULL || pcm == NULL || pcm_len == 0U) {
    return ESP_ERR_INVALID_ARG;
  }
  /* bbwire/2: one BINARY mic frame (8-byte header + raw PCM16), no Ogg encode. */
  if (bb_transport_is_v2()) {
    return bb_bbwire2_send_mic_pcm16(ctx, pcm, pcm_len);
  }
  if (!bb_transport_is_cloud_saas()) {
    return ESP_ERR_NOT_SUPPORTED;
  }
  if (ctx->ws_encoder == NULL) {
    return ESP_ERR_INVALID_STATE;
  }

  ctx->ws_chunk_count++;
  if (ctx->ws_chunk_count <= 2) {
    ESP_LOGI(TAG, "ws chunk encode stream=%s seq=%d pcm_bytes=%u stack_hw=%u", ctx->stream_id, ctx->ws_chunk_count,
             (unsigned)pcm_len, (unsigned)uxTaskGetStackHighWaterMark(NULL));
  }

  uint8_t* ogg_data = NULL;
  size_t ogg_len = 0;
  esp_err_t err = bb_ogg_opus_encoder_append_pcm16((bb_ogg_opus_encoder_t*)ctx->ws_encoder, (const int16_t*)pcm,
                                                   pcm_len / sizeof(int16_t), &ogg_data, &ogg_len);
  if (err != ESP_OK) {
    return err;
  }
  if (ctx->ws_chunk_count <= 2) {
    ESP_LOGI(TAG, "ws chunk encoded stream=%s seq=%d ogg_bytes=%u stack_hw=%u", ctx->stream_id, ctx->ws_chunk_count,
             (unsigned)ogg_len, (unsigned)uxTaskGetStackHighWaterMark(NULL));
  }
  if (ogg_data != NULL && ogg_len > 0U) {
    err = ws_send_binary_message(ogg_data, ogg_len);
    bb_ogg_opus_free(ogg_data);
    if (err != ESP_OK) {
      return err;
    }
  }
  return ESP_OK;
}

esp_err_t bb_adapter_stream_finish(const bb_stream_ctx_t* ctx, bb_finish_result_t* out_result) {
  if (ctx == NULL || out_result == NULL) {
    return ESP_ERR_INVALID_ARG;
  }
  if (bb_transport_is_cloud_saas() || bb_transport_is_v2()) {
    return bb_adapter_stream_finish_stream(ctx, out_result, NULL, NULL);
  }

  memset(out_result, 0, sizeof(*out_result));

  char body[320] = {0};
  snprintf(body, sizeof(body), "{\"deviceId\":\"%s\",\"sessionKey\":\"%s\",\"streamId\":\"%s\"}",
           BBCLAW_DEVICE_ID, BBCLAW_SESSION_KEY, ctx->stream_id);

  bb_http_dyn_resp_t resp = {0};
  ESP_RETURN_ON_ERROR(
      http_post_json_with_timeout_dynamic("/v1/stream/finish", body, &resp, BBCLAW_HTTP_STREAM_FINISH_TIMEOUT_MS), TAG,
      "stream finish request failed");

  const char* body_str = resp.body != NULL ? resp.body : "";
  int ok_json = body_contains_ok_true(body_str);
  out_result->http_status = resp.status_code;
  parse_finish_result(body_str, out_result);
  free(resp.body);
  resp.body = NULL;

  if (resp.status_code < 200 || resp.status_code >= 300 || !ok_json) {
    ESP_LOGE(TAG, "stream finish failed status=%d", resp.status_code);
    return ESP_FAIL;
  }

  /* 详细文本只在 bb_radio_app 按 phase 打一条，避免与业务层重复刷屏 */
  ESP_LOGD(TAG, "stream finish parsed stream=%s", ctx->stream_id);
  ESP_LOGI(TAG, "stream finish http ok stream=%s", ctx->stream_id);
  return ESP_OK;
}

esp_err_t bb_adapter_stream_finish_stream(const bb_stream_ctx_t* ctx, bb_finish_result_t* out_result,
                                          bb_finish_stream_event_cb_t on_event, void* user_ctx) {
  if (ctx == NULL || out_result == NULL) {
    return ESP_ERR_INVALID_ARG;
  }

  /* bbwire/2: ptt.stop + block until turn{idle}, streaming asr/reply/TTS. */
  if (bb_transport_is_v2()) {
    return bb_bbwire2_finish(ctx, out_result, on_event, user_ctx);
  }

  if (bb_transport_is_cloud_saas()) {
    bb_stream_ctx_t* mutable_ctx = (bb_stream_ctx_t*)ctx;
    memset(out_result, 0, sizeof(*out_result));
    if (mutable_ctx->ws_encoder != NULL) {
      uint8_t* ogg_tail = NULL;
      size_t ogg_tail_len = 0;
      esp_err_t flush_err =
          bb_ogg_opus_encoder_flush((bb_ogg_opus_encoder_t*)mutable_ctx->ws_encoder, &ogg_tail, &ogg_tail_len);
      if (flush_err != ESP_OK) {
        bb_ogg_opus_encoder_destroy((bb_ogg_opus_encoder_t*)mutable_ctx->ws_encoder);
        mutable_ctx->ws_encoder = NULL;
        return flush_err;
      }
      if (ogg_tail != NULL && ogg_tail_len > 0U) {
        flush_err = ws_send_binary_message(ogg_tail, ogg_tail_len);
        bb_ogg_opus_free(ogg_tail);
        if (flush_err != ESP_OK) {
          bb_ogg_opus_encoder_destroy((bb_ogg_opus_encoder_t*)mutable_ctx->ws_encoder);
          mutable_ctx->ws_encoder = NULL;
          return flush_err;
        }
      }
      bb_ogg_opus_encoder_destroy((bb_ogg_opus_encoder_t*)mutable_ctx->ws_encoder);
      mutable_ctx->ws_encoder = NULL;
    }

    xSemaphoreTake(s_ws.lock, portMAX_DELAY);
    ws_finish_reset_locked();
    s_ws.finish_result = out_result;
    s_ws.finish_on_event = on_event;
    s_ws.finish_user_ctx = user_ctx;
    /* 在 finish_waiting=1 之前清位(含 ABORT):barge-in 只在 finish_waiting=1
     * 时才设 ABORT,而我们在置位前已清干净,关掉了"设位后被清"的竞态窗口;
     * 同时清掉上一轮可能残留的 stale ABORT。clear 在锁内做,与置位互斥。 */
    xEventGroupClearBits(s_ws.events,
                         BB_WS_EVENT_DONE | BB_WS_EVENT_ERROR | BB_WS_EVENT_DISCONNECTED | BB_WS_EVENT_ABORT);
    s_ws.finish_waiting = 1;
    s_ws.finish_last_activity_ms = bb_now_ms(); /* 回合起点即活动 */
    snprintf(s_ws.finish_stream_id, sizeof(s_ws.finish_stream_id), "%s", ctx->stream_id);
    xSemaphoreGive(s_ws.lock);

    char body[320] = {0};
    snprintf(body, sizeof(body),
             "{\"type\":\"request\",\"messageId\":\"finish-%s\",\"deviceId\":\"%s\",\"kind\":\"voice.stream.finish\","
             "\"payload\":{\"sessionKey\":\"%s\",\"streamId\":\"%s\"}}",
             ctx->stream_id, BBCLAW_DEVICE_ID, BBCLAW_SESSION_KEY, ctx->stream_id);
    esp_err_t send_err = ws_send_text_message(body);
    if (send_err != ESP_OK) {
      xSemaphoreTake(s_ws.lock, portMAX_DELAY);
      ws_finish_reset_locked();
      xSemaphoreGive(s_ws.lock);
      if (mutable_ctx->ws_encoder != NULL) {
        bb_ogg_opus_encoder_destroy((bb_ogg_opus_encoder_t*)mutable_ctx->ws_encoder);
        mutable_ctx->ws_encoder = NULL;
      }
      return send_err;
    }

    /* 空闲超时等待（1s 切片轮询）：固定 deadline 会把「AI 深思考/多步工具调用、
     * 事件持续流向设备」的健康长回合拦腰砍断。切片间检查本回合最后活动时间,
     * 事件到达即续期;DONE/ERROR/ABORT 位仍即时唤醒(不受切片影响)。不用
     * 「事件组 ACTIVITY 位续期」方案——循环内清位与 DONE/ABORT 置位存在竞态
     * (正是 arm 点注释里修过的那类窗口)。 */
    const int64_t wait_start_ms = bb_now_ms();
    EventBits_t bits = 0;
    for (;;) {
      bits = xEventGroupWaitBits(
          s_ws.events, BB_WS_EVENT_DONE | BB_WS_EVENT_ERROR | BB_WS_EVENT_DISCONNECTED | BB_WS_EVENT_ABORT, pdFALSE,
          pdFALSE, pdMS_TO_TICKS(1000));
      if ((bits & (BB_WS_EVENT_DONE | BB_WS_EVENT_ERROR | BB_WS_EVENT_DISCONNECTED | BB_WS_EVENT_ABORT)) != 0U) {
        break; /* 真事件,走原归因逻辑 */
      }
      const int64_t now_ms = bb_now_ms();
      xSemaphoreTake(s_ws.lock, portMAX_DELAY);
      const int64_t last_ms = s_ws.finish_last_activity_ms;
      xSemaphoreGive(s_ws.lock);
      if (now_ms - last_ms >= (int64_t)BBCLAW_STREAM_FINISH_IDLE_TIMEOUT_MS) {
        break; /* 真静默超限 → bits==0,下方归因 VOICE_SESSION_TIMEOUT */
      }
#if BBCLAW_STREAM_FINISH_MAX_TOTAL_MS > 0
      if (now_ms - wait_start_ms >= (int64_t)BBCLAW_STREAM_FINISH_MAX_TOTAL_MS) {
        break;
      }
#endif
    }
    xSemaphoreTake(s_ws.lock, portMAX_DELAY);
    s_ws.finish_waiting = 0;
    if ((bits & BB_WS_EVENT_DONE) == 0U) {
      if (out_result->error_code[0] == '\0') {
        /* ADR-028 §2.5.1:本地 barge-in 中断优先于 timeout/disconnect 归因。 */
        snprintf(out_result->error_code, sizeof(out_result->error_code), "%s",
                 (bits & BB_WS_EVENT_ABORT) != 0U          ? "ABORTED_BY_USER"
                 : (bits & BB_WS_EVENT_DISCONNECTED) != 0U ? "WS_DISCONNECTED"
                                                           : "VOICE_SESSION_TIMEOUT");
      }
      ws_finish_reset_locked();
      xSemaphoreGive(s_ws.lock);
      if (mutable_ctx->ws_encoder != NULL) {
        bb_ogg_opus_encoder_destroy((bb_ogg_opus_encoder_t*)mutable_ctx->ws_encoder);
        mutable_ctx->ws_encoder = NULL;
      }
      return ESP_FAIL;
    }
    ws_finish_reset_locked();
    xSemaphoreGive(s_ws.lock);
    ESP_LOGI(TAG, "ws voice.stream.finish stream=%s", ctx->stream_id);
    return ESP_OK;
  }

  /* local_home: flush any remaining PCM samples from the Ogg/Opus encoder,
   * send the final Ogg pages as a chunk, then destroy the encoder. */
  bb_stream_ctx_t* mutable_ctx = (bb_stream_ctx_t*)ctx;
  if (mutable_ctx->ws_encoder != NULL) {
    uint8_t* ogg_tail = NULL;
    size_t ogg_tail_len = 0;
    esp_err_t flush_err =
        bb_ogg_opus_encoder_flush((bb_ogg_opus_encoder_t*)mutable_ctx->ws_encoder, &ogg_tail, &ogg_tail_len);
    if (flush_err == ESP_OK && ogg_tail != NULL && ogg_tail_len > 0U) {
      /* Send the final Ogg pages as a regular HTTP chunk before finishing. */
      (void)bb_adapter_stream_chunk(mutable_ctx, ogg_tail, ogg_tail_len, bb_now_ms());
      bb_ogg_opus_free(ogg_tail);
    } else if (ogg_tail != NULL) {
      bb_ogg_opus_free(ogg_tail);
    }
    bb_ogg_opus_encoder_destroy((bb_ogg_opus_encoder_t*)mutable_ctx->ws_encoder);
    mutable_ctx->ws_encoder = NULL;
  }

  memset(out_result, 0, sizeof(*out_result));

  char body[384] = {0};
  /* When no event callback is provided (voice_target_agent mode) the caller
   * only needs the ASR transcript — send replyMode=asr so the adapter skips
   * the OpenClaw delivery round-trip, saving ~5-20 s of latency. */
  const char* reply_mode = (on_event != NULL) ? "stream" : "asr";
  snprintf(body, sizeof(body), "{\"deviceId\":\"%s\",\"sessionKey\":\"%s\",\"streamId\":\"%s\",\"replyMode\":\"%s\"}",
           BBCLAW_DEVICE_ID, BBCLAW_SESSION_KEY, ctx->stream_id, reply_mode);

  char url[256] = {0};
  snprintf(url, sizeof(url), "%s%s", active_base_url(), "/v1/stream/finish");
  bb_finish_stream_accum_t accum = {
      .result = out_result,
      .on_event = on_event,
      .user_ctx = user_ctx,
  };

  esp_http_client_config_t cfg;
  bb_http_cfg_init(&cfg, url, BBCLAW_HTTP_STREAM_FINISH_TIMEOUT_MS, HTTP_METHOD_POST,
                   http_event_handler_finish_stream, &accum);

  esp_http_client_handle_t client = esp_http_client_init(&cfg);
  if (client == NULL) {
    return ESP_ERR_NO_MEM;
  }

  esp_http_client_set_header(client, "Content-Type", "application/json");
  esp_http_client_set_post_field(client, body, (int)strlen(body));

  esp_err_t err = esp_http_client_perform(client);
  if (err == ESP_OK) {
    out_result->http_status = esp_http_client_get_status_code(client);
  }
  esp_http_client_cleanup(client);

  ESP_LOGI(TAG, "stream finish raw: err=%s status=%d saw_done=%d saw_error=%d buf_len=%u buf=%.200s",
           esp_err_to_name(err), out_result->http_status, accum.saw_done, accum.saw_error,
           (unsigned)accum.len, accum.buf != NULL ? accum.buf : "(null)");

  /* 最后一段 NDJSON 可能无换行，仅触发 ON_DATA 不进入按行分割，需补解析 */
  if (err == ESP_OK && accum.buf != NULL && accum.len > 0) {
    while (accum.len > 0 && (accum.buf[accum.len - 1] == '\n' || accum.buf[accum.len - 1] == '\r')) {
      accum.len--;
    }
    accum.buf[accum.len] = '\0';
    if (accum.len > 0) {
      parse_finish_stream_line(accum.buf, &accum);
    }
    if (!accum.saw_done && !accum.saw_error) {
      ESP_LOGW(TAG, "stream finish NDJSON tail still no done/error len=%u preview=%.120s", (unsigned)accum.len,
               accum.buf);
    }
  }

  free(accum.buf);
  accum.buf = NULL;

  if (err != ESP_OK) {
    return err;
  }
  if (out_result->http_status < 200 || out_result->http_status >= 300) {
    return ESP_FAIL;
  }
  if (accum.saw_error || !accum.saw_done) {
    if (out_result->error_code[0] == '\0' && !accum.saw_done) {
      strncpy(out_result->error_code, "STREAM_FINISH_INCOMPLETE", sizeof(out_result->error_code) - 1);
      out_result->error_code[sizeof(out_result->error_code) - 1] = '\0';
    }
    return ESP_FAIL;
  }

  ESP_LOGI(TAG, "stream finish stream ok stream=%s", ctx->stream_id);
  return ESP_OK;
}

esp_err_t bb_adapter_voice_verify_pcm16(const uint8_t* pcm, size_t pcm_len, bb_voice_verify_result_t* out_result) {
  if (pcm == NULL || pcm_len == 0U || out_result == NULL) {
    return ESP_ERR_INVALID_ARG;
  }
  if (!bb_transport_is_cloud_saas()) {
    return ESP_ERR_NOT_SUPPORTED;
  }
  ESP_RETURN_ON_ERROR(ws_client_ensure_connected(), TAG, "ws connect failed");

  memset(out_result, 0, sizeof(*out_result));

  size_t base64_len = 0;
  int ret = mbedtls_base64_encode(NULL, 0, &base64_len, pcm, pcm_len);
  if (ret != MBEDTLS_ERR_BASE64_BUFFER_TOO_SMALL || base64_len == 0U) {
    return ESP_FAIL;
  }

  char* base64_buf = (char*)malloc(base64_len + 1U);
  if (base64_buf == NULL) {
    return ESP_ERR_NO_MEM;
  }
  ret = mbedtls_base64_encode((unsigned char*)base64_buf, base64_len, &base64_len, pcm, pcm_len);
  if (ret != 0) {
    free(base64_buf);
    return ESP_FAIL;
  }
  base64_buf[base64_len] = '\0';

  size_t body_cap = base64_len + 320U;
  char* body = (char*)malloc(body_cap);
  if (body == NULL) {
    free(base64_buf);
    return ESP_ERR_NO_MEM;
  }

  char message_id[64] = {0};
  snprintf(message_id, sizeof(message_id), "verify-%lld", (long long)bb_now_ms());
  snprintf(body, body_cap,
           "{\"type\":\"request\",\"messageId\":\"%s\",\"deviceId\":\"%s\",\"kind\":\"voice.verify\","
           "\"sessionKey\":\"%s\",\"streamId\":\"%s\",\"codec\":\"pcm16\",\"sampleRate\":%d,\"channels\":1,\"audioBase64\":\"%s\"}",
           message_id, BBCLAW_DEVICE_ID, BBCLAW_SESSION_KEY, message_id, BBCLAW_AUDIO_SAMPLE_RATE, base64_buf);
  free(base64_buf);

  xSemaphoreTake(s_ws.lock, portMAX_DELAY);
  ws_verify_reset_locked();
  s_ws.verify_result = out_result;
  s_ws.verify_waiting = 1;
  snprintf(s_ws.verify_message_id, sizeof(s_ws.verify_message_id), "%s", message_id);
  xSemaphoreGive(s_ws.lock);
  xEventGroupClearBits(s_ws.events, BB_WS_EVENT_VERIFY_DONE | BB_WS_EVENT_ERROR | BB_WS_EVENT_DISCONNECTED);

  esp_err_t send_err = ws_send_text_message(body);
  free(body);
  if (send_err != ESP_OK) {
    xSemaphoreTake(s_ws.lock, portMAX_DELAY);
    ws_verify_reset_locked();
    xSemaphoreGive(s_ws.lock);
    return send_err;
  }

  EventBits_t bits = xEventGroupWaitBits(s_ws.events, BB_WS_EVENT_VERIFY_DONE | BB_WS_EVENT_ERROR | BB_WS_EVENT_DISCONNECTED,
                                         pdFALSE, pdFALSE, pdMS_TO_TICKS(BBCLAW_HTTP_STREAM_FINISH_TIMEOUT_MS));
  xSemaphoreTake(s_ws.lock, portMAX_DELAY);
  s_ws.verify_waiting = 0;
  if ((bits & BB_WS_EVENT_VERIFY_DONE) == 0U) {
    if (out_result->message[0] == '\0') {
      snprintf(out_result->message, sizeof(out_result->message), "%s",
               (bits & BB_WS_EVENT_DISCONNECTED) != 0U ? "voice.verify disconnected" : "voice.verify timeout");
    }
    ws_verify_reset_locked();
    xSemaphoreGive(s_ws.lock);
    return ESP_FAIL;
  }
  ws_verify_reset_locked();
  xSemaphoreGive(s_ws.lock);
  ESP_LOGI(TAG, "ws voice.verify ok match=%d confidence=%.3f", out_result->match, (double)out_result->confidence);
  return ESP_OK;
}

/* ── ADR-027: Home Adapter ("机器") switching ───────────────────────────── */

/* Parse the `sites` array out of a sites.list reply body. Walks each object in
 * the array and extracts the per-site fields with the existing scalar helpers
 * (scoped to a copy of the object so labels/ids don't bleed across entries). */
static int parse_sites_array(const char* body, bb_site_info_t* out, int max) {
  if (body == NULL || out == NULL || max <= 0) {
    return 0;
  }
  const char* arr = strstr(body, "\"sites\"");
  if (arr == NULL) {
    return 0;
  }
  arr = strchr(arr, '[');
  if (arr == NULL) {
    return 0;
  }
  int n = 0;
  const char* p = arr + 1;
  while (n < max) {
    const char* obj = strchr(p, '{');
    if (obj == NULL) {
      break;
    }
    int depth = 0;
    const char* end = obj;
    for (; *end != '\0'; end++) {
      if (*end == '{') {
        depth++;
      } else if (*end == '}') {
        depth--;
        if (depth == 0) {
          break;
        }
      }
    }
    if (*end != '}') {
      break;
    }
    size_t len = (size_t)(end - obj + 1);
    char tmp[224];
    if (len >= sizeof(tmp)) {
      len = sizeof(tmp) - 1;
    }
    memcpy(tmp, obj, len);
    tmp[len] = '\0';
    memset(&out[n], 0, sizeof(out[n]));
    (void)json_extract_string(tmp, "homeSiteId", out[n].home_site_id, sizeof(out[n].home_site_id));
    (void)json_extract_string(tmp, "label", out[n].label, sizeof(out[n].label));
    out[n].online = (uint8_t)json_extract_bool(tmp, "online", 0);
    out[n].active = (uint8_t)json_extract_bool(tmp, "active", 0);
    if (out[n].home_site_id[0] != '\0') {
      n++;
    }
    p = end + 1;
    const char* q = p;
    while (*q == ' ' || *q == ',' || *q == '\n' || *q == '\r' || *q == '\t') {
      q++;
    }
    if (*q == ']' || *q == '\0') {
      break;
    }
  }
  return n;
}

/* Send a sites.* control frame and block on the matching reply/error. */
static esp_err_t sites_request(int kind, const char* home_site_id, bb_sites_req_t* req) {
  if (!bb_transport_is_cloud_saas()) {
    return ESP_ERR_NOT_SUPPORTED;
  }
  ESP_RETURN_ON_ERROR(ws_client_ensure_connected(), TAG, "ws connect failed");

  char message_id[64] = {0};
  snprintf(message_id, sizeof(message_id), "sites-%lld", (long long)bb_now_ms());

  char body[256];
  if (kind == 0) {
    snprintf(body, sizeof(body),
             "{\"type\":\"request\",\"kind\":\"sites.list\",\"messageId\":\"%s\",\"deviceId\":\"%s\"}",
             message_id, BBCLAW_DEVICE_ID);
  } else {
    snprintf(body, sizeof(body),
             "{\"type\":\"request\",\"kind\":\"sites.activate\",\"messageId\":\"%s\",\"deviceId\":\"%s\","
             "\"payload\":{\"homeSiteId\":\"%s\"}}",
             message_id, BBCLAW_DEVICE_ID, home_site_id != NULL ? home_site_id : "");
  }

  xSemaphoreTake(s_ws.lock, portMAX_DELAY);
  ws_sites_reset_locked();
  s_ws.sites_req = req;
  s_ws.sites_waiting = 1;
  snprintf(s_ws.sites_message_id, sizeof(s_ws.sites_message_id), "%s", message_id);
  xSemaphoreGive(s_ws.lock);
  xEventGroupClearBits(s_ws.events, BB_WS_EVENT_SITES_DONE | BB_WS_EVENT_ERROR | BB_WS_EVENT_DISCONNECTED);

  esp_err_t send_err = ws_send_text_message(body);
  if (send_err != ESP_OK) {
    xSemaphoreTake(s_ws.lock, portMAX_DELAY);
    ws_sites_reset_locked();
    xSemaphoreGive(s_ws.lock);
    return send_err;
  }

  EventBits_t bits = xEventGroupWaitBits(s_ws.events,
                                         BB_WS_EVENT_SITES_DONE | BB_WS_EVENT_DISCONNECTED,
                                         pdFALSE, pdFALSE, pdMS_TO_TICKS(8000));
  xSemaphoreTake(s_ws.lock, portMAX_DELAY);
  ws_sites_reset_locked();
  esp_err_t result;
  if ((bits & BB_WS_EVENT_SITES_DONE) != 0U && req->ok) {
    result = ESP_OK;
  } else {
    if (req->err_code[0] == '\0') {
      snprintf(req->err_code, sizeof(req->err_code), "%s",
               (bits & BB_WS_EVENT_DISCONNECTED) != 0U ? "DISCONNECTED" : "TIMEOUT");
    }
    result = ESP_FAIL;
  }
  xSemaphoreGive(s_ws.lock);
  return result;
}

esp_err_t bb_adapter_sites_list(bb_site_info_t* out_sites, int max_sites, int* out_count) {
  if (out_sites == NULL || max_sites <= 0 || out_count == NULL) {
    return ESP_ERR_INVALID_ARG;
  }
  *out_count = 0;
  bb_sites_req_t req;
  memset(&req, 0, sizeof(req));
  req.kind = 0;
  req.sites = out_sites;
  req.max = max_sites;
  esp_err_t err = sites_request(0, NULL, &req);
  *out_count = req.count;
  if (err == ESP_OK) {
    ESP_LOGI(TAG, "ws sites.list ok count=%d", req.count);
  } else {
    ESP_LOGW(TAG, "ws sites.list failed err=%s code=%s", esp_err_to_name(err), req.err_code);
  }
  return err;
}

esp_err_t bb_adapter_sites_activate(const char* home_site_id, char* out_active_id, size_t active_len,
                                    char* out_err_code, size_t err_len) {
  if (home_site_id == NULL || home_site_id[0] == '\0') {
    return ESP_ERR_INVALID_ARG;
  }
  bb_sites_req_t req;
  memset(&req, 0, sizeof(req));
  req.kind = 1;
  esp_err_t err = sites_request(1, home_site_id, &req);
  if (out_active_id != NULL && active_len > 0U) {
    snprintf(out_active_id, active_len, "%s", req.active_id);
  }
  if (out_err_code != NULL && err_len > 0U) {
    snprintf(out_err_code, err_len, "%s", req.err_code);
  }
  ESP_LOGI(TAG, "ws sites.activate '%s' -> %s active=%s code=%s", home_site_id, esp_err_to_name(err),
           req.active_id, req.err_code);
  return err;
}

/* Strip markdown formatting and other characters that the TTS engine would
 * either pronounce literally ("asterisk asterisk Sonnet 4.5 asterisk asterisk")
 * or skip while truncating the rest of the chunk. Output buffer is heap-
 * allocated; caller frees. NUL-terminated. UTF-8 safe.
 *
 * Removed: *, `, ~, _ runs (markdown emphasis / code marks); leading # and >
 * at line start; HTML-ish <tag>; BOM and zero-width joiners; ASCII control
 * chars. \r\n\t collapse to a single space; runs of spaces collapse to one.
 * Markdown link [text](url) keeps only "text"; image ![alt](url) keeps "alt".
 *
 * Returns NULL only on allocation failure or when input becomes entirely empty
 * after sanitization (caller should treat as "skip"). */
static char* tts_sanitize_alloc(const char* in) {
  if (in == NULL) return NULL;
  size_t in_len = strlen(in);
  char* out = (char*)malloc(in_len + 1);
  if (out == NULL) return NULL;
  size_t i = 0, j = 0;
  int prev_space = 1;       /* leading-trim: suppress leading spaces */
  int at_line_start = 1;
  while (in[i] != '\0') {
    unsigned char c = (unsigned char)in[i];

    /* Multi-byte UTF-8 lead bytes: pass through, but drop known zero-width
     * codepoints first. */
    if (c >= 0x80) {
      if (c == 0xEF && (unsigned char)in[i + 1] == 0xBB && (unsigned char)in[i + 2] == 0xBF) {
        i += 3;
        continue; /* BOM */
      }
      if (c == 0xE2 && (unsigned char)in[i + 1] == 0x80) {
        unsigned char third = (unsigned char)in[i + 2];
        if (third == 0x8B || third == 0x8C || third == 0x8D) {
          i += 3;
          continue; /* ZWSP / ZWNJ / ZWJ */
        }
      }
      if (c == 0xE2 && (unsigned char)in[i + 1] == 0x81 && (unsigned char)in[i + 2] == 0xA0) {
        i += 3;
        continue; /* WORD JOINER */
      }
      int seq_len = 1;
      if ((c & 0xE0) == 0xC0)
        seq_len = 2;
      else if ((c & 0xF0) == 0xE0)
        seq_len = 3;
      else if ((c & 0xF8) == 0xF0)
        seq_len = 4;
      for (int k = 0; k < seq_len && in[i + k] != '\0'; k++) {
        out[j++] = in[i + k];
      }
      i += (size_t)seq_len;
      prev_space = 0;
      at_line_start = 0;
      continue;
    }

    /* Whitespace and line breaks. */
    if (c == '\r' || c == '\n') {
      if (!prev_space) {
        out[j++] = ' ';
        prev_space = 1;
      }
      at_line_start = 1;
      i++;
      continue;
    }
    if (c == '\t' || c == ' ') {
      if (!prev_space) {
        out[j++] = ' ';
        prev_space = 1;
      }
      i++;
      continue;
    }
    if (c < 0x20) {
      i++;
      continue; /* other control chars */
    }

    /* Markdown emphasis / code / strike marks. */
    if (c == '*' || c == '`' || c == '~' || c == '_') {
      i++;
      continue;
    }

    /* Path separators in long ASCII paths (e.g. /Volumes/1TB/github/foo) are
     * one un-tokenizable run for Chinese TTS engines and tend to be skipped.
     * Replacing with a space exposes each segment as its own readable token. */
    if (c == '/' || c == '\\') {
      if (!prev_space) {
        out[j++] = ' ';
        prev_space = 1;
      }
      i++;
      at_line_start = 0;
      continue;
    }

    /* Line-leading ATX header '#' run. */
    if (at_line_start && c == '#') {
      while (in[i] == '#') i++;
      if (in[i] == ' ' || in[i] == '\t') i++;
      continue;
    }

    /* Line-leading blockquote '>'. */
    if (at_line_start && c == '>') {
      while (in[i] == '>') i++;
      if (in[i] == ' ' || in[i] == '\t') i++;
      continue;
    }

    /* Markdown image: ![alt](url) — keep "alt". */
    if (c == '!' && in[i + 1] == '[') {
      i += 2;
      while (in[i] != '\0' && in[i] != ']' && in[i] != '\n') {
        out[j++] = in[i++];
        prev_space = 0;
      }
      if (in[i] == ']') i++;
      if (in[i] == '(') {
        while (in[i] != '\0' && in[i] != ')') i++;
        if (in[i] == ')') i++;
      }
      at_line_start = 0;
      continue;
    }

    /* Markdown link: [text](url) — keep "text". Only when ] is followed by (. */
    if (c == '[') {
      size_t k = i + 1;
      while (in[k] != '\0' && in[k] != ']' && in[k] != '\n') k++;
      if (in[k] == ']' && in[k + 1] == '(') {
        i++; /* skip '[' */
        while (in[i] != '\0' && in[i] != ']') {
          out[j++] = in[i++];
          prev_space = 0;
        }
        if (in[i] == ']') i++;
        if (in[i] == '(') {
          while (in[i] != '\0' && in[i] != ')') i++;
          if (in[i] == ')') i++;
        }
        at_line_start = 0;
        continue;
      }
      /* not a link — fall through and keep '[' literally */
    }

    /* HTML-ish tag: <letter…> or </…>. Drop. */
    if (c == '<') {
      char next = in[i + 1];
      if ((next >= 'a' && next <= 'z') || (next >= 'A' && next <= 'Z') || next == '/') {
        i++;
        while (in[i] != '\0' && in[i] != '>') i++;
        if (in[i] == '>') i++;
        continue;
      }
    }

    out[j++] = (char)c;
    prev_space = 0;
    at_line_start = 0;
    i++;
  }
  /* Trim trailing space. */
  while (j > 0 && out[j - 1] == ' ') j--;
  out[j] = '\0';
  if (j == 0) {
    free(out);
    return NULL;
  }
  return out;
}

esp_err_t bb_adapter_tts_synthesize_pcm16(const char* text, bb_tts_audio_t* out_audio, int seg_idx) {
  if (text == NULL || out_audio == NULL) {
    return ESP_ERR_INVALID_ARG;
  }
  memset(out_audio, 0, sizeof(*out_audio));

  /* bbwire/2 has no synthesize RPC — reply TTS arrives as downlink frames during
   * a turn. An on-demand synthesize (notifications) is not supported on v2; fail
   * cleanly rather than POST /v1/tts/synthesize at the (wrong) adapter origin. */
  if (bb_transport_is_v2()) {
    return ESP_ERR_NOT_SUPPORTED;
  }

  char* cleaned = tts_sanitize_alloc(text);
  if (cleaned == NULL) {
    /* Empty after sanitize (pure formatting / whitespace) or OOM. Treat as
     * a clean skip so the caller's loop moves on to the next chunk. */
    ESP_LOGI(TAG, "tts: skip empty-after-sanitize chunk (orig_len=%u)", (unsigned)strlen(text));
    return ESP_OK;
  }
  ESP_LOGI(TAG, "tts: seg=%d sanitize raw=%u clean=%u", seg_idx, (unsigned)strlen(text), (unsigned)strlen(cleaned));

  char* escaped = json_escape_alloc(cleaned);
  free(cleaned);
  if (escaped == NULL) {
    return ESP_ERR_NO_MEM;
  }

  size_t body_cap = strlen(escaped) + 256;
  char* body = (char*)malloc(body_cap);
  if (body == NULL) {
    free(escaped);
    return ESP_ERR_NO_MEM;
  }
  /* Issue #169: include segIdx so adapter logs can correlate each synth
   * call with the firmware subtitle cutover timestamp. */
  snprintf(body, body_cap,
           "{\"text\":\"%s\",\"codec\":\"pcm16\",\"sampleRate\":%d,\"channels\":%d,\"deviceId\":\"%s\",\"segIdx\":%d}",
           escaped, BBCLAW_TTS_SAMPLE_RATE, BBCLAW_TTS_CHANNELS, BBCLAW_DEVICE_ID, seg_idx);
  free(escaped);

  bb_http_dyn_resp_t resp = {0};
  esp_err_t err = http_post_json_dynamic("/v1/tts/synthesize", body, &resp);
  free(body);
  if (err != ESP_OK) {
    free(resp.body);
    return err;
  }

  if (resp.status_code < 200 || resp.status_code >= 300 || !body_contains_ok_true(resp.body != NULL ? resp.body : "")) {
    ESP_LOGE(TAG, "tts synth failed status=%d body=%s", resp.status_code, resp.body != NULL ? resp.body : "");
    free(resp.body);
    return ESP_FAIL;
  }

  char format[16] = {0};
  (void)json_extract_string(resp.body, "format", format, sizeof(format));
  if (format[0] == '\0') {
    /*
     * Backward-compat fallback:
     * if format is absent, assume pcm16 when firmware explicitly requests pcm16.
     */
    ESP_LOGW(TAG, "tts response missing format, assume pcm16");
    snprintf(format, sizeof(format), "pcm16");
  }
  if (strcmp(format, "pcm16") != 0 && strcmp(format, "pcm_s16le") != 0) {
    ESP_LOGE(TAG, "tts format unsupported format=%s body=%s", format, resp.body != NULL ? resp.body : "");
    free(resp.body);
    return ESP_ERR_NOT_SUPPORTED;
  }

  char* audio_b64 = NULL;
  size_t audio_b64_len = 0;
  if (!json_extract_alloc_string(resp.body, "audioBase64", &audio_b64, &audio_b64_len) || audio_b64_len == 0U) {
    free(resp.body);
    return ESP_FAIL;
  }

  size_t pcm_cap = 0;
  int ret = mbedtls_base64_decode(NULL, 0, &pcm_cap, (const unsigned char*)audio_b64, audio_b64_len);
  if (ret != MBEDTLS_ERR_BASE64_BUFFER_TOO_SMALL || pcm_cap == 0U) {
    free(audio_b64);
    free(resp.body);
    return ESP_FAIL;
  }
  uint8_t* pcm = (uint8_t*)malloc(pcm_cap);
  if (pcm == NULL) {
    free(audio_b64);
    free(resp.body);
    return ESP_ERR_NO_MEM;
  }
  size_t pcm_len = 0;
  ret = mbedtls_base64_decode(pcm, pcm_cap, &pcm_len, (const unsigned char*)audio_b64, audio_b64_len);
  free(audio_b64);
  /* Must read JSON fields before free(resp.body) — sampleRate/channels live in data{} */
  int sr = json_extract_int(resp.body, "sampleRate", BBCLAW_TTS_SAMPLE_RATE);
  int ch = json_extract_int(resp.body, "channels", BBCLAW_TTS_CHANNELS);
  free(resp.body);
  if (ret != 0 || pcm_len == 0U) {
    free(pcm);
    return ESP_FAIL;
  }
  if (sr <= 0) {
    sr = BBCLAW_TTS_SAMPLE_RATE;
  }
  if (ch <= 0) {
    ch = BBCLAW_TTS_CHANNELS;
  }

  out_audio->pcm_data = pcm;
  out_audio->pcm_len = pcm_len;
  out_audio->sample_rate = sr;
  out_audio->channels = ch;
  return ESP_OK;
}

void bb_adapter_tts_audio_free(bb_tts_audio_t* audio) {
  if (audio == NULL) {
    return;
  }
  if (audio->pcm_data != NULL) {
    free(audio->pcm_data);
  }
  memset(audio, 0, sizeof(*audio));
}

void bb_adapter_tts_chunks_free(bb_tts_chunk_t* head) {
  while (head != NULL) {
    bb_tts_chunk_t* next = head->next;
    free(head->pcm_data);
    free(head);
    head = next;
  }
}

esp_err_t bb_adapter_display_pull(bb_display_task_t* out_task) {
  if (out_task == NULL) {
    return ESP_ERR_INVALID_ARG;
  }
  memset(out_task, 0, sizeof(*out_task));

  char body[192] = {0};
  snprintf(body, sizeof(body), "{\"deviceId\":\"%s\"}", BBCLAW_DEVICE_ID);

  bb_http_resp_t resp = {0};
  ESP_RETURN_ON_ERROR(http_post_json("/v1/display/pull", body, &resp), TAG, "display pull request failed");

  if (resp.status_code < 200 || resp.status_code >= 300 || !body_contains_ok_true(resp.body)) {
    ESP_LOGE(TAG, "display pull failed status=%d body=%s", resp.status_code, resp.body);
    return ESP_FAIL;
  }

  if (strstr(resp.body, "\"task\":null") != NULL) {
    return ESP_OK;
  }

  out_task->has_task = 1;
  (void)json_extract_string(resp.body, "taskId", out_task->task_id, sizeof(out_task->task_id));
  if (!json_extract_string(resp.body, "displayText", out_task->display_text, sizeof(out_task->display_text))) {
    (void)json_extract_string(resp.body, "title", out_task->display_text, sizeof(out_task->display_text));
  }
  return ESP_OK;
}

esp_err_t bb_adapter_display_ack(const char* task_id, const char* action_id) {
  if (task_id == NULL || task_id[0] == '\0') {
    return ESP_ERR_INVALID_ARG;
  }

  char body[256] = {0};
  snprintf(body, sizeof(body), "{\"deviceId\":\"%s\",\"taskId\":\"%s\",\"actionId\":\"%s\"}", BBCLAW_DEVICE_ID, task_id,
           action_id != NULL ? action_id : "shown");

  bb_http_resp_t resp = {0};
  ESP_RETURN_ON_ERROR(http_post_json("/v1/display/ack", body, &resp), TAG, "display ack request failed");

  if (resp.status_code < 200 || resp.status_code >= 300 || !body_contains_ok_true(resp.body)) {
    ESP_LOGE(TAG, "display ack failed status=%d body=%s", resp.status_code, resp.body);
    return ESP_FAIL;
  }
  return ESP_OK;
}
