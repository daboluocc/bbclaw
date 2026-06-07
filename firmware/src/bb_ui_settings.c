/* Settings overlay — 2-level picker (ADR-016 revision).
 *
 * Hardware: rotary encoder UP/DOWN + OK + BACK only. No LEFT/RIGHT — the
 * earlier preview-on-LR / commit-on-OK model is gone. Instead OK on a
 * value row pushes a sub-picker; the picker uses UP/DOWN to highlight,
 * OK to commit, BACK to abandon.
 *
 * Layout (LEVEL_MAIN):
 *     Driver: <name>
 *     Model:  <label>
 *     TTS:    On/Off
 *     Back
 *
 * Layout (LEVEL_DRIVER_PICKER):
 *     <driver-1>   ✓        (✓ = adapter's persisted active_driver)
 *     <driver-2>
 *     ...
 *
 * Layout (LEVEL_MODEL_PICKER):
 *     <model-1>    ✓
 *     <model-2>
 *     ...
 *
 *   (Special row for drivers that don't implement ModelLister:
 *    "(no models)" — only thing visible; OK is a no-op, BACK returns.)
 *
 * Async pipeline: driver+models fetch runs on a background FreeRTOS task
 * on entry; commits (PUT) run on a separate background task per click.
 * All async results dispatch back to the UI via lv_async_call.
 *
 * Session selection is INTENTIONALLY not here — it lives in the Session
 * Picker (bb_ui_agent_chat) reached by short-press OK from chat.
 */

#include "bb_ui_settings.h"

#include <stdio.h>
#include <string.h>

#include "bb_agent_client.h"
#include "bb_session_store.h"
#include "bb_ui_agent_chat.h"
#include "bb_ui_theme.h"
#include "esp_log.h"
#include "esp_lvgl_port.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "lvgl.h"
#include "nvs.h"
#include "nvs_flash.h"

static const char* TAG = "bb_ui_settings";

#define BB_SETTINGS_NVS_NS         "bbclaw"
#define BB_SETTINGS_NVS_KEY_TTS    "agent/tts"
#define BB_SETTINGS_DRIVER_CACHE_MAX 6

#define BB_SETTINGS_FETCH_TASK_STACK 4096
#define BB_SETTINGS_FETCH_TASK_PRIO  4

/* Visual — design/UI_DESIGN_LANGUAGE.md tokens. Selected row = ghost face +
 * teal left-edge bar + cool-white text (shared list selection idiom). */
#define UI_BG          BB_UI_BG
#define UI_HEADER_FG   BB_UI_DOT_LIT
#define UI_ROW_FG      BB_UI_TEXT_DIM
#define UI_ROW_SEL_FG  BB_UI_DOT_LIT
#define UI_ROW_SEL_BG  BB_UI_DOT_GHOST
#define UI_ROW_CHECK_FG BB_UI_ACCENT

#define HEADER_H 22
#define ROW_H    26

/* ── Level / row enums ── */

typedef enum {
  LEVEL_MAIN = 0,
  LEVEL_DRIVER_PICKER,
  LEVEL_MODEL_PICKER,
} settings_level_t;

typedef enum {
  MAIN_ROW_DRIVER = 0,
  MAIN_ROW_MODEL,
  MAIN_ROW_TTS,
  MAIN_ROW_BACK,
  MAIN_ROW_COUNT,
} main_row_t;

/* ── State ── */

typedef struct {
  int active;

  /* LVGL objects (re-created each time the level changes). */
  lv_obj_t* root;
  lv_obj_t* header_lbl;
  lv_obj_t* rows_box;
  lv_obj_t* rows[16]; /* over-sized so it fits driver picker, model picker, main. */
  int rows_used;

  settings_level_t level;
  int sel; /* cursor index at the current level */

  /* Driver catalog (populated async on entry). Shared by main + both pickers. */
  bb_agent_driver_info_t driver_cache[BB_SETTINGS_DRIVER_CACHE_MAX];
  int driver_cache_count;
  volatile int driver_fetch_pending;
  volatile uint32_t driver_fetch_generation;

  /* Persisted active selections from the last successful fetch — what
   * the main page shows as the current value, and what the picker marks
   * with ✓. Updated optimistically on commit so the UI feels snappy
   * without waiting for a re-fetch. */
  char active_driver[24];
  /* active_model is per-driver — we read it off
   * driver_cache[i].active_model directly. */

  int tts_enabled;
} settings_state_t;

