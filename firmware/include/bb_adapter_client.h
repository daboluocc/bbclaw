#pragma once

#include <stddef.h>
#include <stdint.h>

#include "esp_err.h"

typedef struct {
  char stream_id[64];
  int next_seq;
  int64_t first_ts_ms;
  int64_t last_ts_ms;
  void* ws_encoder;
  int ws_chunk_count;
} bb_stream_ctx_t;

/* TTS chunk received during streaming finish. Linked list, caller frees. */
typedef struct bb_tts_chunk bb_tts_chunk_t;
struct bb_tts_chunk {
  uint8_t* pcm_data;
  size_t pcm_len;
  int sample_rate;
  int channels;
  int seq;
  char tts_text[256]; /* sentence text from cloud TTS, for display sync */
  bb_tts_chunk_t* next;
};

typedef struct {
  char transcript[512];
  char reply_text[4096];
  char saved_input_path[320];
  int reply_wait_timed_out;
  int http_status;
  char error_code[48];
  bb_tts_chunk_t* tts_chunks;      /* compatibility path; main runtime now prefers event callback */
  bb_tts_chunk_t* tts_chunks_tail; /* internal: append pointer */
} bb_finish_result_t;

typedef enum {
  BB_FINISH_STREAM_EVENT_STATUS = 0,
  BB_FINISH_STREAM_EVENT_ASR_FINAL,
  BB_FINISH_STREAM_EVENT_REPLY_DELTA,
  BB_FINISH_STREAM_EVENT_TTS_CHUNK,
  BB_FINISH_STREAM_EVENT_VOICE_DONE,
  BB_FINISH_STREAM_EVENT_TTS_DONE,
  BB_FINISH_STREAM_EVENT_ERROR,
  BB_FINISH_STREAM_EVENT_THINKING,
  BB_FINISH_STREAM_EVENT_TOOL_CALL,
  /* ADR-021-firmware-ui §1.2: butler dispatch progress (started/done/async/error) */
  BB_FINISH_STREAM_EVENT_DISPATCH_STATUS,
  /* Issue #146: cloud voice (butler) session frame — carries the resolved
   * session id (in event->text) + driver (in event->phase) so the device can
   * persist the session for cache replay + history on CHAT re-entry. */
  BB_FINISH_STREAM_EVENT_SESSION,
} bb_finish_stream_event_type_t;

/* Dispatch status payload — carried in bb_finish_stream_event_t.dispatch when
 * type == BB_FINISH_STREAM_EVENT_DISPATCH_STATUS. */
typedef struct {
  char phase[16];    /* "started" | "done" | "async" | "error" */
  char task_id[64];  /* tool_use id from adapter */
  char cwd[64];      /* project cwd (filled on started) */
  int64_t elapsed_ms; /* worker elapsed time (filled on done/async) */
} bb_dispatch_status_t;

typedef struct {
  bb_finish_stream_event_type_t type;
  const char* phase;
  const char* text;
  /* ADR-030: short tool hint (command / file path) for TOOL_CALL events.
   * text carries the tool name, hint the preview. NULL/"" when unavailable. */
  const char* hint;
  bb_tts_chunk_t* tts_chunk; /* callback owns this when non-NULL */
  int reply_wait_timed_out;
  /* non-NULL when type == BB_FINISH_STREAM_EVENT_DISPATCH_STATUS */
  const bb_dispatch_status_t* dispatch;
} bb_finish_stream_event_t;

typedef void (*bb_finish_stream_event_cb_t)(bb_finish_stream_event_t* event, void* user_ctx);

typedef struct {
  int has_task;
  char task_id[64];
  char display_text[256];
} bb_display_task_t;

typedef struct {
  uint8_t* pcm_data;
  size_t pcm_len;
  int sample_rate;
  int channels;
} bb_tts_audio_t;

typedef struct {
  int match;
  float confidence;
  char message[128];
} bb_voice_verify_result_t;

/* ADR-027: a single Home Adapter ("机器") the device may switch its active
 * binding to. Populated from the cloud-terminated WS `sites.list` reply. */
