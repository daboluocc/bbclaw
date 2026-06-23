#pragma once

#include <stddef.h>
#include <stdint.h>

#include "esp_err.h"

/**
 * ADR-017 — chat history tail cache.
 *
 * Persists the last N messages of the active session per driver so the
 * transcript can be hydrated immediately on chat enter, even when the
 * adapter is unreachable. The adapter remains the source of truth: when a
 * history fetch lands, the transcript is rebuilt from the remote payload
 * and the cache is rewritten to match.
 *
 * Storage: NVS blob under namespace "bbclaw", key "cc/<driver-prefix>"
 * (e.g. cc/cc for claude-code, cc/oc for opencode). One blob per driver,
 * sized so the active driver's recent tail fits in ~1.5 KB. Older messages
 * are evicted FIFO so the blob stays within budget.
 *
 * All append/finalize/clear calls are safe to invoke from the LVGL task —
 * persistence is deferred to a worker task (same pattern as
 * bb_session_store).
 */

/* Roles stored in the cache. Matches the role string passed by the
 * transcript module, encoded as a single byte for compactness. */
#define BB_CHAT_CACHE_ROLE_USER       'u'
#define BB_CHAT_CACHE_ROLE_ASSISTANT  'a'
#define BB_CHAT_CACHE_ROLE_TOOL       't'
#define BB_CHAT_CACHE_ROLE_ERROR      'e'

/* Callback for hydration. role is the single-byte role; content is a
 * NUL-terminated string owned by the cache and only valid for the call. */
typedef void (*bb_chat_cache_replay_cb)(char role, const char* content,
                                        void* user);

/* Initialize the cache. Idempotent. Currently a no-op (NVS namespace is
 * shared with bb_session_store and already opened on demand). Kept as a
 * hook so we can preload mtime/index later. */
void bb_chat_cache_init(void);

/* Bind the active (driver, session). All subsequent append calls write to
 * this driver's blob. Caller is responsible for clearing the in-memory
 * buffer on session change — bind() leaves the buffer alone so the
 * internal-stack hydrate task can fill it without a race. */
void bb_chat_cache_bind(const char* driver_name, const char* session_id);

/* Read the persisted blob for the bound driver into the in-memory buffer.
 * Must be called from an internal-RAM-stack task (NVS reads disable SPI
 * flash cache and panic on PSRAM stacks — see bb_session_store.c notes).
 * No-op when the blob's sid doesn't match the bound session (a stale
 * cache for a previous session is silently discarded). Returns 0 on
 * successful hydrate, negative on miss/error. */
int  bb_chat_cache_hydrate_from_nvs(void);

/* Append a finalized message to the cache and schedule a persist. Streaming
 * assistant text should go through append_assistant_chunk + finalize_assistant
 * instead so only the assembled message lands in the blob. */
void bb_chat_cache_append_user(const char* text);
void bb_chat_cache_append_tool(const char* tool, const char* hint);
void bb_chat_cache_append_error(const char* msg);

/* Streaming assistant accumulator. Chunks are concatenated in an internal
 * buffer; finalize moves the buffer into the cache as one message. */
void bb_chat_cache_append_assistant_chunk(const char* delta);
void bb_chat_cache_finalize_assistant(void);

/* Walk the cached messages (oldest → newest) for the bound (driver, sid).
 * No-op if the blob's sid doesn't match the bound session — caller should
 * have already called bind() with the active sid. */
void bb_chat_cache_replay(bb_chat_cache_replay_cb cb, void* user);

/* Drop the in-memory buffer and rewrite the blob with the current binding's
 * sid + zero messages. Called when the adapter returns authoritative
 * history that should replace the cached tail. */
void bb_chat_cache_clear(void);

/* ADR-028 §2.5.1 (撤回语义) — withdraw the last turn: discard any in-flight
 * (un-finalized) assistant text and truncate the buffer back to just before
 * the most recent USER message, removing that user line and every
 * assistant/tool/error record that followed it. No-op when there is no user
 * record. Keeps earlier completed turns. Used on PTT barge-in cancel so the
 * cancelled turn doesn't reappear on wake / history replay. */
void bb_chat_cache_drop_last_turn(void);

/* True when the cache has buffered messages for the bound session. */
int bb_chat_cache_has_data(void);
