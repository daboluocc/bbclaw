#include "bb_ui_settings.h"

#include <stdio.h>
#include <string.h>

#include "bb_agent_client.h"
#include "bb_session_store.h"
#include "bb_ui_agent_chat.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "lvgl.h"
#include "nvs.h"
#include "nvs_flash.h"

static const char* TAG = "bb_ui_settings";

#define BB_SETTINGS_NVS_NS         "bbclaw"
#define BB_SETTINGS_NVS_KEY_TTS    "agent/tts"
#define BB_SETTINGS_SESSION_CACHE_MAX 6

#define BB_SETTINGS_FETCH_TASK_STACK 4096
#define BB_SETTINGS_FETCH_TASK_PRIO  4

/* Visual */
#define UI_BG          0x0a0e0c
#define UI_HEADER_FG   0xd8ebe4
#define UI_ROW_FG      0xa6c4ba
#define UI_ROW_SEL_FG  0xffffff
#define UI_ROW_SEL_BG  0x2ec4a0
#define UI_HINT_FG     0x7a9a8c

#define HEADER_H 22
#define ROW_H    26
#define ROWS_BOX_H (ROW_H * ROW_COUNT + 6)

typedef enum {
  ROW_SESSION = 0,
  ROW_TTS,
  ROW_BACK,
  ROW_COUNT,
} settings_row_t;

typedef struct {
  int active;
  lv_obj_t* root;
  lv_obj_t* header_lbl;
  lv_obj_t* rows[ROW_COUNT];
  int sel;

  /* Session list (populated async, per-driver). */
  bb_agent_session_info_t session_cache[BB_SETTINGS_SESSION_CACHE_MAX];
  int session_cache_count;
  int session_idx;
  volatile int session_fetch_pending;
  volatile uint32_t session_fetch_generation;

  char selected_session[64];
  int  tts_enabled;
} settings_state_t;

static settings_state_t s_st = {0};

static int s_tts_loaded = 0;

/* ── NVS helpers ── */

static esp_err_t persist_tts_enabled(int v) {
  nvs_handle_t h;
  esp_err_t err = nvs_open(BB_SETTINGS_NVS_NS, NVS_READWRITE, &h);
  if (err != ESP_OK) return err;
  err = nvs_set_u8(h, BB_SETTINGS_NVS_KEY_TTS, v ? 1 : 0);
  if (err == ESP_OK) err = nvs_commit(h);
  nvs_close(h);
  return err;
}

static void load_tts_enabled_from_nvs(void) {
  if (s_tts_loaded) return;
  s_st.tts_enabled = 1;
  nvs_handle_t h;
  esp_err_t err = nvs_open(BB_SETTINGS_NVS_NS, NVS_READONLY, &h);
  s_tts_loaded = 1;
  if (err != ESP_OK) return;
  uint8_t v = 1;
  if (nvs_get_u8(h, BB_SETTINGS_NVS_KEY_TTS, &v) == ESP_OK) {
    s_st.tts_enabled = v ? 1 : 0;
  }
  nvs_close(h);
}

void bb_ui_settings_preload_nvs(void) {
  load_tts_enabled_from_nvs();
}

/* ── Session fetch (async pipeline) ── */

typedef struct {
  uint32_t gen;
  esp_err_t err;
  int total;
  char driver_name[24];
  bb_agent_session_info_t entries[BB_SETTINGS_SESSION_CACHE_MAX];
} session_fetch_result_t;

static void apply_session_idx(void) {
  int idx = 0;
  if (s_st.selected_session[0] != '\0') {
    for (int i = 0; i < s_st.session_cache_count; ++i) {
      if (strcmp(s_st.session_cache[i].id, s_st.selected_session) == 0) {
        idx = i;
        break;
      }
    }
  }
  s_st.session_idx = idx;
}

static void apply_rows(void);  /* fwd */

static void on_session_fetch_done(void* user_data) {
  session_fetch_result_t* r = (session_fetch_result_t*)user_data;
  if (r == NULL) return;
  s_st.session_fetch_pending = 0;
  if (!s_st.active || r->gen != s_st.session_fetch_generation) {
    free(r);
    return;
  }
  if (r->err != ESP_OK || r->total < 0) {
    ESP_LOGW(TAG, "session fetch failed (%s)", esp_err_to_name(r->err));
    s_st.session_cache_count = 0;
  } else {
    int total = r->total > BB_SETTINGS_SESSION_CACHE_MAX
                  ? BB_SETTINGS_SESSION_CACHE_MAX : r->total;
    memcpy(s_st.session_cache, r->entries, sizeof(r->entries[0]) * (size_t)total);
    s_st.session_cache_count = total;
    ESP_LOGI(TAG, "session fetch ok: %d for driver '%s'", total, r->driver_name);
  }
  apply_session_idx();
  apply_rows();
  free(r);
}

