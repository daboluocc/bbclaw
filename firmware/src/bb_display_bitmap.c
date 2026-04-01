#include "bb_display.h"

#include <ctype.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>

#include "bb_config.h"
#include "bb_ui_assets.h"
#include "driver/gpio.h"
#include "driver/spi_master.h"
#include "esp_lcd_panel_io.h"
#include "esp_lcd_panel_ops.h"
#include "esp_lcd_panel_vendor.h"
#include "esp_check.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/queue.h"
#include "freertos/task.h"

static const char* TAG = "bb_display";

#define DISP_W BBCLAW_ST7789_WIDTH
#define DISP_H BBCLAW_ST7789_HEIGHT
#define DISP_X_GAP BBCLAW_ST7789_X_GAP
#define DISP_Y_GAP BBCLAW_ST7789_Y_GAP
#define DISP_PCLK_HZ BBCLAW_ST7789_PCLK_HZ
#define DISP_SWAP_XY BBCLAW_ST7789_SWAP_XY
#define DISP_MIRROR_X BBCLAW_ST7789_MIRROR_X
#define DISP_MIRROR_Y BBCLAW_ST7789_MIRROR_Y
#define DISP_INVERT_COLOR BBCLAW_ST7789_INVERT_COLOR
#if BBCLAW_ST7789_RGB_ORDER_BGR
#define DISP_RGB_ORDER LCD_RGB_ELEMENT_ORDER_BGR
#define DISP_RGB_ORDER_NAME "BGR"
#else
#define DISP_RGB_ORDER LCD_RGB_ELEMENT_ORDER_RGB
#define DISP_RGB_ORDER_NAME "RGB"
#endif

#define FONT_W 5
#define FONT_H 7
#define FONT_SPACING 1
#define GLYPH_MAX_PIXELS ((FONT_W + FONT_SPACING) * 2 * FONT_H * 2)

#define RGB565(r, g, b)                                                                                                 \
  ((uint16_t)((((uint16_t)(r) & 0xF8U) << 8) | (((uint16_t)(g) & 0xFCU) << 3) | (((uint16_t)(b) & 0xF8U) >> 3)))

typedef enum {
  DISPLAY_EVT_RENDER = 1,
} display_evt_t;

static esp_lcd_panel_io_handle_t s_panel_io;
static esp_lcd_panel_handle_t s_panel;
static QueueHandle_t s_render_queue;
static portMUX_TYPE s_state_lock = portMUX_INITIALIZER_UNLOCKED;
static uint16_t s_line_buf[DISP_W];
static char s_status[32];

typedef struct {
  char you[BBCLAW_DISPLAY_CHAT_LINE_LEN];
  char reply[BBCLAW_DISPLAY_CHAT_LINE_LEN];
} bb_chat_turn_t;

static bb_chat_turn_t s_history[BBCLAW_DISPLAY_CHAT_HISTORY];
static int s_history_count;
static int s_stream_turn_active;
/** 0 = 显示最新一轮；增大 = 看更早的轮次 */
static int s_view_back;
static int s_scroll_you;
static int s_scroll_ai;
/** 非 0 表示滚动键作用在 AI 栏，否则 ME */
static int s_focus_ai;

static int s_ready;

static const uint16_t kColorBg = RGB565(0, 0, 0);
static const uint16_t kColorPanel = RGB565(8, 10, 10);
static const uint16_t kColorPanelBorder = RGB565(34, 40, 40);
static const uint16_t kColorText = RGB565(232, 238, 236);
static const uint16_t kColorMuted = RGB565(132, 148, 144);
static const uint16_t kColorMessageBg = RGB565(12, 14, 14);
static const uint16_t kColorYouBg = RGB565(12, 16, 14);
static const uint16_t kColorAiBg = RGB565(10, 12, 18);
static const uint16_t kColorMessageText = RGB565(196, 218, 211);
static const uint16_t kColorAccent = RGB565(0, 200, 136);
static const uint16_t kColorAccentDim = RGB565(0, 76, 56);
static const uint16_t kColorErr = RGB565(176, 42, 56);

static void backlight_on(void) {
  gpio_config_t io_conf = {
      .pin_bit_mask = 1ULL << BBCLAW_ST7789_BL_GPIO,
      .mode = GPIO_MODE_OUTPUT,
      .pull_up_en = GPIO_PULLUP_DISABLE,
      .pull_down_en = GPIO_PULLDOWN_DISABLE,
      .intr_type = GPIO_INTR_DISABLE,
  };
  (void)gpio_config(&io_conf);
  (void)gpio_set_level(BBCLAW_ST7789_BL_GPIO, 1);
}

static void copy_rows(uint8_t out[FONT_H], const uint8_t in[FONT_H]) {
  memcpy(out, in, FONT_H);
}

