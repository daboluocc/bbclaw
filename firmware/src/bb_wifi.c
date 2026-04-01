#include "bb_wifi.h"

#include <ctype.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "bb_config.h"
#include "esp_check.h"
#include "esp_event.h"
#include "esp_http_server.h"
#include "esp_log.h"
#include "esp_mac.h"
#include "esp_netif.h"
#include "esp_system.h"
#include "esp_wifi.h"
#include "freertos/FreeRTOS.h"
#include "freertos/event_groups.h"
#include "freertos/task.h"
#include "lwip/ip4_addr.h"
#include "lwip/sockets.h"
#include "nvs.h"

static const char* TAG = "bb_wifi";

#define WIFI_CONNECTED_BIT BIT0
#define WIFI_FAIL_BIT BIT1
#define WIFI_FORM_BODY_MAX 192
#define WIFI_SCAN_RESULT_MAX 16

static EventGroupHandle_t s_wifi_event_group;
static esp_netif_t* s_sta_netif;
static esp_netif_t* s_ap_netif;
static httpd_handle_t s_http_server;
static int s_retry_num;
static int s_connected;
static int s_stack_ready;
static int s_wifi_ready;
static int s_event_handlers_registered;
static bb_wifi_mode_t s_mode;
static char s_active_ssid[sizeof(((wifi_sta_config_t*)0)->ssid)] = {0};
static char s_ap_ssid[sizeof(((wifi_ap_config_t*)0)->ssid)] = {0};
static char s_ap_password[sizeof(((wifi_ap_config_t*)0)->password)] = BBCLAW_WIFI_AP_PASSWORD;
static char s_ap_ip[16] = "192.168.4.1";

static void on_wifi_event(void* arg, esp_event_base_t event_base, int32_t event_id, void* event_data);

static int is_nonempty(const char* value) {
  return value != NULL && value[0] != '\0';
}

static void copy_string(char* dst, size_t dst_size, const char* src) {
  if (dst == NULL || dst_size == 0U) {
    return;
  }
  if (src == NULL) {
    dst[0] = '\0';
    return;
  }
  snprintf(dst, dst_size, "%s", src);
}

static void trim_ascii(char* value) {
  if (value == NULL) {
    return;
  }
  size_t len = strlen(value);
  while (len > 0U && isspace((unsigned char)value[len - 1U])) {
    value[--len] = '\0';
  }
  size_t start = 0;
  while (value[start] != '\0' && isspace((unsigned char)value[start])) {
    start++;
  }
  if (start > 0U) {
    memmove(value, value + start, strlen(value + start) + 1U);
  }
}

static int hex_value(char c) {
  if (c >= '0' && c <= '9') {
    return c - '0';
  }
  if (c >= 'a' && c <= 'f') {
    return 10 + (c - 'a');
  }
  if (c >= 'A' && c <= 'F') {
    return 10 + (c - 'A');
  }
  return -1;
}

static size_t url_decode_span(char* dst, size_t dst_size, const char* src, size_t src_len) {
  if (dst == NULL || dst_size == 0U) {
    return 0;
  }
  size_t di = 0;
  for (size_t si = 0; si < src_len && di + 1U < dst_size; ++si) {
    char c = src[si];
    if (c == '+') {
      dst[di++] = ' ';
      continue;
    }
    if (c == '%' && si + 2U < src_len) {
      int hi = hex_value(src[si + 1U]);
      int lo = hex_value(src[si + 2U]);
      if (hi >= 0 && lo >= 0) {
        dst[di++] = (char)((hi << 4) | lo);
        si += 2U;
        continue;
      }
    }
    dst[di++] = c;
  }
  dst[di] = '\0';
  return di;
}

static esp_err_t get_form_value(const char* body, const char* key, char* out, size_t out_size) {
  if (body == NULL || key == NULL || out == NULL || out_size == 0U) {
    return ESP_ERR_INVALID_ARG;
  }
  size_t key_len = strlen(key);
  const char* cursor = body;
  while (*cursor != '\0') {
    const char* pair_end = strchr(cursor, '&');
    if (pair_end == NULL) {
      pair_end = cursor + strlen(cursor);
    }
    const char* eq = memchr(cursor, '=', (size_t)(pair_end - cursor));
    if (eq != NULL && (size_t)(eq - cursor) == key_len && strncmp(cursor, key, key_len) == 0) {
      url_decode_span(out, out_size, eq + 1, (size_t)(pair_end - (eq + 1)));
      trim_ascii(out);
      return ESP_OK;
    }
    if (*pair_end == '\0') {
      break;
    }
    cursor = pair_end + 1;
  }
  out[0] = '\0';
  return ESP_ERR_NOT_FOUND;
}

static esp_err_t load_nvs_string(nvs_handle_t handle, const char* key, char* out, size_t out_size) {
  size_t required = 0;
  esp_err_t err = nvs_get_str(handle, key, NULL, &required);
  if (err != ESP_OK) {
    return err;
  }
  if (required == 0U || required > out_size) {
    return ESP_ERR_NVS_INVALID_LENGTH;
  }
  return nvs_get_str(handle, key, out, &required);
}

/* --- Multi-WiFi NVS helpers --- */

static void nvs_slot_key(char* out, size_t out_size, const char* prefix, int index) {
  snprintf(out, out_size, "%s_%d", prefix, index);
}

