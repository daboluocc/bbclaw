#define SDL_MAIN_HANDLED

#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include "bb_config.h"
#include "bb_display.h"
#include "bb_page_apconfig.h"
#include "bb_page_boot.h"
#include "bb_page_netconn.h"
#include "bb_page_ota_confirm.h"
#include "bb_page_prompt_select.h"
#include "bb_time.h"
#include "lvgl.h"
#include "src/drivers/sdl/lv_sdl_keyboard.h"
#include "src/drivers/sdl/lv_sdl_mouse.h"
#include "src/drivers/sdl/lv_sdl_mousewheel.h"
#include "src/drivers/sdl/lv_sdl_window.h"

/* 分辨率跟随板级配置（sdkconfig.h 来自 cmake 的 BBCLAW_SIM_CONFIG_DIR，
 * 默认 ../build/config；指到 ../build-ws206/config 即可模拟 410x502 手表屏） */
#define DISP_W BBCLAW_ST7789_WIDTH
#define DISP_H BBCLAW_ST7789_HEIGHT
#define APP_TEXT_LEN 192

typedef enum {
  APP_MODE_AUTO = 0,
  APP_MODE_IDLE = 1,
  APP_MODE_ACTIVE = 2,
  /* Legacy aliases for CLI compat */
  APP_MODE_NOTIFICATION = 2,
  APP_MODE_SPEAKING = 3,
  APP_MODE_NETCONN = 4,
  APP_MODE_LOCKED = 5,
  APP_MODE_APCONFIG = 6,
  APP_MODE_OTA_CONFIRM = 7,
  APP_MODE_PROMPT_SELECT = 13, /* ADR-033 blocking-prompt confirm page */
  APP_MODE_CHAT = 14,          /* chat overlay active: top-bar activity dot + 中文状态词 */
  APP_MODE_BOOT = 15,          /* boot splash: BBCLAW 点阵字模逐列扫亮 */
  /* LED preview modes (issue #168): maps to v2 LED states */
  APP_MODE_LED_IDLE = 8,      /* 空闲：绿常亮 */
  APP_MODE_LED_LISTENING = 9, /* 倾听：橙常亮 */
  APP_MODE_LED_THINKING = 10, /* AI 思考：橙慢闪 */
  APP_MODE_LED_WORKER = 11,   /* Worker 长任务：红慢闪 */
  APP_MODE_LED_ERROR = 12,    /* 错误/失联：红快闪 */
} app_mode_t;

/* LED 状态描述，用于 stdout 打印和色块预览 */
typedef struct {
  uint8_t  r, g, b;
  const char* anim;   /* "SOLID" / "SLOW_BLINK" / "FAST_BLINK" */
  uint32_t period_ms;
} led_state_t;

