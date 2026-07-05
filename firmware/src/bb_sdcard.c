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
  host.max_freq_khz = SDMMC_FREQ_DEFAULT; /* 20MHz；1-bit 实际带宽仍远超需求 */

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
    ESP_LOGW(TAG, "mount failed: %s (no card?)", esp_err_to_name(err));
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