static void glyph_rows(char c, uint8_t rows[FONT_H]) {
  static const uint8_t blank[FONT_H] = {0, 0, 0, 0, 0, 0, 0};
  static const uint8_t qmark[FONT_H] = {0x0E, 0x11, 0x01, 0x02, 0x04, 0x00, 0x04};
  c = (char)toupper((unsigned char)c);
  switch (c) {
    case 'A':
      copy_rows(rows, (const uint8_t[FONT_H]){0x0E, 0x11, 0x11, 0x1F, 0x11, 0x11, 0x11});
      return;
    case 'B':
      copy_rows(rows, (const uint8_t[FONT_H]){0x1E, 0x11, 0x11, 0x1E, 0x11, 0x11, 0x1E});
      return;
    case 'C':
      copy_rows(rows, (const uint8_t[FONT_H]){0x0E, 0x11, 0x10, 0x10, 0x10, 0x11, 0x0E});
      return;
    case 'D':
      copy_rows(rows, (const uint8_t[FONT_H]){0x1E, 0x11, 0x11, 0x11, 0x11, 0x11, 0x1E});
      return;
    case 'E':
      copy_rows(rows, (const uint8_t[FONT_H]){0x1F, 0x10, 0x10, 0x1E, 0x10, 0x10, 0x1F});
      return;
    case 'F':
      copy_rows(rows, (const uint8_t[FONT_H]){0x1F, 0x10, 0x10, 0x1E, 0x10, 0x10, 0x10});
      return;
    case 'G':
      copy_rows(rows, (const uint8_t[FONT_H]){0x0E, 0x11, 0x10, 0x17, 0x11, 0x11, 0x0F});
      return;
    case 'H':
      copy_rows(rows, (const uint8_t[FONT_H]){0x11, 0x11, 0x11, 0x1F, 0x11, 0x11, 0x11});
      return;
    case 'I':
      copy_rows(rows, (const uint8_t[FONT_H]){0x1F, 0x04, 0x04, 0x04, 0x04, 0x04, 0x1F});
      return;
    case 'J':
      copy_rows(rows, (const uint8_t[FONT_H]){0x01, 0x01, 0x01, 0x01, 0x11, 0x11, 0x0E});
      return;
    case 'K':
      copy_rows(rows, (const uint8_t[FONT_H]){0x11, 0x12, 0x14, 0x18, 0x14, 0x12, 0x11});
      return;
    case 'L':
      copy_rows(rows, (const uint8_t[FONT_H]){0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x1F});
      return;
    case 'M':
      copy_rows(rows, (const uint8_t[FONT_H]){0x11, 0x1B, 0x15, 0x15, 0x11, 0x11, 0x11});
      return;
    case 'N':
      copy_rows(rows, (const uint8_t[FONT_H]){0x11, 0x19, 0x15, 0x13, 0x11, 0x11, 0x11});
      return;
    case 'O':
      copy_rows(rows, (const uint8_t[FONT_H]){0x0E, 0x11, 0x11, 0x11, 0x11, 0x11, 0x0E});
      return;
    case 'P':
      copy_rows(rows, (const uint8_t[FONT_H]){0x1E, 0x11, 0x11, 0x1E, 0x10, 0x10, 0x10});
      return;
    case 'Q':
      copy_rows(rows, (const uint8_t[FONT_H]){0x0E, 0x11, 0x11, 0x11, 0x15, 0x12, 0x0D});
      return;
    case 'R':
      copy_rows(rows, (const uint8_t[FONT_H]){0x1E, 0x11, 0x11, 0x1E, 0x14, 0x12, 0x11});
      return;
    case 'S':
      copy_rows(rows, (const uint8_t[FONT_H]){0x0F, 0x10, 0x10, 0x0E, 0x01, 0x01, 0x1E});
      return;
    case 'T':
      copy_rows(rows, (const uint8_t[FONT_H]){0x1F, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04});
      return;
    case 'U':
      copy_rows(rows, (const uint8_t[FONT_H]){0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x0E});
      return;
    case 'V':
      copy_rows(rows, (const uint8_t[FONT_H]){0x11, 0x11, 0x11, 0x11, 0x11, 0x0A, 0x04});
      return;
    case 'W':
      copy_rows(rows, (const uint8_t[FONT_H]){0x11, 0x11, 0x11, 0x15, 0x15, 0x15, 0x0A});
      return;
    case 'X':
      copy_rows(rows, (const uint8_t[FONT_H]){0x11, 0x11, 0x0A, 0x04, 0x0A, 0x11, 0x11});
      return;
    case 'Y':
      copy_rows(rows, (const uint8_t[FONT_H]){0x11, 0x11, 0x0A, 0x04, 0x04, 0x04, 0x04});
      return;
    case 'Z':
      copy_rows(rows, (const uint8_t[FONT_H]){0x1F, 0x01, 0x02, 0x04, 0x08, 0x10, 0x1F});
      return;
    case '0':
      copy_rows(rows, (const uint8_t[FONT_H]){0x0E, 0x11, 0x13, 0x15, 0x19, 0x11, 0x0E});
      return;
    case '1':
      copy_rows(rows, (const uint8_t[FONT_H]){0x04, 0x0C, 0x04, 0x04, 0x04, 0x04, 0x0E});
      return;
    case '2':
      copy_rows(rows, (const uint8_t[FONT_H]){0x0E, 0x11, 0x01, 0x02, 0x04, 0x08, 0x1F});
      return;
    case '3':
      copy_rows(rows, (const uint8_t[FONT_H]){0x1E, 0x01, 0x01, 0x0E, 0x01, 0x01, 0x1E});
      return;
    case '4':
      copy_rows(rows, (const uint8_t[FONT_H]){0x02, 0x06, 0x0A, 0x12, 0x1F, 0x02, 0x02});
      return;
    case '5':
      copy_rows(rows, (const uint8_t[FONT_H]){0x1F, 0x10, 0x10, 0x1E, 0x01, 0x01, 0x1E});
      return;
    case '6':
      copy_rows(rows, (const uint8_t[FONT_H]){0x0E, 0x10, 0x10, 0x1E, 0x11, 0x11, 0x0E});
      return;
    case '7':
      copy_rows(rows, (const uint8_t[FONT_H]){0x1F, 0x01, 0x02, 0x04, 0x08, 0x08, 0x08});
      return;
    case '8':
      copy_rows(rows, (const uint8_t[FONT_H]){0x0E, 0x11, 0x11, 0x0E, 0x11, 0x11, 0x0E});
      return;
    case '9':
      copy_rows(rows, (const uint8_t[FONT_H]){0x0E, 0x11, 0x11, 0x0F, 0x01, 0x01, 0x0E});
      return;
    case ':':
      copy_rows(rows, (const uint8_t[FONT_H]){0x00, 0x04, 0x00, 0x00, 0x04, 0x00, 0x00});
      return;
    case '.':
      copy_rows(rows, (const uint8_t[FONT_H]){0x00, 0x00, 0x00, 0x00, 0x00, 0x06, 0x06});
      return;
    case ',':
      copy_rows(rows, (const uint8_t[FONT_H]){0x00, 0x00, 0x00, 0x00, 0x04, 0x04, 0x08});
      return;
    case '-':
      copy_rows(rows, (const uint8_t[FONT_H]){0x00, 0x00, 0x00, 0x1F, 0x00, 0x00, 0x00});
      return;
    case '_':
      copy_rows(rows, (const uint8_t[FONT_H]){0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x1F});
      return;
    case '/':
      copy_rows(rows, (const uint8_t[FONT_H]){0x01, 0x02, 0x04, 0x08, 0x10, 0x00, 0x00});
      return;
    case '(':
      copy_rows(rows, (const uint8_t[FONT_H]){0x02, 0x04, 0x08, 0x08, 0x08, 0x04, 0x02});
      return;
    case ')':
      copy_rows(rows, (const uint8_t[FONT_H]){0x08, 0x04, 0x02, 0x02, 0x02, 0x04, 0x08});
      return;
    case '!':
      copy_rows(rows, (const uint8_t[FONT_H]){0x04, 0x04, 0x04, 0x04, 0x04, 0x00, 0x04});
      return;
    case '?':
      copy_rows(rows, qmark);
      return;
    case '+':
      copy_rows(rows, (const uint8_t[FONT_H]){0x00, 0x04, 0x04, 0x1F, 0x04, 0x04, 0x00});
      return;
    case '=':
      copy_rows(rows, (const uint8_t[FONT_H]){0x00, 0x1F, 0x00, 0x1F, 0x00, 0x00, 0x00});
      return;
    case ' ':
      copy_rows(rows, blank);
      return;
    default:
      if ((unsigned char)c < 0x20 || (unsigned char)c > 0x7E) {
        copy_rows(rows, qmark);
      } else {
        copy_rows(rows, blank);
      }
      return;
  }
}