static led_state_t led_state_for_mode(app_mode_t mode) {
  switch (mode) {
    case APP_MODE_LED_ERROR:
      return (led_state_t){255, 0, 0, "FAST_BLINK", 333};
    case APP_MODE_LED_WORKER:
      return (led_state_t){255, 0, 0, "SLOW_BLINK", 1000};
    case APP_MODE_LED_THINKING:
      return (led_state_t){255, 165, 0, "SLOW_BLINK", 1000};
    case APP_MODE_LED_LISTENING:
      return (led_state_t){255, 165, 0, "SOLID", 0};
    case APP_MODE_LED_IDLE:
    default:
      return (led_state_t){0, 255, 0, "SOLID", 0};
  }
}

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
      if (strcmp(argv[i], "idle") == 0 || strcmp(argv[i], "standby") == 0) {
        state->mode = APP_MODE_IDLE;
      } else if (strcmp(argv[i], "notification") == 0) {
        state->mode = APP_MODE_NOTIFICATION;
      } else if (strcmp(argv[i], "speaking") == 0) {
        state->mode = APP_MODE_SPEAKING;
      } else if (strcmp(argv[i], "chat") == 0) {
        state->mode = APP_MODE_CHAT;
      } else if (strcmp(argv[i], "netconn") == 0) {
        state->mode = APP_MODE_NETCONN;
      } else if (strcmp(argv[i], "locked") == 0) {
        state->mode = APP_MODE_LOCKED;
      } else if (strcmp(argv[i], "apconfig") == 0) {
        state->mode = APP_MODE_APCONFIG;
      } else if (strcmp(argv[i], "ota_confirm") == 0) {
        state->mode = APP_MODE_OTA_CONFIRM;
      } else if (strcmp(argv[i], "prompt_select") == 0) {
        state->mode = APP_MODE_PROMPT_SELECT;
      } else if (strcmp(argv[i], "boot") == 0) {
        state->mode = APP_MODE_BOOT;
      } else if (strcmp(argv[i], "error") == 0) {
        state->mode = APP_MODE_LED_ERROR;
      } else if (strcmp(argv[i], "worker") == 0) {
        state->mode = APP_MODE_LED_WORKER;
      } else if (strcmp(argv[i], "thinking") == 0) {
        state->mode = APP_MODE_LED_THINKING;
      } else if (strcmp(argv[i], "listening") == 0) {
        state->mode = APP_MODE_LED_LISTENING;
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
      printf("bbclaw_lvgl_sim [--mode auto|standby|notification|speaking|netconn|locked|apconfig|ota_confirm] [--status READY|TASK|TX|RX|SPEAK]\n");
      printf("               [--you TEXT] [--reply TEXT] [--turn-num N] [--turn-den N]\n");
      printf("               [--zoom 3.0] [--timeout-ms 0] [--export PATH]\n");
      printf("\nLED preview modes (v2, issue #168):\n");
      printf("  --mode idle       绿常亮   (空闲/待机)\n");
      printf("  --mode listening  橙常亮   (倾听中，PTT 按下)\n");
      printf("  --mode thinking   橙慢闪   (AI 思考/生成中)\n");
      printf("  --mode worker     红慢闪   (Worker 长任务)\n");
      printf("  --mode error      红快闪   (错误/失联)\n");
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
  /* LED-only preview modes (issue #168): print LED state to stdout, skip LVGL */
  if (state->mode >= APP_MODE_LED_IDLE && state->mode <= APP_MODE_LED_ERROR) {
    led_state_t ls = led_state_for_mode(state->mode);
    printf("LED color=#%02X%02X%02X anim=%s period_ms=%u\n",
           ls.r, ls.g, ls.b, ls.anim, (unsigned)ls.period_ms);
    return;
  }
  (void)bb_display_init();

  if (state->mode == APP_MODE_IDLE) {
    bb_display_set_battery(1, 1, 82, 0, 0);
    (void)bb_display_show_status(state->status[0] != '\0' ? state->status : "READY");
    return;
  }

  if (state->mode == APP_MODE_LOCKED) {
    bb_display_set_locked(1);
    bb_display_set_battery(1, 1, 82, 0, 0);
    /* --status VERIFY / "VERIFY TX" / "VERIFY ERR" previews those beats */
    (void)bb_display_show_status(state->status[0] != '\0' ? state->status : "LOCKED");
    return;
  }

  if (state->mode == APP_MODE_BOOT) {
    /* Boot splash on lv_layer_top over the standby base view — mirrors the
     * device boot path. Sweep 30 cols × 35 ms + underline ≈ 1.5 s. */
    bb_display_set_battery(1, 1, 82, 0, 0);
    (void)bb_display_show_status("READY");
    bb_page_boot_show();
    return;
  }

  if (state->mode == APP_MODE_NETCONN) {
    /* Standby beneath, netconn page on lv_layer_top — wifi stub never
     * connects so the page loops its arc animation forever. */
    bb_display_set_battery(1, 1, 82, 0, 0);
    (void)bb_display_show_status("READY");
    bb_page_netconn_show();
    return;
  }

  if (state->mode == APP_MODE_APCONFIG) {
    /* SoftAP provisioning 配网 page on lv_layer_top — sample AP info comes
     * from the wifi stub getters. */
    bb_display_set_battery(1, 1, 82, 0, 0);
    (void)bb_display_show_status("WIFI AP");
    bb_page_apconfig_show();
    return;
  }

  if (state->mode == APP_MODE_OTA_CONFIRM) {
    /* OTA user-confirm page: shows version prompt + 30 s countdown bar. */
    bb_display_set_battery(1, 1, 82, 0, 0);
    (void)bb_display_show_status("READY");
    bb_page_ota_confirm_show("v0.4.17", "v0.4.18", 1024 * 1024 + 256 * 1024, NULL);
    return;
  }

  if (state->mode == APP_MODE_PROMPT_SELECT) {
    /* ADR-033 blocking-prompt confirm page preview (a real claude permission menu). */
    bb_display_set_battery(1, 1, 82, 0, 0);
    (void)bb_display_show_status("READY");
    bb_prompt_t p;
    memset(&p, 0, sizeof(p));
    snprintf(p.prompt_id, sizeof(p.prompt_id), "p1");
    snprintf(p.kind, sizeof(p.kind), "permission");
    snprintf(p.question, sizeof(p.question), "Do you want to proceed?");
    snprintf(p.options[0].key, sizeof(p.options[0].key), "1");
    snprintf(p.options[0].label, sizeof(p.options[0].label), "Yes");
    p.options[0].is_default = 1;
    snprintf(p.options[1].key, sizeof(p.options[1].key), "2");
    snprintf(p.options[1].label, sizeof(p.options[1].label), "Yes, allow reading etc/ this project");
#if BBCLAW_ST7789_HEIGHT > BBCLAW_ST7789_WIDTH
    /* 竖屏手表预览注入 4 选项 = BB_PROMPT_MAX_OPTIONS 最坏情况，验证 Y 栈
     * （4 行 + 倒计时条 + hint 不重叠）。方屏预览保持原 3 选项不动。 */
    snprintf(p.options[2].key, sizeof(p.options[2].key), "3");
    snprintf(p.options[2].label, sizeof(p.options[2].label), "Yes, allow all edits during this session");
    snprintf(p.options[3].key, sizeof(p.options[3].key), "4");
    snprintf(p.options[3].label, sizeof(p.options[3].label), "No, and tell Claude what to do differently");
    p.n_options = 4;
#else
    snprintf(p.options[2].key, sizeof(p.options[2].key), "3");
    snprintf(p.options[2].label, sizeof(p.options[2].label), "No");
    p.n_options = 3;
#endif
    bb_page_prompt_select_show(&p, NULL);
    return;
  }

  /* For speaking/active modes, always populate chat turns first */
  int turns = state->turn_den > 0 ? state->turn_den : 1;
  for (int i = 1; i < turns; ++i) {
    char older_you[64];
    char older_reply[96];
    snprintf(older_you, sizeof(older_you), "Earlier turn %d", i);
    snprintf(older_reply, sizeof(older_reply), "Previous assistant reply %d.", i);
    (void)bb_display_show_chat_turn(older_you, older_reply);
  }
  (void)bb_display_show_chat_turn(state->you, state->reply);

  if (state->mode == APP_MODE_CHAT) {
    /* Chat overlay active — preview the top-bar activity dot + 中文状态词
     * (聆听中…/识别中…/回复中…/出错). --status TX|RX|SPEAK|ERR picks the beat. */
    const char* status = state->status[0] != '\0' ? state->status : "TX";
    bb_bar_state_t bs = BB_BAR_STATE_LISTENING;
    if (strcmp(status, "RX") == 0) bs = BB_BAR_STATE_BUSY;
    else if (strcmp(status, "SPEAK") == 0) bs = BB_BAR_STATE_SPEAKING;
    else if (strstr(status, "ERR") != NULL) bs = BB_BAR_STATE_ERROR;
    bb_display_set_chat_active(1);
    bb_display_set_agent_bar_state(bs);
    if (bs == BB_BAR_STATE_LISTENING) bb_display_set_record_level(72, 1);
    (void)bb_display_show_status(status);
  } else if (state->mode == APP_MODE_SPEAKING) {
    const char* status = state->status[0] != '\0' ? state->status : "TX";
    (void)bb_display_show_status(status);
  } else if (state->mode == APP_MODE_NOTIFICATION || state->turn_den > 0) {
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

  /* LED-only modes don't need LVGL or SDL — print and exit */
  if (state.mode >= APP_MODE_LED_IDLE && state.mode <= APP_MODE_LED_ERROR) {
    populate_preview_state(&state);
    return 0;
  }

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
    /* Pump ~0.7s of virtual time so animated surfaces (bottom-bar sweep,
     * clock colon, record meter) settle into a representative frame instead
     * of their t=0 initial position. Boot splash needs ~1.5s (sweep +
     * underline), so pump longer to capture its finished frame. */
    const int pump_frames = (state.mode == APP_MODE_BOOT) ? 150 : 45;
    for (int f = 0; f < pump_frames; f++) {
      lv_tick_inc(15); /* headless has no SDL tick cb — advance manually */
      lv_timer_handler();
    }
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

  printf("BBClaw LVGL simulator: %dx%d, zoom=%.2f\n", DISP_W, DISP_H, state.zoom);
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
