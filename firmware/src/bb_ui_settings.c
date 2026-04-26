#include "bb_ui_settings.h"

#include <stdio.h>
#include <string.h>

#include "bb_agent_client.h"
#include "bb_agent_theme.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "lvgl.h"
#include "nvs.h"
#include "nvs_flash.h"

static const char* TAG = "bb_ui_settings";

/*
 * Phase 4.7 — standalone Settings overlay.
 *
 * Architecture mirrors the Phase 4.2.5 settings sub-mode that used to live
 * inside bb_ui_agent_chat. The big difference is layout: we render full-
 * screen on a dedicated parent (not as a bottom strip on top of the chat
 * theme), and we own our own state — the chat module no longer has any
 * settings code.
 *
 * NVS keys are SHARED with bb_ui_agent_chat:
 *   namespace "bbclaw":
 *     "agent/driver" → string, user-selected driver name (empty = adapter default)
 *     "agent/theme"  → string, active theme name (consumed by bb_agent_theme)
 *     "agent/tts"    → u8, 0/1 TTS-reply toggle
 *
 * The chat module reads these on chat-show; settings writes them when the
 * user commits a row. Mutually exclusive overlays guarantee no concurrent
 * access. After settings is dismissed, the next chat-show picks up new
 * values.
 */

#define BB_SETTINGS_NVS_NS         "bbclaw"
#define BB_SETTINGS_NVS_KEY_DRIVER "agent/driver"
#define BB_SETTINGS_NVS_KEY_TTS    "agent/tts"
#define BB_SETTINGS_DRIVER_FALLBACK "claude-code"
#define BB_SETTINGS_DRIVER_CACHE_MAX 6

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
#define ROW_H    24

typedef enum {
  ROW_AGENT = 0,
  ROW_THEME,
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

  /* Driver list (populated async). */
  bb_agent_driver_info_t driver_cache[BB_SETTINGS_DRIVER_CACHE_MAX];
  int driver_cache_count;
  int driver_cache_offline;
  int driver_idx;
  volatile int driver_fetch_pending;
  volatile uint32_t driver_fetch_generation;

  /* Theme list (in-process, sync). */
  const char* const* theme_list;
  int theme_count;
  int theme_idx;

  /* Editable values (snapshot at show; committed on click). */
  char selected_driver[24];  /* "" = adapter default */
  int  tts_enabled;          /* 1 = on */
} settings_state_t;

static settings_state_t s_st = {0};

/* ── NVS helpers ── */

static esp_err_t persist_selected_driver(const char* name) {
  if (name == NULL) return ESP_ERR_INVALID_ARG;
  nvs_handle_t h;
  esp_err_t err = nvs_open(BB_SETTINGS_NVS_NS, NVS_READWRITE, &h);
  if (err != ESP_OK) return err;
  err = nvs_set_str(h, BB_SETTINGS_NVS_KEY_DRIVER, name);
  if (err == ESP_OK) err = nvs_commit(h);
  nvs_close(h);
  return err;
}

static void load_selected_driver_from_nvs(void) {
  s_st.selected_driver[0] = '\0';
  nvs_handle_t h;
  esp_err_t err = nvs_open(BB_SETTINGS_NVS_NS, NVS_READONLY, &h);
  if (err != ESP_OK) return;
  size_t sz = sizeof(s_st.selected_driver);
  err = nvs_get_str(h, BB_SETTINGS_NVS_KEY_DRIVER, s_st.selected_driver, &sz);
  nvs_close(h);
  if (err == ESP_OK) {
    ESP_LOGI(TAG, "selected_driver loaded: '%s'", s_st.selected_driver);
  }
}

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
  s_st.tts_enabled = 1;  /* default ON */
  nvs_handle_t h;
  if (nvs_open(BB_SETTINGS_NVS_NS, NVS_READONLY, &h) != ESP_OK) return;
  uint8_t v = 1;
  if (nvs_get_u8(h, BB_SETTINGS_NVS_KEY_TTS, &v) == ESP_OK) {
    s_st.tts_enabled = v ? 1 : 0;
  }
  nvs_close(h);
}

