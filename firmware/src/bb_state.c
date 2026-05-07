/**
 * bb_state.c — 全局状态协调器实现
 *
 * 设计要点
 * ========
 * 1. 状态结构 s_state 唯一拥有者是本文件；外部通过 bb_state_get() 拿快照。
 * 2. dispatch() 通过 lv_async_call 切到 LVGL 任务执行 dispatch_on_lvgl()，
 *    保证所有状态变更串行化。
 * 3. 转换由两层组成：
 *    - 转换表 (k_transitions[]) 处理"显式"页面/agent/ptt 转换
 *    - 副作用块（dispatch_on_lvgl 内）处理累积字段（turn_id、request_id、
 *      session_id、driver_name、tts_in_flight 等）
 * 4. 不变量在每次状态变更后跑；违反只 WARN 不 abort（生产构建保留）。
 * 5. 监听器最多 8 个；在 LVGL 任务上回调，listener 内部不可阻塞。
 */

#include "bb_state.h"
#include "bb_state_log.h"

#include <string.h>
#include <stdio.h>
#include <inttypes.h>

#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "freertos/portmacro.h"
#include "esp_attr.h"
#include "esp_log.h"
#include "esp_heap_caps.h"
#include "esp_lvgl_port.h"
#include "lvgl.h"

#include "bb_time.h"
#include "bb_transport.h"

static const char* TAG = "bb_state";

/* ================ 状态名称表 ================ */

static const char* k_page_names[] = {
  [BB_PAGE_LOCKED]   = "LOCKED",
  [BB_PAGE_CHAT]     = "CHAT",
  [BB_PAGE_SETTINGS] = "SETTINGS",
};

static const char* k_agent_names[] = {
  [BB_AGENT_STATE_SLEEP]     = "SLEEP",
  [BB_AGENT_STATE_IDLE]      = "IDLE",
  [BB_AGENT_STATE_BUSY]      = "BUSY",
  [BB_AGENT_STATE_ATTENTION] = "ATTN",
  [BB_AGENT_STATE_CELEBRATE] = "CELEB",
  [BB_AGENT_STATE_DIZZY]     = "DIZZY",
  [BB_AGENT_STATE_HEART]     = "HEART",
  [BB_AGENT_STATE_LISTENING] = "LISTEN",
  [BB_AGENT_STATE_SPEAKING]  = "SPEAK",
};

static const char* k_ptt_names[] = {
  [BB_PTT_IDLE]            = "IDLE",
  [BB_PTT_ARMED]           = "ARMED",
  [BB_PTT_STREAMING]       = "STREAM",
  [BB_PTT_RELEASED_WAIT]   = "WAIT",
};

static const char* k_net_names[] = {
  [BB_NET_OFFLINE]   = "OFFLINE",
  [BB_NET_LOCAL]     = "LOCAL",
  [BB_NET_CLOUD]     = "CLOUD",
  [BB_NET_DEGRADED]  = "DEGRADED",
};

static const char* k_event_names[BB_EVT__COUNT] = {
  [BB_EVT_PTT_DOWN]               = "PTT_DOWN",
  [BB_EVT_PTT_UP]                 = "PTT_UP",
  [BB_EVT_NAV_UP]                 = "NAV_UP",
  [BB_EVT_NAV_DOWN]               = "NAV_DOWN",
  [BB_EVT_NAV_LEFT]               = "NAV_LEFT",
  [BB_EVT_NAV_RIGHT]              = "NAV_RIGHT",
  [BB_EVT_NAV_OK]                 = "NAV_OK",
  [BB_EVT_NAV_BACK]               = "NAV_BACK",
  [BB_EVT_AUDIO_VAD_START]        = "VAD_START",
  [BB_EVT_AUDIO_TX_STOPPED]       = "TX_STOPPED",
  [BB_EVT_ASR_RESULT]             = "ASR_RESULT",
  [BB_EVT_ASR_ERROR]              = "ASR_ERROR",
  [BB_EVT_AGENT_SESSION]          = "AGENT_SESS",
  [BB_EVT_AGENT_TEXT]             = "AGENT_TEXT",
  [BB_EVT_AGENT_TURN_END]         = "AGENT_END",
  [BB_EVT_AGENT_ERROR]            = "AGENT_ERR",
  [BB_EVT_TTS_START]              = "TTS_START",
  [BB_EVT_TTS_DONE]               = "TTS_DONE",
  [BB_EVT_TTS_CANCELLED]          = "TTS_CANCEL",
  [BB_EVT_NET_UP]                 = "NET_UP",
  [BB_EVT_NET_DOWN]               = "NET_DOWN",
  [BB_EVT_NET_DEGRADED]           = "NET_DEGRADED",
  [BB_EVT_REQUEST_SETTINGS_ENTER] = "REQ_SETTINGS",
  [BB_EVT_REQUEST_SETTINGS_EXIT]  = "EXIT_SETTINGS",
  [BB_EVT_VOICE_VERIFY_OK]        = "VERIFY_OK",
  [BB_EVT_VOICE_VERIFY_FAIL]      = "VERIFY_FAIL",
  [BB_EVT_DRIVER_CYCLE]           = "DRV_CYCLE",
  [BB_EVT_DRIVER_NAME_UPDATE]     = "DRV_NAME",
  [BB_EVT_FORCE_AGENT_STATE]      = "FORCE_AGENT",
  [BB_EVT_FORCE_PTT_PHASE]        = "FORCE_PTT",
  [BB_EVT_LVGL_LOCK_TIMEOUT]      = "LVGL_TO",
  [BB_EVT_TIMER_TICK]             = "TICK",
};

