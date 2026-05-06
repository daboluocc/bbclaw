#pragma once

#include <stddef.h>
#include <stdint.h>

#include "esp_err.h"

esp_err_t bb_audio_init(void);
esp_err_t bb_audio_start_tx(void);
esp_err_t bb_audio_stop_tx(void);
esp_err_t bb_audio_start_playback(void);
esp_err_t bb_audio_set_playback_sample_rate(int sample_rate);
esp_err_t bb_audio_stop_playback(void);
void bb_audio_request_playback_interrupt(void);
void bb_audio_clear_playback_interrupt(void);
int bb_audio_is_playback_interrupt_requested(void);
esp_err_t bb_audio_read_pcm_frame(uint8_t* out_buf, size_t out_buf_len, size_t* out_read);
esp_err_t bb_audio_play_pcm_blocking(const uint8_t* pcm, size_t pcm_len);
esp_err_t bb_audio_play_test_tone(uint32_t freq_hz, uint32_t duration_ms, int16_t amplitude);
void bb_audio_set_volume_pct(int pct);
void bb_audio_set_speaker_enabled(int enabled);
int bb_audio_get_speaker_sw_enabled(void);
void bb_audio_poll_speaker_sw(void);