static esp_err_t load_saved_wifi_slot(int index, char* ssid, size_t ssid_size, char* password, size_t password_size) {
  if (index < 0 || index >= BBCLAW_WIFI_MAX_SAVED) {
    return ESP_ERR_INVALID_ARG;
  }
  ssid[0] = '\0';
  password[0] = '\0';
  nvs_handle_t handle;
  esp_err_t err = nvs_open(BBCLAW_WIFI_NVS_NAMESPACE, NVS_READONLY, &handle);
  if (err != ESP_OK) {
    return ESP_ERR_NOT_FOUND; /* no namespace yet — not an error */
  }
  char key[16];
  nvs_slot_key(key, sizeof(key), BBCLAW_WIFI_NVS_KEY_SSID, index);
  err = load_nvs_string(handle, key, ssid, ssid_size);
  if (err == ESP_OK && is_nonempty(ssid)) {
    nvs_slot_key(key, sizeof(key), BBCLAW_WIFI_NVS_KEY_PASSWORD, index);
    esp_err_t perr = load_nvs_string(handle, key, password, password_size);
    if (perr == ESP_ERR_NVS_NOT_FOUND) {
      password[0] = '\0';
    } else if (perr != ESP_OK) {
      err = perr;
    }
  }
  nvs_close(handle);
  return err;
}

static esp_err_t save_wifi_slot(int index, const char* ssid, const char* password) {
  if (index < 0 || index >= BBCLAW_WIFI_MAX_SAVED) {
    return ESP_ERR_INVALID_ARG;
  }
  nvs_handle_t handle;
  ESP_RETURN_ON_ERROR(nvs_open(BBCLAW_WIFI_NVS_NAMESPACE, NVS_READWRITE, &handle), TAG, "nvs open");
  char key[16];
  nvs_slot_key(key, sizeof(key), BBCLAW_WIFI_NVS_KEY_SSID, index);
  esp_err_t err = nvs_set_str(handle, key, ssid);
  if (err == ESP_OK) {
    nvs_slot_key(key, sizeof(key), BBCLAW_WIFI_NVS_KEY_PASSWORD, index);
    err = nvs_set_str(handle, key, password != NULL ? password : "");
  }
  if (err == ESP_OK) {
    err = nvs_commit(handle);
  }
  nvs_close(handle);
  return err;
}

static esp_err_t delete_wifi_slot(int index) {
  if (index < 0 || index >= BBCLAW_WIFI_MAX_SAVED) {
    return ESP_ERR_INVALID_ARG;
  }
  nvs_handle_t handle;
  ESP_RETURN_ON_ERROR(nvs_open(BBCLAW_WIFI_NVS_NAMESPACE, NVS_READWRITE, &handle), TAG, "nvs open");
  char key[16];
  nvs_slot_key(key, sizeof(key), BBCLAW_WIFI_NVS_KEY_SSID, index);
  (void)nvs_erase_key(handle, key);
  nvs_slot_key(key, sizeof(key), BBCLAW_WIFI_NVS_KEY_PASSWORD, index);
  (void)nvs_erase_key(handle, key);
  /* compact: shift higher slots down */
  for (int i = index; i < BBCLAW_WIFI_MAX_SAVED - 1; i++) {
    char s[sizeof(((wifi_sta_config_t*)0)->ssid)] = {0};
    char p[sizeof(((wifi_sta_config_t*)0)->password)] = {0};
    char sk[16], pk[16];
    nvs_slot_key(sk, sizeof(sk), BBCLAW_WIFI_NVS_KEY_SSID, i + 1);
    size_t req = sizeof(s);
    if (nvs_get_str(handle, sk, s, &req) == ESP_OK && is_nonempty(s)) {
      nvs_slot_key(pk, sizeof(pk), BBCLAW_WIFI_NVS_KEY_PASSWORD, i + 1);
      req = sizeof(p);
      (void)nvs_get_str(handle, pk, p, &req);
      char dk[16], dpk[16];
      nvs_slot_key(dk, sizeof(dk), BBCLAW_WIFI_NVS_KEY_SSID, i);
      nvs_slot_key(dpk, sizeof(dpk), BBCLAW_WIFI_NVS_KEY_PASSWORD, i);
      nvs_set_str(handle, dk, s);
      nvs_set_str(handle, dpk, p);
      (void)nvs_erase_key(handle, sk);
      (void)nvs_erase_key(handle, pk);
    } else {
      break;
    }
  }
  nvs_commit(handle);
  nvs_close(handle);
  return ESP_OK;
}

/* Find first empty slot, or -1 if full. Skip if ssid already saved. */
static int find_or_add_wifi_slot(const char* ssid) {
  int first_empty = -1;
  nvs_handle_t handle;
  if (nvs_open(BBCLAW_WIFI_NVS_NAMESPACE, NVS_READONLY, &handle) != ESP_OK) {
    return 0;
  }
  char key[16];
  char tmp[sizeof(((wifi_sta_config_t*)0)->ssid)];
  for (int i = 0; i < BBCLAW_WIFI_MAX_SAVED; i++) {
    nvs_slot_key(key, sizeof(key), BBCLAW_WIFI_NVS_KEY_SSID, i);
    size_t req = sizeof(tmp);
    if (nvs_get_str(handle, key, tmp, &req) == ESP_OK && is_nonempty(tmp)) {
      if (strcmp(tmp, ssid) == 0) {
        nvs_close(handle);
        return i; /* already exists, overwrite */
      }
    } else if (first_empty < 0) {
      first_empty = i;
    }
  }
  nvs_close(handle);
  return first_empty; /* -1 if full */
}

/* --- Legacy-compatible wrappers --- */

static esp_err_t save_sta_credentials(const char* ssid, const char* password) {
  int slot = find_or_add_wifi_slot(ssid);
  if (slot < 0) {
    return ESP_ERR_NO_MEM; /* all slots full */
  }
  return save_wifi_slot(slot, ssid, password);
}