const char* bb_page_name(bb_page_t p) {
  if ((unsigned)p < sizeof(k_page_names)/sizeof(k_page_names[0]) && k_page_names[p])
    return k_page_names[p];
  return "?";
}
const char* bb_agent_state_name(bb_agent_state_t s) {
  if ((unsigned)s < sizeof(k_agent_names)/sizeof(k_agent_names[0]) && k_agent_names[s])
    return k_agent_names[s];
  return "?";
}
const char* bb_ptt_phase_name(bb_ptt_phase_t p) {
  if ((unsigned)p < sizeof(k_ptt_names)/sizeof(k_ptt_names[0]) && k_ptt_names[p])
    return k_ptt_names[p];
  return "?";
}
const char* bb_net_name(bb_net_t n) {
  if ((unsigned)n < sizeof(k_net_names)/sizeof(k_net_names[0]) && k_net_names[n])
    return k_net_names[n];
  return "?";
}
const char* bb_event_name(bb_event_t e) {
  if ((unsigned)e < BB_EVT__COUNT && k_event_names[e])
    return k_event_names[e];
  return "?";
}

/* ================ 全局状态 + sequence lock ================ */

/* Sequence lock: 写者递增 seq，读者重试直到 seq 稳定且为偶数。
 * 写者只能在 LVGL 任务上（dispatch_on_lvgl 内），所以只有一个写者。
 * 读者可在任意线程；写并发时短暂自旋，无锁。 */
static portMUX_TYPE s_seq_lock = portMUX_INITIALIZER_UNLOCKED;
static volatile uint32_t s_seq = 0;
static bb_state_t s_state;
static bool s_initialized = false;

#define BB_STATE_LISTENER_MAX 8
static bb_state_listener_t s_listeners[BB_STATE_LISTENER_MAX];
static int s_listener_count = 0;

/* ================ 不变量检查 ================ */

