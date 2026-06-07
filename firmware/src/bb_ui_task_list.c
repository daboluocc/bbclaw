/**
 * bb_ui_task_list.c — Task List page (ADR-021-firmware-ui §3 / §4.2, Sub-PR D)
 *
 * Independent LVGL screen. Fetches /v1/butler/dispatch/recent in a background
 * FreeRTOS task and renders the results using the same lv_async_call pattern
 * as the session picker.
 */
#include "bb_ui_task_list.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#include "bb_agent_client.h"
#include "bb_ui_agent_chat.h"
#include "bb_ui_theme.h"
#include "bb_wifi.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "lvgl.h"

static const char* TAG = "bb_task_list";

/* ── Layout / colour constants ── */
#define BB_TASK_LIST_MAX      20
#define BB_TASK_LIST_VISIBLE  6
#define BB_TASK_LIST_ROW_H    22  /* ADR §4.2: row height 22px */
#define BB_TASK_LIST_TITLE_H  22

/* Colour palette — design/UI_DESIGN_LANGUAGE.md tokens. Selected row =
 * ghost face + teal left-edge bar + cool-white text (shared list idiom). */
#define BB_TL_BG       BB_UI_BG
#define BB_TL_FG       BB_UI_DOT_LIT
#define BB_TL_FG_DIM   BB_UI_TEXT_DIM
#define BB_TL_FG_RUN   BB_UI_ACCENT /* running highlight (primary) */
#define BB_TL_FG_ERR   BB_UI_ERR    /* error red */
#define BB_TL_SEL_BG   BB_UI_DOT_GHOST
#define BB_TL_SEL_FG   BB_UI_DOT_LIT

/* ── Module state ── */
typedef struct {
  int visible;          /* 0=hidden, 1=loading, 2=shown */
  int sel;              /* selected row index (0-based) */
  int count;            /* number of entries loaded */
  bb_agent_dispatch_entry_t entries[BB_TASK_LIST_MAX];

  lv_obj_t* screen;     /* independent LVGL screen */
  lv_obj_t* lbl_title;  /* "派发任务  N" */
  lv_obj_t* lbl_empty;  /* "暂无派发任务" label */
  lv_obj_t* rows[BB_TASK_LIST_MAX];

  volatile int fetch_pending;
  volatile uint32_t fetch_gen;
} bb_task_list_state_t;

static bb_task_list_state_t s_tl = {0};

/* ── safe_lv_async_call mirror (same approach as bb_ui_agent_chat.c) ── */
static inline lv_result_t tl_lv_async_call(lv_async_cb_t cb, void* user_data) {
  if (lv_async_call(cb, user_data) != LV_RESULT_OK) {
    ESP_LOGW(TAG, "lv_async_call failed");
    return LV_RESULT_INVALID;
  }
  return LV_RESULT_OK;
}

/* ── Background fetch task ── */
#define BB_TL_FETCH_STACK 4096
#define BB_TL_FETCH_PRIO  4

typedef struct {
  uint32_t gen;
  esp_err_t err;
  int count;
  bb_agent_dispatch_entry_t entries[BB_TASK_LIST_MAX];
} tl_fetch_result_t;

/* Forward declarations */
static void task_list_build_ui(void);
static void task_list_apply_styles(void);

/* ── relative time helper ── */
static void tl_format_elapsed(int64_t elapsed_ms, char* buf, int len) {
  if (elapsed_ms <= 0) { buf[0] = '\0'; return; }
  long ms = (long)elapsed_ms;
  if (ms < 1000) {
    snprintf(buf, (size_t)len, "%ldms", ms);
  } else if (ms < 60000) {
    snprintf(buf, (size_t)len, "%.1fs", (double)ms / 1000.0);
  } else {
    snprintf(buf, (size_t)len, "%ldm", ms / 60000);
  }
}

static void tl_format_started(int64_t started_ms, char* buf, int len) {
  if (started_ms <= 0) { buf[0] = '\0'; return; }
  time_t now_sec = time(NULL);
  if (now_sec < 1577836800LL) { buf[0] = '\0'; return; }
  int64_t diff_s = (int64_t)now_sec - started_ms / 1000;
  if (diff_s < 0) diff_s = 0;
  if (diff_s < 60) {
    snprintf(buf, (size_t)len, "%llds", (long long)diff_s);
  } else if (diff_s < 3600) {
    snprintf(buf, (size_t)len, "%lldm", (long long)(diff_s / 60));
  } else {
    snprintf(buf, (size_t)len, "%lldh", (long long)(diff_s / 3600));
  }
}

