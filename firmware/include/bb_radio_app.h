#pragma once

#include "esp_err.h"

/* Forward decl so this public header needn't pull in src/bb_ota.h (which is a
 * private header only on the .c include path). Tagged struct in bb_ota.h. */
struct ota_update_info;

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
  /* ADR-044: 长录音形态。与对话互斥；PTT=书签,双 BACK=停止,豁免空闲锁屏 */
  BBCLAW_STATE_RECORDER = 3,
} bb_radio_app_state_t;

esp_err_t bb_radio_app_start(void);

/** ADR-044: 请求录音一键启停(与物理 PWR 键同一通路)。devmon 测试注入用;
 *  线程安全,下一轮输入循环消费。 */
void bb_radio_app_request_recorder_toggle(void);

/* Present the OTA confirm page for a manually-triggered update (Settings →
 * Firmware row → click). Stashes the checked update info and shows the same
 * confirm page the boot auto-check uses, routing an accept into the preheated
 * internal-RAM apply task. Call on the LVGL thread (under lvgl_port lock); the
 * confirm page sits on lv_layer_top and intercepts OK/BACK even while the
 * Settings overlay is up, so no teardown is needed. Copies *info; no-op if
 * info is NULL or has no update. */
void bb_radio_app_present_ota_update(const struct ota_update_info* info);
