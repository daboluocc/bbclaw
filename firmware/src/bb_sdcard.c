/**
 * bb_sdcard.c — Micro SD（SDMMC 1-bit）挂载（ADR-044 §3.4）。
 *
 * 手表板卡槽引脚：CLK=2 / CMD=1 / D0=3（design/boards/…-amoled-2.06.md）。
 * 1-bit 模式够用：录音写入 ~2.4KB/s，远低于 1-bit SDMMC 的 MB/s 量级。
 * 无卡是常态：mount 失败只降级（录音功能不可用），不影响其它子系统。
 */
#include "bb_sdcard.h"

#include "bb_config.h"

#if BBCLAW_SDMMC_ENABLE

#include <errno.h>
#include <string.h>
#include <unistd.h>

#include "driver/sdmmc_host.h"
#include "esp_log.h"
#include "esp_vfs_fat.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "sdmmc_cmd.h"

static const char* TAG = "bb_sdcard";
#define MOUNT_POINT "/sdcard"

static sdmmc_card_t* s_card;

/* s_card 生命周期锁:自 2026-07 起 SD 热插拔轮询在独立任务(sd_hotplug_task),
 * 会与 UI 上下文的 mount/format/selftest 并发。挂载/卸载/CMD13 探测/格式化都要串行,
 * 否则双挂载(两路都见 s_card==NULL)或 use-after-unmount。首次调用发生在 boot 初始
 * mount(单线程,轮询任务尚未创建),懒创建无竞态。mounted() 只读指针,advisory 不锁。 */
static SemaphoreHandle_t s_sd_lock;
static void ensure_sd_lock(void) {
  if (s_sd_lock == NULL) {
    s_sd_lock = xSemaphoreCreateMutex();
  }
}
#define SD_LOCK()                                              \
  do {                                                         \
    ensure_sd_lock();                                          \
    if (s_sd_lock != NULL) xSemaphoreTake(s_sd_lock, portMAX_DELAY); \
  } while (0)
#define SD_UNLOCK()                                            \
  do {                                                         \
    if (s_sd_lock != NULL) xSemaphoreGive(s_sd_lock);          \
  } while (0)

static esp_err_t sdcard_mount_impl(int format_if_mount_failed) {
  SD_LOCK();
  if (s_card != NULL) {
    SD_UNLOCK();
    return ESP_OK;
  }

  sdmmc_host_t host = SDMMC_HOST_DEFAULT();
  host.max_freq_khz = SDMMC_FREQ_DEFAULT; /* 20MHz;1-bit 带宽仍远超录音需求。
   * 2026-07-05 排障记录:曾疑此处时序问题降到 400kHz,实为坏卡(NCard 2GB
   * 写入应答成功但不持久,read-back 失败)——selftest 门禁已能识别这类卡。 */

  sdmmc_slot_config_t slot = SDMMC_SLOT_CONFIG_DEFAULT();
  slot.width = 1;
  slot.clk = BBCLAW_SDMMC_CLK_GPIO;
  slot.cmd = BBCLAW_SDMMC_CMD_GPIO;
  slot.d0 = BBCLAW_SDMMC_D0_GPIO;
  slot.flags |= SDMMC_SLOT_FLAG_INTERNAL_PULLUP; /* 板上若无外置上拉则靠内部 */

  const esp_vfs_fat_sdmmc_mount_config_t mount_cfg = {
      /* 默认不主动格式化——卡上可能有用户数据。恢复路径(录音入口,用户已
       * 知情拍板 SD 是缓存)走 bb_sdcard_mount_format() 显式格式化挂载:
       * exFAT/空白卡 FATFS 报 FR_NO_FILESYSTEM,普通 format API 又要求已
       * 挂载,只有 mount 时格式化这一条路。 */
      .format_if_mount_failed = format_if_mount_failed ? true : false,
      .max_files = 4,
      .allocation_unit_size = 16 * 1024,
  };

  esp_err_t err = esp_vfs_fat_sdmmc_mount(MOUNT_POINT, &host, &slot, &mount_cfg, &s_card);
  if (err != ESP_OK) {
    /* 热插拔轮询会周期性走到这里:失败日志只报一次,静默直到状态变化 */
    static int s_fail_logged;
    if (!s_fail_logged) {
      ESP_LOGW(TAG, "mount failed: %s (no card? polling continues quietly)", esp_err_to_name(err));
      s_fail_logged = 1;
    }
    s_card = NULL;
    SD_UNLOCK();
    return err;
  }
  ESP_LOGI(TAG, "mounted %s: %s %.1fGB%s", MOUNT_POINT, s_card->cid.name,
           ((float)s_card->csd.capacity * s_card->csd.sector_size) / (1024.0f * 1024.0f * 1024.0f),
           format_if_mount_failed ? " (format-on-fail armed)" : "");
  SD_UNLOCK();
  return ESP_OK;
}

