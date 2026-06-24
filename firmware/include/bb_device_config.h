#pragma once

#include "esp_err.h"

/**
 * Device configuration management (cloud-synced).
 *
 * Provides a unified interface for device configuration that can be:
 * - Set by cloud via WebSocket (config.update message)
 * - Persisted to NVS for offline use
 * - Queried by modules to control feature behavior
 *
 * Configuration is synced from cloud on connect (welcome message) and
 * updated in real-time via config.update messages.
 */

typedef struct {
  int version;           /* Config version (monotonic) */
  int miyu_enabled;      /* 1 = miyu feature enabled, 0 = disabled */
  int volume_pct;        /* Volume percentage (0-100) */
  int speed_ratio_x10;   /* Playback speed (10 = 1.0x, 12 = 1.2x) */
  int speaker_enabled;   /* 1 = speaker enabled, 0 = disabled */
} bb_device_config_t;

/**
 * Load config from NVS. Call once at boot from internal-RAM stack.
 * Returns ESP_OK if loaded, ESP_ERR_NOT_FOUND if no config in NVS.
 */
esp_err_t bb_device_config_load(void);

/**
 * Get current config. Returns pointer to internal state (read-only).
 * Safe to call from any task after bb_device_config_load().
 */
const bb_device_config_t* bb_device_config_get(void);

/**
 * Apply config update from cloud. Merges partial update into current config,
 * increments version, and persists to NVS. Returns ESP_OK on success.
 *
 * @param version  New version from cloud (must be > current version)
 * @param updates  JSON object with fields to update (may be partial)
 */
esp_err_t bb_device_config_apply_update(int version, const char* updates_json);

/**
 * Apply full config from cloud welcome message. Replaces current config
 * and persists to NVS. Returns ESP_OK on success.
 *
 * @param config_json  JSON object with full config
 */
esp_err_t bb_device_config_apply_welcome(const char* config_json);

/**
 * Set volume percentage locally (device-initiated change).
 * Clamps to [0, 100], persists to NVS immediately so the choice survives
 * reboots (re-applied at boot in bb_radio_app via bb_audio_set_volume_pct).
 * The cloud heartbeat only overrides the volume when the cloud value changes
 * at runtime, so a local change is never clobbered by a cloud re-send of an
 * unchanged value.
 *
 * @param pct  Volume 0–100
 */
esp_err_t bb_device_config_set_volume_pct(int pct);

/**
 * Mirror a CLOUD-applied volume into the local config (cloud-initiated change).
 * Use this when the cloud heartbeat overrides the volume at runtime: it updates
 * the in-memory field and persists to NVS so the Settings menu (which reads this
 * field on entry) and the next boot show the value the cloud just applied —
 * WITHOUT bumping version. Version is the cloud's own monotonic counter for
 * config.update / welcome gating; inflating it from a cloud-driven change would
 * make a later cloud config.update (for any field) be ignored as stale.
 * Clamps to [0, 100]; a no-op when the value is unchanged.
 *
 * @param pct  Volume 0–100, as reported by the cloud
 */
esp_err_t bb_device_config_note_volume_pct(int pct);
