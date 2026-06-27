#ifndef BB_OTA_H
#define BB_OTA_H

#include <stdint.h>
#include <stdbool.h>
#include "esp_err.h"

// OTA 状态
typedef enum {
    OTA_STATE_IDLE = 0,
    OTA_STATE_CHECKING,
    OTA_STATE_AVAILABLE,
    OTA_STATE_DOWNLOADING,
    OTA_STATE_VERIFYING,
    OTA_STATE_BOOTING,
} ota_state_t;

// OTA 检查结果
typedef struct ota_update_info {
    bool has_update;
    char version[32];
    char release_notes[256];
    char download_url[128];
    uint32_t size;
    char checksum[64];  // sha256:...
} ota_update_info_t;

// 进度回调 (percent: 0-100)
typedef void (*ota_progress_cb)(int percent);

// API

// 初始化 OTA 模块
esp_err_t bb_ota_init(void);

// 获取当前状态
ota_state_t bb_ota_get_state(void);
const char* bb_ota_get_state_str(ota_state_t state);

// 检查更新 (连接到 OTA 服务器查询)。保留 dev/dirty 构建护栏：本地 make flash 的
// 调试固件不会被自动 OTA 顶替（开机/静默路径用这个）。
esp_err_t bb_ota_check(ota_update_info_t *info);

// 同上，但 allow_dev_build=true 时跳过 dev/dirty 护栏——用户在菜单主动点
// 「Check Update」表示明确要升级，连 dev 构建也照常查云端（ADR-040 后续：
// dirty 也能被识别升级，但只在主动触发时，不在开机自动路径，避免调试固件被弹窗顶掉）。
esp_err_t bb_ota_check_ex(ota_update_info_t *info, bool allow_dev_build);

// 下载并烧写固件
// progress 回调会在下载过程中被调用，传入进度百分比 (0-100)
esp_err_t bb_ota_download_and_flash(ota_update_info_t *info, ota_progress_cb progress);

// 应用更新 (设置下次启动分区并重启)
esp_err_t bb_ota_apply_update(void);

// 获取当前运行的固件版本
const char* bb_ota_get_current_version(void);

// 静默检查 (启动时自动调用，不显示进度)
esp_err_t bb_ota_check_silent(void);

// 检查并清除更新完成标志（启动时调用，显示庆祝画面）
int bb_ota_was_just_updated(void);

#endif  // BB_OTA_H