esp_err_t bb_sdcard_mount(void) { return sdcard_mount_impl(0); }

esp_err_t bb_sdcard_mount_format(void) {
  ESP_LOGW(TAG, "mount with format-on-fail: unmountable card will be ERASED (FAT)");
  return sdcard_mount_impl(1);
}

void bb_sdcard_unmount(void) {
  SD_LOCK();
  if (s_card == NULL) {
    SD_UNLOCK();
    return;
  }
  esp_vfs_fat_sdcard_unmount(MOUNT_POINT, s_card);
  s_card = NULL;
  ESP_LOGI(TAG, "unmounted");
  SD_UNLOCK();
}

int bb_sdcard_mounted(void) { return s_card != NULL; }

int bb_sdcard_check_present(void) {
  SD_LOCK();
  if (s_card == NULL) {
    SD_UNLOCK();
    return 0;
  }
  if (sdmmc_get_status(s_card) == ESP_OK) {
    SD_UNLOCK();
    return 1;
  }
  /* CMD13 无应答=卡已拔。立刻卸载,让 mounted() 状态与现实一致,
   * 下次插回走热插拔路径重新挂载。 */
  ESP_LOGW(TAG, "card removed (status probe failed) — unmounting");
  esp_vfs_fat_sdcard_unmount(MOUNT_POINT, s_card);
  s_card = NULL;
  SD_UNLOCK();
  return -1;
}

esp_err_t bb_sdcard_format(void) {
  SD_LOCK();
  if (s_card == NULL) {
    SD_UNLOCK();
    return ESP_ERR_INVALID_STATE;
  }
  /* 拔卡防御(2026-07-07 真机 panic 实锤):卡被拔后 mounted 状态陈旧,
   * 对已拔卡跑 esp_vfs_fat_sdcard_format = LoadProhibited(vfs_fat_sdmmc.c:519,
   * task=bb_stream_task)。先 CMD13 验在位,不在就卸载返错,别格式化空气。 */
  if (sdmmc_get_status(s_card) != ESP_OK) {
    ESP_LOGW(TAG, "format refused: card no longer present — unmounting");
    esp_vfs_fat_sdcard_unmount(MOUNT_POINT, s_card);
    s_card = NULL;
    SD_UNLOCK();
    return ESP_ERR_NOT_FOUND;
  }
  ESP_LOGW(TAG, "formatting card (FAT), all data on card will be erased");
  esp_err_t err = esp_vfs_fat_sdcard_format(MOUNT_POINT, s_card);
  ESP_LOGI(TAG, "format: %s", esp_err_to_name(err));
  SD_UNLOCK();
  return err;
}

