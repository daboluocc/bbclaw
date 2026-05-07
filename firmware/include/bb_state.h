#pragma once

/**
 * bb_state — 全局状态协调器（Single Source of Truth）
 *
 * 设计目标
 * ========
 * 取代固件中分散在多个模块的并行状态变量：
 *   - bb_radio_app.c: s_app_state (page) / s_agent_chat_active / s_settings_active
 *   - bb_ui_agent_chat.c: s_chat.state (agent FSM)
 *   - bb_radio_app.c 主循环: arming / streaming / s_cloud_wait_busy
 *   - bb_audio.c TTS 任务: tts_cancel_requested
 * 把它们合并到一个 bb_state_t 结构，所有变更走唯一入口
 * bb_state_dispatch()，所有读取走 bb_state_get() 拿快照。
 *
 * 使用方式
 * ========
 *   生产事件方（PTT 中断、nav 回调、网络回调、ASR/agent 流事件等）：
 *     bb_state_dispatch((bb_event_payload_t){ .type = BB_EVT_PTT_DOWN });
 *
 *   消费状态方（buddy 主题、overlay 渲染、LED、日志）：
 *     bb_state_t st = bb_state_get();   // 读快照
 *     // 或注册 listener 自动响应变更：
 *     bb_state_subscribe(my_listener);
 *
 * 线程模型
 * ========
 * dispatch 内部走 lv_async_call，最终在 LVGL 任务上序列化执行。
 * 任何线程调用 bb_state_dispatch() 都是安全的（不会阻塞）。
 * Listener 一律在 LVGL 任务上回调；listener 内部应避免阻塞调用。
 *
 * 日志
 * ====
 * 每次 dispatch 自动打印一行结构化 key=value 日志，TAG="bb_state"。
 * 字段：turn=N req=N evt=NAME page=X[→Y] agent=X[→Y] ptt=X[→Y] net=X
 *       drv=NAME sess=hex8 ms=N [action=DROPPED reason=X]
 * 配合 adapter 的 phase= 字段可拼装出端到端时间线。
 */

#include <stdint.h>
#include <stdbool.h>

#include "bb_agent_theme.h"  /* bb_agent_state_t (9 状态) — 沿用，不重定义 */

