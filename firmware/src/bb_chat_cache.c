/**
 * ADR-017 chat cache — see header for design notes.
 *
 * Memory model:
 *   - One in-memory ring of message records (s_buf) keyed by the active
 *     (driver, sid). Cap is BB_CACHE_BLOB_CAP bytes total.
 *   - Streaming assistant chunks accumulate in s_pending until finalize.
 *   - Each finalized append schedules a persist task that serializes the
 *     ring + sid into a single NVS blob.
 *
 * The buffer is intentionally simple — a single byte array with messages
 * stored back-to-back as (role uint8, len uint16, content[len]). Eviction
 * removes whole messages from the head until the new tail fits.
 */
#include "bb_chat_cache.h"

#include <stdlib.h>
#include <string.h>

#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "freertos/task.h"
#include "nvs.h"

static const char* TAG = "bb_chat_cache";

#define BB_CACHE_NVS_NS         "bbclaw"
#define BB_CACHE_BLOB_CAP       1536U   /* per-driver budget */
#define BB_CACHE_MSG_MAX_LEN    480U    /* per-message content cap */
#define BB_CACHE_PENDING_CAP    4096U   /* streaming accumulator */
#define BB_CACHE_PERSIST_STACK  4096
#define BB_CACHE_PERSIST_PRIO   3

/* Magic + version are baked into the blob so a future format change can
 * detect and ignore stale layouts instead of mis-decoding. */
#define BB_CACHE_MAGIC  0xBB17CACEU
#define BB_CACHE_VERSION 1U

/* Driver name → short NVS-key suffix. Mirrors bb_session_store's mapping
 * so the two stores stay symmetric and readable when dumping NVS. */
typedef struct {
  const char* driver_name;
  const char* nvs_key;
} driver_key_map_t;

static const driver_key_map_t s_driver_map[] = {
  {"claude-code", "cc/cc"},
  {"opencode",    "cc/oc"},
  {"openclaw",    "cc/op"},
  {"ollama",      "cc/ol"},
  {"aider",       "cc/ai"},
};

static const char* driver_to_key(const char* driver_name) {
  if (driver_name == NULL) return NULL;
  for (size_t i = 0; i < sizeof(s_driver_map) / sizeof(s_driver_map[0]); ++i) {
    if (strcmp(driver_name, s_driver_map[i].driver_name) == 0) {
      return s_driver_map[i].nvs_key;
    }
  }
  return NULL;
}

/* Bound state. Mutated only on the LVGL task (transcript callbacks). */
static char s_driver[24];
static char s_sid[64];
static uint8_t s_buf[BB_CACHE_BLOB_CAP];
static size_t s_buf_used;
static char* s_pending;
static size_t s_pending_len;
static int s_dirty;
static int s_persist_inflight;
static SemaphoreHandle_t s_persist_mutex;  /* serializes persist tasks */

/* ─── helpers ─── */

static void buf_reset(void) {
  s_buf_used = 0;
}

/* Walk the buffer skipping the first `n_msgs` records. Returns offset of
 * the byte just past the last skipped message. */
static size_t skip_messages(size_t n_msgs) {
  size_t off = 0;
  for (size_t i = 0; i < n_msgs && off + 3 <= s_buf_used; ++i) {
    uint16_t len = (uint16_t)s_buf[off + 1] | ((uint16_t)s_buf[off + 2] << 8);
    size_t step = (size_t)3 + len;
    if (off + step > s_buf_used) break;
    off += step;
  }
  return off;
}

static size_t count_messages(void) {
  size_t off = 0;
  size_t n = 0;
  while (off + 3 <= s_buf_used) {
    uint16_t len = (uint16_t)s_buf[off + 1] | ((uint16_t)s_buf[off + 2] << 8);
    if (off + 3 + len > s_buf_used) break;
    off += 3 + len;
    n++;
  }
  return n;
}

/* Evict whole messages from the head until the requested payload fits. */
static void evict_to_fit(size_t want) {
  if (want > BB_CACHE_BLOB_CAP) return;  /* caller must clamp */
  while (s_buf_used + want > BB_CACHE_BLOB_CAP) {
    size_t one = skip_messages(1);
    if (one == 0) break;  /* malformed buffer — bail out without infinite loop */
    memmove(s_buf, s_buf + one, s_buf_used - one);
    s_buf_used -= one;
  }
}

