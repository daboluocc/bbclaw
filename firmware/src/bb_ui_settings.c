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
 *     (no Back row — encoder long-press exits; footer hint says so)
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

#include "bb_adapter_client.h"
#include "bb_agent_client.h"
#include "bb_audio.h"
#include "bb_config.h"
#include "bb_device_config.h"
#include "bb_ota.h"
#include "bb_radio_app.h"
#include "bb_session_store.h"
#include "bb_transport.h"
#include "bb_ui_agent_chat.h"
#include "bb_ui_theme.h"
#include "esp_log.h"
#include "esp_lvgl_port.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "lvgl.h"

static const char* TAG = "bb_ui_settings";

/* CJK-capable UI font — the same font every other CJK-rendering page binds
 * (chat / locked / task_list / apconfig / ...). The default theme font is
 * montserrat (ASCII-only), so any label that can hold Chinese — the live
 * session titles in the Sessions picker (user conversation first-messages) —
 * renders as tofu boxes unless we bind this explicitly. The static menu
 * labels are English now, but the font also carries 0x20-0x7F so those rows
 * (Driver / Volume / Voice / ...) stay correct too. */
extern const lv_font_t lv_font_bbclaw_cjk;
static const lv_font_t* ui_font(void) { return &lv_font_bbclaw_cjk; }

#define BB_SETTINGS_DRIVER_CACHE_MAX 6
#define BB_SETTINGS_SITE_CACHE_MAX   6

/* 4096 was too small for the HTTPS commit/fetch (TLS handshake on a fresh
 * connection), and v0.5.9's switch to software AES pushed cipher work onto the
 * stack too — selecting/switching a model (commit PUT) could overflow and
 * reboot the device probabilistically. 8192 matches ESP-IDF's HTTPS-task norm.
 *
 * The two READ-ONLY fetch tasks (driver list / sites.list) now allocate their
 * stack from PSRAM via xTaskCreateWithCaps — internal DRAM gets badly
 * fragmented while in Settings (largest free block seen as low as ~7.9KB),
 * so plain xTaskCreate(8192) would fail and the Driver/Adapter rows silently
 * stayed empty ("offline"/"(none)") even though the cloud had online adapters.
 * They do network I/O only (no NVS / flash writes), so a PSRAM stack is safe
 * w.r.t. the flash-cache-disable constraint. The commit/persist task keeps an
 * INTERNAL stack on purpose — it writes NVS (cache frozen). */
#define BB_SETTINGS_FETCH_TASK_STACK 8192
#define BB_SETTINGS_FETCH_TASK_PRIO  4
/* Volume/TTS persist only does an NVS set+commit (no TLS) — a 4KB stack is
 * plenty and, unlike 8KB, reliably fits the fragmented internal heap that
 * starved the old 8KB commit/persist tasks (xTaskCreate failed → settings
 * silently not applied). Must stay INTERNAL: NVS write freezes the flash
 * cache, which would fault on a PSRAM stack. */
#define BB_SETTINGS_PERSIST_TASK_STACK 4096

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
/* Reserve the bottom strip for the footer hint ("Hold to exit") — the
 * "reminder" the list must never overlap. The bbclaw panel is only 172px tall,
 * so the rows live in [HEADER_H, DISP_H - FOOTER_H] and scroll within it. */
#define FOOTER_H 22

/* ── Level / row enums ── */

typedef enum {
  LEVEL_MAIN = 0,
  LEVEL_DRIVER_PICKER,
  LEVEL_MODEL_PICKER,
  LEVEL_ADAPTER_PICKER,
  LEVEL_SESSION_PICKER,
  LEVEL_VOLUME_ADJUST,
} settings_level_t;

/* Logical main-page row ids. MAIN_ROW_ADAPTER (ADR-027) is only shown in
 * cloud_saas mode, so the on-screen row order/count is dynamic — see
 * main_visible_rows(). Cursor index (s_st.sel) indexes the *visible* list,
 * not these ids directly. */
typedef enum {
  MAIN_ROW_DRIVER = 0,
  MAIN_ROW_MODEL,
  MAIN_ROW_ADAPTER,
  MAIN_ROW_SESSIONS,
  MAIN_ROW_VOLUME,
  MAIN_ROW_MIYU,
  MAIN_ROW_FIRMWARE,
  MAIN_ROW_BACK,
  MAIN_ROW_ID_COUNT,
} main_row_t;

/* Firmware row click state — view version, click to check/upgrade (OTA). */
typedef enum {
  OTA_ROW_IDLE = 0, /* show version only */
  OTA_ROW_CHECKING, /* check in flight */
  OTA_ROW_LATEST,   /* checked: already newest */
  OTA_ROW_ERROR,    /* check failed */
  OTA_ROW_CLOUD_ONLY, /* not cloud_saas — OTA unavailable */
} ota_row_status_t;

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

  /* Firmware row OTA check (Settings → Firmware → click). */
  ota_row_status_t ota_status;
  volatile int ota_check_pending;
  volatile uint32_t ota_check_generation;

  /* Persisted active selections from the last successful fetch — what
   * the main page shows as the current value, and what the picker marks
   * with ✓. Updated optimistically on commit so the UI feels snappy
   * without waiting for a re-fetch. */
  char active_driver[24];
  /* active_model is per-driver — we read it off
   * driver_cache[i].active_model directly. */

  /* ADR-027: Home Adapter ("机器") catalog. Populated async (WS sites.list)
   * when the Adapter picker is entered / Settings opened in cloud_saas. */
  bb_site_info_t site_cache[BB_SETTINGS_SITE_CACHE_MAX];
  int site_cache_count;
  volatile int site_fetch_pending;
  volatile uint32_t site_fetch_generation;
  char active_site_id[40]; /* optimistic active selection for snappy UI */
  int pending_chat_sync;   /* 1 = re-sync chat driver/session after site switch */

  /* adapter_v2 P2: Sessions picker catalog. Populated async (HTTP list-sessions
   * for the chat's current driver) when the Sessions picker is entered. */
  bb_agent_session_info_t session_cache[16];
  int session_cache_count;
  volatile int session_fetch_pending;
  volatile uint32_t session_fetch_generation;

  int miyu_enabled; /* 密语(锁屏语音解锁) on/off; loaded from device config on entry (ADR-037) */

  /* Volume adjust state */
  int volume_pct;        /* current working value while in LEVEL_VOLUME_ADJUST */
  int volume_dirty;      /* 1 = changed since entering adjust mode */
  lv_obj_t* vol_fill;    /* fill rect inside the big bar (partial-update ref) */
  lv_obj_t* vol_pct_lbl; /* "NN%" label next to the bar (partial-update ref) */
  lv_obj_t* hint_lbl;    /* footer hint on main page ("hold to go back") */
} settings_state_t;

static settings_state_t s_st = {0};

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
  if (s_st.hint_lbl != NULL) {
    lv_obj_del(s_st.hint_lbl);
    s_st.hint_lbl = NULL;
  }
}

