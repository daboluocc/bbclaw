#include "bb_ui_agent_chat.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "bb_agent_client.h"
#include "bb_agent_theme.h"
#include "bb_time.h"
#include "esp_err.h"
#include "esp_log.h"
#include "esp_lvgl_port.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "lvgl.h"
#include "nvs.h"
#include "nvs_flash.h"

static const char* TAG = "bb_agent_ui";

/* 6 KB on internal heap. 8 KB was too generous: PTT/voice loop already
 * fragments internal heap so the largest free block is ~7.9 KB on a fresh
 * boot — xTaskCreate(8192) reliably fails. The agent task only spawns
 * http_post + cJSON parsing + lv_async_call dispatch; 6 KB has plenty of
 * margin per esp32_dump after ~10 turns of testing. */
#define BB_CHAT_AGENT_TASK_STACK 6144
#define BB_CHAT_AGENT_TASK_PRIO  5
#define BB_CHAT_HEART_THRESHOLD_MS 5000

/*
 * 屏幕静态状态。所有 LVGL 操作都必须在 LVGL 任务里完成；
 * agent 任务通过 lv_async_call 跨线程派发主题调用。
 *
 * 生命期：
 *   - bb_ui_agent_chat_show 由 LVGL 任务调用（调主题 on_enter）
 *   - bb_ui_agent_chat_send 任意线程调用，启动 agent 任务
 *   - agent 任务从 bb_agent_send_message 的 stream callback 同步收到事件后，
 *     堆上拷贝 payload，丢给 lv_async_call；payload 在 LVGL 任务侧 free
 *   - agent 任务结束时 self-delete
 */
/* Phase 4.2 picker 上限（防止意外大数组）。
 * 注意：picker 内部会在首位插入 "Settings" 行，所以实际最多容纳
 * BB_CHAT_PICKER_MAX_ITEMS - 1 个调用方提供的短语。 */
#define BB_CHAT_PICKER_MAX_ITEMS 12
#define BB_CHAT_DRIVER_CACHE_MAX 6

/* NVS 配置（与 bb_agent_theme.c 同 namespace）。 */
#define BB_CHAT_NVS_NS         "bbclaw"
#define BB_CHAT_NVS_KEY_DRIVER "agent/driver"
#define BB_CHAT_DRIVER_FALLBACK "claude-code"

typedef enum {
  BB_CHAT_MODE_PICKER = 0,  /* 默认：phrase 选择 */
  BB_CHAT_MODE_SETTINGS,    /* settings sub-menu */
} bb_chat_mode_t;

/* settings 行下标。顺序固定，长度 = 3。 */
typedef enum {
  BB_SETTINGS_ROW_AGENT = 0,
  BB_SETTINGS_ROW_THEME,
  BB_SETTINGS_ROW_BACK,
  BB_SETTINGS_ROW_COUNT,
} bb_settings_row_t;

typedef struct {
  int active;                 /* show 之后置 1，hide 后清 0 */
  int sending;                /* agent 任务在跑期间置 1 */
  TaskHandle_t agent_task;
  lv_obj_t* parent;
  bb_agent_state_t state;
  int64_t turn_start_ms;
  int saw_text_in_turn;       /* 当前 turn 是否已收到首个 TEXT（控制 BUSY 切换） */
  /* 持久化 session/driver；adapter SESSION 帧到达时更新。Phase 4.2 接入 NVS。 */
  char session_id[64];
  char driver_name[24];          /* 来自 SESSION 帧（adapter 真实分配的 driver） */
  char selected_driver[24];      /* 用户设置选中的 driver；空 = 让 adapter 默认 */

  /* picker 状态（Phase 4.2） */
  lv_obj_t* picker_root;
  lv_obj_t* picker_items[BB_CHAT_PICKER_MAX_ITEMS];
  const char* const* picker_phrases;  /* 调用方持有；不含 Settings 行 */
  int picker_count;            /* 包含首行 Settings；= caller_count + 1 */
  int picker_sel;
  /* 当前模式：picker 或 settings */
  bb_chat_mode_t mode;

  /* settings 状态（Phase 4.2.5） */
  lv_obj_t* settings_root;
  lv_obj_t* settings_rows[BB_SETTINGS_ROW_COUNT];
  int settings_sel;            /* 当前高亮行（0..BB_SETTINGS_ROW_COUNT-1） */
  /* drivers 列表懒加载缓存（首次进 settings 时拉取 adapter） */
  bb_agent_driver_info_t driver_cache[BB_CHAT_DRIVER_CACHE_MAX];
  int driver_cache_count;
  int driver_cache_offline;    /* 1 = HTTP 失败，cache 是 fallback */
  int driver_cache_idx;        /* 当前选中 driver 在 cache 中的 index */
  /* themes 列表（指针来自 bb_agent_theme_list；生命期 = 程序生命期） */
  const char* const* theme_list;
  int theme_count;
  int theme_idx;               /* 当前选中 theme 在 list 中的 index */
} bb_chat_state_t;

static bb_chat_state_t s_chat = {0};

/* ── async payload 类型（agent 线程 → LVGL 线程） ── */

typedef enum {
  BB_ASYNC_SET_STATE = 0,
  BB_ASYNC_APPEND_USER,
  BB_ASYNC_APPEND_ASSISTANT_CHUNK,
  BB_ASYNC_APPEND_TOOL_CALL,
  BB_ASYNC_APPEND_ERROR,
  BB_ASYNC_SET_DRIVER,
  BB_ASYNC_SET_SESSION,
} bb_async_kind_t;

typedef struct {
  bb_async_kind_t kind;
  bb_agent_state_t state;
  char* s1; /* text / user / driver / session / error / tool */
  char* s2; /* hint（仅 tool_call 用） */
} bb_async_payload_t;

static bb_async_payload_t* async_alloc(bb_async_kind_t kind) {
  bb_async_payload_t* p = (bb_async_payload_t*)calloc(1, sizeof(*p));
  if (p != NULL) {
    p->kind = kind;
  }
  return p;
}

