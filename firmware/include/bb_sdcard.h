#pragma once

#include <stdint.h>

#include "esp_err.h"

/**
 * Micro SD 卡（SDMMC 1-bit）——录音本地优先存储（ADR-044 §3.4）。
 *
 * 挂载点固定 /sdcard（FATFS）。卡可能不在位：init 失败是常态路径，
 * 调用方用 bb_sdcard_mounted() 判定功能可用性，不要当错误处理。
 */

/** 挂载 SD 卡。无卡/坏卡返回错误并保持未挂载状态（可重试）。 */
esp_err_t bb_sdcard_mount(void);

/** 卸载（关机/退出录音后可选调用；平时保持挂载）。 */
void bb_sdcard_unmount(void);

/** 1 = 已挂载可用。 */
int bb_sdcard_mounted(void);

/** 容量信息（KB）；未挂载返回 ESP_ERR_INVALID_STATE。任意指针可 NULL。 */
esp_err_t bb_sdcard_space(uint64_t* total_kb, uint64_t* free_kb);
