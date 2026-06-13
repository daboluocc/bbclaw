#include "bb_uart_cmd.h"

#include "sdkconfig.h"

#ifdef CONFIG_BBCLAW_DEVICE_MONITOR

#include <ctype.h>
#include <stdlib.h>
#include <string.h>

#include "bb_nav_input.h"
#include "bb_ptt.h"
#include "driver/uart.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

static const char* TAG = "bb_uart_cmd";

#define UART_CMD_PORT (CONFIG_ESP_CONSOLE_UART_NUM)
#define UART_CMD_RX_BUF 256
#define UART_CMD_LINE_MAX 64
#define UART_CMD_PTT_TAP_DEFAULT_MS 200

static int s_started;

struct nav_name {
  const char* name;
  bb_nav_event_t event;
};

static const struct nav_name kNavNames[] = {
    {"up", BB_NAV_EVENT_UP},     {"down", BB_NAV_EVENT_DOWN},
    {"left", BB_NAV_EVENT_LEFT}, {"right", BB_NAV_EVENT_RIGHT},
    {"ok", BB_NAV_EVENT_OK},     {"back", BB_NAV_EVENT_BACK},
    {"ok-long", BB_NAV_EVENT_OK_LONG},
};

static void print_help(void) {
  ESP_LOGI(TAG, "commands: key up|down|left|right|ok|back|ok-long | ptt down|up|tap [ms] | help");
}

/* Tokenize in place on spaces. Returns argc, fills argv (max `max`). */
static int tokenize(char* line, char** argv, int max) {
  int argc = 0;
  char* p = line;
  while (*p && argc < max) {
    while (*p == ' ' || *p == '\t') p++;
    if (!*p) break;
    argv[argc++] = p;
    while (*p && *p != ' ' && *p != '\t') p++;
    if (*p) *p++ = '\0';
  }
  return argc;
}

static void handle_line(char* line) {
  /* lowercase the whole line for case-insensitive matching */
  for (char* c = line; *c; ++c) *c = (char)tolower((unsigned char)*c);

  char* argv[4];
  int argc = tokenize(line, argv, 4);
  if (argc == 0) {
    return;
  }

  if (strcmp(argv[0], "help") == 0) {
    print_help();
    return;
  }

  if (strcmp(argv[0], "key") == 0) {
    if (argc < 2) {
      ESP_LOGW(TAG, "usage: key up|down|left|right|ok|back|ok-long");
      return;
    }
    for (size_t i = 0; i < sizeof(kNavNames) / sizeof(kNavNames[0]); ++i) {
      if (strcmp(argv[1], kNavNames[i].name) == 0) {
        ESP_LOGI(TAG, "inject nav key=%s", argv[1]);
        bb_nav_input_inject(kNavNames[i].event);
        return;
      }
    }
    ESP_LOGW(TAG, "unknown key '%s'", argv[1]);
    return;
  }

  if (strcmp(argv[0], "ptt") == 0) {
    if (argc < 2) {
      ESP_LOGW(TAG, "usage: ptt down|up|tap [ms]");
      return;
    }
    if (strcmp(argv[1], "down") == 0) {
      ESP_LOGI(TAG, "inject ptt down");
      bb_ptt_inject(1);
    } else if (strcmp(argv[1], "up") == 0) {
      ESP_LOGI(TAG, "inject ptt up");
      bb_ptt_inject(0);
    } else if (strcmp(argv[1], "tap") == 0) {
      int ms = (argc >= 3) ? atoi(argv[2]) : UART_CMD_PTT_TAP_DEFAULT_MS;
      if (ms <= 0) ms = UART_CMD_PTT_TAP_DEFAULT_MS;
      ESP_LOGI(TAG, "inject ptt tap %dms", ms);
      bb_ptt_inject(1);
      vTaskDelay(pdMS_TO_TICKS(ms));
      bb_ptt_inject(0);
    } else {
      ESP_LOGW(TAG, "unknown ptt arg '%s'", argv[1]);
    }
    return;
  }

  ESP_LOGW(TAG, "unknown command '%s' (try: help)", argv[0]);
}

static void uart_cmd_task(void* arg) {
  (void)arg;
  char line[UART_CMD_LINE_MAX];
  int len = 0;
  uint8_t ch;
  print_help();
  for (;;) {
    int n = uart_read_bytes(UART_CMD_PORT, &ch, 1, portMAX_DELAY);
    if (n != 1) {
      continue;
    }
    if (ch == '\n' || ch == '\r') {
      if (len > 0) {
        line[len] = '\0';
        handle_line(line);
        len = 0;
      }
      continue;
    }
    if (len < (int)sizeof(line) - 1) {
      line[len++] = (char)ch;
    } else {
      /* overflow: reset, drop the runaway line */
      len = 0;
    }
  }
}

esp_err_t bb_uart_cmd_start(void) {
  if (s_started) {
    return ESP_OK;
  }
  /* Install the UART driver for RX only (tx_buffer=0). Console TX / ESP_LOG
   * keeps using its existing stdout path on the TX FIFO; we only consume RX,
   * so the two don't contend. */
  esp_err_t err = uart_driver_install(UART_CMD_PORT, UART_CMD_RX_BUF, 0, 0, NULL, 0);
  if (err != ESP_OK) {
    ESP_LOGW(TAG, "uart_driver_install(port=%d) failed err=%s — UART cmd disabled", UART_CMD_PORT,
             esp_err_to_name(err));
    return err;
  }
  /* inject runs the full nav/ptt callback chain (→ bb_state dispatch) inline in
   * this task, same as the esp_timer task does for real edges. Size the stack
   * with margin over that path — 3072 overflowed. */
  if (xTaskCreate(uart_cmd_task, "bb_uart_cmd", 6144, NULL, 5, NULL) != pdPASS) {
    ESP_LOGW(TAG, "uart_cmd task create failed");
    uart_driver_delete(UART_CMD_PORT);
    return ESP_ERR_NO_MEM;
  }
  s_started = 1;
  ESP_LOGI(TAG, "UART debug command channel ready on UART%d (type 'help')", UART_CMD_PORT);
  return ESP_OK;
}

#else /* !CONFIG_BBCLAW_DEVICE_MONITOR */

esp_err_t bb_uart_cmd_start(void) {
  return ESP_OK;
}

#endif
