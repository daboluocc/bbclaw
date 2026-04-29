#pragma once

#include "esp_err.h"

/**
 * bb_session_store — per-driver session ID persistence (Phase S2)
 *
 * NVS namespace "bbclaw" keys:
 *   s/cc → claude-code session ID
 *   s/oc → opencode session ID
 *   s/op → openclaw session ID
 *   s/ol → ollama session ID
 *
 * Uses deferred persist pattern (commit f33e232) to avoid NVS writes under
 * LVGL lock, which can cause device restarts due to flash IO contention.
 */

/**
 * Load the last-used session ID for the given driver.
 *
 * @param driver_name  Driver name (e.g., "claude-code", "opencode")
 * @param out_sid      Output buffer for session ID
 * @param sz           Buffer size (recommend 64 bytes)
 * @return ESP_OK on success, ESP_ERR_NOT_FOUND if no stored session,
 *         ESP_ERR_INVALID_ARG if driver unknown or params invalid
 */
esp_err_t bb_session_store_load(const char* driver_name, char* out_sid, size_t sz);

/**
 * Save the current session ID for the given driver (deferred write).
 *
 * Spawns a background task to write to NVS, avoiding flash IO under LVGL lock.
 *
 * @param driver_name  Driver name
 * @param session_id   Session ID to persist (empty string clears the entry)
 * @return ESP_OK if task spawned, error code otherwise
 */
esp_err_t bb_session_store_save(const char* driver_name, const char* session_id);