int bb_state_check_invariants(const bb_state_t* st) {
  int violations = 0;
  if (!st) return 0;

  /* INV_1: page == SETTINGS ⇒ ptt == IDLE && agent ∉ {LISTENING, SPEAKING} */
  if (st->page == BB_PAGE_SETTINGS) {
    if (st->ptt != BB_PTT_IDLE) {
      BB_LOG_INVARIANT("INV_1_settings_ptt_active", st);
      violations++;
    }
    if (st->agent == BB_AGENT_STATE_LISTENING || st->agent == BB_AGENT_STATE_SPEAKING) {
      BB_LOG_INVARIANT("INV_1_settings_agent_busy", st);
      violations++;
    }
  }

  /* INV_2: page == LOCKED ⇒ agent ∈ {SLEEP, IDLE, BUSY} */
  if (st->page == BB_PAGE_LOCKED) {
    if (st->agent != BB_AGENT_STATE_SLEEP &&
        st->agent != BB_AGENT_STATE_IDLE &&
        st->agent != BB_AGENT_STATE_BUSY) {
      BB_LOG_INVARIANT("INV_2_locked_agent_unexpected", st);
      violations++;
    }
  }

  /* INV_3: agent == LISTENING ⇒ ptt ∈ {ARMED, STREAMING} && page == CHAT */
  if (st->agent == BB_AGENT_STATE_LISTENING) {
    if (st->ptt != BB_PTT_ARMED && st->ptt != BB_PTT_STREAMING) {
      BB_LOG_INVARIANT("INV_3_listening_ptt_idle", st);
      violations++;
    }
    if (st->page != BB_PAGE_CHAT) {
      BB_LOG_INVARIANT("INV_3_listening_not_chat", st);
      violations++;
    }
  }

  /* INV_4: agent == SPEAKING ⇒ tts_in_flight == true */
  if (st->agent == BB_AGENT_STATE_SPEAKING && !st->tts_in_flight) {
    BB_LOG_INVARIANT("INV_4_speaking_no_tts", st);
    violations++;
  }

  /* INV_5: ptt == STREAMING ⇒ agent == LISTENING */
  if (st->ptt == BB_PTT_STREAMING && st->agent != BB_AGENT_STATE_LISTENING) {
    BB_LOG_INVARIANT("INV_5_stream_not_listen", st);
    violations++;
  }

  /* INV_6: agent_in_flight ⇒ request_id_in_flight != 0 */
  if (st->agent_in_flight && st->request_id_in_flight == 0) {
    BB_LOG_INVARIANT("INV_6_inflight_no_req", st);
    violations++;
  }

  /* INV_7: page state 与 transport profile 一致性
   *   不强检查（cloud_saas 也可能是 CHAT，密语关闭时直入 CHAT），仅观察 */

  return violations;
}

/* ================ 转换表 ================ */

typedef struct {
  bb_page_t        from_page;        /* (bb_page_t)-1 = wildcard */
  bb_agent_state_t from_agent;       /* (bb_agent_state_t)-1 = wildcard */
  bb_event_t       event;
  bb_page_t        to_page;          /* (bb_page_t)-1 = unchanged */
  bb_agent_state_t to_agent;         /* (bb_agent_state_t)-1 = unchanged */
  bb_ptt_phase_t   to_ptt;           /* (bb_ptt_phase_t)-1 = unchanged */
  const char*      label;
} bb_transition_t;

#define ANY_PAGE   ((bb_page_t)-1)
#define ANY_AGENT  ((bb_agent_state_t)-1)
#define KEEP_PAGE  ((bb_page_t)-1)
#define KEEP_AGENT ((bb_agent_state_t)-1)
#define KEEP_PTT   ((bb_ptt_phase_t)-1)

