/*
 * bbwire/2 device protocol client (see adapter_v2/docs/device-protocol.md and
 * firmware/docs/bbwire2-client.md). A self-contained WebSocket client to
 * adapter_v2 at /v2/dev/ws, parallel to the cloud/local paths in
 * bb_adapter_client.c — it is deliberately NOT a branch inside the cloud_saas WS
 * state machine, which is coupled to that protocol's welcome/sites/verify/pairing
 * schema none of which bbwire/2 needs. The WS lifecycle scaffolding mirrors
 * bb_adapter_client.c's proven cloud client (event group connect-wait, fragment
 * reassembly, finish-wait); only the protocol schema is new.
 *
 * Increment 2 / first bring-up: raw PCM16 both ways (codec 0x02) — no on-device
 * Opus encode, no ffmpeg on the adapter. Opus is a later optimisation.
 */

#include "bb_bbwire2.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "bb_config.h"
#include "bb_prompt.h"
#include "bb_time.h"
#include "esp_crt_bundle.h"
#include "esp_heap_caps.h"
#include "esp_log.h"
#include "esp_websocket_client.h"
#include "freertos/FreeRTOS.h"
#include "freertos/event_groups.h"
#include "freertos/semphr.h"
#include "freertos/task.h"

static const char* TAG = "bb_bbwire2";

/* Event-group bits for the v2 client (private; same BIT scheme as the cloud WS). */
#define BW_CONNECTED    BIT0 /* WS transport up */
#define BW_HELLO_OK     BIT1 /* hello.ok received */
#define BW_DONE         BIT2 /* turn{idle} — reply complete */
#define BW_ERROR        BIT3 /* error frame / disconnect during a turn */
#define BW_DISCONNECTED BIT4 /* WS dropped */

typedef struct {
  esp_websocket_client_handle_t client;
  SemaphoreHandle_t lock;
  EventGroupHandle_t events;
  int initialized;
  int connected;
  int hello_ok;

  /* current turn */
  uint16_t turn_u;
  char turn_id[16];
  uint16_t frame_seq;

  /* finish-wait state (set before ptt.stop, read by the WS task's handlers) */
  bb_finish_result_t* fin_result;
  bb_finish_stream_event_cb_t fin_cb;
  void* fin_user;
  int fin_waiting;
  /* 本回合最后一次流式活动（文本帧/二进制 TTS 帧,guard 通过后打点）。
   * 等待循环据此做空闲超时;s_bw.lock 保护。 */
  int64_t fin_last_activity_ms;

  /* inbound frame reassembly (one in flight; the WS task is single-threaded) */
  uint8_t* rx_buf;
  size_t rx_len;
  size_t rx_cap;
  uint8_t rx_opcode;
} bb_bw_state_t;

static bb_bw_state_t s_bw;

/* TTS PCM chunks may live in PSRAM (playback copies them into the I2S DMA buffer);
 * prefer PSRAM, fall back to internal. Mirrors bb_adapter_client.c's tts_alloc. */
static void* bw_alloc(size_t n) {
  void* p = heap_caps_malloc(n, BBCLAW_MALLOC_CAP_PREFER_PSRAM);
  if (p == NULL) {
    p = heap_caps_malloc(n, MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT);
  }
  return p;
}

/* ── minimal JSON field extraction (bbwire/2 control frames are small/flat) ── */

/* Extract a string value for "key" into out (NUL-terminated, unescaping \" \\ \/
 * \n \r \t). Returns 1 on success. Good enough for the flat control frames; reply
 * text with exotic escapes degrades gracefully (kept verbatim). */
static int bw_json_str(const char* json, const char* key, char* out, size_t out_cap) {
  if (json == NULL || key == NULL || out == NULL || out_cap == 0U) {
    return 0;
  }
  out[0] = '\0';
  char pat[40];
  snprintf(pat, sizeof(pat), "\"%s\"", key);
  const char* p = strstr(json, pat);
  if (p == NULL) {
    return 0;
  }
  p += strlen(pat);
  while (*p == ' ' || *p == ':' || *p == '\t') {
    p++;
  }
  if (*p != '"') {
    return 0;
  }
  p++;
  size_t i = 0;
  while (*p != '\0' && *p != '"' && i + 1U < out_cap) {
    char c = *p++;
    if (c == '\\' && *p != '\0') {
      char e = *p++;
      switch (e) {
        case 'n': c = '\n'; break;
        case 'r': c = '\r'; break;
        case 't': c = '\t'; break;
        case '"': c = '"'; break;
        case '\\': c = '\\'; break;
        case '/': c = '/'; break;
        default: c = e; break; /* \uXXXX etc. — drop the marker, keep the byte */
      }
    }
    out[i++] = c;
  }
  out[i] = '\0';
  return 1;
}