/* Append one record. Truncates content > BB_CACHE_MSG_MAX_LEN. */
static void buf_append(char role, const char* content) {
  if (content == NULL) return;
  size_t len = strlen(content);
  if (len == 0) return;
  if (len > BB_CACHE_MSG_MAX_LEN) len = BB_CACHE_MSG_MAX_LEN;
  size_t need = 3 + len;
  if (need > BB_CACHE_BLOB_CAP) return;
  evict_to_fit(need);
  s_buf[s_buf_used + 0] = (uint8_t)role;
  s_buf[s_buf_used + 1] = (uint8_t)(len & 0xFF);
  s_buf[s_buf_used + 2] = (uint8_t)((len >> 8) & 0xFF);
  memcpy(&s_buf[s_buf_used + 3], content, len);
  s_buf_used += need;
  s_dirty = 1;
}

/* ─── persistence ─── */

typedef struct {
  char driver[24];
  char sid[64];
  uint8_t blob[BB_CACHE_BLOB_CAP + 96];  /* header + sid + buf */
  size_t blob_len;
} persist_payload_t;

static size_t serialize(persist_payload_t* p) {
  size_t off = 0;
  uint32_t magic = BB_CACHE_MAGIC;
  uint32_t version = BB_CACHE_VERSION;
  memcpy(&p->blob[off], &magic, 4);   off += 4;
  memcpy(&p->blob[off], &version, 4); off += 4;
  uint16_t sid_len = (uint16_t)strnlen(p->sid, sizeof(p->sid));
  memcpy(&p->blob[off], &sid_len, 2); off += 2;
  if (sid_len > 0) { memcpy(&p->blob[off], p->sid, sid_len); off += sid_len; }
  uint16_t buf_len = (uint16_t)(p->blob_len);  /* repurposed slot below */
  /* placeholder; rewritten after copying */
  size_t buf_len_off = off;
  memcpy(&p->blob[off], &buf_len, 2); off += 2;
  size_t bytes = s_buf_used;
  memcpy(&p->blob[off], s_buf, bytes);
  buf_len = (uint16_t)bytes;
  memcpy(&p->blob[buf_len_off], &buf_len, 2);
  off += bytes;
  return off;
}

static int deserialize(const uint8_t* data, size_t len,
                       char* sid_out, size_t sid_cap) {
  if (data == NULL || len < 12) return -1;
  uint32_t magic, version;
  memcpy(&magic, data + 0, 4);
  memcpy(&version, data + 4, 4);
  if (magic != BB_CACHE_MAGIC || version != BB_CACHE_VERSION) return -1;
  size_t off = 8;
  uint16_t sid_len;
  memcpy(&sid_len, data + off, 2); off += 2;
  if (off + sid_len > len) return -1;
  if (sid_out != NULL && sid_cap > 0) {
    size_t cap = sid_len < sid_cap - 1 ? sid_len : sid_cap - 1;
    memcpy(sid_out, data + off, cap);
    sid_out[cap] = '\0';
  }
  off += sid_len;
  if (off + 2 > len) return -1;
  uint16_t buf_len;
  memcpy(&buf_len, data + off, 2); off += 2;
  if (off + buf_len > len || buf_len > BB_CACHE_BLOB_CAP) return -1;
  memcpy(s_buf, data + off, buf_len);
  s_buf_used = buf_len;
  return 0;
}

