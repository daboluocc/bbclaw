#pragma once

/**
 * bb_state 结构化日志宏
 *
 * 统一 TAG="bb_state"，所有字段 key=value，空格分隔，便于 grep。
 * 字段顺序约定：turn -> req -> evt -> page -> agent -> ptt -> net ->
 *               drv -> sess -> ms -> [action/reason/extras]
 *
 * 使用：
 *   BB_LOG_DISPATCH(prev, next, evt);          // 状态变更
 *   BB_LOG_DROPPED(prev, evt, reason);         // 事件被忽略
 *   BB_LOG_INVARIANT(name, st);                // 不变量违反
 *
 * 不要在这里包含 esp_log.h 的实现，调用者自己包含。
 */

#include "esp_log.h"
#include "bb_state.h"

#ifdef __cplusplus
extern "C" {
#endif

#define BB_STATE_LOG_TAG "bb_state"

/* session_id 短形式（前 8 位），缺省 "-" */
static inline const char* bb_state_log_session_short(const bb_state_t* st, char buf[12]) {
  if (!st || st->session_id[0] == '\0') {
    buf[0] = '-'; buf[1] = '\0';
    return buf;
  }
  size_t n = 0;
  while (n < 8 && st->session_id[n] != '\0') {
    buf[n] = st->session_id[n];
    n++;
  }
  buf[n] = '\0';
  return buf;
}

static inline const char* bb_state_log_driver(const bb_state_t* st) {
  return (st && st->driver_name[0]) ? st->driver_name : "-";
}

/* 状态变更日志 — 当 prev->X != next->X 时输出 X→Y，否则 X */
#define BB_LOG_DISPATCH(prev, next, evt_payload)                                          \
  do {                                                                                    \
    char _sess_buf[12];                                                                   \
    const bb_state_t* _p = (prev);                                                        \
    const bb_state_t* _n = (next);                                                        \
    const bb_event_payload_t* _e = (evt_payload);                                         \
    if (_p->page != _n->page || _p->agent != _n->agent || _p->ptt != _n->ptt) {           \
      ESP_LOGI(BB_STATE_LOG_TAG,                                                          \
        "turn=%lu req=%lu evt=%s page=%s%s%s agent=%s%s%s ptt=%s%s%s net=%s drv=%s "      \
        "sess=%s ms=%llu",                                                                \
        (unsigned long)_n->turn_id, (unsigned long)_n->request_id,                        \
        bb_event_name(_e->type),                                                          \
        bb_page_name(_p->page),                                                           \
        (_p->page != _n->page) ? "→" : "",                                                \
        (_p->page != _n->page) ? bb_page_name(_n->page) : "",                             \
        bb_agent_state_name(_p->agent),                                                   \
        (_p->agent != _n->agent) ? "→" : "",                                              \
        (_p->agent != _n->agent) ? bb_agent_state_name(_n->agent) : "",                   \
        bb_ptt_phase_name(_p->ptt),                                                       \
        (_p->ptt != _n->ptt) ? "→" : "",                                                  \
        (_p->ptt != _n->ptt) ? bb_ptt_phase_name(_n->ptt) : "",                           \
        bb_net_name(_n->net), bb_state_log_driver(_n),                                    \
        bb_state_log_session_short(_n, _sess_buf),                                        \
        (unsigned long long)_n->last_event_ms);                                           \
    } else {                                                                              \
      ESP_LOGD(BB_STATE_LOG_TAG,                                                          \
        "turn=%lu req=%lu evt=%s page=%s agent=%s ptt=%s net=%s drv=%s sess=%s ms=%llu",  \
        (unsigned long)_n->turn_id, (unsigned long)_n->request_id,                        \
        bb_event_name(_e->type),                                                          \
        bb_page_name(_n->page), bb_agent_state_name(_n->agent),                           \
        bb_ptt_phase_name(_n->ptt), bb_net_name(_n->net),                                 \
        bb_state_log_driver(_n),                                                          \
        bb_state_log_session_short(_n, _sess_buf),                                        \
        (unsigned long long)_n->last_event_ms);                                           \
    }                                                                                     \
  } while (0)

/* 事件被忽略的日志 — 强制 WARN，必须含 reason */
#define BB_LOG_DROPPED(state, evt_payload, reason_str)                                    \
  do {                                                                                    \
    char _sess_buf[12];                                                                   \
    const bb_state_t* _s = (state);                                                       \
    const bb_event_payload_t* _e = (evt_payload);                                         \
    ESP_LOGW(BB_STATE_LOG_TAG,                                                            \
      "turn=%lu req=%lu evt=%s page=%s agent=%s ptt=%s net=%s drv=%s sess=%s ms=%llu "    \
      "action=DROPPED reason=%s",                                                         \
      (unsigned long)_s->turn_id, (unsigned long)_s->request_id,                          \
      bb_event_name(_e->type),                                                            \
      bb_page_name(_s->page), bb_agent_state_name(_s->agent),                             \
      bb_ptt_phase_name(_s->ptt), bb_net_name(_s->net),                                   \
      bb_state_log_driver(_s),                                                            \
      bb_state_log_session_short(_s, _sess_buf),                                          \
      (unsigned long long)_s->last_event_ms, (reason_str));                               \
  } while (0)

/* 不变量违反日志 */
#define BB_LOG_INVARIANT(inv_name, state)                                                 \
  do {                                                                                    \
    char _sess_buf[12];                                                                   \
    const bb_state_t* _s = (state);                                                       \
    ESP_LOGW(BB_STATE_LOG_TAG,                                                            \
      "INVARIANT_FAIL=%s turn=%lu page=%s agent=%s ptt=%s net=%s tts_inflight=%d "        \
      "agent_inflight=%d sess=%s",                                                        \
      (inv_name), (unsigned long)_s->turn_id,                                             \
      bb_page_name(_s->page), bb_agent_state_name(_s->agent),                             \
      bb_ptt_phase_name(_s->ptt), bb_net_name(_s->net),                                   \
      (int)_s->tts_in_flight, (int)_s->agent_in_flight,                                   \
      bb_state_log_session_short(_s, _sess_buf));                                         \
  } while (0)

#ifdef __cplusplus
}
#endif
