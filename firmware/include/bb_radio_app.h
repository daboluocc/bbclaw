#pragma once

#include "esp_err.h"

/**
 * App-level page state machine (3-state, STANDBY removed).
 *
 * See design/decisions/ADR-012-fixed-page-menu.md.
 *
 * - LOCKED:   cloud_saas only; awaits passphrase unlock via PTT.
 * - CHAT:     default home page; PTT records, OK -> SETTINGS.
 * - SETTINGS: Settings page; nav within rows, BACK -> CHAT.
 */
typedef enum {
  BBCLAW_STATE_LOCKED = 0,
  BBCLAW_STATE_CHAT = 1,
  BBCLAW_STATE_SETTINGS = 2,
} bb_radio_app_state_t;

esp_err_t bb_radio_app_start(void);