static void persist_task(void* arg) {
  persist_payload_t* p = (persist_payload_t*)arg;
  if (p == NULL) { vTaskDelete(NULL); return; }
  if (s_persist_mutex != NULL) xSemaphoreTake(s_persist_mutex, portMAX_DELAY);

  const char* key = driver_to_key(p->driver);
  if (key == NULL) {
    ESP_LOGW(TAG, "persist: unknown driver '%s'", p->driver);
    goto done;
  }
  nvs_handle_t h;
  esp_err_t err = nvs_open(BB_CACHE_NVS_NS, NVS_READWRITE, &h);
  if (err != ESP_OK) {
    ESP_LOGW(TAG, "persist: nvs_open failed (%s)", esp_err_to_name(err));
    goto done;
  }
  err = nvs_set_blob(h, key, p->blob, p->blob_len);
  if (err == ESP_OK) err = nvs_commit(h);
  nvs_close(h);
  if (err != ESP_OK) {
    ESP_LOGW(TAG, "persist: write failed (%s)", esp_err_to_name(err));
  } else {
    ESP_LOGD(TAG, "persist: '%s' %u bytes (sid='%s')", key,
             (unsigned)p->blob_len, p->sid);
  }
done:
  s_persist_inflight = 0;
  if (s_persist_mutex != NULL) xSemaphoreGive(s_persist_mutex);
  free(p);
  vTaskDelete(NULL);
}

static void schedule_persist(void) {
  if (!s_dirty || s_driver[0] == '\0') return;
  if (s_persist_inflight) return;  /* coalesce — task will pick up latest state */
  persist_payload_t* p = (persist_payload_t*)calloc(1, sizeof(*p));
  if (p == NULL) {
    ESP_LOGW(TAG, "schedule_persist: oom");
    return;
  }
  strncpy(p->driver, s_driver, sizeof(p->driver) - 1);
  strncpy(p->sid, s_sid, sizeof(p->sid) - 1);
  p->blob_len = serialize(p);
  s_dirty = 0;
  s_persist_inflight = 1;
  BaseType_t ok = xTaskCreate(persist_task, "chat_cache_w",
                              BB_CACHE_PERSIST_STACK, p,
                              BB_CACHE_PERSIST_PRIO, NULL);
  if (ok != pdPASS) {
    ESP_LOGW(TAG, "schedule_persist: xTaskCreate failed");
    s_persist_inflight = 0;
    free(p);
  }
}

/* Read NVS blob for `driver` into s_buf. On success, sets sid_out to the
 * cached sid. */
static int load_blob(const char* driver, char* sid_out, size_t sid_cap) {
  const char* key = driver_to_key(driver);
  if (key == NULL) return -1;
  nvs_handle_t h;
  esp_err_t err = nvs_open(BB_CACHE_NVS_NS, NVS_READONLY, &h);
  if (err != ESP_OK) return -1;
  size_t blob_len = 0;
  err = nvs_get_blob(h, key, NULL, &blob_len);
  if (err != ESP_OK || blob_len == 0 || blob_len > sizeof(((persist_payload_t*)0)->blob)) {
    nvs_close(h);
    return -1;
  }
  uint8_t* tmp = (uint8_t*)malloc(blob_len);
  if (tmp == NULL) { nvs_close(h); return -1; }
  err = nvs_get_blob(h, key, tmp, &blob_len);
  nvs_close(h);
  if (err != ESP_OK) { free(tmp); return -1; }
  int rc = deserialize(tmp, blob_len, sid_out, sid_cap);
  free(tmp);
  return rc;
}

/* ─── public API ─── */

void bb_chat_cache_init(void) {
  if (s_persist_mutex == NULL) {
    s_persist_mutex = xSemaphoreCreateMutex();
  }
}

void bb_chat_cache_bind(const char* driver_name, const char* session_id) {
  if (driver_name == NULL) driver_name = "";
  if (session_id == NULL) session_id = "";
  if (strcmp(s_driver, driver_name) == 0 && strcmp(s_sid, session_id) == 0) {
    return;  /* unchanged */
  }
  strncpy(s_driver, driver_name, sizeof(s_driver) - 1);
  s_driver[sizeof(s_driver) - 1] = '\0';
  strncpy(s_sid, session_id, sizeof(s_sid) - 1);
  s_sid[sizeof(s_sid) - 1] = '\0';
  buf_reset();
  if (s_pending != NULL) { s_pending[0] = '\0'; s_pending_len = 0; }
  s_dirty = 0;
}

