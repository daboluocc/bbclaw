#pragma once

#include "esp_err.h"

/**
 * Navigation events.
 *
 * Phase 5 (Option B) introduces dedicated semantic events for the Flipper
 * 6-button layout. The legacy encoder/3-button modes keep emitting the
 * subset they support and we alias the old names to preserve any external
 * call site or compile-time check.
 *
 * Semantic mapping (legacy -> new):
 *   ROTATE_CCW ≡ UP        (encoder turning left = picker going up)
 *   ROTATE_CW  ≡ DOWN      (encoder turning right = picker going down)
 *   CLICK      ≡ OK        (encoder press = confirm)
 *   LONG_PRESS ≡ BACK      (encoder long-press = exit; on Flipper BACK is its
 *                           own dedicated key but emits the same event)
 *
 * LEFT and RIGHT are NEW events — only the Flipper 6-button mode emits them;
 * legacy modes never do, so call sites must treat them as optional.
 *
 * Keep BB_NAV_EVENT_COUNT immediately after the 6 real values so it is the
 * size used for any per-event versioning array. Aliases sit AFTER the count
 * marker so they do not inflate it.
 */
typedef enum {
  BB_NAV_EVENT_UP = 0,
  BB_NAV_EVENT_DOWN,
  BB_NAV_EVENT_LEFT,
  BB_NAV_EVENT_RIGHT,
  BB_NAV_EVENT_OK,
  BB_NAV_EVENT_BACK,
  BB_NAV_EVENT_COUNT,

  /* Backwards-compat aliases (Option A naming). Same numeric value as the
   * new events so existing switch/case code keeps working unchanged. */
  BB_NAV_EVENT_ROTATE_CCW = BB_NAV_EVENT_UP,
  BB_NAV_EVENT_ROTATE_CW = BB_NAV_EVENT_DOWN,
  BB_NAV_EVENT_CLICK = BB_NAV_EVENT_OK,
  BB_NAV_EVENT_LONG_PRESS = BB_NAV_EVENT_BACK,
} bb_nav_event_t;

typedef void (*bb_nav_input_callback_t)(bb_nav_event_t event);

esp_err_t bb_nav_input_init(bb_nav_input_callback_t callback);
