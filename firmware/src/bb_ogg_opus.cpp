#include "bb_ogg_opus.h"

#include <algorithm>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <memory>
#include <vector>

#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "esp_check.h"
#include "esp_heap_caps.h"
#include "esp_log.h"
#include "esp_memory_utils.h"
#include "esp_system.h"
#include "opus.h"

namespace {

constexpr const char* TAG = "bb_ogg_opus";
constexpr int kMaxOpusPacketSize = 1500;
constexpr size_t kEncodedBufSize = 4096;
static SemaphoreHandle_t s_codec_mutex = nullptr;

struct EncoderState {
  OpusEncoder* enc = nullptr;
  void* enc_storage = nullptr;
  int sample_rate = 0;
  int channels = 0;
  int frame_duration_ms = 60;
  int frame_samples_per_channel = 0;
  int16_t* pending_pcm = nullptr;
  size_t pending_samples = 0;
  size_t pending_capacity = 0;
  uint32_t serial = 0;
  uint32_t page_seq = 0;
  uint64_t granule_pos = 0;
  bool headers_sent = false;
  uint32_t encode_calls = 0;
  uint8_t* encoded_buf = nullptr;  // heap-allocated OGG output buffer (PSRAM)
};

struct DecoderState {};

class CodecLockGuard {
 public:
  explicit CodecLockGuard(SemaphoreHandle_t mutex) : mutex_(mutex), locked_(false) {
    if (mutex_ != nullptr) {
      locked_ = (xSemaphoreTake(mutex_, portMAX_DELAY) == pdTRUE);
    }
  }

  ~CodecLockGuard() {
    if (locked_ && mutex_ != nullptr) {
      xSemaphoreGive(mutex_);
    }
  }

  bool ok() const { return locked_; }