static char* dup_str(const char* s) {
  if (s == NULL) return NULL;
  size_t n = strlen(s);
  char* out = (char*)malloc(n + 1);
  if (out == NULL) return NULL;
  memcpy(out, s, n + 1);
  return out;
}

/* 在 LVGL 任务里执行；结束 free payload。*/
static void on_lvgl_dispatch(void* user_data) {
  bb_async_payload_t* p = (bb_async_payload_t*)user_data;
  if (p == NULL) return;
  if (!s_chat.active) {
    /* show 已 hide：丢弃 */
    free(p->s1);
    free(p->s2);
    free(p);
    return;
  }
  const bb_agent_theme_t* theme = bb_agent_theme_get_active();
  if (theme == NULL) {
    free(p->s1);
    free(p->s2);
    free(p);
    return;
  }
  switch (p->kind) {
    case BB_ASYNC_SET_STATE:
      s_chat.state = p->state;
      if (theme->set_state != NULL) theme->set_state(p->state);
      break;
    case BB_ASYNC_APPEND_USER:
      if (theme->append_user != NULL) theme->append_user(p->s1 != NULL ? p->s1 : "");
      break;
    case BB_ASYNC_APPEND_ASSISTANT_CHUNK:
      if (theme->append_assistant_chunk != NULL)
        theme->append_assistant_chunk(p->s1 != NULL ? p->s1 : "");
      break;
    case BB_ASYNC_APPEND_TOOL_CALL:
      if (theme->append_tool_call != NULL)
        theme->append_tool_call(p->s1 != NULL ? p->s1 : "(tool)", p->s2 != NULL ? p->s2 : "");
      break;
    case BB_ASYNC_APPEND_ERROR:
      if (theme->append_error != NULL) theme->append_error(p->s1 != NULL ? p->s1 : "(error)");
      break;
    case BB_ASYNC_SET_DRIVER:
      if (theme->set_driver != NULL) theme->set_driver(p->s1 != NULL ? p->s1 : "");
      break;
    case BB_ASYNC_SET_SESSION:
      if (theme->set_session != NULL) theme->set_session(p->s1 != NULL ? p->s1 : "");
      break;
  }
  free(p->s1);
  free(p->s2);
  free(p);
}

static void async_post(bb_async_payload_t* p) {
  if (p == NULL) return;
  /* lv_async_call 内部把 cb 排到下一帧；可在任意线程调用。 */
  lv_async_call(on_lvgl_dispatch, p);
}

static void post_state(bb_agent_state_t st) {
  bb_async_payload_t* p = async_alloc(BB_ASYNC_SET_STATE);
  if (p == NULL) return;
  p->state = st;
  async_post(p);
}

static void post_user(const char* text) {
  bb_async_payload_t* p = async_alloc(BB_ASYNC_APPEND_USER);
  if (p == NULL) return;
  p->s1 = dup_str(text);
  async_post(p);
}

static void post_assistant_chunk(const char* delta) {
  bb_async_payload_t* p = async_alloc(BB_ASYNC_APPEND_ASSISTANT_CHUNK);
  if (p == NULL) return;
  p->s1 = dup_str(delta);
  async_post(p);
}

static void post_tool_call(const char* tool, const char* hint) {
  bb_async_payload_t* p = async_alloc(BB_ASYNC_APPEND_TOOL_CALL);
  if (p == NULL) return;
  p->s1 = dup_str(tool);
  p->s2 = dup_str(hint);
  async_post(p);
}

static void post_error(const char* msg) {
  bb_async_payload_t* p = async_alloc(BB_ASYNC_APPEND_ERROR);
  if (p == NULL) return;
  p->s1 = dup_str(msg);
  async_post(p);
}

static void post_driver(const char* drv) {
  bb_async_payload_t* p = async_alloc(BB_ASYNC_SET_DRIVER);
  if (p == NULL) return;
  p->s1 = dup_str(drv);
  async_post(p);
}

static void post_session(const char* sid_short) {
  bb_async_payload_t* p = async_alloc(BB_ASYNC_SET_SESSION);
  if (p == NULL) return;
  p->s1 = dup_str(sid_short);
  async_post(p);
}

/* ── stream callback：在 agent 任务里同步调用 ── */

static void on_agent_event(const bb_agent_stream_event_t* evt, void* user_ctx) {
  (void)user_ctx;
  if (evt == NULL) return;

  switch (evt->type) {
    case BB_AGENT_EVENT_SESSION:
      if (evt->session_id != NULL) {
        strncpy(s_chat.session_id, evt->session_id, sizeof(s_chat.session_id) - 1);
        s_chat.session_id[sizeof(s_chat.session_id) - 1] = '\0';
        char shortbuf[16] = {0};
        strncpy(shortbuf, evt->session_id, 8);
        shortbuf[8] = '\0';
        post_session(shortbuf);
      }
      if (evt->driver != NULL) {
        strncpy(s_chat.driver_name, evt->driver, sizeof(s_chat.driver_name) - 1);
        s_chat.driver_name[sizeof(s_chat.driver_name) - 1] = '\0';
        post_driver(evt->driver);
      }
      /* 收到 session 帧通常意味着马上要开始流式回复。先切到 BUSY，
       * 让顶部状态条变化更明确（首个 TEXT 帧到达前就有反馈）。 */
      post_state(BB_AGENT_STATE_BUSY);
      break;

    case BB_AGENT_EVENT_TEXT:
      if (!s_chat.saw_text_in_turn) {
        s_chat.saw_text_in_turn = 1;
        post_state(BB_AGENT_STATE_BUSY);
      }
      if (evt->text != NULL && evt->text[0] != '\0') {
        post_assistant_chunk(evt->text);
      }
      break;

    case BB_AGENT_EVENT_TOOL_CALL:
      post_state(BB_AGENT_STATE_ATTENTION);
      post_tool_call(evt->tool != NULL ? evt->tool : "tool",
                     evt->hint != NULL ? evt->hint : "");
      break;

    case BB_AGENT_EVENT_TOKENS:
      /* 4.1 不做 tokens 角标——后续阶段在 text-only 主题里加。 */
      break;

    case BB_AGENT_EVENT_TURN_END: {
      int64_t now = bb_now_ms();
      int64_t elapsed = now - s_chat.turn_start_ms;
      if (s_chat.turn_start_ms > 0 && elapsed >= 0 && elapsed < BB_CHAT_HEART_THRESHOLD_MS) {
        post_state(BB_AGENT_STATE_HEART);
      } else {
        post_state(BB_AGENT_STATE_IDLE);
      }
      break;
    }

    case BB_AGENT_EVENT_ERROR:
      post_state(BB_AGENT_STATE_DIZZY);
      if (evt->text != NULL && evt->text[0] != '\0') {
        post_error(evt->text);
      } else if (evt->error_code != NULL) {
        post_error(evt->error_code);
      } else {
        post_error("unknown error");
      }
      break;
  }
}

