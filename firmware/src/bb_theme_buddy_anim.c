#include <stdio.h>
#include <string.h>

#include "bb_agent_theme.h"
#include "bb_camera.h" /* ADR-049: 触屏底部「拍照」钮 → bb_camera_shoot_and_send_async */
#include "bb_chat_recording.h"
#include "bb_chat_transcript.h"
#include "bb_display.h"
#include "bb_ptt.h" /* ADR-049: 触屏「按住说话」钮 → bb_ptt_inject */
#include "bb_ui_layout.h"
#include "bb_lvgl_assets.h"
#include "bb_lvgl_element_assets.h"
#include "bb_power.h"
#include "bb_ui_settings.h"
#include "bb_ui_theme.h"
#include "bb_wifi.h"
#include "esp_log.h"
#include "lvgl.h"

static const char* TAG = "bb_theme_anim";

/*
 * buddy-anim — the single shipping agent-chat theme.
 *
 * 布局：overlay 透明，顶/底栏由底层 ACTIVE 视图提供；本主题只拥有中间
 * transcript（聊天消息流）+ 录音遮罩。早期版本在 transcript 右上角浮着一个
 * 字符小人（face+mood 半透明小窗，九态动效），2026-06-10 应用户要求移除——
 * 角色状态已由顶部状态栏图标 + 底栏点阵扫描条表达，右上角小人冗余且遮挡正文。
 * set_state 仍保留（驱动顶栏图标语义），只是不再渲染小人。
 *
 * 历史回放：transcript 是 LVGL flex column；append_history_message 在末尾追加
 * 完成的 user/assistant label，prepend_history_message 把更老的批次插到顶部
 * （lv_obj_move_to_index(0)）实现"上翻自动加载"。消息渲染委托 bb_chat_transcript.c。
 */

/* design/UI_DESIGN_LANGUAGE.md tokens */
#define UI_SCR_BG      BB_UI_BG
#define UI_TEXT_MAIN   BB_UI_DOT_LIT
#define UI_TEXT_DIM    BB_UI_TEXT_DIM
#define UI_STATUS_FG   BB_UI_TEXT_DIM
#define UI_ME_ACCENT   BB_UI_ACCENT
#define UI_TOOL_FG     BB_UI_TEXT_DIM
#define UI_ERROR_FG    BB_UI_ERR

/* Screen corner inset — prevents content from being clipped by the physical
 * display's rounded corners (~R12-R16 on the 1.47" ST7789 panel). */
#define SCREEN_CORNER_INSET_X  8
#define SCREEN_CORNER_INSET_Y  4

typedef struct {
  lv_obj_t* root;
  /* topbar — owned by the underlying ACTIVE view now; kept NULL here so the
   * (NULL-guarded) refresh_topbar stays a no-op without extra branching. */
  lv_obj_t* topbar;
  lv_obj_t* topbar_icon;
  lv_obj_t* topbar_driver_lbl;
  lv_obj_t* topbar_session_lbl;
  lv_obj_t* topbar_bat_container;
  lv_obj_t* topbar_bat_fill;
  lv_obj_t* topbar_bat_frame;
  lv_obj_t* topbar_bat_lbl;
  /* main */
  lv_obj_t* transcript;
  lv_obj_t* active_assistant;
  char driver_buf[24];
  char session_buf[16];
  bb_agent_state_t state;
  int built;
} bb_anim_state_t;

static bb_anim_state_t s_st = {0};

/* ── topbar refresh (no-op while the theme's own topbar is NULL) ── */

static const lv_image_dsc_t* state_icon(bb_agent_state_t s) {
  switch (s) {
    case BB_AGENT_STATE_BUSY:
    case BB_AGENT_STATE_LISTENING:
    case BB_AGENT_STATE_SPEAKING:
      return &bb_img_task;
    case BB_AGENT_STATE_DIZZY:
      /* DIZZY 是每轮答完都会经过的「歇口气」过渡(也兼作错误态),正常对话每轮都进它。
       * 之前用红色 err 图标 → 每次正常回答完页头都闪一下「报错」样式(交互其实没问题)。
       * 改用中性 ready 图标,不再误报。真错误另有文字/语音播报。 */
      return &bb_img_ready;
    default:
      return &bb_img_ready;
  }
}