static void draw_fill_rect(int x, int y, int w, int h, uint16_t color) {
  if (!s_ready || s_panel == NULL || w <= 0 || h <= 0) {
    return;
  }
  if (x < 0) {
    w += x;
    x = 0;
  }
  if (y < 0) {
    h += y;
    y = 0;
  }
  if (x >= DISP_W || y >= DISP_H) {
    return;
  }
  if (x + w > DISP_W) {
    w = DISP_W - x;
  }
  if (y + h > DISP_H) {
    h = DISP_H - y;
  }
  if (w <= 0 || h <= 0) {
    return;
  }

  for (int i = 0; i < w; ++i) {
    s_line_buf[i] = color;
  }
  for (int row = 0; row < h; ++row) {
    (void)esp_lcd_panel_draw_bitmap(s_panel, x, y + row, x + w, y + row + 1, s_line_buf);
  }
}

static void draw_border(int x, int y, int w, int h, uint16_t color) {
  if (w <= 1 || h <= 1) {
    return;
  }
  draw_fill_rect(x, y, w, 1, color);
  draw_fill_rect(x, y + h - 1, w, 1, color);
  draw_fill_rect(x, y, 1, h, color);
  draw_fill_rect(x + w - 1, y, 1, h, color);
}

static void draw_char(int x, int y, char c, uint16_t fg, uint16_t bg, int scale) {
  if (!s_ready || scale <= 0) {
    return;
  }
  const int glyph_w = (FONT_W + FONT_SPACING) * scale;
  const int glyph_h = FONT_H * scale;
  if (x < 0 || y < 0 || x + glyph_w > DISP_W || y + glyph_h > DISP_H) {
    return;
  }
  uint8_t rows[FONT_H];
  uint16_t pixels[GLYPH_MAX_PIXELS];
  glyph_rows(c, rows);

  int idx = 0;
  for (int row = 0; row < FONT_H; ++row) {
    for (int sy = 0; sy < scale; ++sy) {
      for (int col = 0; col < FONT_W; ++col) {
        uint16_t px = ((rows[row] >> (FONT_W - 1 - col)) & 0x1) ? fg : bg;
        for (int sx = 0; sx < scale; ++sx) {
          pixels[idx++] = px;
        }
      }
      for (int sx = 0; sx < scale; ++sx) {
        pixels[idx++] = bg;
      }
    }
  }
  (void)esp_lcd_panel_draw_bitmap(s_panel, x, y, x + glyph_w, y + glyph_h, pixels);
}

static void draw_wrapped_text(int x, int y, int w, int h, const char* text, uint16_t fg, uint16_t bg, int scale,
                              int line_gap) {
  if (text == NULL || w <= 0 || h <= 0 || scale <= 0) {
    return;
  }
  const int char_w = (FONT_W + FONT_SPACING) * scale;
  const int char_h = FONT_H * scale;
  const int line_h = char_h + line_gap;
  int cx = x;
  int cy = y;

  for (const unsigned char* p = (const unsigned char*)text; *p != '\0'; ++p) {
    if (*p == '\n') {
      cx = x;
      cy += line_h;
      if (cy + char_h > y + h) {
        break;
      }
      continue;
    }
    if (cx + char_w > x + w) {
      cx = x;
      cy += line_h;
      if (cy + char_h > y + h) {
        break;
      }
    }
    char out = (char)(*p <= 0x7F ? *p : '?');
    draw_char(cx, cy, out, fg, bg, scale);
    cx += char_w;
  }
}

