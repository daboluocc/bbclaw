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

static const char* TAG = "bb_agent_ui";

#define BB_CHAT_AGENT_TASK_STACK 8192
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
typedef struct {
  int active;                 /* show 之后置 1，hide 后清 0 */
  int sending;                /* agent 任务在跑期间置 1 */
  TaskHandle_t agent_task;
  lv_obj_t* parent;
  bb_agent_state_t state;
  int64_t turn_start_ms;
  int saw_text_in_turn;       /* 当前 turn 是否已收到首个 TEXT（控制 BUSY 切换） */
  /* 持久化 session/driver；adapter SESSION 帧到达时更新。Phase 4.2 还会接 NVS。 */
  char session_id[64];
  char driver_name[24];
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
  s_chat.active = 1;

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
  /* 在调用线程的视角下 session/driver 是稳定字符串，复制进任务参数。 */
  strncpy(args->session_id, s_chat.session_id, sizeof(args->session_id) - 1);
  strncpy(args->driver_name, s_chat.driver_name, sizeof(args->driver_name) - 1);

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
