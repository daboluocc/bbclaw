#pragma once

#include "esp_err.h"

typedef enum {
  BB_CLOUD_PAIR_STATUS_UNKNOWN = 0,
  BB_CLOUD_PAIR_STATUS_PENDING = 1,
  BB_CLOUD_PAIR_STATUS_APPROVED = 2,
  BB_CLOUD_PAIR_STATUS_BINDING_REQUIRED = 3,
} bb_cloud_pair_status_t;

typedef struct {
  bb_cloud_pair_status_t status;
  int http_status;
  char home_site_id[64];
  char detail[40];
  char registration_code[16];
  char registration_expires_at[40];
  int volume_pct;          /* 0-100, from cloud config; -1 = not present */
  int speed_ratio_x10;     /* e.g. 12 = 1.2x; -1 = not present */
  int speaker_enabled;     /* 0=disabled, 1=enabled, -1=not present */
  int adapter_connected;   /* 1=online, 0=offline, -1=not present */
} bb_cloud_pairing_t;

typedef struct {
  int http_status;
  int supports_audio_streaming;
  int supports_tts;
  int supports_display;
} bb_cloud_health_t;

esp_err_t bb_cloud_healthz(bb_cloud_health_t* out_health);
esp_err_t bb_cloud_pair_request(bb_cloud_pairing_t* out_pairing);
const char* bb_cloud_pair_status_name(bb_cloud_pair_status_t status);

/** POST device info to Cloud once after successful pairing. */
esp_err_t bb_cloud_report_device_info(void);

/** ADR-051 设备自助解绑：POST /v1/pairings/release 解除本机在云端的 claim。
 * 成功(2xx + ok:true)后才允许擦本地 NVS——云端失败时本地必须原样保留，
 * 否则设备重启后仍绑在原账号（device_id 由 MAC 派生、跨重置稳定）。 */
esp_err_t bb_cloud_release_pairing(void);