static const char* utf8_inc(const char* p) {
  unsigned char c = (unsigned char)*p;
  if (c == 0) {
    return p;
  }
  if (c < 0x80U) {
    return p + 1;
  }
  if ((c & 0xE0U) == 0xC0U) {
    return p + 2;
  }
  if ((c & 0xF0U) == 0xE0U) {
    return p + 3;
  }
  if ((c & 0xF8U) == 0xF0U) {
    return p + 4;
  }
  return p + 1;
}

static int utf8_is_ascii_byte(const char* p) {
  return (unsigned char)*p < 0x80U;
}

static int count_wrapped_lines(int wrap_w, const char* text, int scale, int line_gap) {
  if (text == NULL || text[0] == '\0' || wrap_w <= 0 || scale <= 0) {
    return 0;
  }
  const int char_w = (FONT_W + FONT_SPACING) * scale;
  int lines = 1;
  int cx = 0;
  for (const char* p = text; *p != '\0';) {
    if (*p == '\n') {
      cx = 0;
      lines++;
      p++;
      continue;
    }
    if (cx + char_w > wrap_w) {
      cx = 0;
      lines++;
    }
    if (utf8_is_ascii_byte(p)) {
      p++;
    } else {
      p = utf8_inc(p);
    }
    cx += char_w;
  }
  return lines;
}

static void draw_wrapped_text_skip(int x, int y, int w, int h, const char* text, uint16_t fg, uint16_t bg, int scale,
                                   int line_gap, int skip_lines) {
  if (text == NULL || w <= 0 || h <= 0 || scale <= 0) {
    return;
  }
  const int char_w = (FONT_W + FONT_SPACING) * scale;
  const int char_h = FONT_H * scale;
  const int line_h = char_h + line_gap;
  int cx = x;
  int cy = y;
  int line_idx = 0;

  for (const char* p = text; *p != '\0';) {
    if (*p == '\n') {
      cx = x;
      cy += line_h;
      line_idx++;
      p++;
      if (cy + char_h > y + h) {
        break;
      }
      continue;
    }
    if (cx + char_w > x + w) {
      cx = x;
      cy += line_h;
      line_idx++;
      if (cy + char_h > y + h) {
        break;
      }
    }
    char out = '?';
    if (utf8_is_ascii_byte(p)) {
      unsigned char c = (unsigned char)*p;
      out = (c <= 0x7FU) ? (char)c : '?';
      p++;
    } else {
      out = '?';
      p = utf8_inc(p);
    }
    if (line_idx >= skip_lines) {
      draw_char(cx, cy, out, fg, bg, scale);
    }
    cx += char_w;
  }
}

static void chat_layout_metrics(int* out_msg_w, int* out_half, int* out_bot_fill_h) {
  const int margin = 6;
  const int panel_h = DISP_H - margin * 2;
  const int right_x = margin + 102 + 6;
  const int right_w = DISP_W - right_x - margin;
  const int msg_h = panel_h - 42;
  const int inner_h = msg_h - 8;
  const int half = inner_h / 2;
  const int bot_fill_h = inner_h - half - 1;
  *out_msg_w = right_w - 12;
  *out_half = half;
  *out_bot_fill_h = bot_fill_h;
}

static int text_area_visible_lines(int px_h, int scale, int line_gap) {
  const int line_h = FONT_H * scale + line_gap;
  if (line_h <= 0) {
    return 1;
  }
  if (px_h <= 0) {
    return 1;
  }
  return px_h / line_h;
}

static void draw_mono_bitmap(int x, int y, const bb_ui_mono_bitmap_t* bitmap, uint16_t fg, uint16_t bg) {
  if (!s_ready || s_panel == NULL || bitmap == NULL) {
    return;
  }
  if (bitmap->width == 0 || bitmap->height == 0 || bitmap->bits == NULL) {
    return;
  }
  const int bw = (int)bitmap->width;
  const int bh = (int)bitmap->height;
  if (bw > DISP_W || bh > DISP_H || x < 0 || y < 0 || x + bw > DISP_W || y + bh > DISP_H) {
    return;
  }
  for (uint8_t row = 0; row < bitmap->height; ++row) {
    const uint8_t* row_bits = bitmap->bits + (size_t)row * bitmap->stride;
    for (uint8_t col = 0; col < bitmap->width; ++col) {
      const uint8_t byte = row_bits[col >> 3];
      const uint8_t bit = (uint8_t)((byte >> (7 - (col & 0x7))) & 0x1);
      s_line_buf[col] = bit ? fg : bg;
    }
    (void)esp_lcd_panel_draw_bitmap(s_panel, x, y + row, x + bitmap->width, y + row + 1, s_line_buf);
  }
}

static bool starts_with_ci(const char* text, const char* prefix) {
  if (text == NULL || prefix == NULL) {
    return false;
  }
  while (*prefix != '\0') {
    if (*text == '\0') {
      return false;
    }
    if (toupper((unsigned char)(*text)) != toupper((unsigned char)(*prefix))) {
      return false;
    }
    ++text;
    ++prefix;
  }
  return true;
}

static bool contains_ci(const char* text, const char* needle) {
  if (text == NULL || needle == NULL || *needle == '\0') {
    return false;
  }
  const size_t needle_len = strlen(needle);
  for (const char* p = text; *p != '\0'; ++p) {
    size_t i = 0;
    for (; i < needle_len; ++i) {
      if (p[i] == '\0') {
        break;
      }
      if (toupper((unsigned char)p[i]) != toupper((unsigned char)needle[i])) {
        break;
      }
    }
    if (i == needle_len) {
      return true;
    }
  }
  return false;
}