/* ── Driver fetch (async pipeline; mirrors Phase 4.2.5) ── */

typedef struct {
  uint32_t gen;
  esp_err_t err;
  int total;
  bb_agent_driver_info_t entries[BB_SETTINGS_DRIVER_CACHE_MAX];
} fetch_result_t;

static void apply_driver_idx(void) {
  int idx = 0;
  if (s_st.selected_driver[0] != '\0') {
    for (int i = 0; i < s_st.driver_cache_count; ++i) {
      if (strcmp(s_st.driver_cache[i].name, s_st.selected_driver) == 0) {
        idx = i;
        break;
      }
    }
  }
  s_st.driver_idx = idx;
}

static void apply_rows(void);  /* fwd */

static void on_fetch_done(void* user_data) {
  fetch_result_t* r = (fetch_result_t*)user_data;
  if (r == NULL) return;
  s_st.driver_fetch_pending = 0;
  if (!s_st.active || r->gen != s_st.driver_fetch_generation) {
    /* Stale or overlay closed — drop. */
    free(r);
    return;
  }
  if (r->err != ESP_OK || r->total <= 0) {
    ESP_LOGW(TAG, "driver fetch failed (%s), fallback to '%s'",
             esp_err_to_name(r->err), BB_SETTINGS_DRIVER_FALLBACK);
    memset(&s_st.driver_cache[0], 0, sizeof(s_st.driver_cache[0]));
    strncpy(s_st.driver_cache[0].name, BB_SETTINGS_DRIVER_FALLBACK,
            sizeof(s_st.driver_cache[0].name) - 1);
    s_st.driver_cache_count = 1;
    s_st.driver_cache_offline = 1;
  } else {
    int total = r->total > BB_SETTINGS_DRIVER_CACHE_MAX
                  ? BB_SETTINGS_DRIVER_CACHE_MAX : r->total;
    memcpy(s_st.driver_cache, r->entries, sizeof(r->entries[0]) * (size_t)total);
    s_st.driver_cache_count = total;
    s_st.driver_cache_offline = 0;
    ESP_LOGI(TAG, "driver fetch ok: %d", total);
  }
  apply_driver_idx();
  apply_rows();
  free(r);
}

static void fetch_task(void* arg) {
  uint32_t my_gen = (uint32_t)(uintptr_t)arg;
  fetch_result_t* r = (fetch_result_t*)calloc(1, sizeof(*r));
  if (r == NULL) {
    s_st.driver_fetch_pending = 0;
    vTaskDelete(NULL);
    return;
  }
  r->gen = my_gen;
  r->err = bb_agent_list_drivers(r->entries, BB_SETTINGS_DRIVER_CACHE_MAX, &r->total);
  lv_async_call(on_fetch_done, r);
  vTaskDelete(NULL);
}

static void spawn_fetch_task(void) {
  if (s_st.driver_cache_count > 0) return;
  if (s_st.driver_fetch_pending) return;
  s_st.driver_fetch_pending = 1;
  uint32_t gen = ++s_st.driver_fetch_generation;
  TaskHandle_t t = NULL;
  BaseType_t ok = xTaskCreate(fetch_task, "settings_fetch",
                              BB_SETTINGS_FETCH_TASK_STACK,
                              (void*)(uintptr_t)gen,
                              BB_SETTINGS_FETCH_TASK_PRIO, &t);
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "spawn_fetch_task: xTaskCreate failed");
    s_st.driver_fetch_pending = 0;
  }
}

/* ── Theme list ── */

