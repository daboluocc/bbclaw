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
 * UX (Phase 4.7.2 — preview/commit model):
 *   UP/DOWN  : move row cursor
 *   LEFT/RIGHT : on rows 0..2, preview the row's value (no NVS write).
 *                The displayed name updates so the user can see what's
 *                pending, but nothing is persisted yet.
 *   OK       : commit the previewed value for the current row (NVS write +
 *                bb_agent_theme_set_active for theme), then advance cursor.
 *                On Back row, OK exits the overlay.
 *   BACK     : exit immediately. Pending un-committed previews are
 *                discarded (next entry shows the actual saved values).
 *
 * Why this is the model: an earlier auto-save attempt (4.7.1) wrote NVS on
 * every LEFT/RIGHT, which caused device restarts when users cycled rapidly
 * (likely flash IO contending with LVGL drawing under the LVGL lock).
 * Explicit OK-to-commit avoids the rapid NVS write storm.
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