#ifdef __cplusplus
extern "C" {
#endif

/* ========== 状态枚举 ========== */

/* 页面状态机：3 状态固定页面菜单（ADR-012）*/
typedef enum {
  BB_PAGE_LOCKED = 0,    /* cloud_saas 启动；密语解锁 */
  BB_PAGE_CHAT,          /* 默认主页；agent 对话 */
  BB_PAGE_SETTINGS,      /* 全屏设置菜单 */
} bb_page_t;

/* PTT 物理 / 流阶段，独立于 agent FSM 的"逻辑"状态 */
typedef enum {
  BB_PTT_IDLE = 0,           /* 未按；mic 关 */
  BB_PTT_ARMED,              /* 已按下；audio_start_tx 已调；等 VAD 触发 */
  BB_PTT_STREAMING,          /* VAD 触发；PCM 流上行 adapter */
  BB_PTT_RELEASED_WAIT,      /* PTT 已松开；等 ASR/cloud 返回 */
} bb_ptt_phase_t;

/* 网络可达性 */
typedef enum {
  BB_NET_OFFLINE = 0,        /* WiFi 没连或后端不可达 */
  BB_NET_LOCAL,              /* local_home Adapter 可达 */
  BB_NET_CLOUD,              /* cloud_saas WS 已连 */
  BB_NET_DEGRADED,           /* 探测中 / 502 / 慢 */
} bb_net_t;

/* ========== 事件枚举 ========== */

typedef enum {
  /* 输入设备 */
  BB_EVT_PTT_DOWN = 0,
  BB_EVT_PTT_UP,
  BB_EVT_NAV_UP,
  BB_EVT_NAV_DOWN,
  BB_EVT_NAV_LEFT,
  BB_EVT_NAV_RIGHT,
  BB_EVT_NAV_OK,
  BB_EVT_NAV_BACK,

  /* 音频 */
  BB_EVT_AUDIO_VAD_START,        /* VAD 检测到语音起点 */
  BB_EVT_AUDIO_TX_STOPPED,       /* I2S 录音停止完成 */
  BB_EVT_ASR_RESULT,             /* payload.text = 转写结果 */
  BB_EVT_ASR_ERROR,              /* payload.error_code */

  /* Agent stream */
  BB_EVT_AGENT_SESSION,          /* payload.text = session_id, request_id 携带 */
  BB_EVT_AGENT_TEXT,             /* payload.text = chunk */
  BB_EVT_AGENT_TURN_END,
  BB_EVT_AGENT_ERROR,

  /* TTS */
  BB_EVT_TTS_START,
  BB_EVT_TTS_DONE,
  BB_EVT_TTS_CANCELLED,

  /* 网络 */
  BB_EVT_NET_UP,                 /* payload.error_code 复用为 net 模式 (BB_NET_LOCAL / BB_NET_CLOUD) */
  BB_EVT_NET_DOWN,
  BB_EVT_NET_DEGRADED,

  /* 页面切换 */
  BB_EVT_REQUEST_SETTINGS_ENTER,
  BB_EVT_REQUEST_SETTINGS_EXIT,
  BB_EVT_VOICE_VERIFY_OK,
  BB_EVT_VOICE_VERIFY_FAIL,

  /* Driver 切换 */
  BB_EVT_DRIVER_CYCLE,           /* payload.delta = ±1 */
  BB_EVT_DRIVER_NAME_UPDATE,     /* payload.text = 当前 driver 名 */

  /* 迁移期兼容：旧 post_state 直接强写 agent 状态，绕过转换表
   * payload.error_code = (int)bb_agent_state_t；不变量仍然校验 */
  BB_EVT_FORCE_AGENT_STATE,
  BB_EVT_FORCE_PTT_PHASE,        /* payload.error_code = (int)bb_ptt_phase_t */

  /* 自检 / 内部 */
  BB_EVT_LVGL_LOCK_TIMEOUT,      /* subscriber 报告 LVGL 锁超时 */
  BB_EVT_TIMER_TICK,             /* 周期不变量自检 */

  BB_EVT__COUNT,
} bb_event_t;

/* ========== 事件载荷 ========== */

#define BB_EVT_TEXT_MAX 256

typedef struct {
  bb_event_t type;
  uint32_t   request_id;          /* 响应类事件填发起方 request_id；其它事件可填 0 */
  int        delta;               /* NAV_LEFT/RIGHT, DRIVER_CYCLE */
  int        error_code;          /* *_ERROR；NET_UP 时复用为 bb_net_t */
  char       text[BB_EVT_TEXT_MAX]; /* ASR_RESULT / AGENT_TEXT / AGENT_SESSION */
} bb_event_payload_t;

/* ========== 状态快照 ========== */

#define BB_STATE_SESSION_ID_LEN 40
#define BB_STATE_DRIVER_NAME_LEN 24

typedef struct {
  bb_page_t        page;
  bb_agent_state_t agent;             /* 沿用 9 状态 enum */
  bb_ptt_phase_t   ptt;
  bb_net_t         net;

  uint32_t turn_id;                   /* PTT_DOWN 进入 ARMED 时 ++（被吞的 PTT 不消耗） */
  uint32_t request_id;                /* 单调递增，每次 agent_send / driver_cycle ++ */
  uint32_t request_id_in_flight;      /* 当前期望响应的 request_id；0 = 无在飞 */

  char     session_id[BB_STATE_SESSION_ID_LEN];
  char     driver_name[BB_STATE_DRIVER_NAME_LEN];

  uint64_t turn_start_ms;             /* PTT_DOWN 时 bb_now_ms() */
  uint64_t last_event_ms;             /* 最近一次 dispatch 的时间戳 */

  bool     tts_in_flight;
  bool     agent_in_flight;
  bool     adapter_offline;           /* adapter 端 502 标志（local_home 探测） */

  uint32_t lvgl_lock_failures;        /* 累计计数，便于诊断 */
  uint32_t dropped_events;            /* 累计 DROPPED 事件数 */
} bb_state_t;

/* ========== 公共 API ========== */

/**
 * 必须在 LVGL 初始化之后、bb_radio_app_start() 之前调用一次。
 * 设置初始状态（基于 transport profile 决定 page=LOCKED/CHAT，net=OFFLINE）。
 */
void bb_state_init(void);

/**
 * 唯一事件入口；任意线程任意上下文调用都安全。
 * 内部走 lv_async_call，dispatch 在 LVGL 任务上序列化执行。
 *
 * 同步语义：
 *   - 调用立即返回；事件可能尚未被处理
 *   - 同一线程连续调用，事件按 FIFO 处理
 *   - 不同线程的事件按到达顺序进入队列
 *
 * 失败时（队列满）打 ESP_LOGE 但不阻塞。
 */
void bb_state_dispatch(bb_event_payload_t evt);

/**
 * 简化助手：仅 type，无 payload 的事件。
 */
void bb_state_dispatch_simple(bb_event_t type);

/**
 * 拷贝当前状态快照；任意线程调用安全。
 * 实现采用 sequence lock，读侧在写并发时可能短暂自旋（µs 级）。
 */
bb_state_t bb_state_get(void);

/**
 * Listener 在 LVGL 任务上回调。prev/next 都是不可变快照拷贝。
 * 若不关心特定字段可忽略。
 */
typedef void (*bb_state_listener_t)(const bb_state_t* prev,
                                    const bb_state_t* next,
                                    const bb_event_payload_t* evt);

/**
 * 注册 listener；内部固定容量 8 个槽位。同一函数指针重复注册视为 no-op。
 * 返回 0 成功，-1 槽位耗尽。
 */
int bb_state_subscribe(bb_state_listener_t fn);

/* ========== 调试 API ========== */

/** 状态名（不会返回 NULL），用于日志/调试。 */
const char* bb_page_name(bb_page_t p);
const char* bb_agent_state_name(bb_agent_state_t s);
const char* bb_ptt_phase_name(bb_ptt_phase_t p);
const char* bb_net_name(bb_net_t n);
const char* bb_event_name(bb_event_t e);

/**
 * 不变量检查；违反时只打 WARN（不 abort）。
 * 由 dispatch 内部在每次状态变更后自动调用；外部一般不需要直接调。
 * 返回违反的不变量数量（0 = 全部通过）。
 */
int bb_state_check_invariants(const bb_state_t* st);

#ifdef __cplusplus
}
#endif