static void restart_task(void* arg) {
  (void)arg;
  vTaskDelay(pdMS_TO_TICKS(1200));
  esp_restart();
}

/* --- Saved WiFi list: GET /saved -> JSON array --- */
static esp_err_t handle_saved_get(httpd_req_t* req) {
  char json[512];
  int pos = 0;
  json[pos++] = '[';
  char ssid[sizeof(((wifi_sta_config_t*)0)->ssid)];
  char pass[sizeof(((wifi_sta_config_t*)0)->password)];
  int written = 0;
  for (int i = 0; i < BBCLAW_WIFI_MAX_SAVED; i++) {
    if (load_saved_wifi_slot(i, ssid, sizeof(ssid), pass, sizeof(pass)) == ESP_OK && is_nonempty(ssid)) {
      if (written > 0) json[pos++] = ',';
      pos += snprintf(json + pos, sizeof(json) - pos, "{\"i\":%d,\"ssid\":\"%s\"}", i, ssid);
      written++;
    }
  }
  json[pos++] = ']';
  json[pos] = '\0';
  httpd_resp_set_type(req, "application/json");
  return httpd_resp_send(req, json, HTTPD_RESP_USE_STRLEN);
}

/* --- Delete saved WiFi: POST /delete  body: index=N --- */
static esp_err_t handle_delete_post(httpd_req_t* req) {
  if (req->content_len <= 0 || req->content_len >= 32) {
    return httpd_resp_send_err(req, HTTPD_400_BAD_REQUEST, "bad");
  }
  char body[32];
  int received = httpd_req_recv(req, body, req->content_len);
  if (received <= 0) return httpd_resp_send_err(req, HTTPD_400_BAD_REQUEST, "read fail");
  body[received] = '\0';
  char idx_str[8] = {0};
  if (get_form_value(body, "index", idx_str, sizeof(idx_str)) != ESP_OK) {
    return httpd_resp_send_err(req, HTTPD_400_BAD_REQUEST, "index required");
  }
  int idx = atoi(idx_str);
  esp_err_t err = delete_wifi_slot(idx);
  httpd_resp_set_type(req, "application/json");
  return httpd_resp_sendstr(req, err == ESP_OK ? "{\"ok\":true}" : "{\"ok\":false}");
}

static size_t json_escape_span(char* dst, size_t dst_size, const char* src) {
  if (dst == NULL || dst_size == 0U) {
    return 0;
  }
  size_t di = 0;
  if (src == NULL) {
    dst[0] = '\0';
    return 0;
  }
  for (size_t si = 0; src[si] != '\0' && di + 1U < dst_size; ++si) {
    char c = src[si];
    if ((c == '"' || c == '\\') && di + 2U < dst_size) {
      dst[di++] = '\\';
      dst[di++] = c;
    } else if (c == '\n' && di + 2U < dst_size) {
      dst[di++] = '\\';
      dst[di++] = 'n';
    } else if (c == '\r' && di + 2U < dst_size) {
      dst[di++] = '\\';
      dst[di++] = 'r';
    } else if (c == '\t' && di + 2U < dst_size) {
      dst[di++] = '\\';
      dst[di++] = 't';
    } else {
      dst[di++] = c;
    }
  }
  dst[di] = '\0';
  return di;
}

static int compare_ap_record_rssi_desc(const void* lhs, const void* rhs) {
  const wifi_ap_record_t* a = (const wifi_ap_record_t*)lhs;
  const wifi_ap_record_t* b = (const wifi_ap_record_t*)rhs;
  return (int)b->rssi - (int)a->rssi;
}

static esp_err_t handle_scan_get(httpd_req_t* req) {
  wifi_mode_t mode = WIFI_MODE_NULL;
  esp_err_t err = esp_wifi_get_mode(&mode);
  if (err != ESP_OK) {
    ESP_LOGE(TAG, "wifi get mode failed err=%s", esp_err_to_name(err));
    return httpd_resp_send_err(req, HTTPD_500_INTERNAL_SERVER_ERROR, "wifi mode failed");
  }
  if (mode == WIFI_MODE_AP) {
    err = esp_wifi_set_mode(WIFI_MODE_APSTA);
    if (err != ESP_OK) {
      ESP_LOGE(TAG, "wifi set APSTA failed err=%s", esp_err_to_name(err));
      return httpd_resp_send_err(req, HTTPD_500_INTERNAL_SERVER_ERROR, "wifi scan mode failed");
    }
  }

  wifi_scan_config_t scan_cfg = {
      .show_hidden = false,
  };
  ESP_LOGI(TAG, "wifi provisioning scan start");
  err = esp_wifi_scan_start(&scan_cfg, true);
  if (err != ESP_OK) {
    ESP_LOGW(TAG, "wifi scan start failed err=%s", esp_err_to_name(err));
    httpd_resp_set_status(req, "503 Service Unavailable");
    return httpd_resp_sendstr(req, "wifi scan failed");
  }

  uint16_t ap_num = 0;
  ESP_ERROR_CHECK_WITHOUT_ABORT(esp_wifi_scan_get_ap_num(&ap_num));
  if (ap_num == 0) {
    httpd_resp_set_type(req, "application/json");
    return httpd_resp_sendstr(req, "{\"ok\":true,\"networks\":[]}");
  }

  uint16_t fetch_num = ap_num > WIFI_SCAN_RESULT_MAX ? WIFI_SCAN_RESULT_MAX : ap_num;
  wifi_ap_record_t records[WIFI_SCAN_RESULT_MAX];
  memset(records, 0, sizeof(records));
  err = esp_wifi_scan_get_ap_records(&fetch_num, records);
  if (err != ESP_OK) {
    ESP_LOGW(TAG, "wifi scan get records failed err=%s", esp_err_to_name(err));
    return httpd_resp_send_err(req, HTTPD_500_INTERNAL_SERVER_ERROR, "wifi scan records failed");
  }

  qsort(records, fetch_num, sizeof(records[0]), compare_ap_record_rssi_desc);

  char json[2048];
  int pos = snprintf(json, sizeof(json), "{\"ok\":true,\"networks\":[");
  int written = 0;
  for (uint16_t i = 0; i < fetch_num && pos > 0 && pos < (int)sizeof(json); ++i) {
    const char* ssid = (const char*)records[i].ssid;
    if (!is_nonempty(ssid)) {
      continue;
    }
    int duplicate = 0;
    for (uint16_t j = 0; j < i; ++j) {
      if (strcmp((const char*)records[j].ssid, ssid) == 0) {
        duplicate = 1;
        break;
      }
    }
    if (duplicate) {
      continue;
    }
    char escaped_ssid[sizeof(records[i].ssid) * 2U + 1U];
    json_escape_span(escaped_ssid, sizeof(escaped_ssid), ssid);
    pos += snprintf(json + pos, sizeof(json) - (size_t)pos, "%s{\"ssid\":\"%s\",\"rssi\":%d,\"secure\":%s}",
                    written > 0 ? "," : "", escaped_ssid, (int)records[i].rssi,
                    records[i].authmode == WIFI_AUTH_OPEN ? "false" : "true");
    written++;
  }
  if (pos < 0 || pos >= (int)sizeof(json)) {
    return httpd_resp_send_err(req, HTTPD_500_INTERNAL_SERVER_ERROR, "wifi scan response too large");
  }
  pos += snprintf(json + pos, sizeof(json) - (size_t)pos, "]}");
  if (pos < 0 || pos >= (int)sizeof(json)) {
    return httpd_resp_send_err(req, HTTPD_500_INTERNAL_SERVER_ERROR, "wifi scan response too large");
  }
  ESP_LOGI(TAG, "wifi provisioning scan done networks=%d", written);
  httpd_resp_set_type(req, "application/json");
  return httpd_resp_send(req, json, HTTPD_RESP_USE_STRLEN);
}

