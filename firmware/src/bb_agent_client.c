#include "bb_agent_client.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>

#include "bb_config.h"
#include "cJSON.h"
#include "esp_check.h"
#include "esp_crt_bundle.h"
#include "esp_err.h"
#include "esp_heap_caps.h"
#include "esp_http_client.h"
#include "esp_log.h"

static const char* TAG = "bb_agent";

/*
 * Mirror bb_adapter_client.c::active_base_url(): cloud_saas profile uses
 * the cloud URL, otherwise the local adapter. Phase 4.8 wired the cloud-side
 * /v1/agent/... reverse proxy through to the paired home adapter; the cloud
 * picks the target device by deviceId query param (see ADR-004). Local mode
 * needs no deviceId — the home adapter only serves one device.
 */
static int agent_is_cloud_mode(void) {
  return strcasecmp(BBCLAW_TRANSPORT_PROFILE, "cloud_saas") == 0 ? 1 : 0;
}

static const char* agent_base_url(void) {
  return agent_is_cloud_mode() ? BBCLAW_CLOUD_BASE_URL : BBCLAW_ADAPTER_BASE_URL;
}

/* Build a fully-qualified agent endpoint URL. In cloud_saas mode appends
 * "?deviceId=<BBCLAW_DEVICE_ID>" so the cloud proxy can route to the right
 * home adapter; in local mode the path is left bare. Path must start with
 * '/' (we don't add the slash). */
static void agent_build_url(char* out, size_t out_len, const char* path) {
  if (out == NULL || out_len == 0) return;
  const char* base = agent_base_url();
  if (agent_is_cloud_mode()) {
    const char* sep = strchr(path, '?') != NULL ? "&" : "?";
    snprintf(out, out_len, "%s%s%sdeviceId=%s", base, path, sep, BBCLAW_DEVICE_ID);
  } else {
    snprintf(out, out_len, "%s%s", base, path);
  }
}

/* ── 通用 HTTP 配置：复制自 bb_adapter_client.c 的 bb_http_cfg_init 风格 ── */
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