static const bb_transition_t k_transitions[] = {
  /* ===== Page transitions ===== */
  /* LOCKED → CHAT 由密语解锁 */
  { BB_PAGE_LOCKED, ANY_AGENT, BB_EVT_VOICE_VERIFY_OK,
    BB_PAGE_CHAT, BB_AGENT_STATE_IDLE, BB_PTT_IDLE, "unlock" },

  /* CHAT → SETTINGS */
  { BB_PAGE_CHAT, ANY_AGENT, BB_EVT_REQUEST_SETTINGS_ENTER,
    BB_PAGE_SETTINGS, KEEP_AGENT, BB_PTT_IDLE, "enter_settings" },

  /* SETTINGS → CHAT (back / 显式退出) */
  { BB_PAGE_SETTINGS, ANY_AGENT, BB_EVT_NAV_BACK,
    BB_PAGE_CHAT, KEEP_AGENT, KEEP_PTT, "back_to_chat" },
  { BB_PAGE_SETTINGS, ANY_AGENT, BB_EVT_REQUEST_SETTINGS_EXIT,
    BB_PAGE_CHAT, KEEP_AGENT, KEEP_PTT, "exit_settings" },

  /* ===== PTT lifecycle (only valid in CHAT) ===== */
  { BB_PAGE_CHAT, ANY_AGENT, BB_EVT_PTT_DOWN,
    KEEP_PAGE, BB_AGENT_STATE_LISTENING, BB_PTT_ARMED, "ptt_down" },

  { BB_PAGE_CHAT, BB_AGENT_STATE_LISTENING, BB_EVT_AUDIO_VAD_START,
    KEEP_PAGE, KEEP_AGENT, BB_PTT_STREAMING, "vad_start" },

  { BB_PAGE_CHAT, BB_AGENT_STATE_LISTENING, BB_EVT_PTT_UP,
    KEEP_PAGE, BB_AGENT_STATE_BUSY, BB_PTT_RELEASED_WAIT, "ptt_up" },

  /* PTT-down on LOCKED 走密语流程；副作用 hook 处理 voice capture 启动，
   * 这里只是允许进入 ARMED 暂存（agent 保持 BUSY/SLEEP，不进 LISTENING）。 */
  { BB_PAGE_LOCKED, ANY_AGENT, BB_EVT_PTT_DOWN,
    KEEP_PAGE, KEEP_AGENT, BB_PTT_ARMED, "ptt_down_unlock" },
  { BB_PAGE_LOCKED, ANY_AGENT, BB_EVT_PTT_UP,
    KEEP_PAGE, KEEP_AGENT, BB_PTT_RELEASED_WAIT, "ptt_up_unlock" },

  /* ===== ASR (PTT_RELEASED_WAIT → IDLE 等待) ===== */
  { ANY_PAGE, BB_AGENT_STATE_BUSY, BB_EVT_ASR_RESULT,
    KEEP_PAGE, KEEP_AGENT, BB_PTT_IDLE, "asr_done" },
  { ANY_PAGE, ANY_AGENT, BB_EVT_ASR_ERROR,
    KEEP_PAGE, BB_AGENT_STATE_DIZZY, BB_PTT_IDLE, "asr_error" },

  /* ===== Agent stream ===== */
  { ANY_PAGE, ANY_AGENT, BB_EVT_AGENT_SESSION,
    KEEP_PAGE, BB_AGENT_STATE_BUSY, KEEP_PTT, "agent_session" },
  /* AGENT_TEXT 不强制改 agent state — 保持 BUSY 即可（副作用块刷 saw_text 标志） */
  { ANY_PAGE, ANY_AGENT, BB_EVT_AGENT_ERROR,
    KEEP_PAGE, BB_AGENT_STATE_DIZZY, KEEP_PTT, "agent_error" },

  /* ===== TTS ===== */
  { ANY_PAGE, ANY_AGENT, BB_EVT_TTS_START,
    KEEP_PAGE, BB_AGENT_STATE_SPEAKING, KEEP_PTT, "tts_start" },
  /* TTS_DONE / TTS_CANCELLED 由副作用块计算最终 agent state（HEART vs IDLE） */

  /* ===== 密语解锁失败 ===== */
  { BB_PAGE_LOCKED, ANY_AGENT, BB_EVT_VOICE_VERIFY_FAIL,
    KEEP_PAGE, BB_AGENT_STATE_DIZZY, BB_PTT_IDLE, "verify_fail" },
};

#define BB_TRANSITION_COUNT (sizeof(k_transitions) / sizeof(k_transitions[0]))

/**
 * 在转换表中查找匹配项；找到返回指针，未找到返回 NULL。
 * 优先精确匹配 (page+agent)，再退到 wildcard。
 */
static const bb_transition_t* find_transition(const bb_state_t* st, bb_event_t e) {
  const bb_transition_t* best = NULL;
  int best_specificity = -1;
  for (size_t i = 0; i < BB_TRANSITION_COUNT; i++) {
    const bb_transition_t* t = &k_transitions[i];
    if (t->event != e) continue;
    if (t->from_page != ANY_PAGE && t->from_page != st->page) continue;
    if (t->from_agent != ANY_AGENT && t->from_agent != st->agent) continue;
    int spec = (t->from_page != ANY_PAGE ? 2 : 0) + (t->from_agent != ANY_AGENT ? 1 : 0);
    if (spec > best_specificity) {
      best = t;
      best_specificity = spec;
    }
  }
  return best;
}

/* ================ Listener ================ */

int bb_state_subscribe(bb_state_listener_t fn) {
  if (!fn) return -1;
  for (int i = 0; i < s_listener_count; i++) {
    if (s_listeners[i] == fn) return 0;
  }
  if (s_listener_count >= BB_STATE_LISTENER_MAX) {
    ESP_LOGE(TAG, "subscribe: listener slots full (%d)", BB_STATE_LISTENER_MAX);
    return -1;
  }
  s_listeners[s_listener_count++] = fn;
  return 0;
}

static void notify_listeners(const bb_state_t* prev, const bb_state_t* next,
                             const bb_event_payload_t* evt) {
  for (int i = 0; i < s_listener_count; i++) {
    if (s_listeners[i]) s_listeners[i](prev, next, evt);
  }
}