esp_err_t bb_sdcard_selftest(void) {
  /* 全程持锁:避免 sd_hotplug_task 的 check_present 在 selftest 持 FILE* 期间因
   * 真机拔卡而 esp_vfs_fat_sdcard_unmount(注销 VFS)→ use-after-free。 */
  SD_LOCK();
  esp_err_t ret = ESP_OK;
  if (s_card == NULL) {
    ret = ESP_ERR_INVALID_STATE;
    goto done;
  }
  ESP_LOGI(TAG, "selftest: card=%s cap=%.1fGB", s_card->cid.name,
           ((float)s_card->csd.capacity * s_card->csd.sector_size) / (1024.0f * 1024.0f * 1024.0f));
  errno = 0;
  FILE* f = fopen(MOUNT_POINT "/bbtest.txt", "w");
  if (f == NULL) {
    ESP_LOGE(TAG, "selftest fopen(w) failed errno=%d(%s)", errno, strerror(errno));
    ret = ESP_FAIL;
    goto done;
  }
  errno = 0;
  int n = fputs("bbclaw sd selftest\n", f);
  int fe = (n < 0) ? errno : 0;
  errno = 0;
  int cl = fclose(f);
  ESP_LOGI(TAG, "selftest write: fputs=%d(errno=%d) fclose=%d(errno=%d)", n, fe, cl, errno);
  if (n < 0 || cl != 0) {
    ret = ESP_FAIL;
    goto done;
  }
  /* 读回校验:假卡/坏卡会对写命令应答成功但数据不持久——unlink ENOENT
   * 已经暗示目录项没落盘,这里终审。 */
  errno = 0;
  f = fopen(MOUNT_POINT "/bbtest.txt", "r");
  if (f == NULL) {
    ESP_LOGE(TAG, "selftest READ-BACK FAILED errno=%d(%s) — 卡对写入撒谎(假卡/坏卡),换卡",
             errno, strerror(errno));
    ret = ESP_FAIL;
    goto done;
  }
  char rb[32] = {0};
  char* got = fgets(rb, sizeof(rb), f);
  fclose(f);
  ESP_LOGI(TAG, "selftest read-back: %s", (got != NULL && strstr(rb, "bbclaw") != NULL) ? "MATCH" : "MISMATCH");
  errno = 0;
  if (unlink(MOUNT_POINT "/bbtest.txt") != 0) {
    ESP_LOGW(TAG, "selftest unlink errno=%d(%s)", errno, strerror(errno));
  }
  ESP_LOGI(TAG, "selftest done");
done:
  SD_UNLOCK();
  return ret;
}

esp_err_t bb_sdcard_space(uint64_t* total_kb, uint64_t* free_kb) {
  if (s_card == NULL) return ESP_ERR_INVALID_STATE;
  /* 按挂载路径查询:板上有两个 FAT 卷(内部字体分区+SD),裸卷标 "0:" 会查到
   * 内部分区(录音页 SD 空间不显示的根因)。 */
  uint64_t total = 0, freeb = 0;
  esp_err_t err = esp_vfs_fat_info(MOUNT_POINT, &total, &freeb);
  if (err != ESP_OK) return err;
  if (total_kb != NULL) *total_kb = total / 1024ULL;
  if (free_kb != NULL) *free_kb = freeb / 1024ULL;
  return ESP_OK;
}

#else /* !BBCLAW_SDMMC_ENABLE */

esp_err_t bb_sdcard_mount(void) { return ESP_ERR_NOT_SUPPORTED; }
esp_err_t bb_sdcard_mount_format(void) { return ESP_ERR_NOT_SUPPORTED; }
int bb_sdcard_check_present(void) { return 0; }
void bb_sdcard_unmount(void) {}
int bb_sdcard_mounted(void) { return 0; }
esp_err_t bb_sdcard_selftest(void) { return ESP_ERR_NOT_SUPPORTED; }
esp_err_t bb_sdcard_format(void) { return ESP_ERR_NOT_SUPPORTED; }
esp_err_t bb_sdcard_space(uint64_t* total_kb, uint64_t* free_kb) {
  (void)total_kb;
  (void)free_kb;
  return ESP_ERR_NOT_SUPPORTED;
}

#endif