static void build_rows_box(int row_count) {
  destroy_rows();
  s_st.rows_box = lv_obj_create(s_st.root);
  lv_obj_remove_style_all(s_st.rows_box);
  /* Bound the list to the region between the header and the footer hint. When
   * the menu has more rows than fit (e.g. cloud_saas: Adapter/Sessions + Volume
   * /Voice/Miyu/Firmware = 6 rows, ~178px on a 172px panel) the box caps to the
   * available height and scrolls; highlight_selected() keeps the cursor in view.
   * When everything fits, box_h == content so there's nothing to scroll.
   * content = pad_top(4) + rows*ROW_H + pad_bottom(4). */
  int content_h = row_count * ROW_H + 8;
  int avail_h = BBCLAW_ST7789_HEIGHT - HEADER_H - FOOTER_H;
  int box_h = content_h < avail_h ? content_h : avail_h;
  lv_obj_set_size(s_st.rows_box, lv_pct(100), box_h);
  lv_obj_align(s_st.rows_box, LV_ALIGN_TOP_LEFT, 0, HEADER_H);
  lv_obj_set_flex_flow(s_st.rows_box, LV_FLEX_FLOW_COLUMN);
  lv_obj_set_style_pad_all(s_st.rows_box, 4, 0);
  lv_obj_set_style_pad_row(s_st.rows_box, 0, 0); /* exact ROW_H spacing for box_h math */
  /* Encoder-driven vertical scroll; lock the axis to vertical so a long marquee
   * row can't drag the list sideways. MODE_AUTO draws a thumb only when there's
   * actually content off-screen (st>0 || sb>0) — its position + length tells the
   * user where they are and how much is above/below; when the menu fits, no bar.
   * highlight_selected() scrolls the cursor row into view as it moves. */
  lv_obj_set_scroll_dir(s_st.rows_box, LV_DIR_VER);
  lv_obj_set_scrollbar_mode(s_st.rows_box, LV_SCROLLBAR_MODE_AUTO);
  lv_obj_set_style_bg_opa(s_st.rows_box, LV_OPA_TRANSP, 0);
  /* Scrollbar thumb — thin teal accent to match the selection idiom (dot-matrix
   * design language). Needs bg_opa > MIN or LVGL skips drawing it entirely. */
  lv_obj_set_style_width(s_st.rows_box, 3, LV_PART_SCROLLBAR);
  lv_obj_set_style_pad_right(s_st.rows_box, 1, LV_PART_SCROLLBAR);
  lv_obj_set_style_radius(s_st.rows_box, 2, LV_PART_SCROLLBAR);
  lv_obj_set_style_bg_color(s_st.rows_box, lv_color_hex(BB_UI_ACCENT), LV_PART_SCROLLBAR);
  lv_obj_set_style_bg_opa(s_st.rows_box, LV_OPA_70, LV_PART_SCROLLBAR);
  for (int i = 0; i < row_count && i < (int)(sizeof(s_st.rows) / sizeof(s_st.rows[0])); ++i) {
    lv_obj_t* row = lv_label_create(s_st.rows_box);
    lv_obj_set_size(row, lv_pct(100), ROW_H);
    lv_obj_set_style_pad_left(row, 6, 0);
    lv_obj_set_style_pad_right(row, 6, 0);
    lv_obj_set_style_pad_top(row, 4, 0);
    lv_obj_set_style_radius(row, 3, 0);
    lv_obj_set_style_text_color(row, lv_color_hex(UI_ROW_FG), 0);
    lv_obj_set_style_text_font(row, ui_font(), 0);
    /* SCROLL (marquee) so long Chinese session titles in the picker scroll
     * instead of being truncated with "...". The row has a bounded width
     * (lv_pct(100) above), so LVGL only animates when the text overflows;
     * short static menu rows (Driver/Volume/...) stay put. */
    lv_label_set_long_mode(row, LV_LABEL_LONG_MODE_SCROLL);
    s_st.rows[i] = row;
  }
  s_st.rows_used = row_count;
}

