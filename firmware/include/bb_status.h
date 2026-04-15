#pragma once

/**
 * BBClaw Status String Constants
 *
 * Centralized definitions for all status strings used across the firmware.
 * These strings drive the UI display, LED patterns, and app state logic.
 *
 * Usage:
 *   #include "bb_status.h"
 *   bb_display_show_status(BB_STATUS_READY);
 */

/* ── Idle / Base states ── */
#define BB_STATUS_READY  "READY"
#define BB_STATUS_LOCKED "LOCKED"

/* ── Recording / Transmission ── */
#define BB_STATUS_TX "TX"
#define BB_STATUS_RX "RX"

/* ── Voice Verification (cloud_saas passphrase unlock) ── */
#define BB_STATUS_VERIFY     "VERIFY"
#define BB_STATUS_VERIFY_TX  "VERIFY TX"
#define BB_STATUS_VERIFY_ERR "VERIFY ERR"

/* ── Response / Playback ── */
#define BB_STATUS_SPEAK  "SPEAK"
#define BB_STATUS_RESULT "RESULT"
#define BB_STATUS_TASK   "TASK"
#define BB_STATUS_BUSY   "BUSY"

/* ── Boot / Connection ── */
#define BB_STATUS_BOOT     "BOOT"
#define BB_STATUS_WIFI    "WIFI"
#define BB_STATUS_WIFI_AP "WIFI AP"
#define BB_STATUS_ADAPTER  "ADAPTER"
#define BB_STATUS_SPK     "SPK"
#define BB_STATUS_PAIR    "PAIR"

/* ── WiFi / Network Errors ── */
#define BB_STATUS_NO_WIFI  "NO WIFI"
#define BB_STATUS_WIFI_ERR "WIFI ERR"

/* ── Error ── */
#define BB_STATUS_ERR  "ERR"
#define BB_STATUS_AUTH "AUTH"

/* ── TTS ── */
#define BB_STATUS_SKIP "SKIP"