static esp_err_t handle_root_get(httpd_req_t* req) {
  /* static buffers — httpd is single-threaded per connection */
  static char saved_html[400];
  static char html[6200];
  saved_html[0] = '\0';
  int spos = 0;
  char ssid[sizeof(((wifi_sta_config_t*)0)->ssid)];
  char pass[sizeof(((wifi_sta_config_t*)0)->password)];
  for (int i = 0; i < BBCLAW_WIFI_MAX_SAVED; i++) {
    if (load_saved_wifi_slot(i, ssid, sizeof(ssid), pass, sizeof(pass)) == ESP_OK && is_nonempty(ssid)) {
      spos += snprintf(saved_html + spos, sizeof(saved_html) - spos,
                       "<div class='row'><span>%s</span>"
                       "<button type='button' onclick=\"del(%d)\">&#x2715;</button></div>",
                       ssid, i);
    }
  }

  static const char fmt[] =
      "<!doctype html><html><head><meta charset='utf-8'>"
      "<meta name='viewport' content='width=device-width,initial-scale=1'>"
      "<title>BBClaw Wi-Fi</title><style>"
      "body{font-family:sans-serif;max-width:520px;margin:32px auto;padding:0 16px;line-height:1.5;}"
      "input{width:100%%;padding:10px;margin:6px 0 12px;box-sizing:border-box;}"
      "button{padding:10px 14px;}.card{border:1px solid #ddd;border-radius:12px;padding:20px;margin-bottom:16px;}"
      ".muted{color:#666;font-size:13px;}.row{display:flex;justify-content:space-between;align-items:center;"
      "padding:6px 0;border-bottom:1px solid #eee;}.row button{background:#f44;color:#fff;border:none;"
      "border-radius:4px;cursor:pointer;padding:4px 10px;}.wifi-item{display:flex;justify-content:space-between;"
      "align-items:center;gap:12px;padding:10px 0;border-bottom:1px solid #eee;}.wifi-meta{font-size:12px;color:#666;}"
      ".pick{background:#111;color:#fff;border:none;border-radius:8px;cursor:pointer;padding:8px 12px;}"
      ".secondary{background:#f3f3f3;color:#111;border:1px solid #ddd;border-radius:8px;cursor:pointer;padding:8px 12px;}"
      "</style></head><body>"
      "<div class='card'><h2>BBClaw Wi-Fi</h2>"
      "<p class='muted'>AP: %s &middot; IP: %s &middot; PW: %s</p>"
      "<form method='post' action='/configure'>"
      "<label>SSID</label><input id='ssid' name='ssid' maxlength='32' required>"
      "<label>Password</label><input name='password' maxlength='64' type='password'>"
      "<button type='submit'>Save &amp; Reboot</button></form></div>"
      "<div class='card'><div class='row'><strong>Nearby Wi-Fi</strong>"
      "<button class='secondary' type='button' onclick='scanWifi()'>Rescan</button></div>"
      "<p id='scan-status' class='muted'>Scanning nearby networks...</p><div id='scan-list'></div></div>"
      "<div class='card'><h3>Saved WiFi</h3><div id='saved'>%s</div>"
      "<p class='muted'>Max %d. First match connects.</p></div>"
      "<script>"
      "function esc(s){return String(s||'').replace(/[&<>\"']/g,function(c){return({'&':'&amp;','<':'&lt;','>':'&gt;','\"':'&quot;',\"'\":'&#39;'})[c];});}"
      "function pickWifi(ssid,secure){document.getElementById('ssid').value=ssid||'';"
      "var pwd=document.querySelector('input[name=password]');if(pwd){if(!secure)pwd.value='';pwd.focus();}}"
      "function renderWifi(items){var list=document.getElementById('scan-list');"
      "if(!items||!items.length){list.innerHTML=\"<p class='muted'>No nearby networks found.</p>\";return;}"
      "list.innerHTML=items.map(function(n){var meta='RSSI '+n.rssi+' | '+(n.secure?'secure':'open');"
      "return \"<div class='wifi-item'><div><div><strong>\"+esc(n.ssid)+\"</strong></div><div class='wifi-meta'>\"+meta+\"</div></div>\"+"
      "\"<button class='pick' type='button' onclick='pickWifi(\"+JSON.stringify(n.ssid)+\",\"+(n.secure?'true':'false')+\")'>Use</button></div>\";"
      "}).join('');}"
      "function scanWifi(){document.getElementById('scan-status').textContent='Scanning nearby networks...';"
      "fetch('/scan').then(function(r){if(!r.ok)throw new Error('scan failed');return r.json();}).then(function(data){"
      "document.getElementById('scan-status').textContent='Tap a network to fill the form.';renderWifi(data.networks||[]);"
      "}).catch(function(){document.getElementById('scan-status').textContent='Scan failed. Try again.';"
      "document.getElementById('scan-list').innerHTML='';});}"
      "function del(i){if(!confirm('Delete?'))return;"
      "fetch('/delete',{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded'},"
      "body:'index='+i}).then(()=>location.reload());}"
      "scanWifi();"
      "</script></body></html>";

  int n = snprintf(html, sizeof(html), fmt, s_ap_ssid, s_ap_ip,
                   is_nonempty(s_ap_password) ? s_ap_password : "(open)",
                   saved_html, BBCLAW_WIFI_MAX_SAVED);
  if (n < 0 || (size_t)n >= sizeof(html)) {
    return httpd_resp_send_err(req, HTTPD_500_INTERNAL_SERVER_ERROR, "page build failed");
  }
  httpd_resp_set_type(req, "text/html; charset=utf-8");
  return httpd_resp_send(req, html, HTTPD_RESP_USE_STRLEN);
}

