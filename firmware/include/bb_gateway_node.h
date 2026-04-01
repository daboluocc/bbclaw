#pragma once

#include "esp_err.h"

typedef struct {
  const char* node_id;
  const char* gateway_url;
  const char* pairing_token;
} bb_gateway_node_config_t;

esp_err_t bb_gateway_node_init(const bb_gateway_node_config_t* config);
esp_err_t bb_gateway_node_connect(void);
esp_err_t bb_gateway_node_send_ptt_state(int pressed);
esp_err_t bb_gateway_node_send_voice_transcript(const char* text, const char* session_key, const char* stream_id);