/* ── agent task ── */

typedef struct {
  char* text;
  char session_id[64];
  char driver_name[24];
} bb_agent_task_args_t;

static void agent_task(void* arg) {
  bb_agent_task_args_t* args = (bb_agent_task_args_t*)arg;

  s_chat.turn_start_ms = bb_now_ms();
  s_chat.saw_text_in_turn = 0;

  /* Push BUSY immediately so the user gets visible feedback that the click
   * was accepted, even before HTTP connect lands. Without this the topbar
   * stayed on the previous state (IDLE/HEART) while the user mashed the
   * picker repeatedly during a slow connect — see real-device debug log
   * 2026-04-25. The on_agent_event handler will switch to BUSY on the
   * first SESSION/TEXT frame too; this make-it-stick early. */
  post_state(BB_AGENT_STATE_BUSY);

  esp_err_t err = bb_agent_send_message(
      args->text,
      args->session_id[0] != '\0' ? args->session_id : NULL,
      args->driver_name[0] != '\0' ? args->driver_name : NULL,
      on_agent_event,
      NULL);

  if (err != ESP_OK) {
    ESP_LOGW(TAG, "agent send failed: %s", esp_err_to_name(err));
    /* 没有 ERROR 帧的失败（比如 HTTP 层）：手动派发一次错误 + DIZZY。 */
    if (s_chat.state != BB_AGENT_STATE_DIZZY) {
      post_state(BB_AGENT_STATE_DIZZY);
      post_error(esp_err_to_name(err));
    }
  }

  free(args->text);
  free(args);

  s_chat.sending = 0;
  s_chat.agent_task = NULL;
  vTaskDelete(NULL);
}

/* ── public API ── */

/* 从 NVS 加载用户上次选中的 driver 名；找不到则保持空字符串（让 adapter 默认）。 */
static void load_selected_driver_from_nvs(void) {
  s_chat.selected_driver[0] = '\0';
  nvs_handle_t h;
  esp_err_t err = nvs_open(BB_CHAT_NVS_NS, NVS_READONLY, &h);
  if (err != ESP_OK) {
    ESP_LOGI(TAG, "selected_driver: no nvs (%s), leaving empty", esp_err_to_name(err));
    return;
  }
  size_t sz = sizeof(s_chat.selected_driver);
  err = nvs_get_str(h, BB_CHAT_NVS_KEY_DRIVER, s_chat.selected_driver, &sz);
  nvs_close(h);
  if (err != ESP_OK) {
    s_chat.selected_driver[0] = '\0';
    ESP_LOGI(TAG, "selected_driver: not in nvs (%s)", esp_err_to_name(err));
    return;
  }
  ESP_LOGI(TAG, "selected_driver: loaded '%s'", s_chat.selected_driver);
}

static esp_err_t persist_selected_driver(const char* name) {
  if (name == NULL) return ESP_ERR_INVALID_ARG;
  nvs_handle_t h;
  esp_err_t err = nvs_open(BB_CHAT_NVS_NS, NVS_READWRITE, &h);
  if (err != ESP_OK) return err;
  err = nvs_set_str(h, BB_CHAT_NVS_KEY_DRIVER, name);
  if (err == ESP_OK) {
    err = nvs_commit(h);
  }
  nvs_close(h);
  return err;
}

void bb_ui_agent_chat_show(lv_obj_t* parent) {
  if (parent == NULL) {
    ESP_LOGE(TAG, "show: parent is NULL");
    return;
  }
  if (s_chat.active) {
    ESP_LOGW(TAG, "show: already active, ignoring");
    return;
  }
  const bb_agent_theme_t* theme = bb_agent_theme_get_active();
  if (theme == NULL) {
    ESP_LOGE(TAG, "show: no active theme registered");
    return;
  }
  s_chat.parent = parent;
  s_chat.state = BB_AGENT_STATE_SLEEP;
  s_chat.saw_text_in_turn = 0;
  s_chat.turn_start_ms = 0;
  s_chat.mode = BB_CHAT_MODE_PICKER;
  s_chat.active = 1;
  load_selected_driver_from_nvs();

  ESP_LOGI(TAG, "show: theme=%s", theme->name != NULL ? theme->name : "(unnamed)");
  if (theme->on_enter != NULL) theme->on_enter(parent);
  if (theme->set_state != NULL) theme->set_state(BB_AGENT_STATE_SLEEP);
  if (s_chat.driver_name[0] != '\0' && theme->set_driver != NULL) {
    theme->set_driver(s_chat.driver_name);
  }
  if (s_chat.session_id[0] != '\0' && theme->set_session != NULL) {
    char shortbuf[16] = {0};
    strncpy(shortbuf, s_chat.session_id, 8);
    shortbuf[8] = '\0';
    theme->set_session(shortbuf);
  }
}