/* ── Build row text: "<emoji> <cwd> <title>  <status>  <time>" ── */
static void tl_format_row(const bb_agent_dispatch_entry_t* e, char* buf, int buf_len) {
  const char* emoji = "❓";
  if (strcmp(e->status, "running") == 0) emoji = "⏳";
  else if (strcmp(e->status, "done")    == 0) emoji = "✅";
  else if (strcmp(e->status, "error")   == 0) emoji = "❌";
  else if (strcmp(e->status, "async")   == 0) emoji = "⏰";

  char time_buf[12] = {0};
  if (strcmp(e->status, "done") == 0 || strcmp(e->status, "error") == 0) {
    tl_format_elapsed(e->elapsed_ms, time_buf, sizeof(time_buf));
  } else {
    tl_format_started(e->started_at_ms, time_buf, sizeof(time_buf));
  }

  /* Truncate title to first 24 bytes (rough CJK limit) */
  char title_trunc[52];
  strncpy(title_trunc, e->title, sizeof(title_trunc) - 1);
  title_trunc[sizeof(title_trunc) - 1] = '\0';
  if (strlen(title_trunc) > 24) {
    title_trunc[24] = '\0';
    strncat(title_trunc, "…", sizeof(title_trunc) - strlen(title_trunc) - 1);
  }

  snprintf(buf, (size_t)buf_len, "%s %s %s  %s  %s",
           emoji,
           e->cwd[0] != '\0' ? e->cwd : "?",
           title_trunc,
           e->status,
           time_buf);
}

/* ── Apply selection highlight ── */
static void task_list_apply_styles(void) {
  if (s_tl.screen == NULL || s_tl.count == 0) return;
  int first = s_tl.sel - BB_TASK_LIST_VISIBLE / 2;
  if (first < 0) first = 0;
  if (first + BB_TASK_LIST_VISIBLE > s_tl.count)
    first = s_tl.count - BB_TASK_LIST_VISIBLE;
  if (first < 0) first = 0;

  for (int i = 0; i < s_tl.count; i++) {
    lv_obj_t* row = s_tl.rows[i];
    if (row == NULL) continue;
    int visible = (i >= first && i < first + BB_TASK_LIST_VISIBLE);
    if (!visible) { lv_obj_add_flag(row, LV_OBJ_FLAG_HIDDEN); continue; }
    lv_obj_clear_flag(row, LV_OBJ_FLAG_HIDDEN);

    const bb_agent_dispatch_entry_t* e = &s_tl.entries[i];
    if (i == s_tl.sel) {
      lv_obj_set_style_bg_color(row, lv_color_hex(BB_TL_SEL_BG), 0);
      lv_obj_set_style_bg_opa(row, LV_OPA_COVER, 0);
      lv_obj_set_style_text_color(row, lv_color_hex(BB_TL_SEL_FG), 0);
      lv_obj_set_style_border_side(row, LV_BORDER_SIDE_LEFT, 0);
      lv_obj_set_style_border_width(row, 3, 0);
      lv_obj_set_style_border_color(row, lv_color_hex(BB_UI_ACCENT), 0);
    } else {
      lv_obj_set_style_bg_opa(row, LV_OPA_TRANSP, 0);
      lv_obj_set_style_border_width(row, 0, 0);
      uint32_t fg = BB_TL_FG;
      if (strcmp(e->status, "running") == 0) fg = BB_TL_FG_RUN;
      else if (strcmp(e->status, "error") == 0) fg = BB_TL_FG_ERR;
      else if (strcmp(e->status, "done") == 0 || strcmp(e->status, "async") == 0)
        fg = BB_TL_FG_DIM;
      lv_obj_set_style_text_color(row, lv_color_hex(fg), 0);
    }
  }

  /* Update title count */
  if (s_tl.lbl_title != NULL) {
    char tbuf[32];
    snprintf(tbuf, sizeof(tbuf), "派发任务  %d", s_tl.count);
    lv_label_set_text(s_tl.lbl_title, tbuf);
  }
}