static void highlight_selected(void) {
  for (int i = 0; i < s_st.rows_used; ++i) {
    lv_obj_t* row = s_st.rows[i];
    if (row == NULL) continue;
    if (i == s_st.sel) {
      /* 选中行：teal accent 半透明高亮——明显高于未选中行的透明背景。原来用
       * DOT_GHOST(0x152128) COVER，和屏幕背景(0x070b0e)都极暗、对比几乎为零，
       * 看不出选中。改 accent@40% + 左侧 accent 竖条加粗 + 亮白字，一眼可辨。 */
      lv_obj_set_style_bg_color(row, lv_color_hex(BB_UI_ACCENT), 0);
      lv_obj_set_style_bg_opa(row, LV_OPA_40, 0);
      lv_obj_set_style_text_color(row, lv_color_hex(UI_ROW_SEL_FG), 0);
      lv_obj_set_style_border_side(row, LV_BORDER_SIDE_LEFT, 0);
      lv_obj_set_style_border_width(row, 4, 0);
      lv_obj_set_style_border_color(row, lv_color_hex(BB_UI_ACCENT), 0);
    } else {
      lv_obj_set_style_bg_opa(row, LV_OPA_TRANSP, 0);
      lv_obj_set_style_border_width(row, 0, 0);
      lv_obj_set_style_text_color(row, lv_color_hex(UI_ROW_FG), 0);
    }
  }

  /* Keep the cursor visible when the list is taller than the bounded rows_box.
   * update_layout first so the freshly-built/moved rows have current
   * coordinates; lv_obj_scroll_to_view is a no-op when the row already fits. */
  if (s_st.rows_box != NULL && s_st.sel >= 0 && s_st.sel < s_st.rows_used &&
      s_st.rows[s_st.sel] != NULL) {
    lv_obj_update_layout(s_st.rows_box);
    lv_obj_scroll_to_view(s_st.rows[s_st.sel], LV_ANIM_OFF);
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

/* ── Dynamic main-row layout (ADR-027: Adapter row only in cloud_saas) ── */

static int main_visible_rows(main_row_t* out) {
  int n = 0;
  /* Driver/Model are intentionally NOT shown: the agent backend (driver + model)
   * is configured on the adapter side now, not per-device. The picker/fetch code
   * is left in place (harmless, and the driver fetch still drives the post-switch
   * chat re-sync) but is simply unreachable from the menu. */
  if (bb_transport_is_cloud_saas()) {
    out[n++] = MAIN_ROW_ADAPTER;
    out[n++] = MAIN_ROW_SESSIONS;
  }
  out[n++] = MAIN_ROW_VOLUME;
  /* 密语(锁屏语音解锁) only works in cloud_saas (passphrase_unlock_enabled), so
   * the toggle is only meaningful there — like the ADAPTER/SESSIONS rows (ADR-037). */
  if (bb_transport_is_cloud_saas()) {
    out[n++] = MAIN_ROW_MIYU;
  }
  /* Firmware version + OTA upgrade — always shown (version is useful in any
   * mode; click only triggers an OTA check in cloud_saas, else shows a note). */
  out[n++] = MAIN_ROW_FIRMWARE;
  /* No explicit Back row — encoder long-press (BB_NAV_EVENT_BACK) exits at the
   * main level; a footer hint on the main page tells the user. */
  return n;
}

static int main_visible_row_count(void) {
  main_row_t rows[MAIN_ROW_ID_COUNT];
  return main_visible_rows(rows);
}

/* Map a logical row id to its index in the current visible list (for cursor
 * placement). Falls back to 0 if not visible. */
static int main_row_to_index(main_row_t row) {
  main_row_t rows[MAIN_ROW_ID_COUNT];
  int n = main_visible_rows(rows);
  for (int i = 0; i < n; ++i) {
    if (rows[i] == row) return i;
  }
  return 0;
}

/* Label of the currently-active Home Adapter for the main-page Adapter row. */
static const char* current_site_label(void) {
  /* Static scratch — single-threaded LVGL render, freshly filled each call. */
  static char buf[64];
  if (s_st.site_fetch_pending && s_st.site_cache_count == 0) return "(loading)";
  for (int i = 0; i < s_st.site_cache_count; ++i) {
    int is_active = s_st.site_cache[i].active ||
                    (s_st.active_site_id[0] != '\0' &&
                     strcmp(s_st.site_cache[i].home_site_id, s_st.active_site_id) == 0);
    if (is_active) {
      const char* lbl = s_st.site_cache[i].label[0] != '\0' ? s_st.site_cache[i].label
                                                            : s_st.site_cache[i].home_site_id;
      /* Show the bound adapter's name + live status even from the main page. */
      snprintf(buf, sizeof(buf), "%s %s", lbl,
               s_st.site_cache[i].online ? "[on]" : "[offline]");
      return buf;
    }
  }
  if (s_st.active_site_id[0] != '\0') return s_st.active_site_id;
  return "(none)";
}

static void render_main(void) {
  if (s_st.root == NULL) return;
  lv_label_set_text(s_st.header_lbl, "Settings");

  main_row_t rows[MAIN_ROW_ID_COUNT];
  int n = main_visible_rows(rows);
  build_rows_box(n);
  char buf[80];

  const bb_agent_driver_info_t* active = active_driver_entry();

  for (int i = 0; i < n; ++i) {
    switch (rows[i]) {
      case MAIN_ROW_DRIVER: {
        const char* drv_name = (active != NULL) ? active->name :
                                (s_st.driver_fetch_pending ? "loading..." : "(offline)");
        snprintf(buf, sizeof(buf), "Driver: %s", drv_name);
        break;
      }
      case MAIN_ROW_MODEL:
        snprintf(buf, sizeof(buf), "Model: %s", current_model_label(active));
        break;
      case MAIN_ROW_ADAPTER:
        snprintf(buf, sizeof(buf), "Adapter: %s", current_site_label());
        break;
      case MAIN_ROW_SESSIONS:
        /* Static label — the live list (Chinese session titles from
         * session_cache) lives in the picker; this row is just the English
         * menu entry. build_rows_box still binds lv_font_bbclaw_cjk to every
         * row so the picker's CJK titles render. */
        snprintf(buf, sizeof(buf), "Sessions");
        break;
      case MAIN_ROW_VOLUME: {
        int pct = s_st.volume_pct;
        const int BLOCKS = 10;
        int filled = (pct * BLOCKS + 50) / 100;
        if (filled < 0) filled = 0;
        if (filled > BLOCKS) filled = BLOCKS;
        char mini[16];
        int ci = 0;
        mini[ci++] = '[';
        for (int k = 0; k < BLOCKS; k++) {
          mini[ci++] = (k < filled) ? '#' : '-';
        }
        mini[ci++] = ']';
        mini[ci] = '\0';
        snprintf(buf, sizeof(buf), "Volume: %s %d%%", mini, pct);
        break;
      }
      case MAIN_ROW_MIYU:
        snprintf(buf, sizeof(buf), "Miyu: %s", s_st.miyu_enabled ? "on" : "off");
        break;
      case MAIN_ROW_FIRMWARE: {
        const char* ver = bb_ota_get_current_version();
        switch (s_st.ota_status) {
          case OTA_ROW_CHECKING:
            snprintf(buf, sizeof(buf), "Firmware: %s · checking…", ver);
            break;
          case OTA_ROW_LATEST:
            snprintf(buf, sizeof(buf), "Firmware: %s · up to date", ver);
            break;
          case OTA_ROW_ERROR:
            snprintf(buf, sizeof(buf), "Firmware: %s · check failed", ver);
            break;
          case OTA_ROW_CLOUD_ONLY:
            snprintf(buf, sizeof(buf), "Firmware: %s · cloud only", ver);
            break;
          case OTA_ROW_IDLE:
          default:
            snprintf(buf, sizeof(buf), "Firmware: %s", ver);
            break;
        }
        break;
      }
      case MAIN_ROW_BACK:
        snprintf(buf, sizeof(buf), "Back");
        break;
      default:
        buf[0] = '\0';
        break;
    }
    lv_label_set_text(s_st.rows[i], buf);
  }

  highlight_selected();

  /* Footer hint — there is no Back row; long-press exits (see
   * main_visible_rows / bb_ui_settings_handle_back). Cleaned up by
   * destroy_rows on the next render / level change. */
  s_st.hint_lbl = lv_label_create(s_st.root);
  lv_obj_set_style_text_color(s_st.hint_lbl, lv_color_hex(BB_UI_TEXT_DIM), 0);
  lv_label_set_text(s_st.hint_lbl, "Hold to exit");
  lv_obj_align(s_st.hint_lbl, LV_ALIGN_BOTTOM_MID, 0, -4);
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

/* ── Render: Adapter picker (ADR-027 — WS sites.list, async) ── */

static void render_adapter_picker(void) {
  if (s_st.root == NULL) return;
  lv_label_set_text(s_st.header_lbl, "Adapter");

  if (s_st.site_fetch_pending && s_st.site_cache_count == 0) {
    build_rows_box(1);
    lv_label_set_text(s_st.rows[0], "loading...");
    highlight_selected();
    return;
  }

  if (s_st.site_cache_count == 0) {
    build_rows_box(1);
    lv_label_set_text(s_st.rows[0], "(none)");
    highlight_selected();
    return;
  }

  build_rows_box(s_st.site_cache_count);
  for (int i = 0; i < s_st.site_cache_count; ++i) {
    char buf[80];
    const char* lbl = s_st.site_cache[i].label[0] != '\0' ? s_st.site_cache[i].label
                                                          : s_st.site_cache[i].home_site_id;
    int is_active = s_st.site_cache[i].active ||
                    (s_st.active_site_id[0] != '\0' &&
                     strcmp(s_st.site_cache[i].home_site_id, s_st.active_site_id) == 0);
    /* Always show the connectivity status so offline adapters are clearly
     * marked (still selectable — picking one just won't reach it until it
     * comes back online). "*" marks the currently-bound one. */
    snprintf(buf, sizeof(buf), "%s%s  %s", lbl,
             is_active ? "  *" : "",
             s_st.site_cache[i].online ? "[on]" : "[offline]");
    lv_label_set_text(s_st.rows[i], buf);
  }
  highlight_selected();
}

/* ── Render: Sessions picker (adapter_v2 P2 — HTTP list-sessions, async) ──
 *
 * Row 0 is a synthetic "+ New" (new conversation). Rows 1..N are the cached
 * sessions; the one whose id matches the chat's current session is marked "*".
 * Mirrors render_adapter_picker's loading/empty handling.
 *
 * NB: the session TITLES below are dynamic content from session_cache (the
 * user's real conversation first-messages, usually Chinese) — they stay as-is
 * and rely on the CJK font bound to every row in build_rows_box. Only the
 * static chrome (header, "+ New", loading) is English. */

static void render_session_picker(void) {
  if (s_st.root == NULL) return;
  lv_label_set_text(s_st.header_lbl, "Sessions");

  if (s_st.session_fetch_pending && s_st.session_cache_count == 0) {
    /* Still loading the first page — show the New row + a Loading hint. */
    build_rows_box(2);
    lv_label_set_text(s_st.rows[0], "+ New");
    lv_label_set_text(s_st.rows[1], "loading...");
    highlight_selected();
    return;
  }

  const char* cur = bb_ui_agent_chat_get_current_session();
  int rows = s_st.session_cache_count + 1; /* +1 synthetic New row */
  build_rows_box(rows);
  lv_label_set_text(s_st.rows[0], "+ New");
  for (int i = 0; i < s_st.session_cache_count && s_st.rows[i + 1] != NULL; ++i) {
    char buf[80];
    const bb_agent_session_info_t* s = &s_st.session_cache[i];
    char shortid[9];
    const char* label;
    if (s->title[0] != '\0') {
      label = s->title;
    } else {
      /* No title — use the first 8 chars of the id. */
      strncpy(shortid, s->id, sizeof(shortid) - 1);
      shortid[sizeof(shortid) - 1] = '\0';
      label = shortid;
    }
    int is_active = (cur != NULL && cur[0] != '\0' && strcmp(s->id, cur) == 0);
    snprintf(buf, sizeof(buf), "%s%s", label, is_active ? "  *" : "");
    lv_label_set_text(s_st.rows[i + 1], buf);
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

/* ── Render: Volume adjust page ── */

#define VOL_BAR_X      8
#define VOL_BAR_Y      48
#define VOL_BAR_H      28
#define VOL_BAR_BORDER 2
#define VOL_PCT_LABEL_Y  84

static void render_volume_adjust(void) {
  if (s_st.root == NULL) return;
  lv_label_set_text(s_st.header_lbl, "Volume");

  /* Reuse rows_box as a plain container for volume adjust widgets so that
   * destroy_rows() cleans them all up when leaving this level. */
  destroy_rows();
  s_st.rows_box = lv_obj_create(s_st.root);
  lv_obj_remove_style_all(s_st.rows_box);
  lv_obj_set_size(s_st.rows_box, lv_pct(100), lv_pct(100) - HEADER_H);
  lv_obj_align(s_st.rows_box, LV_ALIGN_TOP_LEFT, 0, HEADER_H);
  lv_obj_clear_flag(s_st.rows_box, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_set_style_bg_opa(s_st.rows_box, LV_OPA_TRANSP, 0);

  /* Compute bar width from display width */
  lv_coord_t disp_w = lv_obj_get_width(s_st.root);
  lv_coord_t bar_w = disp_w - VOL_BAR_X * 2;
  lv_coord_t inner_w = bar_w - VOL_BAR_BORDER * 2;
  int pct = s_st.volume_pct;
  lv_coord_t fill_w = (lv_coord_t)(pct * (int)inner_w / 100);
  if (fill_w < 0) fill_w = 0;
  if (fill_w > inner_w) fill_w = inner_w;

  /* Bar container */
  lv_obj_t* container = lv_obj_create(s_st.rows_box);
  lv_obj_remove_style_all(container);
  lv_obj_set_size(container, bar_w, VOL_BAR_H);
  lv_obj_set_pos(container, VOL_BAR_X, VOL_BAR_Y - HEADER_H);
  lv_obj_clear_flag(container, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_set_style_bg_opa(container, LV_OPA_TRANSP, 0);

  /* Fill rect (accent color) */
  lv_obj_t* fill = lv_obj_create(container);
  lv_obj_remove_style_all(fill);
  lv_obj_set_size(fill, fill_w, VOL_BAR_H - VOL_BAR_BORDER * 2);
  lv_obj_set_pos(fill, VOL_BAR_BORDER, VOL_BAR_BORDER);
  lv_obj_set_style_radius(fill, 2, 0);
  lv_obj_set_style_bg_color(fill, lv_color_hex(BB_UI_ACCENT), 0);
  lv_obj_set_style_bg_opa(fill, LV_OPA_COVER, 0);
  s_st.vol_fill = fill;

  /* Border frame on top */
  lv_obj_t* frame = lv_obj_create(container);
  lv_obj_remove_style_all(frame);
  lv_obj_set_size(frame, bar_w, VOL_BAR_H);
  lv_obj_set_pos(frame, 0, 0);
  lv_obj_set_style_radius(frame, 3, 0);
  lv_obj_set_style_border_width(frame, VOL_BAR_BORDER, 0);
  lv_obj_set_style_border_color(frame, lv_color_hex(BB_UI_ACCENT), 0);
  lv_obj_set_style_border_opa(frame, LV_OPA_COVER, 0);
  lv_obj_set_style_bg_opa(frame, LV_OPA_0, 0);
  lv_obj_clear_flag(frame, LV_OBJ_FLAG_SCROLLABLE);

  /* Percentage label */
  lv_obj_t* pct_lbl = lv_label_create(s_st.rows_box);
  lv_obj_set_style_text_color(pct_lbl, lv_color_hex(BB_UI_DOT_LIT), 0);
  lv_obj_set_style_text_font(pct_lbl, ui_font(), 0);
  char buf[16];
  snprintf(buf, sizeof(buf), "%d%%", pct);
  lv_label_set_text(pct_lbl, buf);
  lv_obj_set_pos(pct_lbl, VOL_BAR_X, VOL_PCT_LABEL_Y - HEADER_H);
  s_st.vol_pct_lbl = pct_lbl;

  /* Hint label */
  lv_obj_t* hint = lv_label_create(s_st.rows_box);
  lv_obj_set_style_text_color(hint, lv_color_hex(BB_UI_TEXT_DIM), 0);
  lv_obj_set_style_text_font(hint, ui_font(), 0);
  lv_label_set_text(hint, "Up/Down adjust  OK to save");
  lv_obj_set_pos(hint, VOL_BAR_X, VOL_PCT_LABEL_Y - HEADER_H + 20);
}

/* Partial update — only change fill width and label; no object rebuild */
static void update_volume_bar(int pct) {
  if (s_st.vol_fill == NULL || s_st.vol_pct_lbl == NULL) return;
  lv_coord_t disp_w = lv_obj_get_width(s_st.root);
  lv_coord_t bar_w = disp_w - VOL_BAR_X * 2;
  lv_coord_t inner_w = bar_w - VOL_BAR_BORDER * 2;
  lv_coord_t fill_w = (lv_coord_t)(pct * (int)inner_w / 100);
  if (fill_w < 0) fill_w = 0;
  if (fill_w > inner_w) fill_w = inner_w;
  lv_obj_set_width(s_st.vol_fill, fill_w);
  char buf[16];
  snprintf(buf, sizeof(buf), "%d%%", pct);
  lv_label_set_text(s_st.vol_pct_lbl, buf);
}

static void rerender(void) {
  switch (s_st.level) {
    case LEVEL_MAIN:           render_main(); break;
    case LEVEL_DRIVER_PICKER:  render_driver_picker(); break;
    case LEVEL_MODEL_PICKER:   render_model_picker(); break;
    case LEVEL_ADAPTER_PICKER: render_adapter_picker(); break;
    case LEVEL_SESSION_PICKER: render_session_picker(); break;
    case LEVEL_VOLUME_ADJUST:  render_volume_adjust(); break;
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
  /* ADR-027 §4: a Home Adapter switch requested a chat context re-sync. Use the
   * adapter-switch path (NOT set_active_driver) — set_active_driver no-ops when
   * the driver name is unchanged, but both machines usually expose the same
   * driver ("claude-code"), so that no-op left the chat stuck on the OLD
   * machine's session + history. resync_after_adapter_switch always drops the
   * stale session and pulls the new machine's sessions live. We run inside an
   * lv_async_call (LVGL task), so direct chat calls are safe here. */
  if (s_st.pending_chat_sync && s_st.active_driver[0] != '\0') {
    s_st.pending_chat_sync = 0;
    esp_err_t cerr = bb_ui_agent_chat_resync_after_adapter_switch(s_st.active_driver);
    if (cerr != ESP_OK) {
      ESP_LOGW(TAG, "site switch: chat resync failed (%s)", esp_err_to_name(cerr));
    }
  }
  rerender();
  free(r);
}

static void driver_fetch_task(void* arg) {
  driver_fetch_result_t* r = (driver_fetch_result_t*)arg;
  if (r == NULL) {
    s_st.driver_fetch_pending = 0;
    vTaskDeleteWithCaps(NULL); /* stack is PSRAM-allocated (xTaskCreateWithCaps) */
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
  vTaskDeleteWithCaps(NULL);
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
  /* PSRAM stack — internal DRAM is too fragmented in Settings to spare 8KB. */
  BaseType_t ok = xTaskCreateWithCaps(driver_fetch_task, "drv_fetch",
                                      BB_SETTINGS_FETCH_TASK_STACK, r,
                                      BB_SETTINGS_FETCH_TASK_PRIO, &t,
                                      BBCLAW_MALLOC_CAP_PREFER_PSRAM);
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "spawn_driver_fetch_task: xTaskCreateWithCaps failed");
    s_st.driver_fetch_pending = 0;
    free(r);
  }
}

/* ── Firmware OTA check — Settings → Firmware row click, async ──
 * Mirrors the driver-fetch pattern: run the blocking bb_ota_check() on a PSRAM
 * stack task, marshal the result back to the LVGL thread. On has_update, hand
 * off to bb_radio_app_present_ota_update() which shows the existing confirm page
 * (lv_layer_top, intercepts OK/BACK over the Settings overlay) and routes accept
 * into the preheated apply task. Otherwise just update the row status text. */

typedef struct {
  uint32_t gen;
  esp_err_t err;
  ota_update_info_t info;
} ota_check_result_t;

static void on_ota_check_done(void* user_data) {
  ota_check_result_t* r = (ota_check_result_t*)user_data;
  if (r == NULL) return;
  s_st.ota_check_pending = 0;
  if (!s_st.active || r->gen != s_st.ota_check_generation) {
    free(r);
    return;
  }
  if (r->err != ESP_OK) {
    ESP_LOGW(TAG, "ota check failed: %s", esp_err_to_name(r->err));
    s_st.ota_status = OTA_ROW_ERROR;
    rerender();
  } else if (r->info.has_update) {
    ESP_LOGI(TAG, "ota check: update %s available -> confirm", r->info.version);
    s_st.ota_status = OTA_ROW_IDLE; /* confirm page takes over the screen */
    rerender();
    bb_radio_app_present_ota_update(&r->info);
  } else {
    ESP_LOGI(TAG, "ota check: already up to date");
    s_st.ota_status = OTA_ROW_LATEST;
    rerender();
  }
  free(r);
}

static void ota_check_task(void* arg) {
  ota_check_result_t* r = (ota_check_result_t*)arg;
  if (r == NULL) {
    s_st.ota_check_pending = 0;
    vTaskDeleteWithCaps(NULL);
    return;
  }
  r->err = bb_ota_check(&r->info);
  if (lvgl_port_lock(200)) {
    lv_async_call(on_ota_check_done, r);
    lvgl_port_unlock();
  } else {
    s_st.ota_check_pending = 0;
    free(r);
  }
  vTaskDeleteWithCaps(NULL);
}

static void spawn_ota_check_task(void) {
  if (s_st.ota_check_pending) return;
  s_st.ota_check_pending = 1;
  s_st.ota_status = OTA_ROW_CHECKING;
  uint32_t gen = ++s_st.ota_check_generation;
  ota_check_result_t* r = (ota_check_result_t*)calloc(1, sizeof(*r));
  if (r == NULL) {
    ESP_LOGE(TAG, "spawn_ota_check_task: calloc failed");
    s_st.ota_check_pending = 0;
    s_st.ota_status = OTA_ROW_ERROR;
    return;
  }
  r->gen = gen;
  TaskHandle_t t = NULL;
  /* Larger PSRAM stack than the driver fetch: bb_ota_check() does an HTTP(S) GET
   * to the cloud, and the TLS handshake (mbedTLS + cert bundle) is stack-hungry. */
  BaseType_t ok = xTaskCreateWithCaps(ota_check_task, "ota_check", 16384, r,
                                      BB_SETTINGS_FETCH_TASK_PRIO, &t,
                                      BBCLAW_MALLOC_CAP_PREFER_PSRAM);
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "spawn_ota_check_task: xTaskCreateWithCaps failed");
    s_st.ota_check_pending = 0;
    s_st.ota_status = OTA_ROW_ERROR;
    free(r);
  }
  rerender();
}

/* ── Site (Home Adapter) fetch — ADR-027, WS sites.list, async ── */

typedef struct {
  uint32_t gen;
  esp_err_t err;
  int count;
  bb_site_info_t sites[BB_SETTINGS_SITE_CACHE_MAX];
} site_fetch_result_t;

static void on_site_fetch_done(void* user_data) {
  site_fetch_result_t* r = (site_fetch_result_t*)user_data;
  if (r == NULL) return;
  s_st.site_fetch_pending = 0;
  if (!s_st.active || r->gen != s_st.site_fetch_generation) {
    free(r);
    return;
  }
  if (r->err != ESP_OK || r->count <= 0) {
    ESP_LOGW(TAG, "site fetch failed (%s) count=%d", esp_err_to_name(r->err), r->count);
    s_st.site_cache_count = 0;
  } else {
    int total = r->count > BB_SETTINGS_SITE_CACHE_MAX ? BB_SETTINGS_SITE_CACHE_MAX : r->count;
    memcpy(s_st.site_cache, r->sites, sizeof(r->sites[0]) * (size_t)total);
    s_st.site_cache_count = total;
    /* Seed active_site_id from the reply's active flag (cloud truth). */
    char prev_active[sizeof(s_st.active_site_id)];
    strncpy(prev_active, s_st.active_site_id, sizeof(prev_active) - 1);
    prev_active[sizeof(prev_active) - 1] = '\0';
    for (int i = 0; i < total; ++i) {
      if (r->sites[i].active) {
        strncpy(s_st.active_site_id, r->sites[i].home_site_id, sizeof(s_st.active_site_id) - 1);
        s_st.active_site_id[sizeof(s_st.active_site_id) - 1] = '\0';
        break;
      }
    }
    /* Persist when the cloud's active differs from what we had (covers a binding
     * set from another device/web), so the NVS default stays in sync. */
    if (s_st.active_site_id[0] != '\0' && strcmp(s_st.active_site_id, prev_active) != 0) {
      bb_session_store_save_active_site(s_st.active_site_id);
    }
    ESP_LOGI(TAG, "site fetch ok: %d sites active='%s'", total, s_st.active_site_id);
  }
  if (s_st.level == LEVEL_ADAPTER_PICKER || s_st.level == LEVEL_MAIN) {
    rerender();
  }
  free(r);
}

static void site_fetch_task(void* arg) {
  site_fetch_result_t* r = (site_fetch_result_t*)arg;
  if (r == NULL) {
    s_st.site_fetch_pending = 0;
    vTaskDeleteWithCaps(NULL); /* stack is PSRAM-allocated (xTaskCreateWithCaps) */
    return;
  }
  r->err = bb_adapter_sites_list(r->sites, BB_SETTINGS_SITE_CACHE_MAX, &r->count);
  /* Hand the result back on the LVGL thread. Retry the lock a few times; if we
   * still can't get it, clear the pending flag directly and drop the result so
   * the picker is never wedged on "(loading)" forever. (Leaving site_fetch_pending
   * set would also block every future refresh, since spawn_site_fetch_task
   * early-returns while a fetch is pending.) */
  int delivered = 0;
  for (int attempt = 0; attempt < 3 && !delivered; ++attempt) {
    if (lvgl_port_lock(200)) {
      lv_async_call(on_site_fetch_done, r);
      lvgl_port_unlock();
      delivered = 1;
    }
  }
  if (!delivered) {
    ESP_LOGW(TAG, "site fetch: lvgl lock unavailable, clearing pending");
    s_st.site_fetch_pending = 0;
    free(r);
  }
  vTaskDeleteWithCaps(NULL);
}

static void spawn_site_fetch_task(void) {
  if (!bb_transport_is_cloud_saas()) return;
  if (s_st.site_fetch_pending) return;
  s_st.site_fetch_pending = 1;
  uint32_t gen = ++s_st.site_fetch_generation;

  site_fetch_result_t* r = (site_fetch_result_t*)calloc(1, sizeof(*r));
  if (r == NULL) {
    ESP_LOGE(TAG, "spawn_site_fetch_task: calloc failed");
    s_st.site_fetch_pending = 0;
    return;
  }
  r->gen = gen;
  TaskHandle_t t = NULL;
  /* PSRAM stack — internal DRAM is too fragmented in Settings to spare 8KB. */
  BaseType_t ok = xTaskCreateWithCaps(site_fetch_task, "site_fetch",
                                      BB_SETTINGS_FETCH_TASK_STACK, r,
                                      BB_SETTINGS_FETCH_TASK_PRIO, &t,
                                      BBCLAW_MALLOC_CAP_PREFER_PSRAM);
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "spawn_site_fetch_task: xTaskCreateWithCaps failed");
    s_st.site_fetch_pending = 0;
    free(r);
  }
}

/* ── Session fetch (adapter_v2 P2 — HTTP list-sessions, async) ── */

typedef struct {
  uint32_t gen;
  esp_err_t err;
  int count;
  bb_agent_session_info_t sessions[16];
} session_fetch_result_t;

static void on_session_fetch_done(void* user_data) {
  session_fetch_result_t* r = (session_fetch_result_t*)user_data;
  if (r == NULL) return;
  s_st.session_fetch_pending = 0;
  if (!s_st.active || r->gen != s_st.session_fetch_generation) {
    free(r);
    return;
  }
  if (r->err != ESP_OK || r->count <= 0) {
    ESP_LOGW(TAG, "session fetch failed (%s) count=%d", esp_err_to_name(r->err), r->count);
    s_st.session_cache_count = 0;
  } else {
    /* Cap to 15 so the synthetic "+ New" row + sessions fit the 16-slot
     * rows[] LVGL label array (rows[16] would be NULL → crash). */
    int total = r->count > 15 ? 15 : r->count;
    memcpy(s_st.session_cache, r->sessions, sizeof(r->sessions[0]) * (size_t)total);
    s_st.session_cache_count = total;
    ESP_LOGI(TAG, "session fetch ok: %d sessions (capped from %d)", total, r->count);
  }
  if (s_st.level == LEVEL_SESSION_PICKER) {
    rerender();
  }
  free(r);
}

static void session_fetch_task(void* arg) {
  session_fetch_result_t* r = (session_fetch_result_t*)arg;
  if (r == NULL) {
    s_st.session_fetch_pending = 0;
    vTaskDeleteWithCaps(NULL); /* stack is PSRAM-allocated (xTaskCreateWithCaps) */
    return;
  }
  r->err = bb_agent_list_sessions(bb_ui_agent_chat_get_current_driver(), r->sessions, 16, &r->count);
  /* Hand the result back on the LVGL thread. Retry the lock a few times; if we
   * still can't get it, clear the pending flag directly so the picker is never
   * wedged on "Loading..." forever (a stuck pending flag would also block every
   * future refresh — spawn early-returns while pending). */
  int delivered = 0;
  for (int attempt = 0; attempt < 3 && !delivered; ++attempt) {
    if (lvgl_port_lock(200)) {
      lv_async_call(on_session_fetch_done, r);
      lvgl_port_unlock();
      delivered = 1;
    }
  }
  if (!delivered) {
    ESP_LOGW(TAG, "session fetch: lvgl lock unavailable, clearing pending");
    s_st.session_fetch_pending = 0;
    free(r);
  }
  vTaskDeleteWithCaps(NULL);
}

static void spawn_session_fetch_task(void) {
  if (!bb_transport_is_cloud_saas()) return;
  if (s_st.session_fetch_pending) return;
  s_st.session_fetch_pending = 1;
  uint32_t gen = ++s_st.session_fetch_generation;

  session_fetch_result_t* r = (session_fetch_result_t*)calloc(1, sizeof(*r));
  if (r == NULL) {
    ESP_LOGE(TAG, "spawn_session_fetch_task: calloc failed");
    s_st.session_fetch_pending = 0;
    return;
  }
  r->gen = gen;
  TaskHandle_t t = NULL;
  /* PSRAM stack — internal DRAM is too fragmented in Settings to spare 8KB,
   * and list-sessions is network I/O only (HTTPS, no NVS) so PSRAM is safe. */
  BaseType_t ok = xTaskCreateWithCaps(session_fetch_task, "sess_fetch",
                                      BB_SETTINGS_FETCH_TASK_STACK, r,
                                      BB_SETTINGS_FETCH_TASK_PRIO, &t,
                                      BBCLAW_MALLOC_CAP_PREFER_PSRAM);
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "spawn_session_fetch_task: xTaskCreateWithCaps failed");
    s_st.session_fetch_pending = 0;
    free(r);
  }
}

/* ADR-027 §4: after the active binding flips, re-pull agent state from the
 * now-active adapter (driver/model catalog + chat session context) and refresh
 * the site list so its `active` flag reflects cloud truth. Runs on the LVGL
 * thread (dispatched via lv_async_call). */
static void on_adapter_activated(void* arg) {
  (void)arg;
  if (!s_st.active) return;
  s_st.pending_chat_sync = 1;
  spawn_driver_fetch_task();
  spawn_site_fetch_task();
}

/* sites.activate failed — reconcile the optimistic active mark against cloud
 * truth by re-pulling the list (no driver/chat re-sync; binding didn't move). */
static void on_adapter_activate_failed(void* arg) {
  (void)arg;
  if (!s_st.active) return;
  spawn_site_fetch_task();
}

/* ── Commit task (async PUT) ── */

typedef enum {
  COMMIT_KIND_DRIVER = 0,
  COMMIT_KIND_MODEL,
  COMMIT_KIND_VOLUME,  /* int_val = volume pct 0-100 */
  COMMIT_KIND_MIYU,    /* int_val = 0/1 → bb_device_config_set_miyu (ADR-037) */
  COMMIT_KIND_ADAPTER, /* site_id = target homeSiteId (WS sites.activate) */
  COMMIT_KIND_PERSIST_DRIVER, /* driver_name → NVS only (split off DRIVER's
                               * HTTPS PUT so the PUT can run on a PSRAM stack
                               * while this tiny NVS write stays internal) */
} commit_kind_t;

/* Forward decl: DRIVER commit (PSRAM stack) defers its NVS write here. */
static void spawn_persist_driver(const char* driver_name);

typedef struct {
  commit_kind_t kind;
  char driver_name[24];
  char model_id[40];
  char site_id[40];
  int int_val;
  UBaseType_t mem_caps; /* 0 = internal (xTaskCreate); else PSRAM caps for
                         * xTaskCreateWithCaps. Network-only commits (MODEL PUT,
                         * ADAPTER WS-activate) run on a PSRAM stack to dodge
                         * internal-DRAM fragmentation; NVS-touching kinds
                         * (DRIVER/VOLUME/TTS) stay internal. Drives the matching
                         * self-delete (vTaskDeleteWithCaps vs vTaskDelete). */
} commit_payload_t;

static void commit_task(void* arg) {
  commit_payload_t* p = (commit_payload_t*)arg;
  if (p == NULL) {
    vTaskDelete(NULL);
    return;
  }
  esp_err_t err = ESP_OK;
  if (p->kind == COMMIT_KIND_PERSIST_DRIVER) {
    /* NVS-only tail of a DRIVER commit — runs on a small INTERNAL stack so the
     * cache-freezing NVS write is safe. */
    bb_session_store_save_active_driver(p->driver_name);
    ESP_LOGI(TAG, "persist active driver='%s' (nvs)", p->driver_name);
  } else if (p->kind == COMMIT_KIND_DRIVER) {
    err = bb_agent_set_active_driver(p->driver_name);
    if (err == ESP_OK) {
      /* NVS write can't run on this PSRAM stack (cache freeze) — defer it to a
       * tiny internal task. */
      spawn_persist_driver(p->driver_name);
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
  } else if (p->kind == COMMIT_KIND_MODEL) {
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
  } else if (p->kind == COMMIT_KIND_VOLUME) {
    err = bb_device_config_set_volume_pct(p->int_val);
    ESP_LOGI(TAG, "commit volume=%d%% -> %s", p->int_val, esp_err_to_name(err));
  } else if (p->kind == COMMIT_KIND_MIYU) {
    err = bb_device_config_set_miyu(p->int_val);
    ESP_LOGI(TAG, "commit miyu=%d -> %s", p->int_val, esp_err_to_name(err));
  } else if (p->kind == COMMIT_KIND_ADAPTER) {
    /* ADR-027: WS sites.activate (cloud-terminated). On success the active
     * binding has flipped; refresh agent state from the new adapter. On
     * failure the optimistic UI selection is left as-is and the next picker
     * open / refresh will reconcile from cloud truth. */
    char active_id[40] = {0};
    char err_code[32] = {0};
    err = bb_adapter_sites_activate(p->site_id, active_id, sizeof(active_id), err_code, sizeof(err_code));
    ESP_LOGI(TAG, "commit adapter site='%s' -> %s (active='%s' code='%s')",
             p->site_id, esp_err_to_name(err), active_id, err_code);
    if (err == ESP_OK) {
      /* Remember the chosen adapter so next boot's Settings shows it as default
       * even before/without a fresh sites.list (save_active_site spawns its own
       * internal-stack writer, safe to call from this PSRAM commit task). */
      bb_session_store_save_active_site(active_id[0] != '\0' ? active_id : p->site_id);
    }
    if (lvgl_port_lock(200)) {
      lv_async_call(err == ESP_OK ? on_adapter_activated : on_adapter_activate_failed, NULL);
      lvgl_port_unlock();
    } else {
      ESP_LOGW(TAG, "commit adapter: lvgl_port_lock timeout, agent state will sync on next entry");
    }
  }
  /* Diagnostic (crash hunt): how close did the TLS commit get to overflowing
   * the stack? Words remaining ×4 = bytes. A small number here at 8192 confirms
   * the old 4096 stack was overflowing on model/driver commit. */
  ESP_LOGI(TAG, "commit_task done kind=%d stack_min_free_bytes=%u", (int)p->kind,
           (unsigned)(uxTaskGetStackHighWaterMark(NULL) * sizeof(StackType_t)));
  UBaseType_t caps = p->mem_caps;
  free(p);
  /* Match the allocator: WithCaps tasks must be torn down with the WithCaps
   * variant so the PSRAM-backed stack/TCB are freed. */
  if (caps != 0) {
    vTaskDeleteWithCaps(NULL);
  } else {
    vTaskDelete(NULL);
  }
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
  /* Both DRIVER and MODEL commits are HTTPS PUTs → PSRAM stack, dodging the
   * fragmented internal heap. DRIVER's NVS write is deferred to a small
   * internal task (COMMIT_KIND_PERSIST_DRIVER) so this PUT stays PSRAM-safe. */
  p->mem_caps = BBCLAW_MALLOC_CAP_PREFER_PSRAM;
  TaskHandle_t t = NULL;
  BaseType_t ok = xTaskCreateWithCaps(commit_task, "drv_commit",
                                      BB_SETTINGS_FETCH_TASK_STACK, p,
                                      BB_SETTINGS_FETCH_TASK_PRIO, &t, p->mem_caps);
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "spawn_commit_task: xTaskCreateWithCaps failed (kind=%d)", (int)kind);
    free(p);
  }
}

