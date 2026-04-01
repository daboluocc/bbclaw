#include "bb_gateway_node.h"

#include <stddef.h>

#include "esp_log.h"

static const char* TAG = "bb_gateway";
static bb_gateway_node_config_t s_config;

esp_err_t bb_gateway_node_init(const bb_gateway_node_config_t* config) {
  if (config == NULL) {
    return ESP_ERR_INVALID_ARG;
  }
  s_config = *config;
  ESP_LOGI(TAG, "node init: id=%s, url=%s", s_config.node_id, s_config.gateway_url);
  return ESP_OK;
}

esp_err_t bb_gateway_node_connect(void) {
  ESP_LOGI(TAG, "connect gateway (stub): %s", s_config.gateway_url ? s_config.gateway_url : "(unset)");
  return ESP_OK;
}

esp_err_t bb_gateway_node_send_ptt_state(int pressed) {
  ESP_LOGI(TAG, "send node.event ptt=%d", pressed ? 1 : 0);
  return ESP_OK;
}

esp_err_t bb_gateway_node_send_voice_transcript(const char* text, const char* session_key, const char* stream_id) {
  if (text == NULL || text[0] == '\0') {
    return ESP_ERR_INVALID_ARG;
  }
  ESP_LOGI(TAG, "send node.event voice.transcript session=%s stream=%s text=%s",
           session_key != NULL ? session_key : "(default)", stream_id != NULL ? stream_id : "(unset)", text);
  return ESP_OK;
}