static const char* chat_line_visible(const char* s) {
  return (s != NULL && s[0] != '\0') ? s : "--";
}

static bool is_recording_status(const char* status) {
  return starts_with_ci(status, "TX");
}

static const bb_ui_mono_bitmap_t* pick_status_icon(const char* status, uint32_t frame) {
  if (is_recording_status(status)) {
    switch (frame % 3U) {
      case 0:
        return &BB_UI_ICON_REC_12_1;
      case 1:
        return &BB_UI_ICON_REC_12_2;
      default:
        return &BB_UI_ICON_REC_12_3;
    }
  }
  if (contains_ci(status, "ERR")) {
    return &BB_UI_ICON_ERR_12;
  }
  if (starts_with_ci(status, "READY")) {
    return &BB_UI_ICON_READY_12;
  }
  if (starts_with_ci(status, "TX")) {
    return &BB_UI_ICON_TX_12;
  }
  if (starts_with_ci(status, "RX")) {
    return &BB_UI_ICON_RX_12;
  }
  if (starts_with_ci(status, "SPEAK")) {
    return &BB_UI_ICON_SPEAK_12;
  }
  if (starts_with_ci(status, "TASK") || starts_with_ci(status, "WIFI") || starts_with_ci(status, "ADAPTER")) {
    return &BB_UI_ICON_TASK_12;
  }
  return NULL;
}

static uint16_t pick_status_accent(const char* status) {
  if (contains_ci(status, "ERR")) {
    return kColorErr;
  }
  if (is_recording_status(status)) {
    return kColorAccent;
  }
  if (starts_with_ci(status, "SPEAK")) {
    return RGB565(0, 160, 188);
  }
  if (starts_with_ci(status, "READY")) {
    return RGB565(0, 168, 112);
  }
  return RGB565(96, 120, 116);
}

static void draw_rec_meter(int x, int y, uint32_t frame, uint16_t active, uint16_t inactive) {
  static const uint8_t kPatterns[4][5] = {
      {2, 4, 6, 4, 2},
      {3, 6, 9, 6, 3},
      {4, 8, 11, 8, 4},
      {3, 7, 10, 7, 3},
  };
  const uint8_t* p = kPatterns[frame % 4U];
  for (int i = 0; i < 5; ++i) {
    const int h = (int)p[i];
    const int bx = x + i * 5;
    draw_fill_rect(bx, y + 12 - h, 3, h, active);
  }
  for (int i = 0; i < 5; ++i) {
    const int bx = x + i * 5;
    draw_fill_rect(bx, y + 12, 3, 1, inactive);
  }
}

typedef struct {
  char status[32];
  char you[BBCLAW_DISPLAY_CHAT_LINE_LEN];
  char reply[BBCLAW_DISPLAY_CHAT_LINE_LEN];
  int skip_you;
  int skip_ai;
  int focus_ai;
  int turn_num;
  int turn_den;
  int more_you;
  int more_ai;
} display_snap_t;

static void snapshot_fill(display_snap_t* o) {
  portENTER_CRITICAL(&s_state_lock);
  memcpy(o->status, s_status, sizeof(o->status));
  o->skip_you = s_scroll_you;
  o->skip_ai = s_scroll_ai;
  o->focus_ai = s_focus_ai;
  o->turn_den = s_history_count;
  if (s_history_count <= 0) {
    o->you[0] = '\0';
    o->reply[0] = '\0';
    o->turn_num = 0;
  } else {
    int idx = s_history_count - 1 - s_view_back;
    if (idx < 0) {
      idx = 0;
    }
    memcpy(o->you, s_history[idx].you, sizeof(o->you));
    memcpy(o->reply, s_history[idx].reply, sizeof(o->reply));
    o->turn_num = s_history_count - s_view_back;
  }

  int msg_w, half, bot_fill_h;
  chat_layout_metrics(&msg_w, &half, &bot_fill_h);
  int me_h = half - 16;
  int ai_h = bot_fill_h - 18;
  int vis_me = text_area_visible_lines(me_h, 1, 1);
  int vis_ai = text_area_visible_lines(ai_h, 1, 1);
  const char* u = (o->you[0] != '\0') ? o->you : "--";
  const char* r = (o->reply[0] != '\0') ? o->reply : "--";
  int ly = count_wrapped_lines(msg_w, u, 1, 1);
  int la = count_wrapped_lines(msg_w, r, 1, 1);
  int max_me = ly > vis_me ? ly - vis_me : 0;
  int max_ai = la > vis_ai ? la - vis_ai : 0;
  o->more_you = (s_scroll_you < max_me) ? 1 : 0;
  o->more_ai = (s_scroll_ai < max_ai) ? 1 : 0;
  portEXIT_CRITICAL(&s_state_lock);
}