/* ── event delivery (self-contained copy of the adapter_client emit helper) ── */

static void bw_emit(bb_finish_stream_event_type_t type, const char* text, bb_tts_chunk_t* chunk) {
  if (s_bw.fin_cb == NULL) {
    /* No consumer for an owned chunk → free it here rather than leak (the
     * callback is the only other free path). Callers hold s_bw.lock, so fin_cb
     * cannot flip mid-call; this guards a future caller that forgets the lock. */
    if (chunk != NULL) {
      bb_adapter_tts_chunks_free(chunk);
    }
    return;
  }
  bb_finish_stream_event_t ev = {
      .type = type,
      .phase = NULL,
      .text = text,
      .hint = NULL,
      .tts_chunk = chunk,
      .reply_wait_timed_out = 0,
      .dispatch = NULL,
  };
  s_bw.fin_cb(&ev, s_bw.fin_user);
}

/* Emit a PROMPT_OPEN/PROMPT_CLOSE event carrying the parsed menu (ADR-033). */
static void bw_emit_prompt(bb_finish_stream_event_type_t type, const bb_prompt_t* prompt) {
  if (s_bw.fin_cb == NULL) {
    return;
  }
  bb_finish_stream_event_t ev = {
      .type = type,
      .phase = NULL,
      .text = NULL,
      .hint = NULL,
      .tts_chunk = NULL,
      .reply_wait_timed_out = 0,
      .dispatch = NULL,
      .prompt = prompt,
  };
  s_bw.fin_cb(&ev, s_bw.fin_user);
}

/* Deliver one PCM16 TTS unit to the device as a TTS_CHUNK event; the callback
 * takes ownership of the chunk (per bb_adapter_client.h). */
static void bw_deliver_pcm16_tts(const uint8_t* pcm, size_t pcm_len) {
  if (s_bw.fin_cb == NULL || pcm == NULL || pcm_len == 0U) {
    return;
  }
  bb_tts_chunk_t* chunk = (bb_tts_chunk_t*)bw_alloc(sizeof(bb_tts_chunk_t));
  if (chunk == NULL) {
    return;
  }
  memset(chunk, 0, sizeof(*chunk));
  chunk->pcm_data = (uint8_t*)bw_alloc(pcm_len);
  if (chunk->pcm_data == NULL) {
    free(chunk);
    return;
  }
  memcpy(chunk->pcm_data, pcm, pcm_len);
  chunk->pcm_len = pcm_len;
  chunk->sample_rate = BBCLAW_TTS_SAMPLE_RATE;
  chunk->channels = BBCLAW_TTS_CHANNELS;
  bw_emit(BB_FINISH_STREAM_EVENT_TTS_CHUNK, NULL, chunk);
}

/* ── inbound dispatch ──────────────────────────────────────────────────────── */

