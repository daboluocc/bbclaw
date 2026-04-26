#pragma once

#include "esp_err.h"
#include "lvgl.h"

/**
 * Agent Chat 屏幕（Phase 4.1）
 *
 * 职责：
 *   - 维护 transcript 容器 / driver / session / 七态状态机
 *   - 调当前主题（bb_agent_theme_get_active）渲染所有视觉
 *   - 提供 send 接口，内部起 FreeRTOS 任务调用 bb_agent_send_message，
 *     事件回调通过 lv_async_call 切回 LVGL 任务再调主题方法。
 *
 * 注意：本模块不会自己 pop 屏幕；调用方负责 show / hide 的生命期。
 *       Phase 4.2 会接入菜单 hook。
 */

/**
 * 在 parent 上构建 Agent Chat 视图。
 * - 调用当前激活主题的 on_enter(parent)
 * - 主题首次激活时也会被设置为 SLEEP 态，等首条消息切换
 * 多次调用前请先 hide。
 */
void bb_ui_agent_chat_show(lv_obj_t* parent);

/**
 * 拆掉视图：调主题 on_exit，清理任务/异步队列。
 */
void bb_ui_agent_chat_hide(void);

/**
 * 发送一条文本消息。立即返回；实际 HTTP 流式接收在内部任务里跑。
 * 当前如有未结束的发送，本次调用返回 ESP_ERR_INVALID_STATE。
 *
 * @param text  非空字符串；内部会拷贝。
 */
esp_err_t bb_ui_agent_chat_send(const char* text);

/**
 * Phase 4.2 预置短语 picker（Phase 4.7 起：纯短语，不再含 Settings 行）。
 *
 * 在 agent-chat 根容器底部叠一个简单的高亮列表。调用前必须先 _show()。
 * - phrases / count：调用方持有所有权；内部仅引用指针，要求 phrases 在
 *   下次 _picker_show / _hide 之前保持有效（典型用法是 static 数组）。
 * - 多次调用会替换之前的 picker。
 *
 * Settings 已搬到独立 overlay（见 bb_ui_settings.h），从主屏 OK 直接弹。
 * 必须在 LVGL 任务中调用（持有 lvgl 锁的上下文）。
 */
void bb_ui_agent_chat_picker_show(const char* const* phrases, int count);

/** 移动 picker 高亮项；delta = -1 / +1。LVGL 任务中调用。 */
void bb_ui_agent_chat_picker_move(int delta);

/**
 * 发送当前 picker 选中的短语（等价于 send(selected)）。
 * 如有正在发送的请求，返回 ESP_ERR_INVALID_STATE（picker 不会消失，用户可以
 * 在 turn_end 后再按）。
 */
esp_err_t bb_ui_agent_chat_picker_send_selected(void);

/**
 * Phase 5 — quick driver cycle from picker mode (LEFT/RIGHT shortcut).
 *
 * Cycles the active agent driver by `delta` (-1 = previous, +1 = next) without
 * entering Settings. Persists to NVS, refreshes the theme topbar, and shows a
 * brief transient hint so the user knows what they switched to. Wraps around
 * the cached driver list.
 *
 * Cache is lazily populated on first call via the same blocking HTTP path as
 * Settings entry (Phase 4.2.5 will make this async). When the cache is empty
 * and the HTTP fetch fails, this is a no-op (offline fallback only has the
 * single default driver, nothing to cycle through).
 *
 * Must be called inside the LVGL lock and only when chat overlay is active.
 * Returns ESP_OK on success, ESP_ERR_INVALID_STATE if not in picker mode,
 * ESP_ERR_NOT_FOUND if the cache is empty / single-entry.
 */
esp_err_t bb_ui_agent_chat_cycle_driver(int delta);

/**
 * Phase 4.5 — voice bridge helpers.
 *
 * Used by bb_radio_app.c to drive the PTT-as-text-input flow when the chat
 * overlay is open. Cheap query + visual hints; no agent network traffic.
 */

/**
 * Returns 1 when the chat overlay is currently servicing a turn (the agent
 * task is in flight or replies are still streaming). Used by radio_app to
 * reject concurrent PTT presses.
 */
int bb_ui_agent_chat_is_busy(void);

/**
 * Phase 4.9 — cancel an in-flight agent turn (thinking/streaming).
 *
 * The HTTP stream continues draining in the background, but events are
 * discarded, TTS is killed, and the UI goes back to IDLE immediately.
 * No-op if no turn is in flight.
 */
void bb_ui_agent_chat_cancel(void);

/**
 * Light visual hint that the device is recording PTT for an agent turn.
 * begin=1 → topbar shows "LISTEN…" + state goes LISTENING.
 * begin=0 → restore the cached session id (or clear if none).
 *
 * Safe to call from any task; internally posts via lv_async_call.
 */
void bb_ui_agent_chat_voice_listening(int begin);

/**
 * Transition from LISTENING → BUSY when PTT is released and the device
 * enters the ASR upload / cloud-wait phase. Clears the listening topbar
 * hint and posts BUSY so the buddy shows "thinking..." during the
 * (potentially long) blocking stream-finish HTTP call.
 *
 * Safe to call from any task; internally posts via lv_async_call.
 */
void bb_ui_agent_chat_voice_processing(void);