static settings_state_t s_st = {0};
static int s_tts_loaded = 0;

/* ── NVS helpers (TTS only — driver/model live on adapter) ── */

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

int bb_ui_settings_tts_enabled(void) {
  if (!s_tts_loaded) {
    load_tts_enabled_from_nvs();
  }
  return s_st.tts_enabled ? 1 : 0;
}

/* ── Cache lookup helpers ── */

static int find_driver_idx(const char* name) {
  if (name == NULL || name[0] == '\0') return -1;
  for (int i = 0; i < s_st.driver_cache_count; ++i) {
    if (strcmp(s_st.driver_cache[i].name, name) == 0) return i;
  }
  return -1;
}

static const bb_agent_driver_info_t* active_driver_entry(void) {
  int idx = find_driver_idx(s_st.active_driver);
  if (idx < 0) idx = 0;
  if (idx >= s_st.driver_cache_count) return NULL;
  return &s_st.driver_cache[idx];
}

/* ── LVGL rebuild helpers ── */

static void destroy_rows(void) {
  for (int i = 0; i < (int)(sizeof(s_st.rows) / sizeof(s_st.rows[0])); ++i) {
    s_st.rows[i] = NULL;
  }
  s_st.rows_used = 0;
  if (s_st.rows_box != NULL) {
    lv_obj_del(s_st.rows_box);
    s_st.rows_box = NULL;
  }
}