static esp_err_t handle_configure_post(httpd_req_t* req) {
  if (req->content_len <= 0 || req->content_len >= WIFI_FORM_BODY_MAX) {
    return httpd_resp_send_err(req, HTTPD_400_BAD_REQUEST, "invalid form size");
  }

  char body[WIFI_FORM_BODY_MAX];
  int received = 0;
  while (received < req->content_len) {
    int chunk = httpd_req_recv(req, body + received, req->content_len - received);
    if (chunk <= 0) {
      return httpd_resp_send_err(req, HTTPD_400_BAD_REQUEST, "form read failed");
    }
    received += chunk;
  }
  body[received] = '\0';

  char ssid[sizeof(((wifi_sta_config_t*)0)->ssid)] = {0};
  char password[sizeof(((wifi_sta_config_t*)0)->password)] = {0};
  if (get_form_value(body, "ssid", ssid, sizeof(ssid)) != ESP_OK || !is_nonempty(ssid)) {
    return httpd_resp_send_err(req, HTTPD_400_BAD_REQUEST, "ssid required");
  }
  (void)get_form_value(body, "password", password, sizeof(password));

  esp_err_t err = save_sta_credentials(ssid, password);
  if (err != ESP_OK) {
    ESP_LOGE(TAG, "wifi nvs save failed err=%s", esp_err_to_name(err));
    return httpd_resp_send_err(req, HTTPD_500_INTERNAL_SERVER_ERROR, "save failed");
  }

  ESP_LOGI(TAG, "wifi credentials saved ssid=%s rebooting", ssid);
  httpd_resp_set_type(req, "text/html; charset=utf-8");
  ESP_RETURN_ON_ERROR(
      httpd_resp_sendstr(req, "<html><body><h3>Saved</h3><p>Device will reboot and retry Wi-Fi.</p></body></html>"), TAG,
      "send response failed");
  BaseType_t ok = xTaskCreate(restart_task, "bb_wifi_restart", 2048, NULL, 4, NULL);
  return ok == pdPASS ? ESP_OK : ESP_ERR_NO_MEM;
}

/* --- Captive portal: redirect any unknown GET to "/" --- */
static esp_err_t handle_captive_redirect(httpd_req_t* req) {
  httpd_resp_set_status(req, "302 Found");
  httpd_resp_set_hdr(req, "Location", "http://192.168.4.1/");
  return httpd_resp_send(req, NULL, 0);
}

