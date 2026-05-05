#include "bb_adapter_client.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>

#include "bb_device_config.h"
#include "bb_notification.h"
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
  int finish_saw_done;
  int finish_saw_error;
  char finish_stream_id[64];
  bb_voice_verify_result_t* verify_result;
  int verify_waiting;
  char verify_message_id[64];
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
  };
  cb(&event, user_ctx);
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

static bb_tts_chunk_t* clone_tts_chunk(const bb_tts_chunk_t* src) {
  if (src == NULL || src->pcm_data == NULL || src->pcm_len == 0U) {
    return NULL;
  }
  bb_tts_chunk_t* copy = (bb_tts_chunk_t*)calloc(1, sizeof(bb_tts_chunk_t));
  if (copy == NULL) {
    return NULL;
  }
  copy->pcm_data = (uint8_t*)malloc(src->pcm_len);
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
  bb_tts_chunk_t* chunk = (bb_tts_chunk_t*)calloc(1, sizeof(bb_tts_chunk_t));
  if (chunk == NULL) {
    free(audio_b64);
    return NULL;
  }
  chunk->pcm_data = (uint8_t*)malloc(pcm_cap);
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
  bb_tts_chunk_t* chunk = (bb_tts_chunk_t*)calloc(1, sizeof(bb_tts_chunk_t));
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
        json_extract_string(brace, "sessionId", sid, sizeof(sid));
        json_extract_string(brace, "driver", drv, sizeof(drv));
        json_extract_string(brace, "type", ntype, sizeof(ntype));
        json_extract_string(brace, "preview", preview, sizeof(preview));
        if (sid[0] != '\0') {
          bb_notification_on_ws_event(sid, drv, ntype, preview);
        }
      }
    }
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

  if (strcmp(kind, "voice.reply.status") == 0) {
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
    (void)json_extract_string(msg, "name", name, sizeof(name));
    if (s_ws.finish_on_event != NULL && name[0] != '\0') {
      emit_finish_stream_event(s_ws.finish_on_event, s_ws.finish_user_ctx, BB_FINISH_STREAM_EVENT_TOOL_CALL, NULL,
                               name, NULL, 0);
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
    strncpy(accum->result->transcript, text, sizeof(accum->result->transcript) - 1);
    accum->result->transcript[sizeof(accum->result->transcript) - 1] = '\0';
    if (accum->on_event != NULL) {
      emit_finish_stream_event(accum->on_event, accum->user_ctx, BB_FINISH_STREAM_EVENT_ASR_FINAL, NULL, text, NULL, 0);
    }
    return;
  }

  if (strcmp(type, "reply.delta") == 0) {
    char text[sizeof(accum->result->reply_text)] = {0};
    (void)json_extract_string(line, "text", text, sizeof(text));
    strncpy(accum->result->reply_text, text, sizeof(accum->result->reply_text) - 1);
    accum->result->reply_text[sizeof(accum->result->reply_text) - 1] = '\0';
    if (accum->on_event != NULL) {
      emit_finish_stream_event(accum->on_event, accum->user_ctx, BB_FINISH_STREAM_EVENT_REPLY_DELTA, NULL, text, NULL,
                               0);
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
    (void)json_extract_string(line, "name", name, sizeof(name));
    if (accum->on_event != NULL && name[0] != '\0') {
      emit_finish_stream_event(accum->on_event, accum->user_ctx, BB_FINISH_STREAM_EVENT_TOOL_CALL, NULL, name, NULL, 0);
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

  memset(ctx, 0, sizeof(*ctx));
  snprintf(ctx->stream_id, sizeof(ctx->stream_id), "esp-%lld", (long long)bb_now_ms());
  ctx->next_seq = 1;
  ctx->ws_chunk_count = 0;

  if (bb_transport_is_cloud_saas()) {
    log_mem_snapshot("stream start before encoder");
    ctx->ws_encoder = bb_ogg_opus_encoder_create(BBCLAW_AUDIO_SAMPLE_RATE, BBCLAW_AUDIO_CHANNELS, BBCLAW_STREAM_CHUNK_MS);
    if (ctx->ws_encoder == NULL) {
      ESP_LOGE(TAG, "ws encoder create failed stream=%s", ctx->stream_id);
      log_mem_snapshot("stream start encoder failed");
      return ESP_ERR_NO_MEM;
    }
    log_mem_snapshot("stream start after encoder");
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
  ESP_RETURN_ON_ERROR(http_post_json("/v1/stream/start", body, &resp), TAG, "stream start request failed");

  if (resp.status_code < 200 || resp.status_code >= 300 || !body_contains_ok_true(resp.body)) {
    ESP_LOGE(TAG, "stream start failed status=%d body=%s", resp.status_code, resp.body);
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
  if (bb_transport_is_cloud_saas()) {
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
  if (bb_transport_is_cloud_saas()) {
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
    s_ws.finish_waiting = 1;
    snprintf(s_ws.finish_stream_id, sizeof(s_ws.finish_stream_id), "%s", ctx->stream_id);
    xSemaphoreGive(s_ws.lock);
    xEventGroupClearBits(s_ws.events, BB_WS_EVENT_DONE | BB_WS_EVENT_ERROR | BB_WS_EVENT_DISCONNECTED);

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

    EventBits_t bits = xEventGroupWaitBits(s_ws.events, BB_WS_EVENT_DONE | BB_WS_EVENT_ERROR | BB_WS_EVENT_DISCONNECTED,
                                           pdFALSE, pdFALSE, pdMS_TO_TICKS(BBCLAW_HTTP_STREAM_FINISH_TIMEOUT_MS));
    xSemaphoreTake(s_ws.lock, portMAX_DELAY);
    s_ws.finish_waiting = 0;
    if ((bits & BB_WS_EVENT_DONE) == 0U) {
      if (out_result->error_code[0] == '\0') {
        snprintf(out_result->error_code, sizeof(out_result->error_code), "%s",
                 (bits & BB_WS_EVENT_DISCONNECTED) != 0U ? "WS_DISCONNECTED" : "VOICE_SESSION_TIMEOUT");
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

esp_err_t bb_adapter_tts_synthesize_pcm16(const char* text, bb_tts_audio_t* out_audio) {
  if (text == NULL || out_audio == NULL) {
    return ESP_ERR_INVALID_ARG;
  }
  memset(out_audio, 0, sizeof(*out_audio));

  char* escaped = json_escape_alloc(text);
  if (escaped == NULL) {
    return ESP_ERR_NO_MEM;
  }

  size_t body_cap = strlen(escaped) + 256;
  char* body = (char*)malloc(body_cap);
  if (body == NULL) {
    free(escaped);
    return ESP_ERR_NO_MEM;
  }
  snprintf(body, body_cap,
           "{\"text\":\"%s\",\"codec\":\"pcm16\",\"sampleRate\":%d,\"channels\":%d,\"deviceId\":\"%s\"}",
           escaped, BBCLAW_TTS_SAMPLE_RATE, BBCLAW_TTS_CHANNELS, BBCLAW_DEVICE_ID);
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