 private:
  SemaphoreHandle_t mutex_;
  bool locked_;
};

static esp_err_t ensure_codec_mutex() {
  if (s_codec_mutex != nullptr) {
    return ESP_OK;
  }
  SemaphoreHandle_t mutex = xSemaphoreCreateMutex();
  if (mutex == nullptr) {
    ESP_LOGE(TAG, "codec mutex create failed");
    return ESP_ERR_NO_MEM;
  }
  s_codec_mutex = mutex;
  ESP_LOGI(TAG, "codec mutex created");
  return ESP_OK;
}

static const char* ptr_region(const void* ptr) {
  if (ptr == nullptr) {
    return "null";
  }
  if (esp_ptr_internal(ptr)) {
    return "internal";
  }
  if (esp_ptr_external_ram(ptr)) {
    return "psram";
  }
  return "other";
}

static void log_heap_snapshot(const char* phase) {
  ESP_LOGI(TAG,
           "heap %s total_free=%u total_largest=%u internal_free=%u internal_largest=%u spiram_free=%u "
           "spiram_largest=%u",
           phase != nullptr ? phase : "(unknown)", (unsigned)esp_get_free_heap_size(),
           (unsigned)heap_caps_get_largest_free_block(MALLOC_CAP_8BIT),
           (unsigned)heap_caps_get_free_size(MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT),
           (unsigned)heap_caps_get_largest_free_block(MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT),
           (unsigned)heap_caps_get_free_size(MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT),
           (unsigned)heap_caps_get_largest_free_block(MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT));
}

static uint32_t ogg_crc32(const uint8_t* data, size_t len) {
  uint32_t crc = 0;
  for (size_t i = 0; i < len; ++i) {
    crc ^= static_cast<uint32_t>(data[i]) << 24;
    for (int bit = 0; bit < 8; ++bit) {
      if ((crc & 0x80000000U) != 0U) {
        crc = (crc << 1) ^ 0x04C11DB7U;
      } else {
        crc <<= 1;
      }
    }
  }
  return crc;
}

static void write_le16(uint8_t* dst, uint16_t value) {
  dst[0] = static_cast<uint8_t>(value & 0xffU);
  dst[1] = static_cast<uint8_t>((value >> 8) & 0xffU);
}

static void write_le32(uint8_t* dst, uint32_t value) {
  dst[0] = static_cast<uint8_t>(value & 0xffU);
  dst[1] = static_cast<uint8_t>((value >> 8) & 0xffU);
  dst[2] = static_cast<uint8_t>((value >> 16) & 0xffU);
  dst[3] = static_cast<uint8_t>((value >> 24) & 0xffU);
}

static void write_le64(uint8_t* dst, uint64_t value) {
  for (int shift = 0; shift < 8; ++shift) {
    dst[shift] = static_cast<uint8_t>((value >> (shift * 8)) & 0xffU);
  }
}

static size_t build_opus_head_packet(uint8_t* out, int sample_rate, int channels) {
  memcpy(out, "OpusHead", 8);
  out[8] = 1;
  out[9] = static_cast<uint8_t>(channels);
  write_le16(out + 10, 0);
  write_le32(out + 12, static_cast<uint32_t>(sample_rate));
  write_le16(out + 16, 0);
  out[18] = 0;
  return 19U;
}

static size_t build_opus_tags_packet(uint8_t* out) {
  static constexpr char vendor[] = "BBClaw";
  memcpy(out, "OpusTags", 8);
  write_le32(out + 8, static_cast<uint32_t>(sizeof(vendor) - 1));
  memcpy(out + 12, vendor, sizeof(vendor) - 1);
  write_le32(out + 12 + sizeof(vendor) - 1, 0);
  return 16U + (sizeof(vendor) - 1U);
}

static esp_err_t append_ogg_page_to_buffer(uint8_t* out, size_t out_cap, size_t* out_len, const uint8_t* packet,
                                           size_t packet_len, uint8_t header_type, uint64_t granule_pos,
                                           uint32_t serial, uint32_t seq) {
  if (out == nullptr || out_len == nullptr) {
    return ESP_ERR_INVALID_ARG;
  }
  uint8_t segments[8];
  size_t segment_count = 0;
  size_t remaining = packet_len;
  while (remaining > 0) {
    size_t seg = std::min<size_t>(remaining, 255);
    if (segment_count >= sizeof(segments)) {
      return ESP_ERR_INVALID_SIZE;
    }
    segments[segment_count++] = static_cast<uint8_t>(seg);
    remaining -= seg;
  }
  if (packet_len > 0 && packet_len % 255U == 0U) {
    if (segment_count >= sizeof(segments)) {
      return ESP_ERR_INVALID_SIZE;
    }
    segments[segment_count++] = 0;
  }

  const size_t page_len = 27U + segment_count + packet_len;
  if (*out_len + page_len > out_cap) {
    ESP_LOGE(TAG, "ogg page overflow out_len=%u page_len=%u cap=%u", (unsigned)*out_len, (unsigned)page_len,
             (unsigned)out_cap);
    return ESP_ERR_INVALID_SIZE;
  }
  uint8_t* page = out + *out_len;
  memcpy(page, "OggS", 4);
  page[4] = 0;
  page[5] = header_type;
  write_le64(page + 6, granule_pos);
  write_le32(page + 14, serial);
  write_le32(page + 18, seq);
  write_le32(page + 22, 0);
  page[26] = static_cast<uint8_t>(segment_count);
  memcpy(page + 27, segments, segment_count);
  if (packet_len > 0U) {
    memcpy(page + 27 + segment_count, packet, packet_len);
  }
  const uint32_t crc = ogg_crc32(page, page_len);
  write_le32(page + 22, crc);
  *out_len += page_len;
  return ESP_OK;
}

static esp_err_t malloc_copy(const uint8_t* data, size_t data_len, uint8_t** out_data, size_t* out_len) {
  if (out_data == nullptr || out_len == nullptr) {
    return ESP_ERR_INVALID_ARG;
  }
  *out_data = nullptr;
  *out_len = 0;
  if (data == nullptr || data_len == 0U) {
    return ESP_OK;
  }
  uint8_t* buf = static_cast<uint8_t*>(heap_caps_malloc(data_len, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT));
  if (buf == nullptr) {
    buf = static_cast<uint8_t*>(heap_caps_malloc(data_len, MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT));
  }
  if (buf == nullptr) {
    return ESP_ERR_NO_MEM;
  }
  memcpy(buf, data, data_len);
  *out_data = buf;
  *out_len = data_len;
  return ESP_OK;
}

static esp_err_t encode_available_frames(EncoderState* state, uint8_t* out, size_t out_cap, size_t* out_len) {
  if (state == nullptr || state->enc == nullptr) {
    return ESP_ERR_INVALID_STATE;
  }
  if (out == nullptr || out_len == nullptr) {
    return ESP_ERR_INVALID_ARG;
  }
  *out_len = 0U;
  if (!state->headers_sent) {
    uint8_t head[19];
    const size_t head_len = build_opus_head_packet(head, state->sample_rate, state->channels);
    ESP_RETURN_ON_ERROR(append_ogg_page_to_buffer(out, out_cap, out_len, head, head_len, 0x02, 0, state->serial,
                                                  state->page_seq++),
                        TAG, "append opus head failed");
    uint8_t tags[22];
    const size_t tags_len = build_opus_tags_packet(tags);
    ESP_RETURN_ON_ERROR(append_ogg_page_to_buffer(out, out_cap, out_len, tags, tags_len, 0x00, 0, state->serial,
                                                  state->page_seq++),
                        TAG, "append opus tags failed");
    state->headers_sent = true;
  }

  const size_t frame_samples_total = static_cast<size_t>(state->frame_samples_per_channel * state->channels);
  while (state->pending_samples >= frame_samples_total) {
    state->encode_calls++;
    if (state->encode_calls <= 2U) {
      ESP_LOGI(TAG,
               "encode begin seq=%u enc=%p(%s) pcm=%p(%s) pending=%u frame_total=%u frame_per_ch=%u heap_int=%u "
               "heap_spiram=%u",
               (unsigned)state->encode_calls, state->enc, ptr_region(state->enc), state->pending_pcm,
               ptr_region(state->pending_pcm), (unsigned)state->pending_samples, (unsigned)frame_samples_total,
               (unsigned)state->frame_samples_per_channel,
               (unsigned)heap_caps_get_free_size(MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT),
               (unsigned)heap_caps_get_free_size(MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT));
    }
    struct {
      uint32_t guard_before;
      uint8_t packet[kMaxOpusPacketSize];
      uint32_t guard_after;
    } packet_buf = {.guard_before = 0x13579BDFU, .guard_after = 0x2468ACE0U};
    const int packet_len = opus_encode(state->enc, state->pending_pcm, state->frame_samples_per_channel,
                                       packet_buf.packet, sizeof(packet_buf.packet));
    if (packet_len < 0) {
      ESP_LOGE(TAG, "opus_encode failed code=%d", packet_len);
      return ESP_FAIL;
    }
    if (packet_buf.guard_before != 0x13579BDFU || packet_buf.guard_after != 0x2468ACE0U) {
      ESP_LOGE(TAG, "opus packet buffer overflow seq=%u before=0x%08x after=0x%08x packet_len=%d",
               (unsigned)state->encode_calls, packet_buf.guard_before, packet_buf.guard_after, packet_len);
      return ESP_ERR_INVALID_SIZE;
    }
    if (state->encode_calls <= 2U) {
      ESP_LOGI(TAG, "encode done seq=%u packet_len=%d pending_before_shift=%u", (unsigned)state->encode_calls,
               packet_len, (unsigned)state->pending_samples);
    }
    if (state->pending_samples > frame_samples_total) {
      memmove(state->pending_pcm, state->pending_pcm + frame_samples_total,
              (state->pending_samples - frame_samples_total) * sizeof(int16_t));
    }
    state->pending_samples -= frame_samples_total;
    state->granule_pos += static_cast<uint64_t>(state->frame_samples_per_channel);
    ESP_RETURN_ON_ERROR(append_ogg_page_to_buffer(out, out_cap, out_len, packet_buf.packet, (size_t)packet_len, 0x00,
                                                  state->granule_pos, state->serial, state->page_seq++),
                        TAG, "append opus page failed");
  }
  return ESP_OK;
}

static uint32_t read_le32(const uint8_t* src) {
  return static_cast<uint32_t>(src[0]) | (static_cast<uint32_t>(src[1]) << 8) | (static_cast<uint32_t>(src[2]) << 16) |
         (static_cast<uint32_t>(src[3]) << 24);
}

static esp_err_t append_pcm_samples(int16_t** pcm_buf, size_t* pcm_samples, size_t* pcm_cap_samples,
                                    const int16_t* chunk, size_t chunk_samples) {
  if (pcm_buf == nullptr || pcm_samples == nullptr || pcm_cap_samples == nullptr) {
    return ESP_ERR_INVALID_ARG;
  }
  if (chunk == nullptr || chunk_samples == 0U) {
    return ESP_OK;
  }
  size_t need = *pcm_samples + chunk_samples;
  if (need > *pcm_cap_samples) {
    size_t new_cap = (*pcm_cap_samples == 0U) ? chunk_samples : *pcm_cap_samples;
    while (new_cap < need) {
      size_t grown = new_cap * 2U;
      if (grown <= new_cap) {
        new_cap = need;
        break;
      }
      new_cap = grown;
    }
    int16_t* new_buf = static_cast<int16_t*>(
        heap_caps_realloc(*pcm_buf, new_cap * sizeof(int16_t), MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT));
    if (new_buf == nullptr) {
      new_buf = static_cast<int16_t*>(
          heap_caps_realloc(*pcm_buf, new_cap * sizeof(int16_t), MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT));
    }
    if (new_buf == nullptr) {
      log_heap_snapshot("decode append alloc failed");
      return ESP_ERR_NO_MEM;
    }
    *pcm_buf = new_buf;
    *pcm_cap_samples = new_cap;
  }
  memcpy(*pcm_buf + *pcm_samples, chunk, chunk_samples * sizeof(int16_t));
  *pcm_samples += chunk_samples;
  return ESP_OK;
}

static int decode_packets(const uint8_t* ogg_data, size_t ogg_len, int output_sample_rate, int output_channels,
                          int16_t** out_pcm_buf, size_t* out_pcm_samples, int* source_sample_rate,
                          int* source_channels, DecoderState* decoder_state) {
  if (ogg_data == nullptr || ogg_len < 27U) {
    return ESP_ERR_INVALID_ARG;
  }
  if (out_pcm_buf == nullptr || out_pcm_samples == nullptr) {
    return ESP_ERR_INVALID_ARG;
  }
  *out_pcm_buf = nullptr;
  *out_pcm_samples = 0U;

  int stream_sample_rate = 16000;
  int stream_channels = 1;
  OpusDecoder* decoder = nullptr;
  std::vector<uint8_t> packet_buf;
  int16_t* pcm_buf = nullptr;
  size_t pcm_samples = 0U;
  size_t pcm_cap_samples = 0U;

  size_t offset = 0;
  while (offset + 27U <= ogg_len) {
    const uint8_t* page = ogg_data + offset;
    if (memcmp(page, "OggS", 4) != 0) {
      free(pcm_buf);
      if (decoder != nullptr) {
        opus_decoder_destroy(decoder);
      }
      return ESP_FAIL;
    }
    const uint8_t page_segments = page[26];
    if (offset + 27U + page_segments > ogg_len) {
      free(pcm_buf);
      if (decoder != nullptr) {
        opus_decoder_destroy(decoder);
      }
      return ESP_FAIL;
    }
    const uint8_t* segment_table = page + 27;
    size_t body_len = 0;
    for (uint8_t i = 0; i < page_segments; ++i) {
      body_len += segment_table[i];
    }
    if (offset + 27U + page_segments + body_len > ogg_len) {
      free(pcm_buf);
      if (decoder != nullptr) {
        opus_decoder_destroy(decoder);
      }
      return ESP_FAIL;
    }
    const uint8_t* body = segment_table + page_segments;
    size_t body_off = 0;
    for (uint8_t i = 0; i < page_segments; ++i) {
      const uint8_t seg_len = segment_table[i];
      packet_buf.insert(packet_buf.end(), body + body_off, body + body_off + seg_len);
      body_off += seg_len;
      if (seg_len == 255U) {
        continue;
      }

      if (packet_buf.size() >= 8U && memcmp(packet_buf.data(), "OpusHead", 8) == 0) {
        if (packet_buf.size() >= 19U) {
          stream_channels = packet_buf[9];
          stream_sample_rate = static_cast<int>(read_le32(packet_buf.data() + 12));
        }
      } else if (packet_buf.size() >= 8U && memcmp(packet_buf.data(), "OpusTags", 8) == 0) {
        // no-op
      } else if (!packet_buf.empty()) {
        if (decoder == nullptr) {
          int error = 0;
          decoder = opus_decoder_create(stream_sample_rate, stream_channels, &error);
          if (decoder == nullptr || error != OPUS_OK) {
            free(pcm_buf);
            if (decoder != nullptr) {
              opus_decoder_destroy(decoder);
            }
            return ESP_FAIL;
          }
        }
        const int frame_samples = opus_decoder_get_nb_samples(decoder, packet_buf.data(), packet_buf.size());
        if (frame_samples <= 0) {
          free(pcm_buf);
          opus_decoder_destroy(decoder);
          return ESP_FAIL;
        }
        size_t frame_pcm_samples = static_cast<size_t>(frame_samples * stream_channels);
        int16_t* frame_pcm = static_cast<int16_t*>(
            heap_caps_malloc(frame_pcm_samples * sizeof(int16_t), MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT));
        if (frame_pcm == nullptr) {
          frame_pcm = static_cast<int16_t*>(
              heap_caps_malloc(frame_pcm_samples * sizeof(int16_t), MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT));
        }
        if (frame_pcm == nullptr) {
          free(pcm_buf);
          opus_decoder_destroy(decoder);
          log_heap_snapshot("decode frame alloc failed");
          return ESP_ERR_NO_MEM;
        }
        const int decoded =
            opus_decode(decoder, packet_buf.data(), static_cast<opus_int32>(packet_buf.size()), frame_pcm,
                        frame_samples, 0);
        if (decoded < 0) {
          free(frame_pcm);
          free(pcm_buf);
          opus_decoder_destroy(decoder);
          return ESP_FAIL;
        }
        esp_err_t append_err = append_pcm_samples(&pcm_buf, &pcm_samples, &pcm_cap_samples, frame_pcm,
                                                 static_cast<size_t>(decoded * stream_channels));
        free(frame_pcm);
        if (append_err != ESP_OK) {
          free(pcm_buf);
          opus_decoder_destroy(decoder);
          return append_err;
        }
      }
      packet_buf.clear();
    }
    offset += 27U + page_segments + body_len;
  }

  if (decoder != nullptr) {
    opus_decoder_destroy(decoder);
  }

  if (source_sample_rate != nullptr) {
    *source_sample_rate = stream_sample_rate;
  }
  if (source_channels != nullptr) {
    *source_channels = stream_channels;
  }
  if (stream_channels != output_channels) {
    free(pcm_buf);
    return ESP_ERR_NOT_SUPPORTED;
  }

  if (stream_sample_rate != output_sample_rate) {
    (void)decoder_state;
    free(pcm_buf);
    return ESP_ERR_NOT_SUPPORTED;
  }

  *out_pcm_buf = pcm_buf;
  *out_pcm_samples = pcm_samples;
  return ESP_OK;
}

}  // namespace

