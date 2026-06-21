#pragma once

/*
 * bbwire/2 device protocol — firmware client (see
 * adapter_v2/docs/device-protocol.md and adapter_v2/internal/devicews/frames.go).
 *
 * One client-dialed WebSocket to the v2 adapter at /v2/dev/ws. The WS frame
 * opcode is the type discriminator: TEXT frames carry JSON control objects,
 * BINARY frames carry an 8-byte little-endian header + raw codec audio (no
 * base64). This header declares ONLY the wire format (the most byte-order-error-
 * prone part) as static-inline helpers that mirror the Go server's frames.go
 * exactly, so a unit/bench check can pin the encoding. The WS client lifecycle
 * and the bb_adapter_* integration land in bb_bbwire2.c (increment 2).
 */

#include <stdint.h>
#include <string.h>

/* Protocol version announced in the hello handshake (hello.proto). */
#define BB_BBWIRE2_PROTO 2

/* streamKind — binary header byte 0. */
#define BB_BBWIRE2_STREAM_UPLINK_MIC   0x01 /* device → adapter: mic audio */
#define BB_BBWIRE2_STREAM_DOWNLINK_TTS 0x02 /* adapter → device: reply audio */

/* codec — binary header byte 1. */
#define BB_BBWIRE2_CODEC_OPUS  0x01
#define BB_BBWIRE2_CODEC_PCM16 0x02

/* flags — binary header bytes 6-7 (bitfield). bit0 marks the last frame of a
 * PLAYABLE AUDIO UNIT (one sentence under streaming TTS, or the whole reply
 * under one-shot): play the accumulated buffer and reset on it. The TURN end is
 * the turn{idle} control frame, NOT this flag. */
#define BB_BBWIRE2_FLAG_FINAL    (1u << 0)
#define BB_BBWIRE2_FLAG_KEYFRAME (1u << 1) /* opus config / keyframe present */

/* Fixed little-endian header on every BINARY frame. */
#define BB_BBWIRE2_BIN_HEADER_LEN 8

typedef struct {
  uint8_t  stream_kind; /* BB_BBWIRE2_STREAM_* */
  uint8_t  codec;       /* BB_BBWIRE2_CODEC_*  */
  uint16_t turn_seq;    /* mirrors the JSON turn's `u` */
  uint16_t frame_seq;   /* per-turn monotonic; gap/reorder/resume */
  uint16_t flags;       /* BB_BBWIRE2_FLAG_*   */
} bb_bbwire2_bin_header_t;

/* Encode the 8-byte header into out[0..7] (little-endian), mirroring Go
 * binHeader.encode. out must have room for BB_BBWIRE2_BIN_HEADER_LEN bytes. */
static inline void bb_bbwire2_header_encode(const bb_bbwire2_bin_header_t* h, uint8_t* out) {
  out[0] = h->stream_kind;
  out[1] = h->codec;
  out[2] = (uint8_t)(h->turn_seq & 0xFF);
  out[3] = (uint8_t)((h->turn_seq >> 8) & 0xFF);
  out[4] = (uint8_t)(h->frame_seq & 0xFF);
  out[5] = (uint8_t)((h->frame_seq >> 8) & 0xFF);
  out[6] = (uint8_t)(h->flags & 0xFF);
  out[7] = (uint8_t)((h->flags >> 8) & 0xFF);
}

/* Decode the 8-byte header from a BINARY frame, mirroring Go decodeBinFrame.
 * Returns the payload length (frame_len - 8) and sets *payload to the audio
 * bytes; returns -1 (and leaves *payload NULL) if the frame is too short. */
static inline int bb_bbwire2_header_decode(const uint8_t* frame, int frame_len, bb_bbwire2_bin_header_t* out_h,
                                           const uint8_t** out_payload) {
  if (out_payload) {
    *out_payload = NULL;
  }
  if (frame == NULL || frame_len < BB_BBWIRE2_BIN_HEADER_LEN) {
    return -1;
  }
  out_h->stream_kind = frame[0];
  out_h->codec       = frame[1];
  out_h->turn_seq    = (uint16_t)(frame[2] | (frame[3] << 8));
  out_h->frame_seq   = (uint16_t)(frame[4] | (frame[5] << 8));
  out_h->flags       = (uint16_t)(frame[6] | (frame[7] << 8));
  if (out_payload) {
    *out_payload = frame + BB_BBWIRE2_BIN_HEADER_LEN;
  }
  return frame_len - BB_BBWIRE2_BIN_HEADER_LEN;
}

/*
 * ── Client API (implemented in bb_bbwire2.c, increment 2) ──────────────────
 *
 * These mirror the v1/cloud_saas surface that bb_adapter_client.c's stream
 * functions branch into, so the radio app keeps calling bb_adapter_* unchanged
 * (the adapter is transparent to the device). Declared here; defined later.
 *
 *   bb_bbwire2_connect()       — dial /v2/dev/ws, hello/hello.ok handshake.
 *   bb_bbwire2_ptt_start(u)    — open uplink turn u.
 *   bb_bbwire2_send_mic(...)   — one BINARY mic frame (header + opus/pcm16).
 *   bb_bbwire2_ptt_stop(...)   — close the utterance; the reply streams back via
 *                                the bb_finish_stream_event_cb_t the caller passed.
 *
 * The downlink dispatch (asr.final / reply.delta / reply.end / turn / error +
 * BINARY TTS frames) maps onto bb_finish_stream_event_t exactly as the cloud
 * path does, so bb_radio_app.c needs no new event handling.
 */