static void load_theme_list(void) {
  int n = 0;
  s_st.theme_list = bb_agent_theme_list(&n);
  s_st.theme_count = n;
  const bb_agent_theme_t* cur = bb_agent_theme_get_active();
  int idx = 0;
  if (cur != NULL && cur->name != NULL && s_st.theme_list != NULL) {
    for (int i = 0; i < n; ++i) {
      if (s_st.theme_list[i] != NULL && strcmp(s_st.theme_list[i], cur->name) == 0) {
        idx = i;
        break;
      }
    }
  }
  s_st.theme_idx = idx;
}

/* ── Rendering ── */

static const char* active_driver_label(void) {
  if (s_st.driver_cache_count <= 0) return "(none)";
  if (s_st.driver_idx < 0 || s_st.driver_idx >= s_st.driver_cache_count) return "(none)";
  const char* n = s_st.driver_cache[s_st.driver_idx].name;
  return (n != NULL && n[0] != '\0') ? n : "(unnamed)";
}

static const char* active_theme_label(void) {
  if (s_st.theme_count <= 0 || s_st.theme_list == NULL) return "(none)";
  if (s_st.theme_idx < 0 || s_st.theme_idx >= s_st.theme_count) return "(none)";
  const char* n = s_st.theme_list[s_st.theme_idx];
  return (n != NULL && n[0] != '\0') ? n : "(unnamed)";
}

static void apply_rows(void) {
  if (s_st.root == NULL) return;
  char buf[80];

  /* Row 0: Agent */
  if (s_st.rows[ROW_AGENT] != NULL) {
    if (s_st.driver_cache_count <= 0 && s_st.driver_fetch_pending) {
      snprintf(buf, sizeof(buf), "Agent: loading...");
    } else if (s_st.driver_cache_offline) {
      snprintf(buf, sizeof(buf), "Agent: %s (offline)", active_driver_label());
    } else if (s_st.driver_cache_count > 1) {
      snprintf(buf, sizeof(buf), "Agent: %s (%d/%d)", active_driver_label(),
               s_st.driver_idx + 1, s_st.driver_cache_count);
    } else {
      snprintf(buf, sizeof(buf), "Agent: %s", active_driver_label());
    }
    lv_label_set_text(s_st.rows[ROW_AGENT], buf);
  }

  /* Row 1: Theme */
  if (s_st.rows[ROW_THEME] != NULL) {
    if (s_st.theme_count > 1) {
      snprintf(buf, sizeof(buf), "Theme: %s (%d/%d)", active_theme_label(),
               s_st.theme_idx + 1, s_st.theme_count);
    } else {
      snprintf(buf, sizeof(buf), "Theme: %s", active_theme_label());
    }
    lv_label_set_text(s_st.rows[ROW_THEME], buf);
  }

  /* Row 2: TTS */
  if (s_st.rows[ROW_TTS] != NULL) {
    snprintf(buf, sizeof(buf), "TTS reply: %s", s_st.tts_enabled ? "On" : "Off");
    lv_label_set_text(s_st.rows[ROW_TTS], buf);
  }

  /* Row 3: Back */
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
  load_selected_driver_from_nvs();
  load_tts_enabled_from_nvs();
  load_theme_list();
  /* Reset driver cache so async fetch repopulates each time settings opens.
   * (Stale data is unlikely to bite given driver list rarely changes, but
   * fresh fetch keeps "is openclaw still online?" honest.) */
  s_st.driver_cache_count = 0;
  s_st.driver_cache_offline = 0;
  s_st.driver_idx = 0;

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

  /* Rows container */
  lv_obj_t* rows_box = lv_obj_create(s_st.root);
  lv_obj_remove_style_all(rows_box);
  lv_obj_set_size(rows_box, lv_pct(100), lv_pct(100) - HEADER_H);
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

  s_st.sel = ROW_AGENT;
  s_st.active = 1;

  /* Kick off async driver fetch — apply_rows will re-render when done. */
  spawn_fetch_task();
  apply_rows();

  ESP_LOGI(TAG, "show (themes=%d, driver_pref='%s', tts=%d)",
           s_st.theme_count, s_st.selected_driver, s_st.tts_enabled);
}