static void build_rows_box(int row_count) {
  destroy_rows();
  s_st.rows_box = lv_obj_create(s_st.root);
  lv_obj_remove_style_all(s_st.rows_box);
  /* Height = row_count * ROW_H + small padding. Cap so it never overflows
   * the (typically 240-tall) screen — caller picks reasonable row_count. */
  int box_h = row_count * ROW_H + 6;
  lv_obj_set_size(s_st.rows_box, lv_pct(100), box_h);
  lv_obj_align(s_st.rows_box, LV_ALIGN_TOP_LEFT, 0, HEADER_H);
  lv_obj_set_flex_flow(s_st.rows_box, LV_FLEX_FLOW_COLUMN);
  lv_obj_set_style_pad_all(s_st.rows_box, 4, 0);
  lv_obj_clear_flag(s_st.rows_box, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_set_style_bg_opa(s_st.rows_box, LV_OPA_TRANSP, 0);
  for (int i = 0; i < row_count && i < (int)(sizeof(s_st.rows) / sizeof(s_st.rows[0])); ++i) {
    lv_obj_t* row = lv_label_create(s_st.rows_box);
    lv_obj_set_size(row, lv_pct(100), ROW_H);
    lv_obj_set_style_pad_left(row, 6, 0);
    lv_obj_set_style_pad_right(row, 6, 0);
    lv_obj_set_style_pad_top(row, 4, 0);
    lv_obj_set_style_radius(row, 3, 0);
    lv_obj_set_style_text_color(row, lv_color_hex(UI_ROW_FG), 0);
    lv_label_set_long_mode(row, LV_LABEL_LONG_MODE_DOTS);
    s_st.rows[i] = row;
  }
  s_st.rows_used = row_count;
}

static void highlight_selected(void) {
  for (int i = 0; i < s_st.rows_used; ++i) {
    lv_obj_t* row = s_st.rows[i];
    if (row == NULL) continue;
    if (i == s_st.sel) {
      lv_obj_set_style_bg_color(row, lv_color_hex(UI_ROW_SEL_BG), 0);
      lv_obj_set_style_bg_opa(row, LV_OPA_COVER, 0);
      lv_obj_set_style_text_color(row, lv_color_hex(UI_ROW_SEL_FG), 0);
      lv_obj_set_style_border_side(row, LV_BORDER_SIDE_LEFT, 0);
      lv_obj_set_style_border_width(row, 3, 0);
      lv_obj_set_style_border_color(row, lv_color_hex(BB_UI_ACCENT), 0);
    } else {
      lv_obj_set_style_bg_opa(row, LV_OPA_TRANSP, 0);
      lv_obj_set_style_border_width(row, 0, 0);
      lv_obj_set_style_text_color(row, lv_color_hex(UI_ROW_FG), 0);
    }
  }
}

/* ── Render: main page ── */

static const char* current_model_label(const bb_agent_driver_info_t* d) {
  if (d == NULL) return "-";
  if (d->model_count == 0) return "(n/a)";
  /* Find the slot whose id matches the driver's active_model. */
  if (d->active_model[0] != '\0') {
    for (int j = 0; j < d->model_count; ++j) {
      if (strcmp(d->models[j].id, d->active_model) == 0) {
        return d->models[j].label[0] != '\0' ? d->models[j].label : d->models[j].id;
      }
    }
  }
  /* No match — show the first model as a hint (= driver-default fallback). */
  return d->models[0].label[0] != '\0' ? d->models[0].label : d->models[0].id;
}

static void render_main(void) {
  if (s_st.root == NULL) return;
  lv_label_set_text(s_st.header_lbl, "Settings");

  build_rows_box(MAIN_ROW_COUNT);
  char buf[80];

  const bb_agent_driver_info_t* active = active_driver_entry();

  /* Row: Driver */
  const char* drv_name = (active != NULL) ? active->name :
                          (s_st.driver_fetch_pending ? "loading..." : "(offline)");
  snprintf(buf, sizeof(buf), "Driver: %s", drv_name);
  lv_label_set_text(s_st.rows[MAIN_ROW_DRIVER], buf);

  /* Row: Model */
  snprintf(buf, sizeof(buf), "Model: %s", current_model_label(active));
  lv_label_set_text(s_st.rows[MAIN_ROW_MODEL], buf);

  /* Row: TTS */
  snprintf(buf, sizeof(buf), "TTS: %s", s_st.tts_enabled ? "On" : "Off");
  lv_label_set_text(s_st.rows[MAIN_ROW_TTS], buf);

  /* Row: Back */
  lv_label_set_text(s_st.rows[MAIN_ROW_BACK], "Back");

  highlight_selected();
}

/* ── Render: Driver picker ── */

static void render_driver_picker(void) {
  if (s_st.root == NULL) return;
  lv_label_set_text(s_st.header_lbl, "Driver");

  if (s_st.driver_cache_count == 0) {
    build_rows_box(1);
    lv_label_set_text(s_st.rows[0],
                      s_st.driver_fetch_pending ? "loading..." : "(offline)");
    highlight_selected();
    return;
  }

  build_rows_box(s_st.driver_cache_count);
  for (int i = 0; i < s_st.driver_cache_count; ++i) {
    char buf[64];
    int is_active = (strcmp(s_st.driver_cache[i].name, s_st.active_driver) == 0);
    snprintf(buf, sizeof(buf), "%s%s",
             s_st.driver_cache[i].name,
             is_active ? "  *" : "");
    lv_label_set_text(s_st.rows[i], buf);
  }
  highlight_selected();
}

/* ── Render: Model picker (depends on currently active driver) ── */

static void render_model_picker(void) {
  if (s_st.root == NULL) return;

  const bb_agent_driver_info_t* d = active_driver_entry();
  if (d == NULL) {
    lv_label_set_text(s_st.header_lbl, "Model");
    build_rows_box(1);
    lv_label_set_text(s_st.rows[0], "(no driver)");
    highlight_selected();
    return;
  }

  char header[40];
  snprintf(header, sizeof(header), "Model (%s)", d->name);
  lv_label_set_text(s_st.header_lbl, header);

  if (d->model_count == 0) {
    build_rows_box(1);
    lv_label_set_text(s_st.rows[0], "(no models)");
    highlight_selected();
    return;
  }

  build_rows_box(d->model_count);
  for (int j = 0; j < d->model_count; ++j) {
    char buf[64];
    const char* lbl = d->models[j].label[0] != '\0' ? d->models[j].label : d->models[j].id;
    int is_active = (strcmp(d->models[j].id, d->active_model) == 0);
    snprintf(buf, sizeof(buf), "%s%s", lbl, is_active ? "  *" : "");
    lv_label_set_text(s_st.rows[j], buf);
  }
  highlight_selected();
}

static void rerender(void) {
  switch (s_st.level) {
    case LEVEL_MAIN:          render_main(); break;
    case LEVEL_DRIVER_PICKER: render_driver_picker(); break;
    case LEVEL_MODEL_PICKER:  render_model_picker(); break;
  }
}

/* ── Driver+model fetch (async) ── */

typedef struct {
  uint32_t gen;
  esp_err_t err;
  int total;
  char active_driver[24];
  bb_agent_driver_info_t entries[BB_SETTINGS_DRIVER_CACHE_MAX];
} driver_fetch_result_t;

static void on_driver_fetch_done(void* user_data) {
  driver_fetch_result_t* r = (driver_fetch_result_t*)user_data;
  if (r == NULL) return;
  s_st.driver_fetch_pending = 0;
  if (!s_st.active || r->gen != s_st.driver_fetch_generation) {
    free(r);
    return;
  }
  if (r->err != ESP_OK || r->total <= 0) {
    ESP_LOGW(TAG, "driver fetch failed (%s) total=%d",
             esp_err_to_name(r->err), r->total);
    s_st.driver_cache_count = 0;
  } else {
    int total = r->total > BB_SETTINGS_DRIVER_CACHE_MAX
                  ? BB_SETTINGS_DRIVER_CACHE_MAX : r->total;
    memcpy(s_st.driver_cache, r->entries, sizeof(r->entries[0]) * (size_t)total);
    s_st.driver_cache_count = total;
    /* Resolve active_driver: prefer adapter's reply; fall back to chat's
     * current driver; fall back to driver_cache[0]. */
    const char* fallback = bb_ui_agent_chat_get_current_driver();
    const char* want = (r->active_driver[0] != '\0') ? r->active_driver
                       : (fallback != NULL ? fallback : "");
    int idx = find_driver_idx(want);
    if (idx < 0) idx = 0;
    strncpy(s_st.active_driver, s_st.driver_cache[idx].name,
            sizeof(s_st.active_driver) - 1);
    s_st.active_driver[sizeof(s_st.active_driver) - 1] = '\0';
    ESP_LOGI(TAG, "driver fetch ok: %d drivers active='%s'",
             total, s_st.active_driver);
  }
  rerender();
  free(r);
}

static void driver_fetch_task(void* arg) {
  driver_fetch_result_t* r = (driver_fetch_result_t*)arg;
  if (r == NULL) {
    s_st.driver_fetch_pending = 0;
    vTaskDelete(NULL);
    return;
  }
  r->err = bb_agent_list_drivers(r->entries, BB_SETTINGS_DRIVER_CACHE_MAX, &r->total,
                                 r->active_driver, sizeof(r->active_driver));
  if (lvgl_port_lock(200)) {
    lv_async_call(on_driver_fetch_done, r);
    lvgl_port_unlock();
  } else {
    free(r);
  }
  vTaskDelete(NULL);
}

static void spawn_driver_fetch_task(void) {
  if (s_st.driver_fetch_pending) return;
  s_st.driver_fetch_pending = 1;
  uint32_t gen = ++s_st.driver_fetch_generation;

  driver_fetch_result_t* r = (driver_fetch_result_t*)calloc(1, sizeof(*r));
  if (r == NULL) {
    ESP_LOGE(TAG, "spawn_driver_fetch_task: calloc failed");
    s_st.driver_fetch_pending = 0;
    return;
  }
  r->gen = gen;
  TaskHandle_t t = NULL;
  BaseType_t ok = xTaskCreate(driver_fetch_task, "drv_fetch",
                              BB_SETTINGS_FETCH_TASK_STACK, r,
                              BB_SETTINGS_FETCH_TASK_PRIO, &t);
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "spawn_driver_fetch_task: xTaskCreate failed");
    s_st.driver_fetch_pending = 0;
    free(r);
  }
}