void bb_ui_agent_chat_hide(void) {
  if (!s_chat.active) {
    return;
  }
  /* 把 active 先清，让任何 in-flight 的 async dispatch 直接丢弃。
   * 在跑的 agent task 不强 kill；它结束后 self-delete。 */
  s_chat.active = 0;
  /* picker / settings 都是 parent 的子；caller 会在 hide 后 delete parent，
   * 这里只是清指针避免悬挂。 */
  s_chat.picker_root = NULL;
  for (int i = 0; i < BB_CHAT_PICKER_MAX_ITEMS; ++i) {
    s_chat.picker_items[i] = NULL;
  }
  s_chat.picker_phrases = NULL;
  s_chat.picker_count = 0;
  s_chat.picker_sel = 0;
  s_chat.settings_root = NULL;
  for (int i = 0; i < BB_SETTINGS_ROW_COUNT; ++i) {
    s_chat.settings_rows[i] = NULL;
  }
  s_chat.settings_sel = 0;
  s_chat.mode = BB_CHAT_MODE_PICKER;

  const bb_agent_theme_t* theme = bb_agent_theme_get_active();
  if (theme != NULL && theme->on_exit != NULL) {
    theme->on_exit();
  }
  s_chat.parent = NULL;
  ESP_LOGI(TAG, "hide");
}

esp_err_t bb_ui_agent_chat_send(const char* text) {
  if (text == NULL || text[0] == '\0') {
    return ESP_ERR_INVALID_ARG;
  }
  if (!s_chat.active) {
    return ESP_ERR_INVALID_STATE;
  }
  if (s_chat.sending) {
    ESP_LOGW(TAG, "send: still sending, refused");
    return ESP_ERR_INVALID_STATE;
  }

  bb_agent_task_args_t* args = (bb_agent_task_args_t*)calloc(1, sizeof(*args));
  if (args == NULL) return ESP_ERR_NO_MEM;
  args->text = dup_str(text);
  if (args->text == NULL) {
    free(args);
    return ESP_ERR_NO_MEM;
  }
  /* 在调用线程的视角下 session/driver 是稳定字符串，复制进任务参数。
   * 注意：driver 优先用用户在 settings 里选的 selected_driver；
   * 为空时（首次启动 / NVS 没值）退回 driver_name（来自上次 SESSION 帧），
   * 仍为空就传 NULL，让 adapter 选默认。 */
  strncpy(args->session_id, s_chat.session_id, sizeof(args->session_id) - 1);
  if (s_chat.selected_driver[0] != '\0') {
    strncpy(args->driver_name, s_chat.selected_driver, sizeof(args->driver_name) - 1);
  } else {
    strncpy(args->driver_name, s_chat.driver_name, sizeof(args->driver_name) - 1);
  }

  /* 立刻在 transcript 里渲染用户那行（不等回包）。这步是 LVGL 操作，
   * 但 send() 既可能在 LVGL 任务也可能在按键任务里被调；统一走 async。 */
  post_user(text);

  s_chat.sending = 1;
  s_chat.turn_start_ms = bb_now_ms();
  s_chat.saw_text_in_turn = 0;

  BaseType_t ok = xTaskCreate(agent_task, "bb_agent_task", BB_CHAT_AGENT_TASK_STACK, args,
                              BB_CHAT_AGENT_TASK_PRIO, &s_chat.agent_task);
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "send: xTaskCreate failed");
    free(args->text);
    free(args);
    s_chat.sending = 0;
    return ESP_FAIL;
  }
  return ESP_OK;
}

/* ─────────────────────────────────────────────────────────────────────
 * Phase 4.2 — preset phrase picker
 *
 * The picker is a thin overlay (~52px tall) anchored to the bottom of the
 * agent-chat parent container. It deliberately sits ON TOP of whatever the
 * theme drew there (text-only theme reserves a 20px input row that says
 * "(input not wired yet)"; we cover it).
 *
 * Visual model: a column of label rows. The selected row gets a brighter
 * background. There is no LVGL focus group here (single source of truth is
 * s_chat.picker_sel + an explicit re-render).
 * ───────────────────────────────────────────────────────────────────── */

#define BB_PICKER_BG       0x12211b
#define BB_PICKER_FG       0xc8e2d6
#define BB_PICKER_FG_DIM   0x6b8c80
#define BB_PICKER_SEL_BG   0x2ec4a0
#define BB_PICKER_SEL_FG   0x0a0e0c
#define BB_PICKER_ROW_H    16
#define BB_PICKER_VISIBLE  3   /* show 3 rows; user sees current + neighbors */

static void picker_apply_styles(void) {
  if (s_chat.picker_count == 0) return;
  /* "viewport" = which contiguous slice of items is visible */
  int first = s_chat.picker_sel - BB_PICKER_VISIBLE / 2;
  if (first < 0) first = 0;
  if (first + BB_PICKER_VISIBLE > s_chat.picker_count) {
    first = s_chat.picker_count - BB_PICKER_VISIBLE;
    if (first < 0) first = 0;
  }
  for (int i = 0; i < s_chat.picker_count; ++i) {
    lv_obj_t* row = s_chat.picker_items[i];
    if (row == NULL) continue;
    int visible = (i >= first && i < first + BB_PICKER_VISIBLE);
    if (visible) {
      lv_obj_clear_flag(row, LV_OBJ_FLAG_HIDDEN);
    } else {
      lv_obj_add_flag(row, LV_OBJ_FLAG_HIDDEN);
    }
    if (i == s_chat.picker_sel) {
      lv_obj_set_style_bg_color(row, lv_color_hex(BB_PICKER_SEL_BG), 0);
      lv_obj_set_style_bg_opa(row, LV_OPA_COVER, 0);
      lv_obj_set_style_text_color(row, lv_color_hex(BB_PICKER_SEL_FG), 0);
    } else {
      lv_obj_set_style_bg_opa(row, LV_OPA_TRANSP, 0);
      lv_obj_set_style_text_color(row, lv_color_hex(BB_PICKER_FG_DIM), 0);
    }
  }
}

