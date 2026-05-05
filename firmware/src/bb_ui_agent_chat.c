#include "bb_ui_agent_chat.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "bb_adapter_client.h"
#include "bb_agent_client.h"
#include "bb_agent_theme.h"
#include "bb_audio.h"
#include "bb_config.h"
#include "bb_notification.h"
#include "bb_session_store.h"
#include "bb_time.h"
#include "bb_wifi.h"
#include "esp_err.h"
#include "esp_heap_caps.h"
#include "esp_log.h"
#include "esp_lvgl_port.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "freertos/task.h"
#include "lvgl.h"
#include "nvs.h"
#include "nvs_flash.h"

static const char* TAG = "bb_agent_ui";

/* Thread-safe wrapper around lv_async_call(). LVGL is configured with
 * LV_OS_NONE so lv_malloc/lv_timer_create inside lv_async_call have no
 * internal mutex. Without this wrapper, calling lv_async_call from worker
 * tasks races with lv_timer_handler on the LVGL task, corrupting the TLSF
 * heap and causing infinite-loop watchdog triggers.
 *
 * lvgl_port_lock uses a recursive mutex, so it's safe to call from both
 * the LVGL task (already holds the lock) and from worker tasks (blocks
 * until the LVGL task releases between timer handler iterations). */
static inline lv_result_t safe_lv_async_call(lv_async_cb_t cb, void* user_data) {
  if (!lvgl_port_lock(pdMS_TO_TICKS(200))) {
    ESP_LOGW(TAG, "safe_lv_async_call: lock timeout, dropping");
    return LV_RESULT_INVALID;
  }
  lv_result_t res = lv_async_call(cb, user_data);
  lvgl_port_unlock();
  return res;
}

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

/* Session picker (Phase S1) — full-screen overlay for multi-session switching. */
#define BB_SESSION_PICKER_MAX      8
#define BB_SESSION_PICKER_VISIBLE  6
#define BB_SESSION_PICKER_ROW_H    20

/* Phase S3 — history replay tunables.
 *  - PAGE_SIZE: how many messages we ask for in one round trip. 50 fits a
 *    typical session in two pages and keeps adapter response under ~50 KB.
 *  - MAX_LOADED: hard cap on labels in the LVGL flex column. Beyond this we
 *    stop chasing earlier history (the user is told via a one-shot toast in
 *    the topbar — for v1, just stop silently).
 */
#define BB_HISTORY_PAGE_SIZE   50
#define BB_HISTORY_MAX_LOADED  300

/* CWD pool picker (issue #30) — shown when creating a new session and the
 * adapter has more than one project configured. */
#define BB_CWD_PICKER_MAX      8
#define BB_CWD_PICKER_VISIBLE  5
#define BB_CWD_PICKER_ROW_H    20

/* Action codes returned by session_picker_select(). */
#define BB_SESSION_PICKER_ACTION_SWITCH       0
#define BB_SESSION_PICKER_ACTION_SETTINGS     1
#define BB_SESSION_PICKER_ACTION_NEW_SESSION  2
#define BB_SESSION_PICKER_ACTION_NONE        (-1)

/* NVS 配置（与 bb_agent_theme.c 同 namespace）。 */
#define BB_CHAT_NVS_NS         "bbclaw"
#define BB_CHAT_NVS_KEY_DRIVER "agent/driver"
#define BB_CHAT_NVS_KEY_TTS    "agent/tts"
#define BB_CHAT_DRIVER_FALLBACK "claude-code"

/* Phase 4.5.1 — TTS reply playback. The accumulator caps at 4 KiB; cloud
 * replies longer than that fall back to truncated TTS (we log a warn). */
#define BB_CHAT_REPLY_BUF_CAP    4096
/* Phase 4.5.2 streaming pipeline: the task persists across multiple
 * sentence-level synth + play cycles, with cJSON parsing + I2S playback
 * on the stack — observed overflow at 4 KB, so 8 KB with headroom.
 * Allocated from PSRAM via xTaskCreateWithCaps; internal heap largest
 * contiguous block drops to ~7.6 KB after PTT/voice loop, making
 * xTaskCreate(8192) unreliable (ESP_LOGW "xTaskCreate failed"). */
#define BB_CHAT_TTS_TASK_STACK   8192
#define BB_CHAT_TTS_TASK_PRIO    4

typedef enum {
  BB_CHAT_MODE_PICKER = 0,  /* 唯一模式：phrase 选择。Phase 4.7 后 Settings 已搬到独立 overlay。 */
} bb_chat_mode_t;

typedef struct {
  int active;                 /* show 之后置 1，hide 后清 0 */
  int sending;                /* agent 任务在跑期间置 1 */
  volatile int agent_cancel_requested;  /* Phase 4.9: user cancelled turn */
  TaskHandle_t agent_task;
  lv_obj_t* parent;
  bb_agent_state_t state;
  int64_t turn_start_ms;
  int saw_text_in_turn;       /* 当前 turn 是否已收到首个 TEXT（控制 BUSY 切换） */
  /* 持久化 session/driver；adapter SESSION 帧到达时更新。Phase 4.2 接入 NVS。 */
  char session_id[64];
  char driver_name[24];          /* 来自 SESSION 帧（adapter 真实分配的 driver） */

  /* picker 状态（Phase 4.2） */
  lv_obj_t* picker_root;
  lv_obj_t* picker_items[BB_CHAT_PICKER_MAX_ITEMS];
  const char* const* picker_phrases;  /* 调用方持有；不含 Settings 行 */
  int picker_count;            /* 包含首行 Settings；= caller_count + 1 */
  int picker_sel;
  /* 当前模式：picker 或 settings */
  bb_chat_mode_t mode;

  /* drivers 列表缓存（Phase 4.2.5 / Phase 5：仅供 cycle_driver 使用，
   * Phase 4.7 起 Settings 主菜单已经搬到独立 overlay 各自维护自己的 cache） */
  bb_agent_driver_info_t driver_cache[BB_CHAT_DRIVER_CACHE_MAX];
  int driver_cache_count;
  int driver_cache_offline;
  int driver_cache_idx;
  volatile int driver_fetch_pending;
  volatile uint32_t driver_fetch_generation;

  /* Session picker (Phase S1) — multi-session management. */
  bb_agent_session_info_t session_list[BB_SESSION_PICKER_MAX];
  int session_list_count;
  int session_picker_sel;              /* highlighted row (0-based, includes + New / Settings) */
  int session_picker_active_idx;       /* index of current session in list (-1 if not found) */
  int session_picker_visible;          /* 0=hidden, 1=loading, 2=visible */
  lv_obj_t* session_picker_root;
  lv_obj_t* session_picker_items[BB_SESSION_PICKER_MAX + 2]; /* + New + sessions + Settings */
  int session_picker_total_rows;       /* session_list_count + 1 (Settings) */
  volatile int session_fetch_pending;
  volatile uint32_t session_fetch_generation;

  /* Phase S3 — history replay state. Updated on the LVGL task only.
   *  history_total:        last-known total messages on adapter side
   *  history_min_seq:      smallest seq currently rendered in transcript;
   *                        -1 = nothing loaded yet. Used as `before` cursor
   *                        for the next "load earlier" fetch.
   *  history_loaded_count: number of history labels currently in the flex
   *                        column (excludes live user/assistant turn labels).
   *                        Capped at BB_HISTORY_MAX_LOADED.
   *  history_has_more:     server told us earlier messages exist beyond the
   *                        oldest loaded one.
   *  history_fetch_pending / generation: stale-fetch guard, identical to the
   *                        session_fetch_* pair above.
   */
  int history_total;
  int history_min_seq;
  int history_loaded_count;
  int history_has_more;
  volatile int history_fetch_pending;
  volatile uint32_t history_fetch_generation;

  /* CWD pool picker (issue #30) — shown when creating a new session and the
   * adapter has more than one project configured. Fetched once per session-
   * picker open; cached for the lifetime of the chat overlay. */
  bb_agent_cwd_entry_t cwd_pool[BB_CWD_PICKER_MAX];
  int cwd_pool_count;
  volatile int cwd_pool_fetch_pending;
  volatile uint32_t cwd_pool_fetch_generation;
  /* CWD picker UI state (only active when cwd_picker_visible > 0). */
  int cwd_picker_visible;   /* 0=hidden, 1=loading, 2=visible */
  int cwd_picker_sel;
  lv_obj_t* cwd_picker_root;
  lv_obj_t* cwd_picker_items[BB_CWD_PICKER_MAX];

  /* Phase 4.5.1 — TTS reply toggle + per-turn accumulator.
   * Phase 4.5.2 — sentence-level streaming + cancel-and-replace.
   *
   * Pipeline: reply_buf is the canonical accumulator, written by the agent task
   * as text chunks arrive. reply_synth_offset tracks how much of reply_buf has
   * already been handed to the TTS task. The TTS task drains reply_buf one
   * sentence at a time (or the whole tail at turn end), so the speaker starts
   * playing the first sentence while the agent is still streaming the rest.
   *
   * Concurrency: reply_buf and its companions are touched by the agent task
   * (writer) and the TTS task (reader). reply_len / reply_synth_offset /
   * reply_turn_complete are size_t / int — single-word reads are atomic on
   * ESP32-S3, and any race produces at worst a one-iteration lag, never a
   * crash. tts_cancel_requested is a soft-kill flag the TTS task polls.
   */
  int tts_enabled;             /* 1 = synth reply via adapter TTS; default ON */
  char* reply_buf;             /* heap, capacity BB_CHAT_REPLY_BUF_CAP */
  size_t reply_len;            /* bytes used (excl. terminating NUL) */
  size_t reply_synth_offset;   /* bytes already extracted for synthesis */
  int reply_truncated;         /* 1 once we've logged the truncation warn */
  volatile int reply_turn_complete; /* 1 once EvTurnEnd seen — TTS may flush tail */
  volatile int tts_cancel_requested; /* 1 to ask TTS task to abort + exit */
  TaskHandle_t tts_task;       /* non-NULL while a TTS playback task is alive */
} bb_chat_state_t;

static bb_chat_state_t s_chat = {0};

/* ── Chunk coalesce double-buffer (agent task -> LVGL task) ─────────────────
 *
 * Problem: every streaming token calls post_assistant_chunk() -> lv_async_call()
 * -> queues a new 0ms LVGL timer.  For an N-token response the lv_timer_handler
 * do-while loop restarts from the list head on every deletion, producing O(N^2)
 * traversals without yielding -> taskLVGL starves IDLE1 -> Task WDT fires.
 *
 * Fix: coalesce all tokens arriving between two LVGL dispatches into one
 * lv_async_call().  Double-buffer lets the LVGL drain happen without memcpy
 * inside the critical section (only a few integer assignments).
 *
 * Write side (agent task):  append to buf[widx], gate lv_async_call on flag.
 * Read side  (LVGL task):   swap widx, drain old buf, reset flag.
 */
#define BB_CHAT_CHUNK_COALESCE_CAP 1024  /* per-buffer cap (bytes, excl. NUL) */

static char         s_chunk_buf[2][BB_CHAT_CHUNK_COALESCE_CAP + 1];
static size_t       s_chunk_len[2]  = {0, 0};
static int          s_chunk_widx    = 0;   /* write-buffer index (0 or 1) */
static volatile int s_chunk_queued  = 0;   /* 1 = lv_async_call already pending */
static portMUX_TYPE s_chunk_mux     = portMUX_INITIALIZER_UNLOCKED;

/* Forward decls for Phase 4.5.1/4.5.2 TTS pipeline (defined further down). */
static void reply_buf_reset(void);
static void reply_buf_append(const char* delta);
static int  reply_buf_has_content(void);
static void tts_kick_or_spawn(void);
static void tts_cancel_in_flight(void);

/* Forward decls for Phase 4.2.5 / Phase 5 async driver fetch (used by cycle_driver). */
static void spawn_driver_fetch_task(void);
static void apply_driver_cache_idx(void);

/* Forward decls for Phase S3 history replay (defined alongside session-fetch). */
static void spawn_history_fetch_task(int before, int is_initial);
static void history_state_reset(void);

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
  /* High-frequency alloc (one per assistant text chunk during streaming).
   * Force PSRAM so a fast-talking turn doesn't bloat internal heap. */
  bb_async_payload_t* p = (bb_async_payload_t*)heap_caps_calloc(
      1, sizeof(*p), MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
  if (p != NULL) {
    p->kind = kind;
  }
  return p;
}

/* All async-payload string copies go to PSRAM. Internal heap is too tight
 * (~135 KB shared with WiFi/TCP/FreeRTOS) to absorb the per-turn churn —
 * after long uptime + several turns we'd see xTaskCreate failures because
 * internal heap was exhausted by transient duplicates. PSRAM has 8 MB. */
