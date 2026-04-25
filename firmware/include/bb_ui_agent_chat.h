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
 * Phase 4.2 预置短语 picker。
 *
 * 在 agent-chat 根容器底部叠一个简单的高亮列表。调用前必须先 _show()。
 * - phrases / count：调用方持有所有权；内部仅引用指针，要求 phrases 在
 *   下次 _picker_show / _hide 之前保持有效（典型用法是 static 数组）。
 * - 多次调用会替换之前的 picker。
 *
 * 必须在 LVGL 任务中调用（持有 lvgl 锁的上下文）。
 */
void bb_ui_agent_chat_picker_show(const char* const* phrases, int count);

/** 移动 picker 高亮项；delta = -1 / +1。LVGL 任务中调用。 */
void bb_ui_agent_chat_picker_move(int delta);

/**
 * 发送当前 picker 选中的短语（等价于 send(selected)）。
 * - 如有正在发送的请求，返回 ESP_ERR_INVALID_STATE（picker 不会消失，用户可以
 *   在 turn_end 后再按）。
 * - 如果当前选中的是首行 "Settings"，则切换到 settings 模式（不发消息）。
 */
esp_err_t bb_ui_agent_chat_picker_send_selected(void);

/**
 * Phase 4.2.5 — settings sub-mode.
 *
 * Settings 是一个覆盖在 chat 屏幕底部的列表，三行：
 *   1. Agent: <name>   ── 旋转切换 driver；点击确认 + 持久化（NVS "agent/driver"）
 *   2. Theme: <name>   ── 旋转切换主题；点击 bb_agent_theme_set_active + on_exit/on_enter
 *   3. Back            ── 返回到 phrase picker
 *
 * 进入：picker 中选 "Settings" 行后 click。
 * 退出：选 "Back" 后 click → 回到 picker；或按住长按 → 退出整个 chat overlay
 *       （long-press 路由由 radio_app 处理；此处只暴露状态查询给 radio_app）。
 *
 * 调用者通过 bb_ui_agent_chat_in_settings() 决定把 rotate/click 派发到哪一组 API。
 */
int bb_ui_agent_chat_in_settings(void);

/** 旋转事件：在 settings 模式下移动当前行的内部光标（driver / theme 列表里循环）。 */
void bb_ui_agent_chat_settings_handle_rotate(int delta);

/** 点击事件：在 settings 模式下确认当前行（switch driver / switch theme / back）。 */
void bb_ui_agent_chat_settings_handle_click(void);

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
 * Light visual hint that the device is recording PTT for an agent turn.
 * begin=1 → topbar shows "LISTEN…" + state goes BUSY.
 * begin=0 → restore the cached session id (or clear if none).
 *
 * Safe to call from any task; internally posts via lv_async_call.
 */
void bb_ui_agent_chat_voice_listening(int begin);
