#define SDL_MAIN_HANDLED

#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include "bb_config.h"
#include "bb_display.h"
#include "bb_time.h"
#include "lvgl.h"
#include "src/drivers/sdl/lv_sdl_keyboard.h"
#include "src/drivers/sdl/lv_sdl_mouse.h"
#include "src/drivers/sdl/lv_sdl_mousewheel.h"
#include "src/drivers/sdl/lv_sdl_window.h"

#define DISP_W 320
#define DISP_H 172
#define APP_TEXT_LEN 192

typedef enum {
  APP_MODE_AUTO = 0,
  APP_MODE_STANDBY = 1,
  APP_MODE_NOTIFICATION = 2,
  APP_MODE_SPEAKING = 3,
} app_mode_t;

typedef struct {
  app_mode_t mode;
  char status[32];
  char you[APP_TEXT_LEN];
  char reply[APP_TEXT_LEN];
  int turn_num;
  int turn_den;
  float zoom;
  uint32_t timeout_ms;
  uint32_t start_tick;
  char export_path[512];
} app_state_t;

static volatile sig_atomic_t s_exit_requested = 0;

static void on_signal(int sig) {
  (void)sig;
  s_exit_requested = 1;
}

static int parse_int(const char* s, int fallback) {
  char* end = NULL;
  long v = strtol(s, &end, 10);
  if (s == NULL || *s == '\0' || end == s) {
    return fallback;
  }
  return (int)v;
}

static float parse_float(const char* s, float fallback) {
  char* end = NULL;
  float v = strtof(s, &end);
  if (s == NULL || *s == '\0' || end == s) {
    return fallback;
  }
  return v;
}

static void init_default_state(app_state_t* state) {
  memset(state, 0, sizeof(*state));
  state->mode = APP_MODE_AUTO;
  state->zoom = 3.0f;
  state->timeout_ms = 0;
  state->turn_num = 1;
  state->turn_den = 1;
  strncpy(state->status, "READY", sizeof(state->status) - 1);
  strncpy(state->you, "好， 有事随时叫我。", sizeof(state->you) - 1);
  strncpy(state->reply, "Assistant reply: the latest note is ready for review.", sizeof(state->reply) - 1);
}

static void parse_args(app_state_t* state, int argc, char** argv) {
  for (int i = 1; i < argc; ++i) {
    if ((strcmp(argv[i], "--mode") == 0) && i + 1 < argc) {
      ++i;
      if (strcmp(argv[i], "standby") == 0) {
        state->mode = APP_MODE_STANDBY;
      } else if (strcmp(argv[i], "notification") == 0) {
        state->mode = APP_MODE_NOTIFICATION;
      } else if (strcmp(argv[i], "speaking") == 0) {
        state->mode = APP_MODE_SPEAKING;
      } else {
        state->mode = APP_MODE_AUTO;
      }
    } else if ((strcmp(argv[i], "--status") == 0) && i + 1 < argc) {
      ++i;
      strncpy(state->status, argv[i], sizeof(state->status) - 1);
      state->status[sizeof(state->status) - 1] = '\0';
    } else if ((strcmp(argv[i], "--you") == 0) && i + 1 < argc) {
      ++i;
      strncpy(state->you, argv[i], sizeof(state->you) - 1);
      state->you[sizeof(state->you) - 1] = '\0';
    } else if ((strcmp(argv[i], "--reply") == 0) && i + 1 < argc) {
      ++i;
      strncpy(state->reply, argv[i], sizeof(state->reply) - 1);
      state->reply[sizeof(state->reply) - 1] = '\0';
    } else if ((strcmp(argv[i], "--turn-num") == 0) && i + 1 < argc) {
      ++i;
      state->turn_num = parse_int(argv[i], state->turn_num);
    } else if ((strcmp(argv[i], "--turn-den") == 0) && i + 1 < argc) {
      ++i;
      state->turn_den = parse_int(argv[i], state->turn_den);
    } else if ((strcmp(argv[i], "--zoom") == 0) && i + 1 < argc) {
      ++i;
      state->zoom = parse_float(argv[i], state->zoom);
    } else if ((strcmp(argv[i], "--timeout-ms") == 0) && i + 1 < argc) {
      ++i;
      state->timeout_ms = (uint32_t)parse_int(argv[i], (int)state->timeout_ms);
    } else if ((strcmp(argv[i], "--export") == 0) && i + 1 < argc) {
      ++i;
      strncpy(state->export_path, argv[i], sizeof(state->export_path) - 1);
      state->export_path[sizeof(state->export_path) - 1] = '\0';
    } else if (strcmp(argv[i], "--help") == 0) {
      printf("bbclaw_lvgl_sim [--mode auto|standby|notification|speaking] [--status READY|TASK|TX|RX|SPEAK]\n");
      printf("               [--you TEXT] [--reply TEXT] [--turn-num N] [--turn-den N]\n");
      printf("               [--zoom 3.0] [--timeout-ms 0] [--export PATH]\n");
      exit(0);
    }
  }

  if (state->turn_num <= 0) {
    state->turn_num = state->turn_den > 0 ? state->turn_den : 1;
  }
  if (state->turn_den < state->turn_num) {
    state->turn_den = state->turn_num;
  }
}