/* ================ 副作用块 (in dispatch_on_lvgl) ================ */

/* 处理累积字段：turn_id / request_id / session_id / driver_name /
 * tts_in_flight / agent_in_flight / adapter_offline / lvgl_lock_failures */
static void apply_side_effects(bb_state_t* st, const bb_event_payload_t* evt) {
  switch (evt->type) {
    case BB_EVT_PTT_DOWN:
      /* 进入新 turn — 仅在能进 ARMED 时；转换表已经判定，到这里说明转换通过 */
      if (st->ptt == BB_PTT_ARMED && st->page == BB_PAGE_CHAT) {
        st->turn_id++;
        st->turn_start_ms = (uint64_t)bb_now_ms();
      }
      break;

    case BB_EVT_AGENT_SESSION:
      /* payload.text = session_id；同时记 request_id_in_flight */
      if (evt->text[0]) {
        strncpy(st->session_id, evt->text, sizeof(st->session_id) - 1);
        st->session_id[sizeof(st->session_id) - 1] = '\0';
      }
      if (evt->request_id != 0) {
        st->request_id_in_flight = evt->request_id;
      }
      st->agent_in_flight = true;
      break;

    case BB_EVT_AGENT_TURN_END:
      st->agent_in_flight = false;
      st->request_id_in_flight = 0;
      break;

    case BB_EVT_AGENT_ERROR:
      st->agent_in_flight = false;
      st->request_id_in_flight = 0;
      break;

    case BB_EVT_TTS_START:
      st->tts_in_flight = true;
      break;

    case BB_EVT_TTS_DONE:
    case BB_EVT_TTS_CANCELLED: {
      st->tts_in_flight = false;
      /* 计算最终 agent state：fast (<5s) → HEART，否则 IDLE */
      uint64_t now = (uint64_t)bb_now_ms();
      uint64_t elapsed = (st->turn_start_ms && now > st->turn_start_ms)
                          ? (now - st->turn_start_ms) : UINT64_MAX;
      st->agent = (elapsed < 5000) ? BB_AGENT_STATE_HEART : BB_AGENT_STATE_IDLE;
      break;
    }

    case BB_EVT_DRIVER_CYCLE:
      st->request_id++;
      st->request_id_in_flight = 0;
      st->agent_in_flight = false;
      st->tts_in_flight = false;
      st->session_id[0] = '\0';
      /* driver_name 由 BB_EVT_DRIVER_NAME_UPDATE 在落地后回调更新 */
      break;

    case BB_EVT_DRIVER_NAME_UPDATE:
      if (evt->text[0]) {
        strncpy(st->driver_name, evt->text, sizeof(st->driver_name) - 1);
        st->driver_name[sizeof(st->driver_name) - 1] = '\0';
      }
      break;

    case BB_EVT_FORCE_AGENT_STATE:
      /* 迁移期兼容：旧 post_state 调用方直接指定 agent 状态。
       * 不变量在 dispatch 末尾会自动检查，违规会 WARN（不阻断）。 */
      if (evt->error_code >= 0 && evt->error_code <= BB_AGENT_STATE_SPEAKING) {
        st->agent = (bb_agent_state_t)evt->error_code;
        if (st->agent == BB_AGENT_STATE_SPEAKING) st->tts_in_flight = true;
        if (st->agent == BB_AGENT_STATE_BUSY) st->agent_in_flight = true;
        if (st->agent == BB_AGENT_STATE_IDLE || st->agent == BB_AGENT_STATE_HEART) {
          st->agent_in_flight = false;
          st->tts_in_flight = false;
        }
      }
      break;

    case BB_EVT_FORCE_PTT_PHASE:
      if (evt->error_code >= 0 && evt->error_code <= BB_PTT_RELEASED_WAIT) {
        st->ptt = (bb_ptt_phase_t)evt->error_code;
      }
      break;

    case BB_EVT_NET_UP:
      /* error_code 复用为 bb_net_t */
      if (evt->error_code == BB_NET_LOCAL || evt->error_code == BB_NET_CLOUD) {
        st->net = (bb_net_t)evt->error_code;
      } else {
        st->net = BB_NET_LOCAL;
      }
      st->adapter_offline = false;
      break;

    case BB_EVT_NET_DOWN:
      st->net = BB_NET_OFFLINE;
      st->adapter_offline = true;
      break;

    case BB_EVT_NET_DEGRADED:
      st->net = BB_NET_DEGRADED;
      break;

    case BB_EVT_LVGL_LOCK_TIMEOUT:
      st->lvgl_lock_failures++;
      break;

    case BB_EVT_ASR_RESULT:
      /* PTT 流结束 + ASR 完成；agent_in_flight 已在 send_message 时置位 */
      break;

    default:
      break;
  }
}