static void refresh_topbar(void) {
  if (s_st.topbar_icon != NULL) {
    lv_image_set_src(s_st.topbar_icon, state_icon(s_st.state));
  }
  if (s_st.topbar_driver_lbl != NULL) {
    lv_label_set_text(s_st.topbar_driver_lbl,
                      s_st.driver_buf[0] != '\0' ? s_st.driver_buf : "---");
  }
  if (s_st.topbar_session_lbl != NULL) {
    lv_label_set_text(s_st.topbar_session_lbl,
                      s_st.session_buf[0] != '\0' ? s_st.session_buf : "--------");
  }
  if (s_st.topbar_bat_container != NULL) {
    bb_power_state_t pwr = {0};
    bb_power_get_state(&pwr);
    if (!pwr.supported || !pwr.available) {
      lv_obj_add_flag(s_st.topbar_bat_container, LV_OBJ_FLAG_HIDDEN);
    } else {
      lv_obj_clear_flag(s_st.topbar_bat_container, LV_OBJ_FLAG_HIDDEN);
      int pct = pwr.percent < 0 ? 0 : (pwr.percent > 100 ? 100 : pwr.percent);
      lv_obj_set_width(s_st.topbar_bat_fill, (pct * 18) / 100);
      lv_obj_set_style_bg_color(s_st.topbar_bat_fill,
                                lv_color_hex(pwr.low ? UI_ERROR_FG : UI_ME_ACCENT), 0);
      char buf[8];
      snprintf(buf, sizeof(buf), "%d", pct);
      lv_label_set_text(s_st.topbar_bat_lbl, buf);
    }
  }
}

static void theme_scroll_transcript(int lines) {
  bb_chat_transcript_scroll(lines);
}

/* Message rendering is delegated to bb_chat_transcript.c. */

#if BBCLAW_CAMERA_ENABLE
/* ── ADR-049 触屏底部功能条：「拍照」+「按住说话」──
 * 实战派(lichuang-szp)是触屏板、且板载摄像头。对话页底部挂两枚大钮:
 *   拍照     — 单击拍一帧 JPEG 喂 AI 看图(异步,不阻塞 LVGL 线程)。
 *   按住说话 — 屏上 PTT:按下开录、松手/滑走发送,与实体 BOOT-PTT 同一条链路。
 * 钮是主题根 s_st.root 的子节点,随对话页进/出创建/销毁;仅有摄像头的板编进来。 */
#ifdef BBCLAW_HAVE_CJK_FONT
extern const lv_font_t lv_font_bbclaw_cjk;
#endif

static const lv_font_t* action_font(void) {
#ifdef BBCLAW_HAVE_CJK_FONT
  return &lv_font_bbclaw_cjk;
#else
  return lv_font_get_default();
#endif
}

static void chat_cam_btn_cb(lv_event_t* e) {
  (void)e;
  ESP_LOGI(TAG, "chat: 拍照 钮 tapped → shoot async"); /* 排障:确认触摸是否送达按钮 */
  bb_camera_shoot_and_send_async(NULL); /* 默认文案:请看图并简洁描述 */
}

static void chat_ptt_btn_cb(lv_event_t* e) {
  const lv_event_code_t code = lv_event_get_code(e);
  if (code == LV_EVENT_PRESSED) {
    bb_ptt_inject(1);
  } else if (code == LV_EVENT_RELEASED || code == LV_EVENT_PRESS_LOST) {
    bb_ptt_inject(0); /* 松手或手指滑出钮都算松开→结束这轮并发送 */
  }
}

