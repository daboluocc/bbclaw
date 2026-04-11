#pragma once

#include <stdint.h>

#include "esp_err.h"

esp_err_t bb_display_init(void);
esp_err_t bb_display_show_status(const char* status_line);
/** 主界面：仅展示「自己说的」与「助手回复」，短信式上下两行（ME / AI） */
esp_err_t bb_display_show_chat_turn(const char* user_said, const char* assistant_reply);
esp_err_t bb_display_upsert_chat_turn(const char* user_said, const char* assistant_reply, int finalize);

/** 查看更早一轮 / 较新一轮（需 GPIO 等调用；勿在 ISR 内直接调用） */
esp_err_t bb_display_chat_prev_turn(void);
esp_err_t bb_display_chat_next_turn(void);
/** 当前轮内长文上下滚动（默认作用在 AI 栏，可用 focus 切换） */
esp_err_t bb_display_chat_scroll_down(void);
esp_err_t bb_display_chat_scroll_up(void);
void bb_display_chat_focus_me(void);
void bb_display_chat_focus_ai(void);
void bb_display_set_locked(int locked);
/** TTS 播放状态：播放期间抑制 scroll reset 和 auto-scroll 循环回顶部 */
void bb_display_set_tts_playing(int playing);
/** TTS 播放到新句子时，滚动到该句子在回复文本中的位置 */
void bb_display_set_tts_sentence(const char* sentence_text);
/** PTT 录音阶段的实时输入电平（0-100）与是否检测到有效声音 */
void bb_display_set_record_level(uint8_t level_pct, int voiced);
/** 状态栏电池信息；supported=0 时整个组件隐藏，available=0 时显示占位 */
void bb_display_set_battery(int supported, int available, int percent, int low);