/* Phase 4.2.5: picker 首行固定为 "Settings"，剩下的 row 来自调用方 phrases。
 * 这个标签只是一个 ASCII 字符串；屏幕字体未必有 ⚙ glyph，所以保持纯 ASCII。 */
#define BB_PICKER_SETTINGS_LABEL "Settings"

void bb_ui_agent_chat_picker_show(const char* const* phrases, int count) {
  if (!s_chat.active || s_chat.parent == NULL) {
    ESP_LOGW(TAG, "picker_show: chat not active");
    return;
  }
  if (phrases == NULL || count <= 0) return;
  /* 内部首行是 Settings，所以调用方 phrases 最多 BB_CHAT_PICKER_MAX_ITEMS - 1 个。 */
  if (count > BB_CHAT_PICKER_MAX_ITEMS - 1) count = BB_CHAT_PICKER_MAX_ITEMS - 1;
  const int total = count + 1;  /* +1 for Settings row */

  /* Replace any prior picker. */
  if (s_chat.picker_root != NULL) {
    lv_obj_del(s_chat.picker_root);
    s_chat.picker_root = NULL;
    for (int i = 0; i < BB_CHAT_PICKER_MAX_ITEMS; ++i) s_chat.picker_items[i] = NULL;
  }

  s_chat.picker_phrases = phrases;
  s_chat.picker_count = total;
  /* 默认高亮第一条短语（index 1），Settings 在 index 0 旋上去就能看到。 */
  s_chat.picker_sel = total > 1 ? 1 : 0;
  s_chat.mode = BB_CHAT_MODE_PICKER;

  const int picker_h = BB_PICKER_ROW_H * BB_PICKER_VISIBLE + 4;

  s_chat.picker_root = lv_obj_create(s_chat.parent);
  lv_obj_remove_style_all(s_chat.picker_root);
  lv_obj_set_size(s_chat.picker_root, lv_pct(100), picker_h);
  lv_obj_align(s_chat.picker_root, LV_ALIGN_BOTTOM_LEFT, 0, 0);
  lv_obj_set_style_bg_color(s_chat.picker_root, lv_color_hex(BB_PICKER_BG), 0);
  lv_obj_set_style_bg_opa(s_chat.picker_root, LV_OPA_COVER, 0);
  lv_obj_set_style_pad_all(s_chat.picker_root, 2, 0);
  lv_obj_set_flex_flow(s_chat.picker_root, LV_FLEX_FLOW_COLUMN);
  lv_obj_clear_flag(s_chat.picker_root, LV_OBJ_FLAG_SCROLLABLE);
  /* Make sure picker overlays the theme's input area. */
  lv_obj_move_foreground(s_chat.picker_root);

  for (int i = 0; i < total; ++i) {
    lv_obj_t* row = lv_label_create(s_chat.picker_root);
    lv_obj_set_size(row, lv_pct(100), BB_PICKER_ROW_H);
    lv_obj_set_style_pad_left(row, 4, 0);
    lv_obj_set_style_pad_right(row, 4, 0);
    lv_obj_set_style_radius(row, 3, 0);
    lv_obj_set_style_text_color(row, lv_color_hex(BB_PICKER_FG), 0);
    lv_label_set_long_mode(row, LV_LABEL_LONG_MODE_DOTS);
    if (i == 0) {
      lv_label_set_text(row, BB_PICKER_SETTINGS_LABEL);
    } else {
      const char* p = phrases[i - 1];
      lv_label_set_text(row, p != NULL ? p : "");
    }
    s_chat.picker_items[i] = row;
  }
  picker_apply_styles();
}

void bb_ui_agent_chat_picker_move(int delta) {
  if (!s_chat.active || s_chat.picker_root == NULL || s_chat.picker_count == 0) return;
  int sel = s_chat.picker_sel + delta;
  if (sel < 0) sel = 0;
  if (sel >= s_chat.picker_count) sel = s_chat.picker_count - 1;
  if (sel == s_chat.picker_sel) return;
  s_chat.picker_sel = sel;
  picker_apply_styles();
}

/* Forward decl —— 在 settings 区段下面定义。 */
static void settings_show(void);

esp_err_t bb_ui_agent_chat_picker_send_selected(void) {
  if (!s_chat.active || s_chat.picker_root == NULL || s_chat.picker_count == 0) {
    return ESP_ERR_INVALID_STATE;
  }
  if (s_chat.picker_sel < 0 || s_chat.picker_sel >= s_chat.picker_count) {
    return ESP_ERR_INVALID_STATE;
  }
  /* index 0 是固定的 "Settings" 行，不发消息——切到 settings 子菜单。 */
  if (s_chat.picker_sel == 0) {
    settings_show();
    return ESP_OK;
  }
  if (s_chat.picker_phrases == NULL) return ESP_ERR_INVALID_STATE;
  const char* phrase = s_chat.picker_phrases[s_chat.picker_sel - 1];
  if (phrase == NULL || phrase[0] == '\0') return ESP_ERR_INVALID_ARG;
  return bb_ui_agent_chat_send(phrase);
}

/* ─────────────────────────────────────────────────────────────────────
 * Phase 4.2.5 — Settings overlay (Agent / Theme / Back)
 *
 * Settings 复用 picker 的视觉语言（同一组配色、同一行高），但布局更紧凑：
 * 三行固定，每行内部还要显示当前选中的值，用 "Label: value" 格式。
 * 旋转改的是当前行内部光标（cycle 一个 driver 或 theme），不是行下标——
 * 行下标用 click 来推进（或者说：每次 click 提交当前行 + 进到下一行；
 * 不过依据 Phase 4.2.5 spec，更直接的语义是：
 *   行 0/1：rotate 切换值，click 提交（持久化 + 应用）
 *   行 2 (Back)：rotate 不响应，click 回到 picker
 * 这里采用 spec 语义。要切换"当前是哪一行"的概念目前用 ROTATE 也能做——
 * 选行 vs 改值用单一旋转轮做：限制只在 click 之后才接受 rotate 切行。
 * 但 spec 明确说 "rotate left/right cycles through the array"——也就是
 * rotate 始终改值。要改行只能 click。我们就这么做。
 *
 * 兜底：万一只有 1 个 driver / 1 个 theme，rotate 不会有可视变化（cycle 等同
 * 不变），用户会困惑。我们在标签里附加 "(1/1)" 让它至少是 self-evident。
 * ───────────────────────────────────────────────────────────────────── */