/* ── Build the LVGL overlay ── */
static void task_list_build_ui(void) {
  /* Tear down existing overlay if any. */
  if (s_tl.screen != NULL) {
    lv_obj_del(s_tl.screen);
    s_tl.screen = NULL;
    s_tl.lbl_title = NULL;
    s_tl.lbl_empty = NULL;
    memset(s_tl.rows, 0, sizeof(s_tl.rows));
  }

#ifdef BBCLAW_HAVE_CJK_FONT
  extern const lv_font_t lv_font_bbclaw_cjk;
  const lv_font_t* font = &lv_font_bbclaw_cjk;
#else
  const lv_font_t* font = lv_font_get_default();
#endif

  /* Create full-screen overlay on the active screen (same pattern as session
   * picker / CWD picker — keeps LVGL screen state intact for easy BACK). */
  lv_obj_t* parent = lv_screen_active();
  if (parent == NULL) {
    ESP_LOGE(TAG, "task_list_build_ui: no active screen");
    s_tl.visible = 0;
    return;
  }
  s_tl.screen = lv_obj_create(parent);
  lv_obj_remove_style_all(s_tl.screen);
  lv_obj_set_size(s_tl.screen, 320, 172);
  lv_obj_align(s_tl.screen, LV_ALIGN_TOP_LEFT, 0, 0);
  lv_obj_set_style_bg_color(s_tl.screen, lv_color_hex(BB_TL_BG), 0);
  lv_obj_set_style_bg_opa(s_tl.screen, LV_OPA_COVER, 0);
  lv_obj_set_style_pad_all(s_tl.screen, 2, 0);
  lv_obj_set_flex_flow(s_tl.screen, LV_FLEX_FLOW_COLUMN);
  lv_obj_clear_flag(s_tl.screen, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_move_foreground(s_tl.screen);

  /* Title row */
  s_tl.lbl_title = lv_label_create(s_tl.screen);
  lv_obj_set_size(s_tl.lbl_title, 316, BB_TASK_LIST_TITLE_H);
  lv_obj_set_style_text_color(s_tl.lbl_title, lv_color_hex(BB_TL_FG), 0);
  lv_obj_set_style_text_font(s_tl.lbl_title, font, 0);
  lv_obj_set_style_pad_left(s_tl.lbl_title, 2, 0);
  lv_label_set_long_mode(s_tl.lbl_title, LV_LABEL_LONG_MODE_DOTS);
  {
    char tbuf[32];
    snprintf(tbuf, sizeof(tbuf), "派发任务  %d", s_tl.count);
    lv_label_set_text(s_tl.lbl_title, tbuf);
  }

  if (s_tl.count == 0) {
    /* Empty state */
    s_tl.lbl_empty = lv_label_create(s_tl.screen);
    lv_obj_set_size(s_tl.lbl_empty, 316, 120);
    lv_obj_set_style_text_color(s_tl.lbl_empty, lv_color_hex(BB_TL_FG_DIM), 0);
    lv_obj_set_style_text_font(s_tl.lbl_empty, font, 0);
    lv_obj_set_style_text_align(s_tl.lbl_empty, LV_TEXT_ALIGN_CENTER, 0);
    lv_label_set_text(s_tl.lbl_empty, "暂无派发任务");
  } else {
    /* List rows */
    for (int i = 0; i < s_tl.count; i++) {
      lv_obj_t* row = lv_label_create(s_tl.screen);
      s_tl.rows[i] = row;
      lv_obj_set_size(row, 316, BB_TASK_LIST_ROW_H);
      lv_obj_set_style_pad_left(row, 4, 0);
      lv_obj_set_style_pad_right(row, 4, 0);
      lv_obj_set_style_radius(row, 3, 0);
      lv_obj_set_style_text_font(row, font, 0);
      lv_label_set_long_mode(row, LV_LABEL_LONG_MODE_DOTS);
      char row_buf[80];
      tl_format_row(&s_tl.entries[i], row_buf, sizeof(row_buf));
      lv_label_set_text(row, row_buf);
    }
    task_list_apply_styles();
  }

  s_tl.visible = 2;
  ESP_LOGI(TAG, "task_list: built screen, count=%d sel=%d", s_tl.count, s_tl.sel);
}

/* ── Fetch callback (runs on LVGL task via lv_async_call) ── */
static void on_dispatch_fetch_done(void* user_data) {
  tl_fetch_result_t* res = (tl_fetch_result_t*)user_data;
  if (res == NULL) return;
  s_tl.fetch_pending = 0;

  if (res->gen != s_tl.fetch_gen || !s_tl.visible) {
    free(res);
    return;
  }
  if (res->err != ESP_OK) {
    ESP_LOGW(TAG, "dispatch fetch failed: err=%d", res->err);
    s_tl.count = 0;
  } else {
    s_tl.count = res->count;
    if (s_tl.count > 0) {
      memcpy(s_tl.entries, res->entries,
             sizeof(s_tl.entries[0]) * (size_t)s_tl.count);
    }
  }
  s_tl.sel = 0;
  task_list_build_ui();
  free(res);
}

/* FreeRTOS worker task */
static void dispatch_fetch_task(void* arg) {
  uint32_t my_gen = (uint32_t)(uintptr_t)arg;
  tl_fetch_result_t* res = (tl_fetch_result_t*)calloc(1, sizeof(*res));
  if (res == NULL) {
    s_tl.fetch_pending = 0;
    vTaskDelete(NULL);
    return;
  }
  res->gen = my_gen;

  /* Wait for Wi-Fi up to 10 s */
  for (int i = 0; i < 50 && !bb_wifi_is_connected(); i++) {
    vTaskDelay(pdMS_TO_TICKS(200));
  }

  int count = 0;
  res->err = bb_agent_list_dispatch_recent(res->entries, BB_TASK_LIST_MAX, &count);
  res->count = (res->err == ESP_OK && count <= BB_TASK_LIST_MAX) ? count : 0;

  tl_lv_async_call(on_dispatch_fetch_done, res);
  vTaskDelete(NULL);
}

static void spawn_dispatch_fetch_task(void) {
  if (s_tl.fetch_pending) return;
  s_tl.fetch_pending = 1;
  uint32_t gen = ++s_tl.fetch_gen;
  TaskHandle_t t = NULL;
  BaseType_t ok = xTaskCreate(dispatch_fetch_task, "tl_fetch",
                              BB_TL_FETCH_STACK,
                              (void*)(uintptr_t)gen,
                              BB_TL_FETCH_PRIO, &t);
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "spawn_dispatch_fetch_task: xTaskCreate failed");
    s_tl.fetch_pending = 0;
  }
}

