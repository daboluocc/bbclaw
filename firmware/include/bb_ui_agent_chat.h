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