static void session_fetch_task(void* arg) {
  session_fetch_result_t* r = (session_fetch_result_t*)arg;
  if (r == NULL) {
    s_st.session_fetch_pending = 0;
    vTaskDelete(NULL);
    return;
  }
  r->err = bb_agent_list_sessions(r->driver_name, r->entries, BB_SETTINGS_SESSION_CACHE_MAX, &r->total);
  lv_async_call(on_session_fetch_done, r);
  vTaskDelete(NULL);
}

static void spawn_session_fetch_task(void) {
  if (s_st.session_fetch_pending) return;
  const char* driver = bb_ui_agent_chat_get_current_driver();
  if (driver == NULL || driver[0] == '\0') {
    s_st.session_cache_count = 0;
    return;
  }

  s_st.session_fetch_pending = 1;
  uint32_t gen = ++s_st.session_fetch_generation;

  session_fetch_result_t* r = (session_fetch_result_t*)calloc(1, sizeof(*r));
  if (r == NULL) {
    ESP_LOGE(TAG, "spawn_session_fetch_task: calloc failed");
    s_st.session_fetch_pending = 0;
    return;
  }

  r->gen = gen;
  strncpy(r->driver_name, driver, sizeof(r->driver_name) - 1);

  TaskHandle_t t = NULL;
  BaseType_t ok = xTaskCreate(session_fetch_task, "session_fetch",
                              BB_SETTINGS_FETCH_TASK_STACK, r,
                              BB_SETTINGS_FETCH_TASK_PRIO, &t);
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "spawn_session_fetch_task: xTaskCreate failed");
    s_st.session_fetch_pending = 0;
    free(r);
  }
}

/* ── Rendering ── */

static const char* active_session_preview(void) {
  if (s_st.session_idx == s_st.session_cache_count) {
    return "(new session)";
  }
  if (s_st.session_cache_count <= 0) return "(none)";
  if (s_st.session_idx < 0 || s_st.session_idx >= s_st.session_cache_count) return "(none)";
  const char* t = s_st.session_cache[s_st.session_idx].title;
  return (t != NULL && t[0] != '\0') ? t : "(unnamed)";
}

static void apply_rows(void) {
  if (s_st.root == NULL) return;
  char buf[80];

  /* Row 0: Session */
  if (s_st.rows[ROW_SESSION] != NULL) {
    if (s_st.session_fetch_pending) {
      snprintf(buf, sizeof(buf), "Session: loading...");
    } else {
      int total = s_st.session_cache_count + 1;
      snprintf(buf, sizeof(buf), "Session: %s (%d/%d)", active_session_preview(),
               s_st.session_idx + 1, total);
    }
    lv_label_set_text(s_st.rows[ROW_SESSION], buf);
  }

  /* Row 1: TTS */
  if (s_st.rows[ROW_TTS] != NULL) {
    snprintf(buf, sizeof(buf), "TTS reply: %s", s_st.tts_enabled ? "On" : "Off");
    lv_label_set_text(s_st.rows[ROW_TTS], buf);
  }

  /* Row 2: Back */
  if (s_st.rows[ROW_BACK] != NULL) {
    lv_label_set_text(s_st.rows[ROW_BACK], "Back");
  }

  /* Highlight selected row. */
  for (int i = 0; i < ROW_COUNT; ++i) {
    lv_obj_t* row = s_st.rows[i];
    if (row == NULL) continue;
    if (i == s_st.sel) {
      lv_obj_set_style_bg_color(row, lv_color_hex(UI_ROW_SEL_BG), 0);
      lv_obj_set_style_bg_opa(row, LV_OPA_50, 0);
      lv_obj_set_style_text_color(row, lv_color_hex(UI_ROW_SEL_FG), 0);
    } else {
      lv_obj_set_style_bg_opa(row, LV_OPA_TRANSP, 0);
      lv_obj_set_style_text_color(row, lv_color_hex(UI_ROW_FG), 0);
    }
  }
}