/* ── Public API ── */

void bb_ui_task_list_show(void) {
  if (s_tl.visible) return;
  s_tl.visible = 1; /* loading */
  s_tl.count = 0;
  s_tl.sel = 0;
  memset(s_tl.rows, 0, sizeof(s_tl.rows));
  ESP_LOGI(TAG, "task_list: show (fetching dispatch/recent)");
  spawn_dispatch_fetch_task();
}

void bb_ui_task_list_hide(void) {
  s_tl.visible = 0;
  s_tl.fetch_gen++; /* invalidate any in-flight fetch */
  if (s_tl.screen != NULL) {
    /* Destroy overlay — underlying screen (CHAT) is automatically visible again. */
    lv_obj_del(s_tl.screen);
    s_tl.screen = NULL;
    s_tl.lbl_title = NULL;
    s_tl.lbl_empty = NULL;
    memset(s_tl.rows, 0, sizeof(s_tl.rows));
  }
  ESP_LOGI(TAG, "task_list: hidden");
}

int bb_ui_task_list_visible(void) {
  return s_tl.visible;
}

void bb_ui_task_list_move(int delta) {
  if (s_tl.visible != 2 || s_tl.count == 0) return;
  int n = s_tl.count;
  int sel = ((s_tl.sel + delta) % n + n) % n;
  if (sel == s_tl.sel) return;
  s_tl.sel = sel;
  task_list_apply_styles();
}

void bb_ui_task_list_activate(void) {
  if (s_tl.visible != 2 || s_tl.count == 0) return;
  if (s_tl.sel < 0 || s_tl.sel >= s_tl.count) return;

  const bb_agent_dispatch_entry_t* e = &s_tl.entries[s_tl.sel];
  char cmd[80];
  snprintf(cmd, sizeof(cmd), "task_status #%s", e->task_id);
  ESP_LOGI(TAG, "task_list: activating row %d, sending '%s'", s_tl.sel, cmd);

  /* Hide first, then send turn — bb_ui_agent_chat_send requires chat active */
  bb_ui_task_list_hide();
  bb_ui_agent_chat_send(cmd);
}