/* ── 动态 body accumulator（一次性 GET 用） ── */
typedef struct {
  char* buf;
  size_t len;
  size_t cap;
} bb_http_dyn_accum_t;

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
    /* Force PSRAM placement: agent stream / history bodies can run into
     * hundreds of KB. Internal RAM is ~135 KB total and shared with WiFi /
     * TCP / FreeRTOS — putting these here previously caused xTaskCreate
     * failures after a few turns (see "send: xTaskCreate failed" 2026-05-04). */
    char* new_buf = (char*)heap_caps_realloc(accum->buf, new_cap,
                                             MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
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

/* ── NDJSON 流 accumulator（POST /v1/agent/message 用） ── */
typedef struct {
  bb_agent_stream_cb_t on_event;
  void* user_ctx;
  char* buf;
  size_t len;
  size_t cap;
  int saw_turn_end;
  int saw_error;
  char last_error[64];
} bb_agent_stream_accum_t;

/* 把一帧解析好的事件丢给回调；所有指针在回调返回前有效（cJSON 对象未释放）。*/
static void emit_event(bb_agent_stream_accum_t* accum, const bb_agent_stream_event_t* evt) {
  if (accum == NULL || accum->on_event == NULL || evt == NULL) {
    return;
  }
  accum->on_event(evt, accum->user_ctx);
}

/* 解析一行 NDJSON。type 决定事件类型；其余字段按需取。
 * cJSON 对象在函数末尾释放，所有 const char* 字段都指向其内部，调用回调期间保持有效。 */
static void parse_agent_stream_line(const char* line, bb_agent_stream_accum_t* accum) {
  if (line == NULL || accum == NULL || line[0] == '\0') {
    return;
  }
  cJSON* root = cJSON_Parse(line);
  if (root == NULL) {
    ESP_LOGW(TAG, "ndjson parse failed: %.120s", line);
    return;
  }

  const cJSON* type_node = cJSON_GetObjectItemCaseSensitive(root, "type");
  if (!cJSON_IsString(type_node) || type_node->valuestring == NULL) {
    cJSON_Delete(root);
    return;
  }
  const char* type = type_node->valuestring;

  bb_agent_stream_event_t evt = {0};

  if (strcmp(type, "session") == 0) {
    evt.type = BB_AGENT_EVENT_SESSION;
    const cJSON* sid = cJSON_GetObjectItemCaseSensitive(root, "sessionId");
    const cJSON* is_new = cJSON_GetObjectItemCaseSensitive(root, "isNew");
    const cJSON* drv = cJSON_GetObjectItemCaseSensitive(root, "driver");
    if (cJSON_IsString(sid)) {
      evt.session_id = sid->valuestring;
    }
    if (cJSON_IsBool(is_new)) {
      evt.is_new = cJSON_IsTrue(is_new) ? 1 : 0;
    } else if (cJSON_IsNumber(is_new)) {
      evt.is_new = is_new->valueint != 0 ? 1 : 0;
    }
    if (cJSON_IsString(drv)) {
      evt.driver = drv->valuestring;
    }
    ESP_LOGI(TAG, "stream session sid=%s isNew=%d driver=%s",
             evt.session_id != NULL ? evt.session_id : "(null)", evt.is_new,
             evt.driver != NULL ? evt.driver : "(null)");
    emit_event(accum, &evt);
  } else if (strcmp(type, "text") == 0) {
    evt.type = BB_AGENT_EVENT_TEXT;
    const cJSON* text = cJSON_GetObjectItemCaseSensitive(root, "text");
    if (cJSON_IsString(text)) {
      evt.text = text->valuestring;
    }
    emit_event(accum, &evt);
  } else if (strcmp(type, "tool_call") == 0) {
    evt.type = BB_AGENT_EVENT_TOOL_CALL;
    const cJSON* tool = cJSON_GetObjectItemCaseSensitive(root, "tool");
    const cJSON* hint = cJSON_GetObjectItemCaseSensitive(root, "hint");
    const cJSON* drv = cJSON_GetObjectItemCaseSensitive(root, "driver");
    if (cJSON_IsString(tool)) {
      evt.tool = tool->valuestring;
    }
    if (cJSON_IsString(hint)) {
      evt.hint = hint->valuestring;
    }
    if (cJSON_IsString(drv)) {
      evt.driver = drv->valuestring;
    }
    ESP_LOGI(TAG, "stream tool_call tool=%s", evt.tool != NULL ? evt.tool : "(null)");
    emit_event(accum, &evt);
  } else if (strcmp(type, "tokens") == 0) {
    evt.type = BB_AGENT_EVENT_TOKENS;
    const cJSON* in_node = cJSON_GetObjectItemCaseSensitive(root, "in");
    const cJSON* out_node = cJSON_GetObjectItemCaseSensitive(root, "out");
    if (cJSON_IsNumber(in_node)) {
      evt.tokens_in = in_node->valueint;
    }
    if (cJSON_IsNumber(out_node)) {
      evt.tokens_out = out_node->valueint;
    }
    emit_event(accum, &evt);
  } else if (strcmp(type, "turn_end") == 0) {
    evt.type = BB_AGENT_EVENT_TURN_END;
    accum->saw_turn_end = 1;
    ESP_LOGI(TAG, "stream turn_end");
    emit_event(accum, &evt);
  } else if (strcmp(type, "error") == 0) {
    evt.type = BB_AGENT_EVENT_ERROR;
    /* adapter 可能用 error/text/detail 多个字段。优先 error，fallback text/detail。*/
    const cJSON* err_node = cJSON_GetObjectItemCaseSensitive(root, "error");
    const cJSON* text_node = cJSON_GetObjectItemCaseSensitive(root, "text");
    const cJSON* detail_node = cJSON_GetObjectItemCaseSensitive(root, "detail");
    if (cJSON_IsString(err_node)) {
      evt.error_code = err_node->valuestring;
      strncpy(accum->last_error, err_node->valuestring, sizeof(accum->last_error) - 1);
      accum->last_error[sizeof(accum->last_error) - 1] = '\0';
    }
    if (cJSON_IsString(text_node)) {
      evt.text = text_node->valuestring;
    } else if (cJSON_IsString(detail_node)) {
      evt.text = detail_node->valuestring;
    }
    accum->saw_error = 1;
    ESP_LOGW(TAG, "stream error code=%s text=%s",
             evt.error_code != NULL ? evt.error_code : "(null)",
             evt.text != NULL ? evt.text : "(null)");
    emit_event(accum, &evt);
  } else {
    ESP_LOGD(TAG, "ignore unknown type=%s", type);
  }

  cJSON_Delete(root);
}

static esp_err_t http_event_handler_agent_stream(esp_http_client_event_t* evt) {
  bb_agent_stream_accum_t* accum = (bb_agent_stream_accum_t*)evt->user_data;
  if (accum == NULL) {
    return ESP_OK;
  }
  if (evt->event_id == HTTP_EVENT_ON_HEADER) {
    ESP_LOGD(TAG, "agent stream hdr: %s: %s", evt->header_key, evt->header_value);
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
    /* Force PSRAM placement: agent stream / history bodies can run into
     * hundreds of KB. Internal RAM is ~135 KB total and shared with WiFi /
     * TCP / FreeRTOS — putting these here previously caused xTaskCreate
     * failures after a few turns (see "send: xTaskCreate failed" 2026-05-04). */
    char* new_buf = (char*)heap_caps_realloc(accum->buf, new_cap,
                                             MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
    if (new_buf == NULL) {
      return ESP_ERR_NO_MEM;
    }
    accum->buf = new_buf;
    accum->cap = new_cap;
  }

  memcpy(accum->buf + accum->len, evt->data, (size_t)evt->data_len);
  accum->len += (size_t)evt->data_len;
  accum->buf[accum->len] = '\0';

  /* 按 \n 切行解析；剩余不完整的留在 buf 里等下次 ON_DATA。*/
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
      parse_agent_stream_line(accum->buf, accum);
    }
    size_t consumed = (size_t)(nl - accum->buf) + 1;
    memmove(accum->buf, accum->buf + consumed, accum->len - consumed);
    accum->len -= consumed;
    accum->buf[accum->len] = '\0';
  }
  return ESP_OK;
}