static int write_ppm_from_rgb565_pixels(const uint16_t* pixels, uint32_t w, uint32_t h, const char* path) {
  FILE* fp = fopen(path, "wb");
  if (fp == NULL) {
    perror("fopen");
    return -1;
  }

  fprintf(fp, "P6\n%u %u\n255\n", w, h);
  for (uint32_t y = 0; y < h; ++y) {
    const uint16_t* row = pixels + (y * w);
    for (uint32_t x = 0; x < w; ++x) {
      uint16_t px = row[x];
      uint8_t rgb[3];
      rgb[0] = (uint8_t)((((px >> 11) & 0x1f) * 255U) / 31U);
      rgb[1] = (uint8_t)((((px >> 5) & 0x3f) * 255U) / 63U);
      rgb[2] = (uint8_t)(((px & 0x1f) * 255U) / 31U);
      fwrite(rgb, 1, sizeof(rgb), fp);
    }
  }

  fclose(fp);
  return 0;
}

static int convert_ppm_to_png_with_pillow(const char* ppm_path, const char* png_path) {
  char cmd[1600];
  snprintf(cmd, sizeof(cmd),
           "python3 -c \"from PIL import Image; import sys; Image.open(sys.argv[1]).save(sys.argv[2])\" \"%s\" \"%s\"",
           ppm_path, png_path);
  return system(cmd);
}

static int export_headless_buffer_image(const char* export_path, const uint16_t* pixels, uint32_t w, uint32_t h) {
  char ppm_path[640];
  int rc;
  size_t len;
  int is_png;

  if (export_path == NULL || export_path[0] == '\0' || pixels == NULL || w == 0 || h == 0) {
    return -1;
  }

  len = strlen(export_path);
  is_png = (len >= 4 && strcmp(export_path + len - 4, ".png") == 0);
  if (is_png) {
    snprintf(ppm_path, sizeof(ppm_path), "%s.ppm", export_path);
    rc = write_ppm_from_rgb565_pixels(pixels, w, h, ppm_path);
    if (rc == 0) {
      rc = convert_ppm_to_png_with_pillow(ppm_path, export_path);
      unlink(ppm_path);
    }
  } else {
    rc = write_ppm_from_rgb565_pixels(pixels, w, h, export_path);
  }

  if (rc != 0) {
    fprintf(stderr, "headless export failed: %s\n", export_path);
    return -1;
  }

  printf("exported preview: %s\n", export_path);
  return 0;
}

static void headless_flush_cb(lv_display_t* display, const lv_area_t* area, uint8_t* px_map) {
  (void)area;
  (void)px_map;
  lv_display_flush_ready(display);
}

