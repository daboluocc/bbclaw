#include "bb_time.h"

#include <stdbool.h>
#include <stdio.h>
#include <string.h>
#include <sys/time.h>
#include <time.h>

#include "lvgl.h"

static time_t s_wall_time_unix = 1711283400; /* 2024-03-24 12:30:00 +0800 */
static bool s_wall_time_ready = true;

int64_t bb_now_ms(void) {
  return (int64_t)lv_tick_get();
}

void bb_sntp_start(void) {}

bool bb_wall_time_ready(void) {
  return s_wall_time_ready;
}

void bb_wall_time_format_hm(char* buf, size_t buf_sz) {
  if (buf == NULL || buf_sz < 6U) {
    return;
  }
  if (!s_wall_time_ready) {
    strncpy(buf, "--:--", buf_sz);
    buf[buf_sz - 1] = '\0';
    return;
  }

  struct tm tml;
  localtime_r(&s_wall_time_unix, &tml);
  strftime(buf, buf_sz, "%H:%M", &tml);
}

void bb_wall_time_set_unix(time_t unix_sec) {
  s_wall_time_unix = unix_sec;
  s_wall_time_ready = true;
}