/* ── public: list drivers ── */

esp_err_t bb_agent_list_drivers(bb_agent_driver_info_t* out_list, int cap, int* out_count) {
  if (out_count != NULL) {
    *out_count = 0;
  }

  char url[256] = {0};
  agent_build_url(url, sizeof(url), "/v1/agent/drivers");

  bb_http_dyn_accum_t accum = {0};
  esp_http_client_config_t cfg;
  bb_http_cfg_init(&cfg, url, BBCLAW_HTTP_TIMEOUT_MS, HTTP_METHOD_GET, http_event_handler_dyn, &accum);

  esp_http_client_handle_t client = esp_http_client_init(&cfg);
  if (client == NULL) {
    return ESP_ERR_NO_MEM;
  }

  esp_err_t err = esp_http_client_perform(client);
  int status = (err == ESP_OK) ? esp_http_client_get_status_code(client) : 0;
  esp_http_client_cleanup(client);

  if (err != ESP_OK) {
    free(accum.buf);
    ESP_LOGE(TAG, "list_drivers transport err=%s", esp_err_to_name(err));
    return err;
  }
  if (status < 200 || status >= 300 || accum.buf == NULL) {
    ESP_LOGE(TAG, "list_drivers http status=%d body=%.120s", status, accum.buf != NULL ? accum.buf : "(null)");
    free(accum.buf);
    return ESP_FAIL;
  }

  cJSON* root = cJSON_Parse(accum.buf);
  free(accum.buf);
  accum.buf = NULL;
  if (root == NULL) {
    ESP_LOGE(TAG, "list_drivers parse failed");
    return ESP_FAIL;
  }

  const cJSON* data = cJSON_GetObjectItemCaseSensitive(root, "data");
  const cJSON* drivers = cJSON_GetObjectItemCaseSensitive(data, "drivers");
  if (!cJSON_IsArray(drivers)) {
    ESP_LOGE(TAG, "list_drivers missing data.drivers[]");
    cJSON_Delete(root);
    return ESP_FAIL;
  }

  int total = cJSON_GetArraySize(drivers);
  if (out_count != NULL) {
    *out_count = total;
  }
  if (out_list != NULL && cap > 0) {
    int n = total < cap ? total : cap;
    for (int i = 0; i < n; i++) {
      const cJSON* item = cJSON_GetArrayItem(drivers, i);
      if (item == NULL) {
        continue;
      }
      bb_agent_driver_info_t* slot = &out_list[i];
      memset(slot, 0, sizeof(*slot));
      const cJSON* name = cJSON_GetObjectItemCaseSensitive(item, "name");
      if (cJSON_IsString(name) && name->valuestring != NULL) {
        strncpy(slot->name, name->valuestring, sizeof(slot->name) - 1);
        slot->name[sizeof(slot->name) - 1] = '\0';
      }
      const cJSON* caps = cJSON_GetObjectItemCaseSensitive(item, "capabilities");
      if (cJSON_IsObject(caps)) {
        const cJSON* ta = cJSON_GetObjectItemCaseSensitive(caps, "toolApproval");
        const cJSON* rs = cJSON_GetObjectItemCaseSensitive(caps, "resume");
        const cJSON* st = cJSON_GetObjectItemCaseSensitive(caps, "streaming");
        slot->tool_approval = cJSON_IsTrue(ta) ? 1 : 0;
        slot->resume = cJSON_IsTrue(rs) ? 1 : 0;
        slot->streaming = cJSON_IsTrue(st) ? 1 : 0;
      }
    }
  }

  ESP_LOGI(TAG, "list_drivers ok total=%d", total);
  cJSON_Delete(root);
  return ESP_OK;
}

