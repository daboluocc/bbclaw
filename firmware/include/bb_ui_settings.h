#pragma once

#include "esp_err.h"
#include "lvgl.h"

/**
 * Standalone Settings overlay (Phase 4.7).
 *
 * From the main radio screen, pressing OK opens this overlay full-screen:
 *
 *     Session:    <session preview>
 *     TTS reply:  On / Off
 *     Back
 *
 * UX (preview/commit model):
 *   UP/DOWN    : move row cursor
 *   LEFT/RIGHT : on Session row, cycle through sessions for the current
 *                driver. On TTS row, toggle On/Off. No NVS write yet.
 *   OK         : commit the previewed value (NVS write), then advance
 *                cursor. On Back row, OK exits the overlay.
 *   BACK       : exit immediately. Pending un-committed previews are
 *                discarded (next entry shows the actual saved values).
 *
 * Session fetch is async (background FreeRTOS task → lv_async_call).
 * While loading, the Session row shows "loading...". The session list
 * follows the current driver (from the adapter's SESSION frame).
 *
 * Lifecycle:
 *   bb_ui_settings_show(parent)  ── builds the overlay, kicks off async
 *                                   session list fetch. Caller must hold
 *                                   the LVGL lock.
 *   bb_ui_settings_hide()        ── tears down; safe when not active.
 *   bb_ui_settings_is_active()   ── 1 while the overlay is visible.
 */

void bb_ui_settings_show(lv_obj_t* parent);
void bb_ui_settings_hide(void);
int  bb_ui_settings_is_active(void);

void bb_ui_settings_handle_rotate(int delta);
void bb_ui_settings_handle_value(int delta);
void bb_ui_settings_handle_click(void);

/* Phase 4.8.x — read the persisted TTS-reply toggle. Source of truth lives
 * in bb_ui_settings (NVS-backed); chat module has a local copy that's
 * loaded at chat-show. Themes use this accessor to render an indicator
 * in the topbar. Returns 0 or 1. */
int bb_ui_settings_tts_enabled(void);

/* Eagerly load NVS-backed settings into the in-memory cache. MUST be called
 * from a task with an internal-RAM stack (e.g. app_main / bb_radio_app_start)
 * — NVS reads disable SPI flash cache, which makes PSRAM stacks unreachable
 * and traps esp_task_stack_is_sane_cache_disabled. After this returns, all
 * later reads in bb_ui_settings_show / bb_ui_settings_tts_enabled are pure
 * memory reads safe from any task. Idempotent (subsequent calls are no-ops). */
void bb_ui_settings_preload_nvs(void);
