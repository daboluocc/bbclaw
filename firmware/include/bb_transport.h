#pragma once

#include "esp_err.h"

typedef enum {
  BB_TRANSPORT_PAIRING_NOT_APPLICABLE = 0,
  BB_TRANSPORT_PAIRING_PENDING = 1,
  BB_TRANSPORT_PAIRING_APPROVED = 2,
  BB_TRANSPORT_PAIRING_BINDING_REQUIRED = 3,
} bb_transport_pairing_status_t;

typedef struct {
  int ready;
  int supports_audio_streaming;
  int supports_tts;
  int supports_display;
  int http_status;
  bb_transport_pairing_status_t pairing_status;
  char detail[64];
  char cloud_registration_code[16];
  char cloud_registration_expires_at[40];
  int cloud_volume_pct;      /* -1 = not present */
  int cloud_speed_ratio_x10; /* -1 = not present */
} bb_transport_state_t;

const char* bb_transport_profile_name(void);
int bb_transport_is_cloud_saas(void);
int bb_transport_supports_audio_streaming(void);
int bb_transport_supports_tts(void);
int bb_transport_supports_display(void);
esp_err_t bb_transport_bootstrap(bb_transport_state_t* out_state);
esp_err_t bb_transport_refresh_state(bb_transport_state_t* out_state);
