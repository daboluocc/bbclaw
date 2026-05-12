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
 * OK_LONG is a NEW event — only the Flipper 6-button mode emits it. It fires
 * when the OK button is held for BBCLAW_NAV_LONG_PRESS_MS (700 ms). When
 * OK_LONG fires, the subsequent release edge does NOT also emit OK, so the
 * two gestures are mutually exclusive.
 *
 * Keep BB_NAV_EVENT_COUNT immediately after all real values so it is the
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
  BB_NAV_EVENT_OK_LONG, /* long-press OK: enter SETTINGS from CHAT (Flipper mode only) */
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

/* Inject a navigation event as if a real button was pressed.
 *
 * Bypasses GPIO polling and debouncing; the registered callback is
 * dispatched immediately on the caller's task. Intended for the device
 * monitor (ADR-015) to drive UI from the host tool. Safe to call before
 * bb_nav_input_init — becomes a silent no-op until a callback registers.
 */
void bb_nav_input_inject(bb_nav_event_t event);