static void bw_handle_text(const char* msg) {
  char t[24] = {0};
  if (!bw_json_str(msg, "t", t, sizeof(t))) {
    return;
  }
  if (strcmp(t, "hello.ok") == 0) {
    /* Handshake frame — independent of any turn; no finish state touched. */
    s_bw.hello_ok = 1;
    xEventGroupSetBits(s_bw.events, BW_HELLO_OK);
    ESP_LOGI(TAG, "hello.ok");
    return;
  }

  /* Everything below pertains to an in-flight turn: it reads fin_result/fin_cb
   * and may invoke the callback. Hold s_bw.lock for the whole read-through-invoke
   * so it is mutually exclusive with bb_bbwire2_finish()'s teardown (which clears
   * fin_* under the same lock). Without this the lock-free read is a cross-core
   * TOCTOU → use-after-free of fin_user / a leaked TTS chunk on the timeout/error
   * paths. This mirrors the cloud client, which likewise invokes its callback
   * under s_ws.lock (append_tts_chunk_from_ogg_locked). The radio callback only
   * queues audio for playback and never re-enters this module, so no deadlock. */
  xSemaphoreTake(s_bw.lock, portMAX_DELAY);

  /* 咽喉点：所有回合内帧从这里过——打「最后活动」时间戳,喂空闲超时。 */
  s_bw.fin_last_activity_ms = bb_now_ms();

  if (strcmp(t, "turn") == 0) {
    char state[16] = {0};
    bw_json_str(msg, "state", state, sizeof(state));
    if (strcmp(state, "idle") == 0) {
      xEventGroupSetBits(s_bw.events, BW_DONE);
    }
  } else if (strcmp(t, "error") == 0) {
    char code[48] = {0};
    bw_json_str(msg, "code", code, sizeof(code));
    if (s_bw.fin_waiting && s_bw.fin_result != NULL && s_bw.fin_result->error_code[0] == '\0') {
      snprintf(s_bw.fin_result->error_code, sizeof(s_bw.fin_result->error_code), "%s",
               code[0] != '\0' ? code : "V2_ERROR");
    }
    ESP_LOGW(TAG, "error frame code=%s", code);
    xEventGroupSetBits(s_bw.events, BW_ERROR);
  } else if (strcmp(t, "prompt.open") == 0) {
    /* ADR-033: a blocking permission/confirm menu — forward to the device UI for
     * approval. Handled unconditionally (not gated on fin_waiting) so it works
     * whoever drove the turn on the shared PTY. */
    bb_prompt_t p;
    if (bb_prompt_parse_open(msg, &p)) {
      ESP_LOGI(TAG, "prompt.open id=%s opts=%d", p.prompt_id, p.n_options);
      bw_emit_prompt(BB_FINISH_STREAM_EVENT_PROMPT_OPEN, &p);
    }
  } else if (strcmp(t, "prompt.close") == 0) {
    bb_prompt_t p;
    memset(&p, 0, sizeof(p));
    bw_json_str(msg, "promptId", p.prompt_id, sizeof(p.prompt_id));
    bw_json_str(msg, "reason", p.reason, sizeof(p.reason));
    ESP_LOGI(TAG, "prompt.close id=%s reason=%s", p.prompt_id, p.reason);
    bw_emit_prompt(BB_FINISH_STREAM_EVENT_PROMPT_CLOSE, &p);
  } else if (s_bw.fin_waiting) {
    if (strcmp(t, "asr.final") == 0) {
      char text[512] = {0};
      bw_json_str(msg, "text", text, sizeof(text));
      if (s_bw.fin_result != NULL) {
        snprintf(s_bw.fin_result->transcript, sizeof(s_bw.fin_result->transcript), "%s", text);
      }
      bw_emit(BB_FINISH_STREAM_EVENT_ASR_FINAL, text, NULL);
    } else if (strcmp(t, "reply.delta") == 0) {
      char text[1024] = {0};
      bw_json_str(msg, "text", text, sizeof(text));
      bw_emit(BB_FINISH_STREAM_EVENT_REPLY_DELTA, text, NULL);
    } else if (strcmp(t, "reply.end") == 0) {
      char text[2048] = {0};
      bw_json_str(msg, "text", text, sizeof(text));
      if (s_bw.fin_result != NULL) {
        snprintf(s_bw.fin_result->reply_text, sizeof(s_bw.fin_result->reply_text), "%s", text);
      }
    }
    /* asr.partial / unknown — ignore. */
  }
  xSemaphoreGive(s_bw.lock);
}