/* ── Commit task (async PUT) ── */

typedef enum {
  COMMIT_KIND_DRIVER = 0,
  COMMIT_KIND_MODEL,
} commit_kind_t;

typedef struct {
  commit_kind_t kind;
  char driver_name[24];
  char model_id[40];
} commit_payload_t;

static void commit_task(void* arg) {
  commit_payload_t* p = (commit_payload_t*)arg;
  if (p == NULL) {
    vTaskDelete(NULL);
    return;
  }
  esp_err_t err = ESP_OK;
  if (p->kind == COMMIT_KIND_DRIVER) {
    err = bb_agent_set_active_driver(p->driver_name);
    if (err == ESP_OK) {
      bb_session_store_save_active_driver(p->driver_name);
      /* ADR-016: also flip the active chat overlay over to the new driver so
       * the next user prompt routes there + the right session is loaded.
       * The chat overlay is still up underneath Settings; set_active_driver
       * touches LVGL state so we must hold the port lock. Failure is logged
       * but not propagated — the next chat re-entry will rebuild correctly
       * from NVS-cached drv/active. */
      if (lvgl_port_lock(200)) {
        esp_err_t cerr = bb_ui_agent_chat_set_active_driver(p->driver_name);
        lvgl_port_unlock();
        if (cerr != ESP_OK) {
          ESP_LOGW(TAG, "commit driver: chat sync failed (%s) — adapter is consistent",
                   esp_err_to_name(cerr));
        }
      } else {
        ESP_LOGW(TAG, "commit driver: lvgl_port_lock timeout, chat will sync on next entry");
      }
    }
    ESP_LOGI(TAG, "commit driver='%s' -> %s", p->driver_name, esp_err_to_name(err));
  } else {
    err = bb_agent_set_active_model(p->driver_name, p->model_id);
    if (err == ESP_OK) {
      /* ADR-016: push the new model into the bottom-bar slot. We pass model_id
       * directly because Settings has the human label cached but commit_task
       * doesn't — close enough: id and label are mostly the same string for
       * static catalogues (ollama tags don't have separate labels). */
      if (lvgl_port_lock(200)) {
        bb_ui_agent_chat_set_active_model(p->model_id);
        lvgl_port_unlock();
      }
    }
    ESP_LOGI(TAG, "commit driver='%s' model='%s' -> %s",
             p->driver_name, p->model_id, esp_err_to_name(err));
  }
  free(p);
  vTaskDelete(NULL);
}