static void populate_preview_state(const app_state_t* state) {
  bb_wall_time_set_unix(1711283400);
  (void)bb_display_init();

  if (state->mode == APP_MODE_STANDBY) {
    (void)bb_display_show_status(state->status[0] != '\0' ? state->status : "READY");
    return;
  }

  if (state->mode == APP_MODE_SPEAKING) {
    const char* status = state->status[0] != '\0' ? state->status : "TX";
    (void)bb_display_show_status(status);
    return;
  }

  int turns = state->turn_den > 0 ? state->turn_den : 1;
  for (int i = 1; i < turns; ++i) {
    char older_you[64];
    char older_reply[96];
    snprintf(older_you, sizeof(older_you), "Earlier turn %d", i);
    snprintf(older_reply, sizeof(older_reply), "Previous assistant reply %d.", i);
    (void)bb_display_show_chat_turn(older_you, older_reply);
  }
  (void)bb_display_show_chat_turn(state->you, state->reply);

  if (state->mode == APP_MODE_NOTIFICATION || state->turn_den > 0) {
    (void)bb_display_show_status(state->status[0] != '\0' ? state->status : "TASK");
  } else {
    (void)bb_display_show_status(state->status[0] != '\0' ? state->status : "READY");
  }

  int back_steps = state->turn_den - state->turn_num;
  for (int i = 0; i < back_steps; ++i) {
    (void)bb_display_chat_prev_turn();
  }
}

int main(int argc, char** argv) {
  app_state_t state;
  lv_display_t* display = NULL;
  void* headless_buf = NULL;

  signal(SIGINT, on_signal);
  signal(SIGTERM, on_signal);

  init_default_state(&state);
  parse_args(&state, argc, argv);

  lv_init();

  if (state.export_path[0] != '\0') {
    const size_t headless_buf_size = (size_t)DISP_W * (size_t)DISP_H * sizeof(uint16_t);
    headless_buf = malloc(headless_buf_size);
    if (headless_buf == NULL) {
      fprintf(stderr, "failed to allocate export buffer\n");
      return 1;
    }

    display = lv_display_create(DISP_W, DISP_H);
    if (display == NULL) {
      fprintf(stderr, "failed to create headless display\n");
      free(headless_buf);
      return 1;
    }

    lv_display_set_flush_cb(display, headless_flush_cb);
    lv_display_set_buffers(display, headless_buf, NULL, (uint32_t)headless_buf_size, LV_DISPLAY_RENDER_MODE_FULL);
    populate_preview_state(&state);
    lv_timer_handler();
    lv_refr_now(display);
    int export_rc = export_headless_buffer_image(state.export_path, (const uint16_t*)headless_buf, DISP_W, DISP_H);
    free(headless_buf);
    return export_rc == 0 ? 0 : 1;
  }

  display = lv_sdl_window_create(DISP_W, DISP_H);
  if (display == NULL) {
    fprintf(stderr, "failed to create SDL window\n");
    return 1;
  }

  lv_sdl_window_set_title(display, "BBClaw LVGL Preview");
  lv_sdl_window_set_zoom(display, state.zoom);
  lv_sdl_window_set_resizeable(display, false);
  lv_sdl_mouse_create();
  lv_sdl_mousewheel_create();
  lv_sdl_keyboard_create();

  populate_preview_state(&state);
  state.start_tick = lv_tick_get();

  printf("BBClaw LVGL simulator: 320x172, zoom=%.2f\n", state.zoom);
  printf("status=%s mode=%d timeout_ms=%u\n", state.status, (int)state.mode, state.timeout_ms);

  while (!s_exit_requested) {
    lv_timer_handler();
    usleep(5000);

    if (state.timeout_ms > 0 && lv_tick_elaps(state.start_tick) >= state.timeout_ms) {
      break;
    }
  }

  lv_sdl_quit();
  return 0;
}