static void bw_handle_binary(const uint8_t* frame, size_t len) {
  bb_bbwire2_bin_header_t h;
  const uint8_t* payload = NULL;
  int plen = bb_bbwire2_header_decode(frame, (int)len, &h, &payload);
  if (plen < 0 || payload == NULL) {
    return;
  }
  if (h.stream_kind != BB_BBWIRE2_STREAM_DOWNLINK_TTS) {
    return;
  }
  /* Deliver under s_bw.lock so the fin_waiting check, the chunk allocation, and
   * the callback invocation are atomic vs bb_bbwire2_finish()'s teardown — see
   * the rationale in bw_handle_text. Drop a late/stale frame whose turn_seq !=
   * the current turn, or that arrives after finish stopped waiting. */
  xSemaphoreTake(s_bw.lock, portMAX_DELAY);
  if (s_bw.fin_waiting && h.turn_seq == s_bw.turn_u && h.codec == BB_BBWIRE2_CODEC_PCM16) {
    s_bw.fin_last_activity_ms = bb_now_ms(); /* TTS 帧也是回合活动 */
    bw_deliver_pcm16_tts(payload, (size_t)plen);
  }
  /* codec == OPUS: a later optimisation (accumulate + bb_ogg_opus decode). Phase
   * A adapter emits PCM16, so the opus branch is intentionally unhandled here. */
  xSemaphoreGive(s_bw.lock);
}

/* ── WS event handler (mirrors bb_adapter_client.c ws_event_handler) ────────── */

static void bw_ws_event(void* args, esp_event_base_t base, int32_t id, void* data) {
  (void)args;
  (void)base;
  if (!s_bw.initialized) {
    return;
  }
  if (id == WEBSOCKET_EVENT_CONNECTED) {
    s_bw.connected = 1;
    xEventGroupSetBits(s_bw.events, BW_CONNECTED);
    ESP_LOGI(TAG, "ws connected");
    return;
  }
  if (id == WEBSOCKET_EVENT_DISCONNECTED || id == WEBSOCKET_EVENT_CLOSED) {
    s_bw.connected = 0;
    s_bw.hello_ok = 0;
    xEventGroupSetBits(s_bw.events, BW_DISCONNECTED);
    if (s_bw.fin_waiting) {
      xEventGroupSetBits(s_bw.events, BW_ERROR);
    }
    ESP_LOGW(TAG, "ws disconnected");
    return;
  }
  if (id != WEBSOCKET_EVENT_DATA) {
    return;
  }
  esp_websocket_event_data_t* evt = (esp_websocket_event_data_t*)data;
  if (evt == NULL || evt->data_ptr == NULL || evt->data_len <= 0) {
    return;
  }
  xSemaphoreTake(s_bw.lock, portMAX_DELAY);
  if (evt->payload_offset == 0) {
    s_bw.rx_len = 0;
    s_bw.rx_opcode = evt->op_code;
  }
  size_t need = s_bw.rx_len + (size_t)evt->data_len + 1U;
  if (need > s_bw.rx_cap) {
    size_t new_cap = s_bw.rx_cap == 0U ? 4096U : s_bw.rx_cap;
    while (new_cap < need) {
      new_cap *= 2U;
    }
    uint8_t* nb = (uint8_t*)heap_caps_realloc(s_bw.rx_buf, new_cap, BBCLAW_MALLOC_CAP_PREFER_PSRAM);
    if (nb == NULL) {
      xSemaphoreGive(s_bw.lock);
      return;
    }
    s_bw.rx_buf = nb;
    s_bw.rx_cap = new_cap;
  }
  memcpy(s_bw.rx_buf + s_bw.rx_len, evt->data_ptr, (size_t)evt->data_len);
  s_bw.rx_len += (size_t)evt->data_len;
  s_bw.rx_buf[s_bw.rx_len] = '\0';
  int complete = evt->fin && (evt->payload_offset + evt->data_len >= evt->payload_len);
  uint8_t opcode = s_bw.rx_opcode;
  size_t msg_len = s_bw.rx_len;
  xSemaphoreGive(s_bw.lock);

  if (!complete) {
    return;
  }
  if (opcode == 0x1U) {
    bw_handle_text((const char*)s_bw.rx_buf);
  } else if (opcode == 0x2U) {
    bw_handle_binary(s_bw.rx_buf, msg_len);
  }
}

/* ── connection + handshake ────────────────────────────────────────────────── */

/* Build "ws(s)://host[:port]/v2/dev/ws" from BBCLAW_ADAPTER_V2_BASE_URL, mapping
 * http→ws / https→wss and stripping any path the base URL carries. */