static void spawn_commit_task(commit_kind_t kind, const char* driver, const char* model_id) {
  commit_payload_t* p = (commit_payload_t*)calloc(1, sizeof(*p));
  if (p == NULL) {
    ESP_LOGE(TAG, "spawn_commit_task: calloc failed");
    return;
  }
  p->kind = kind;
  if (driver != NULL) {
    strncpy(p->driver_name, driver, sizeof(p->driver_name) - 1);
  }
  if (model_id != NULL) {
    strncpy(p->model_id, model_id, sizeof(p->model_id) - 1);
  }
  TaskHandle_t t = NULL;
  BaseType_t ok = xTaskCreate(commit_task, "drv_commit",
                              BB_SETTINGS_FETCH_TASK_STACK, p,
                              BB_SETTINGS_FETCH_TASK_PRIO, &t);
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "spawn_commit_task: xTaskCreate failed");
    free(p);
  }
}

/* ── Level transitions ── */

static void enter_driver_picker(void) {
  s_st.level = LEVEL_DRIVER_PICKER;
  /* Cursor lands on the currently-active driver. */
  int idx = find_driver_idx(s_st.active_driver);
  s_st.sel = (idx >= 0) ? idx : 0;
  rerender();
}

static void enter_model_picker(void) {
  s_st.level = LEVEL_MODEL_PICKER;
  const bb_agent_driver_info_t* d = active_driver_entry();
  int sel = 0;
  if (d != NULL && d->model_count > 0 && d->active_model[0] != '\0') {
    for (int j = 0; j < d->model_count; ++j) {
      if (strcmp(d->models[j].id, d->active_model) == 0) {
        sel = j;
        break;
      }
    }
  }
  s_st.sel = sel;
  ESP_LOGI(TAG, "enter_model_picker: driver='%s' model_count=%d sel=%d",
           d != NULL ? d->name : "(null)",
           d != NULL ? d->model_count : 0, sel);
  rerender();
}

