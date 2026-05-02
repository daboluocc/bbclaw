#include "bb_transport.h"

#include <stdio.h>
#include <string.h>
#include <strings.h>

#include "bb_adapter_client.h"
#include "bb_cloud_client.h"
#include "bb_config.h"

static int is_cloud_profile(void) {
  return strcasecmp(BBCLAW_TRANSPORT_PROFILE, "cloud_saas") == 0;
}

const char* bb_transport_profile_name(void) {
  return BBCLAW_TRANSPORT_PROFILE;
}

int bb_transport_is_cloud_saas(void) {
  return is_cloud_profile();
}

int bb_transport_supports_audio_streaming(void) {
  if (!is_cloud_profile()) {
    return 1;
  }
  return BBCLAW_CLOUD_AUDIO_STREAMING_READY ? 1 : 0;
}

int bb_transport_supports_tts(void) {
  return 1;
}

int bb_transport_supports_display(void) {
  return 1;
}

static void fill_local_state(bb_transport_state_t* state, int http_status, esp_err_t err) {
  state->http_status = http_status;
  state->pairing_status = BB_TRANSPORT_PAIRING_NOT_APPLICABLE;
  state->cloud_registration_code[0] = '\0';
  state->cloud_registration_expires_at[0] = '\0';
  state->supports_audio_streaming = 1;
  state->supports_tts = 1;
  state->supports_display = 1;
  state->ready = err == ESP_OK ? 1 : 0;
  snprintf(state->detail, sizeof(state->detail), "%s", err == ESP_OK ? "ready" : "adapter_unavailable");
}

static void fill_cloud_state(bb_transport_state_t* state, const bb_cloud_health_t* health, const bb_cloud_pairing_t* pairing,
                             esp_err_t err) {
  state->http_status = health != NULL ? health->http_status : 0;
  state->supports_audio_streaming = health != NULL ? health->supports_audio_streaming : bb_transport_supports_audio_streaming();
  state->supports_tts = health != NULL ? health->supports_tts : 0;
  state->supports_display = health != NULL ? health->supports_display : 0;
  if (err != ESP_OK) {
    state->ready = 0;
    state->pairing_status = BB_TRANSPORT_PAIRING_PENDING;
    state->cloud_registration_code[0] = '\0';
    state->cloud_registration_expires_at[0] = '\0';
    if (pairing != NULL && pairing->detail[0] != '\0') {
      snprintf(state->detail, sizeof(state->detail), "%s", pairing->detail);
    } else if (state->http_status == 401 || state->http_status == 403) {
      snprintf(state->detail, sizeof(state->detail), "%s", "unauthorized");
    } else {
      snprintf(state->detail, sizeof(state->detail), "%s", "cloud_unavailable");
    }
    return;
  }

  if (pairing == NULL) {
    state->ready = 0;
    state->pairing_status = BB_TRANSPORT_PAIRING_PENDING;
    snprintf(state->detail, sizeof(state->detail), "%s", "cloud_unavailable");
    return;
  }

  if (pairing->status == BB_CLOUD_PAIR_STATUS_APPROVED) {
    state->ready = 1;
    state->pairing_status = BB_TRANSPORT_PAIRING_APPROVED;
  } else if (pairing->status == BB_CLOUD_PAIR_STATUS_BINDING_REQUIRED) {
    state->ready = 0;
    state->pairing_status = BB_TRANSPORT_PAIRING_BINDING_REQUIRED;
  } else {
    state->ready = 0;
    state->pairing_status = BB_TRANSPORT_PAIRING_PENDING;
  }
  snprintf(state->detail, sizeof(state->detail), "%s", pairing->detail);
  snprintf(state->cloud_registration_code, sizeof(state->cloud_registration_code), "%s", pairing->registration_code);
  snprintf(state->cloud_registration_expires_at, sizeof(state->cloud_registration_expires_at), "%s",
           pairing->registration_expires_at);
  state->cloud_volume_pct = pairing->volume_pct;
  state->cloud_speed_ratio_x10 = pairing->speed_ratio_x10;
  state->cloud_speaker_enabled = pairing->speaker_enabled;
  state->cloud_adapter_connected = pairing->adapter_connected;
}

static esp_err_t refresh_local(bb_transport_state_t* out_state) {
  int http_status = 0;
  esp_err_t err = bb_adapter_healthz(&http_status);
  fill_local_state(out_state, http_status, err);
  return err;
}

static esp_err_t refresh_cloud(bb_transport_state_t* out_state) {
  bb_cloud_health_t health = {0};
  esp_err_t err = bb_cloud_healthz(&health);
  if (err != ESP_OK) {
    fill_cloud_state(out_state, &health, NULL, err);
    return err;
  }

  bb_cloud_pairing_t pairing = {0};
  err = bb_cloud_pair_request(&pairing);
  if (pairing.http_status != 0) {
    health.http_status = pairing.http_status;
  }
  fill_cloud_state(out_state, &health, &pairing, err);
  return err;
}

esp_err_t bb_transport_bootstrap(bb_transport_state_t* out_state) {
  if (out_state == NULL) {
    return ESP_ERR_INVALID_ARG;
  }
  memset(out_state, 0, sizeof(*out_state));
  if (is_cloud_profile()) {
    return refresh_cloud(out_state);
  }
  return refresh_local(out_state);
}

esp_err_t bb_transport_refresh_state(bb_transport_state_t* out_state) {
  return bb_transport_bootstrap(out_state);
}

esp_err_t bb_transport_report_device_info(void) {
  if (!is_cloud_profile()) {
    return ESP_OK;
  }
  return bb_cloud_report_device_info();
}