/* --- Captive portal: tiny DNS server that resolves everything to our AP IP --- */
static void dns_captive_task(void* arg) {
  int sock = socket(AF_INET, SOCK_DGRAM, IPPROTO_UDP);
  if (sock < 0) {
    ESP_LOGE(TAG, "captive dns: socket failed");
    vTaskDelete(NULL);
    return;
  }
  struct sockaddr_in addr = {
      .sin_family = AF_INET,
      .sin_port = htons(53),
      .sin_addr.s_addr = htonl(INADDR_ANY),
  };
  if (bind(sock, (struct sockaddr*)&addr, sizeof(addr)) < 0) {
    ESP_LOGE(TAG, "captive dns: bind failed");
    close(sock);
    vTaskDelete(NULL);
    return;
  }
  ESP_LOGI(TAG, "captive dns started");
  uint8_t buf[512];
  while (1) {
    struct sockaddr_in src;
    socklen_t src_len = sizeof(src);
    int n = recvfrom(sock, buf, sizeof(buf), 0, (struct sockaddr*)&src, &src_len);
    if (n < 12) continue;
    /* Build minimal DNS response: copy query, set QR+AA flags, append A record pointing to 192.168.4.1 */
    buf[2] |= 0x80;  /* QR = response */
    buf[3] = 0x00;    /* no error */
    buf[6] = 0; buf[7] = 1;  /* 1 answer */
    /* Append answer: pointer to name in question, type A, class IN, TTL 60, 4 bytes, 192.168.4.1 */
    if (n + 16 <= (int)sizeof(buf)) {
      uint8_t* p = buf + n;
      *p++ = 0xc0; *p++ = 0x0c;          /* name pointer */
      *p++ = 0x00; *p++ = 0x01;          /* type A */
      *p++ = 0x00; *p++ = 0x01;          /* class IN */
      *p++ = 0x00; *p++ = 0x00; *p++ = 0x00; *p++ = 0x3c;  /* TTL 60s */
      *p++ = 0x00; *p++ = 0x04;          /* rdlength */
      *p++ = 192; *p++ = 168; *p++ = 4; *p++ = 1;
      n = (int)(p - buf);
    }
    sendto(sock, buf, n, 0, (struct sockaddr*)&src, src_len);
  }
}

static void start_captive_dns(void) {
  static int s_dns_started = 0;
  if (s_dns_started) return;
  s_dns_started = 1;
  xTaskCreate(dns_captive_task, "captive_dns", 3072, NULL, 3, NULL);
}

static esp_err_t ensure_http_server_started(void) {
  if (s_http_server != NULL) {
    return ESP_OK;
  }

  httpd_config_t config = HTTPD_DEFAULT_CONFIG();
  config.stack_size = 8192;
  /*
   * HTTP server internally reserves 3 sockets, and provisioning mode also keeps
   * one UDP socket open for captive DNS. Using the HTTPD default (7) together
   * with CONFIG_LWIP_MAX_SOCKETS=10 exhausts the global socket table and causes
   * accept() to fail with ENFILE under captive portal traffic.
   */
  config.max_open_sockets = BBCLAW_WIFI_AP_MAX_CONNECTIONS;
  config.max_uri_handlers = 12;
  config.lru_purge_enable = true;
  config.uri_match_fn = httpd_uri_match_wildcard;

  ESP_RETURN_ON_ERROR(httpd_start(&s_http_server, &config), TAG, "http server start failed");

  httpd_uri_t root = {
      .uri = "/",
      .method = HTTP_GET,
      .handler = handle_root_get,
      .user_ctx = NULL,
  };
  httpd_uri_t configure = {
      .uri = "/configure",
      .method = HTTP_POST,
      .handler = handle_configure_post,
      .user_ctx = NULL,
  };
  httpd_uri_t scan = {
      .uri = "/scan",
      .method = HTTP_GET,
      .handler = handle_scan_get,
      .user_ctx = NULL,
  };
  httpd_uri_t submit_legacy = {
      .uri = "/submit",
      .method = HTTP_POST,
      .handler = handle_configure_post,
      .user_ctx = NULL,
  };

  esp_err_t err = httpd_register_uri_handler(s_http_server, &root);
  if (err == ESP_OK) {
    err = httpd_register_uri_handler(s_http_server, &configure);
  }
  if (err == ESP_OK) {
    err = httpd_register_uri_handler(s_http_server, &scan);
  }
  if (err == ESP_OK) {
    err = httpd_register_uri_handler(s_http_server, &submit_legacy);
  }
  /* Saved list, delete */
  if (err == ESP_OK) {
    httpd_uri_t saved = {.uri = "/saved", .method = HTTP_GET, .handler = handle_saved_get};
    err = httpd_register_uri_handler(s_http_server, &saved);
  }
  if (err == ESP_OK) {
    httpd_uri_t del = {.uri = "/delete", .method = HTTP_POST, .handler = handle_delete_post};
    err = httpd_register_uri_handler(s_http_server, &del);
  }
  /* Captive portal: catch-all redirect for any other GET (must be registered last) */
  if (err == ESP_OK) {
    httpd_uri_t captive_catch_all = {
        .uri = "/*",
        .method = HTTP_GET,
        .handler = handle_captive_redirect,
        .user_ctx = NULL,
    };
    err = httpd_register_uri_handler(s_http_server, &captive_catch_all);
  }
  if (err != ESP_OK) {
    httpd_stop(s_http_server);
    s_http_server = NULL;
    return err;
  }
  return ESP_OK;
}

static esp_err_t ensure_wifi_stack_ready(void) {
  if (!s_stack_ready) {
    ESP_RETURN_ON_ERROR(esp_netif_init(), TAG, "esp_netif_init failed");
    ESP_RETURN_ON_ERROR(esp_event_loop_create_default(), TAG, "event loop create failed");
    s_stack_ready = 1;
  }

  if (s_sta_netif == NULL) {
    s_sta_netif = esp_netif_create_default_wifi_sta();
    if (s_sta_netif == NULL) {
      return ESP_ERR_NO_MEM;
    }
  }
  if (s_ap_netif == NULL) {
    s_ap_netif = esp_netif_create_default_wifi_ap();
    if (s_ap_netif == NULL) {
      return ESP_ERR_NO_MEM;
    }
  }
  if (s_wifi_event_group == NULL) {
    s_wifi_event_group = xEventGroupCreate();
    if (s_wifi_event_group == NULL) {
      return ESP_ERR_NO_MEM;
    }
  }
  if (!s_wifi_ready) {
    wifi_init_config_t cfg = WIFI_INIT_CONFIG_DEFAULT();
    ESP_RETURN_ON_ERROR(esp_wifi_init(&cfg), TAG, "esp_wifi_init failed");
    ESP_RETURN_ON_ERROR(esp_wifi_set_storage(WIFI_STORAGE_RAM), TAG, "set wifi storage failed");
    s_wifi_ready = 1;
  }
  if (!s_event_handlers_registered) {
    ESP_RETURN_ON_ERROR(
        esp_event_handler_register(WIFI_EVENT, ESP_EVENT_ANY_ID, &on_wifi_event, NULL), TAG, "wifi event register failed");
    ESP_RETURN_ON_ERROR(
        esp_event_handler_register(IP_EVENT, IP_EVENT_STA_GOT_IP, &on_wifi_event, NULL), TAG, "ip event register failed");
    s_event_handlers_registered = 1;
  }
  return ESP_OK;
}