static char* dup_str(const char* s) {
  if (s == NULL) return NULL;
  size_t n = strlen(s);
  char* out = (char*)heap_caps_malloc(n + 1, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
  if (out == NULL) return NULL;
  memcpy(out, s, n + 1);
  return out;
}

/* 在 LVGL 任务里执行；结束 free payload。*/

/* Drains the coalesced chunk buffer. Runs in LVGL task via lv_async_call(). */
static void on_lvgl_chunk_dispatch(void* unused) {
  (void)unused;
  /* Atomically swap write buffer so agent task continues filling the other slot
   * while we drain this one.  Critical section is only integer assignments. */
  int drain_idx;
  size_t drain_len;
  taskENTER_CRITICAL(&s_chunk_mux);
  drain_idx                    = s_chunk_widx;
  drain_len                    = s_chunk_len[drain_idx];
  s_chunk_widx                 = drain_idx ^ 1;   /* agent writes here next */
  s_chunk_len[s_chunk_widx]    = 0;               /* reset new write slot */
  s_chunk_buf[s_chunk_widx][0] = '\0';
  s_chunk_queued               = 0;               /* allow next post to requeue */
  taskEXIT_CRITICAL(&s_chunk_mux);

  if (!s_chat.active || drain_len == 0) return;
  s_chunk_buf[drain_idx][drain_len] = '\0';       /* ensure NUL-termination */
  const bb_agent_theme_t* theme = bb_agent_theme_get_active();
  if (theme != NULL && theme->append_assistant_chunk != NULL) {
    theme->append_assistant_chunk(s_chunk_buf[drain_idx]);
  }
}

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
  safe_lv_async_call(on_lvgl_dispatch, p);
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
  /* Coalesce: append to the double-buffer and only queue one lv_async_call per
   * LVGL tick instead of one per streaming token.  Eliminates the O(N^2)
   * lv_timer_handler restart behavior that starves IDLE1 causing Task WDT. */
  if (delta == NULL) return;
  size_t dlen = strlen(delta);
  if (dlen == 0) return;

  int need_queue = 0;
  taskENTER_CRITICAL(&s_chunk_mux);
  size_t avail = BB_CHAT_CHUNK_COALESCE_CAP - s_chunk_len[s_chunk_widx];
  if (avail > 0) {
    size_t copy = dlen < avail ? dlen : avail;
    memcpy(s_chunk_buf[s_chunk_widx] + s_chunk_len[s_chunk_widx], delta, copy);
    s_chunk_len[s_chunk_widx] += copy;
    /* NUL-termination deferred to drain side for speed; buf is [CAP+1] */
  }
  if (!s_chunk_queued) {
    s_chunk_queued = 1;
    need_queue     = 1;
  }
  taskEXIT_CRITICAL(&s_chunk_mux);

  if (need_queue) {
    safe_lv_async_call(on_lvgl_chunk_dispatch, NULL);
  }
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

/* Build a short display string from a full session ID (e.g. "ls-234049fd34da990c").
 * Strips the common "ls-" prefix and takes the TAIL (most distinguishing part).
 * Result fits in a 16-char buffer: up to 12 hex chars shown. */
static void session_id_short(const char* full_sid, char* out, size_t out_sz) {
  if (full_sid == NULL || full_sid[0] == '\0') {
    snprintf(out, out_sz, "--------");
    return;
  }
  const char* hex = full_sid;
  /* Skip known prefixes: "ls-", "cs-", etc. */
  if (hex[0] != '\0' && hex[1] != '\0' && hex[2] == '-') {
    hex += 3;
  }
  size_t hlen = strlen(hex);
  /* Take the last 10 chars (or all if shorter) — tail has highest entropy. */
  const int show = 10;
  const char* tail = hlen > (size_t)show ? hex + hlen - show : hex;
  snprintf(out, out_sz, "%s", tail);
}

/* ── stream callback：在 agent 任务里同步调用 ── */

static void on_agent_event(const bb_agent_stream_event_t* evt, void* user_ctx) {
  (void)user_ctx;
  if (evt == NULL) return;
  /* Phase 4.9: user cancelled the turn — discard all remaining events.
   * The HTTP stream keeps running in agent_task until turn_end/error,
   * but UI and TTS are no longer updated. */
  if (s_chat.agent_cancel_requested) return;

  switch (evt->type) {
    case BB_AGENT_EVENT_SESSION:
      if (evt->session_id != NULL) {
        strncpy(s_chat.session_id, evt->session_id, sizeof(s_chat.session_id) - 1);
        s_chat.session_id[sizeof(s_chat.session_id) - 1] = '\0';
        char shortbuf[16] = {0};
        session_id_short(evt->session_id, shortbuf, sizeof(shortbuf));
        post_session(shortbuf);

        /* Phase S2 — save session ID to NVS for current driver */
        if (evt->driver != NULL && evt->driver[0] != '\0') {
          bb_session_store_save(evt->driver, evt->session_id);
        } else if (s_chat.driver_name[0] != '\0') {
          bb_session_store_save(s_chat.driver_name, evt->session_id);
        }
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
        /* Phase 4.5.1 — accumulate plain assistant text for end-of-turn TTS.
         * Append happens on the agent task thread; reply_buf is single-writer. */
        reply_buf_append(evt->text);
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
      /* Phase 4.5.1 — speak the assistant reply if the toggle is ON.
       * Phase 4.5.2 — set turn_complete so the streaming task will flush the
       * tail (any text after the last sentence boundary) and then exit. The
       * task may already be running mid-stream; if not, kick spawns it now.
       * Phase 4.8.x — only post the final IDLE/HEART here when we know TTS
       * will NOT play (toggle off OR no spoken content). When TTS will play,
       * the streaming task posts SPEAKING on entry and IDLE/HEART on exit,
       * so posting IDLE here would just cause a brief flicker. */
      s_chat.reply_turn_complete = 1;
      const int will_speak = s_chat.tts_enabled && reply_buf_has_content();
      if (will_speak) {
        tts_kick_or_spawn();  /* TTS task will handle final state. */
      } else {
        int64_t now = bb_now_ms();
        int64_t elapsed = now - s_chat.turn_start_ms;
        if (s_chat.turn_start_ms > 0 && elapsed >= 0 && elapsed < BB_CHAT_HEART_THRESHOLD_MS) {
          post_state(BB_AGENT_STATE_HEART);
        } else {
          post_state(BB_AGENT_STATE_IDLE);
        }
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
  /* Phase 4.5.2 — cancel-and-replace: if a previous turn's TTS is still
   * playing, kill it before resetting the buffer (otherwise the streaming
   * task would continue reading reply_buf as we wipe it from under it). */
  tts_cancel_in_flight();
  /* Phase 4.5.1 — fresh per-turn accumulator; reused buffer if already alloc. */
  reply_buf_reset();

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

  if (err != ESP_OK && !s_chat.agent_cancel_requested) {
    ESP_LOGW(TAG, "agent send failed: %s", esp_err_to_name(err));
    /* 没有 ERROR 帧的失败（比如 HTTP 层）：手动派发一次错误 + DIZZY。 */
    if (s_chat.state != BB_AGENT_STATE_DIZZY) {
      post_state(BB_AGENT_STATE_DIZZY);
      post_error(esp_err_to_name(err));
    }
  }
  if (s_chat.agent_cancel_requested) {
    ESP_LOGI(TAG, "agent_task: turn was cancelled, discarding result");
  }

  free(args->text);
  free(args);

  s_chat.agent_cancel_requested = 0;
  s_chat.sending = 0;
  s_chat.agent_task = NULL;
  vTaskDelete(NULL);
}

/* ── public API ── */

/* Phase 4.9: NVS reads must run on a task whose stack lives in internal
 * RAM.  stream_task's stack is PSRAM-backed; during SPI-flash reads
 * (required by NVS) the cache is disabled, making PSRAM inaccessible
 * and triggering the esp_task_stack_is_sane_cache_disabled assert.
 * Solution: spawn a one-shot task on internal heap, wait for it.
 * Forward-declare load_tts_enabled_from_nvs (defined below). */
static void load_tts_enabled_from_nvs(void);

static void load_nvs_task(void* arg) {
  load_tts_enabled_from_nvs();
  /* Load session ID for current driver from NVS on internal RAM stack */
  const char* driver = bb_ui_agent_chat_get_current_driver();
  if (driver != NULL && driver[0] != '\0') {
    esp_err_t err = bb_session_store_load(driver, s_chat.session_id, sizeof(s_chat.session_id));
    if (err == ESP_OK) {
      ESP_LOGI(TAG, "loaded session '%s' for driver '%s'", s_chat.session_id, driver);
    } else if (err != ESP_ERR_NVS_NOT_FOUND) {
      ESP_LOGW(TAG, "session load failed: %s", esp_err_to_name(err));
    }
  }
  SemaphoreHandle_t sem = (SemaphoreHandle_t)arg;
  xSemaphoreGive(sem);
  vTaskDelete(NULL);
}

static void load_nvs_on_internal_stack(void) {
  SemaphoreHandle_t sem = xSemaphoreCreateBinary();
  if (sem == NULL) {
    ESP_LOGW(TAG, "load_nvs: semaphore alloc failed, skipping");
    return;
  }
  BaseType_t ok = xTaskCreate(load_nvs_task, "load_nvs", 4096, sem, 5, NULL);
  if (ok != pdPASS) {
    ESP_LOGW(TAG, "load_nvs: task create failed, skipping");
    vSemaphoreDelete(sem);
    return;
  }
  /* 2 s is generous — NVS read is typically < 1 ms. */
  xSemaphoreTake(sem, pdMS_TO_TICKS(2000));
  vSemaphoreDelete(sem);
}

/* ── Phase 4.5.1 — TTS reply toggle persistence ── */

/* Load NVS string at "agent/tts"; missing or unparseable → default ON. */
static void load_tts_enabled_from_nvs(void) {
  s_chat.tts_enabled = 1;
  nvs_handle_t h;
  esp_err_t err = nvs_open(BB_CHAT_NVS_NS, NVS_READONLY, &h);
  if (err != ESP_OK) {
    ESP_LOGI(TAG, "tts_enabled: no nvs (%s), default ON", esp_err_to_name(err));
    return;
  }
  char buf[8] = {0};
  size_t sz = sizeof(buf);
  err = nvs_get_str(h, BB_CHAT_NVS_KEY_TTS, buf, &sz);
  nvs_close(h);
  if (err != ESP_OK) {
    ESP_LOGI(TAG, "tts_enabled: not in nvs (%s), default ON", esp_err_to_name(err));
    return;
  }
  if (strcmp(buf, "off") == 0) {
    s_chat.tts_enabled = 0;
  } else {
    s_chat.tts_enabled = 1;
  }
  ESP_LOGI(TAG, "tts_enabled: loaded '%s'", buf);
}

/* ── Phase 4.5.1/4.5.2 — reply accumulator (called from agent_task only) ── */

/* UTF-8 lead-byte test: 0xxxxxxx (ASCII) or 11xxxxxx (multi-byte start).
 * Continuation bytes are 10xxxxxx and never lead. Used by the truncation
 * helper to avoid splitting a multi-byte codepoint when we hit the cap. */
static inline int is_utf8_lead(unsigned char c) {
  return (c & 0xC0U) != 0x80U;
}

/* Walk back at most 3 bytes from `len` to find a UTF-8 lead boundary.
 * Returns a length <= the input that ends on a clean codepoint boundary.
 * If no lead is found in the window, falls back to the input length
 * (safer to return slightly malformed than to lose data). */
static size_t utf8_safe_truncate(const char* buf, size_t len) {
  if (buf == NULL || len == 0U) return 0;
  size_t lookback = len < 4U ? len : 4U;
  for (size_t i = 0; i < lookback; ++i) {
    if (is_utf8_lead((unsigned char)buf[len - 1U - i])) {
      /* If this lead byte starts a multi-byte char, only keep it if the full
       * char fits inside our window. UTF-8 char length from lead byte:
       *   0xxxxxxx -> 1, 110xxxxx -> 2, 1110xxxx -> 3, 11110xxx -> 4. */
      unsigned char lead = (unsigned char)buf[len - 1U - i];
      size_t char_len;
      if ((lead & 0x80U) == 0U)        char_len = 1;
      else if ((lead & 0xE0U) == 0xC0U) char_len = 2;
      else if ((lead & 0xF0U) == 0xE0U) char_len = 3;
      else if ((lead & 0xF8U) == 0xF0U) char_len = 4;
      else                              char_len = 1;  /* malformed lead */
      size_t end = (len - 1U - i) + char_len;
      return end <= len ? end : (len - 1U - i);
    }
  }
  return len;
}

/* Reset (or lazily allocate) the reply buffer. Called at turn start. */
static void reply_buf_reset(void) {
  if (s_chat.reply_buf == NULL) {
    s_chat.reply_buf = (char*)malloc(BB_CHAT_REPLY_BUF_CAP);
    if (s_chat.reply_buf == NULL) {
      ESP_LOGW(TAG, "reply_buf: malloc failed; TTS will be skipped this turn");
      s_chat.reply_len = 0;
      s_chat.reply_synth_offset = 0;
      return;
    }
  }
  s_chat.reply_buf[0] = '\0';
  s_chat.reply_len = 0;
  s_chat.reply_synth_offset = 0;
  s_chat.reply_truncated = 0;
  s_chat.reply_turn_complete = 0;
  s_chat.tts_cancel_requested = 0;
}

/* Append a delta; truncates at cap (UTF-8-safe) and warns once. After
 * appending, kicks the TTS task if the new bytes contain a sentence boundary
 * — that's how Phase 4.5.2 starts speaking before turn-end. */
static void reply_buf_append(const char* delta) {
  if (delta == NULL || s_chat.reply_buf == NULL) return;
  size_t add = strlen(delta);
  if (add == 0U) return;
  size_t room = BB_CHAT_REPLY_BUF_CAP - 1U - s_chat.reply_len;
  if (room == 0U) {
    if (!s_chat.reply_truncated) {
      ESP_LOGW(TAG, "reply_buf: cap %d reached, TTS will use truncated text", BB_CHAT_REPLY_BUF_CAP);
      s_chat.reply_truncated = 1;
    }
    return;
  }
  size_t copy = add < room ? add : room;
  /* When we have to truncate, walk back to a UTF-8 char boundary so we don't
   * leave a torn multi-byte char dangling at the end (causes box glyphs in
   * transcript and garbled audio in TTS). */
  if (copy < add) {
    copy = utf8_safe_truncate(delta, copy);
    if (copy == 0U) {
      /* Nothing fits cleanly — drop this chunk's tail and warn. */
      if (!s_chat.reply_truncated) {
        ESP_LOGW(TAG, "reply_buf: cap %d reached at multi-byte boundary",
                 BB_CHAT_REPLY_BUF_CAP);
        s_chat.reply_truncated = 1;
      }
      return;
    }
  }
  size_t before = s_chat.reply_len;
  memcpy(s_chat.reply_buf + before, delta, copy);
  s_chat.reply_len = before + copy;
  s_chat.reply_buf[s_chat.reply_len] = '\0';
  if (copy < add && !s_chat.reply_truncated) {
    ESP_LOGW(TAG, "reply_buf: cap %d reached, TTS will use truncated text", BB_CHAT_REPLY_BUF_CAP);
    s_chat.reply_truncated = 1;
  }

  /* Phase 4.5.2 — if the new bytes crossed a sentence boundary, kick TTS so
   * it starts speaking the first sentence right away instead of waiting for
   * EvTurnEnd. ASCII boundary chars are sufficient — Chinese 。！？ are
   * multi-byte but a typical assistant reply has plenty of ASCII boundaries. */
  if (s_chat.tts_enabled) {
    for (size_t i = before; i < s_chat.reply_len; ++i) {
      char c = s_chat.reply_buf[i];
      if (c == '.' || c == '!' || c == '?' || c == '\n') {
        tts_kick_or_spawn();
        break;
      }
    }
  }
}

/* True if the reply buffer is non-empty and contains at least one
 * non-whitespace byte. ASCII-only check is sufficient (CJK bytes are all
 * >0x7F so they pass). */
static int reply_buf_has_content(void) {
  if (s_chat.reply_buf == NULL || s_chat.reply_len == 0U) return 0;
  for (size_t i = 0; i < s_chat.reply_len; ++i) {
    unsigned char c = (unsigned char)s_chat.reply_buf[i];
    if (c > 0x20U) return 1;
  }
  return 0;
}

/* Phase 4.5.2 — extract the next chunk for synthesis.
 *
 * Modes:
 *   - flush_tail=0: scan forward from reply_synth_offset for a sentence
 *     boundary (. ! ? \n). If found, return [synth_offset .. boundary+1)
 *     and advance synth_offset. If no boundary, return NULL.
 *   - flush_tail=1: return everything remaining [synth_offset .. reply_len),
 *     advance synth_offset to reply_len. Used when EvTurnEnd has fired.
 *
 * Returns a heap-allocated NUL-terminated string (caller frees), or NULL if
 * nothing to emit. Skips emitting if the chunk is whitespace-only.
 */
static char* extract_pending_chunk(int flush_tail) {
  if (s_chat.reply_buf == NULL) return NULL;
  size_t start = s_chat.reply_synth_offset;
  size_t end = start;
  if (flush_tail) {
    end = s_chat.reply_len;
  } else {
    for (size_t i = start; i < s_chat.reply_len; ++i) {
      char c = s_chat.reply_buf[i];
      if (c == '.' || c == '!' || c == '?' || c == '\n') {
        end = i + 1;
        break;
      }
    }
    if (end == start) return NULL;  /* no boundary found */
  }
  if (end <= start) return NULL;
  size_t n = end - start;
  /* Skip whitespace-only chunks (saves a pointless TTS round-trip). */
  int has_content = 0;
  for (size_t i = start; i < end; ++i) {
    if ((unsigned char)s_chat.reply_buf[i] > 0x20U) {
      has_content = 1;
      break;
    }
  }
  if (!has_content) {
    s_chat.reply_synth_offset = end;
    return NULL;
  }
  char* out = (char*)malloc(n + 1);
  if (out == NULL) {
    ESP_LOGW(TAG, "extract_pending_chunk: OOM (%u bytes)", (unsigned)n);
    return NULL;
  }
  memcpy(out, s_chat.reply_buf + start, n);
  out[n] = '\0';
  s_chat.reply_synth_offset = end;
  return out;
}

/* ── Phase 4.5.2 — sentence-level streaming TTS playback task ──
 *
 * Lifecycle (single long-running task per turn):
 *   - Spawned by tts_kick_or_spawn() the first time a sentence boundary
 *     is detected mid-stream (or at EvTurnEnd if no boundaries appeared).
 *   - Loop: extract next sentence from reply_buf, synth, play, repeat.
 *   - When extract returns NULL: if reply_turn_complete is set we're done
 *     (no more text coming); otherwise sleep briefly waiting for more chunks.
 *   - tts_cancel_requested aborts both the wait and the in-progress playback
 *     (via bb_audio_request_playback_interrupt).
 *   - On exit: clears s_chat.tts_task, self-deletes.
 *
 * Hide / cancel-and-replace: callers set tts_cancel_requested + interrupt
 * audio. The task drains current playback (if any), then exits next loop iter.
 *
 * Re-entrance after exit: agent_task at new turn calls tts_cancel_in_flight()
 * (waits for old task to die) before reply_buf_reset, then a fresh task is
 * spawned when the new turn's first sentence arrives. */

/* Returns 1 if we should bail right now (overlay closed or cancel requested). */
static int tts_should_abort(void) {
  return (!s_chat.active || s_chat.tts_cancel_requested) ? 1 : 0;
}

/* Synth + play a single chunk. Returns ESP_OK on a clean play, otherwise the
 * caller should treat as "skip and continue" (typically an interrupt or
 * synth failure — neither is fatal to the loop). */
static esp_err_t tts_synth_and_play(const char* text) {
  if (text == NULL || text[0] == '\0') return ESP_OK;
  ESP_LOGI(TAG, "tts: synth start len=%u", (unsigned)strlen(text));
  bb_tts_audio_t audio = {0};
  esp_err_t syn = bb_adapter_tts_synthesize_pcm16(text, &audio);
  if (syn != ESP_OK || audio.pcm_data == NULL || audio.pcm_len == 0U) {
    ESP_LOGW(TAG, "tts: synth failed err=%s pcm=%p len=%u", esp_err_to_name(syn),
             (void*)audio.pcm_data, (unsigned)audio.pcm_len);
    bb_adapter_tts_audio_free(&audio);
    return syn != ESP_OK ? syn : ESP_FAIL;
  }
  if (tts_should_abort()) {
    ESP_LOGI(TAG, "tts: aborted before playback (active=%d cancel=%d)",
             s_chat.active ? 1 : 0, s_chat.tts_cancel_requested ? 1 : 0);
    bb_adapter_tts_audio_free(&audio);
    return ESP_ERR_INVALID_STATE;
  }
  esp_err_t tx = bb_audio_start_playback();
  if (tx != ESP_OK) {
    ESP_LOGW(TAG, "tts: start_playback failed err=%s", esp_err_to_name(tx));
    bb_adapter_tts_audio_free(&audio);
    return tx;
  }
  if (audio.sample_rate > 0 && audio.sample_rate != BBCLAW_AUDIO_SAMPLE_RATE) {
    (void)bb_audio_set_playback_sample_rate(audio.sample_rate);
  }
  ESP_LOGI(TAG, "tts: play pcm_bytes=%u rate=%d ch=%d", (unsigned)audio.pcm_len,
           audio.sample_rate, audio.channels);
  esp_err_t play_err = bb_audio_play_pcm_blocking(audio.pcm_data, audio.pcm_len);
  if (play_err != ESP_OK) {
    ESP_LOGW(TAG, "tts: play_pcm failed (likely interrupted)");
  }
  (void)bb_audio_stop_playback();
  (void)bb_audio_set_playback_sample_rate(BBCLAW_AUDIO_SAMPLE_RATE);
  bb_audio_clear_playback_interrupt();
  bb_adapter_tts_audio_free(&audio);
  return play_err;
}

static void tts_playback_task(void* arg) {
  (void)arg;
  ESP_LOGI(TAG, "tts: streaming task start → SPEAKING");
  /* Phase 4.8.x: dedicated SPEAKING state while the audio is being
   * synthesized + played. Buddy shows "(^o^)~ speaking..." through the
   * whole reply playback (overrides any IDLE/HEART that EvTurnEnd might
   * have posted milliseconds earlier). */
  post_state(BB_AGENT_STATE_SPEAKING);

  /* Outer loop: drain reply_buf one sentence at a time. */
  while (!tts_should_abort()) {
    int turn_done = s_chat.reply_turn_complete ? 1 : 0;
    char* chunk = extract_pending_chunk(turn_done);
    if (chunk == NULL) {
      if (turn_done) {
        /* No more text and turn ended → clean exit. */
        break;
      }
      /* No sentence boundary yet — wait for more chunks. Light poll: 80 ms
       * is short enough that the user-perceived gap between sentences stays
       * tiny while still avoiding a busy loop. */
      vTaskDelay(pdMS_TO_TICKS(80));
      continue;
    }
    esp_err_t err = tts_synth_and_play(chunk);
    free(chunk);
    if (err == ESP_ERR_INVALID_STATE) {
      /* Aborted mid-flight. Don't try the next sentence. */
      break;
    }
    /* ESP_FAIL / synth-fail: drop this sentence, keep going (next sentence
     * may still be useful). */
  }
  /* Phase 4.8.x: TTS done — transition out of SPEAKING. Pick HEART if the
   * end-to-end turn was fast (< BB_CHAT_HEART_THRESHOLD_MS), otherwise
   * IDLE. We can't tell from inside the TTS task whether EvTurnEnd already
   * landed (it definitely did, that's how reply_turn_complete became 1),
   * so just compute from turn_start_ms. */
  {
    int64_t now = bb_now_ms();
    int64_t elapsed = (s_chat.turn_start_ms > 0) ? (now - s_chat.turn_start_ms) : -1;
    if (elapsed >= 0 && elapsed < BB_CHAT_HEART_THRESHOLD_MS) {
      post_state(BB_AGENT_STATE_HEART);
    } else {
      post_state(BB_AGENT_STATE_IDLE);
    }
  }
  ESP_LOGI(TAG, "tts: streaming task done (cancel=%d turn=%d) → IDLE/HEART",
           s_chat.tts_cancel_requested ? 1 : 0,
           s_chat.reply_turn_complete ? 1 : 0);
  s_chat.tts_task = NULL;
  vTaskDelete(NULL);
}

/* Idempotent kick: if the streaming task is already running, do nothing
 * (its loop will pick up the new chunk on the next iteration). Otherwise
 * spawn it. Called from reply_buf_append (mid-stream sentence boundary)
 * and from EvTurnEnd (flush tail). */
static void tts_kick_or_spawn(void) {
  if (!s_chat.tts_enabled) return;
  if (s_chat.tts_task != NULL) return;
  if (!reply_buf_has_content()) return;

  TaskHandle_t handle = NULL;
  BaseType_t ok = xTaskCreateWithCaps(tts_playback_task, "bb_chat_tts", BB_CHAT_TTS_TASK_STACK,
                                     NULL, BB_CHAT_TTS_TASK_PRIO, &handle,
                                     BBCLAW_MALLOC_CAP_PREFER_PSRAM);
  if (ok != pdPASS) {
    ESP_LOGW(TAG, "tts: xTaskCreateWithCaps failed (stack=%u), skip",
             (unsigned)BB_CHAT_TTS_TASK_STACK);
    return;
  }
  s_chat.tts_task = handle;
}

/* Cancel-and-replace: ask the streaming task to abort + interrupt audio, then
 * wait briefly for it to clean up. Used when a new user turn arrives mid-
 * playback so the old reply doesn't keep speaking over the new question. */
static void tts_cancel_in_flight(void) {
  if (s_chat.tts_task == NULL) return;
  ESP_LOGI(TAG, "tts: cancel-and-replace requested");
  s_chat.tts_cancel_requested = 1;
  bb_audio_request_playback_interrupt();
  /* Wait up to ~500 ms for the task to self-delete. If it doesn't, something
   * is stuck deep in synth — accept the leak rather than block agent_task
   * forever. The cancel flag stays set; the old task will eventually exit. */
  for (int i = 0; i < 50 && s_chat.tts_task != NULL; ++i) {
    vTaskDelay(pdMS_TO_TICKS(10));
  }
  if (s_chat.tts_task != NULL) {
    ESP_LOGW(TAG, "tts: cancel timeout, in-flight task still alive (will exit lazily)");
  }
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
  /* Load NVS data (tts_enabled, session_id) on internal RAM stack. */
  load_nvs_on_internal_stack();

  /* Phase 4.2.5 — pre-warm the driver list off-LVGL so Settings entry / LEFT-
   * RIGHT cycle don't have to wait for HTTP. Cheap if cache already populated
   * from an earlier session. */
  spawn_driver_fetch_task();

  bb_notification_init();

  ESP_LOGI(TAG, "show: theme=%s", theme->name != NULL ? theme->name : "(unnamed)");
  if (theme->on_enter != NULL) theme->on_enter(parent);
  if (theme->set_state != NULL) theme->set_state(BB_AGENT_STATE_SLEEP);
  /* Initialize s_chat.driver_name with the fallback so downstream callers
   * (session_picker_select / spawn_history_fetch_task / session_store_save)
   * always see a non-empty driver. The first SESSION frame from adapter
   * will overwrite this with the real driver name. Without this, picking a
   * session from the picker before any agent turn happens triggers the
   * "save: unknown driver ''" warning in session_store. */
  if (s_chat.driver_name[0] == '\0') {
    strncpy(s_chat.driver_name, BB_CHAT_DRIVER_FALLBACK, sizeof(s_chat.driver_name) - 1);
    s_chat.driver_name[sizeof(s_chat.driver_name) - 1] = '\0';
  }
  /* Show the effective driver in the topbar immediately on chat entry.
   * The SESSION frame that lands later will overwrite via post_driver. */
  if (theme->set_driver != NULL) {
    theme->set_driver(s_chat.driver_name);
  }
  if (s_chat.session_id[0] != '\0' && theme->set_session != NULL) {
    char shortbuf[16] = {0};
    session_id_short(s_chat.session_id, shortbuf, sizeof(shortbuf));
    theme->set_session(shortbuf);
  }
  /* Phase S3 — if we resumed a session from NVS, fetch its history so the
   * transcript isn't an unsettling blank. Skipped when session_id is empty
   * (fresh boot with no prior session) since there's nothing to load. */
  if (s_chat.session_id[0] != '\0') {
    history_state_reset();
    spawn_history_fetch_task(-1, /*is_initial=*/1);
  }
}

void bb_ui_agent_chat_hide(void) {
  if (!s_chat.active) {
    return;
  }
  /* 把 active 先清，让任何 in-flight 的 async dispatch 直接丢弃。
   * 在跑的 agent task 不强 kill；它结束后 self-delete。 */
  s_chat.active = 0;
  /* Phase 4.2.5 — bump the generation so any in-flight driver_fetch_task's
   * lv_async_call payload arrives stale and gets dropped in
   * on_driver_fetch_done. The task itself self-deletes; we just refuse its
   * result. */
  s_chat.driver_fetch_generation++;
  /* Phase S3 — same trick for any in-flight history fetch. */
  s_chat.history_fetch_generation++;
  /* Phase 4.5.1/4.5.2 — if a TTS streaming task is still running, mark it
   * cancelled and break out of any blocking playback. The task's loop sees
   * the cancel flag (or the active=0 we already set), exits cleanly, and
   * nulls s_chat.tts_task. */
  if (s_chat.tts_task != NULL) {
    ESP_LOGI(TAG, "hide: interrupting in-flight TTS playback");
    s_chat.tts_cancel_requested = 1;
    bb_audio_request_playback_interrupt();
  }
  /* Free the per-overlay reply accumulator. The TTS task already owns its
   * own snapshot at this point, so this is safe. */
  if (s_chat.reply_buf != NULL) {
    free(s_chat.reply_buf);
    s_chat.reply_buf = NULL;
    s_chat.reply_len = 0;
    s_chat.reply_truncated = 0;
  }
  /* picker / settings 都是 parent 的子；caller 会在 hide 后 delete parent，
   * 这里只是清指针避免悬挂。 */
  s_chat.picker_root = NULL;
  for (int i = 0; i < BB_CHAT_PICKER_MAX_ITEMS; ++i) {
    s_chat.picker_items[i] = NULL;
  }
  s_chat.picker_phrases = NULL;
  s_chat.picker_count = 0;
  s_chat.picker_sel = 0;
  s_chat.mode = BB_CHAT_MODE_PICKER;

  /* CWD picker cleanup (issue #30). */
  if (s_chat.cwd_picker_root != NULL) {
    lv_obj_del(s_chat.cwd_picker_root);
    s_chat.cwd_picker_root = NULL;
    memset(s_chat.cwd_picker_items, 0, sizeof(s_chat.cwd_picker_items));
  }
  s_chat.cwd_picker_visible = 0;
  /* Invalidate the pool cache so it's re-fetched on next chat entry. */
  s_chat.cwd_pool_count = 0;
  s_chat.cwd_pool_fetch_generation++;

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

  bb_agent_task_args_t* args = (bb_agent_task_args_t*)heap_caps_calloc(
      1, sizeof(*args), MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
  if (args == NULL) return ESP_ERR_NO_MEM;
  args->text = dup_str(text);
  if (args->text == NULL) {
    free(args);
    return ESP_ERR_NO_MEM;
  }
  /* Load NVS data (tts_enabled, session_id) on internal RAM stack. */
  load_nvs_on_internal_stack();
  strncpy(args->session_id, s_chat.session_id, sizeof(args->session_id) - 1);
  strncpy(args->driver_name, s_chat.driver_name, sizeof(args->driver_name) - 1);
  ESP_LOGI(TAG, "send: text='%.40s' driver=%s", text,
           args->driver_name[0] != '\0' ? args->driver_name : "(default)");

  /* 立刻在 transcript 里渲染用户那行（不等回包）。这步是 LVGL 操作，
   * 但 send() 既可能在 LVGL 任务也可能在按键任务里被调；统一走 async。 */
  post_user(text);

  s_chat.sending = 1;
  s_chat.turn_start_ms = bb_now_ms();
  s_chat.saw_text_in_turn = 0;

  BaseType_t ok = xTaskCreateWithCaps(agent_task, "bb_agent_task", BB_CHAT_AGENT_TASK_STACK, args,
                                      BB_CHAT_AGENT_TASK_PRIO, &s_chat.agent_task,
                                      BBCLAW_MALLOC_CAP_PREFER_PSRAM);
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "send: xTaskCreate failed stack=%u free_int=%u free_psram=%u",
             (unsigned)BB_CHAT_AGENT_TASK_STACK,
             (unsigned)heap_caps_get_free_size(MALLOC_CAP_INTERNAL),
             (unsigned)heap_caps_get_free_size(MALLOC_CAP_SPIRAM));
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

void bb_ui_agent_chat_picker_show(const char* const* phrases, int count) {
  if (!s_chat.active || s_chat.parent == NULL) {
    ESP_LOGW(TAG, "picker_show: chat not active");
    return;
  }
  if (phrases == NULL || count <= 0) return;
  /* Phase 4.7: picker is just phrases now — Settings moved to standalone overlay. */
  if (count > BB_CHAT_PICKER_MAX_ITEMS) count = BB_CHAT_PICKER_MAX_ITEMS;
  const int total = count;

  /* Replace any prior picker. */
  if (s_chat.picker_root != NULL) {
    lv_obj_del(s_chat.picker_root);
    s_chat.picker_root = NULL;
    for (int i = 0; i < BB_CHAT_PICKER_MAX_ITEMS; ++i) s_chat.picker_items[i] = NULL;
  }

  s_chat.picker_phrases = phrases;
  s_chat.picker_count = total;
  s_chat.picker_sel = 0;
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
    const char* p = phrases[i];
    lv_label_set_text(row, p != NULL ? p : "");
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

esp_err_t bb_ui_agent_chat_picker_send_selected(void) {
  if (!s_chat.active || s_chat.picker_root == NULL || s_chat.picker_count == 0) {
    return ESP_ERR_INVALID_STATE;
  }
  if (s_chat.picker_sel < 0 || s_chat.picker_sel >= s_chat.picker_count) {
    return ESP_ERR_INVALID_STATE;
  }
  if (s_chat.picker_phrases == NULL) return ESP_ERR_INVALID_STATE;
  const char* phrase = s_chat.picker_phrases[s_chat.picker_sel];
  if (phrase == NULL || phrase[0] == '\0') return ESP_ERR_INVALID_ARG;
  return bb_ui_agent_chat_send(phrase);
}

/* ── Phase 4.2.5 / Phase 5 async driver fetch (used by cycle_driver) ──
 *
 * The driver list is populated off-LVGL via a short-lived FreeRTOS task.
 * Result is posted back via lv_async_call. Generation counter lets us drop
 * stale results when chat hides between fetch start and finish.
 *
 * The Settings overlay (Phase 4.7) maintains its own independent cache —
 * see bb_ui_settings.c. Two caches is wasteful but acceptable; cycle_driver
 * (chat) and the Agent row (settings) are mutually exclusive in practice.
 */
typedef struct {
  uint32_t gen;
  esp_err_t err;
  int total;
  bb_agent_driver_info_t entries[BB_CHAT_DRIVER_CACHE_MAX];
} driver_fetch_result_t;

#define BB_CHAT_DRIVER_FETCH_TASK_STACK 4096
#define BB_CHAT_DRIVER_FETCH_TASK_PRIO  4

static void apply_driver_cache_idx(void) {
  int idx = 0;
  const char* prefer = s_chat.driver_name[0] != '\0' ? s_chat.driver_name : NULL;
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

static void on_driver_fetch_done(void* user_data) {
  driver_fetch_result_t* res = (driver_fetch_result_t*)user_data;
  if (res == NULL) return;
  s_chat.driver_fetch_pending = 0;
  if (!s_chat.active || res->gen != s_chat.driver_fetch_generation) {
    free(res);
    return;
  }
  if (res->err != ESP_OK || res->total <= 0) {
    ESP_LOGW(TAG, "driver fetch failed (%s), fallback to '%s'",
             esp_err_to_name(res->err), BB_CHAT_DRIVER_FALLBACK);
    memset(&s_chat.driver_cache[0], 0, sizeof(s_chat.driver_cache[0]));
    strncpy(s_chat.driver_cache[0].name, BB_CHAT_DRIVER_FALLBACK,
            sizeof(s_chat.driver_cache[0].name) - 1);
    s_chat.driver_cache_count = 1;
    s_chat.driver_cache_offline = 1;
    const bb_agent_theme_t* theme = bb_agent_theme_get_active();
    if (theme != NULL) {
      if (theme->set_driver != NULL) theme->set_driver("OFFLINE");
      if (theme->append_error != NULL) theme->append_error("adapter offline");
    }
  } else {
    int total = res->total > BB_CHAT_DRIVER_CACHE_MAX
                  ? BB_CHAT_DRIVER_CACHE_MAX : res->total;
    memcpy(s_chat.driver_cache, res->entries, sizeof(res->entries[0]) * (size_t)total);
    s_chat.driver_cache_count = total;
    s_chat.driver_cache_offline = 0;
  }
  apply_driver_cache_idx();
  free(res);
}

static void driver_fetch_task(void* arg) {
  uint32_t my_gen = (uint32_t)(uintptr_t)arg;
  driver_fetch_result_t* res = (driver_fetch_result_t*)calloc(1, sizeof(*res));
  if (res == NULL) {
    s_chat.driver_fetch_pending = 0;
    vTaskDelete(NULL);
    return;
  }
  res->gen = my_gen;
  /* Wait for WiFi before making HTTP calls — avoids lwIP assert crash
   * when CHAT enters before network is ready. 60 s upper bound so a
   * stale saved-SSID retries + compile-time fallback (~15 s on observed
   * boots) finish before we even attempt the request. */
  for (int i = 0; i < 300 && !bb_wifi_is_connected(); ++i) {
    vTaskDelay(pdMS_TO_TICKS(200));
  }
  /* Up to 3 attempts so a single transient failure (DNS race right after
   * fallback, cloud 5xx, adapter still booting) does not lock the chat
   * UI into permanent "OFFLINE" / "! adapter offline" state. */
  for (int attempt = 0; attempt < 3; ++attempt) {
    res->err = bb_agent_list_drivers(res->entries, BB_CHAT_DRIVER_CACHE_MAX, &res->total);
    if (res->err == ESP_OK && res->total > 0) break;
    if (attempt < 2) vTaskDelay(pdMS_TO_TICKS(3000));
  }
  safe_lv_async_call(on_driver_fetch_done, res);
  vTaskDelete(NULL);
}

static void spawn_driver_fetch_task(void) {
  if (s_chat.driver_cache_count > 0 && !s_chat.driver_cache_offline) return;
  if (s_chat.driver_fetch_pending) return;
  s_chat.driver_fetch_pending = 1;
  uint32_t gen = ++s_chat.driver_fetch_generation;
  TaskHandle_t t = NULL;
  BaseType_t ok = xTaskCreate(driver_fetch_task, "drv_fetch",
                              BB_CHAT_DRIVER_FETCH_TASK_STACK,
                              (void*)(uintptr_t)gen,
                              BB_CHAT_DRIVER_FETCH_TASK_PRIO, &t);
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "spawn_driver_fetch_task: xTaskCreate failed");
    s_chat.driver_fetch_pending = 0;
  }
}

/* ── Phase S1 — session picker (multi-session management) ──
 *
 * Full-screen overlay listing sessions for the current driver. Fetched
 * asynchronously via bb_agent_list_sessions (already implemented for both
 * local_home and cloud_saas). Pattern mirrors spawn_driver_fetch_task.
 */

typedef struct {
  uint32_t gen;
  esp_err_t err;
  int total;
  bb_agent_session_info_t entries[BB_SESSION_PICKER_MAX];
} session_fetch_result_t;

#define BB_SESSION_FETCH_TASK_STACK 4096
#define BB_SESSION_FETCH_TASK_PRIO  4

static void session_picker_apply_styles(void);
static void session_picker_build_ui(void);

static void on_session_fetch_done(void* user_data) {
  session_fetch_result_t* res = (session_fetch_result_t*)user_data;
  if (res == NULL) return;
  s_chat.session_fetch_pending = 0;
  if (!s_chat.active || res->gen != s_chat.session_fetch_generation) {
    free(res);
    return;
  }
  if (res->err != ESP_OK) {
    ESP_LOGW(TAG, "session fetch failed: %s", esp_err_to_name(res->err));
    s_chat.session_list_count = 0;
    s_chat.session_picker_visible = 0;
    free(res);
    return;
  }
  int total = res->total > BB_SESSION_PICKER_MAX ? BB_SESSION_PICKER_MAX : res->total;
  memcpy(s_chat.session_list, res->entries, sizeof(res->entries[0]) * (size_t)total);
  s_chat.session_list_count = total;

  /* Find the active session in the list. */
  s_chat.session_picker_active_idx = -1;
  for (int i = 0; i < total; ++i) {
    if (s_chat.session_id[0] != '\0' &&
        strcmp(s_chat.session_list[i].id, s_chat.session_id) == 0) {
      s_chat.session_picker_active_idx = i;
      break;
    }
  }

  s_chat.session_picker_visible = 2;
  session_picker_build_ui();
  free(res);
}

static void session_fetch_task(void* arg) {
  uint32_t my_gen = (uint32_t)(uintptr_t)arg;
  session_fetch_result_t* res = (session_fetch_result_t*)calloc(1, sizeof(*res));
  if (res == NULL) {
    s_chat.session_fetch_pending = 0;
    vTaskDelete(NULL);
    return;
  }
  res->gen = my_gen;
  for (int i = 0; i < 50 && !bb_wifi_is_connected(); ++i) {
    vTaskDelay(pdMS_TO_TICKS(200));
  }
  const char* drv = s_chat.driver_name[0] != '\0'
                      ? s_chat.driver_name : BB_CHAT_DRIVER_FALLBACK;
  res->err = bb_agent_list_sessions(drv, res->entries, BB_SESSION_PICKER_MAX, &res->total);
  safe_lv_async_call(on_session_fetch_done, res);
  vTaskDelete(NULL);
}

static void spawn_session_fetch_task(void) {
  if (s_chat.session_fetch_pending) return;
  s_chat.session_fetch_pending = 1;
  uint32_t gen = ++s_chat.session_fetch_generation;
  TaskHandle_t t = NULL;
  BaseType_t ok = xTaskCreate(session_fetch_task, "ses_fetch",
                              BB_SESSION_FETCH_TASK_STACK,
                              (void*)(uintptr_t)gen,
                              BB_SESSION_FETCH_TASK_PRIO, &t);
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "spawn_session_fetch_task: xTaskCreate failed");
    s_chat.session_fetch_pending = 0;
  }
}

/* ── Phase S3 — history replay fetch ───────────────────────────────────────
 *
 * Lifecycle:
 *   1. session_picker_select() rebuilds the empty transcript and calls
 *      spawn_history_fetch_task(-1, is_initial=1).
 *   2. The worker task calls bb_agent_load_messages() (HTTP GET, blocks
 *      until response or timeout).
 *   3. Worker hands the result struct (which OWNS the bb_agent_message_t
 *      heap array) to the LVGL task via lv_async_call(on_history_fetch_done).
 *   4. on_history_fetch_done renders messages through the active theme and
 *      frees everything it received.
 *
 * Stale guard: history_fetch_generation is bumped on every spawn. If the
 * user picks a different session before the response lands, we drop the
 * stale result on arrival. Same pattern as session_fetch_*.
 */

#define BB_HISTORY_FETCH_TASK_STACK 4096
#define BB_HISTORY_FETCH_TASK_PRIO  4

typedef struct {
  uint32_t gen;
  esp_err_t err;
  int is_initial;          /* 1 = first page after entering session, 0 = paginate-earlier */
  int total;
  int has_more;
  int count;
  bb_agent_message_t* msgs;  /* heap array; ownership transfers to dispatcher */
  /* Captured at spawn time so the worker doesn't read shared mutable state. */
  char session_id[64];
  char driver_name[24];
  int  before;
} history_fetch_result_t;

typedef struct {
  uint32_t gen;
  int is_initial;
  int before;
  char session_id[64];
  char driver_name[24];
} history_fetch_args_t;

static void on_history_fetch_done(void* user_data);

static void history_fetch_task(void* arg) {
  history_fetch_args_t* args = (history_fetch_args_t*)arg;
  history_fetch_result_t* res = (history_fetch_result_t*)heap_caps_calloc(
      1, sizeof(*res), MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
  if (res == NULL) {
    free(args);
    s_chat.history_fetch_pending = 0;
    vTaskDelete(NULL);
    return;
  }
  res->gen = args->gen;
  res->is_initial = args->is_initial;
  res->before = args->before;
  strncpy(res->session_id, args->session_id, sizeof(res->session_id) - 1);
  strncpy(res->driver_name, args->driver_name, sizeof(res->driver_name) - 1);

  /* Wait for WiFi the same way session_fetch_task does. */
  for (int i = 0; i < 50 && !bb_wifi_is_connected(); ++i) {
    vTaskDelay(pdMS_TO_TICKS(200));
  }

  res->err = bb_agent_load_messages(args->session_id, args->driver_name,
                                    args->before, BB_HISTORY_PAGE_SIZE,
                                    &res->msgs, &res->count, &res->total, &res->has_more);
  free(args);
  safe_lv_async_call(on_history_fetch_done, res);
  vTaskDelete(NULL);
}

static void spawn_history_fetch_task(int before, int is_initial) {
  if (s_chat.history_fetch_pending) return;
  if (s_chat.session_id[0] == '\0') return;
  if (s_chat.history_loaded_count >= BB_HISTORY_MAX_LOADED && !is_initial) {
    ESP_LOGD(TAG, "history fetch: in-DOM cap %d reached, skip", BB_HISTORY_MAX_LOADED);
    return;
  }

  history_fetch_args_t* args = (history_fetch_args_t*)heap_caps_calloc(
      1, sizeof(*args), MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
  if (args == NULL) return;
  args->is_initial = is_initial;
  args->before = before;
  args->gen = ++s_chat.history_fetch_generation;
  strncpy(args->session_id, s_chat.session_id, sizeof(args->session_id) - 1);
  const char* drv = s_chat.driver_name[0] != '\0' ? s_chat.driver_name : BB_CHAT_DRIVER_FALLBACK;
  strncpy(args->driver_name, drv, sizeof(args->driver_name) - 1);

  s_chat.history_fetch_pending = 1;
  TaskHandle_t t = NULL;
  BaseType_t ok = xTaskCreate(history_fetch_task, "hist_fetch",
                              BB_HISTORY_FETCH_TASK_STACK,
                              args, BB_HISTORY_FETCH_TASK_PRIO, &t);
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "spawn_history_fetch_task: xTaskCreate failed");
    s_chat.history_fetch_pending = 0;
    free(args);
  }
}

/* Runs in LVGL task. Renders the page through the active theme then frees
 * the result + its message strings. */
static void on_history_fetch_done(void* user_data) {
  history_fetch_result_t* res = (history_fetch_result_t*)user_data;
  if (res == NULL) return;
  s_chat.history_fetch_pending = 0;

  /* Stale: user switched sessions before we got back. */
  if (!s_chat.active || res->gen != s_chat.history_fetch_generation ||
      strcmp(res->session_id, s_chat.session_id) != 0) {
    bb_agent_messages_free(res->msgs, res->count);
    free(res);
    return;
  }

  if (res->err != ESP_OK) {
    if (res->err == ESP_ERR_NOT_SUPPORTED) {
      ESP_LOGI(TAG, "history fetch: driver does not support replay (sid=%s)", res->session_id);
    } else {
      ESP_LOGW(TAG, "history fetch failed err=%s", esp_err_to_name(res->err));
    }
    /* Hard fail: leave transcript blank, future scrolls won't retry. */
    s_chat.history_has_more = 0;
    bb_agent_messages_free(res->msgs, res->count);
    free(res);
    return;
  }

  s_chat.history_total = res->total;
  s_chat.history_has_more = res->has_more;

  if (res->count <= 0) {
    free(res);
    return;
  }

  const bb_agent_theme_t* theme = bb_agent_theme_get_active();
  if (theme == NULL) {
    bb_agent_messages_free(res->msgs, res->count);
    free(res);
    return;
  }

  if (res->is_initial) {
    /* Chronological order — append from oldest to newest. */
    if (theme->append_history_message != NULL) {
      for (int i = 0; i < res->count; i++) {
        theme->append_history_message(res->msgs[i].role, res->msgs[i].content);
      }
    }
    /* min_seq is the oldest seq we just rendered. */
    s_chat.history_min_seq = res->msgs[0].seq;
    s_chat.history_loaded_count = res->count;
    /* After initial load, scroll to the most-recent turn so the user sees
     * the conversation's latest state, not the oldest in the loaded window.
     * scroll_transcript_to_bottom uses lv_obj_scroll_to_view internally
     * which forces a layout pass — scroll_by_bounded silently no-ops if
     * the freshly-appended labels haven't reflowed yet (real bug seen on
     * device 2026-05-04). */
    if (theme->scroll_transcript_to_bottom != NULL) {
      theme->scroll_transcript_to_bottom();
    }
  } else {
    /* Pagination: prepend in REVERSE so oldest in batch ends up at the very
     * top after all the move_to_index(0) calls. */
    if (theme->prepend_history_message != NULL) {
      for (int i = res->count - 1; i >= 0; i--) {
        theme->prepend_history_message(res->msgs[i].role, res->msgs[i].content);
      }
    }
    s_chat.history_min_seq = res->msgs[0].seq;
    s_chat.history_loaded_count += res->count;
    if (s_chat.history_loaded_count > BB_HISTORY_MAX_LOADED) {
      s_chat.history_has_more = 0;  /* stop further pagination */
    }
  }
  ESP_LOGI(TAG, "history loaded sid=%s initial=%d count=%d total=%d min_seq=%d has_more=%d",
           res->session_id, res->is_initial, res->count, res->total,
           s_chat.history_min_seq, s_chat.history_has_more);

  bb_agent_messages_free(res->msgs, res->count);
  free(res);
}

/* Reset all history bookkeeping. Called when entering a new session so a
 * stale "min_seq" from the previous session can't be used as a cursor. */
static void history_state_reset(void) {
  s_chat.history_total = 0;
  s_chat.history_min_seq = -1;
  s_chat.history_loaded_count = 0;
  s_chat.history_has_more = 0;
  /* bumping generation invalidates any in-flight fetch we don't want anymore */
  s_chat.history_fetch_generation++;
}

/* ── Session picker UI ── */

#define BB_SPICKER_BG       0x12211b
#define BB_SPICKER_FG       0xc8e2d6
#define BB_SPICKER_FG_DIM   0x6b8c80
#define BB_SPICKER_SEL_BG   0x2ec4a0
#define BB_SPICKER_SEL_FG   0x0a0e0c
#define BB_SPICKER_TITLE_H  22

static void session_picker_apply_styles(void) {
  const int total = s_chat.session_picker_total_rows;
  if (total == 0) return;
  int first = s_chat.session_picker_sel - BB_SESSION_PICKER_VISIBLE / 2;
  if (first < 0) first = 0;
  if (first + BB_SESSION_PICKER_VISIBLE > total) {
    first = total - BB_SESSION_PICKER_VISIBLE;
    if (first < 0) first = 0;
  }
  for (int i = 0; i < total; ++i) {
    lv_obj_t* row = s_chat.session_picker_items[i];
    if (row == NULL) continue;
    int visible = (i >= first && i < first + BB_SESSION_PICKER_VISIBLE);
    if (visible) {
      lv_obj_clear_flag(row, LV_OBJ_FLAG_HIDDEN);
    } else {
      lv_obj_add_flag(row, LV_OBJ_FLAG_HIDDEN);
    }
    if (i == s_chat.session_picker_sel) {
      lv_obj_set_style_bg_color(row, lv_color_hex(BB_SPICKER_SEL_BG), 0);
      lv_obj_set_style_bg_opa(row, LV_OPA_COVER, 0);
      lv_obj_set_style_text_color(row, lv_color_hex(BB_SPICKER_SEL_FG), 0);
    } else {
      lv_obj_set_style_bg_opa(row, LV_OPA_TRANSP, 0);
      lv_obj_set_style_text_color(row, lv_color_hex(BB_SPICKER_FG_DIM), 0);
    }
  }
}

static void format_relative_time(int64_t last_used_ms, char* buf, int buf_len) {
  if (last_used_ms <= 0) {
    buf[0] = '\0';
    return;
  }
  /* Use wall-clock time. Guard against SNTP not yet synced: if the system
   * clock is still near the epoch (< 2020-01-01), the time is unreliable. */
  time_t now_sec = time(NULL);
  if (now_sec < 1577836800LL) { /* 2020-01-01 00:00:00 UTC */
    buf[0] = '\0';
    return;
  }
  int64_t now_ms = (int64_t)now_sec * 1000LL;
  int64_t diff_s = (now_ms - last_used_ms) / 1000;
  if (diff_s < 0) diff_s = 0;
  if (diff_s < 60) {
    snprintf(buf, (size_t)buf_len, "<1m");
  } else if (diff_s < 3600) {
    snprintf(buf, (size_t)buf_len, "%dm", (int)(diff_s / 60));
  } else if (diff_s < 86400) {
    snprintf(buf, (size_t)buf_len, "%dh", (int)(diff_s / 3600));
  } else {
    snprintf(buf, (size_t)buf_len, "%dd", (int)(diff_s / 86400));
  }
}

static void session_picker_build_ui(void) {
  if (s_chat.parent == NULL) return;

#ifdef BBCLAW_HAVE_CJK_FONT
  extern const lv_font_t lv_font_bbclaw_cjk;
  const lv_font_t* font = &lv_font_bbclaw_cjk;
#else
  const lv_font_t* font = lv_font_get_default();
#endif

  /* Tear down previous picker if any. */
  if (s_chat.session_picker_root != NULL) {
    lv_obj_del(s_chat.session_picker_root);
    s_chat.session_picker_root = NULL;
    memset(s_chat.session_picker_items, 0, sizeof(s_chat.session_picker_items));
  }

  const int n_sessions = s_chat.session_list_count;
  /* Layout: [+ 新建 session] [session 0..n-1] [Settings] */
  const int total_rows = n_sessions + 2; /* + New + Settings */
  s_chat.session_picker_total_rows = total_rows;
  /* Active idx is into session_list[]; the picker visual idx is shifted by 1
   * because row 0 is the "+ 新建 session" entry. */
  s_chat.session_picker_sel = (s_chat.session_picker_active_idx >= 0)
                                ? s_chat.session_picker_active_idx + 1 : 0;

  /* Full-screen overlay. */
  s_chat.session_picker_root = lv_obj_create(s_chat.parent);
  lv_obj_remove_style_all(s_chat.session_picker_root);
  lv_obj_set_size(s_chat.session_picker_root, lv_pct(100), lv_pct(100));
  lv_obj_align(s_chat.session_picker_root, LV_ALIGN_TOP_LEFT, 0, 0);
  lv_obj_set_style_bg_color(s_chat.session_picker_root, lv_color_hex(BB_SPICKER_BG), 0);
  lv_obj_set_style_bg_opa(s_chat.session_picker_root, LV_OPA_COVER, 0);
  lv_obj_set_style_pad_all(s_chat.session_picker_root, 2, 0);
  lv_obj_set_flex_flow(s_chat.session_picker_root, LV_FLEX_FLOW_COLUMN);
  lv_obj_clear_flag(s_chat.session_picker_root, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_move_foreground(s_chat.session_picker_root);

  /* Title row. */
  lv_obj_t* title = lv_label_create(s_chat.session_picker_root);
  lv_obj_set_size(title, lv_pct(100), BB_SPICKER_TITLE_H);
  lv_obj_set_style_text_color(title, lv_color_hex(BB_SPICKER_FG), 0);
  lv_obj_set_style_text_font(title, font, 0);
  lv_obj_set_style_pad_left(title, 2, 0);
  char title_buf[48];
  const char* drv = s_chat.driver_name[0] != '\0'
                      ? s_chat.driver_name : BB_CHAT_DRIVER_FALLBACK;
  snprintf(title_buf, sizeof(title_buf), "Sessions (%s)", drv);
  lv_label_set_text(title, title_buf);

  /* "+ 新建 session" fixed row at the top. */
  {
    lv_obj_t* row = lv_label_create(s_chat.session_picker_root);
    lv_obj_set_size(row, lv_pct(100), BB_SESSION_PICKER_ROW_H);
    lv_obj_set_style_pad_left(row, 4, 0);
    lv_obj_set_style_radius(row, 3, 0);
    lv_obj_set_style_text_font(row, font, 0);
    lv_label_set_text(row, "+ 新建 session");
    s_chat.session_picker_items[0] = row;
  }

  /* Session rows: "preview [Nm] [Xt] [<]" */
  for (int i = 0; i < n_sessions; ++i) {
    lv_obj_t* row = lv_label_create(s_chat.session_picker_root);
    lv_obj_set_size(row, lv_pct(100), BB_SESSION_PICKER_ROW_H);
    lv_obj_set_style_pad_left(row, 4, 0);
    lv_obj_set_style_pad_right(row, 4, 0);
    lv_obj_set_style_radius(row, 3, 0);
    lv_obj_set_style_text_font(row, font, 0);
    lv_label_set_long_mode(row, LV_LABEL_LONG_MODE_DOTS);

    const bb_agent_session_info_t* si = &s_chat.session_list[i];
    const char* title = si->title;
    if (title == NULL || title[0] == '\0') title = "(untitled)";

    char time_buf[8] = {0};
    format_relative_time(si->last_used_ms, time_buf, sizeof(time_buf));

    char suffix[32] = {0};
    int off = 0;
    if (si->message_count > 0) {
      off += snprintf(suffix + off, sizeof(suffix) - (size_t)off, " %dm", si->message_count);
    }
    if (time_buf[0] != '\0') {
      off += snprintf(suffix + off, sizeof(suffix) - (size_t)off, " %s", time_buf);
    }
    if (si->cwd[0] != '\0') {
      off += snprintf(suffix + off, sizeof(suffix) - (size_t)off, " %s", si->cwd);
    }
    if (i == s_chat.session_picker_active_idx) {
      snprintf(suffix + off, sizeof(suffix) - (size_t)off, " <");
    }

    int title_max = 22 - (int)strlen(suffix);
    if (title_max < 6) title_max = 6;

    char row_buf[48];
    snprintf(row_buf, sizeof(row_buf), "%.*s%s", title_max, title, suffix);
    lv_label_set_text(row, row_buf);
    /* Sessions occupy visual rows [1 .. n_sessions]; row 0 is "+ 新建". */
    s_chat.session_picker_items[1 + i] = row;
  }

  /* "Settings" row at the very end. */
  {
    lv_obj_t* row = lv_label_create(s_chat.session_picker_root);
    lv_obj_set_size(row, lv_pct(100), BB_SESSION_PICKER_ROW_H);
    lv_obj_set_style_pad_left(row, 4, 0);
    lv_obj_set_style_radius(row, 3, 0);
    lv_obj_set_style_text_font(row, font, 0);
    lv_label_set_text(row, "* Settings");
    s_chat.session_picker_items[1 + n_sessions] = row;
  }

  session_picker_apply_styles();
  ESP_LOGI(TAG, "session_picker: built %d sessions + 2 fixed rows", n_sessions);
}

/* ── Session picker public API ── */

void bb_ui_agent_chat_session_picker_show(void) {
  if (!s_chat.active || s_chat.parent == NULL) return;
  if (s_chat.session_picker_visible) return;
  s_chat.session_picker_visible = 1; /* loading */
  ESP_LOGI(TAG, "session_picker: show (fetching sessions for '%s')",
           s_chat.driver_name[0] != '\0' ? s_chat.driver_name : BB_CHAT_DRIVER_FALLBACK);
  spawn_session_fetch_task();
}

void bb_ui_agent_chat_session_picker_hide(void) {
  if (!s_chat.active) return;
  if (s_chat.session_picker_root != NULL) {
    lv_obj_del(s_chat.session_picker_root);
    s_chat.session_picker_root = NULL;
    memset(s_chat.session_picker_items, 0, sizeof(s_chat.session_picker_items));
  }
  s_chat.session_picker_visible = 0;
  s_chat.session_picker_total_rows = 0;
  ESP_LOGI(TAG, "session_picker: hidden");
}

void bb_ui_agent_chat_session_picker_move(int delta) {
  if (!s_chat.active || s_chat.session_picker_visible != 2) return;
  const int total = s_chat.session_picker_total_rows;
  if (total == 0) return;
  int sel = ((s_chat.session_picker_sel + delta) % total + total) % total;
  if (sel == s_chat.session_picker_sel) return;
  s_chat.session_picker_sel = sel;
  session_picker_apply_styles();
}

/* Common transcript-reset + topbar-refresh used after both session-switch and
 * new-session-creation. Caller already updated s_chat.session_id and persisted
 * to NVS. Must run on the LVGL task. */
static void apply_session_switch_ui(const char* sid) {
  char shortbuf[16] = {0};
  session_id_short(sid, shortbuf, sizeof(shortbuf));
  post_session(shortbuf);

  const bb_agent_theme_t* theme = bb_agent_theme_get_active();
  if (theme != NULL) {
    if (theme->on_exit != NULL) theme->on_exit();
    if (theme->on_enter != NULL) theme->on_enter(s_chat.parent);
  }
  post_state(BB_AGENT_STATE_IDLE);
}

/* ── ADR-014 — new-session creation worker ── */

#define BB_NEW_SESSION_TASK_STACK 6144
#define BB_NEW_SESSION_TASK_PRIO  4

typedef struct {
  esp_err_t err;
  char session_id[64];
  char driver_name[24];
} new_session_result_t;

typedef struct {
  char driver_name[24];
  char cwd_name[32]; /* issue #30: selected project name; "" = no selection */
} new_session_args_t;

static void on_new_session_done(void* user_data) {
  new_session_result_t* res = (new_session_result_t*)user_data;
  if (res == NULL) return;
  if (!s_chat.active) {
    free(res);
    return;
  }
  const bb_agent_theme_t* theme = bb_agent_theme_get_active();
  if (res->err != ESP_OK || res->session_id[0] == '\0') {
    ESP_LOGW(TAG, "new_session: create failed err=%s", esp_err_to_name(res->err));
    if (theme != NULL && theme->append_error != NULL) {
      theme->append_error("new session failed");
    }
    free(res);
    return;
  }

  ESP_LOGI(TAG, "new_session: created sid=%s driver=%s", res->session_id, res->driver_name);
  strncpy(s_chat.session_id, res->session_id, sizeof(s_chat.session_id) - 1);
  s_chat.session_id[sizeof(s_chat.session_id) - 1] = '\0';
  if (res->driver_name[0] != '\0') {
    bb_session_store_save(res->driver_name, res->session_id);
  } else if (s_chat.driver_name[0] != '\0') {
    bb_session_store_save(s_chat.driver_name, res->session_id);
  }
  bb_notification_ack(res->session_id);

  apply_session_switch_ui(res->session_id);

  /* Brand-new session has no history; just reset bookkeeping. */
  history_state_reset();

  free(res);
}

static void new_session_task(void* arg) {
  new_session_args_t* args = (new_session_args_t*)arg;
  new_session_result_t* res = (new_session_result_t*)heap_caps_calloc(
      1, sizeof(*res), MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
  if (res == NULL) {
    free(args);
    vTaskDelete(NULL);
    return;
  }
  if (args != NULL) {
    strncpy(res->driver_name, args->driver_name, sizeof(res->driver_name) - 1);
  }

  for (int i = 0; i < 50 && !bb_wifi_is_connected(); ++i) {
    vTaskDelay(pdMS_TO_TICKS(200));
  }

  res->err = bb_agent_create_session(res->driver_name,
                                     NULL,
                                     args->cwd_name[0] != '\0' ? args->cwd_name : NULL,
                                     res->session_id, sizeof(res->session_id));
  free(args);
  safe_lv_async_call(on_new_session_done, res);
  vTaskDelete(NULL);
}

static void spawn_new_session_task(const char* cwd_name) {
  new_session_args_t* args = (new_session_args_t*)heap_caps_calloc(
      1, sizeof(*args), MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
  if (args == NULL) {
    ESP_LOGE(TAG, "spawn_new_session_task: alloc failed");
    return;
  }
  const char* drv = s_chat.driver_name[0] != '\0'
                      ? s_chat.driver_name : BB_CHAT_DRIVER_FALLBACK;
  strncpy(args->driver_name, drv, sizeof(args->driver_name) - 1);
  if (cwd_name != NULL && cwd_name[0] != '\0') {
    strncpy(args->cwd_name, cwd_name, sizeof(args->cwd_name) - 1);
  }

  TaskHandle_t t = NULL;
  BaseType_t ok = xTaskCreateWithCaps(new_session_task, "new_session",
                                      BB_NEW_SESSION_TASK_STACK, args,
                                      BB_NEW_SESSION_TASK_PRIO, &t,
                                      BBCLAW_MALLOC_CAP_PREFER_PSRAM);
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "spawn_new_session_task: xTaskCreateWithCaps failed");
    free(args);
  }
}

/* ── CWD pool fetch + picker (issue #30) ──────────────────────────────────
 *
 * When the user selects "+ 新建 session" and the adapter has more than one
 * project configured, we show a project-selection sub-menu before creating
 * the session. If the pool has 0 or 1 entries we skip the picker entirely
 * (backward-compatible with single-project setups).
 *
 * Lifecycle:
 *   1. session_picker_select() detects the "+ 新建" row.
 *   2. If cwd_pool_count > 1 (already cached) → show picker immediately.
 *      If not yet fetched → spawn cwd_pool_fetch_task, show picker when done.
 *      If pool has ≤ 1 entry → create session directly (existing behaviour).
 *   3. User navigates with UP/DOWN, confirms with OK → spawn_new_session_task(name).
 *   4. BACK → hide picker, return to session picker (or just close).
 */

#define BB_CWD_FETCH_TASK_STACK 4096
#define BB_CWD_FETCH_TASK_PRIO  4

typedef struct {
  uint32_t gen;
  esp_err_t err;
  int total;
  bb_agent_cwd_entry_t entries[BB_CWD_PICKER_MAX];
} cwd_pool_fetch_result_t;

static void cwd_picker_build_ui(void);

static void on_cwd_pool_fetch_done(void* user_data) {
  cwd_pool_fetch_result_t* res = (cwd_pool_fetch_result_t*)user_data;
  if (res == NULL) return;
  s_chat.cwd_pool_fetch_pending = 0;
  if (!s_chat.active || res->gen != s_chat.cwd_pool_fetch_generation) {
    free(res);
    return;
  }
  if (res->err != ESP_OK || res->total <= 0) {
    ESP_LOGW(TAG, "cwd_pool fetch failed (%s), creating session without cwd_name",
             esp_err_to_name(res->err));
    s_chat.cwd_pool_count = 0;
    /* Fall back: create session directly without a cwd_name. */
    if (s_chat.cwd_picker_visible) {
      s_chat.cwd_picker_visible = 0;
      spawn_new_session_task(NULL);
    }
    free(res);
    return;
  }
  int total = res->total > BB_CWD_PICKER_MAX ? BB_CWD_PICKER_MAX : res->total;
  memcpy(s_chat.cwd_pool, res->entries, sizeof(res->entries[0]) * (size_t)total);
  s_chat.cwd_pool_count = total;
  free(res);

  if (total <= 1) {
    /* Single entry or empty — skip picker, create directly. */
    s_chat.cwd_picker_visible = 0;
    const char* name = (total == 1) ? s_chat.cwd_pool[0].name : NULL;
    spawn_new_session_task(name);
    return;
  }
  /* Multiple entries — show the picker. */
  s_chat.cwd_picker_visible = 2;
  s_chat.cwd_picker_sel = 0;
  cwd_picker_build_ui();
}

static void cwd_pool_fetch_task(void* arg) {
  uint32_t my_gen = (uint32_t)(uintptr_t)arg;
  cwd_pool_fetch_result_t* res = (cwd_pool_fetch_result_t*)calloc(1, sizeof(*res));
  if (res == NULL) {
    s_chat.cwd_pool_fetch_pending = 0;
    vTaskDelete(NULL);
    return;
  }
  res->gen = my_gen;
  for (int i = 0; i < 50 && !bb_wifi_is_connected(); ++i) {
    vTaskDelay(pdMS_TO_TICKS(200));
  }
  res->err = bb_agent_list_cwd_pool(res->entries, BB_CWD_PICKER_MAX, &res->total);
  safe_lv_async_call(on_cwd_pool_fetch_done, res);
  vTaskDelete(NULL);
}

/* Fetch the CWD pool from the adapter. If already cached (count > 0), calls
 * on_cwd_pool_fetch_done synchronously via lv_async_call with the cached data
 * so the caller always gets a consistent callback-driven flow. */
static void spawn_cwd_pool_fetch_or_use_cache(void) {
  if (s_chat.cwd_pool_count > 0) {
    /* Already have data — synthesise a result from cache. */
    cwd_pool_fetch_result_t* res = (cwd_pool_fetch_result_t*)calloc(1, sizeof(*res));
    if (res == NULL) {
      spawn_new_session_task(NULL);
      return;
    }
    res->gen = s_chat.cwd_pool_fetch_generation;
    res->err = ESP_OK;
    res->total = s_chat.cwd_pool_count;
    memcpy(res->entries, s_chat.cwd_pool,
           sizeof(res->entries[0]) * (size_t)s_chat.cwd_pool_count);
    safe_lv_async_call(on_cwd_pool_fetch_done, res);
    return;
  }
  if (s_chat.cwd_pool_fetch_pending) return;
  s_chat.cwd_pool_fetch_pending = 1;
  uint32_t gen = ++s_chat.cwd_pool_fetch_generation;
  TaskHandle_t t = NULL;
  BaseType_t ok = xTaskCreate(cwd_pool_fetch_task, "cwd_fetch",
                              BB_CWD_FETCH_TASK_STACK,
                              (void*)(uintptr_t)gen,
                              BB_CWD_FETCH_TASK_PRIO, &t);
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "spawn_cwd_pool_fetch: xTaskCreate failed");
    s_chat.cwd_pool_fetch_pending = 0;
    spawn_new_session_task(NULL);
  }
}

/* ── CWD picker UI ── */

#define BB_CWDPICKER_BG      0x12211b
#define BB_CWDPICKER_FG      0xc8e2d6
#define BB_CWDPICKER_FG_DIM  0x6b8c80
#define BB_CWDPICKER_SEL_BG  0x2ec4a0
#define BB_CWDPICKER_SEL_FG  0x0a0e0c
#define BB_CWDPICKER_TITLE_H 22

static void cwd_picker_apply_styles(void) {
  const int total = s_chat.cwd_pool_count;
  if (total == 0) return;
  int first = s_chat.cwd_picker_sel - BB_CWD_PICKER_VISIBLE / 2;
  if (first < 0) first = 0;
  if (first + BB_CWD_PICKER_VISIBLE > total) {
    first = total - BB_CWD_PICKER_VISIBLE;
    if (first < 0) first = 0;
  }
  for (int i = 0; i < total; ++i) {
    lv_obj_t* row = s_chat.cwd_picker_items[i];
    if (row == NULL) continue;
    int visible = (i >= first && i < first + BB_CWD_PICKER_VISIBLE);
    if (visible) {
      lv_obj_clear_flag(row, LV_OBJ_FLAG_HIDDEN);
    } else {
      lv_obj_add_flag(row, LV_OBJ_FLAG_HIDDEN);
    }
    if (i == s_chat.cwd_picker_sel) {
      lv_obj_set_style_bg_color(row, lv_color_hex(BB_CWDPICKER_SEL_BG), 0);
      lv_obj_set_style_bg_opa(row, LV_OPA_COVER, 0);
      lv_obj_set_style_text_color(row, lv_color_hex(BB_CWDPICKER_SEL_FG), 0);
    } else {
      lv_obj_set_style_bg_opa(row, LV_OPA_TRANSP, 0);
      lv_obj_set_style_text_color(row, lv_color_hex(BB_CWDPICKER_FG_DIM), 0);
    }
  }
}

static void cwd_picker_build_ui(void) {
  if (s_chat.parent == NULL) return;

#ifdef BBCLAW_HAVE_CJK_FONT
  extern const lv_font_t lv_font_bbclaw_cjk;
  const lv_font_t* font = &lv_font_bbclaw_cjk;
#else
  const lv_font_t* font = lv_font_get_default();
#endif

  /* Tear down previous picker if any. */
  if (s_chat.cwd_picker_root != NULL) {
    lv_obj_del(s_chat.cwd_picker_root);
    s_chat.cwd_picker_root = NULL;
    memset(s_chat.cwd_picker_items, 0, sizeof(s_chat.cwd_picker_items));
  }

  const int n = s_chat.cwd_pool_count;

  s_chat.cwd_picker_root = lv_obj_create(s_chat.parent);
  lv_obj_remove_style_all(s_chat.cwd_picker_root);
  lv_obj_set_size(s_chat.cwd_picker_root, lv_pct(100), lv_pct(100));
  lv_obj_align(s_chat.cwd_picker_root, LV_ALIGN_TOP_LEFT, 0, 0);
  lv_obj_set_style_bg_color(s_chat.cwd_picker_root, lv_color_hex(BB_CWDPICKER_BG), 0);
  lv_obj_set_style_bg_opa(s_chat.cwd_picker_root, LV_OPA_COVER, 0);
  lv_obj_set_style_pad_all(s_chat.cwd_picker_root, 2, 0);
  lv_obj_set_flex_flow(s_chat.cwd_picker_root, LV_FLEX_FLOW_COLUMN);
  lv_obj_clear_flag(s_chat.cwd_picker_root, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_move_foreground(s_chat.cwd_picker_root);

  /* Title row. */
  lv_obj_t* title = lv_label_create(s_chat.cwd_picker_root);
  lv_obj_set_size(title, lv_pct(100), BB_CWDPICKER_TITLE_H);
  lv_obj_set_style_text_color(title, lv_color_hex(BB_CWDPICKER_FG), 0);
  lv_obj_set_style_text_font(title, font, 0);
  lv_obj_set_style_pad_left(title, 2, 0);
  lv_label_set_text(title, "新建 Session");

  /* Project rows. */
  for (int i = 0; i < n; ++i) {
    lv_obj_t* row = lv_label_create(s_chat.cwd_picker_root);
    lv_obj_set_size(row, lv_pct(100), BB_CWD_PICKER_ROW_H);
    lv_obj_set_style_pad_left(row, 4, 0);
    lv_obj_set_style_pad_right(row, 4, 0);
    lv_obj_set_style_radius(row, 3, 0);
    lv_obj_set_style_text_font(row, font, 0);
    lv_label_set_long_mode(row, LV_LABEL_LONG_MODE_DOTS);
    lv_label_set_text(row, s_chat.cwd_pool[i].name);
    s_chat.cwd_picker_items[i] = row;
  }

  cwd_picker_apply_styles();
  ESP_LOGI(TAG, "cwd_picker: built %d project entries", n);
}

/* Hide and destroy the CWD picker overlay. */
static void cwd_picker_hide(void) {
  if (s_chat.cwd_picker_root != NULL) {
    lv_obj_del(s_chat.cwd_picker_root);
    s_chat.cwd_picker_root = NULL;
    memset(s_chat.cwd_picker_items, 0, sizeof(s_chat.cwd_picker_items));
  }
  s_chat.cwd_picker_visible = 0;
}

/* Move selection up/down in the CWD picker. */
void bb_ui_agent_chat_cwd_picker_move(int delta) {
  if (!s_chat.active || s_chat.cwd_picker_visible != 2) return;
  const int total = s_chat.cwd_pool_count;
  if (total == 0) return;
  int sel = ((s_chat.cwd_picker_sel + delta) % total + total) % total;
  if (sel == s_chat.cwd_picker_sel) return;
  s_chat.cwd_picker_sel = sel;
  cwd_picker_apply_styles();
}

/* Confirm selection in the CWD picker (OK key). Creates the session with the
 * selected project name. */
void bb_ui_agent_chat_cwd_picker_confirm(void) {
  if (!s_chat.active || s_chat.cwd_picker_visible != 2) return;
  const int sel = s_chat.cwd_picker_sel;
  if (sel < 0 || sel >= s_chat.cwd_pool_count) return;
  const char* name = s_chat.cwd_pool[sel].name;
  ESP_LOGI(TAG, "cwd_picker: selected '%s'", name);
  cwd_picker_hide();
  spawn_new_session_task(name);
}

/* Cancel the CWD picker (BACK key). Returns to the session picker. */
void bb_ui_agent_chat_cwd_picker_cancel(void) {
  if (!s_chat.active || !s_chat.cwd_picker_visible) return;
  ESP_LOGI(TAG, "cwd_picker: cancelled");
  cwd_picker_hide();
}

/* Returns non-zero when the CWD picker is visible (so the key handler can
 * route UP/DOWN/OK/BACK to it instead of the session picker). */
int bb_ui_agent_chat_cwd_picker_is_visible(void) {
  return (s_chat.active && s_chat.cwd_picker_visible > 0) ? 1 : 0;
}

int bb_ui_agent_chat_session_picker_select(void) {
  if (!s_chat.active || s_chat.session_picker_visible != 2) {
    return BB_SESSION_PICKER_ACTION_NONE;
  }
  const int sel = s_chat.session_picker_sel;
  const int n = s_chat.session_list_count;

  if (sel == 0) {
    /* "+ 新建 session" row — fetch CWD pool; if > 1 entry show project picker,
     * otherwise create directly (backward-compatible). */
    ESP_LOGI(TAG, "session_picker: new session requested");
    bb_ui_agent_chat_session_picker_hide();
    /* Mark cwd_picker as loading so key handler routes to it. */
    s_chat.cwd_picker_visible = 1;
    spawn_cwd_pool_fetch_or_use_cache();
    return BB_SESSION_PICKER_ACTION_NEW_SESSION;
  }

  if (sel >= 1 && sel <= n) {
    /* Session row — switch to it. (Visual idx is shifted by 1 because row 0
     * is "+ 新建 session".) */
    const int idx = sel - 1;
    const bb_agent_session_info_t* session = &s_chat.session_list[idx];
    ESP_LOGI(TAG, "session_picker: switch to '%s'", session->id);

    strncpy(s_chat.session_id, session->id, sizeof(s_chat.session_id) - 1);
    s_chat.session_id[sizeof(s_chat.session_id) - 1] = '\0';
    bb_session_store_save(s_chat.driver_name, session->id);
    bb_notification_ack(session->id);

    apply_session_switch_ui(session->id);

    /* Phase S3 — fetch history for the just-selected session. The fetch is
     * async (worker task); the empty transcript is what the user sees in the
     * meantime. We don't gate session_picker_hide() on the response so picker
     * UX stays snappy. */
    history_state_reset();
    spawn_history_fetch_task(-1, /*is_initial=*/1);

    bb_ui_agent_chat_session_picker_hide();
    return BB_SESSION_PICKER_ACTION_SWITCH;
  }

  if (sel == n + 1) {
    /* "Settings" row. */
    ESP_LOGI(TAG, "session_picker: enter settings");
    bb_ui_agent_chat_session_picker_hide();
    return BB_SESSION_PICKER_ACTION_SETTINGS;
  }

  return BB_SESSION_PICKER_ACTION_NONE;
}

int bb_ui_agent_chat_session_picker_is_visible(void) {
  return (s_chat.active && s_chat.session_picker_visible > 0) ? 1 : 0;
}

/* ── Phase 4.5 — voice bridge helpers ── */

int bb_ui_agent_chat_is_busy(void) {
  return (s_chat.active && s_chat.sending && !s_chat.agent_cancel_requested) ? 1 : 0;
}

int bb_ui_agent_chat_is_adapter_offline(void) {
  return (s_chat.active && s_chat.driver_cache_offline) ? 1 : 0;
}

void bb_ui_agent_chat_retry_adapter(void) {
  if (!s_chat.active || !s_chat.driver_cache_offline) return;
  spawn_driver_fetch_task();
}

const char* bb_ui_agent_chat_get_current_driver(void) {
  if (s_chat.driver_name[0] != '\0') return s_chat.driver_name;
  return "claude-code";
}

/* Phase 4.9: cancel an in-flight agent turn. The HTTP stream continues to
 * drain in the background (agent_task), but on_agent_event discards all
 * events, TTS is killed, and the UI goes back to IDLE immediately. */
void bb_ui_agent_chat_cancel(void) {
  if (!s_chat.active || !s_chat.sending) return;
  if (s_chat.agent_cancel_requested) return;  /* already cancelled */
  s_chat.agent_cancel_requested = 1;
  tts_cancel_in_flight();
  post_state(BB_AGENT_STATE_IDLE);
  ESP_LOGI(TAG, "cancel: user cancelled in-flight turn");
}

void bb_ui_agent_chat_scroll(int lines) {
  if (!s_chat.active || lines == 0) return;
  const bb_agent_theme_t* theme = bb_agent_theme_get_active();
  if (theme != NULL && theme->scroll_transcript != NULL) {
    theme->scroll_transcript(lines);
  }
  /* Phase S3 — auto-fetch earlier history when the user scrolls to the very
   * top. We require:
   *   - an upward scroll gesture (lines < 0)
   *   - server reported more history exists
   *   - haven't hit the in-DOM cap
   *   - no fetch already in flight
   *   - theme reports we're at the top edge
   */
  if (lines < 0 && s_chat.history_has_more && !s_chat.history_fetch_pending &&
      s_chat.history_loaded_count < BB_HISTORY_MAX_LOADED && s_chat.history_min_seq > 0 &&
      theme != NULL && theme->is_transcript_at_top != NULL && theme->is_transcript_at_top()) {
    ESP_LOGI(TAG, "scroll-to-top: load earlier history before=%d", s_chat.history_min_seq);
    spawn_history_fetch_task(s_chat.history_min_seq, /*is_initial=*/0);
  }
}

/* When true, the topbar's session field shows the listening hint instead of
 * the cached short session id. Cleared by voice_listening(0) so that future
 * SESSION frames (post-send) restore the real id. */
static void post_listening_topbar(int begin) {
  if (!s_chat.active) return;
  if (begin) {
    /* Use the existing set_session pipe — the text-only theme renders this
     * in the topbar, so reusing it keeps the change purely additive in the
     * theme layer. "LISTEN..." fits in the 16-char buffer. */
    post_session("LISTEN..");
  } else {
    char shortbuf[16] = {0};
    session_id_short(s_chat.session_id, shortbuf, sizeof(shortbuf));
    post_session(shortbuf);
  }
}

void bb_ui_agent_chat_voice_listening(int begin) {
  if (!s_chat.active) {
    ESP_LOGW(TAG, "voice_listening(%d) called while chat inactive", begin);
    return;
  }
  ESP_LOGI(TAG, "voice_listening(%s) → %s", begin ? "begin" : "end",
           begin ? "LISTENING" : "topbar=restore");
  if (begin) {
    /* Phase 4.8.x: dedicated LISTENING state — buddy shows "listening..."
     * while user holds PTT. Agent's "thinking..." (BUSY) only kicks in
     * once agent_task starts after PTT release + ASR. */
    post_state(BB_AGENT_STATE_LISTENING);
    post_listening_topbar(1);
  } else {
    post_listening_topbar(0);
    /* Don't force IDLE here — caller follows up with bb_ui_agent_chat_send
     * which will drive BUSY → text → TURN_END. If we reset to IDLE here we
     * get a brief flicker; better to leave the previous state in place. */
  }
}

void bb_ui_agent_chat_voice_processing(void) {
  if (!s_chat.active) return;
  ESP_LOGI(TAG, "voice_processing → BUSY (ASR/cloud wait)");
  post_listening_topbar(0);
  post_state(BB_AGENT_STATE_BUSY);
}

void bb_ui_agent_chat_voice_error(void) {
  if (!s_chat.active) return;
  if (s_chat.sending) return;
  post_state(BB_AGENT_STATE_DIZZY);
}

esp_err_t bb_ui_agent_chat_cycle_driver(int delta) {
  /* Phase 5 — LEFT/RIGHT quick driver switch from picker mode.
   *
   * Reuses the Settings driver_cache: lazily populated on first call via the
   * same (currently blocking) HTTP fetch Settings entry uses. Phase 4.2.5 will
   * make that path async — when it does, this function automatically benefits.
   *
   * Behavior diverges from Settings in one place: we WRAP at the cache
   * boundaries instead of clamping, so the LEFT/RIGHT gesture feels like a
   * carousel rather than a slider that bonks at the ends.
   */
  if (!s_chat.active || s_chat.mode != BB_CHAT_MODE_PICKER) {
    return ESP_ERR_INVALID_STATE;
  }
  /* Phase 4.9: block driver switching while an agent turn is in flight. */
  if (s_chat.sending) {
    ESP_LOGI(TAG, "cycle_driver: blocked (agent turn in flight)");
    return ESP_ERR_INVALID_STATE;
  }
  if (delta == 0) return ESP_OK;

  /* Phase 4.2.5 — async cache: if it's still loading, spawn (idempotent) and
   * let the user know via the return code. The radio_app wrapper logs at
   * DEBUG so the gesture isn't a silent no-op forever — typically the next
   * press a moment later succeeds. */
  if (s_chat.driver_cache_count <= 0) {
    spawn_driver_fetch_task();
    return s_chat.driver_fetch_pending ? ESP_ERR_INVALID_STATE : ESP_ERR_NOT_FOUND;
  }
  const int n = s_chat.driver_cache_count;
  if (n <= 1) {
    /* Single (or zero) entry: nothing to cycle through. The offline-fallback
     * branch ends up here too — that's correct, switching to the only known
     * driver is a no-op. */
    ESP_LOGD(TAG, "cycle_driver: cache has %d entries, nothing to do", n);
    return ESP_ERR_NOT_FOUND;
  }

  /* Positive-modulo wrap (handles delta=-1 going from 0 -> n-1). */
  int next = ((s_chat.driver_cache_idx + delta) % n + n) % n;
  s_chat.driver_cache_idx = next;

  const char* name = s_chat.driver_cache[next].name;
  if (name == NULL || name[0] == '\0') return ESP_ERR_INVALID_RESPONSE;

  /* Phase 4.8.x: in-memory update + UI refresh stay on the LVGL-locked
   * caller path (cheap, no flash IO). The actual NVS write is offloaded
   * to a background task so the SPI-flash subsystem can safely disable
   * caches without colliding with the audio/agent stream that's likely
   * still running on the LVGL task. Worst case if the task fails to
   * spawn: in-memory state still updated → driver works for THIS
   * session, just doesn't survive reboot (logged). */
  strncpy(s_chat.driver_name, name, sizeof(s_chat.driver_name) - 1);
  s_chat.driver_name[sizeof(s_chat.driver_name) - 1] = '\0';

  s_chat.session_id[0] = '\0';

  /* ── Restore new driver's last session + history (issue #41) ── */
  /* 1. Load the new driver's last-used session_id from NVS (per-driver store). */
  load_nvs_on_internal_stack();

  /* 2. Rebuild transcript UI: clear stale content, re-init theme for new session. */
  apply_session_switch_ui(s_chat.session_id);

  /* 3. Fetch history for the restored session (or just reset state if none). */
  if (s_chat.session_id[0] != '\0') {
    history_state_reset();
    spawn_history_fetch_task(-1, /*is_initial=*/1);
  } else {
    history_state_reset();
  }

  const bb_agent_theme_t* theme = bb_agent_theme_get_active();
  if (theme != NULL && theme->set_driver != NULL) {
    theme->set_driver(name);
  }
  ESP_LOGI(TAG, "cycle_driver: '%s' (delta=%+d, idx=%d/%d) session restored",
           name, delta, next, n);
  return ESP_OK;
}
