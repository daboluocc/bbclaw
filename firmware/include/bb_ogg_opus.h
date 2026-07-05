#pragma once

#include <stddef.h>
#include <stdint.h>

#include "esp_err.h"

#ifdef __cplusplus
extern "C" {
#endif

typedef struct bb_ogg_opus_encoder bb_ogg_opus_encoder_t;
typedef struct bb_ogg_opus_decoder bb_ogg_opus_decoder_t;

bb_ogg_opus_encoder_t* bb_ogg_opus_encoder_create(int sample_rate, int channels, int frame_duration_ms);
void bb_ogg_opus_encoder_destroy(bb_ogg_opus_encoder_t* encoder);
esp_err_t bb_ogg_opus_encoder_append_pcm16(bb_ogg_opus_encoder_t* encoder, const int16_t* pcm, size_t sample_count,
                                           uint8_t** out_data, size_t* out_len);
esp_err_t bb_ogg_opus_encoder_flush(bb_ogg_opus_encoder_t* encoder, uint8_t** out_data, size_t* out_len);
/** 录音场景:显式 CBR 码率+DTX（create 之后调;PTT 路径不调用,默认行为不变） */
esp_err_t bb_ogg_opus_encoder_set_bitrate(bb_ogg_opus_encoder_t* encoder, int bitrate_bps, int enable_dtx);

bb_ogg_opus_decoder_t* bb_ogg_opus_decoder_create(void);
void bb_ogg_opus_decoder_destroy(bb_ogg_opus_decoder_t* decoder);
esp_err_t bb_ogg_opus_decoder_decode_all(bb_ogg_opus_decoder_t* decoder, const uint8_t* ogg_data, size_t ogg_len,
                                         int output_sample_rate, int output_channels, uint8_t** out_pcm,
                                         size_t* out_pcm_len, int* source_sample_rate, int* source_channels);

void bb_ogg_opus_free(void* ptr);

#ifdef __cplusplus
}
#endif