static esp_err_t start_sta_connection(const char* ssid, const char* password) {
  s_connected = 0;
  s_retry_num = 0;
  xEventGroupClearBits(s_wifi_event_group, WIFI_CONNECTED_BIT | WIFI_FAIL_BIT);
  copy_string(s_active_ssid, sizeof(s_active_ssid), ssid);

  wifi_config_t wifi_config = {
      .sta =
          {
              .failure_retry_cnt = BBCLAW_WIFI_STA_MAX_RETRY,
              .threshold.authmode = is_nonempty(password) ? WIFI_AUTH_WPA2_PSK : WIFI_AUTH_OPEN,
              .pmf_cfg =
                  {
                      .capable = true,
                      .required = false,
                  },
          },
  };
  copy_string((char*)wifi_config.sta.ssid, sizeof(wifi_config.sta.ssid), ssid);
  copy_string((char*)wifi_config.sta.password, sizeof(wifi_config.sta.password), password);

  (void)esp_wifi_disconnect();
  (void)esp_wifi_stop();
  ESP_RETURN_ON_ERROR(esp_wifi_set_mode(WIFI_MODE_STA), TAG, "set sta mode failed");
  ESP_RETURN_ON_ERROR(esp_wifi_set_config(WIFI_IF_STA, &wifi_config), TAG, "set wifi config failed");
  ESP_RETURN_ON_ERROR(esp_wifi_start(), TAG, "wifi start failed");

  EventBits_t bits = xEventGroupWaitBits(
      s_wifi_event_group, WIFI_CONNECTED_BIT | WIFI_FAIL_BIT, pdFALSE, pdFALSE,
      pdMS_TO_TICKS(BBCLAW_WIFI_STA_CONNECT_TIMEOUT_MS));

  if (bits & WIFI_CONNECTED_BIT) {
    s_mode = BB_WIFI_MODE_STA_CONNECTED;
    ESP_LOGI(TAG, "wifi connected ssid=%s", ssid);
    return ESP_OK;
  }

  ESP_LOGW(TAG, "wifi connect timeout/fail ssid=%s", ssid);
  return ESP_FAIL;
}

static esp_err_t start_ap_provisioning_mode(void) {
  uint8_t mac[6] = {0};
  if (esp_read_mac(mac, ESP_MAC_WIFI_SOFTAP) == ESP_OK) {
    snprintf(s_ap_ssid, sizeof(s_ap_ssid), "%s-%02X%02X%02X", BBCLAW_WIFI_AP_SSID_PREFIX, mac[3], mac[4], mac[5]);
  } else {
    copy_string(s_ap_ssid, sizeof(s_ap_ssid), BBCLAW_WIFI_AP_SSID_PREFIX);
  }
  copy_string(s_ap_password, sizeof(s_ap_password), BBCLAW_WIFI_AP_PASSWORD);

  wifi_config_t ap_config = {
      .ap =
          {
              .ssid_len = 0,
              .channel = BBCLAW_WIFI_AP_CHANNEL,
              .max_connection = BBCLAW_WIFI_AP_MAX_CONNECTIONS,
              .authmode = is_nonempty(s_ap_password) ? WIFI_AUTH_WPA_WPA2_PSK : WIFI_AUTH_OPEN,
          },
  };
  copy_string((char*)ap_config.ap.ssid, sizeof(ap_config.ap.ssid), s_ap_ssid);
  copy_string((char*)ap_config.ap.password, sizeof(ap_config.ap.password), s_ap_password);
  ap_config.ap.ssid_len = strlen(s_ap_ssid);

  (void)esp_wifi_disconnect();
  (void)esp_wifi_stop();
  ESP_RETURN_ON_ERROR(esp_wifi_set_mode(WIFI_MODE_AP), TAG, "set ap mode failed");
  ESP_RETURN_ON_ERROR(esp_wifi_set_config(WIFI_IF_AP, &ap_config), TAG, "set ap config failed");
  ESP_RETURN_ON_ERROR(esp_wifi_start(), TAG, "ap start failed");

  esp_netif_ip_info_t ip_info;
  if (s_ap_netif != NULL && esp_netif_get_ip_info(s_ap_netif, &ip_info) == ESP_OK) {
    snprintf(s_ap_ip, sizeof(s_ap_ip), IPSTR, IP2STR(&ip_info.ip));
  } else {
    copy_string(s_ap_ip, sizeof(s_ap_ip), "192.168.4.1");
  }

  ESP_RETURN_ON_ERROR(ensure_http_server_started(), TAG, "provision server start failed");
  start_captive_dns();
  s_mode = BB_WIFI_MODE_AP_PROVISIONING;
  s_connected = 0;
  s_active_ssid[0] = '\0';
  ESP_LOGW(TAG, "wifi provisioning ap ready ssid=%s password=%s ip=%s", s_ap_ssid,
           is_nonempty(s_ap_password) ? s_ap_password : "(open)", s_ap_ip);
  return ESP_OK;
}

