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
} bb_finish_stream_event_type_t;

typedef struct {
  bb_finish_stream_event_type_t type;
  const char* phase;
  const char* text;
  bb_tts_chunk_t* tts_chunk; /* callback owns this when non-NULL */
  int reply_wait_timed_out;
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

esp_err_t bb_adapter_healthz(int* http_status);
esp_err_t bb_adapter_stream_start(bb_stream_ctx_t* ctx);
esp_err_t bb_adapter_stream_chunk(bb_stream_ctx_t* ctx, const uint8_t* data, size_t len, int64_t ts_ms);
esp_err_t bb_adapter_stream_chunk_pcm(bb_stream_ctx_t* ctx, const uint8_t* pcm, size_t pcm_len, int64_t ts_ms);
esp_err_t bb_adapter_stream_finish(const bb_stream_ctx_t* ctx, bb_finish_result_t* out_result);
esp_err_t bb_adapter_stream_finish_stream(const bb_stream_ctx_t* ctx, bb_finish_result_t* out_result,
                                          bb_finish_stream_event_cb_t on_event, void* user_ctx);
esp_err_t bb_adapter_tts_synthesize_pcm16(const char* text, bb_tts_audio_t* out_audio);
void bb_adapter_tts_audio_free(bb_tts_audio_t* audio);
void bb_adapter_tts_chunks_free(bb_tts_chunk_t* head);
esp_err_t bb_adapter_display_pull(bb_display_task_t* out_task);
esp_err_t bb_adapter_display_ack(const char* task_id, const char* action_id);
