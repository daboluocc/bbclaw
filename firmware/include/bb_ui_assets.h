#pragma once

#include <stdint.h>

typedef struct {
  uint8_t width;
  uint8_t height;
  uint8_t stride;
  const uint8_t* bits;
} bb_ui_mono_bitmap_t;

extern const bb_ui_mono_bitmap_t BB_UI_LOGO_CLAW_16;
extern const bb_ui_mono_bitmap_t BB_UI_ICON_READY_12;
extern const bb_ui_mono_bitmap_t BB_UI_ICON_TX_12;
extern const bb_ui_mono_bitmap_t BB_UI_ICON_RX_12;
extern const bb_ui_mono_bitmap_t BB_UI_ICON_ERR_12;
extern const bb_ui_mono_bitmap_t BB_UI_ICON_TASK_12;
extern const bb_ui_mono_bitmap_t BB_UI_ICON_SPEAK_12;
extern const bb_ui_mono_bitmap_t BB_UI_ICON_REC_12_1;
extern const bb_ui_mono_bitmap_t BB_UI_ICON_REC_12_2;
extern const bb_ui_mono_bitmap_t BB_UI_ICON_REC_12_3;