/* ── Public API ── */

void bb_ui_settings_show(lv_obj_t* parent) {
  if (parent == NULL) {
    ESP_LOGE(TAG, "show: parent NULL");
    return;
  }
  if (s_st.active) {
    ESP_LOGW(TAG, "show: already active, ignoring");
    return;
  }

  /* Snapshot persisted prefs into editable state. */

  /* Load current session from NVS for current driver. */
  const char* driver = bb_ui_agent_chat_get_current_driver();
  s_st.selected_session[0] = '\0';
  if (driver != NULL && driver[0] != '\0') {
    bb_session_store_load(driver, s_st.selected_session, sizeof(s_st.selected_session));
  }
  s_st.session_cache_count = 0;
  s_st.session_idx = 0;

  s_st.root = lv_obj_create(parent);
  lv_obj_remove_style_all(s_st.root);
  lv_obj_set_size(s_st.root, lv_pct(100), lv_pct(100));
  lv_obj_set_style_bg_color(s_st.root, lv_color_hex(UI_BG), 0);
  lv_obj_set_style_bg_opa(s_st.root, LV_OPA_COVER, 0);
  lv_obj_clear_flag(s_st.root, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_move_foreground(s_st.root);

  /* Header */
  s_st.header_lbl = lv_label_create(s_st.root);
  lv_obj_set_size(s_st.header_lbl, lv_pct(100), HEADER_H);
  lv_obj_align(s_st.header_lbl, LV_ALIGN_TOP_LEFT, 0, 0);
  lv_obj_set_style_text_color(s_st.header_lbl, lv_color_hex(UI_HEADER_FG), 0);
  lv_obj_set_style_pad_left(s_st.header_lbl, 6, 0);
  lv_obj_set_style_pad_top(s_st.header_lbl, 4, 0);
  lv_label_set_text(s_st.header_lbl, "Settings");

  /* Rows container — absolute height so the 4 rows always fit. lv_pct minus
   * an integer is NOT supported in LVGL 9 (lv_pct returns a special-encoded
   * coord, not a regular pixel count), so use ROWS_BOX_H directly. */
  lv_obj_t* rows_box = lv_obj_create(s_st.root);
  lv_obj_remove_style_all(rows_box);
  lv_obj_set_size(rows_box, lv_pct(100), ROWS_BOX_H);
  lv_obj_align(rows_box, LV_ALIGN_TOP_LEFT, 0, HEADER_H);
  lv_obj_set_flex_flow(rows_box, LV_FLEX_FLOW_COLUMN);
  lv_obj_set_style_pad_all(rows_box, 4, 0);
  lv_obj_clear_flag(rows_box, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_set_style_bg_opa(rows_box, LV_OPA_TRANSP, 0);

  for (int i = 0; i < ROW_COUNT; ++i) {
    lv_obj_t* row = lv_label_create(rows_box);
    lv_obj_set_size(row, lv_pct(100), ROW_H);
    lv_obj_set_style_pad_left(row, 6, 0);
    lv_obj_set_style_pad_right(row, 6, 0);
    lv_obj_set_style_pad_top(row, 4, 0);
    lv_obj_set_style_radius(row, 3, 0);
    lv_obj_set_style_text_color(row, lv_color_hex(UI_ROW_FG), 0);
    lv_label_set_long_mode(row, LV_LABEL_LONG_MODE_DOTS);
    s_st.rows[i] = row;
  }

  s_st.sel = ROW_SESSION;
  s_st.active = 1;

  /* Kick off async session fetch — apply_rows will re-render when done. */
  spawn_session_fetch_task();
  apply_rows();

  ESP_LOGI(TAG, "show (driver='%s', tts=%d)",
           bb_ui_agent_chat_get_current_driver(), s_st.tts_enabled);
}

void bb_ui_settings_hide(void) {
  if (!s_st.active) return;
  s_st.active = 0;
  s_st.session_fetch_generation++;
  if (s_st.root != NULL) {
    lv_obj_del(s_st.root);
    s_st.root = NULL;
  }
  s_st.header_lbl = NULL;
  for (int i = 0; i < ROW_COUNT; ++i) s_st.rows[i] = NULL;
  ESP_LOGI(TAG, "hide");
}

int bb_ui_settings_is_active(void) {
  return s_st.active ? 1 : 0;
}

void bb_ui_settings_handle_rotate(int delta) {
  if (!s_st.active || delta == 0) return;
  int next = s_st.sel + delta;
  if (next < 0) next = 0;
  if (next >= ROW_COUNT) next = ROW_COUNT - 1;
  if (next == s_st.sel) return;
  s_st.sel = next;
  apply_rows();
}

/* LEFT/RIGHT — preview the current row's value (no NVS write).
 *
 * Phase 4.7.2: auto-save model (4.7.1) was reverted because rapid LEFT/RIGHT
 * cycling triggered NVS writes inside the LVGL lock, which appeared to
 * cause device restarts (likely flash IO + LVGL drawing contention). Back
 * to "preview on LEFT/RIGHT, commit on OK" — explicit and safer.
 */
void bb_ui_settings_handle_value(int delta) {
  if (!s_st.active || delta == 0) return;
  switch ((settings_row_t)s_st.sel) {
    case ROW_SESSION: {
      int total = s_st.session_cache_count + 1;
      if (total <= 1) return;
      int next = ((s_st.session_idx + delta) % total + total) % total;
      s_st.session_idx = next;
      break;
    }
    case ROW_TTS: {
      s_st.tts_enabled = !s_st.tts_enabled;
      break;
    }
    case ROW_BACK:
    case ROW_COUNT:
    default:
      return;
  }
  apply_rows();
}

/* OK — commit the previewed value for the current row, then advance cursor.
 * On Back row, OK exits the overlay. */
void bb_ui_settings_handle_click(void) {
  if (!s_st.active) return;
  switch ((settings_row_t)s_st.sel) {
    case ROW_SESSION: {
      /* Resolve session_idx to session ID */
      if (s_st.session_idx == s_st.session_cache_count) {
        /* "(new session)" selected — create a new logical session via adapter.
         * ADR-014: POST /v1/agent/sessions with current driver, no cwd. */
        const char* driver = bb_ui_agent_chat_get_current_driver();
        char new_sid[64] = {0};
        esp_err_t create_err = bb_agent_create_session(driver, NULL, new_sid, sizeof(new_sid));
        if (create_err == ESP_OK && new_sid[0] != '\0') {
          strncpy(s_st.selected_session, new_sid, sizeof(s_st.selected_session) - 1);
          s_st.selected_session[sizeof(s_st.selected_session) - 1] = '\0';
          bb_session_store_save(driver, new_sid);
          ESP_LOGI(TAG, "new session created -> '%s' for driver '%s'", new_sid, driver);
        } else {
          ESP_LOGW(TAG, "new session create failed (%s), clearing session", esp_err_to_name(create_err));
          s_st.selected_session[0] = '\0';
          bb_session_store_save(driver, s_st.selected_session);
        }
      } else if (s_st.session_idx >= 0 && s_st.session_idx < s_st.session_cache_count) {
        strncpy(s_st.selected_session, s_st.session_cache[s_st.session_idx].id,
                sizeof(s_st.selected_session) - 1);
        s_st.selected_session[sizeof(s_st.selected_session) - 1] = '\0';
        const char* driver = bb_ui_agent_chat_get_current_driver();
        bb_session_store_save(driver, s_st.selected_session);
        ESP_LOGI(TAG, "session -> '%s' for driver '%s' (committed)",
                 s_st.selected_session, driver);
      }
      s_st.sel = ROW_TTS;
      apply_rows();
      break;
    }
    case ROW_TTS: {
      esp_err_t err = persist_tts_enabled(s_st.tts_enabled);
      if (err != ESP_OK) {
        ESP_LOGW(TAG, "persist tts '%s' failed (%s)",
                 s_st.tts_enabled ? "on" : "off", esp_err_to_name(err));
      }
      ESP_LOGI(TAG, "tts -> %s (committed)", s_st.tts_enabled ? "on" : "off");
      s_st.sel = ROW_BACK;
      apply_rows();
      break;
    }
    case ROW_BACK:
      bb_ui_settings_hide();
      break;
    case ROW_COUNT:
    default: break;
  }
}

int bb_ui_settings_tts_enabled(void) {
  /* Lazy-load: if the user hasn't opened Settings yet this boot, s_st.tts_enabled
   * was never explicitly loaded from NVS. Populate it now (load also sets the
   * default ON if no NVS entry exists). After the first call this is a single
   * read of the cached value. */
  if (!s_tts_loaded) {
    load_tts_enabled_from_nvs();
  }
  return s_st.tts_enabled ? 1 : 0;
}
