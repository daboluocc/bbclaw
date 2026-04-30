#pragma once

#include "esp_err.h"

/**
 * App-level page state machine.
 *
 * See design/decisions/ADR-012-fixed-page-menu.md.
 *
 * - LOCKED:   cloud_saas only; awaits passphrase unlock via PTT.
 * - STANDBY:  default home page; OK -> SETTINGS, BACK -> CHAT.
 * - CHAT:     Agent Chat page; PTT records, BACK -> STANDBY.
 * - SETTINGS: Settings page; nav within rows, BACK -> STANDBY.
 *
 * Numeric values are stable across renames (LOCKED=0, STANDBY=1) so any
 * code historically reading the previous {LOCKED, UNLOCKED} pair keeps
 * its meaning (UNLOCKED == STANDBY == 1).
 */
typedef enum {
  BBCLAW_STATE_LOCKED = 0,
  BBCLAW_STATE_STANDBY = 1,
  BBCLAW_STATE_CHAT = 2,
  BBCLAW_STATE_SETTINGS = 3,
} bb_radio_app_state_t;

esp_err_t bb_radio_app_start(void);
