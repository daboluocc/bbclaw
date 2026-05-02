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
