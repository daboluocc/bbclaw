#pragma once

#include "esp_err.h"
#include "lvgl.h"

/**
 * Standalone Settings overlay (ADR-016 revision: 2-level menu).
 *
 * Hardware has only encoder ↑/↓ + OK + BACK (no LEFT/RIGHT), so the
 * Phase-4.7 "preview-on-LEFT/RIGHT, commit-on-OK" model is gone. Replaced
 * with a classic 2-level picker:
 *
 *   Main page:
 *       Driver:   <name>     ─┐
 *       Model:    <label>     │── OK on these rows pushes a sub-picker
 *       Back                  ┘    (Driver / Model).
 *
 *   Sub-pickers (Driver / Model) are flat lists:
 *       > <option1>  ✓        ← current selection marked, cursor here
 *         <option2>
 *         <option3>
 *
 *     OK   = commit + pop back to main (async PUT to adapter)
 *     BACK = abandon + pop back to main
 *
 *  Session selection lives in Session Picker (bb_ui_agent_chat) — entered
 *  by short-press OK from chat. Settings is the long-press path and does
 *  NOT duplicate session selection.
 *
 * Lifecycle / threading: same as before — caller holds LVGL lock; async
 * adapter fetches run on background FreeRTOS tasks and dispatch back via
 * lv_async_call.
 */

void bb_ui_settings_show(lv_obj_t* parent);
/* Open the overlay at the 精简主菜单 (对话 / 提醒 / 设置) instead of straight into
 * the Settings list — the STANDBY entry point (ADR-021-firmware-ui §9.1). */
void bb_ui_settings_show_menu(lv_obj_t* parent);
void bb_ui_settings_hide(void);
int  bb_ui_settings_is_active(void);

/* Live-refresh the displayed volume when it is changed remotely (cloud
 * heartbeat applied a `device set-volume`) while the Settings menu is open. A
 * no-op when Settings is closed (bb_ui_settings_show re-reads the persisted
 * value on entry) or while the user is mid-edit. Caller must hold lvgl_port_lock. */
void bb_ui_settings_notify_volume_pct(int pct);

/* UP/DOWN rotation moves the cursor at the current level (main or picker). */
void bb_ui_settings_handle_rotate(int delta);

/* OK on the current cursor row.
 *  - main page: Driver/Model row pushes a picker; Back row exits the overlay
 *    (caller still must call bb_ui_settings_hide).
 *  - picker: commits the highlighted choice via async PUT, then pops back to
 *    the main page.
 * Returns 1 if the overlay should be torn down to CHAT (对话 in the 精简主菜单),
 * 2 to tear down to STANDBY idle, 0 otherwise. Caller (bb_radio_app) drives the
 * state transition. */
int  bb_ui_settings_handle_click(void);

/* BACK navigation:
 *  - on a sub-picker: pops back to main without committing. Returns 0.
 *  - on the Settings main page (opened from chat): returns 1 (→ chat).
 *  - at the top of the 精简主菜单 (opened from standby): returns 2 (→ standby).
 * Keeps "BACK is always one level up" consistent across the whole overlay. */
int  bb_ui_settings_handle_back(void);