/* ADR-027: commit a Home Adapter switch (WS sites.activate) on a fresh task —
 * the call blocks on the WS round-trip, so it must not run on the LVGL thread. */
static void spawn_commit_adapter(const char* home_site_id) {
  if (home_site_id == NULL || home_site_id[0] == '\0') return;
  commit_payload_t* p = (commit_payload_t*)calloc(1, sizeof(*p));
  if (p == NULL) {
    ESP_LOGE(TAG, "spawn_commit_adapter: calloc failed");
    return;
  }
  p->kind = COMMIT_KIND_ADAPTER;
  strncpy(p->site_id, home_site_id, sizeof(p->site_id) - 1);
  /* WS sites.activate only (no TLS handshake, no NVS) → PSRAM stack. */
  p->mem_caps = BBCLAW_MALLOC_CAP_PREFER_PSRAM;
  TaskHandle_t t = NULL;
  BaseType_t ok = xTaskCreateWithCaps(commit_task, "site_commit",
                                      BB_SETTINGS_FETCH_TASK_STACK, p,
                                      BB_SETTINGS_FETCH_TASK_PRIO, &t, p->mem_caps);
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "spawn_commit_adapter: xTaskCreateWithCaps failed");
    free(p);
  }
}

/* Persist a simple integer setting (volume / miyu) on a fresh task. NVS/flash
 * writes freeze the cache; the stream_task that drives Settings nav has its
 * stack in PSRAM, so writing from there panics
 * (s_task_stack_is_sane_when_cache_frozen). xTaskCreate stacks live in
 * internal RAM, so the write is safe here. */
