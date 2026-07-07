#pragma once

#include <stdint.h>

#include "esp_err.h"

/**
 * Micro SD 卡（SDMMC 1-bit）——录音本地优先存储（ADR-044 §3.4）。
 *
 * 挂载点固定 /sdcard（FATFS）。卡可能不在位：init 失败是常态路径，
 * 调用方用 bb_sdcard_mounted() 判定功能可用性，不要当错误处理。
 */

/** 挂载 SD 卡。无卡/坏卡返回错误并保持未挂载状态（可重试）。不主动格式化。 */
esp_err_t bb_sdcard_mount(void);

/** 挂载,FS 不识别(exFAT/空白/脏 FAT)时**格式化后挂载**(FAT,清空卡!)。
 *  录音入口恢复路径专用(SD 是缓存,云端才是归档——ADR-044,用户已知情拍板);
 *  调用方负责用户提示。无卡仍失败(格式化救不了卡不在)。 */
esp_err_t bb_sdcard_mount_format(void);

/** 卸载（关机/退出录音后可选调用；平时保持挂载）。 */
void bb_sdcard_unmount(void);

/** 1 = 已挂载可用。 */
int bb_sdcard_mounted(void);

/** 拔卡探测(CMD13,毫秒级)。已挂载且卡仍在返回 1;检测到已拔卡则就地卸载
 *  并返回 -1(调用方提醒用户);未挂载返回 0。录音/回放中勿调——写错误路径
 *  已兜底,且卸载会打断在读文件。 */
int bb_sdcard_check_present(void);

/** 写自检：根目录建/写/删测试文件,每步带 errno 日志（诊断读好写坏的卡） */
esp_err_t bb_sdcard_selftest(void);

/** 设备端格式化（FAT,整卡擦除!）。脏 FS 恢复用——SD 是缓存,云端才是归档
 *  (ADR-044);调用方负责用户知情。需已挂载。 */
esp_err_t bb_sdcard_format(void);

/** 容量信息（KB）；未挂载返回 ESP_ERR_INVALID_STATE。任意指针可 NULL。 */
esp_err_t bb_sdcard_space(uint64_t* total_kb, uint64_t* free_kb);