static void bw_build_url(char* out, size_t cap) {
  if (out == NULL || cap == 0U) {
    return;
  }
  const char* base = BBCLAW_ADAPTER_V2_BASE_URL;
  const char* scheme = "ws://";
  const char* rest = base;
  if (strncmp(base, "https://", 8) == 0) {
    scheme = "wss://";
    rest = base + 8;
  } else if (strncmp(base, "wss://", 6) == 0) {
    scheme = "wss://";
    rest = base + 6;
  } else if (strncmp(base, "http://", 7) == 0) {
    scheme = "ws://";
    rest = base + 7;
  } else if (strncmp(base, "ws://", 5) == 0) {
    scheme = "ws://";
    rest = base + 5;
  }
  char host[192] = {0};
  const char* slash = strchr(rest, '/');
  size_t host_len = slash != NULL ? (size_t)(slash - rest) : strlen(rest);
  if (host_len >= sizeof(host)) {
    host_len = sizeof(host) - 1U;
  }
  memcpy(host, rest, host_len);
  host[host_len] = '\0';
  snprintf(out, cap, "%s%s/v2/dev/ws", scheme, host);
}

static esp_err_t bw_send_text(const char* payload) {
  if (s_bw.client == NULL) {
    return ESP_ERR_INVALID_STATE;
  }
  int sent = esp_websocket_client_send_text(s_bw.client, payload, (int)strlen(payload), pdMS_TO_TICKS(5000));
  return sent >= 0 ? ESP_OK : ESP_FAIL;
}

static esp_err_t bw_send_binary(const uint8_t* data, size_t len) {
  if (s_bw.client == NULL || data == NULL || len == 0U) {
    return ESP_ERR_INVALID_STATE;
  }
  int sent = esp_websocket_client_send_bin(s_bw.client, (const char*)data, (int)len, pdMS_TO_TICKS(5000));
  return sent >= 0 ? ESP_OK : ESP_FAIL;
}

static esp_err_t bw_send_hello(void) {
  char body[256];
  snprintf(body, sizeof(body),
           "{\"t\":\"hello\",\"proto\":%d,\"dev\":\"%s\","
           "\"mic\":{\"codec\":\"pcm16\",\"rate\":%d,\"ch\":%d},"
           "\"spk\":{\"codec\":\"pcm16\",\"rate\":%d,\"ch\":%d}}",
           BB_BBWIRE2_PROTO, BBCLAW_DEVICE_ID, BBCLAW_AUDIO_SAMPLE_RATE, BBCLAW_AUDIO_CHANNELS,
           BBCLAW_TTS_SAMPLE_RATE, BBCLAW_TTS_CHANNELS);
  return bw_send_text(body);
}

esp_err_t bb_bbwire2_send_prompt_select(const char* prompt_id, const char* option_key) {
  if (prompt_id == NULL || option_key == NULL || prompt_id[0] == '\0' || option_key[0] == '\0') {
    return ESP_ERR_INVALID_ARG;
  }
  char body[160];
  snprintf(body, sizeof(body),
           "{\"t\":\"prompt.select\",\"promptId\":\"%s\",\"optionKey\":\"%s\"}",
           prompt_id, option_key);
  esp_err_t err = bw_send_text(body);
  ESP_LOGI(TAG, "prompt.select id=%s key=%s err=%d", prompt_id, option_key, (int)err);
  return err;
}

