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
#include "sdmmc_cmd.h"

static const char* TAG = "bb_sdcard";
#define MOUNT_POINT "/sdcard"

static sdmmc_card_t* s_card;

esp_err_t bb_sdcard_mount(void) {
  if (s_card != NULL) return ESP_OK;

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
      .format_if_mount_failed = false, /* 不主动格式化——卡上可能有用户数据 */
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
    return err;
  }
  ESP_LOGI(TAG, "mounted %s: %s %.1fGB", MOUNT_POINT, s_card->cid.name,
           ((float)s_card->csd.capacity * s_card->csd.sector_size) / (1024.0f * 1024.0f * 1024.0f));
  return ESP_OK;
}

void bb_sdcard_unmount(void) {
  if (s_card == NULL) return;
  esp_vfs_fat_sdcard_unmount(MOUNT_POINT, s_card);
  s_card = NULL;
  ESP_LOGI(TAG, "unmounted");
}

int bb_sdcard_mounted(void) { return s_card != NULL; }

esp_err_t bb_sdcard_format(void) {
  if (s_card == NULL) return ESP_ERR_INVALID_STATE;
  ESP_LOGW(TAG, "formatting card (FAT), all data on card will be erased");
  esp_err_t err = esp_vfs_fat_sdcard_format(MOUNT_POINT, s_card);
  ESP_LOGI(TAG, "format: %s", esp_err_to_name(err));
  return err;
}

esp_err_t bb_sdcard_selftest(void) {
  if (s_card == NULL) return ESP_ERR_INVALID_STATE;
  ESP_LOGI(TAG, "selftest: card=%s cap=%.1fGB", s_card->cid.name,
           ((float)s_card->csd.capacity * s_card->csd.sector_size) / (1024.0f * 1024.0f * 1024.0f));
  errno = 0;
  FILE* f = fopen(MOUNT_POINT "/bbtest.txt", "w");
  if (f == NULL) {
    ESP_LOGE(TAG, "selftest fopen(w) failed errno=%d(%s)", errno, strerror(errno));
    return ESP_FAIL;
  }
  errno = 0;
  int n = fputs("bbclaw sd selftest\n", f);
  int fe = (n < 0) ? errno : 0;
  errno = 0;
  int cl = fclose(f);
  ESP_LOGI(TAG, "selftest write: fputs=%d(errno=%d) fclose=%d(errno=%d)", n, fe, cl, errno);
  if (n < 0 || cl != 0) return ESP_FAIL;
  /* 读回校验:假卡/坏卡会对写命令应答成功但数据不持久——unlink ENOENT
   * 已经暗示目录项没落盘,这里终审。 */
  errno = 0;
  f = fopen(MOUNT_POINT "/bbtest.txt", "r");
  if (f == NULL) {
    ESP_LOGE(TAG, "selftest READ-BACK FAILED errno=%d(%s) — 卡对写入撒谎(假卡/坏卡),换卡",
             errno, strerror(errno));
    return ESP_FAIL;
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
  return ESP_OK;
}

esp_err_t bb_sdcard_space(uint64_t* total_kb, uint64_t* free_kb) {
  if (s_card == NULL) return ESP_ERR_INVALID_STATE;
  FATFS* fs = NULL;
  DWORD free_clust = 0;
  if (f_getfree("0:", &free_clust, &fs) != FR_OK || fs == NULL) return ESP_FAIL;
  const uint64_t clust_kb = ((uint64_t)fs->csize * 512ULL) / 1024ULL;
  if (total_kb != NULL) *total_kb = (uint64_t)(fs->n_fatent - 2) * clust_kb;
  if (free_kb != NULL) *free_kb = (uint64_t)free_clust * clust_kb;
  return ESP_OK;
}

#else /* !BBCLAW_SDMMC_ENABLE */

esp_err_t bb_sdcard_mount(void) { return ESP_ERR_NOT_SUPPORTED; }
void bb_sdcard_unmount(void) {}
int bb_sdcard_mounted(void) { return 0; }
esp_err_t bb_sdcard_space(uint64_t* total_kb, uint64_t* free_kb) {
  (void)total_kb;
  (void)free_kb;
  return ESP_ERR_NOT_SUPPORTED;
}

#endif