static void render_screen(const display_snap_t* snap, uint32_t frame) {
  const char* status = snap->status;
  const bb_ui_mono_bitmap_t* status_icon = pick_status_icon(status, frame);
  const uint16_t status_accent = pick_status_accent(status);
  const int margin = 6;
  const int left_w = 102;
  const int gap = 6;
  const int panel_y = margin;
  const int panel_h = DISP_H - margin * 2;
  const int right_x = margin + left_w + gap;
  const int right_w = DISP_W - right_x - margin;

  draw_fill_rect(0, 0, DISP_W, DISP_H, kColorBg);
  draw_fill_rect(0, 0, DISP_W, 1, kColorAccentDim);
  draw_fill_rect(0, DISP_H - 1, DISP_W, 1, kColorAccentDim);

  // Left panel: identity + live status.
  draw_fill_rect(margin, panel_y, left_w, panel_h, kColorPanel);
  draw_border(margin, panel_y, left_w, panel_h, kColorPanelBorder);
  draw_fill_rect(margin + 1, panel_y + 1, left_w - 2, 2, kColorAccentDim);
  draw_mono_bitmap(margin + 8, panel_y + 8, &BB_UI_LOGO_CLAW_16, kColorText, kColorPanel);
  draw_wrapped_text(margin + 30, panel_y + 10, left_w - 36, 18, "BBCLAW", kColorText, kColorPanel, 1, 0);
  draw_wrapped_text(margin + 30, panel_y + 20, left_w - 36, 14, "NODE", kColorMuted, kColorPanel, 1, 0);

  draw_fill_rect(margin + 7, panel_y + 38, left_w - 14, 46, RGB565(12, 18, 16));
  draw_border(margin + 7, panel_y + 38, left_w - 14, 46, status_accent);
  if (status_icon != NULL) {
    draw_mono_bitmap(margin + 12, panel_y + 47, status_icon, status_accent, RGB565(12, 18, 16));
  }
  draw_wrapped_text(margin + 30, panel_y + 46, left_w - 36, 28, status, kColorText, RGB565(12, 18, 16), 1, 1);

  if (is_recording_status(status)) {
    draw_wrapped_text(margin + 10, panel_y + 90, left_w - 20, 10, "REC LIVE", kColorAccent, kColorPanel, 1, 0);
    draw_rec_meter(margin + 18, panel_y + 104, frame, kColorAccent, kColorAccentDim);
  } else {
    draw_wrapped_text(margin + 10, panel_y + 90, left_w - 20, 10, "PTT READY", kColorMuted, kColorPanel, 1, 0);
    draw_fill_rect(margin + 18, panel_y + 112, left_w - 36, 1, kColorAccentDim);
  }
  draw_wrapped_text(margin + 10, panel_y + panel_h - 14, left_w - 20, 10, "HOLD TO TALK", kColorMuted, kColorPanel, 1,
                    0);

  // Right panel: one question / one answer (ME = 自己说的, AI = 回复).
  draw_fill_rect(right_x, panel_y, right_w, panel_h, kColorPanel);
  draw_border(right_x, panel_y, right_w, panel_h, kColorPanelBorder);
  draw_fill_rect(right_x + 1, panel_y + 1, right_w - 2, 2, status_accent);
  draw_wrapped_text(right_x + 8, panel_y + 8, right_w - 16, 10, "CHAT", kColorMuted, kColorPanel, 1, 0);

  const int msg_x = right_x + 6;
  const int msg_y = panel_y + 22;
  const int msg_w = right_w - 12;
  const int msg_h = panel_h - 42;
  const int inner_y = msg_y + 4;
  const int inner_h = msg_h - 8;
  const int half = inner_h / 2;
  const int bot_fill_h = inner_h - half - 1;

  const char* you_vis = chat_line_visible(snap->you);
  const char* ai_vis = chat_line_visible(snap->reply);

  draw_fill_rect(msg_x, msg_y, msg_w, msg_h, kColorMessageBg);
  draw_border(msg_x, msg_y, msg_w, msg_h, kColorPanelBorder);

  draw_fill_rect(msg_x + 1, inner_y, msg_w - 2, half, kColorYouBg);
  draw_wrapped_text(msg_x + 6, inner_y + 3, msg_w - 12, 10, "ME", kColorAccent, kColorYouBg, 1, 0);
  draw_wrapped_text_skip(msg_x + 4, inner_y + 12, msg_w - 8, half - 16, you_vis, kColorMessageText, kColorYouBg, 1, 1,
                         snap->skip_you);

  draw_fill_rect(msg_x + 1, inner_y + half, msg_w - 2, 1, kColorPanelBorder);

  draw_fill_rect(msg_x + 1, inner_y + half + 1, msg_w - 2, bot_fill_h, kColorAiBg);
  draw_wrapped_text(msg_x + 6, inner_y + half + 4, msg_w - 12, 10, "AI", RGB565(120, 180, 200), kColorAiBg, 1, 0);
  draw_wrapped_text_skip(msg_x + 4, inner_y + half + 14, msg_w - 8, bot_fill_h - 18, ai_vis, kColorMessageText,
                         kColorAiBg, 1, 1, snap->skip_ai);

  char foot[28];
  if (snap->turn_den > 0) {
    int show_v = (snap->focus_ai && snap->more_ai) || (!snap->focus_ai && snap->more_you);
    if (show_v) {
      snprintf(foot, sizeof(foot), "%d/%d v", snap->turn_num, snap->turn_den);
    } else {
      snprintf(foot, sizeof(foot), "%d/%d", snap->turn_num, snap->turn_den);
    }
  } else {
    snprintf(foot, sizeof(foot), "--");
  }
  draw_wrapped_text(right_x + 8, panel_y + panel_h - 14, right_w - 16, 10, foot, kColorMuted, kColorPanel, 1, 0);
}

static void queue_render(void) {
  if (s_render_queue == NULL) {
    return;
  }
  display_evt_t evt = DISPLAY_EVT_RENDER;
  (void)xQueueSend(s_render_queue, &evt, 0);
}

