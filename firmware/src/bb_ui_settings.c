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

#include <dirent.h>
#include <time.h>

#include "bb_recorder.h"
#include "bb_recplay.h"
#include "bb_sdcard.h"
#include "bb_time.h"

#include <stdint.h>
#include <stdio.h>
#include <string.h>

#include "bb_adapter_client.h"
#include "bb_agent_client.h"
#include "bb_audio.h"
#include "bb_config.h"
#include "bb_device_config.h"
#include "bb_nav_input.h" /* touch row-tap → inject OK (same path as the physical key) */
#include "bb_notification.h" /* ADR-021 §9: 已提醒 list + unread badge source */
#include "bb_ota.h"
#include "bb_radio_app.h"
#include "bb_session_store.h"
#include "bb_transport.h"
#include "bb_ui_agent_chat.h"
#include "bb_ui_layout.h"
#include "bb_ui_theme.h"
#include "esp_log.h"
#include "esp_lvgl_port.h"
#include "esp_system.h"  /* esp_get_free_heap_size — System Info page */
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

#if BB_UI_PORTRAIT
/* 竖屏手表 410×502，R60 物理圆角（顶角遮挡区 y<60，底角区 y>502-60=442）：
 *  - HEADER_H=64：list 首行（全宽高亮底）从 y=64 起 → 完全在顶角区之下；标题
 *    文字水平+垂直居中（pad_top=(64-20)/2，字面 line_height=20），居中构图避角。
 *  - ROW_H=64：纯触屏设备，行=触摸目标，≥56px（ADR-040 §UI）；行内文字
 *    pad_top 垂直居中。可见行数不写死——build_rows_box 用
 *    avail_h = DISP_H-HEADER_H-FOOTER_H 运行时推导（(502-64-64-8)/64 ≈ 5.7
 *    → 5 行整 + 半行滚动暗示）。
 *  - FOOTER_H=64：list 底缘 502-64=438 < 442 → 行不进底角区；footer 提示
 *    居中放在这条带里。 */
#define HEADER_H 64
#define ROW_H    64
#define FOOTER_H 64
/* 行内缩/选中条/滚动条按 64px 行高与安全区同步放大 */
#define ROW_PAD_LR        14
#define ROW_RADIUS        8
#define SEL_BAR_W         6
#define SCROLLBAR_W       5
#define SCROLLBAR_PAD_R   BB_UI_SAFE_LR /* 滚动条离右缘 ≥ SAFE_LR(12)，不再贴边 */
#define FOOTER_HINT_INSET 22 /* 提示行在 64px footer 带内垂直居中：(64-20)/2 */
#define BOUNCE_PEAK       16 /* 过滚回弹幅度随 502px 高度放大（方屏 8px） */
#else
#define HEADER_H 22
#define ROW_H    26
/* Reserve the bottom strip for the footer hint ("Hold to exit") — the
 * "reminder" the list must never overlap. The bbclaw panel is only 172px tall,
 * so the rows live in [HEADER_H, DISP_H - FOOTER_H] and scroll within it. */
#define FOOTER_H 22
#define ROW_PAD_LR        6
#define ROW_RADIUS        3
#define SEL_BAR_W         4
#define SCROLLBAR_W       3
#define SCROLLBAR_PAD_R   1
#define FOOTER_HINT_INSET 4
#define BOUNCE_PEAK       8
#endif
/* System Info ("About") page rows: Firmware / Device / Mode / Heap. */
#define SYSINFO_ROW_COUNT 4

/* ── Level / row enums ── */

typedef enum {
  LEVEL_MAIN = 0,
  LEVEL_DRIVER_PICKER,
  LEVEL_MODEL_PICKER,
  LEVEL_ADAPTER_PICKER,
  LEVEL_SESSION_PICKER,
  LEVEL_VOLUME_ADJUST,
  LEVEL_SYSINFO, /* read-only "About" page (firmware ver / device / mode / heap) */
  /* ADR-021-firmware-ui §9.2 提醒页 — 已提醒 list from the notification store.
   * Reached as a row in the Settings list (there is ONE menu = Settings; no
   * separate standby main-menu). */
  LEVEL_REMINDERS,
  LEVEL_RECFILES,   /* ADR-044:SD 录音浏览(会话→段→点按播放) */
} settings_level_t;

/* Logical main-page row ids. MAIN_ROW_ADAPTER (ADR-027) is only shown in
 * cloud_saas mode, so the on-screen row order/count is dynamic — see
 * main_visible_rows(). Cursor index (s_st.sel) indexes the *visible* list,
 * not these ids directly. */
typedef enum {
  MAIN_ROW_DRIVER = 0,
  MAIN_ROW_MODEL,
  MAIN_ROW_REMINDERS,    /* 提醒 — opens the 已提醒 page (LEVEL_REMINDERS) */
  MAIN_ROW_ADAPTER,
  MAIN_ROW_SESSIONS,
  MAIN_ROW_VOLUME,
  MAIN_ROW_MIYU,
  MAIN_ROW_RECORDER,     /* ADR-044 长录音入口(有 SD 卡槽的板);双击确认防误进 */
  MAIN_ROW_RECFILES,     /* ADR-044:SD 录音浏览+设备端回放 */
  MAIN_ROW_CHECK_UPDATE, /* cloud_saas only — runs an OTA check (→ confirm page) */
  MAIN_ROW_SYSINFO,      /* read-only "About" page */
  MAIN_ROW_BACK,
  MAIN_ROW_ID_COUNT,
} main_row_t;