static esp_err_t bw_ensure_connected(void) {
  if (!s_bw.initialized) {
    memset(&s_bw, 0, sizeof(s_bw));
    s_bw.lock = xSemaphoreCreateMutex();
    s_bw.events = xEventGroupCreate();
    if (s_bw.lock == NULL || s_bw.events == NULL) {
      return ESP_ERR_NO_MEM;
    }
    s_bw.initialized = 1;
  }
  if (s_bw.connected && s_bw.hello_ok && s_bw.client != NULL && esp_websocket_client_is_connected(s_bw.client)) {
    return ESP_OK;
  }

  if (s_bw.client == NULL) {
    char url[320] = {0};
    bw_build_url(url, sizeof(url));
    esp_websocket_client_config_t cfg = {
        .uri = url,
        .buffer_size = 1024,
        .network_timeout_ms = BBCLAW_HTTP_TIMEOUT_MS,
        .reconnect_timeout_ms = 2000,
        .task_stack = 8192,
        .disable_auto_reconnect = false,
        .task_name = "bbclaw_ws_v2",
        .ping_interval_sec = 15,
        .disable_pingpong_discon = true,
        .crt_bundle_attach = strncmp(url, "wss", 3) == 0 ? esp_crt_bundle_attach : NULL,
    };
    ESP_LOGI(TAG, "dial %s", url);
    s_bw.client = esp_websocket_client_init(&cfg);
    if (s_bw.client == NULL) {
      return ESP_ERR_NO_MEM;
    }
    esp_websocket_register_events(s_bw.client, WEBSOCKET_EVENT_ANY, bw_ws_event, NULL);
    xEventGroupClearBits(s_bw.events, BW_CONNECTED | BW_DISCONNECTED | BW_HELLO_OK);
    if (esp_websocket_client_start(s_bw.client) != ESP_OK) {
      esp_websocket_client_destroy(s_bw.client);
      s_bw.client = NULL;
      return ESP_FAIL;
    }
  }

  /* Wait for transport up, then handshake. */
  EventBits_t bits = xEventGroupWaitBits(s_bw.events, BW_CONNECTED | BW_DISCONNECTED, pdFALSE, pdFALSE,
                                         pdMS_TO_TICKS(BBCLAW_HTTP_TIMEOUT_MS));
  if ((bits & BW_CONNECTED) == 0U) {
    ESP_LOGW(TAG, "connect timeout");
    return ESP_FAIL;
  }
  if (s_bw.hello_ok) {
    return ESP_OK;
  }
  xEventGroupClearBits(s_bw.events, BW_HELLO_OK);
  esp_err_t err = bw_send_hello();
  if (err != ESP_OK) {
    return err;
  }
  bits = xEventGroupWaitBits(s_bw.events, BW_HELLO_OK | BW_DISCONNECTED, pdFALSE, pdFALSE,
                             pdMS_TO_TICKS(BBCLAW_HTTP_TIMEOUT_MS));
  if ((bits & BW_HELLO_OK) == 0U) {
    ESP_LOGW(TAG, "hello.ok timeout");
    return ESP_FAIL;
  }
  return ESP_OK;
}

/* ── public API (called by bb_adapter_client.c v2 arms) ────────────────────── */

esp_err_t bb_bbwire2_stream_start(bb_stream_ctx_t* ctx) {
  if (ctx == NULL) {
    return ESP_ERR_INVALID_ARG;
  }
  memset(ctx, 0, sizeof(*ctx));
  esp_err_t err = bw_ensure_connected();
  if (err != ESP_OK) {
    return err;
  }
  /* New turn: bump u (uint16, wraps), reset the per-turn frame counter. */
  s_bw.turn_u++;
  if (s_bw.turn_u == 0U) {
    s_bw.turn_u = 1U;
  }
  s_bw.frame_seq = 0;
  snprintf(s_bw.turn_id, sizeof(s_bw.turn_id), "u%u", (unsigned)s_bw.turn_u);
  snprintf(ctx->stream_id, sizeof(ctx->stream_id), "%s", s_bw.turn_id);

  char body[96];
  snprintf(body, sizeof(body), "{\"t\":\"ptt.start\",\"turnId\":\"%s\",\"u\":%u}", s_bw.turn_id, (unsigned)s_bw.turn_u);
  err = bw_send_text(body);
  if (err == ESP_OK) {
    ESP_LOGI(TAG, "ptt.start %s", s_bw.turn_id);
  }
  return err;
}

esp_err_t bb_bbwire2_send_mic_pcm16(bb_stream_ctx_t* ctx, const uint8_t* pcm, size_t pcm_len) {
  (void)ctx;
  if (pcm == NULL || pcm_len == 0U) {
    return ESP_ERR_INVALID_ARG;
  }
  /* 8-byte header + raw PCM16. One WS binary frame per mic chunk; flags=0
   * (ptt.stop, not a per-frame flag, ends the utterance). */
  bb_bbwire2_bin_header_t h = {
      .stream_kind = BB_BBWIRE2_STREAM_UPLINK_MIC,
      .codec = BB_BBWIRE2_CODEC_PCM16,
      .turn_seq = s_bw.turn_u,
      .frame_seq = s_bw.frame_seq++,
      .flags = 0,
  };
  size_t frame_len = (size_t)BB_BBWIRE2_BIN_HEADER_LEN + pcm_len;
  uint8_t* frame = (uint8_t*)bw_alloc(frame_len);
  if (frame == NULL) {
    return ESP_ERR_NO_MEM;
  }
  bb_bbwire2_header_encode(&h, frame);
  memcpy(frame + BB_BBWIRE2_BIN_HEADER_LEN, pcm, pcm_len);
  esp_err_t err = bw_send_binary(frame, frame_len);
  free(frame);
  return err;
}

