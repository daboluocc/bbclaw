/**
 * bb_pm 实现 —— 自动 light sleep + DFS（ADR-047）。
 *
 * 门控：CONFIG_PM_ENABLE（sdkconfig）× BBCLAW_PM_LIGHT_SLEEP_ENABLE（板级）。
 * 两者任缺其一则整文件退化为 no-op 桩，零运行时开销。
 */

#include "bb_pm.h"
#include "bb_config.h"

#include <esp_log.h>

static const char* TAG = "bb_pm";

#if CONFIG_PM_ENABLE && BBCLAW_PM_LIGHT_SLEEP_ENABLE

#include "esp_pm.h"
#include "sdkconfig.h"

#ifndef BBCLAW_PM_MAX_FREQ_MHZ
#define BBCLAW_PM_MAX_FREQ_MHZ 240
#endif
#ifndef BBCLAW_PM_MIN_FREQ_MHZ
#define BBCLAW_PM_MIN_FREQ_MHZ 80
#endif

/* 「交互锁」：持有 = 禁 light sleep（DFS 仍可降频）。开机默认持有（全响应），
 * 息屏进 SLEEPING 才释放，让 SoC 在 WiFi beacon 间隙自动 light-sleep。 */
static esp_pm_lock_handle_t s_interactive_lock;
static int s_lock_held; /* 1=已持有交互锁（当前禁深睡） */
static int s_ready;

esp_err_t bb_pm_init(void) {
  if (s_ready) {
    return ESP_OK;
  }

  esp_pm_config_t cfg = {
      .max_freq_mhz = BBCLAW_PM_MAX_FREQ_MHZ,
      .min_freq_mhz = BBCLAW_PM_MIN_FREQ_MHZ,
      .light_sleep_enable = true,
  };
  esp_err_t err = esp_pm_configure(&cfg);
  if (err != ESP_OK) {
    ESP_LOGE(TAG, "esp_pm_configure failed: %s", esp_err_to_name(err));
    return err;
  }

  err = esp_pm_lock_create(ESP_PM_NO_LIGHT_SLEEP, 0, "bb_interactive", &s_interactive_lock);
  if (err != ESP_OK) {
    ESP_LOGE(TAG, "pm_lock_create failed: %s", esp_err_to_name(err));
    return err;
  }

  /* 开机全响应：先持锁，等息屏状态机把设备判定为 SLEEPING 才允许深睡。 */
  esp_pm_lock_acquire(s_interactive_lock);
  s_lock_held = 1;
  s_ready = 1;

  ESP_LOGI(TAG, "light-sleep PM enabled (DFS %d↔%dMHz, auto light sleep; NO deep sleep)",
           BBCLAW_PM_MAX_FREQ_MHZ, BBCLAW_PM_MIN_FREQ_MHZ);
  return ESP_OK;
}

void bb_pm_set_sleeping(int sleeping) {
  if (!s_ready) {
    return;
  }
  if (sleeping) {
    /* SLEEPING：释放交互锁 → 空闲即 light-sleep（WiFi modem-sleep 维持关联，
     * 云端 push 靠 WiFi RX 中断照常唤醒 → 消息唤醒链路不变）。 */
    if (s_lock_held) {
      esp_pm_lock_release(s_interactive_lock);
      s_lock_held = 0;
      ESP_LOGD(TAG, "interactive lock released → light sleep allowed");
    }
  } else {
    /* ACTIVE/DIMMING/WAKING：持锁禁深睡，保证 UI/音频跟手。 */
    if (!s_lock_held) {
      esp_pm_lock_acquire(s_interactive_lock);
      s_lock_held = 1;
      ESP_LOGD(TAG, "interactive lock acquired → light sleep blocked");
    }
  }
}

void bb_pm_dump_status(void) {
  ESP_LOGI(TAG, "=== bb_pm Status ===");
  ESP_LOGI(TAG, "ready=%d interactive_lock_held=%d (0=light-sleep allowed)", s_ready, s_lock_held);
  ESP_LOGI(TAG, "DFS %d↔%dMHz, auto light sleep on", BBCLAW_PM_MAX_FREQ_MHZ, BBCLAW_PM_MIN_FREQ_MHZ);
#if defined(CONFIG_PM_PROFILING) || defined(CONFIG_PM_LIGHT_SLEEP_CALLBACKS)
  esp_pm_dump_locks(stdout);
#endif
}

#else /* 未启用：no-op 桩 */

esp_err_t bb_pm_init(void) {
  ESP_LOGI(TAG, "light-sleep PM disabled (CONFIG_PM_ENABLE / BBCLAW_PM_LIGHT_SLEEP_ENABLE off)");
  return ESP_OK;
}
void bb_pm_set_sleeping(int sleeping) { (void)sleeping; }
void bb_pm_dump_status(void) { ESP_LOGI(TAG, "bb_pm: disabled"); }

#endif