typedef struct {
  char home_site_id[40];
  char label[40];
  uint8_t online;
  uint8_t active;
} bb_site_info_t;

esp_err_t bb_adapter_healthz(int* http_status);
esp_err_t bb_adapter_stream_start(bb_stream_ctx_t* ctx);
esp_err_t bb_adapter_stream_chunk(bb_stream_ctx_t* ctx, const uint8_t* data, size_t len, int64_t ts_ms);
esp_err_t bb_adapter_stream_chunk_pcm(bb_stream_ctx_t* ctx, const uint8_t* pcm, size_t pcm_len, int64_t ts_ms);
esp_err_t bb_adapter_stream_finish(const bb_stream_ctx_t* ctx, bb_finish_result_t* out_result);
esp_err_t bb_adapter_stream_finish_stream(const bb_stream_ctx_t* ctx, bb_finish_result_t* out_result,
                                          bb_finish_stream_event_cb_t on_event, void* user_ctx);
esp_err_t bb_adapter_voice_verify_pcm16(const uint8_t* pcm, size_t pcm_len, bb_voice_verify_result_t* out_result);
esp_err_t bb_adapter_tts_synthesize_pcm16(const char* text, bb_tts_audio_t* out_audio, int seg_idx);
void bb_adapter_tts_audio_free(bb_tts_audio_t* audio);
void bb_adapter_tts_chunks_free(bb_tts_chunk_t* head);
esp_err_t bb_adapter_display_pull(bb_display_task_t* out_task);
esp_err_t bb_adapter_display_ack(const char* task_id, const char* action_id);

/* Send a raw text frame over the adapter client WebSocket (cloud_saas mode). */
esp_err_t bb_adapter_client_send_text(const char* payload);

/* ADR-028 §2.5.1 barge-in — fire-and-forget turn cancel.
 *
 * Tells the adapter to abort the device's in-flight agent turn (kills the
 * CLI subprocess server-side, keeps the session resumable) and records an
 * interruption note that is injected into the next turn's prompt.
 *
 * played_text is the last TTS sentence the user actually heard (may be NULL
 * or ""). The network IO runs on a short-lived background task, so this is
 * safe to call from timer callbacks / the PTT edge handler; it never blocks.
 * Duplicate requests while one is still in flight are coalesced. */
esp_err_t bb_adapter_request_turn_cancel(const char* played_text);

/* ADR-028 §2.5.1 barge-in — abort the local finish-stream wait immediately.
 *
 * bb_adapter_stream_finish_stream() blocks up to BBCLAW_HTTP_STREAM_FINISH_TIMEOUT_MS
 * (90s) waiting for the cloud/adapter reply. On PTT barge-in the device must
 * NOT depend on the server actually honouring turn.cancel (it may be slow, or
 * an old adapter ignores it) — this unblocks the wait right away so stream_task
 * can start the next turn. The in-flight finish then returns with
 * error_code="ABORTED_BY_USER"; any late reply/TTS frames for that stream are
 * dropped (stream id no longer matches). Safe to call from the PTT edge /
 * esp_timer context. No-op when no finish wait is in flight. */
void bb_adapter_abort_finish_wait(void);

/* ADR-027 — device-side Home Adapter switching (cloud_saas only). Both calls
 * are synchronous: they send a cloud-terminated WS control frame and block on
 * the matching reply (or error / timeout). Call from a background task, not the
 * LVGL thread.
 *
 * sites.list  — fills out_sites (up to max_sites) and *out_count.
 * sites.activate — ESP_OK when the reply is result:"ok"; on failure returns
 *   ESP_FAIL and fills out_err_code with the stable cloud error code
 *   (NOT_BOUND / OWNER_MISMATCH / ADAPTER_OFFLINE / INTERNAL / TIMEOUT). */
esp_err_t bb_adapter_sites_list(bb_site_info_t* out_sites, int max_sites, int* out_count);
esp_err_t bb_adapter_sites_activate(const char* home_site_id, char* out_active_id, size_t active_len,
                                    char* out_err_code, size_t err_len);