/* Firmware row click state — view version, click to check/upgrade (OTA). */
typedef enum {
  OTA_ROW_IDLE = 0, /* idle — no check run yet this session */
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
  /* 1 = overlay was opened at LEVEL_MAINMENU (from STANDBY, ADR-021 §9): BACK
   * from LEVEL_MAIN / LEVEL_REMINDERS returns to the menu instead of exiting.
   * 0 = opened straight into Settings (from CHAT) — BACK exits as before. */
  int from_mainmenu;

  /* Driver catalog (populated async on entry). Shared by main + both pickers. */
  bb_agent_driver_info_t driver_cache[BB_SETTINGS_DRIVER_CACHE_MAX];
  int driver_cache_count;
  volatile int driver_fetch_pending;
  volatile uint32_t driver_fetch_generation;

  /* Firmware row OTA check (Settings → Firmware → click). */
  ota_row_status_t ota_status;
  int64_t recorder_arm_ms; /* ADR-044 录音行双击确认窗口起点(0=未武装) */
  int64_t miyu_arm_ms;     /* 密语开关双击确认窗口起点(0=未武装,用户要求状态修改需确认) */
  /* ADR-044 录音浏览器:0=会话列表 1=段列表;条目名缓存(会话=epoch 目录名,段=文件名) */
  int recfiles_mode;
  char recfiles_dir[24];              /* 当前会话目录名 */
  char recfiles_names[16][16];        /* 上限=行池容量 s_st.rows[16](超出越界=悬垂
                                       * 指针 set_text 崩,真机踩过);倒序留最新 16 条 */
  int recfiles_count;
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

/* Touch (ADR-040 §UI.5 v2): row tap = select + confirm. Defined after
 * highlight_selected (it re-highlights before confirming). */
static void row_clicked_cb(lv_event_t* e);

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
#if BB_UI_PORTRAIT
  /* 行两侧离屏缘 SAFE_LR(12)：list 在竖向中段（y∈[64,438]），圆角只吃四角，
   * 边中部横向内缩 SAFE_LR 已足（贴角才需 CORNER_INSET）。上下 pad 保持 4，
   * content_h 的 +8 数学不变。 */
  lv_obj_set_style_pad_left(s_st.rows_box, BB_UI_SAFE_LR, 0);
  lv_obj_set_style_pad_right(s_st.rows_box, BB_UI_SAFE_LR, 0);
#endif
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
  lv_obj_set_style_width(s_st.rows_box, SCROLLBAR_W, LV_PART_SCROLLBAR);
  lv_obj_set_style_pad_right(s_st.rows_box, SCROLLBAR_PAD_R, LV_PART_SCROLLBAR);
  lv_obj_set_style_radius(s_st.rows_box, 2, LV_PART_SCROLLBAR);
  lv_obj_set_style_bg_color(s_st.rows_box, lv_color_hex(BB_UI_ACCENT), LV_PART_SCROLLBAR);
  lv_obj_set_style_bg_opa(s_st.rows_box, LV_OPA_70, LV_PART_SCROLLBAR);
  for (int i = 0; i < row_count && i < (int)(sizeof(s_st.rows) / sizeof(s_st.rows[0])); ++i) {
    lv_obj_t* row = lv_label_create(s_st.rows_box);
    lv_obj_set_size(row, lv_pct(100), ROW_H);
    lv_obj_set_style_pad_left(row, ROW_PAD_LR, 0);
    lv_obj_set_style_pad_right(row, ROW_PAD_LR, 0);
#if BB_UI_PORTRAIT
    /* 行=64px 触摸目标；文字（line_height=20）在行内垂直居中：
     * pad_top=(64-20)/2=22 → 字面 y∈[22,42]，上下各余 22。运行时取
     * line_height，换字库不失中。 */
    lv_obj_set_style_pad_top(
        row, (ROW_H - (int)lv_font_get_line_height(ui_font())) / 2, 0);
#else
    lv_obj_set_style_pad_top(row, 4, 0);
#endif
    lv_obj_set_style_radius(row, ROW_RADIUS, 0);
    lv_obj_set_style_text_color(row, lv_color_hex(UI_ROW_FG), 0);
    lv_obj_set_style_text_font(row, ui_font(), 0);
    /* SCROLL (marquee) so long Chinese session titles in the picker scroll
     * instead of being truncated with "...". The row has a bounded width
     * (lv_pct(100) above), so LVGL only animates when the text overflows;
     * short static menu rows (Driver/Volume/...) stay put. */
    lv_label_set_long_mode(row, LV_LABEL_LONG_MODE_SCROLL);
    /* Touch: every row is a tap target. Labels drop LV_OBJ_FLAG_CLICKABLE in
     * their constructor, so re-add it; on the square (no-touch) panel there is
     * no pointer indev, so this never fires — zero behavior change there.
     * LVGL suppresses CLICKED when the press turned into a rows_box scroll, so
     * flick-scrolling the list doesn't accidentally activate a row. */
    lv_obj_add_flag(row, LV_OBJ_FLAG_CLICKABLE);
    lv_obj_add_event_cb(row, row_clicked_cb, LV_EVENT_CLICKED, (void*)(intptr_t)i);
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
      lv_obj_set_style_border_width(row, SEL_BAR_W, 0);
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

/* Row tap (LV_EVENT_CLICKED, pointer indev) — select the tapped row, then run
 * the SAME confirm path as the physical OK key ("滑到该行再按 OK" in one tap).
 * This callback runs on the LVGL task with the port lock already held, so it
 * must not re-lock or block: it only moves the local cursor, then injects
 * BB_NAV_EVENT_OK via bb_nav_input_inject — a non-blocking version-counter
 * bump consumed by radio_app's stream_task, which dispatches
 * settings_click_locked → bb_ui_settings_handle_click under its own lock and
 * also owns the exit-to-chat teardown when handle_click returns 1 (Sessions
 * pick / "+ New"). No business logic is duplicated here. */
static void row_clicked_cb(lv_event_t* e) {
  if (!s_st.active) return;
  int idx = (int)(intptr_t)lv_event_get_user_data(e);
  if (idx < 0 || idx >= s_st.rows_used) return;
  if (s_st.sel != idx) {
    s_st.sel = idx;
    highlight_selected();
  }
  bb_nav_input_inject(BB_NAV_EVENT_OK);
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
  /* 提醒 first — the primary "查看提醒" entry (ADR-021 §9.2). Always shown; reads
   * the on-device notification store, which is populated in both cloud_saas and
   * local_home. */
  out[n++] = MAIN_ROW_REMINDERS;
  /* Driver/Model are intentionally NOT shown: the agent backend (driver + model)
   * is configured on the adapter side now, not per-device. The picker/fetch code
   * is left in place (harmless, and the driver fetch still drives the post-switch
   * chat re-sync) but is simply unreachable from the menu. */
  if (bb_transport_is_cloud_saas()) {
    out[n++] = MAIN_ROW_ADAPTER;
    out[n++] = MAIN_ROW_SESSIONS;
  }
  out[n++] = MAIN_ROW_VOLUME;
#if BBCLAW_SDMMC_ENABLE
  /* 长录音(ADR-044):有卡槽的板常显;无卡时行文案提示 no SD card */
  out[n++] = MAIN_ROW_RECORDER;
  out[n++] = MAIN_ROW_RECFILES;
#endif
  /* 密语(锁屏语音解锁) only works in cloud_saas (passphrase_unlock_enabled), so
   * the toggle is only meaningful there — like the ADAPTER/SESSIONS rows (ADR-037). */
  if (bb_transport_is_cloud_saas()) {
    out[n++] = MAIN_ROW_MIYU;
    /* OTA is cloud-only (local_home has no OTA server), so the Check Update
     * action only appears in cloud_saas. The firmware version itself is shown
     * read-only in System Info below, which is available in any mode. */
    out[n++] = MAIN_ROW_CHECK_UPDATE;
  }
  /* System Info ("About") — read-only firmware version / device id / mode / heap.
   * Always shown; click pushes the read-only sub-page. */
  out[n++] = MAIN_ROW_SYSINFO;
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

/* Resolve a human-readable label for a session: its title (the adapter's
 * auto-generated summary of the first prompt), else the workspace name
 * (cwd_name), else a short id prefix. Never the bare full id. */
static const char* session_display_label(const bb_agent_session_info_t* s) {
  if (s == NULL) return "(none)";
  if (s->title[0] != '\0') return s->title;
  if (s->cwd_name[0] != '\0') return s->cwd_name;
  /* Untitled + no workspace name — last resort, a short id prefix. */
  static char idbuf[10];
  snprintf(idbuf, sizeof(idbuf), "%.8s", s->id);
  return idbuf;
}

/* Label of the currently-active session for the main-page Sessions row. Matches
 * the chat's active session id against the session_cache (fetched on entry) to
 * resolve a title/workspace name. Display-only — the picker switches sessions. */
static const char* current_session_label(void) {
  static char buf[80]; /* static scratch — single-threaded LVGL render */
  const char* cur = bb_ui_agent_chat_get_current_session();
  if (cur == NULL || cur[0] == '\0') return "(none)";
  for (int i = 0; i < s_st.session_cache_count; ++i) {
    if (strcmp(s_st.session_cache[i].id, cur) == 0) {
      snprintf(buf, sizeof(buf), "%s", session_display_label(&s_st.session_cache[i]));
      return buf;
    }
  }
  /* Not in cache yet (still loading, or beyond the first page) — show a short
   * id so the row is never empty; the title fills in once the fetch lands. */
  snprintf(buf, sizeof(buf), "%.8s", cur);
  return buf;
}

/* Render a boolean toggle state as a word instead of a bare "on"/"off". */
static const char* toggle_state_label(int on) {
  return on ? "Enabled" : "Disabled";
}

static void render_main(void) {
  if (s_st.root == NULL) return;
  lv_label_set_text(s_st.header_lbl, "Settings");

  main_row_t rows[MAIN_ROW_ID_COUNT];
  int n = main_visible_rows(rows);
  build_rows_box(n);
  /* Wide enough for "Sessions: " + a full session title (current_session_label
   * returns up to 79 bytes) without format-truncation. */
  char buf[96];

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
      case MAIN_ROW_REMINDERS: {
        int unread = bb_notification_unread_count();
        if (unread > 0) snprintf(buf, sizeof(buf), "Reminders · %d", unread);
        else snprintf(buf, sizeof(buf), "Reminders");
        break;
      }
      case MAIN_ROW_ADAPTER:
        snprintf(buf, sizeof(buf), "Adapter: %s", current_site_label());
        break;
      case MAIN_ROW_SESSIONS:
        /* Show the currently-active session (its CJK title when known) so the
         * row reflects state like Adapter does, not just a static label. Long
         * titles marquee-scroll (LV_LABEL_LONG_MODE_SCROLL on every row).
         * current_session_label() matches the chat's active id against the
         * session_cache fetched on entry; falls back to a short id / (none). */
        snprintf(buf, sizeof(buf), "Sessions: %s", current_session_label());
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
        if (s_st.miyu_arm_ms != 0) {
          snprintf(buf, sizeof(buf), "Miyu: %s · tap again to %s", toggle_state_label(s_st.miyu_enabled),
                   s_st.miyu_enabled ? "DISABLE" : "ENABLE");
        } else {
          snprintf(buf, sizeof(buf), "Miyu: %s", toggle_state_label(s_st.miyu_enabled));
        }
        break;
      case MAIN_ROW_RECORDER:
        if (s_st.recorder_arm_ms != 0) {
          snprintf(buf, sizeof(buf), "Recording · tap again to START");
        } else if (!bb_sdcard_mounted()) {
          snprintf(buf, sizeof(buf), "Recording · no SD card");
        } else {
          snprintf(buf, sizeof(buf), "Recording");
        }
        break;
      case MAIN_ROW_RECFILES:
        snprintf(buf, sizeof(buf), "Recordings");
        break;
      case MAIN_ROW_CHECK_UPDATE:
        /* Dedicated OTA-check action (the read-only version lives in System
         * Info now). Click runs bb_ota_check; the result is shown inline here,
         * and an available update opens the confirm page. */
        switch (s_st.ota_status) {
          case OTA_ROW_CHECKING:
            snprintf(buf, sizeof(buf), "Check Update · checking…");
            break;
          case OTA_ROW_LATEST:
            snprintf(buf, sizeof(buf), "Check Update · up to date");
            break;
          case OTA_ROW_ERROR:
            snprintf(buf, sizeof(buf), "Check Update · check failed");
            break;
          case OTA_ROW_CLOUD_ONLY:
            snprintf(buf, sizeof(buf), "Check Update · cloud only");
            break;
          case OTA_ROW_IDLE:
          default:
            snprintf(buf, sizeof(buf), "Check Update");
            break;
        }
        break;
      case MAIN_ROW_SYSINFO:
        snprintf(buf, sizeof(buf), "System Info");
        break;
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
#if BB_UI_PORTRAIT
  /* 触屏语义（手表）：点按行=选中并确认，右滑=BACK（屏幕级手势）。"·" 不在
   * montserrat(仅 ASCII) 里，绑 CJK 字库（其 --symbols 集含 "·"）。 */
  lv_obj_set_style_text_font(s_st.hint_lbl, ui_font(), 0);
  lv_label_set_text(s_st.hint_lbl, "Tap to select · Swipe right to exit");
#else
  lv_label_set_text(s_st.hint_lbl, "Hold to exit");
#endif
  /* 竖屏：提示在 64px footer 带内垂直居中（-22 → y∈[460,480]），水平居中
   * 天然避开底部两角（底角遮挡区 y>442 且 x<60 / x>350）。方屏保持 -4。 */
  lv_obj_align(s_st.hint_lbl, LV_ALIGN_BOTTOM_MID, 0, -FOOTER_HINT_INSET);
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
    /* Title (adapter's summary) → workspace name → short id; never the bare id. */
    int is_active = (cur != NULL && cur[0] != '\0' && strcmp(s->id, cur) == 0);
    snprintf(buf, sizeof(buf), "%s%s", session_display_label(s), is_active ? "  *" : "");
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

#if BB_UI_PORTRAIT
/* 竖屏：控件组垂直居中 + 放大（方屏原版绝对 Y 聚顶）。
 * 组高 = 条48 + 间距24 + 百分比20 + 间距12 + 提示20 = 124px；
 * 可用带 = [HEADER_H, DISP_H - SAFE_BOTTOM] = [64, 484]，高 420 →
 * VOL_BAR_Y = 64 + (420-124)/2 = 212，组占 y∈[212,336]，居中构图远离
 * 上（y<60）下（y>442）圆角区。条宽 410-2×36=338（x∈[36,374]，边中部
 * 只需 ≥SAFE_LR=12，富余）。 */
#define VOL_BAR_X      36
#define VOL_BAR_H      48
#define VOL_BAR_BORDER 3
#define VOL_FILL_R     6
#define VOL_FRAME_R    10
#define VOL_GAP_BAR_PCT  24 /* 条底 → 百分比标签 */
#define VOL_GAP_PCT_HINT 12 /* 百分比 → 提示行 */
#define VOL_TEXT_H       20 /* lv_font_bbclaw_cjk line_height */
#define VOL_GROUP_H \
  (VOL_BAR_H + VOL_GAP_BAR_PCT + VOL_TEXT_H + VOL_GAP_PCT_HINT + VOL_TEXT_H)
#define VOL_BAR_Y \
  (HEADER_H + (BB_DISP_H - HEADER_H - BB_UI_SAFE_BOTTOM - VOL_GROUP_H) / 2)
#define VOL_PCT_LABEL_Y (VOL_BAR_Y + VOL_BAR_H + VOL_GAP_BAR_PCT)
#define VOL_HINT_DY     (VOL_TEXT_H + VOL_GAP_PCT_HINT)
#else
#define VOL_BAR_X      8
#define VOL_BAR_Y      48
#define VOL_BAR_H      28
#define VOL_BAR_BORDER 2
#define VOL_FILL_R     2
#define VOL_FRAME_R    3
#define VOL_PCT_LABEL_Y  84
#define VOL_HINT_DY    20
#endif

/* Touch: press/drag on the volume bar maps x → percent. Defined after
 * spawn_persist_int (release persists through it, same as the old OK path). */
static void vol_bar_touch_cb(lv_event_t* e);

static void render_volume_adjust(void) {
  if (s_st.root == NULL) return;
  lv_label_set_text(s_st.header_lbl, "Volume");

  /* Reuse rows_box as a plain container for volume adjust widgets so that
   * destroy_rows() cleans them all up when leaving this level. */
  destroy_rows();
  s_st.rows_box = lv_obj_create(s_st.root);
  lv_obj_remove_style_all(s_st.rows_box);
#if BB_UI_PORTRAIT
  /* lv_pct() 是编码值，不能与整数加减——方屏那行 lv_pct(100)-HEADER_H 实际
   * 解码成 pct(78)，纯属巧合能看。竖屏直接用像素高度（方屏原状保留，勿动）。 */
  lv_obj_set_size(s_st.rows_box, lv_pct(100), BB_DISP_H - HEADER_H);
#else
  lv_obj_set_size(s_st.rows_box, lv_pct(100), lv_pct(100) - HEADER_H);
#endif
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
  /* Touch (ADR-040 §UI.5 v2): press/drag anywhere on the bar sets the volume
   * (x → percent, live visual + audio; release persists). The container is the
   * single hit target; a generous ext click area makes the strip easy to grab
   * on the watch. GESTURE_BUBBLE is cleared so a horizontal drag that LVGL
   * classifies as a gesture stays on the bar instead of bubbling to the
   * screen-level swipe-right=BACK handler mid-adjust. No pointer indev on the
   * square panel → callbacks never fire there. */
  lv_obj_clear_flag(container, LV_OBJ_FLAG_GESTURE_BUBBLE);
  lv_obj_set_ext_click_area(container, 16);
  lv_obj_add_event_cb(container, vol_bar_touch_cb, LV_EVENT_PRESSED, NULL);
  lv_obj_add_event_cb(container, vol_bar_touch_cb, LV_EVENT_PRESSING, NULL);
  lv_obj_add_event_cb(container, vol_bar_touch_cb, LV_EVENT_RELEASED, NULL);
  lv_obj_add_event_cb(container, vol_bar_touch_cb, LV_EVENT_PRESS_LOST, NULL);

  /* Fill rect (accent color) */
  lv_obj_t* fill = lv_obj_create(container);
  lv_obj_remove_style_all(fill);
  lv_obj_set_size(fill, fill_w, VOL_BAR_H - VOL_BAR_BORDER * 2);
  lv_obj_set_pos(fill, VOL_BAR_BORDER, VOL_BAR_BORDER);
  lv_obj_set_style_radius(fill, VOL_FILL_R, 0);
  lv_obj_set_style_bg_color(fill, lv_color_hex(BB_UI_ACCENT), 0);
  lv_obj_set_style_bg_opa(fill, LV_OPA_COVER, 0);
  /* Not a hit target — presses must land on the container underneath. */
  lv_obj_clear_flag(fill, LV_OBJ_FLAG_CLICKABLE);
  s_st.vol_fill = fill;

  /* Border frame on top */
  lv_obj_t* frame = lv_obj_create(container);
  lv_obj_remove_style_all(frame);
  lv_obj_set_size(frame, bar_w, VOL_BAR_H);
  lv_obj_set_pos(frame, 0, 0);
  lv_obj_set_style_radius(frame, VOL_FRAME_R, 0);
  lv_obj_set_style_border_width(frame, VOL_BAR_BORDER, 0);
  lv_obj_set_style_border_color(frame, lv_color_hex(BB_UI_ACCENT), 0);
  lv_obj_set_style_border_opa(frame, LV_OPA_COVER, 0);
  lv_obj_set_style_bg_opa(frame, LV_OPA_0, 0);
  lv_obj_clear_flag(frame, LV_OBJ_FLAG_SCROLLABLE);
  /* Not a hit target — presses must land on the container underneath. */
  lv_obj_clear_flag(frame, LV_OBJ_FLAG_CLICKABLE);

  /* Percentage label */
  lv_obj_t* pct_lbl = lv_label_create(s_st.rows_box);
  lv_obj_set_style_text_color(pct_lbl, lv_color_hex(BB_UI_DOT_LIT), 0);
  lv_obj_set_style_text_font(pct_lbl, ui_font(), 0);
  char buf[16];
  snprintf(buf, sizeof(buf), "%d%%", pct);
  lv_label_set_text(pct_lbl, buf);
#if BB_UI_PORTRAIT
  /* 百分比/提示与条同宽并居中——竖屏中置构图（方屏保持左对齐原状）。 */
  lv_obj_set_width(pct_lbl, bar_w);
  lv_obj_set_style_text_align(pct_lbl, LV_TEXT_ALIGN_CENTER, 0);
#endif
  lv_obj_set_pos(pct_lbl, VOL_BAR_X, VOL_PCT_LABEL_Y - HEADER_H);
  s_st.vol_pct_lbl = pct_lbl;

  /* Hint label */
  lv_obj_t* hint = lv_label_create(s_st.rows_box);
  lv_obj_set_style_text_color(hint, lv_color_hex(BB_UI_TEXT_DIM), 0);
  lv_obj_set_style_text_font(hint, ui_font(), 0);
#if BB_UI_PORTRAIT
  /* 触屏语义（手表）：拖动条=调节（松手即存），右滑=BACK（返回也会存）。 */
  lv_label_set_text(hint, "Drag to adjust · Swipe right to go back");
  lv_obj_set_width(hint, bar_w);
  lv_obj_set_style_text_align(hint, LV_TEXT_ALIGN_CENTER, 0);
#else
  lv_label_set_text(hint, "Up/Down adjust  OK to save");
#endif
  lv_obj_set_pos(hint, VOL_BAR_X, VOL_PCT_LABEL_Y - HEADER_H + VOL_HINT_DY);
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

/* ── Render: System Info ("About") — read-only ── */

static void render_sysinfo(void) {
  if (s_st.root == NULL) return;
  lv_label_set_text(s_st.header_lbl, "System Info");

  build_rows_box(SYSINFO_ROW_COUNT);
  char buf[80];

  snprintf(buf, sizeof(buf), "Firmware: %s", bb_ota_get_current_version());
  lv_label_set_text(s_st.rows[0], buf);

  snprintf(buf, sizeof(buf), "Device: %s", bbclaw_device_id());
  lv_label_set_text(s_st.rows[1], buf);

  snprintf(buf, sizeof(buf), "Mode: %s",
           bb_transport_is_cloud_saas() ? "cloud" : "local");
  lv_label_set_text(s_st.rows[2], buf);

  snprintf(buf, sizeof(buf), "Heap: %u KB free",
           (unsigned)(esp_get_free_heap_size() / 1024));
  lv_label_set_text(s_st.rows[3], buf);

  highlight_selected();
}

/* ── Render: 提醒页 (LEVEL_REMINDERS, ADR-021 §9.2) ──
 * F1 shows only 已提醒 — the notifications the device already received over the
 * control WS (bb_notification store, cloud-free). 即将 (scheduled pull) lands in
 * F3 with the agent.reminders.list request. */

static void render_reminders(void) {
  if (s_st.root == NULL) return;
  int unread = bb_notification_unread_count();
  char hdr[32];
  if (unread > 0) {
    snprintf(hdr, sizeof(hdr), "Reminders · %d", unread);
  } else {
    snprintf(hdr, sizeof(hdr), "Reminders");
  }
  lv_label_set_text(s_st.header_lbl, hdr);

  static bb_notification_t items[BB_NOTIFY_MAX];
  int n = bb_notification_list(items, BB_NOTIFY_MAX);
  if (n <= 0) {
    build_rows_box(1);
    lv_label_set_text(s_st.rows[0], "No reminders yet");
    highlight_selected();
    return;
  }
  build_rows_box(n);
  for (int i = 0; i < n; ++i) {
    char buf[80];
    /* Unread rows get a leading dot marker (read rows pad two spaces to align). */
    snprintf(buf, sizeof(buf), "%s%s", items[i].read ? "  " : "• ",
             items[i].preview[0] != '\0' ? items[i].preview : "(notification)");
    lv_label_set_text(s_st.rows[i], buf);
  }
  highlight_selected();
}

static void render_recfiles(void); /* fwd: 定义在 rerender 之后 */

static void rerender(void) {
  switch (s_st.level) {
    case LEVEL_MAIN:           render_main(); break;
    case LEVEL_DRIVER_PICKER:  render_driver_picker(); break;
    case LEVEL_MODEL_PICKER:   render_model_picker(); break;
    case LEVEL_ADAPTER_PICKER: render_adapter_picker(); break;
    case LEVEL_SESSION_PICKER: render_session_picker(); break;
    case LEVEL_VOLUME_ADJUST:  render_volume_adjust(); break;
    case LEVEL_SYSINFO:        render_sysinfo(); break;
    case LEVEL_REMINDERS:      render_reminders(); break;
    case LEVEL_RECFILES:       render_recfiles(); break;
  }
}

static void enter_reminders(void) {
  s_st.level = LEVEL_REMINDERS;
  s_st.sel = 0;
  /* Opening the 已提醒 list = the user has seen them → clear unread (badge)。 */
  bb_notification_mark_all_read();
  rerender();
}

/* ── Render: 录音浏览页 (LEVEL_RECFILES, ADR-044) ──
 * 会话列表(目录名=epoch,格式化为日期)→段列表(.opus,点按播放/再点停止)。
 * FS 扫描只在进入/切层时做一次并缓存;录音进行中不进段播放(FATFS 单用户)。 */

static int recfiles_name_cmp_desc(const void* a, const void* b) {
  return strcmp((const char*)b, (const char*)a); /* 倒序:最新在前 */
}

static void recfiles_scan(void) {
  s_st.recfiles_count = 0;
  char path[64];
  if (s_st.recfiles_mode == 0) {
    snprintf(path, sizeof(path), "/sdcard/ambient");
  } else {
    snprintf(path, sizeof(path), "/sdcard/ambient/%s", s_st.recfiles_dir);
  }
  DIR* d = opendir(path);
  if (d == NULL) return;
  struct dirent* e;
  while ((e = readdir(d)) != NULL && s_st.recfiles_count < 16) {
    if (e->d_name[0] == '.') continue;
    if (s_st.recfiles_mode == 0) {
      if (e->d_type != DT_DIR) continue;
    } else {
      if (strstr(e->d_name, ".opus") == NULL) continue;
    }
    /* d_name 最长 255,显式截断到槽宽(目录名=epoch 10 位/段名 11 位,实际不会截) */
    strncpy(s_st.recfiles_names[s_st.recfiles_count], e->d_name, sizeof(s_st.recfiles_names[0]) - 1);
    s_st.recfiles_names[s_st.recfiles_count][sizeof(s_st.recfiles_names[0]) - 1] = '\0';
    s_st.recfiles_count++;
  }
  closedir(d);
  qsort(s_st.recfiles_names, (size_t)s_st.recfiles_count, sizeof(s_st.recfiles_names[0]),
        recfiles_name_cmp_desc);
}

static void render_recfiles(void) {
  if (s_st.root == NULL) return;
  lv_label_set_text(s_st.header_lbl, s_st.recfiles_mode == 0 ? "Recordings" : "Segments");
  if (s_st.recfiles_count <= 0) {
    build_rows_box(1);
    lv_label_set_text(s_st.rows[0], s_st.recfiles_mode == 0 ? "No recordings yet" : "(empty)");
    highlight_selected();
    return;
  }
  build_rows_box(s_st.recfiles_count);
  for (int i = 0; i < s_st.recfiles_count; ++i) {
    char buf[64];
    if (s_st.recfiles_mode == 0) {
      /* 目录名=epoch 秒 → "07-05 23:14";b 前缀(无墙钟)原样显示 */
      long long ep = atoll(s_st.recfiles_names[i]);
      if (ep > 1600000000LL) {
        time_t tt = (time_t)ep;
        struct tm tmv;
        localtime_r(&tt, &tmv);
        snprintf(buf, sizeof(buf), "%02d-%02d %02d:%02d", tmv.tm_mon + 1, tmv.tm_mday, tmv.tm_hour,
                 tmv.tm_min);
      } else {
        snprintf(buf, sizeof(buf), "%s", s_st.recfiles_names[i]);
      }
    } else {
      char full[96];
      snprintf(full, sizeof(full), "/sdcard/ambient/%s/%s", s_st.recfiles_dir, s_st.recfiles_names[i]);
      int playing = (strcmp(bb_recplay_current(), full) == 0);
      snprintf(buf, sizeof(buf), "%s%s", playing ? "> " : "  ", s_st.recfiles_names[i]);
    }
    lv_label_set_text(s_st.rows[i], buf);
  }
  highlight_selected();
}

static void enter_recfiles(void) {
  s_st.level = LEVEL_RECFILES;
  s_st.recfiles_mode = 0;
  s_st.sel = 0;
  recfiles_scan();
  rerender();
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
  /* 菜单「Check Update」是用户主动触发（manual_check=true）。是否升级由云端决定，
   * 固件侧不判断 dev/dirty（dev 护栏已取消，dirty 构建也照常查云端）。
   *
   * ⚠️ 本任务栈在 PSRAM（spawn_ota_check_task 用 BBCLAW_MALLOC_CAP_PREFER_PSRAM）。
   * 因此 manual_check=true 这条路径绝不能做任何 NVS / SPI flash 操作——flash 读写会
   * 冻结 cache，期间 PSRAM 栈不可访问 → cache-disabled assert → 设备重启。
   * bb_ota_check_ex 已把 #179 NVS 死循环护栏限定在 manual_check=false（开机内部栈）
   * 路径。日后别往这条 check 路径加 NVS。 */
  r->err = bb_ota_check_ex(&r->info, /*manual_check=*/true);
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
  /* Re-render the picker (live list) or the main page (the "Sessions: <title>"
   * row resolves its title from this cache). */
  if (s_st.level == LEVEL_SESSION_PICKER || s_st.level == LEVEL_MAIN) {
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

/* ── Volume bar touch (press/drag → percent) ──
 * Runs on the LVGL task (pointer indev event, port lock held) — no re-lock, no
 * blocking work. PRESSED/PRESSING map the touch x onto the bar's inner width
 * and mirror the encoder path exactly: bb_audio_set_volume_pct() applies the
 * value live (cheap: sets an int) and update_volume_bar() partial-updates the
 * fill + "NN%" label in real time, no rebuild. RELEASED/PRESS_LOST persist via
 * spawn_persist_int(COMMIT_KIND_VOLUME) — the same NVS-off-LVGL-task save the
 * old OK path (handle_click LEVEL_VOLUME_ADJUST) performs, so 松手保存 == 旧
 * OK 的保存语义 (the page stays up for further drags; BACK still saves too). */
static void vol_bar_touch_cb(lv_event_t* e) {
  if (!s_st.active || s_st.level != LEVEL_VOLUME_ADJUST) return;
  lv_event_code_t code = lv_event_get_code(e);
  if (code == LV_EVENT_PRESSED || code == LV_EVENT_PRESSING) {
    lv_indev_t* indev = lv_event_get_indev(e);
    if (indev == NULL) return;
    lv_point_t p;
    lv_indev_get_point(indev, &p);
    lv_obj_t* bar = (lv_obj_t*)lv_event_get_current_target(e);
    lv_area_t coords;
    lv_obj_get_coords(bar, &coords);
    int inner_w = (int)lv_area_get_width(&coords) - VOL_BAR_BORDER * 2;
    if (inner_w <= 0) return;
    int v = (((int)p.x - (int)coords.x1 - VOL_BAR_BORDER) * 100) / inner_w;
    if (v < 0) v = 0;
    if (v > 100) v = 100;
    if (v == s_st.volume_pct) return;
    s_st.volume_pct = v;
    s_st.volume_dirty = 1;
    bb_audio_set_volume_pct(v); /* live apply — same as handle_rotate */
    update_volume_bar(v);       /* real-time fill + "NN%" while dragging */
  } else if (code == LV_EVENT_RELEASED || code == LV_EVENT_PRESS_LOST) {
    if (s_st.volume_dirty) {
      spawn_persist_int(COMMIT_KIND_VOLUME, s_st.volume_pct);
      s_st.volume_dirty = 0;
    }
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

/* Open the read-only System Info ("About") page. */
static void enter_sysinfo(void) {
  s_st.level = LEVEL_SYSINFO;
  s_st.sel = 0;
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

  /* Initialise level + cursor before any LVGL work. sel is an index into the
   * VISIBLE row list, so the first row is index 0 (提醒, then the settings rows). */
  s_st.level = LEVEL_MAIN;
  s_st.sel = 0;
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
#if BB_UI_PORTRAIT
  /* 标题水平+垂直居中于 64px header 带（方屏是贴左上 (0,0)+6px，那会撞
   * R60 顶角）。文字 y∈[22,42]；顶角遮挡区在 y=22 处仅 x<14 / x>396
   * （(x-60)²+(22-60)²>60² → x<13.6），居中文本远离两角。 */
  lv_obj_set_style_text_align(s_st.header_lbl, LV_TEXT_ALIGN_CENTER, 0);
  lv_obj_set_style_pad_top(
      s_st.header_lbl,
      (HEADER_H - (int)lv_font_get_line_height(ui_font())) / 2, 0);
#else
  lv_obj_set_style_pad_left(s_st.header_lbl, 6, 0);
  lv_obj_set_style_pad_top(s_st.header_lbl, 4, 0);
#endif

  s_st.active = 1;

  /* Kick off async driver/model fetch. apply renders a stale (or empty)
   * snapshot in the meantime — on_driver_fetch_done re-renders when done. */
  s_st.pending_chat_sync = 0;
  spawn_driver_fetch_task();
  /* ADR-027: in cloud_saas, also pull the Home Adapter list so the main-page
   * "Adapter: <label>" row shows the active machine on first paint. */
  spawn_site_fetch_task();
  /* Pull the session list too so the main-page "Sessions: <title>" row can
   * resolve the active session's human title (not just a short id) on first
   * paint, mirroring the Adapter row. cloud_saas-only (no-op otherwise). */
  spawn_session_fetch_task();
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

/* ── Overscroll bounce (rubber-band edge feedback) ──
 * The list no longer wraps. When a press can't move the cursor (already on the
 * first/last row), we play a short rubber-band nudge on the rows box instead, so
 * the user feels they've hit the end — the phone-style overscroll the encoder
 * can't otherwise convey. translate_y is render-only (doesn't disturb layout or
 * the scrollbar thumb), and the anim's playback phase springs it back to 0, so a
 * freshly-rebuilt rows_box (re-created on every level change) always starts at 0. */
static void rows_box_translate_cb(void* obj, int32_t v) {
  lv_obj_set_style_translate_y((lv_obj_t*)obj, v, 0);
}

static void overscroll_bounce(int dir) {
  if (s_st.rows_box == NULL) return;
  /* dir > 0 = tried to go past the bottom → nudge content UP and spring back;
   * dir < 0 = past the top → nudge DOWN. ~8px reads clearly on the 172px panel
   * (竖屏 502px 高按比例放大到 16px，BOUNCE_PEAK). */
  int32_t peak = (dir > 0) ? -BOUNCE_PEAK : BOUNCE_PEAK;
  lv_anim_del(s_st.rows_box, rows_box_translate_cb); /* restart cleanly on a fast double-press */
  lv_anim_t a;
  lv_anim_init(&a);
  lv_anim_set_var(&a, s_st.rows_box);
  lv_anim_set_exec_cb(&a, rows_box_translate_cb);
  lv_anim_set_values(&a, 0, peak);
  lv_anim_set_duration(&a, 90);           /* push out to the edge */
  lv_anim_set_playback_duration(&a, 150); /* then spring back, a touch slower */
  lv_anim_set_path_cb(&a, lv_anim_path_ease_out);
  lv_anim_start(&a);
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
    case LEVEL_SYSINFO:
      row_count = SYSINFO_ROW_COUNT; /* read-only, but cursor drives scroll */
      break;
    case LEVEL_REMINDERS: {
      static bb_notification_t tmp[BB_NOTIFY_MAX];
      int cnt = bb_notification_list(tmp, BB_NOTIFY_MAX);
      row_count = cnt > 0 ? cnt : 1; /* the "还没有提醒" placeholder is 1 row */
      break;
    }
    case LEVEL_RECFILES:
      row_count = s_st.recfiles_count > 0 ? s_st.recfiles_count : 1;
      break;
    default:
      return;
  }
  /* No wrap-around: the cursor clamps at the ends. When a press can't move it
   * (already on the first/last row) we play a rubber-band bounce so the user
   * feels the edge instead of the cursor silently looping to the other end.
   * delta is ±1 in practice. */
  int next = s_st.sel + delta;
  if (next < 0) next = 0;
  if (next > row_count - 1) next = row_count - 1;
  if (next == s_st.sel) {
    overscroll_bounce(delta > 0 ? 1 : -1);
    return;
  }
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
        case MAIN_ROW_REMINDERS:
          enter_reminders();
          break;
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
           * miyu gates lock-on-boot, so we don't lock the user out of Settings now.
           * 双击确认(用户要求:状态修改类操作需确认,与 Recording 行同一习惯)。 */
          int64_t miyu_now = bb_now_ms();
          if (s_st.miyu_arm_ms == 0 || miyu_now - s_st.miyu_arm_ms >= 5000) {
            s_st.miyu_arm_ms = miyu_now;
            rerender();
            break;
          }
          s_st.miyu_arm_ms = 0;
          s_st.miyu_enabled = !s_st.miyu_enabled;
          spawn_persist_int(COMMIT_KIND_MIYU, s_st.miyu_enabled);
          rerender();
          break;
        }
        case MAIN_ROW_RECFILES:
          enter_recfiles();
          break;
        case MAIN_ROW_RECORDER: {
          /* ADR-044:录音是隐私敏感操作,双击确认(5s 窗口)。确认后返回 2,
           * radio_app 负责退出设置并进 RECORDER 态(含无卡二次挂载重试)。 */
          int64_t now = bb_now_ms();
          if (s_st.recorder_arm_ms != 0 && now - s_st.recorder_arm_ms < 5000) {
            s_st.recorder_arm_ms = 0;
            return 2;
          }
          s_st.recorder_arm_ms = now;
          rerender();
          break;
        }
        case MAIN_ROW_CHECK_UPDATE:
          /* Run an OTA check; if an update is found the confirm page (re-press
           * OK to flash) takes over. The row is cloud_saas-only (see
           * main_visible_rows), but guard defensively. */
          if (!bb_transport_is_cloud_saas()) {
            s_st.ota_status = OTA_ROW_CLOUD_ONLY;
            ESP_LOGI(TAG, "check update: OTA only in cloud_saas mode");
            rerender();
          } else {
            spawn_ota_check_task();
          }
          break;
        case MAIN_ROW_SYSINFO:
          enter_sysinfo();
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

    case LEVEL_SYSINFO:
      /* Read-only page — nothing to commit, so OK just dismisses back to main
       * (same as BACK), the natural "tap to return" for an info screen. */
      return_to_main(MAIN_ROW_SYSINFO);
      return 0;

    case LEVEL_REMINDERS:
      /* Read-only list. OK returns to the Settings list (like BACK). */
      return_to_main(MAIN_ROW_REMINDERS);
      return 0;

    case LEVEL_RECFILES: {
      if (s_st.recfiles_count <= 0) {
        return_to_main(MAIN_ROW_RECFILES);
        return 0;
      }
      if (s_st.sel < 0 || s_st.sel >= s_st.recfiles_count) return 0;
      if (s_st.recfiles_mode == 0) {
        /* 会话 → 段列表 */
        snprintf(s_st.recfiles_dir, sizeof(s_st.recfiles_dir), "%s", s_st.recfiles_names[s_st.sel]);
        s_st.recfiles_mode = 1;
        s_st.sel = 0;
        recfiles_scan();
        rerender();
        return 0;
      }
      /* 段 → 播放/停止(录音中或 TTS 占用时拒绝并提示) */
      char full[96];
      snprintf(full, sizeof(full), "/sdcard/ambient/%s/%s", s_st.recfiles_dir,
               s_st.recfiles_names[s_st.sel]);
      esp_err_t perr = bb_recplay_toggle(full);
      if (perr == ESP_ERR_INVALID_STATE) {
        lv_label_set_text(s_st.header_lbl, "Busy (recording/TTS)");
      }
      rerender();
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
    case LEVEL_REMINDERS:
      return_to_main(MAIN_ROW_REMINDERS);
      return 0;
    case LEVEL_RECFILES:
      bb_recplay_stop(); /* 离开浏览层即停播 */
      if (s_st.recfiles_mode == 1) {
        s_st.recfiles_mode = 0;
        s_st.sel = 0;
        recfiles_scan();
        rerender();
        return 0;
      }
      return_to_main(MAIN_ROW_RECFILES);
      return 0;
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
    case LEVEL_SYSINFO:
      return_to_main(MAIN_ROW_SYSINFO);
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

/* 外部状态变化(如 SD 热插卡)时刷新当前页;overlay 未显示则 no-op。
 * 需在 LVGL 锁内调用。 */
void bb_ui_settings_refresh_if_visible(void) {
  if (s_st.root == NULL) return;
  rerender();
}
