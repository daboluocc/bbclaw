#pragma once

/**
 * Task List page — independent LVGL screen showing recent butler dispatch tasks.
 *
 * ADR-021-firmware-ui §3 / §4.2 (Sub-PR D):
 *   - Entered via short-press OK from CHAT
 *   - ↑↓ selects row; OK sends "task_status #<taskId>" turn and returns to CHAT
 *   - BACK returns to CHAT with no side effects
 *
 * All functions must be called from the LVGL task (inside lvgl_port_lock).
 */

/**
 * Show the Task List page. Triggers an async fetch of /v1/butler/dispatch/recent
 * and renders results. No-op if already visible.
 * Must be called inside the LVGL lock.
 */
void bb_ui_task_list_show(void);

/**
 * Hide the Task List page and destroy its LVGL screen.
 * Must be called inside the LVGL lock.
 */
void bb_ui_task_list_hide(void);

/**
 * Returns 1 if the Task List page is currently visible (loading or shown).
 */
int bb_ui_task_list_visible(void);

/**
 * Move the selection highlight. delta = -1 (up) / +1 (down), wraps.
 * Must be called inside the LVGL lock.
 */
void bb_ui_task_list_move(int delta);

/**
 * Activate the currently selected row:
 *   - Sends "task_status #<taskId>" as a text turn via bb_ui_agent_chat_send
 *   - Hides the Task List page (returns to CHAT screen)
 * Must be called inside the LVGL lock.
 */
void bb_ui_task_list_activate(void);
