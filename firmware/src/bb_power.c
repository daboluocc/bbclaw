#include "bb_power.h"

#include <stdint.h>
#include "bb_config.h"
#include "esp_check.h"
#include "esp_log.h"

#if !defined(BBCLAW_SIMULATOR) && BBCLAW_POWER_ENABLE && (BBCLAW_POWER_ADC_GPIO >= 0)
#include "esp_adc/adc_cali.h"
#include "esp_adc/adc_cali_scheme.h"
#include "esp_adc/adc_oneshot.h"
#endif

static const char* TAG = "bb_power";
static bb_power_state_t s_state = {
    .supported = (BBCLAW_POWER_ENABLE && (BBCLAW_POWER_ADC_GPIO >= 0)) ? 1 : 0,
    .available = 0,
    .millivolts = 0,
    .percent = -1,
    .low = 0,
};

#if !defined(BBCLAW_SIMULATOR) && BBCLAW_POWER_ENABLE && (BBCLAW_POWER_ADC_GPIO >= 0)
static adc_oneshot_unit_handle_t s_adc_handle;
static adc_channel_t s_adc_channel;
static adc_cali_handle_t s_adc_cali_handle;
static int s_adc_cali_ready;
#endif

static int clamp_percent(int pct) {
  if (pct < 0) return 0;
  if (pct > 100) return 100;
  return pct;
}

static int battery_percent_from_mv(int mv) {
  const int empty_mv = BBCLAW_POWER_BATTERY_EMPTY_MV;
  const int full_mv = BBCLAW_POWER_BATTERY_FULL_MV;
  if (mv <= empty_mv) return 0;
  if (mv >= full_mv) return 100;
  if (full_mv <= empty_mv) return 0;
  return clamp_percent((mv - empty_mv) * 100 / (full_mv - empty_mv));
}

void bb_power_get_state(bb_power_state_t* out_state) {
  if (out_state == NULL) return;
  *out_state = s_state;
}

esp_err_t bb_power_init(void) {
#if !defined(BBCLAW_SIMULATOR) && BBCLAW_POWER_ENABLE && (BBCLAW_POWER_ADC_GPIO >= 0)
  adc_channel_t channel;
  switch (BBCLAW_POWER_ADC_GPIO) {
    case 1: channel = ADC_CHANNEL_0; break;
    case 2: channel = ADC_CHANNEL_1; break;
    case 3: channel = ADC_CHANNEL_2; break;
    case 4: channel = ADC_CHANNEL_3; break;
    case 5: channel = ADC_CHANNEL_4; break;
    case 6: channel = ADC_CHANNEL_5; break;
    case 7: channel = ADC_CHANNEL_6; break;
    case 8: channel = ADC_CHANNEL_7; break;
    case 9: channel = ADC_CHANNEL_8; break;
    case 10: channel = ADC_CHANNEL_9; break;
    default:
      ESP_LOGE(TAG, "unsupported adc gpio=%d", BBCLAW_POWER_ADC_GPIO);
      return ESP_ERR_NOT_SUPPORTED;
  }

  adc_oneshot_unit_init_cfg_t unit_cfg = {
      .unit_id = ADC_UNIT_1,
  };
  ESP_RETURN_ON_ERROR(adc_oneshot_new_unit(&unit_cfg, &s_adc_handle), TAG, "adc unit init failed");

  adc_oneshot_chan_cfg_t chan_cfg = {
      .atten = ADC_ATTEN_DB_12,
      .bitwidth = ADC_BITWIDTH_DEFAULT,
  };
  ESP_RETURN_ON_ERROR(adc_oneshot_config_channel(s_adc_handle, channel, &chan_cfg), TAG, "adc channel init failed");

  s_adc_channel = channel;
  s_adc_cali_ready = 0;

#if ADC_CALI_SCHEME_CURVE_FITTING_SUPPORTED
  adc_cali_curve_fitting_config_t cali_cfg = {
      .unit_id = ADC_UNIT_1,
      .atten = ADC_ATTEN_DB_12,
      .bitwidth = ADC_BITWIDTH_DEFAULT,
  };
  if (adc_cali_create_scheme_curve_fitting(&cali_cfg, &s_adc_cali_handle) == ESP_OK) {
    s_adc_cali_ready = 1;
  }
#elif ADC_CALI_SCHEME_LINE_FITTING_SUPPORTED
  adc_cali_line_fitting_config_t cali_cfg = {
      .unit_id = ADC_UNIT_1,
      .atten = ADC_ATTEN_DB_12,
      .bitwidth = ADC_BITWIDTH_DEFAULT,
  };
  if (adc_cali_create_scheme_line_fitting(&cali_cfg, &s_adc_cali_handle) == ESP_OK) {
    s_adc_cali_ready = 1;
  }
#endif

  ESP_LOGI(TAG,
           "power adc ready gpio=%d channel=%d divider=%d/%d cali=%d",
           BBCLAW_POWER_ADC_GPIO, (int)s_adc_channel, BBCLAW_POWER_ADC_RTOP_OHM, BBCLAW_POWER_ADC_RBOT_OHM,
           s_adc_cali_ready);
  return ESP_OK;
#else
  return ESP_OK;
#endif
}

esp_err_t bb_power_refresh(void) {
#if !defined(BBCLAW_SIMULATOR) && BBCLAW_POWER_ENABLE && (BBCLAW_POWER_ADC_GPIO >= 0)
  if (s_adc_handle == NULL) {
    return ESP_ERR_INVALID_STATE;
  }

  int raw_sum = 0;
  for (int i = 0; i < 8; ++i) {
    int raw = 0;
    esp_err_t err = adc_oneshot_read(s_adc_handle, s_adc_channel, &raw);
    if (err != ESP_OK) {
      ESP_LOGW(TAG, "adc read failed err=%s", esp_err_to_name(err));
      return err;
    }
    raw_sum += raw;
  }

  int raw_avg = raw_sum / 8;
  int adc_mv = 0;
  if (s_adc_cali_ready) {
    ESP_RETURN_ON_ERROR(adc_cali_raw_to_voltage(s_adc_cali_handle, raw_avg, &adc_mv), TAG, "adc convert failed");
  } else {
    adc_mv = (raw_avg * 3300) / 4095;
  }

  int vbat_mv = adc_mv;
  if (BBCLAW_POWER_ADC_RBOT_OHM > 0) {
    vbat_mv = (int)(((int64_t)adc_mv * (BBCLAW_POWER_ADC_RTOP_OHM + BBCLAW_POWER_ADC_RBOT_OHM)) /
                    BBCLAW_POWER_ADC_RBOT_OHM);
  }

  s_state.supported = 1;
  s_state.available = 1;
  s_state.millivolts = vbat_mv;
  s_state.percent = battery_percent_from_mv(vbat_mv);
  s_state.low = s_state.percent <= BBCLAW_POWER_LOW_PERCENT ? 1 : 0;
  return ESP_OK;
#else
  s_state.supported = 0;
  s_state.available = 0;
  s_state.millivolts = 0;
  s_state.percent = -1;
  s_state.low = 0;
  return ESP_OK;
#endif
}