/* ================ Dispatch 核心 ================ */

/* 判断事件是否应被忽略；返回非 NULL 表示 reason，事件被 DROP */
static const char* should_drop(const bb_state_t* st, const bb_event_payload_t* evt) {
  /* PTT_DOWN 在 SETTINGS 页：永远丢弃 */
  if (evt->type == BB_EVT_PTT_DOWN && st->page == BB_PAGE_SETTINGS) {
    return "page_settings";
  }
  /* PTT_DOWN 在 CHAT 但 net offline：丢弃 */
  if (evt->type == BB_EVT_PTT_DOWN && st->page == BB_PAGE_CHAT &&
      (st->net == BB_NET_OFFLINE || st->adapter_offline)) {
    return "net_offline";
  }
  /* PTT_DOWN 在 CHAT 但 agent 在飞 / TTS 在播：允许，会触发 cancel-and-replace
   * （cancel 由副作用 hook 处理） — 不丢弃 */

  /* 过期 request_id */
  if (st->request_id_in_flight != 0 &&
      evt->request_id != 0 &&
      evt->request_id != st->request_id_in_flight &&
      (evt->type == BB_EVT_AGENT_SESSION ||
       evt->type == BB_EVT_AGENT_TEXT ||
       evt->type == BB_EVT_AGENT_TURN_END)) {
    return "stale_request";
  }

  /* AGENT_SESSION 携带新 request_id 但当前没有 in_flight：忽略 */
  /* (此情况 drv cycle 之后老 driver 的尾响应) */
  if (evt->type == BB_EVT_AGENT_SESSION &&
      st->request_id_in_flight == 0 &&
      evt->request_id != 0 &&
      evt->request_id < st->request_id) {
    return "stale_request_no_inflight";
  }

  return NULL;
}

/* 真正的 dispatch — 在 LVGL 任务上执行 */
static void dispatch_on_lvgl(void* user_data) {
  bb_event_payload_t* evt = (bb_event_payload_t*)user_data;
  if (!evt) return;
  if (!s_initialized) {
    free(evt);
    return;
  }

  bb_state_t prev = s_state;
  bb_state_t next = prev;

  /* 1) 检查是否丢弃 */
  const char* drop_reason = should_drop(&prev, evt);
  if (drop_reason) {
    next.dropped_events++;
    next.last_event_ms = (uint64_t)bb_now_ms();
    /* 写回（仅 dropped_events / last_event_ms 变化）*/
    taskENTER_CRITICAL(&s_seq_lock);
    s_seq++;
    s_state = next;
    s_seq++;
    taskEXIT_CRITICAL(&s_seq_lock);
    BB_LOG_DROPPED(&prev, evt, drop_reason);
    free(evt);
    return;
  }

  /* 2) 查转换表 */
  const bb_transition_t* t = find_transition(&prev, evt->type);
  if (t) {
    if (t->to_page != KEEP_PAGE) next.page = t->to_page;
    if (t->to_agent != KEEP_AGENT) next.agent = t->to_agent;
    if (t->to_ptt != KEEP_PTT) next.ptt = t->to_ptt;
  }

  /* 3) 副作用：累积字段 */
  apply_side_effects(&next, evt);
  next.last_event_ms = (uint64_t)bb_now_ms();

  /* 4) 写回（sequence lock）*/
  taskENTER_CRITICAL(&s_seq_lock);
  s_seq++;
  s_state = next;
  s_seq++;
  taskEXIT_CRITICAL(&s_seq_lock);

  /* 5) 日志 + 不变量 + 通知 listener */
  BB_LOG_DISPATCH(&prev, &next, evt);
  bb_state_check_invariants(&next);
  notify_listeners(&prev, &next, evt);

  free(evt);
}