static void return_to_main(int new_sel_row) {
  s_st.level = LEVEL_MAIN;
  if (new_sel_row >= 0 && new_sel_row < MAIN_ROW_COUNT) {
    s_st.sel = new_sel_row;
  } else {
    s_st.sel = MAIN_ROW_DRIVER;
  }
  rerender();
}

/* ── Public lifecycle ── */

void bb_ui_settings_show(lv_obj_t* parent) {
  if (parent == NULL) {
    ESP_LOGE(TAG, "show: parent NULL");
    return;
  }
  if (s_st.active) {
    ESP_LOGW(TAG, "show: already active, ignoring");
    return;
  }

  /* Initialise level + cursor before any LVGL work. */
  s_st.level = LEVEL_MAIN;
  s_st.sel = MAIN_ROW_DRIVER;
  /* Seed active_driver from NVS cache so the first paint of the main page
   * shows something sensible even before the async fetch lands. */
  if (s_st.active_driver[0] == '\0') {
    bb_session_store_load_active_driver(s_st.active_driver, sizeof(s_st.active_driver));
  }

  s_st.root = lv_obj_create(parent);
  lv_obj_remove_style_all(s_st.root);
  lv_obj_set_size(s_st.root, lv_pct(100), lv_pct(100));
  lv_obj_set_style_bg_color(s_st.root, lv_color_hex(UI_BG), 0);
  lv_obj_set_style_bg_opa(s_st.root, LV_OPA_COVER, 0);
  lv_obj_clear_flag(s_st.root, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_move_foreground(s_st.root);

  s_st.header_lbl = lv_label_create(s_st.root);
  lv_obj_set_size(s_st.header_lbl, lv_pct(100), HEADER_H);
  lv_obj_align(s_st.header_lbl, LV_ALIGN_TOP_LEFT, 0, 0);
  lv_obj_set_style_text_color(s_st.header_lbl, lv_color_hex(UI_HEADER_FG), 0);
  lv_obj_set_style_pad_left(s_st.header_lbl, 6, 0);
  lv_obj_set_style_pad_top(s_st.header_lbl, 4, 0);

  s_st.active = 1;

  /* Kick off async driver/model fetch. apply renders a stale (or empty)
   * snapshot in the meantime — on_driver_fetch_done re-renders when done. */
  spawn_driver_fetch_task();
  rerender();

  ESP_LOGI(TAG, "show level=MAIN tts=%d", s_st.tts_enabled);
}

void bb_ui_settings_hide(void) {
  if (!s_st.active) return;
  s_st.active = 0;
  s_st.driver_fetch_generation++;
  destroy_rows();
  if (s_st.root != NULL) {
    lv_obj_del(s_st.root);
    s_st.root = NULL;
  }
  s_st.header_lbl = NULL;
  ESP_LOGI(TAG, "hide");
}

int bb_ui_settings_is_active(void) {
  return s_st.active ? 1 : 0;
}

/* ── Input handlers ── */

void bb_ui_settings_handle_rotate(int delta) {
  if (!s_st.active || delta == 0) return;
  int row_count;
  switch (s_st.level) {
    case LEVEL_MAIN:
      row_count = MAIN_ROW_COUNT;
      break;
    case LEVEL_DRIVER_PICKER:
      row_count = (s_st.driver_cache_count > 0) ? s_st.driver_cache_count : 1;
      break;
    case LEVEL_MODEL_PICKER: {
      const bb_agent_driver_info_t* d = active_driver_entry();
      row_count = (d != NULL && d->model_count > 0) ? d->model_count : 1;
      break;
    }
    default:
      return;
  }
  int next = s_st.sel + delta;
  if (next < 0) next = 0;
  if (next >= row_count) next = row_count - 1;
  if (next == s_st.sel) return;
  s_st.sel = next;
  highlight_selected();
}

int bb_ui_settings_handle_click(void) {
  if (!s_st.active) return 0;
  switch (s_st.level) {
    case LEVEL_MAIN:
      switch ((main_row_t)s_st.sel) {
        case MAIN_ROW_DRIVER:
          enter_driver_picker();
          break;
        case MAIN_ROW_MODEL: {
          const bb_agent_driver_info_t* d = active_driver_entry();
          if (d == NULL || d->model_count == 0) {
            /* Nothing to pick — flash the row but don't push a picker. */
            ESP_LOGI(TAG, "main click on Model: driver has no models");
          } else {
            enter_model_picker();
          }
          break;
        }
        case MAIN_ROW_TTS: {
          /* In-place toggle (no sub-picker — binary value). */
          s_st.tts_enabled = !s_st.tts_enabled;
          esp_err_t err = persist_tts_enabled(s_st.tts_enabled);
          if (err != ESP_OK) {
            ESP_LOGW(TAG, "persist tts %d failed (%s)",
                     s_st.tts_enabled, esp_err_to_name(err));
          }
          rerender();
          break;
        }
        case MAIN_ROW_BACK:
          return 1; /* caller tears down + returns to chat */
        case MAIN_ROW_COUNT:
        default:
          break;
      }
      return 0;

    case LEVEL_DRIVER_PICKER:
      if (s_st.driver_cache_count <= 0) {
        return_to_main(MAIN_ROW_DRIVER);
        return 0;
      }
      if (s_st.sel >= 0 && s_st.sel < s_st.driver_cache_count) {
        const char* picked = s_st.driver_cache[s_st.sel].name;
        if (picked != NULL && picked[0] != '\0' &&
            strcmp(picked, s_st.active_driver) != 0) {
          /* Optimistic local update so main page reflects the choice
           * immediately. Adapter will confirm async. */
          strncpy(s_st.active_driver, picked, sizeof(s_st.active_driver) - 1);
          s_st.active_driver[sizeof(s_st.active_driver) - 1] = '\0';
          spawn_commit_task(COMMIT_KIND_DRIVER, picked, NULL);
          ESP_LOGI(TAG, "driver picker -> '%s' (committed)", picked);
        }
      }
      return_to_main(MAIN_ROW_DRIVER);
      return 0;

    case LEVEL_MODEL_PICKER: {
      const bb_agent_driver_info_t* d = active_driver_entry();
      if (d == NULL || d->model_count <= 0) {
        return_to_main(MAIN_ROW_MODEL);
        return 0;
      }
      int idx = s_st.sel;
      if (idx >= 0 && idx < d->model_count) {
        const char* mid = d->models[idx].id;
        if (mid != NULL && mid[0] != '\0') {
          /* Optimistic local update — write into the cache row so the
           * main page's "Model: <label>" reflects it before next fetch. */
          int drv_idx = find_driver_idx(d->name);
          if (drv_idx >= 0) {
            strncpy(s_st.driver_cache[drv_idx].active_model, mid,
                    sizeof(s_st.driver_cache[drv_idx].active_model) - 1);
            s_st.driver_cache[drv_idx].active_model
                [sizeof(s_st.driver_cache[drv_idx].active_model) - 1] = '\0';
          }
          spawn_commit_task(COMMIT_KIND_MODEL, d->name, mid);
          ESP_LOGI(TAG, "model picker -> '%s' for driver '%s' (committed)",
                   mid, d->name);
        }
      }
      return_to_main(MAIN_ROW_MODEL);
      return 0;
    }
  }
  return 0;
}

int bb_ui_settings_handle_back(void) {
  if (!s_st.active) return 0;
  switch (s_st.level) {
    case LEVEL_MAIN:
      return 1; /* caller exits to chat */
    case LEVEL_DRIVER_PICKER:
      return_to_main(MAIN_ROW_DRIVER);
      return 0;
    case LEVEL_MODEL_PICKER:
      return_to_main(MAIN_ROW_MODEL);
      return 0;
  }
  return 0;
}
