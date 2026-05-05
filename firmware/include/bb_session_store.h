#pragma once

#include "esp_err.h"

/**
 * bb_session_store — per-driver logical session ID persistence (ADR-014 Phase B)
 *
 * NVS namespace "bbclaw" keys:
 *   ls/cc → claude-code logical session ID
 *   ls/oc → opencode logical session ID
 *   ls/op → openclaw logical session ID
 *   ls/ol → ollama logical session ID
 *
 * Uses deferred persist pattern (commit f33e232) to avoid NVS writes under
 * LVGL lock, which can cause device restarts due to flash IO contention.
 *
 * ADR-014: Logical session IDs use "ls-" prefix and are stable across CLI
 * session invalidation. The adapter transparently recovers underlying CLI
 * sessions without device involvement.
 */

/**
 * Migrate legacy NVS keys from v0.4.x (s/cc, s/oc, etc.) to the new
 * logical session keys (ls/cc, ls/oc, etc.). Old keys are erased without
 * value migration (CLI session IDs are likely invalid after upgrade).
 *
 * Uses an NVS flag "ls_migrated" to ensure the cleanup runs only once.
 * Safe to call on every boot (idempotent after first run).
 *
 * MUST be called from a task with an internal-RAM stack (e.g. app_main)
 * because NVS reads disable SPI flash cache.
 */
void bb_session_store_migrate(void);

/**
 * Load the last-used logical session ID for the given driver.
 *
 * @param driver_name  Driver name (e.g., "claude-code", "opencode")
 * @param out_sid      Output buffer for session ID
 * @param sz           Buffer size (recommend 64 bytes)
 * @return ESP_OK on success, ESP_ERR_NOT_FOUND if no stored session,
 *         ESP_ERR_INVALID_ARG if driver unknown or params invalid
 */
esp_err_t bb_session_store_load(const char* driver_name, char* out_sid, size_t sz);

/**
 * Save the current logical session ID for the given driver (deferred write).
 *
 * Spawns a background task to write to NVS, avoiding flash IO under LVGL lock.
 *
 * @param driver_name  Driver name
 * @param session_id   Logical session ID to persist (empty string clears the entry)
 * @return ESP_OK if task spawned, error code otherwise
 */
esp_err_t bb_session_store_save(const char* driver_name, const char* session_id);