#define BB_SETTINGS_ROW_H    16
#define BB_SETTINGS_VISIBLE  3
#define BB_DRIVER_LIST_TIMEOUT_MS 4000

static const char* settings_active_driver_name(void) {
  if (s_chat.driver_cache_count <= 0) return "(none)";
  int idx = s_chat.driver_cache_idx;
  if (idx < 0 || idx >= s_chat.driver_cache_count) return "(none)";
  const char* n = s_chat.driver_cache[idx].name;
  return (n != NULL && n[0] != '\0') ? n : "(unnamed)";
}

static const char* settings_active_theme_name(void) {
  if (s_chat.theme_list == NULL || s_chat.theme_count <= 0) return "(none)";
  int idx = s_chat.theme_idx;
  if (idx < 0 || idx >= s_chat.theme_count) return "(none)";
  const char* n = s_chat.theme_list[idx];
  return (n != NULL && n[0] != '\0') ? n : "(unnamed)";
}

/* 重画三行的文字 + 高亮。 */
static void settings_apply_rows(void) {
  if (s_chat.settings_root == NULL) return;
  char buf[64];

  /* Row 0: Agent: <name> [(offline)] [(i/N)] */
  if (s_chat.settings_rows[BB_SETTINGS_ROW_AGENT] != NULL) {
    const char* drv = settings_active_driver_name();
    if (s_chat.driver_cache_offline) {
      snprintf(buf, sizeof(buf), "Agent: %s (offline)", drv);
    } else if (s_chat.driver_cache_count > 1) {
      snprintf(buf, sizeof(buf), "Agent: %s (%d/%d)", drv,
               s_chat.driver_cache_idx + 1, s_chat.driver_cache_count);
    } else {
      snprintf(buf, sizeof(buf), "Agent: %s", drv);
    }
    lv_label_set_text(s_chat.settings_rows[BB_SETTINGS_ROW_AGENT], buf);
  }

  /* Row 1: Theme: <name> [(i/N)] */
  if (s_chat.settings_rows[BB_SETTINGS_ROW_THEME] != NULL) {
    const char* th = settings_active_theme_name();
    if (s_chat.theme_count > 1) {
      snprintf(buf, sizeof(buf), "Theme: %s (%d/%d)", th, s_chat.theme_idx + 1,
               s_chat.theme_count);
    } else {
      snprintf(buf, sizeof(buf), "Theme: %s", th);
    }
    lv_label_set_text(s_chat.settings_rows[BB_SETTINGS_ROW_THEME], buf);
  }

  /* Row 2: Back */
  if (s_chat.settings_rows[BB_SETTINGS_ROW_BACK] != NULL) {
    lv_label_set_text(s_chat.settings_rows[BB_SETTINGS_ROW_BACK], "Back");
  }

  /* 高亮 */
  for (int i = 0; i < BB_SETTINGS_ROW_COUNT; ++i) {
    lv_obj_t* row = s_chat.settings_rows[i];
    if (row == NULL) continue;
    if (i == s_chat.settings_sel) {
      lv_obj_set_style_bg_color(row, lv_color_hex(BB_PICKER_SEL_BG), 0);
      lv_obj_set_style_bg_opa(row, LV_OPA_COVER, 0);
      lv_obj_set_style_text_color(row, lv_color_hex(BB_PICKER_SEL_FG), 0);
    } else {
      lv_obj_set_style_bg_opa(row, LV_OPA_TRANSP, 0);
      lv_obj_set_style_text_color(row, lv_color_hex(BB_PICKER_FG_DIM), 0);
    }
  }
}

/* 第一次进 settings 时拉一次 adapter driver 列表；HTTP 失败 fallback 一个固定项。
 * 之后保持缓存（直到 hide）；不主动刷新。 */
static void settings_ensure_driver_cache(void) {
  if (s_chat.driver_cache_count > 0) return;
  int total = 0;
  esp_err_t err = bb_agent_list_drivers(s_chat.driver_cache, BB_CHAT_DRIVER_CACHE_MAX, &total);
  if (err != ESP_OK || total <= 0) {
    ESP_LOGW(TAG, "settings: list_drivers failed (%s), fallback to '%s'",
             esp_err_to_name(err), BB_CHAT_DRIVER_FALLBACK);
    memset(&s_chat.driver_cache[0], 0, sizeof(s_chat.driver_cache[0]));
    strncpy(s_chat.driver_cache[0].name, BB_CHAT_DRIVER_FALLBACK,
            sizeof(s_chat.driver_cache[0].name) - 1);
    s_chat.driver_cache_count = 1;
    s_chat.driver_cache_offline = 1;
  } else {
    /* total 可能 > BB_CHAT_DRIVER_CACHE_MAX；list api 仍会按 cap 写入。 */
    if (total > BB_CHAT_DRIVER_CACHE_MAX) total = BB_CHAT_DRIVER_CACHE_MAX;
    s_chat.driver_cache_count = total;
    s_chat.driver_cache_offline = 0;
    ESP_LOGI(TAG, "settings: %d driver(s) cached", total);
  }

  /* 把 selected_driver / driver_name 映射到 cache 里的 idx */
  int idx = 0;
  const char* prefer = s_chat.selected_driver[0] != '\0' ? s_chat.selected_driver
                       : (s_chat.driver_name[0] != '\0' ? s_chat.driver_name : NULL);
  if (prefer != NULL) {
    for (int i = 0; i < s_chat.driver_cache_count; ++i) {
      if (strcmp(s_chat.driver_cache[i].name, prefer) == 0) {
        idx = i;
        break;
      }
    }
  }
  s_chat.driver_cache_idx = idx;
}