static void spawn_persist_int(commit_kind_t kind, int val) {
  commit_payload_t* p = (commit_payload_t*)calloc(1, sizeof(*p));
  if (p == NULL) {
    ESP_LOGE(TAG, "spawn_persist_int: calloc failed");
    return;
  }
  p->kind = kind;
  p->int_val = val;
  p->mem_caps = 0; /* internal stack — NVS write freezes flash cache */
  TaskHandle_t t = NULL;
  BaseType_t ok = xTaskCreate(commit_task, "set_persist",
                              BB_SETTINGS_PERSIST_TASK_STACK, p,
                              BB_SETTINGS_FETCH_TASK_PRIO, &t);
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "spawn_persist_int: xTaskCreate failed");
    free(p);
  }
}

/* NVS tail of a DRIVER commit (active-driver name). Small INTERNAL stack —
 * the NVS write freezes the flash cache, so it cannot run on a PSRAM stack. */
static void spawn_persist_driver(const char* driver_name) {
  if (driver_name == NULL || driver_name[0] == '\0') return;
  commit_payload_t* p = (commit_payload_t*)calloc(1, sizeof(*p));
  if (p == NULL) {
    ESP_LOGE(TAG, "spawn_persist_driver: calloc failed");
    return;
  }
  p->kind = COMMIT_KIND_PERSIST_DRIVER;
  strncpy(p->driver_name, driver_name, sizeof(p->driver_name) - 1);
  p->mem_caps = 0; /* internal stack — NVS write freezes flash cache */
  TaskHandle_t t = NULL;
  BaseType_t ok = xTaskCreate(commit_task, "drv_nvs",
                              BB_SETTINGS_PERSIST_TASK_STACK, p,
                              BB_SETTINGS_FETCH_TASK_PRIO, &t);
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "spawn_persist_driver: xTaskCreate failed");
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