static void on_wifi_event(void* arg, esp_event_base_t event_base, int32_t event_id, void* event_data) {
  (void)arg;

  if (event_base == WIFI_EVENT && event_id == WIFI_EVENT_STA_START) {
    (void)esp_wifi_connect();
    return;
  }
  if (event_base == WIFI_EVENT && event_id == WIFI_EVENT_STA_DISCONNECTED) {
    s_connected = 0;
    if (s_mode == BB_WIFI_MODE_AP_PROVISIONING) {
      return;
    }
    if (s_retry_num < BBCLAW_WIFI_STA_MAX_RETRY) {
      s_retry_num++;
      ESP_LOGW(TAG, "wifi reconnect attempt=%d", s_retry_num);
      (void)esp_wifi_connect();
    } else {
      ESP_LOGW(TAG, "wifi retries exhausted for current ssid=%s", s_active_ssid);
      xEventGroupSetBits(s_wifi_event_group, WIFI_FAIL_BIT);
    }
    return;
  }
  if (event_base == IP_EVENT && event_id == IP_EVENT_STA_GOT_IP) {
    const ip_event_got_ip_t* event = (const ip_event_got_ip_t*)event_data;
    s_retry_num = 0;
    s_connected = 1;
    ESP_LOGI(TAG, "got ip: " IPSTR, IP2STR(&event->ip_info.ip));
    xEventGroupSetBits(s_wifi_event_group, WIFI_CONNECTED_BIT);
    /* 记住本次成功的 SSID，下次启动优先尝试 */
    if (is_nonempty(s_active_ssid)) {
      nvs_handle_t h;
      if (nvs_open(BBCLAW_WIFI_NVS_NAMESPACE, NVS_READWRITE, &h) == ESP_OK) {
        nvs_set_str(h, "last_ok", s_active_ssid);
        nvs_commit(h);
        nvs_close(h);
      }
    }
  }
}

esp_err_t bb_wifi_init_and_connect(void) {
  char ssid[sizeof(((wifi_sta_config_t*)0)->ssid)] = {0};
  char password[sizeof(((wifi_sta_config_t*)0)->password)] = {0};

  s_mode = BB_WIFI_MODE_NONE;
  ESP_RETURN_ON_ERROR(ensure_wifi_stack_ready(), TAG, "wifi stack init failed");

  /* 读取上次成功连接的 SSID，优先尝试 */
  char last_ssid[sizeof(ssid)] = {0};
  {
    nvs_handle_t h;
    if (nvs_open(BBCLAW_WIFI_NVS_NAMESPACE, NVS_READONLY, &h) == ESP_OK) {
      (void)load_nvs_string(h, "last_ok", last_ssid, sizeof(last_ssid));
      nvs_close(h);
    }
  }

  /* 如果有 last_ok，在 saved slots 里找到它优先尝试 */
  if (is_nonempty(last_ssid)) {
    for (int i = 0; i < BBCLAW_WIFI_MAX_SAVED; i++) {
      if (load_saved_wifi_slot(i, ssid, sizeof(ssid), password, sizeof(password)) == ESP_OK &&
          strcmp(ssid, last_ssid) == 0) {
        ESP_LOGI(TAG, "trying last-ok wifi slot=%d ssid=%s", i, ssid);
        if (start_sta_connection(ssid, password) == ESP_OK) {
          return ESP_OK;
        }
        ESP_LOGW(TAG, "last-ok wifi failed ssid=%s, trying others", ssid);
        break;
      }
    }
  }

  /* Try each saved slot (skip already-tried last_ok) */
  for (int i = 0; i < BBCLAW_WIFI_MAX_SAVED; i++) {
    if (load_saved_wifi_slot(i, ssid, sizeof(ssid), password, sizeof(password)) == ESP_OK && is_nonempty(ssid)) {
      if (is_nonempty(last_ssid) && strcmp(ssid, last_ssid) == 0) {
        continue; /* already tried above */
      }
      ESP_LOGI(TAG, "trying saved wifi slot=%d ssid=%s", i, ssid);
      if (start_sta_connection(ssid, password) == ESP_OK) {
        return ESP_OK;
      }
      ESP_LOGW(TAG, "wifi slot=%d failed ssid=%s", i, ssid);
    }
  }

  /* Fallback: compile-time default */
  if (is_nonempty(BBCLAW_WIFI_SSID)) {
    ESP_LOGI(TAG, "trying compile-time wifi ssid=%s", BBCLAW_WIFI_SSID);
    if (start_sta_connection(BBCLAW_WIFI_SSID, BBCLAW_WIFI_PASSWORD) == ESP_OK) {
      return ESP_OK;
    }
  }

  ESP_LOGW(TAG, "all wifi attempts failed, entering provisioning ap");
  return start_ap_provisioning_mode();
}

int bb_wifi_is_connected(void) {
  return s_connected;
}

int bb_wifi_is_provisioning_mode(void) {
  return s_mode == BB_WIFI_MODE_AP_PROVISIONING;
}

bb_wifi_mode_t bb_wifi_get_mode(void) {
  return s_mode;
}

const char* bb_wifi_get_active_ssid(void) {
  return s_active_ssid;
}

const char* bb_wifi_get_ap_ssid(void) {
  return s_ap_ssid;
}

const char* bb_wifi_get_ap_password(void) {
  return s_ap_password;
}

const char* bb_wifi_get_ap_ip(void) {
  return s_ap_ip;
}

int bb_wifi_get_rssi(void) {
  if (!s_connected) {
    return 0;
  }
  wifi_ap_record_t ap;
  if (esp_wifi_sta_get_ap_info(&ap) != ESP_OK) {
    return 0;
  }
  return ap.rssi;
}