void bb_state_dispatch(bb_event_payload_t evt) {
  if (!s_initialized) {
    /* init 前的事件丢弃但记 ERROR */
    ESP_LOGE(TAG, "dispatch before init: evt=%s", bb_event_name(evt.type));
    return;
  }
  /* 拷一份到堆，因为 lv_async_call 在 LVGL 任务上异步处理 */
  bb_event_payload_t* heap_evt = (bb_event_payload_t*)malloc(sizeof(*heap_evt));
  if (!heap_evt) {
    ESP_LOGE(TAG, "dispatch: malloc failed (evt=%s)", bb_event_name(evt.type));
    return;
  }
  *heap_evt = evt;
  /* LVGL 配 LV_OS_NONE，跨任务调 lv_async_call 必须先持 lvgl_port 锁，
   * 否则会和 lv_timer_handler 抢 TLSF 堆导致破坏（参考 bb_ui_agent_chat.c
   * 的 safe_lv_async_call 注释）。 */
  if (!lvgl_port_lock(pdMS_TO_TICKS(200))) {
    ESP_LOGW(TAG, "dispatch: lvgl_port_lock timeout (evt=%s) — DROPPED",
             bb_event_name(evt.type));
    free(heap_evt);
    /* 记一笔丢弃但走不到 dropped_events 因为没进队列 — 用 ESP_LOG 标识即可 */
    return;
  }
  lv_result_t r = lv_async_call(dispatch_on_lvgl, heap_evt);
  lvgl_port_unlock();
  if (r != LV_RESULT_OK) {
    ESP_LOGE(TAG, "dispatch: lv_async_call failed (evt=%s)", bb_event_name(evt.type));
    free(heap_evt);
  }
}

void bb_state_dispatch_simple(bb_event_t type) {
  bb_event_payload_t evt = (bb_event_payload_t){ .type = type };
  bb_state_dispatch(evt);
}

/* ================ Snapshot 读 (sequence lock) ================ */

bb_state_t bb_state_get(void) {
  bb_state_t snap;
  if (!s_initialized) {
    memset(&snap, 0, sizeof(snap));
    snap.page = BB_PAGE_CHAT;
    snap.agent = BB_AGENT_STATE_SLEEP;
    snap.ptt = BB_PTT_IDLE;
    snap.net = BB_NET_OFFLINE;
    return snap;
  }
  uint32_t s1 = 0, s2 = 0;
  do {
    s1 = s_seq;
    /* 写者持有 seq lock 时 s_seq 是奇数；等到偶数再读 */
    if (s1 & 1) {
      vTaskDelay(1);
      s2 = s1 + 1;   /* 强制下一轮重读 */
      continue;
    }
    __sync_synchronize();   /* 屏障：保证读 s_state 不被重排到 s1 之前 */
    snap = s_state;
    __sync_synchronize();
    s2 = s_seq;
  } while (s1 != s2 || (s1 & 1));
  return snap;
}

/* ================ Init ================ */

void bb_state_init(void) {
  if (s_initialized) return;

  memset(&s_state, 0, sizeof(s_state));

  /* 初始 page 由 transport profile 决定 */
  if (bb_transport_is_cloud_saas()) {
    /* 注：是否真的进 LOCKED 还要看 miyu_enabled，由 bb_radio_app_start 在
     * 调 bb_state_init() 后用 BB_EVT_VOICE_VERIFY_OK 或显式 bb_state_dispatch
     * 切到 CHAT。这里先按"最严格"初始化为 LOCKED，后续 app 启动逻辑可改。 */
    s_state.page = BB_PAGE_LOCKED;
  } else {
    s_state.page = BB_PAGE_CHAT;
  }
  s_state.agent = BB_AGENT_STATE_SLEEP;  /* adapter 未连前默认 SLEEP */
  s_state.ptt = BB_PTT_IDLE;
  s_state.net = BB_NET_OFFLINE;
  s_state.turn_id = 0;
  s_state.request_id = 0;
  s_state.request_id_in_flight = 0;
  s_state.session_id[0] = '\0';
  s_state.driver_name[0] = '\0';
  s_state.turn_start_ms = 0;
  s_state.last_event_ms = (uint64_t)bb_now_ms();
  s_state.tts_in_flight = false;
  s_state.agent_in_flight = false;
  s_state.adapter_offline = false;
  s_state.lvgl_lock_failures = 0;
  s_state.dropped_events = 0;

  s_initialized = true;
  ESP_LOGI(TAG, "init: page=%s agent=%s net=%s transport=%s",
           bb_page_name(s_state.page),
           bb_agent_state_name(s_state.agent),
           bb_net_name(s_state.net),
           bb_transport_profile_name());
}
