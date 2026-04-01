#include "bb_time.h"

#include <stdlib.h>
#include <string.h>
#include <time.h>

#include "esp_log.h"
#include "esp_sntp.h"
#include "esp_timer.h"
#include <sys/time.h>

static const char *TAG = "bb_time";

static int s_sntp_started;

int64_t bb_now_ms(void) {
  return esp_timer_get_time() / 1000;
}

void bb_wall_time_set_unix(time_t unix_sec) {
  struct timeval tv = {.tv_sec = unix_sec, .tv_usec = 0};
  if (settimeofday(&tv, NULL) != 0) {
    ESP_LOGW(TAG, "settimeofday failed");
  }
}

static void sntp_sync_cb(struct timeval *tv) {
  (void)tv;
  ESP_LOGI(TAG, "wall time synced");
}

void bb_sntp_start(void) {
  if (s_sntp_started) {
    return;
  }
  s_sntp_started = 1;

  if (setenv("TZ", "CST-8", 1) != 0) {
    ESP_LOGW(TAG, "setenv TZ failed");
  }
  tzset();

  esp_sntp_setoperatingmode(ESP_SNTP_OPMODE_POLL);
  esp_sntp_setservername(0, "pool.ntp.org");
  esp_sntp_setservername(1, "time.google.com");
  esp_sntp_set_time_sync_notification_cb(sntp_sync_cb);
  esp_sntp_init();
  ESP_LOGI(TAG, "SNTP started (TZ=CST-8)");
}

bool bb_wall_time_ready(void) {
  if (esp_sntp_get_sync_status() == SNTP_SYNC_STATUS_COMPLETED) {
    return true;
  }
  /* 若已通过 Adapter / 手动 settimeofday 设过时间 */
  time_t t = time(NULL);
  return t > (time_t)1700000000;
}

void bb_wall_time_format_hm(char *buf, size_t buf_sz) {
  if (buf == NULL || buf_sz < 6U) {
    return;
  }
  if (!bb_wall_time_ready()) {
    strncpy(buf, "--:--", buf_sz);
    buf[buf_sz - 1] = '\0';
    return;
  }
  time_t now = time(NULL);
  struct tm tml;
  localtime_r(&now, &tml);
  strftime(buf, buf_sz, "%H:%M", &tml);
}
