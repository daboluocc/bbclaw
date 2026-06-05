/* Simulator stub for bb_chat_cache — the real implementation depends on
 * NVS / FreeRTOS / esp_log which don't exist in the SDL2 host build. The
 * transcript module links against these symbols; on the simulator we don't
 * persist anything, so every entry point is a no-op. Mirrors the
 * bb_time_stub.c / bb_wifi_stub.c pattern. */
#include "bb_chat_cache.h"

void bb_chat_cache_init(void) {}
void bb_chat_cache_bind(const char* driver_name, const char* session_id) {
  (void)driver_name;
  (void)session_id;
}
int bb_chat_cache_hydrate_from_nvs(void) { return -1; }
void bb_chat_cache_append_user(const char* text) { (void)text; }
void bb_chat_cache_append_tool(const char* tool, const char* hint) {
  (void)tool;
  (void)hint;
}
void bb_chat_cache_append_error(const char* msg) { (void)msg; }
void bb_chat_cache_append_assistant_chunk(const char* delta) { (void)delta; }
void bb_chat_cache_finalize_assistant(void) {}
void bb_chat_cache_replay(bb_chat_cache_replay_cb cb, void* user) {
  (void)cb;
  (void)user;
}
void bb_chat_cache_clear(void) {}
int bb_chat_cache_has_data(void) { return 0; }