int bb_chat_cache_hydrate_from_nvs(void) {
  if (s_driver[0] == '\0' || s_sid[0] == '\0') return -1;
  char stored_sid[64] = {0};
  if (load_blob(s_driver, stored_sid, sizeof(stored_sid)) != 0) return -1;
  if (strcmp(stored_sid, s_sid) != 0) {
    ESP_LOGI(TAG, "hydrate: driver=%s blob sid='%s' != active '%s', skip",
             s_driver, stored_sid, s_sid);
    buf_reset();
    s_dirty = 1;
    return -1;
  }
  ESP_LOGI(TAG, "hydrate: %u bytes (driver=%s sid=%s)",
           (unsigned)s_buf_used, s_driver, s_sid);
  return 0;
}

void bb_chat_cache_append_user(const char* text) {
  buf_append(BB_CHAT_CACHE_ROLE_USER, text);
  schedule_persist();
}

void bb_chat_cache_append_tool(const char* tool, const char* hint) {
  char buf[256];
  if (hint != NULL && hint[0] != '\0') {
    snprintf(buf, sizeof(buf), "%s: %s", tool ? tool : "tool", hint);
  } else {
    snprintf(buf, sizeof(buf), "%s", tool ? tool : "tool");
  }
  buf_append(BB_CHAT_CACHE_ROLE_TOOL, buf);
  schedule_persist();
}

void bb_chat_cache_append_error(const char* msg) {
  buf_append(BB_CHAT_CACHE_ROLE_ERROR, msg != NULL ? msg : "error");
  schedule_persist();
}

void bb_chat_cache_append_assistant_chunk(const char* delta) {
  if (delta == NULL || delta[0] == '\0') return;
  if (s_pending == NULL) {
    s_pending = (char*)malloc(BB_CACHE_PENDING_CAP);
    if (s_pending == NULL) return;
    s_pending[0] = '\0';
    s_pending_len = 0;
  }
  size_t add = strlen(delta);
  if (s_pending_len + add + 1 > BB_CACHE_PENDING_CAP) {
    /* Pending overflow — flush what we have, then start a new pending so
     * the rest still lands in the cache (DOT-truncated in transcript, but
     * stored fully here). */
    if (s_pending_len > 0) {
      buf_append(BB_CHAT_CACHE_ROLE_ASSISTANT, s_pending);
      s_pending_len = 0;
      s_pending[0] = '\0';
    }
    if (add + 1 > BB_CACHE_PENDING_CAP) add = BB_CACHE_PENDING_CAP - 1;
  }
  memcpy(s_pending + s_pending_len, delta, add);
  s_pending_len += add;
  s_pending[s_pending_len] = '\0';
}

void bb_chat_cache_finalize_assistant(void) {
  if (s_pending != NULL && s_pending_len > 0) {
    buf_append(BB_CHAT_CACHE_ROLE_ASSISTANT, s_pending);
    s_pending_len = 0;
    s_pending[0] = '\0';
    schedule_persist();
  }
}

void bb_chat_cache_replay(bb_chat_cache_replay_cb cb, void* user) {
  if (cb == NULL || s_buf_used == 0) return;
  size_t off = 0;
  size_t n = 0;
  while (off + 3 <= s_buf_used) {
    char role = (char)s_buf[off];
    uint16_t len = (uint16_t)s_buf[off + 1] | ((uint16_t)s_buf[off + 2] << 8);
    if (off + 3 + len > s_buf_used) break;
    /* Copy into a temporary NUL-terminated buffer so the callback can use
     * string APIs. Max BB_CACHE_MSG_MAX_LEN, allocated on stack. */
    char tmp[BB_CACHE_MSG_MAX_LEN + 1];
    size_t copy = len < sizeof(tmp) - 1 ? len : sizeof(tmp) - 1;
    memcpy(tmp, s_buf + off + 3, copy);
    tmp[copy] = '\0';
    cb(role, tmp, user);
    off += 3 + len;
    n++;
  }
  ESP_LOGD(TAG, "replay: %u messages", (unsigned)n);
  (void)count_messages;
}

void bb_chat_cache_clear(void) {
  buf_reset();
  if (s_pending != NULL) { s_pending[0] = '\0'; s_pending_len = 0; }
  s_dirty = 1;
  schedule_persist();
}

int bb_chat_cache_has_data(void) {
  return s_buf_used > 0 ? 1 : 0;
}