static void settings_ensure_theme_cache(void) {
  if (s_chat.theme_list != NULL && s_chat.theme_count > 0) return;
  int n = 0;
  s_chat.theme_list = bb_agent_theme_list(&n);
  s_chat.theme_count = n;
  /* 把当前激活的 theme 映射到 idx */
  const bb_agent_theme_t* cur = bb_agent_theme_get_active();
  int idx = 0;
  if (cur != NULL && cur->name != NULL && s_chat.theme_list != NULL) {
    for (int i = 0; i < s_chat.theme_count; ++i) {
      if (s_chat.theme_list[i] != NULL && strcmp(s_chat.theme_list[i], cur->name) == 0) {
        idx = i;
        break;
      }
    }
  }
  s_chat.theme_idx = idx;
}

/* 切回 picker；不重建 phrases（picker 还活着，被隐藏）。 */
static void settings_back_to_picker(void) {
  if (s_chat.settings_root != NULL) {
    lv_obj_del(s_chat.settings_root);
    s_chat.settings_root = NULL;
    for (int i = 0; i < BB_SETTINGS_ROW_COUNT; ++i) s_chat.settings_rows[i] = NULL;
  }
  s_chat.mode = BB_CHAT_MODE_PICKER;
  if (s_chat.picker_root != NULL) {
    lv_obj_clear_flag(s_chat.picker_root, LV_OBJ_FLAG_HIDDEN);
    lv_obj_move_foreground(s_chat.picker_root);
  }
}