esp_err_t bb_bbwire2_finish(const bb_stream_ctx_t* ctx, bb_finish_result_t* out_result,
                            bb_finish_stream_event_cb_t on_event, void* user_ctx) {
  if (ctx == NULL || out_result == NULL) {
    return ESP_ERR_INVALID_ARG;
  }
  if (!s_bw.initialized || s_bw.client == NULL) {
    return ESP_ERR_INVALID_STATE;
  }
  memset(out_result, 0, sizeof(*out_result));

  /* Arm the finish-wait BEFORE sending ptt.stop so a fast reply isn't missed. */
  xSemaphoreTake(s_bw.lock, portMAX_DELAY);
  s_bw.fin_result = out_result;
  s_bw.fin_cb = on_event;
  s_bw.fin_user = user_ctx;
  s_bw.fin_waiting = 1;
  s_bw.fin_last_activity_ms = bb_now_ms(); /* 回合起点即活动 */
  xEventGroupClearBits(s_bw.events, BW_DONE | BW_ERROR | BW_DISCONNECTED);
  xSemaphoreGive(s_bw.lock);

  char body[96];
  snprintf(body, sizeof(body), "{\"t\":\"ptt.stop\",\"turnId\":\"%s\",\"u\":%u,\"frames\":%u}", s_bw.turn_id,
           (unsigned)s_bw.turn_u, (unsigned)s_bw.frame_seq);
  esp_err_t err = bw_send_text(body);
  if (err != ESP_OK) {
    xSemaphoreTake(s_bw.lock, portMAX_DELAY);
    s_bw.fin_waiting = 0;
    s_bw.fin_result = NULL;
    s_bw.fin_cb = NULL;
    xSemaphoreGive(s_bw.lock);
    return err;
  }

  /* 空闲超时等待（1s 切片,与 bb_adapter_client 同构）：事件到达续期,
   * 固定 deadline 不再拦腰砍断多步长回合。 */
  const int64_t wait_start_ms = bb_now_ms();
  EventBits_t bits = 0;
  for (;;) {
    bits = xEventGroupWaitBits(s_bw.events, BW_DONE | BW_ERROR | BW_DISCONNECTED, pdFALSE, pdFALSE,
                               pdMS_TO_TICKS(1000));
    if ((bits & (BW_DONE | BW_ERROR | BW_DISCONNECTED)) != 0U) break;
    const int64_t now_ms = bb_now_ms();
    xSemaphoreTake(s_bw.lock, portMAX_DELAY);
    const int64_t last_ms = s_bw.fin_last_activity_ms;
    xSemaphoreGive(s_bw.lock);
    if (now_ms - last_ms >= (int64_t)BBCLAW_STREAM_FINISH_IDLE_TIMEOUT_MS) break;
#if BBCLAW_STREAM_FINISH_MAX_TOTAL_MS > 0
    if (now_ms - wait_start_ms >= (int64_t)BBCLAW_STREAM_FINISH_MAX_TOTAL_MS) break;
#endif
  }
  xSemaphoreTake(s_bw.lock, portMAX_DELAY);
  s_bw.fin_waiting = 0;
  s_bw.fin_result = NULL;
  s_bw.fin_cb = NULL;
  s_bw.fin_user = NULL;
  esp_err_t ret = ESP_OK;
  if ((bits & BW_DONE) == 0U) {
    if (out_result->error_code[0] == '\0') {
      snprintf(out_result->error_code, sizeof(out_result->error_code), "%s",
               (bits & BW_DISCONNECTED) != 0U ? "WS_DISCONNECTED" : "VOICE_SESSION_TIMEOUT");
    }
    ret = ESP_FAIL;
  }
  xSemaphoreGive(s_bw.lock);
  ESP_LOGI(TAG, "finish %s ret=%s reply_chars=%u", s_bw.turn_id, esp_err_to_name(ret),
           (unsigned)strlen(out_result->reply_text));
  return ret;
}