/* ── public: list sessions ── */

esp_err_t bb_agent_list_sessions(const char* driver_name, bb_agent_session_info_t* out_list, int cap, int* out_count) {
  if (driver_name == NULL || driver_name[0] == '\0') {
    return ESP_ERR_INVALID_ARG;
  }
  if (out_count != NULL) {
    *out_count = 0;
  }

  char url[320] = {0};
  char path[128] = {0};
  snprintf(path, sizeof(path), "/v1/agent/sessions?driver=%s&limit=6", driver_name);
  agent_build_url(url, sizeof(url), path);

  bb_http_dyn_accum_t accum = {0};
  esp_http_client_config_t cfg;
  bb_http_cfg_init(&cfg, url, BBCLAW_HTTP_TIMEOUT_MS, HTTP_METHOD_GET, http_event_handler_dyn, &accum);

  esp_http_client_handle_t client = esp_http_client_init(&cfg);
  if (client == NULL) {
    return ESP_ERR_NO_MEM;
  }

  esp_err_t err = esp_http_client_perform(client);
  int status = (err == ESP_OK) ? esp_http_client_get_status_code(client) : 0;
  esp_http_client_cleanup(client);

  if (err != ESP_OK) {
    free(accum.buf);
    ESP_LOGE(TAG, "list_sessions transport err=%s", esp_err_to_name(err));
    return err;
  }
  if (status < 200 || status >= 300 || accum.buf == NULL) {
    ESP_LOGE(TAG, "list_sessions http status=%d body=%.120s", status, accum.buf != NULL ? accum.buf : "(null)");
    free(accum.buf);
    return ESP_FAIL;
  }

  cJSON* root = cJSON_Parse(accum.buf);
  free(accum.buf);
  accum.buf = NULL;
  if (root == NULL) {
    ESP_LOGE(TAG, "list_sessions parse failed");
    return ESP_FAIL;
  }

  const cJSON* data = cJSON_GetObjectItemCaseSensitive(root, "data");
  const cJSON* sessions = cJSON_GetObjectItemCaseSensitive(data, "sessions");
  if (!cJSON_IsArray(sessions)) {
    ESP_LOGE(TAG, "list_sessions missing data.sessions[]");
    cJSON_Delete(root);
    return ESP_FAIL;
  }

  int total = cJSON_GetArraySize(sessions);
  if (out_count != NULL) {
    *out_count = total;
  }
  if (out_list != NULL && cap > 0) {
    int n = total < cap ? total : cap;
    for (int i = 0; i < n; i++) {
      const cJSON* item = cJSON_GetArrayItem(sessions, i);
      if (item == NULL) {
        continue;
      }
      bb_agent_session_info_t* slot = &out_list[i];
      memset(slot, 0, sizeof(*slot));

      const cJSON* id = cJSON_GetObjectItemCaseSensitive(item, "id");
      if (cJSON_IsString(id) && id->valuestring != NULL) {
        strncpy(slot->id, id->valuestring, sizeof(slot->id) - 1);
        slot->id[sizeof(slot->id) - 1] = '\0';
      }

      const cJSON* preview = cJSON_GetObjectItemCaseSensitive(item, "preview");
      if (cJSON_IsString(preview) && preview->valuestring != NULL) {
        strncpy(slot->preview, preview->valuestring, sizeof(slot->preview) - 1);
        slot->preview[sizeof(slot->preview) - 1] = '\0';
      }

      const cJSON* msg_count = cJSON_GetObjectItemCaseSensitive(item, "messageCount");
      if (cJSON_IsNumber(msg_count)) {
        slot->message_count = msg_count->valueint;
      }

      const cJSON* last_used = cJSON_GetObjectItemCaseSensitive(item, "lastUsed");
      if (cJSON_IsNumber(last_used)) {
        slot->last_used_ms = (int64_t)last_used->valuedouble;
      }
    }
  }

  ESP_LOGI(TAG, "list_sessions ok driver=%s total=%d", driver_name, total);
  cJSON_Delete(root);
  return ESP_OK;
}