static void display_task(void* arg) {
  (void)arg;
  display_evt_t evt;
  uint32_t frame = 0;
  char last_status[sizeof(s_status)] = "";
  for (;;) {
    display_snap_t snap;
    snapshot_fill(&snap);

    const bool animate = is_recording_status(snap.status);
    const TickType_t wait_ticks = animate ? pdMS_TO_TICKS(220) : portMAX_DELAY;
    if (xQueueReceive(s_render_queue, &evt, wait_ticks) == pdTRUE) {
      while (xQueueReceive(s_render_queue, &evt, 0) == pdTRUE) {
        // Drain queue; only latest state needs render.
      }
    } else if (!animate) {
      continue;
    }

    snapshot_fill(&snap);
    if (strncmp(last_status, snap.status, sizeof(last_status)) != 0) {
      strncpy(last_status, snap.status, sizeof(last_status) - 1);
      last_status[sizeof(last_status) - 1] = '\0';
      frame = 0;
    }
    render_screen(&snap, frame);
    if (is_recording_status(snap.status)) {
      ++frame;
    }
  }
}

static esp_err_t init_panel(void) {
  const spi_host_device_t host = (spi_host_device_t)BBCLAW_ST7789_HOST;
  spi_bus_config_t buscfg = {
      .sclk_io_num = BBCLAW_ST7789_SCLK_GPIO,
      .mosi_io_num = BBCLAW_ST7789_MOSI_GPIO,
      .miso_io_num = -1,
      .quadwp_io_num = -1,
      .quadhd_io_num = -1,
      .max_transfer_sz = DISP_W * 40 * (int)sizeof(uint16_t),
  };
  esp_err_t err = spi_bus_initialize(host, &buscfg, SPI_DMA_CH_AUTO);
  if (err != ESP_OK && err != ESP_ERR_INVALID_STATE) {
    return err;
  }

  esp_lcd_panel_io_spi_config_t io_config = {
      .dc_gpio_num = BBCLAW_ST7789_DC_GPIO,
      .cs_gpio_num = BBCLAW_ST7789_CS_GPIO,
      .pclk_hz = DISP_PCLK_HZ,
      .spi_mode = 0,
      .trans_queue_depth = 10,
      .lcd_cmd_bits = 8,
      .lcd_param_bits = 8,
  };
  ESP_RETURN_ON_ERROR(esp_lcd_new_panel_io_spi((esp_lcd_spi_bus_handle_t)host, &io_config, &s_panel_io), TAG,
                      "new panel io failed");

  esp_lcd_panel_dev_config_t panel_config = {
      .reset_gpio_num = BBCLAW_ST7789_RST_GPIO,
      .rgb_ele_order = DISP_RGB_ORDER,
      .bits_per_pixel = 16,
  };
  ESP_RETURN_ON_ERROR(esp_lcd_new_panel_st7789(s_panel_io, &panel_config, &s_panel), TAG, "new st7789 panel failed");
  ESP_RETURN_ON_ERROR(esp_lcd_panel_reset(s_panel), TAG, "panel reset failed");
  ESP_RETURN_ON_ERROR(esp_lcd_panel_init(s_panel), TAG, "panel init failed");
  ESP_RETURN_ON_ERROR(esp_lcd_panel_swap_xy(s_panel, DISP_SWAP_XY), TAG, "panel swapxy failed");
  ESP_RETURN_ON_ERROR(esp_lcd_panel_mirror(s_panel, DISP_MIRROR_X, DISP_MIRROR_Y), TAG, "panel mirror failed");
  ESP_RETURN_ON_ERROR(esp_lcd_panel_set_gap(s_panel, DISP_X_GAP, DISP_Y_GAP), TAG, "panel set gap failed");
  ESP_RETURN_ON_ERROR(esp_lcd_panel_invert_color(s_panel, DISP_INVERT_COLOR), TAG, "panel invert failed");
  ESP_RETURN_ON_ERROR(esp_lcd_panel_disp_on_off(s_panel, true), TAG, "panel on failed");
  return ESP_OK;
}

esp_err_t bb_display_init(void) {
  backlight_on();
  strncpy(s_status, "BOOT", sizeof(s_status) - 1);
  s_status[sizeof(s_status) - 1] = '\0';
  s_history_count = 0;
  s_stream_turn_active = 0;
  s_view_back = 0;
  s_scroll_you = 0;
  s_scroll_ai = 0;
  s_focus_ai = 1;

  ESP_RETURN_ON_ERROR(init_panel(), TAG, "display panel init failed");
  s_render_queue = xQueueCreate(8, sizeof(display_evt_t));
  if (s_render_queue == NULL) {
    return ESP_ERR_NO_MEM;
  }
  if (xTaskCreate(display_task, "bb_display_task", 4096, NULL, 4, NULL) != pdPASS) {
    vQueueDelete(s_render_queue);
    s_render_queue = NULL;
    return ESP_ERR_NO_MEM;
  }
  s_ready = 1;
  ESP_LOGI(TAG,
           "display ready st7789 host=%d pclk=%d wh=%dx%d rgb=%s swap_xy=%d mirror=(%d,%d) gap=(%d,%d) invert=%d",
           BBCLAW_ST7789_HOST, DISP_PCLK_HZ, DISP_W, DISP_H, DISP_RGB_ORDER_NAME, DISP_SWAP_XY, DISP_MIRROR_X,
           DISP_MIRROR_Y, DISP_X_GAP, DISP_Y_GAP, DISP_INVERT_COLOR);
  queue_render();
  return ESP_OK;
}

esp_err_t bb_display_show_status(const char* status_line) {
  if (status_line != NULL) {
    portENTER_CRITICAL(&s_state_lock);
    strncpy(s_status, status_line, sizeof(s_status) - 1);
    s_status[sizeof(s_status) - 1] = '\0';
    portEXIT_CRITICAL(&s_state_lock);
  }
  queue_render();
  return ESP_OK;
}

