#pragma once

#include <stddef.h>
#include <stdint.h>

#include "esp_err.h"

/* 与 driver/i2c_master.h 的 typedef 逐字一致（C11 允许相同 typedef 重复）；
 * 前向声明是为了模拟器等无 IDF 头的环境也能 include 本头。 */
typedef struct i2c_master_bus_t *i2c_master_bus_handle_t;

esp_err_t bb_audio_init(void);
/** 板级共享 I2C 总线（audio init 后有效；触摸/IMU/RTC 共用）。NULL=未初始化。 */
i2c_master_bus_handle_t bb_audio_shared_i2c_bus(void);
esp_err_t bb_audio_start_tx(void);
esp_err_t bb_audio_stop_tx(void);
esp_err_t bb_audio_start_playback(void);
esp_err_t bb_audio_set_playback_sample_rate(int sample_rate);
esp_err_t bb_audio_stop_playback(void);
void bb_audio_request_playback_interrupt(void);
void bb_audio_clear_playback_interrupt(void);
int bb_audio_is_playback_interrupt_requested(void);
int bb_audio_is_playback_active(void);
esp_err_t bb_audio_read_pcm_frame(uint8_t* out_buf, size_t out_buf_len, size_t* out_read);
esp_err_t bb_audio_play_pcm_blocking(const uint8_t* pcm, size_t pcm_len);
esp_err_t bb_audio_play_test_tone(uint32_t freq_hz, uint32_t duration_ms, int16_t amplitude);
void bb_audio_set_volume_pct(int pct);
void bb_audio_set_speaker_enabled(int enabled);
int bb_audio_get_speaker_sw_enabled(void);
void bb_audio_poll_speaker_sw(void);
