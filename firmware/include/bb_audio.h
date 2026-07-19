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

/** 注册板级 PA 功放开关钩子（GPIO 直连板不需要，走内建 GPIO 路径）。注册后
 *  功放改为「播放时开、空闲延迟关」的门控，消除常开底噪/电流声。fn(1)=开 fn(0)=关。
 *  注册即把 PA 置为关闭态。用于 PCA9557 / XL9555 等经 IO 扩展控制功放的板子。 */
void bb_audio_set_pa_control(void (*fn)(int on));
void bb_audio_request_playback_interrupt(void);
void bb_audio_clear_playback_interrupt(void);
int bb_audio_is_playback_interrupt_requested(void);
/* 回放暂停：置位后 bb_audio_play_pcm_blocking 在 chunk 边界原地喂静音、位置冻结,
 * 清位从同一样本继续。录音回放页 transport 用;TTS 不置位。 */
void bb_audio_set_playback_paused(int paused);
int bb_audio_is_playback_paused(void);
int bb_audio_is_playback_active(void);
esp_err_t bb_audio_read_pcm_frame(uint8_t* out_buf, size_t out_buf_len, size_t* out_read);
/* 录音态双麦合成:enable=1 时立体声降混对 MIC1/MIC2 求均值(同源两路 → 不相干
 * 噪声 -3dB / SNR +3dB,并消除逐帧挑麦跳变);enable=0 恢复挑响一路(对话/PTT 近场
 * 常有一只麦更近,保留原语义)。录音会话 start 时置 1,stop 时置 0。 */
void bb_audio_set_recorder_mix(int enable);
esp_err_t bb_audio_play_pcm_blocking(const uint8_t* pcm, size_t pcm_len);
esp_err_t bb_audio_play_test_tone(uint32_t freq_hz, uint32_t duration_ms, int16_t amplitude);
void bb_audio_set_volume_pct(int pct);
void bb_audio_set_speaker_enabled(int enabled);
int bb_audio_get_speaker_sw_enabled(void);
void bb_audio_poll_speaker_sw(void);