esp_err_t bb_display_show_chat_turn(const char* user_said, const char* assistant_reply) {
  return bb_display_upsert_chat_turn(user_said, assistant_reply, 1);
}

esp_err_t bb_display_upsert_chat_turn(const char* user_said, const char* assistant_reply, int finalize) {
  const char* u = user_said != NULL ? user_said : "";
  const char* r = assistant_reply != NULL ? assistant_reply : "";
  if (u[0] == '\0' && r[0] == '\0') {
    return ESP_OK;
  }
  portENTER_CRITICAL(&s_state_lock);
  if (s_stream_turn_active && s_history_count > 0) {
    strncpy(s_history[s_history_count - 1].you, u, sizeof(s_history[0].you) - 1);
    s_history[s_history_count - 1].you[sizeof(s_history[0].you) - 1] = '\0';
    strncpy(s_history[s_history_count - 1].reply, r, sizeof(s_history[0].reply) - 1);
    s_history[s_history_count - 1].reply[sizeof(s_history[0].reply) - 1] = '\0';
  } else if (s_history_count < BBCLAW_DISPLAY_CHAT_HISTORY) {
    strncpy(s_history[s_history_count].you, u, sizeof(s_history[0].you) - 1);
    s_history[s_history_count].you[sizeof(s_history[0].you) - 1] = '\0';
    strncpy(s_history[s_history_count].reply, r, sizeof(s_history[0].reply) - 1);
    s_history[s_history_count].reply[sizeof(s_history[0].reply) - 1] = '\0';
    s_history_count++;
  } else {
    memmove(&s_history[0], &s_history[1], sizeof(bb_chat_turn_t) * (BBCLAW_DISPLAY_CHAT_HISTORY - 1));
    strncpy(s_history[BBCLAW_DISPLAY_CHAT_HISTORY - 1].you, u, sizeof(s_history[0].you) - 1);
    s_history[BBCLAW_DISPLAY_CHAT_HISTORY - 1].you[sizeof(s_history[0].you) - 1] = '\0';
    strncpy(s_history[BBCLAW_DISPLAY_CHAT_HISTORY - 1].reply, r, sizeof(s_history[0].reply) - 1);
    s_history[BBCLAW_DISPLAY_CHAT_HISTORY - 1].reply[sizeof(s_history[0].reply) - 1] = '\0';
  }
  s_view_back = 0;
  s_scroll_you = 0;
  s_scroll_ai = 0;
  s_stream_turn_active = finalize ? 0 : 1;
  portEXIT_CRITICAL(&s_state_lock);
  queue_render();
  return ESP_OK;
}

esp_err_t bb_display_chat_prev_turn(void) {
  portENTER_CRITICAL(&s_state_lock);
  if (s_history_count > 0 && s_view_back < s_history_count - 1) {
    s_view_back++;
    s_scroll_you = 0;
    s_scroll_ai = 0;
  }
  portEXIT_CRITICAL(&s_state_lock);
  queue_render();
  return ESP_OK;
}

esp_err_t bb_display_chat_next_turn(void) {
  portENTER_CRITICAL(&s_state_lock);
  if (s_view_back > 0) {
    s_view_back--;
    s_scroll_you = 0;
    s_scroll_ai = 0;
  }
  portEXIT_CRITICAL(&s_state_lock);
  queue_render();
  return ESP_OK;
}

esp_err_t bb_display_chat_scroll_down(void) {
  int msg_w, half, bot_fill_h;
  chat_layout_metrics(&msg_w, &half, &bot_fill_h);
  const int me_h = half - 16;
  const int ai_h = bot_fill_h - 18;
  const int vis_me = text_area_visible_lines(me_h, 1, 1);
  const int vis_ai = text_area_visible_lines(ai_h, 1, 1);
  portENTER_CRITICAL(&s_state_lock);
  if (s_history_count <= 0) {
    portEXIT_CRITICAL(&s_state_lock);
    return ESP_OK;
  }
  const int idx = s_history_count - 1 - s_view_back;
  const char* u = s_history[idx].you[0] != '\0' ? s_history[idx].you : "--";
  const char* r = s_history[idx].reply[0] != '\0' ? s_history[idx].reply : "--";
  const int ly = count_wrapped_lines(msg_w, u, 1, 1);
  const int la = count_wrapped_lines(msg_w, r, 1, 1);
  const int max_me = ly > vis_me ? ly - vis_me : 0;
  const int max_ai = la > vis_ai ? la - vis_ai : 0;
  if (s_focus_ai) {
    if (s_scroll_ai < max_ai) {
      s_scroll_ai++;
    }
  } else {
    if (s_scroll_you < max_me) {
      s_scroll_you++;
    }
  }
  portEXIT_CRITICAL(&s_state_lock);
  queue_render();
  return ESP_OK;
}

esp_err_t bb_display_chat_scroll_up(void) {
  portENTER_CRITICAL(&s_state_lock);
  if (s_focus_ai) {
    if (s_scroll_ai > 0) {
      s_scroll_ai--;
    }
  } else {
    if (s_scroll_you > 0) {
      s_scroll_you--;
    }
  }
  portEXIT_CRITICAL(&s_state_lock);
  queue_render();
  return ESP_OK;
}

void bb_display_chat_focus_me(void) {
  portENTER_CRITICAL(&s_state_lock);
  s_focus_ai = 0;
  portEXIT_CRITICAL(&s_state_lock);
  queue_render();
}

void bb_display_chat_focus_ai(void) {
  portENTER_CRITICAL(&s_state_lock);
  s_focus_ai = 1;
  portEXIT_CRITICAL(&s_state_lock);
  queue_render();
}