/* ADR-027: open the Adapter picker. List arrives async (WS sites.list); show
 * cached entries immediately and kick a refresh so online/active stay current. */
static void enter_adapter_picker(void) {
  s_st.level = LEVEL_ADAPTER_PICKER;
  s_st.sel = 0;
  for (int i = 0; i < s_st.site_cache_count; ++i) {
    int is_active = s_st.site_cache[i].active ||
                    (s_st.active_site_id[0] != '\0' &&
                     strcmp(s_st.site_cache[i].home_site_id, s_st.active_site_id) == 0);
    if (is_active) {
      s_st.sel = i;
      break;
    }
  }
  spawn_site_fetch_task();
  rerender();
}

/* adapter_v2 P2: open the Sessions picker. Cursor lands on the synthetic New
 * row; the list arrives async (HTTP list-sessions for the chat's current
 * driver). */
static void enter_session_picker(void) {
  s_st.level = LEVEL_SESSION_PICKER;
  s_st.sel = 0;
  spawn_session_fetch_task();
  rerender();
}

static void return_to_main(main_row_t row) {
  s_st.level = LEVEL_MAIN;
  s_st.sel = main_row_to_index(row);
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

  /* Initialise level + cursor before any LVGL work. Driver/Model are hidden now,
   * so start the cursor on the first VISIBLE row rather than MAIN_ROW_DRIVER. */
  s_st.level = LEVEL_MAIN;
  {
    main_row_t vrows[MAIN_ROW_ID_COUNT];
    int vn = main_visible_rows(vrows);
    s_st.sel = vn > 0 ? vrows[0] : MAIN_ROW_VOLUME;
  }
  /* Load current volume from persisted config. */
  s_st.volume_pct = bb_device_config_get()->volume_pct;
  s_st.miyu_enabled = bb_device_config_get()->miyu_enabled; /* ADR-037: 密语开关行 */
  s_st.volume_dirty = 0;
  s_st.vol_fill = NULL;
  s_st.vol_pct_lbl = NULL;
  s_st.ota_status = OTA_ROW_IDLE; /* fresh each session — no stale check result */
  /* Seed active_driver from NVS cache so the first paint of the main page
   * shows something sensible even before the async fetch lands. */
  if (s_st.active_driver[0] == '\0') {
    bb_session_store_load_active_driver(s_st.active_driver, sizeof(s_st.active_driver));
  }
  /* Seed the active adapter from NVS too, so the "Adapter:" row shows the last
   * selected machine immediately — even if the sites.list fetch is slow or fails
   * right after boot (routing is server-side; this is for display/default). The
   * fetch refreshes it (incl. live on/offline) when it lands. */
  if (s_st.active_site_id[0] == '\0') {
    bb_session_store_load_active_site(s_st.active_site_id, sizeof(s_st.active_site_id));
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
  lv_obj_set_style_text_font(s_st.header_lbl, ui_font(), 0);
  lv_obj_set_style_pad_left(s_st.header_lbl, 6, 0);
  lv_obj_set_style_pad_top(s_st.header_lbl, 4, 0);

  s_st.active = 1;

  /* Kick off async driver/model fetch. apply renders a stale (or empty)
   * snapshot in the meantime — on_driver_fetch_done re-renders when done. */
  s_st.pending_chat_sync = 0;
  spawn_driver_fetch_task();
  /* ADR-027: in cloud_saas, also pull the Home Adapter list so the main-page
   * "Adapter: <label>" row shows the active machine on first paint. */
  spawn_site_fetch_task();
  rerender();

  ESP_LOGI(TAG, "show level=MAIN");
}

void bb_ui_settings_hide(void) {
  if (!s_st.active) return;
  s_st.active = 0;
  s_st.driver_fetch_generation++;
  s_st.site_fetch_generation++;
  s_st.session_fetch_generation++;
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

void bb_ui_settings_notify_volume_pct(int pct) {
  /* Live-refresh the displayed volume when it changes underneath us (cloud
   * heartbeat applied a remote `device set-volume`). When Settings is CLOSED
   * this is a no-op — bb_ui_settings_show() re-reads the persisted config on
   * entry, so the next open already shows the latest value. When it is OPEN the
   * value was cached at entry, so we update it here. Caller must hold
   * lvgl_port_lock (same contract as the other handlers). */
  if (!s_st.active) return;
  if (s_st.volume_dirty) return; /* user is mid-edit; never clobber their pending value */
  if (pct < 0) pct = 0;
  if (pct > 100) pct = 100;
  if (s_st.volume_pct == pct) return;
  s_st.volume_pct = pct;
  if (s_st.vol_fill != NULL && s_st.vol_pct_lbl != NULL) {
    update_volume_bar(pct); /* on the volume-adjust page: partial-update bar + "NN%" */
  } else if (s_st.level == LEVEL_MAIN) {
    rerender(); /* on the main list: redraw so the "Volume  NN%" row updates */
  }
  ESP_LOGI(TAG, "volume refreshed from cloud pct=%d", pct);
}

/* ── Input handlers ── */

void bb_ui_settings_handle_rotate(int delta) {
  if (!s_st.active || delta == 0) return;

  /* Volume adjust mode: change value directly, update bar in-place. */
  if (s_st.level == LEVEL_VOLUME_ADJUST) {
    int v = s_st.volume_pct + delta * 5;
    if (v < 0) v = 0;
    if (v > 100) v = 100;
    if (v == s_st.volume_pct) return;
    s_st.volume_pct = v;
    s_st.volume_dirty = 1;
    bb_audio_set_volume_pct(v);
    update_volume_bar(v);
    return;
  }

  int row_count;
  switch (s_st.level) {
    case LEVEL_MAIN:
      row_count = main_visible_row_count();
      break;
    case LEVEL_DRIVER_PICKER:
      row_count = (s_st.driver_cache_count > 0) ? s_st.driver_cache_count : 1;
      break;
    case LEVEL_ADAPTER_PICKER:
      row_count = (s_st.site_cache_count > 0) ? s_st.site_cache_count : 1;
      break;
    case LEVEL_SESSION_PICKER:
      /* +1 for the synthetic "+ New" row at index 0. */
      row_count = s_st.session_cache_count + 1;
      break;
    case LEVEL_MODEL_PICKER: {
      const bb_agent_driver_info_t* d = active_driver_entry();
      row_count = (d != NULL && d->model_count > 0) ? d->model_count : 1;
      break;
    }
    default:
      return;
  }
  /* Wrap-around: past the bottom loops to the top and vice-versa. delta is
   * ±1 in practice; the double-mod keeps it correct for any magnitude/sign. */
  int next = ((s_st.sel + delta) % row_count + row_count) % row_count;
  if (next == s_st.sel) return;
  s_st.sel = next;
  highlight_selected();
}

int bb_ui_settings_handle_click(void) {
  if (!s_st.active) return 0;
  switch (s_st.level) {
    case LEVEL_MAIN: {
      main_row_t vis[MAIN_ROW_ID_COUNT];
      int vn = main_visible_rows(vis);
      main_row_t logical = (s_st.sel >= 0 && s_st.sel < vn) ? vis[s_st.sel] : MAIN_ROW_BACK;
      switch (logical) {
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
        case MAIN_ROW_ADAPTER:
          enter_adapter_picker();
          break;
        case MAIN_ROW_SESSIONS:
          enter_session_picker();
          break;
        case MAIN_ROW_VOLUME:
          /* Enter volume adjust sub-level */
          s_st.level = LEVEL_VOLUME_ADJUST;
          s_st.volume_dirty = 0;
          s_st.vol_fill = NULL;
          s_st.vol_pct_lbl = NULL;
          rerender();
          break;
        case MAIN_ROW_MIYU: {
          /* 密语(锁屏语音解锁) in-place toggle (ADR-037). Persist off the PSRAM
           * stack via the commit task (NVS write). Takes effect on NEXT boot —
           * miyu gates lock-on-boot, so we don't lock the user out of Settings now. */
          s_st.miyu_enabled = !s_st.miyu_enabled;
          spawn_persist_int(COMMIT_KIND_MIYU, s_st.miyu_enabled);
          rerender();
          break;
        }
        case MAIN_ROW_FIRMWARE:
          /* Click the Firmware row to check for an OTA update; if one is found
           * the confirm page (re-press OK to flash) takes over. OTA is cloud-only
           * (local_home has no OTA server) — show a note instead in that mode. */
          if (!bb_transport_is_cloud_saas()) {
            s_st.ota_status = OTA_ROW_CLOUD_ONLY;
            ESP_LOGI(TAG, "firmware row: OTA only in cloud_saas mode");
            rerender();
          } else {
            spawn_ota_check_task();
          }
          break;
        case MAIN_ROW_BACK:
          return 1; /* caller tears down + returns to chat */
        case MAIN_ROW_ID_COUNT:
        default:
          break;
      }
      return 0;
    }

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

    case LEVEL_ADAPTER_PICKER:
      if (s_st.site_cache_count <= 0) {
        return_to_main(MAIN_ROW_ADAPTER);
        return 0;
      }
      if (s_st.sel >= 0 && s_st.sel < s_st.site_cache_count) {
        const char* picked = s_st.site_cache[s_st.sel].home_site_id;
        int already = s_st.site_cache[s_st.sel].active ||
                      (s_st.active_site_id[0] != '\0' && strcmp(picked, s_st.active_site_id) == 0);
        if (picked[0] != '\0' && !already) {
          /* Optimistic local update — mark picked active so the main page +
           * picker reflect the choice before the WS round-trip confirms. */
          strncpy(s_st.active_site_id, picked, sizeof(s_st.active_site_id) - 1);
          s_st.active_site_id[sizeof(s_st.active_site_id) - 1] = '\0';
          for (int i = 0; i < s_st.site_cache_count; ++i) {
            s_st.site_cache[i].active = (i == s_st.sel) ? 1 : 0;
          }
          spawn_commit_adapter(picked);
          ESP_LOGI(TAG, "adapter picker -> '%s' (committed)", picked);
        }
      }
      return_to_main(MAIN_ROW_ADAPTER);
      return 0;

    case LEVEL_SESSION_PICKER: {
      /* Row 0 = synthetic "+ New"; rows 1..N = session_cache[sel-1].
       * Both bb_ui_agent_chat_* do LVGL work; handle_click already runs under
       * the LVGL context (other cases call rerender directly), so direct calls
       * are fine. We return 1 so bb_radio_app tears down Settings → shows chat
       * (the user just selected/created the conversation they want to be in). */
      if (s_st.sel == 0) {
        bb_ui_agent_chat_start_new_session();
        ESP_LOGI(TAG, "session picker -> new chat");
        return 1;
      }
      int idx = s_st.sel - 1;
      if (idx >= 0 && idx < s_st.session_cache_count) {
        bb_ui_agent_chat_switch_session(s_st.session_cache[idx].id,
                                        s_st.session_cache[idx].title);
        ESP_LOGI(TAG, "session picker -> '%s'", s_st.session_cache[idx].id);
        return 1;
      }
      /* Out-of-range (e.g. list emptied under us) — just go back to main. */
      return_to_main(MAIN_ROW_SESSIONS);
      return 0;
    }

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

    case LEVEL_VOLUME_ADJUST:
      /* OK in adjust mode: persist and return to main. Live volume is already
       * applied via bb_audio_set_volume_pct() on each rotate; here we only
       * persist — off the stream_task (PSRAM stack) to avoid the cache-freeze
       * panic on the NVS write. */
      if (s_st.volume_dirty) {
        spawn_persist_int(COMMIT_KIND_VOLUME, s_st.volume_pct);
        s_st.volume_dirty = 0;
      }
      s_st.vol_fill = NULL;
      s_st.vol_pct_lbl = NULL;
      return_to_main(MAIN_ROW_VOLUME);
      return 0;
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
    case LEVEL_ADAPTER_PICKER:
      return_to_main(MAIN_ROW_ADAPTER);
      return 0;
    case LEVEL_SESSION_PICKER:
      return_to_main(MAIN_ROW_SESSIONS);
      return 0;
    case LEVEL_MODEL_PICKER:
      return_to_main(MAIN_ROW_MODEL);
      return 0;
    case LEVEL_VOLUME_ADJUST:
      /* BACK: persist then return to main (same as OK). Persist off the
       * stream_task (PSRAM stack) — see handle_click for why. */
      if (s_st.volume_dirty) {
        spawn_persist_int(COMMIT_KIND_VOLUME, s_st.volume_pct);
        s_st.volume_dirty = 0;
      }
      s_st.vol_fill = NULL;
      s_st.vol_pct_lbl = NULL;
      return_to_main(MAIN_ROW_VOLUME);
      return 0;
  }
  return 0;
}