/* ── public: load messages (history pagination) ── */

void bb_agent_messages_free(bb_agent_message_t* list, int count) {
  if (list == NULL) return;
  for (int i = 0; i < count; i++) {
    free(list[i].content);
    list[i].content = NULL;
  }
  free(list);
}

esp_err_t bb_agent_load_messages(const char* session_id,
                                 const char* driver_name,
                                 int before,
                                 int limit,
                                 bb_agent_message_t** out_list,
                                 int* out_count,
                                 int* out_total,
                                 int* out_has_more) {
  if (out_list != NULL) *out_list = NULL;
  if (out_count != NULL) *out_count = 0;
  if (out_total != NULL) *out_total = 0;
  if (out_has_more != NULL) *out_has_more = 0;

  if (session_id == NULL || session_id[0] == '\0' ||
      driver_name == NULL || driver_name[0] == '\0') {
    return ESP_ERR_INVALID_ARG;
  }

  /* URL: /v1/agent/sessions/{id}/messages?driver=X&before=Y&limit=Z */
  char path[224] = {0};
  if (limit > 0) {
    snprintf(path, sizeof(path),
             "/v1/agent/sessions/%s/messages?driver=%s&before=%d&limit=%d",
             session_id, driver_name, before, limit);
  } else {
    snprintf(path, sizeof(path),
             "/v1/agent/sessions/%s/messages?driver=%s&before=%d",
             session_id, driver_name, before);
  }
  char url[384] = {0};
  agent_build_url(url, sizeof(url), path);

  bb_http_dyn_accum_t accum = {0};
  esp_http_client_config_t cfg;
  bb_http_cfg_init(&cfg, url, BBCLAW_HTTP_TIMEOUT_MS, HTTP_METHOD_GET, http_event_handler_dyn, &accum);

  esp_http_client_handle_t client = esp_http_client_init(&cfg);
  if (client == NULL) {
    return ESP_ERR_NO_MEM;
  }

  esp_err_t err = esp_http_client_perform(client);
  int status = (err == ESP_OK) ? esp_http_client_get_status_code(client) : 0;
  esp_http_client_cleanup(client);

  if (err != ESP_OK) {
    free(accum.buf);
    ESP_LOGE(TAG, "load_messages transport err=%s", esp_err_to_name(err));
    return err;
  }
  if (status < 200 || status >= 300 || accum.buf == NULL) {
    ESP_LOGE(TAG, "load_messages http status=%d body=%.120s", status, accum.buf != NULL ? accum.buf : "(null)");
    free(accum.buf);
    return ESP_FAIL;
  }

  cJSON* root = cJSON_Parse(accum.buf);
  free(accum.buf);
  accum.buf = NULL;
  if (root == NULL) {
    ESP_LOGE(TAG, "load_messages parse failed");
    return ESP_FAIL;
  }

  /* Adapter degrades unsupported drivers with HTTP 200 + ok=false. Treat as soft error. */
  const cJSON* ok_node = cJSON_GetObjectItemCaseSensitive(root, "ok");
  if (cJSON_IsBool(ok_node) && !cJSON_IsTrue(ok_node)) {
    const cJSON* err_code = cJSON_GetObjectItemCaseSensitive(root, "error");
    ESP_LOGW(TAG, "load_messages refused: %s",
             cJSON_IsString(err_code) ? err_code->valuestring : "(unknown)");
    cJSON_Delete(root);
    return ESP_ERR_NOT_SUPPORTED;
  }

  const cJSON* data = cJSON_GetObjectItemCaseSensitive(root, "data");
  const cJSON* msgs = cJSON_GetObjectItemCaseSensitive(data, "messages");
  if (!cJSON_IsArray(msgs)) {
    ESP_LOGE(TAG, "load_messages missing data.messages[]");
    cJSON_Delete(root);
    return ESP_FAIL;
  }

  if (out_total != NULL) {
    const cJSON* tot = cJSON_GetObjectItemCaseSensitive(data, "total");
    if (cJSON_IsNumber(tot)) {
      *out_total = tot->valueint;
    }
  }
  if (out_has_more != NULL) {
    const cJSON* hm = cJSON_GetObjectItemCaseSensitive(data, "hasMore");
    if (cJSON_IsBool(hm)) {
      *out_has_more = cJSON_IsTrue(hm) ? 1 : 0;
    }
  }

  int n = cJSON_GetArraySize(msgs);
  if (n <= 0) {
    ESP_LOGI(TAG, "load_messages empty page sid=%s", session_id);
    cJSON_Delete(root);
    return ESP_OK;
  }

  /* History page can be 50 × ~500B = 25 KB of strings — strdup goes to
   * internal heap by default, which we can't afford. Use PSRAM for both
   * the array and per-message content buffers. bb_agent_messages_free
   * uses free(), which works on heap_caps_*-allocated blocks too. */
  bb_agent_message_t* arr = (bb_agent_message_t*)heap_caps_calloc(
      (size_t)n, sizeof(*arr), MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
  if (arr == NULL) {
    cJSON_Delete(root);
    return ESP_ERR_NO_MEM;
  }

  int written = 0;
  for (int i = 0; i < n; i++) {
    const cJSON* item = cJSON_GetArrayItem(msgs, i);
    if (item == NULL) continue;
    const cJSON* role = cJSON_GetObjectItemCaseSensitive(item, "role");
    const cJSON* content = cJSON_GetObjectItemCaseSensitive(item, "content");
    const cJSON* seq = cJSON_GetObjectItemCaseSensitive(item, "seq");
    if (!cJSON_IsString(role) || !cJSON_IsString(content)) {
      continue;
    }
    bb_agent_message_t* slot = &arr[written];
    strncpy(slot->role, role->valuestring, sizeof(slot->role) - 1);
    slot->role[sizeof(slot->role) - 1] = '\0';
    size_t clen = strlen(content->valuestring);
    slot->content = (char*)heap_caps_malloc(clen + 1, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
    if (slot->content == NULL) {
      /* Out of memory mid-page: free what we have and bail. */
      bb_agent_messages_free(arr, written);
      cJSON_Delete(root);
      return ESP_ERR_NO_MEM;
    }
    memcpy(slot->content, content->valuestring, clen + 1);
    slot->seq = cJSON_IsNumber(seq) ? seq->valueint : i;
    written++;
  }

  cJSON_Delete(root);

  if (written == 0) {
    free(arr);
    return ESP_OK;
  }

  *out_list = arr;
  if (out_count != NULL) *out_count = written;
  ESP_LOGI(TAG, "load_messages ok sid=%s before=%d limit=%d returned=%d total=%d hasMore=%d",
           session_id, before, limit, written,
           out_total != NULL ? *out_total : -1,
           out_has_more != NULL ? *out_has_more : -1);
  return ESP_OK;
}

/* ── public: send message ── */

esp_err_t bb_agent_send_message(const char* text, const char* session_id, const char* driver_name,
                                bb_agent_stream_cb_t on_event, void* user_ctx) {
  if (text == NULL) {
    return ESP_ERR_INVALID_ARG;
  }

  /* 用 cJSON 拼请求体，字符转义不用自己写。*/
  cJSON* req = cJSON_CreateObject();
  if (req == NULL) {
    return ESP_ERR_NO_MEM;
  }
  cJSON_AddStringToObject(req, "text", text);
  if (session_id != NULL && session_id[0] != '\0') {
    cJSON_AddStringToObject(req, "sessionId", session_id);
  }
  if (driver_name != NULL && driver_name[0] != '\0') {
    cJSON_AddStringToObject(req, "driver", driver_name);
  }
  char* body = cJSON_PrintUnformatted(req);
  cJSON_Delete(req);
  if (body == NULL) {
    return ESP_ERR_NO_MEM;
  }

  char url[256] = {0};
  agent_build_url(url, sizeof(url), "/v1/agent/message");

  bb_agent_stream_accum_t accum = {
      .on_event = on_event,
      .user_ctx = user_ctx,
  };

  esp_http_client_config_t cfg;
  /* finish_stream 走 90s 超时；agent 这里也按 long-poll 配置同样的窗口。 */
  bb_http_cfg_init(&cfg, url, BBCLAW_HTTP_STREAM_FINISH_TIMEOUT_MS, HTTP_METHOD_POST,
                   http_event_handler_agent_stream, &accum);

  esp_http_client_handle_t client = esp_http_client_init(&cfg);
  if (client == NULL) {
    free(body);
    return ESP_ERR_NO_MEM;
  }

  esp_http_client_set_header(client, "Content-Type", "application/json");
  esp_http_client_set_header(client, "Accept", "application/x-ndjson");
  esp_http_client_set_post_field(client, body, (int)strlen(body));

  ESP_LOGI(TAG, "send_message url=%s len=%u sid=%s driver=%s", url, (unsigned)strlen(body),
           (session_id != NULL && session_id[0] != '\0') ? session_id : "(new)",
           (driver_name != NULL && driver_name[0] != '\0') ? driver_name : "(default)");

  esp_err_t err = esp_http_client_perform(client);
  int status = (err == ESP_OK) ? esp_http_client_get_status_code(client) : 0;
  esp_http_client_cleanup(client);
  free(body);

  /* 末尾可能没有 \n —— 把残留 buf 也跑一遍 parse。*/
  if (err == ESP_OK && accum.buf != NULL && accum.len > 0) {
    while (accum.len > 0 && (accum.buf[accum.len - 1] == '\n' || accum.buf[accum.len - 1] == '\r')) {
      accum.len--;
    }
    accum.buf[accum.len] = '\0';
    if (accum.len > 0) {
      parse_agent_stream_line(accum.buf, &accum);
    }
  }

  ESP_LOGI(TAG, "send_message done err=%s status=%d turn_end=%d error=%d", esp_err_to_name(err), status,
           accum.saw_turn_end, accum.saw_error);

  free(accum.buf);
  accum.buf = NULL;

  if (err != ESP_OK) {
    return err;
  }
  if (status < 200 || status >= 300) {
    ESP_LOGE(TAG, "send_message http status=%d", status);
    return ESP_FAIL;
  }
  if (accum.saw_error || !accum.saw_turn_end) {
    return ESP_FAIL;
  }
  return ESP_OK;
}