/* 进 settings：构建新覆盖层；隐藏 picker。 */
static void settings_show(void) {
  if (!s_chat.active || s_chat.parent == NULL) return;
  /* 把 picker 暂时隐藏，避免两层叠在底部互相挡。 */
  if (s_chat.picker_root != NULL) {
    lv_obj_add_flag(s_chat.picker_root, LV_OBJ_FLAG_HIDDEN);
  }

  /* 拉/同步缓存。 */
  settings_ensure_driver_cache();
  settings_ensure_theme_cache();

  s_chat.settings_sel = BB_SETTINGS_ROW_AGENT;

  /* 已有 settings_root（连续 click？）：先拆。 */
  if (s_chat.settings_root != NULL) {
    lv_obj_del(s_chat.settings_root);
    s_chat.settings_root = NULL;
    for (int i = 0; i < BB_SETTINGS_ROW_COUNT; ++i) s_chat.settings_rows[i] = NULL;
  }

  const int settings_h = BB_SETTINGS_ROW_H * BB_SETTINGS_VISIBLE + 4;
  s_chat.settings_root = lv_obj_create(s_chat.parent);
  lv_obj_remove_style_all(s_chat.settings_root);
  lv_obj_set_size(s_chat.settings_root, lv_pct(100), settings_h);
  lv_obj_align(s_chat.settings_root, LV_ALIGN_BOTTOM_LEFT, 0, 0);
  lv_obj_set_style_bg_color(s_chat.settings_root, lv_color_hex(BB_PICKER_BG), 0);
  lv_obj_set_style_bg_opa(s_chat.settings_root, LV_OPA_COVER, 0);
  lv_obj_set_style_pad_all(s_chat.settings_root, 2, 0);
  lv_obj_set_flex_flow(s_chat.settings_root, LV_FLEX_FLOW_COLUMN);
  lv_obj_clear_flag(s_chat.settings_root, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_move_foreground(s_chat.settings_root);

  for (int i = 0; i < BB_SETTINGS_ROW_COUNT; ++i) {
    lv_obj_t* row = lv_label_create(s_chat.settings_root);
    lv_obj_set_size(row, lv_pct(100), BB_SETTINGS_ROW_H);
    lv_obj_set_style_pad_left(row, 4, 0);
    lv_obj_set_style_pad_right(row, 4, 0);
    lv_obj_set_style_radius(row, 3, 0);
    lv_obj_set_style_text_color(row, lv_color_hex(BB_PICKER_FG), 0);
    lv_label_set_long_mode(row, LV_LABEL_LONG_MODE_DOTS);
    lv_label_set_text(row, "");
    s_chat.settings_rows[i] = row;
  }
  s_chat.mode = BB_CHAT_MODE_SETTINGS;
  settings_apply_rows();
  ESP_LOGI(TAG, "settings: opened (drivers=%d, themes=%d)",
           s_chat.driver_cache_count, s_chat.theme_count);
}

/* 公开 API ──── 被 bb_radio_app.c 路由调用（在 LVGL 锁里调）。 */

int bb_ui_agent_chat_in_settings(void) {
  return (s_chat.active && s_chat.mode == BB_CHAT_MODE_SETTINGS) ? 1 : 0;
}

void bb_ui_agent_chat_settings_handle_rotate(int delta) {
  if (!s_chat.active || s_chat.mode != BB_CHAT_MODE_SETTINGS) return;
  if (delta == 0) return;
  switch ((bb_settings_row_t)s_chat.settings_sel) {
    case BB_SETTINGS_ROW_AGENT: {
      if (s_chat.driver_cache_count <= 0) return;
      int next = s_chat.driver_cache_idx + delta;
      /* clamp 而非 wrap：与 picker 一致的反馈风格。 */
      if (next < 0) next = 0;
      if (next >= s_chat.driver_cache_count) next = s_chat.driver_cache_count - 1;
      s_chat.driver_cache_idx = next;
      break;
    }
    case BB_SETTINGS_ROW_THEME: {
      if (s_chat.theme_count <= 0) return;
      int next = s_chat.theme_idx + delta;
      if (next < 0) next = 0;
      if (next >= s_chat.theme_count) next = s_chat.theme_count - 1;
      s_chat.theme_idx = next;
      break;
    }
    case BB_SETTINGS_ROW_BACK:
    case BB_SETTINGS_ROW_COUNT:
    default:
      /* Back 行：rotate 不切值，但允许"换行" —— 一次 rotate 把光标移回上一行。
       * 不过这会破坏"rotate = cycle value" 的统一约定。简单起见：忽略。 */
      return;
  }
  settings_apply_rows();
}

void bb_ui_agent_chat_settings_handle_click(void) {
  if (!s_chat.active || s_chat.mode != BB_CHAT_MODE_SETTINGS) return;
  switch ((bb_settings_row_t)s_chat.settings_sel) {
    case BB_SETTINGS_ROW_AGENT: {
      if (s_chat.driver_cache_count <= 0) {
        /* 推进到下一行。 */
        s_chat.settings_sel = BB_SETTINGS_ROW_THEME;
        settings_apply_rows();
        return;
      }
      const char* name = s_chat.driver_cache[s_chat.driver_cache_idx].name;
      if (name == NULL || name[0] == '\0') return;
      /* 持久化到 NVS + 同步 selected_driver + 立刻刷新 topbar。 */
      esp_err_t err = persist_selected_driver(name);
      if (err != ESP_OK) {
        ESP_LOGW(TAG, "settings: persist driver '%s' failed (%s)", name, esp_err_to_name(err));
      }
      strncpy(s_chat.selected_driver, name, sizeof(s_chat.selected_driver) - 1);
      s_chat.selected_driver[sizeof(s_chat.selected_driver) - 1] = '\0';
      const bb_agent_theme_t* theme = bb_agent_theme_get_active();
      if (theme != NULL && theme->set_driver != NULL) {
        theme->set_driver(name);
      }
      ESP_LOGI(TAG, "settings: driver -> '%s'", name);
      /* 推进光标到下一行，让 click 自然走完。 */
      s_chat.settings_sel = BB_SETTINGS_ROW_THEME;
      settings_apply_rows();
      break;
    }
    case BB_SETTINGS_ROW_THEME: {
      if (s_chat.theme_count <= 0 || s_chat.theme_list == NULL) {
        s_chat.settings_sel = BB_SETTINGS_ROW_BACK;
        settings_apply_rows();
        return;
      }
      const char* name = s_chat.theme_list[s_chat.theme_idx];
      if (name == NULL || name[0] == '\0') return;
      /* 同主题：仍走一次 set_active 是幂等的；但 on_exit/on_enter 会闪——
       * 跳过 rebuild。 */
      const bb_agent_theme_t* cur = bb_agent_theme_get_active();
      int same = (cur != NULL && cur->name != NULL && strcmp(cur->name, name) == 0);
      esp_err_t err = bb_agent_theme_set_active(name);
      if (err != ESP_OK) {
        ESP_LOGW(TAG, "settings: theme set '%s' failed (%s)", name, esp_err_to_name(err));
        return;
      }
      ESP_LOGI(TAG, "settings: theme -> '%s'", name);
      if (!same) {
        /* live rebuild：把 settings + picker overlay 都拆掉，重建主题，
         * 再恢复 picker（settings 走完一次回到 picker 是合理的 UX）。 */
        const bb_agent_theme_t* old_theme = cur;
        const bb_agent_theme_t* new_theme = bb_agent_theme_get_active();
        if (s_chat.settings_root != NULL) {
          lv_obj_del(s_chat.settings_root);
          s_chat.settings_root = NULL;
          for (int i = 0; i < BB_SETTINGS_ROW_COUNT; ++i) s_chat.settings_rows[i] = NULL;
        }
        if (s_chat.picker_root != NULL) {
          lv_obj_del(s_chat.picker_root);
          s_chat.picker_root = NULL;
          for (int i = 0; i < BB_CHAT_PICKER_MAX_ITEMS; ++i) s_chat.picker_items[i] = NULL;
        }
        if (old_theme != NULL && old_theme->on_exit != NULL) old_theme->on_exit();
        if (new_theme != NULL && new_theme->on_enter != NULL) new_theme->on_enter(s_chat.parent);
        /* 恢复 driver/session 显示。 */
        if (new_theme != NULL && s_chat.driver_name[0] != '\0' && new_theme->set_driver != NULL) {
          new_theme->set_driver(s_chat.driver_name);
        }
        if (new_theme != NULL && s_chat.session_id[0] != '\0' && new_theme->set_session != NULL) {
          char shortbuf[16] = {0};
          strncpy(shortbuf, s_chat.session_id, 8);
          shortbuf[8] = '\0';
          new_theme->set_session(shortbuf);
        }
        /* 重新挂回 picker（用 caller 持有的 phrases 指针）—— 但在 settings 内部
         * 我们没有 caller phrases handle 了；picker_show 把指针存了 s_chat.picker_phrases
         * 然而那个 NULL 的可能性存在（picker_show 把它清空过了？没有，只有 hide 才清）。
         * 只要 chat 当前 active，picker_phrases 还指着 caller 的 static 数组。 */
        if (s_chat.picker_phrases != NULL && s_chat.picker_count > 1) {
          /* 把"包了 Settings 行"的 count 还原成 caller_count，再让 picker_show
           * 重新加 Settings。 */
          int caller_count = s_chat.picker_count - 1;
          const char* const* phrases = s_chat.picker_phrases;
          /* picker_show 会重置 mode=PICKER，下面提前 set 没必要。 */
          bb_ui_agent_chat_picker_show(phrases, caller_count);
        } else {
          s_chat.mode = BB_CHAT_MODE_PICKER;
        }
        return;
      }
      /* same theme：推进光标到 Back 行。 */
      s_chat.settings_sel = BB_SETTINGS_ROW_BACK;
      settings_apply_rows();
      break;
    }
    case BB_SETTINGS_ROW_BACK:
      settings_back_to_picker();
      break;
    case BB_SETTINGS_ROW_COUNT:
    default:
      break;
  }
}