/* Pre-allocated internal RAM buffer for Opus encoder to avoid heap fragmentation
 * caused by WebSocket/TLS allocations splitting the largest free block. */
static uint8_t s_enc_static_buf[25000] __attribute__((aligned(16)));
static int s_enc_static_in_use = 0;

struct bb_ogg_opus_encoder {
  EncoderState state;
};

struct bb_ogg_opus_decoder {
  DecoderState state;
};

extern "C" {

bb_ogg_opus_encoder_t* bb_ogg_opus_encoder_create(int sample_rate, int channels, int frame_duration_ms) {
  if (sample_rate <= 0 || channels <= 0 || frame_duration_ms <= 0) {
    return nullptr;
  }
  if (ensure_codec_mutex() != ESP_OK) {
    return nullptr;
  }
  auto* wrapper = new bb_ogg_opus_encoder_t();
  if (wrapper == nullptr) {
    return nullptr;
  }

  const int enc_size = opus_encoder_get_size(channels);
  if (enc_size <= 0) {
    ESP_LOGE(TAG, "encoder get size failed channels=%d enc_size=%d", channels, enc_size);
    delete wrapper;
    return nullptr;
  }
  const size_t pending_bytes =
      static_cast<size_t>((sample_rate * frame_duration_ms / 1000) * channels * (int)sizeof(int16_t));
  ESP_LOGI(TAG, "encoder create request sample_rate=%d channels=%d frame_ms=%d enc_size=%d pending_bytes=%u",
           sample_rate, channels, frame_duration_ms, enc_size, (unsigned)pending_bytes);
  log_heap_snapshot("encoder create begin");

  /* Encoder state MUST stay in internal RAM — Opus SILK uses function pointer
   * tables embedded in the encoder struct; PSRAM is not executable on ESP32-S3
   * and causes InstructionFetchError.  Use a static internal buffer to avoid
   * fragmentation from WebSocket/TLS allocations. */
  void* enc_mem = nullptr;
  const char* enc_storage_region = "static_internal";
  if (!s_enc_static_in_use && (size_t)enc_size <= sizeof(s_enc_static_buf)) {
    enc_mem = s_enc_static_buf;
    s_enc_static_in_use = 1;
  } else {
    enc_mem = heap_caps_malloc((size_t)enc_size, MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT);
    enc_storage_region = "internal";
  }
  wrapper->state.enc_storage = enc_mem;
  if (wrapper->state.enc_storage == nullptr) {
    ESP_LOGE(TAG, "encoder storage alloc failed size=%d", enc_size);
    log_heap_snapshot("encoder storage alloc failed");
    delete wrapper;
    return nullptr;
  }
  wrapper->state.enc = static_cast<OpusEncoder*>(wrapper->state.enc_storage);
  CodecLockGuard codec_lock(s_codec_mutex);
  if (!codec_lock.ok()) {
    heap_caps_free(wrapper->state.enc_storage);
    wrapper->state.enc_storage = nullptr;
    delete wrapper;
    return nullptr;
  }
  const int error = opus_encoder_init(wrapper->state.enc, sample_rate, channels, OPUS_APPLICATION_VOIP);
  if (error != OPUS_OK) {
    heap_caps_free(wrapper->state.enc_storage);
    wrapper->state.enc_storage = nullptr;
    wrapper->state.enc = nullptr;
    delete wrapper;
    return nullptr;
  }
  wrapper->state.sample_rate = sample_rate;
  wrapper->state.channels = channels;
  wrapper->state.frame_duration_ms = frame_duration_ms;
  wrapper->state.frame_samples_per_channel = sample_rate * frame_duration_ms / 1000;
  wrapper->state.pending_capacity = static_cast<size_t>(wrapper->state.frame_samples_per_channel * channels);
  wrapper->state.pending_pcm = static_cast<int16_t*>(
      heap_caps_malloc(wrapper->state.pending_capacity * sizeof(int16_t), MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT));
  const char* pending_region = "psram";
  if (wrapper->state.pending_pcm == nullptr) {
    ESP_LOGW(TAG, "pending pcm psram alloc failed bytes=%u, try internal",
             (unsigned)(wrapper->state.pending_capacity * sizeof(int16_t)));
    wrapper->state.pending_pcm = static_cast<int16_t*>(
        heap_caps_malloc(wrapper->state.pending_capacity * sizeof(int16_t), MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT));
    pending_region = "internal";
  }
  if (wrapper->state.pending_pcm == nullptr) {
    heap_caps_free(wrapper->state.enc_storage);
    wrapper->state.enc_storage = nullptr;
    wrapper->state.enc = nullptr;
    ESP_LOGE(TAG, "pending pcm alloc failed bytes=%u", (unsigned)(wrapper->state.pending_capacity * sizeof(int16_t)));
    log_heap_snapshot("pending pcm alloc failed");
    delete wrapper;
    return nullptr;
  }
  wrapper->state.encoded_buf = static_cast<uint8_t*>(
      heap_caps_malloc(kEncodedBufSize, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT));
  if (wrapper->state.encoded_buf == nullptr) {
    heap_caps_free(wrapper->state.enc_storage);
    heap_caps_free(wrapper->state.pending_pcm);
    wrapper->state.enc_storage = nullptr;
    wrapper->state.enc = nullptr;
    wrapper->state.pending_pcm = nullptr;
    ESP_LOGE(TAG, "encoded buf alloc failed");
    delete wrapper;
    return nullptr;
  }
  wrapper->state.serial = static_cast<uint32_t>(sample_rate ^ channels ^ frame_duration_ms ^ 0x4242434cU);
  opus_encoder_ctl(wrapper->state.enc, OPUS_SET_DTX(0));
  opus_encoder_ctl(wrapper->state.enc, OPUS_SET_COMPLEXITY(0));
  log_heap_snapshot("encoder create ready");
  ESP_LOGI(TAG,
           "encoder create enc=%p(%s/%s) pending=%p(%s/%s) sample_rate=%d channels=%d frame_ms=%d frame_total=%u "
           "heap_int=%u heap_spiram=%u",
           wrapper->state.enc, ptr_region(wrapper->state.enc), enc_storage_region, wrapper->state.pending_pcm,
           ptr_region(wrapper->state.pending_pcm), pending_region, sample_rate, channels, frame_duration_ms,
           (unsigned)wrapper->state.pending_capacity,
           (unsigned)heap_caps_get_free_size(MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT),
           (unsigned)heap_caps_get_free_size(MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT));
  return wrapper;
}

void bb_ogg_opus_encoder_destroy(bb_ogg_opus_encoder_t* encoder) {
  if (encoder == nullptr) {
    return;
  }
  encoder->state.enc = nullptr;
  if (encoder->state.enc_storage != nullptr) {
    if (encoder->state.enc_storage == s_enc_static_buf) {
      s_enc_static_in_use = 0;
    } else {
      heap_caps_free(encoder->state.enc_storage);
    }
    encoder->state.enc_storage = nullptr;
  }
  if (encoder->state.pending_pcm != nullptr) {
    heap_caps_free(encoder->state.pending_pcm);
    encoder->state.pending_pcm = nullptr;
  }
  if (encoder->state.encoded_buf != nullptr) {
    heap_caps_free(encoder->state.encoded_buf);
    encoder->state.encoded_buf = nullptr;
  }
  delete encoder;
}

esp_err_t bb_ogg_opus_encoder_append_pcm16(bb_ogg_opus_encoder_t* encoder, const int16_t* pcm, size_t sample_count,
                                           uint8_t** out_data, size_t* out_len) {
  if (encoder == nullptr || out_data == nullptr || out_len == nullptr) {
    return ESP_ERR_INVALID_ARG;
  }
  ESP_RETURN_ON_ERROR(ensure_codec_mutex(), TAG, "codec mutex unavailable");
  *out_data = nullptr;
  *out_len = 0;
  if (pcm != nullptr && sample_count > 0U) {
    if (sample_count > encoder->state.pending_capacity - encoder->state.pending_samples) {
      ESP_LOGE(TAG, "append overflow sample_count=%u pending=%u capacity=%u pcm=%p(%s)", (unsigned)sample_count,
               (unsigned)encoder->state.pending_samples, (unsigned)encoder->state.pending_capacity, pcm,
               ptr_region(pcm));
      return ESP_ERR_INVALID_SIZE;
    }
    memcpy(encoder->state.pending_pcm + encoder->state.pending_samples, pcm, sample_count * sizeof(int16_t));
    encoder->state.pending_samples += sample_count;
  }
  size_t encoded_len = 0;
  CodecLockGuard codec_lock(s_codec_mutex);
  if (!codec_lock.ok()) {
    return ESP_ERR_INVALID_STATE;
  }
  esp_err_t err = encode_available_frames(&encoder->state, encoder->state.encoded_buf, kEncodedBufSize, &encoded_len);
  if (err != ESP_OK) {
    return err;
  }
  return malloc_copy(encoder->state.encoded_buf, encoded_len, out_data, out_len);
}

esp_err_t bb_ogg_opus_encoder_flush(bb_ogg_opus_encoder_t* encoder, uint8_t** out_data, size_t* out_len) {
  if (encoder == nullptr || out_data == nullptr || out_len == nullptr) {
    return ESP_ERR_INVALID_ARG;
  }
  ESP_RETURN_ON_ERROR(ensure_codec_mutex(), TAG, "codec mutex unavailable");
  *out_data = nullptr;
  *out_len = 0;
  const size_t frame_samples_total =
      static_cast<size_t>(encoder->state.frame_samples_per_channel * encoder->state.channels);
  if (encoder->state.pending_samples > 0U && encoder->state.pending_samples < frame_samples_total) {
    memset(encoder->state.pending_pcm + encoder->state.pending_samples, 0,
           (frame_samples_total - encoder->state.pending_samples) * sizeof(int16_t));
    encoder->state.pending_samples = frame_samples_total;
  }
  size_t encoded_len = 0;
  CodecLockGuard codec_lock(s_codec_mutex);
  if (!codec_lock.ok()) {
    return ESP_ERR_INVALID_STATE;
  }
  esp_err_t err = encode_available_frames(&encoder->state, encoder->state.encoded_buf, kEncodedBufSize, &encoded_len);
  if (err != ESP_OK) {
    return err;
  }
  return malloc_copy(encoder->state.encoded_buf, encoded_len, out_data, out_len);
}

bb_ogg_opus_decoder_t* bb_ogg_opus_decoder_create(void) {
  return new bb_ogg_opus_decoder_t();
}

void bb_ogg_opus_decoder_destroy(bb_ogg_opus_decoder_t* decoder) {
  delete decoder;
}

esp_err_t bb_ogg_opus_decoder_decode_all(bb_ogg_opus_decoder_t* decoder, const uint8_t* ogg_data, size_t ogg_len,
                                         int output_sample_rate, int output_channels, uint8_t** out_pcm,
                                         size_t* out_pcm_len, int* source_sample_rate, int* source_channels) {
  if (decoder == nullptr || ogg_data == nullptr || out_pcm == nullptr || out_pcm_len == nullptr) {
    return ESP_ERR_INVALID_ARG;
  }
  ESP_RETURN_ON_ERROR(ensure_codec_mutex(), TAG, "codec mutex unavailable");
  *out_pcm = nullptr;
  *out_pcm_len = 0;

  CodecLockGuard codec_lock(s_codec_mutex);
  if (!codec_lock.ok()) {
    return ESP_ERR_INVALID_STATE;
  }
  int16_t* pcm = nullptr;
  size_t pcm_samples = 0U;
  int err = decode_packets(ogg_data, ogg_len, output_sample_rate, output_channels, &pcm, &pcm_samples,
                           source_sample_rate, source_channels, &decoder->state);
  if (err != ESP_OK) {
    return static_cast<esp_err_t>(err);
  }
  if (pcm == nullptr || pcm_samples == 0U) {
    free(pcm);
    return ESP_OK;
  }
  const size_t bytes = pcm_samples * sizeof(int16_t);
  *out_pcm = reinterpret_cast<uint8_t*>(pcm);
  *out_pcm_len = bytes;
  return ESP_OK;
}

void bb_ogg_opus_free(void* ptr) {
  free(ptr);
}

}  // extern "C"