static lv_obj_t* chat_action_btn(lv_obj_t* parent, const char* text) {
  lv_obj_t* b = lv_obj_create(parent);
  lv_obj_remove_style_all(b);
  lv_obj_set_style_radius(b, 8, 0);
  lv_obj_set_style_bg_color(b, lv_color_hex(BB_UI_ACCENT), 0);
  lv_obj_set_style_bg_opa(b, LV_OPA_COVER, 0);
  lv_obj_set_style_bg_opa(b, LV_OPA_50, LV_STATE_PRESSED); /* 按下变暗给反馈 */
  lv_obj_clear_flag(b, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_add_flag(b, LV_OBJ_FLAG_CLICKABLE);
  lv_obj_t* lbl = lv_label_create(b);
  lv_obj_set_style_text_font(lbl, action_font(), 0);
  lv_obj_set_style_text_color(lbl, lv_color_hex(0x0a0a0a), 0); /* teal 底 + 深字 */
  lv_label_set_text(lbl, text);
  lv_obj_center(lbl);
  return b;
}

static void chat_build_action_bar(lv_obj_t* root) {
  const int bar_h = 44, margin = 6, gap = 8;
  const int btn_w = (BB_DISP_W - 2 * margin - gap) / 2; /* 两枚等宽 */
  const int y = BB_DISP_H - bar_h - margin;

  lv_obj_t* cam = chat_action_btn(root, "拍照");
  lv_obj_set_size(cam, btn_w, bar_h);
  lv_obj_set_pos(cam, margin, y);
  lv_obj_add_event_cb(cam, chat_cam_btn_cb, LV_EVENT_CLICKED, NULL);

  lv_obj_t* ptt = chat_action_btn(root, "按住说话");
  lv_obj_set_size(ptt, btn_w, bar_h);
  lv_obj_set_pos(ptt, margin + btn_w + gap, y);
  lv_obj_add_event_cb(ptt, chat_ptt_btn_cb, LV_EVENT_PRESSED, NULL);
  lv_obj_add_event_cb(ptt, chat_ptt_btn_cb, LV_EVENT_RELEASED, NULL);
  lv_obj_add_event_cb(ptt, chat_ptt_btn_cb, LV_EVENT_PRESS_LOST, NULL);
}
#endif /* BBCLAW_CAMERA_ENABLE */

/* ── theme callbacks ── */

static void theme_on_enter(lv_obj_t* parent) {
  if (s_st.built) {
    ESP_LOGW(TAG, "buddy-anim on_enter: already built");
    return;
  }
  if (parent == NULL) return;

  /* Overlay root transparent — let the underlying ACTIVE view's top bar
   * (status/WiFi/battery/clock) and bottom bar (扫描条) show through. The
   * chat theme only owns the middle transcript area. */
  s_st.root = lv_obj_create(parent);
  lv_obj_remove_style_all(s_st.root);
  lv_obj_set_size(s_st.root, lv_pct(100), lv_pct(100));
  lv_obj_set_style_bg_opa(s_st.root, LV_OPA_TRANSP, 0);
  lv_obj_clear_flag(s_st.root, LV_OBJ_FLAG_SCROLLABLE);

  /* The theme's own topbar was retired (Phase 7); those widgets live in
   * bb_lvgl_display.c's ACTIVE view underneath. Keep the pointers NULL. */
  s_st.topbar = NULL;
  s_st.topbar_icon = NULL;
  s_st.topbar_driver_lbl = NULL;
  s_st.topbar_session_lbl = NULL;
  s_st.topbar_bat_container = NULL;
  s_st.topbar_bat_fill = NULL;
  s_st.topbar_bat_frame = NULL;
  s_st.topbar_bat_lbl = NULL;

  /* ── Transcript — aligned with underlying ACTIVE view's content area ──
   * 几何从主视图内容盒取（bb_display_get_content_box），不再手抄经验值
   * （旧 320x112@y32 在手表 410x502 上只盖左上角，是 P0 破相项）。
   * 容器水平居中（TOP_MID），主要展示区落在屏幕中心带。 */
  int cb_x = 0, cb_y = 32, cb_w = 320, cb_h = 112;
  bb_display_get_content_box(&cb_x, &cb_y, &cb_w, &cb_h);
  if (cb_w <= 0 || cb_h <= 0) { /* 兜底：内容盒未初始化时退回旧经验值 */
    cb_y = 32; cb_w = 320; cb_h = 112;
  }
  s_st.transcript = bb_chat_transcript_create(s_st.root, cb_w, cb_h, cb_y);
  {
    lv_obj_t* cont = bb_chat_transcript_get_container();
    if (cont != NULL) {
      lv_obj_align(cont, LV_ALIGN_TOP_MID, 0, cb_y);
#if BB_UI_PORTRAIT
      /* 悬浮 PTT 钮遮内容区底部：补底部内边距，滚到底时最后一条气泡停在
       * 钮上方（空白垫层滚到钮后面）。 */
      lv_obj_set_style_pad_bottom(cont, BB_UI_PTT_ZONE_H, 0);
      /* 滚动条：细 4px、accent 半透明、自动隐藏、离右缘一点 */
      lv_obj_set_scrollbar_mode(cont, LV_SCROLLBAR_MODE_AUTO);
      lv_obj_set_style_width(cont, 4, LV_PART_SCROLLBAR);
      lv_obj_set_style_bg_color(cont, lv_color_hex(BB_UI_ACCENT), LV_PART_SCROLLBAR);
      lv_obj_set_style_bg_opa(cont, LV_OPA_40, LV_PART_SCROLLBAR);
      lv_obj_set_style_radius(cont, 2, LV_PART_SCROLLBAR);
      lv_obj_set_style_pad_right(cont, 6, LV_PART_SCROLLBAR);
#endif
#if !BB_UI_PORTRAIT && BBCLAW_CAMERA_ENABLE
      /* 底部功能条(拍照/按住说话)遮住内容区下缘:补底部内边距,滚到底时最后一条
       * 气泡停在钮上方(空白垫层滚到钮后面)。 */
      lv_obj_set_style_pad_bottom(cont, 56, 0);
#endif
    }
  }

  /* Recording state is shown by the ACTIVE view's bottom bar (BAR_LISTEN /
   * motif_vu, driven by is_recording_status) — the old full-width 320×112
   * recording overlay re-rendered 7 meter bars every 48ms, the dominant
   * active-state LVGL redraw that delayed state→render (录音起点/LISTEN 视觉
   * 滞后 ~240ms). Removed in favour of the cheap bottom-bar canvas strip;
   * the live mic level now feeds the bottom VU instead. */

  s_st.active_assistant = NULL;
#if BB_UI_PORTRAIT
  /* 收编悬浮 PTT 钮：作为主题根的前景子节点，设置等 overlay 在屏幕层
   * 天然盖住它（此前钮在全局顶层，浮在设置页之上——bug） */
  bb_display_ptt_button_adopt(s_st.root);
#endif
#if BBCLAW_CAMERA_ENABLE
  /* 触屏板(实战派)：对话页底部挂「拍照」+「按住说话」功能条(前景子节点)。 */
  chat_build_action_bar(s_st.root);
#endif
  s_st.built = 1;
}

static void theme_on_exit(void) {
  if (!s_st.built) return;
#if BB_UI_PORTRAIT
  /* 必须先归还（顶层+隐藏）再删根，否则钮被级联删除留下悬垂指针 */
  bb_display_ptt_button_release();
#endif
  if (s_st.root != NULL) {
    lv_obj_del(s_st.root);
  }
  s_st.root = NULL;
  s_st.topbar = NULL;
  s_st.topbar_icon = NULL;
  s_st.topbar_driver_lbl = NULL;
  s_st.topbar_session_lbl = NULL;
  s_st.topbar_bat_container = NULL;
  s_st.topbar_bat_fill = NULL;
  s_st.topbar_bat_frame = NULL;
  s_st.topbar_bat_lbl = NULL;
  s_st.transcript = NULL;
  s_st.active_assistant = NULL;
  s_st.built = 0;
  bb_chat_transcript_destroy();  /* reset internal pointers (root del cascaded) */
}

static void theme_set_state(bb_agent_state_t state) {
  s_st.state = state;
  if (!s_st.built) return;
  refresh_topbar();
}

static void theme_append_user(const char* text) {
  if (!s_st.built) return;
  bb_chat_transcript_append_user(text);
  s_st.active_assistant = NULL;
}

static void theme_append_assistant_chunk(const char* delta) {
  if (!s_st.built) return;
  bb_chat_transcript_append_assistant_chunk(delta);
  /* active_assistant tracked inside bb_chat_transcript; sync local flag */
  if (s_st.active_assistant == NULL && delta != NULL) {
    s_st.active_assistant = (lv_obj_t*)1;  /* non-null marker */
  }
}

static void theme_append_tool_call(const char* tool, const char* hint) {
  if (!s_st.built) return;
  bb_chat_transcript_append_tool_call(tool, hint);
  s_st.active_assistant = NULL;
}

static void theme_append_error(const char* msg) {
  if (!s_st.built) return;
  bb_chat_transcript_append_error(msg);
  s_st.active_assistant = NULL;
}

static void theme_set_driver(const char* driver_name) {
  if (driver_name == NULL) return;
  strncpy(s_st.driver_buf, driver_name, sizeof(s_st.driver_buf) - 1);
  s_st.driver_buf[sizeof(s_st.driver_buf) - 1] = '\0';
  if (s_st.built) refresh_topbar();
}

static void theme_set_session(const char* sid_short) {
  if (sid_short == NULL) return;
  strncpy(s_st.session_buf, sid_short, sizeof(s_st.session_buf) - 1);
  s_st.session_buf[sizeof(s_st.session_buf) - 1] = '\0';
  if (s_st.built) refresh_topbar();
}

/* Phase S3 — history replay. Delegated to bb_chat_transcript. */
static void theme_append_history_message(const char* role, const char* content,
                                         int64_t timestamp_ms) {
  if (!s_st.built) return;
  bb_chat_transcript_append_history(role, content, timestamp_ms);
  s_st.active_assistant = NULL;
}

static void theme_prepend_history_message(const char* role, const char* content,
                                          int64_t timestamp_ms) {
  if (!s_st.built) return;
  bb_chat_transcript_prepend_history(role, content, timestamp_ms);
}

static int theme_is_transcript_at_top(void) {
  if (!s_st.built) return 0;
  return bb_chat_transcript_is_at_top();
}

static void theme_scroll_transcript_to_bottom(void) {
  bb_chat_transcript_scroll_to_bottom();
}

static const bb_agent_theme_t s_buddy_anim_theme = {
    .name = "buddy-anim",
    .on_enter = theme_on_enter,
    .on_exit = theme_on_exit,
    .set_state = theme_set_state,
    .append_user = theme_append_user,
    .append_assistant_chunk = theme_append_assistant_chunk,
    .append_tool_call = theme_append_tool_call,
    .append_error = theme_append_error,
    .set_driver = theme_set_driver,
    .set_session = theme_set_session,
    .scroll_transcript = theme_scroll_transcript,
    .append_history_message = theme_append_history_message,
    .prepend_history_message = theme_prepend_history_message,
    .is_transcript_at_top = theme_is_transcript_at_top,
    .scroll_transcript_to_bottom = theme_scroll_transcript_to_bottom,
};

void bb_theme_buddy_anim_init(void) {
  bb_agent_theme_register(&s_buddy_anim_theme);
}
