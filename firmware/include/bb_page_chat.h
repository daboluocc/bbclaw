#pragma once

#include "lvgl.h"

/**
 * CHAT page: top bar + content body + bottom bar.
 *
 * Top bar:    status icon | status text | WiFi | battery | clock
 * Body:       transcript (messages) / recording overlay / session-cwd pickers
 * Bottom bar: session id (left) + cwd pool name (right)
 *
 * This is an independent full-screen view, not an overlay.
 * Replaces the buddy-anim agent_chat overlay mechanism.
 */
void bb_page_chat_create(lv_obj_t* scr);
void bb_page_chat_set_visible(int visible);

/** Get the body container where transcript/recording/pickers should be parented. */
lv_obj_t* bb_page_chat_get_body(void);