void bb_ui_settings_hide(void) {
  if (!s_st.active) return;
  s_st.active = 0;
  /* Bump generation so any in-flight fetch result is dropped. */
  s_st.driver_fetch_generation++;
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

/* LEFT/RIGHT — change the current row's value AND auto-commit to NVS.
 *
 * Phase 4.7.1: switched from "press OK to commit" to "auto-save on every
 * value change" because the OK-commit pattern was a footgun — users
 * naturally expect that what they see on screen is what's saved. With
 * auto-save, BACK can safely exit at any time without losing changes.
 */
void bb_ui_settings_handle_value(int delta) {
  if (!s_st.active || delta == 0) return;
  switch ((settings_row_t)s_st.sel) {
    case ROW_AGENT: {
      if (s_st.driver_cache_count <= 1) return;
      int n = s_st.driver_cache_count;
      int next = ((s_st.driver_idx + delta) % n + n) % n;
      s_st.driver_idx = next;
      const char* name = s_st.driver_cache[next].name;
      if (name != NULL && name[0] != '\0') {
        esp_err_t err = persist_selected_driver(name);
        if (err != ESP_OK) {
          ESP_LOGW(TAG, "persist driver '%s' failed (%s)", name, esp_err_to_name(err));
        }
        strncpy(s_st.selected_driver, name, sizeof(s_st.selected_driver) - 1);
        s_st.selected_driver[sizeof(s_st.selected_driver) - 1] = '\0';
        ESP_LOGI(TAG, "driver -> '%s' (auto-saved)", name);
      }
      break;
    }
    case ROW_THEME: {
      if (s_st.theme_count <= 1) return;
      int n = s_st.theme_count;
      int next = ((s_st.theme_idx + delta) % n + n) % n;
      s_st.theme_idx = next;
      const char* name = s_st.theme_list[next];
      if (name != NULL && name[0] != '\0') {
        /* bb_agent_theme_set_active writes NVS namespace "bbclaw" key
         * "agent/theme" AND updates the in-memory active theme pointer. */
        esp_err_t err = bb_agent_theme_set_active(name);
        if (err != ESP_OK) {
          ESP_LOGW(TAG, "theme set '%s' failed (%s)", name, esp_err_to_name(err));
        } else {
          ESP_LOGI(TAG, "theme -> '%s' (auto-saved)", name);
        }
      }
      break;
    }
    case ROW_TTS: {
      s_st.tts_enabled = !s_st.tts_enabled;
      esp_err_t err = persist_tts_enabled(s_st.tts_enabled);
      if (err != ESP_OK) {
        ESP_LOGW(TAG, "persist tts '%s' failed (%s)",
                 s_st.tts_enabled ? "on" : "off", esp_err_to_name(err));
      }
      ESP_LOGI(TAG, "tts -> %s (auto-saved)", s_st.tts_enabled ? "on" : "off");
      break;
    }
    case ROW_BACK:
    case ROW_COUNT:
    default:
      return;
  }
  apply_rows();
}

/* OK — Phase 4.7.1: pure cursor advance. Values are already auto-saved by
 * handle_value(), so OK doesn't need commit logic. On the Back row, OK exits
 * (same as BACK key) — keeps either gesture working. */
void bb_ui_settings_handle_click(void) {
  if (!s_st.active) return;
  switch ((settings_row_t)s_st.sel) {
    case ROW_AGENT: s_st.sel = ROW_THEME; apply_rows(); break;
    case ROW_THEME: s_st.sel = ROW_TTS;   apply_rows(); break;
    case ROW_TTS:   s_st.sel = ROW_BACK;  apply_rows(); break;
    case ROW_BACK:  bb_ui_settings_hide();              break;
    case ROW_COUNT:
    default: break;
  }
}
