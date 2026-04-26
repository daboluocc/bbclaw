#pragma once

#include "esp_err.h"
#include "lvgl.h"

/**
 * Standalone Settings overlay (Phase 4.7).
 *
 * Replaces the old in-chat-picker "Settings" sub-mode. From the main radio
 * screen, pressing OK opens this overlay full-screen — no chat transcript,
 * no theme rendering, just the four configuration rows:
 *
 *     Agent:      <driver name>
 *     Theme:      <theme name>
 *     TTS reply:  On / Off
 *     Back
 *
 * UX (Phase 4.7.1 — auto-save model):
 *   UP/DOWN  : move row cursor
 *   LEFT/RIGHT : on rows 0..2, cycle the row's value AND persist to NVS
 *                immediately (auto-save). On Theme row also calls
 *                bb_agent_theme_set_active so subsequent chat-show picks it up.
 *   OK       : just advance the row cursor (values already auto-saved); on
 *                Back row, exit the overlay.
 *   BACK     : exit immediately (changes are already saved).
 *
 * Lifecycle:
 *   bb_ui_settings_show(parent)  ── builds the overlay, kicks off async driver
 *                                   list fetch, populates theme list from
 *                                   bb_agent_theme registry. Caller must hold
 *                                   the LVGL lock.
 *   bb_ui_settings_hide()        ── tears down; safe when not active.
 *   bb_ui_settings_is_active()   ── 1 while the overlay is visible.
 *
 * Nav event entry points (called from bb_radio_app under the LVGL lock):
 *   bb_ui_settings_handle_rotate(±1)  ── UP / DOWN row movement
 *   bb_ui_settings_handle_value(±1)   ── LEFT / RIGHT cycle current row's value
 *   bb_ui_settings_handle_click()     ── OK; commits row value or exits on Back
 *
 * Driver fetch is async (background FreeRTOS task → lv_async_call), same
 * pattern as Phase 4.2.5. While loading, the Agent row shows "loading...".
 * If the user closes Settings before the fetch completes, the result is
 * dropped via a generation counter.
 */

void bb_ui_settings_show(lv_obj_t* parent);
void bb_ui_settings_hide(void);
int  bb_ui_settings_is_active(void);

void bb_ui_settings_handle_rotate(int delta);
void bb_ui_settings_handle_value(int delta);
void bb_ui_settings_handle_click(void);
