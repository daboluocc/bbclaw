#include "bb_cloud_client.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "bb_config.h"
#include "esp_app_desc.h"
#include "esp_check.h"
#include "esp_crt_bundle.h"
#include "esp_http_client.h"
#include "esp_log.h"

static const char* TAG = "bb_cloud";

extern const uint8_t isrg_root_x1_pem_start[] asm("_binary_isrg_root_x1_pem_start");
extern const uint8_t isrg_root_x1_pem_end[]   asm("_binary_isrg_root_x1_pem_end");

typedef struct {
  int status_code;
  char body[8192];
} bb_http_resp_t;

typedef struct {
  bb_http_resp_t* resp;
  size_t offset;
} bb_http_accum_t;

static esp_err_t http_event_handler(esp_http_client_event_t* evt) {
  bb_http_accum_t* accum = (bb_http_accum_t*)evt->user_data;
  if (accum == NULL || accum->resp == NULL) {
    return ESP_OK;
  }

  if (evt->event_id == HTTP_EVENT_ON_DATA && evt->data != NULL && evt->data_len > 0) {
    size_t cap = sizeof(accum->resp->body) - 1U; /* pairing JSON: data + config; keep headroom */
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

static esp_err_t http_perform_json(const char* method, const char* path, const char* payload, bb_http_resp_t* out_resp) {
  if (method == NULL || path == NULL || out_resp == NULL) {
    return ESP_ERR_INVALID_ARG;
  }

  char url[256] = {0};
  snprintf(url, sizeof(url), "%s%s", BBCLAW_CLOUD_BASE_URL, path);
  memset(out_resp, 0, sizeof(*out_resp));
  bb_http_accum_t accum = {.resp = out_resp, .offset = 0};

  esp_http_client_method_t http_method = HTTP_METHOD_GET;
  if (strcmp(method, "POST") == 0) {
    http_method = HTTP_METHOD_POST;
  }

  esp_http_client_config_t cfg;
  memset(&cfg, 0, sizeof(cfg));
  cfg.url = url;
  cfg.timeout_ms = BBCLAW_HTTP_TIMEOUT_MS;
  cfg.method = http_method;
  cfg.transport_type = HTTP_TRANSPORT_OVER_TCP;
  cfg.event_handler = http_event_handler;
  cfg.user_data = &accum;
  if (strncasecmp(url, "https", 5) == 0) {
    cfg.crt_bundle_attach = esp_crt_bundle_attach;
  }

  esp_http_client_handle_t client = esp_http_client_init(&cfg);
  if (client == NULL) {
    return ESP_ERR_NO_MEM;
  }

  if (payload != NULL) {
    esp_http_client_set_header(client, "Content-Type", "application/json");
    esp_http_client_set_post_field(client, payload, (int)strlen(payload));
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

static int body_contains_ok_true(const char* body) {
  if (body == NULL) {
    return 0;
  }
  if (strstr(body, "\"ok\":true") != NULL) {
    return 1;
  }
  /* Cloud 可能返回 "ok": true（冒号后带空格） */
  return strstr(body, "\"ok\": true") != NULL;
}

static int json_object_extract_bool(const char* body, const char* object_key, const char* value_key, int fallback) {
  if (body == NULL || object_key == NULL || value_key == NULL) {
    return fallback;
  }

  /* 兼容 "key":{ 和 "key": { */
  char p1[64] = {0};
  char p2[64] = {0};
  snprintf(p1, sizeof(p1), "\"%s\":{", object_key);
  snprintf(p2, sizeof(p2), "\"%s\": {", object_key);
  const char* scope = strstr(body, p1);
  if (scope == NULL) {
    scope = strstr(body, p2);
  }
  if (scope == NULL) {
    return fallback;
  }
  const char* scope_end = strchr(scope, '}');
  if (scope_end == NULL) {
    return fallback;
  }

  char t1[64], t2[64], f1[64], f2[64];
  snprintf(t1, sizeof(t1), "\"%s\":true", value_key);
  snprintf(t2, sizeof(t2), "\"%s\": true", value_key);
  snprintf(f1, sizeof(f1), "\"%s\":false", value_key);
  snprintf(f2, sizeof(f2), "\"%s\": false", value_key);
  const char* hit;
  hit = strstr(scope, t1);
  if (hit == NULL) { hit = strstr(scope, t2); }
  if (hit != NULL && hit < scope_end) {
    return 1;
  }
  hit = strstr(scope, f1);
  if (hit == NULL) { hit = strstr(scope, f2); }
  if (hit != NULL && hit < scope_end) {
    return 0;
  }
  return fallback;
}


static int json_object_extract_int(const char* body, const char* object_key, const char* value_key, int fallback) {
  if (body == NULL || object_key == NULL || value_key == NULL) {
    return fallback;
  }
  char p1[64], p2[64];
  snprintf(p1, sizeof(p1), "\"%s\":{", object_key);
  snprintf(p2, sizeof(p2), "\"%s\": {", object_key);
  const char* scope = strstr(body, p1);
  if (scope == NULL) { scope = strstr(body, p2); }
  if (scope == NULL) { return fallback; }
  const char* scope_end = strchr(scope, '}');
  if (scope_end == NULL) { return fallback; }
  char k1[64], k2[64];
  snprintf(k1, sizeof(k1), "\"%s\":", value_key);
  snprintf(k2, sizeof(k2), "\"%s\": ", value_key);
  const char* hit = strstr(scope, k1);
  if (hit == NULL) { hit = strstr(scope, k2); }
  if (hit == NULL || hit >= scope_end) { return fallback; }
  const char* vp = hit + strlen(hit == strstr(scope, k1) ? k1 : k2);
  while (*vp == ' ') { vp++; }
  return atoi(vp);
}
static void copy_lower_ascii(char* out, size_t out_len, const char* in) {
  if (out == NULL || out_len == 0U) {
    return;
  }
  size_t i = 0;
  if (in != NULL) {
    for (; in[i] != '\0' && i + 1 < out_len; ++i) {
      char c = in[i];
      if (c >= 'A' && c <= 'Z') {
        c = (char)(c - 'A' + 'a');
      }
      out[i] = c;
    }
  }
  out[i] = '\0';
}

static int json_extract_string(const char* body, const char* key, char* out, size_t out_len) {
  if (body == NULL || key == NULL || out == NULL || out_len == 0U) {
    return 0;
  }

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

  size_t j = 0;
  for (const char* p = start; *p != '\0' && j + 1 < out_len; ++p) {
    if (*p == '"') {
      break;
    }
    if (*p == '\\' && p[1] != '\0') {
      ++p;
    }
    out[j++] = *p;
  }
  out[j] = '\0';
  return 1;
}

/** Copy the JSON object value of the top-level `"data"` key into `slice` (brace-balanced). Returns 1 on success. */
static int json_copy_data_object(const char* body, char* slice, size_t slice_len) {
  if (body == NULL || slice == NULL || slice_len < 4U) {
    return 0;
  }
  const char* key = strstr(body, "\"data\"");
  if (key == NULL) {
    return 0;
  }
  const char* p = strchr(key, '{');
  if (p == NULL) {
    return 0;
  }
  const char* start = p;
  int depth = 0;
  int in_str = 0;
  for (; *p != '\0'; ++p) {
    if (in_str) {
      if (*p == '\\' && p[1] != '\0') {
        ++p;
        continue;
      }
      if (*p == '"') {
        in_str = 0;
      }
      continue;
    }
    if (*p == '"') {
      in_str = 1;
      continue;
    }
    if (*p == '{') {
      depth++;
    } else if (*p == '}') {
      depth--;
      if (depth == 0) {
        size_t n = (size_t)(p - start + 1);
        if (n >= slice_len) {
          return 0;
        }
        memcpy(slice, start, n);
        slice[n] = '\0';
        return 1;
      }
    }
  }
  return 0;
}

/** Extract pairing "code" as a JSON string or a bare integer (digits only). */
static int json_extract_pairing_code(const char* body, char* out, size_t out_len) {
  if (body == NULL || out == NULL || out_len == 0U) {
    return 0;
  }
  if (json_extract_string(body, "code", out, out_len)) {
    return 1;
  }
  const char* key = strstr(body, "\"code\"");
  if (key == NULL) {
    out[0] = '\0';
    return 0;
  }
  const char* colon = strchr(key, ':');
  if (colon == NULL) {
    out[0] = '\0';
    return 0;
  }
  const char* p = colon + 1;
  while (*p == ' ' || *p == '\t') {
    p++;
  }
  if (*p == '"') {
    return json_extract_string(body, "code", out, out_len);
  }
  size_t j = 0;
  while (*p >= '0' && *p <= '9' && j + 1 < out_len) {
    out[j++] = *p++;
  }
  out[j] = '\0';
  return j > 0;
}

static void resolve_pairing_error_detail(int http_status, const char* body, char* out, size_t out_len) {
  if (out == NULL || out_len == 0U) {
    return;
  }
  out[0] = '\0';

  if (http_status == 401 || http_status == 403) {
    snprintf(out, out_len, "%s", "unauthorized");
    return;
  }

  char raw_error[32] = {0};
  if (json_extract_string(body, "error", raw_error, sizeof(raw_error)) && raw_error[0] != '\0') {
    copy_lower_ascii(out, out_len, raw_error);
    return;
  }

  if (http_status >= 500) {
    snprintf(out, out_len, "%s", "cloud_unavailable");
    return;
  }

  snprintf(out, out_len, "%s", "request_failed");
}

const char* bb_cloud_pair_status_name(bb_cloud_pair_status_t status) {
  switch (status) {
    case BB_CLOUD_PAIR_STATUS_PENDING:
      return "pending";
    case BB_CLOUD_PAIR_STATUS_APPROVED:
      return "approved";
    case BB_CLOUD_PAIR_STATUS_BINDING_REQUIRED:
      return "binding_required";
    default:
      return "unknown";
  }
}

esp_err_t bb_cloud_healthz(bb_cloud_health_t* out_health) {
  if (out_health == NULL) {
    return ESP_ERR_INVALID_ARG;
  }
  memset(out_health, 0, sizeof(*out_health));

  bb_http_resp_t resp = {0};
  ESP_RETURN_ON_ERROR(http_perform_json("GET", "/healthz", NULL, &resp), TAG, "cloud healthz failed");
  out_health->http_status = resp.status_code;
  if (resp.status_code < 200 || resp.status_code >= 300 || !body_contains_ok_true(resp.body)) {
    ESP_LOGE(TAG, "cloud healthz bad status=%d body=%s", resp.status_code, resp.body);
    return ESP_FAIL;
  }
  out_health->supports_audio_streaming = json_object_extract_bool(resp.body, "asr", "deviceWsAudio",
                                                                  json_object_extract_bool(resp.body, "asr", "ready", 1));
  out_health->supports_tts =
      json_object_extract_bool(resp.body, "tts", "deviceWsAudio", json_object_extract_bool(resp.body, "tts", "ready", 0));
  out_health->supports_display = json_object_extract_bool(resp.body, "display", "enabled", 0);
  return ESP_OK;
}

esp_err_t bb_cloud_pair_request(bb_cloud_pairing_t* out_pairing) {
  if (out_pairing == NULL) {
    return ESP_ERR_INVALID_ARG;
  }
  memset(out_pairing, 0, sizeof(*out_pairing));

  char body[256] = {0};
  snprintf(body, sizeof(body), "{\"deviceId\":\"%s\"}", BBCLAW_DEVICE_ID);

  bb_http_resp_t resp = {0};
  ESP_RETURN_ON_ERROR(http_perform_json("POST", "/v1/pairings/request", body, &resp), TAG, "pair request failed");
  out_pairing->http_status = resp.status_code;

  if (resp.status_code < 200 || resp.status_code >= 300 || !body_contains_ok_true(resp.body)) {
    resolve_pairing_error_detail(resp.status_code, resp.body, out_pairing->detail, sizeof(out_pairing->detail));
    ESP_LOGE(TAG, "pair request bad status=%d body=%s", resp.status_code, resp.body);
    if (resp.status_code == 401 || resp.status_code == 403) {
      return ESP_ERR_INVALID_STATE;
    }
    return ESP_FAIL;
  }

  /* Avoid ~8KB on stack: main task stack is limited; bb_http_resp_t already holds body[8192]. */
  char* data_scope = (char*)malloc(8192U);
  const char* parse = resp.body;
  if (data_scope != NULL && json_copy_data_object(resp.body, data_scope, 8192U)) {
    parse = data_scope;
  }
  (void)json_extract_string(parse, "homeSiteId", out_pairing->home_site_id, sizeof(out_pairing->home_site_id));
  if (!json_extract_string(parse, "status", out_pairing->detail, sizeof(out_pairing->detail))) {
    snprintf(out_pairing->detail, sizeof(out_pairing->detail), "unknown");
  }
  (void)json_extract_pairing_code(parse, out_pairing->registration_code, sizeof(out_pairing->registration_code));
  (void)json_extract_string(parse, "expiresAt", out_pairing->registration_expires_at,
                            sizeof(out_pairing->registration_expires_at));

  if (strcmp(out_pairing->detail, "approved") == 0) {
    out_pairing->status = BB_CLOUD_PAIR_STATUS_APPROVED;
  } else if (strcmp(out_pairing->detail, "binding_required") == 0) {
    out_pairing->status = BB_CLOUD_PAIR_STATUS_BINDING_REQUIRED;
  } else {
    out_pairing->status = BB_CLOUD_PAIR_STATUS_PENDING;
  }
  static char s_pair_poll_code[16];
  static char s_pair_poll_detail[64];
  static char s_pair_poll_exp[40];
  int pair_poll_unchanged =
      (strcmp(s_pair_poll_code, out_pairing->registration_code) == 0 &&
       strcmp(s_pair_poll_detail, out_pairing->detail) == 0 && strcmp(s_pair_poll_exp, out_pairing->registration_expires_at) == 0);
  if (!pair_poll_unchanged) {
    snprintf(s_pair_poll_code, sizeof(s_pair_poll_code), "%s", out_pairing->registration_code);
    snprintf(s_pair_poll_detail, sizeof(s_pair_poll_detail), "%s", out_pairing->detail);
    snprintf(s_pair_poll_exp, sizeof(s_pair_poll_exp), "%s", out_pairing->registration_expires_at);
    ESP_LOGI(TAG,
             "pair request device=%s api_status=%s pair_state=%s home_site=%s code=%s expires=%s",
             BBCLAW_DEVICE_ID, out_pairing->detail, bb_cloud_pair_status_name(out_pairing->status),
             out_pairing->home_site_id[0] != '\0' ? out_pairing->home_site_id : "(n/a)",
             out_pairing->registration_code[0] != '\0' ? out_pairing->registration_code : "-",
             out_pairing->registration_expires_at[0] != '\0' ? out_pairing->registration_expires_at : "-");
  } else {
    ESP_LOGD(TAG, "pair poll unchanged status=%s code=%s", out_pairing->detail,
             out_pairing->registration_code[0] != '\0' ? out_pairing->registration_code : "-");
  }
  out_pairing->volume_pct = json_object_extract_int(resp.body, "config", "volumePct", -1);
  out_pairing->speed_ratio_x10 = json_object_extract_int(resp.body, "config", "speedRatio", -1);
  out_pairing->speaker_enabled = json_object_extract_int(resp.body, "config", "speakerEnabled", -1);
  /* speedRatio comes as e.g. 1 (integer part only from atoi of "1.2"), need to check for decimal */
  {
    char sr_buf[16] = {0};
    /* Extract raw speedRatio string to get decimal */
    char p1[64], p2[64];
    snprintf(p1, sizeof(p1), "\"speedRatio\":");
    snprintf(p2, sizeof(p2), "\"speedRatio\": ");
    const char* cfg_scope = strstr(resp.body, "\"config\"");
    if (cfg_scope != NULL) {
      const char* sr = strstr(cfg_scope, p1);
      if (sr == NULL) { sr = strstr(cfg_scope, p2); }
      if (sr != NULL) {
        sr += strlen(sr == strstr(cfg_scope, p1) ? p1 : p2);
        while (*sr == ' ') { sr++; }
        int di = 0;
        while (di < 15 && ((*sr >= '0' && *sr <= '9') || *sr == '.')) { sr_buf[di++] = *sr++; }
        sr_buf[di] = '\0';
        /* Convert "1.2" -> 12 (x10) */
        float fv = 0;
        if (di > 0) {
          /* simple: multiply by 10 and round */
          int whole = 0, frac = 0;
          char* dot = strchr(sr_buf, '.');
          if (dot != NULL) {
            *dot = '\0';
            whole = atoi(sr_buf);
            frac = atoi(dot + 1);
            if (frac > 9) { frac = frac / 10; } /* handle "1.20" */
          } else {
            whole = atoi(sr_buf);
          }
          out_pairing->speed_ratio_x10 = whole * 10 + frac;
        }
        (void)fv;
      }
    }
  }
  if (out_pairing->volume_pct >= 0 || out_pairing->speed_ratio_x10 > 0 || out_pairing->speaker_enabled >= 0) {
    if (!pair_poll_unchanged) {
      ESP_LOGI(TAG, "pair config volume_pct=%d speed_ratio_x10=%d speaker_enabled=%d", out_pairing->volume_pct,
               out_pairing->speed_ratio_x10, out_pairing->speaker_enabled);
    } else {
      ESP_LOGD(TAG, "pair config unchanged volume_pct=%d speed_ratio_x10=%d speaker_enabled=%d", out_pairing->volume_pct,
               out_pairing->speed_ratio_x10, out_pairing->speaker_enabled);
    }
  }
  free(data_scope);
  return ESP_OK;
}

esp_err_t bb_cloud_report_device_info(void) {
  const esp_app_desc_t *app = esp_app_get_description();
  const char *fw_ver = (app && app->version[0]) ? app->version : "unknown";

  char body[512];
  snprintf(body, sizeof(body),
           "{\"deviceId\":\"%s\","
           "\"firmwareVersion\":\"%s\","
           "\"capabilities\":{"
           "\"audioStreaming\":%s,"
           "\"tts\":%s,"
           "\"display\":%s,"
           "\"vad\":%s"
           "},"
           "\"hardware\":{"
           "\"audioInput\":\"%s\","
           "\"sampleRate\":%d,"
           "\"codec\":\"%s\","
           "\"screenWidth\":%d,"
           "\"screenHeight\":%d"
           "}}",
           BBCLAW_DEVICE_ID, fw_ver,
           BBCLAW_CLOUD_AUDIO_STREAMING_READY ? "true" : "false",
           BBCLAW_ENABLE_TTS_PLAYBACK ? "true" : "false",
           BBCLAW_ENABLE_DISPLAY_PULL ? "true" : "false",
           BBCLAW_VAD_ENABLE ? "true" : "false",
           BBCLAW_AUDIO_INPUT_SOURCE,
           BBCLAW_AUDIO_SAMPLE_RATE,
           BBCLAW_STREAM_CODEC,
           BBCLAW_ST7789_WIDTH,
           BBCLAW_ST7789_HEIGHT);

  bb_http_resp_t resp = {0};
  ESP_RETURN_ON_ERROR(http_perform_json("POST", "/v1/devices/info", body, &resp), TAG, "device info report failed");
  if (resp.status_code < 200 || resp.status_code >= 300) {
    ESP_LOGW(TAG, "device info report bad status=%d body=%s", resp.status_code, resp.body);
    return ESP_FAIL;
  }
  ESP_LOGI(TAG, "device info reported device=%s fw=%s", BBCLAW_DEVICE_ID, fw_ver);
  return ESP_OK;
}
