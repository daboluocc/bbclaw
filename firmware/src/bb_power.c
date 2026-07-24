#include "bb_power.h"

#include <stdint.h>
#include <stdlib.h>
#include "bb_config.h"
#include "esp_check.h"
#include "esp_log.h"

#if !defined(BBCLAW_SIMULATOR) && BBCLAW_POWER_ENABLE && (BBCLAW_POWER_ADC_GPIO >= 0)
#include "esp_adc/adc_cali.h"
#include "esp_adc/adc_cali_scheme.h"
#include "esp_adc/adc_oneshot.h"
#endif

#if !defined(BBCLAW_SIMULATOR) && (BBCLAW_POWER_SOURCE_AXP2101 || BBCLAW_POWER_SOURCE_M5PM1)
#include "bb_audio.h"
#include "driver/i2c_master.h"
#endif

static const char* TAG = "bb_power";
static bb_power_state_t s_state = {
    .supported = BBCLAW_POWER_SUPPORTED ? 1 : 0,
    .available = 0,
    .millivolts = 0,
    .percent = -1,
    .low = 0,
    .charging = 0,
};

#if !defined(BBCLAW_SIMULATOR) && BBCLAW_POWER_ENABLE && (BBCLAW_POWER_ADC_GPIO >= 0)
static adc_oneshot_unit_handle_t s_adc_handle;
static adc_channel_t s_adc_channel;
static adc_cali_handle_t s_adc_cali_handle;
static int s_adc_cali_ready;
/* 跨周期 EMA 电压滤波状态（mV）。s_vbat_ema_init=0 表示首次采样，直接置入。 */
static int s_vbat_ema_mv;
static int s_vbat_ema_init;
/* 上次对外展示的电量百分比，用于迟滞（-1 = 尚未输出过）。 */
static int s_last_shown_pct = -1;
#endif

static int clamp_percent(int pct) {
  if (pct < 0) return 0;
  if (pct > 100) return 100;
  return pct;
}

/* 单节锂电池 OCV–SoC 放电曲线（轻载/静置近似），电压降序排列。
 * 替代第一版线性映射：锂电压在 3.7V 附近停留很久，低电量区才快速跌落，
 * 线性映射会导致掉电飞快/中段卡住/末端突跳，查表+插值更贴合真实电量。
 * 详见 docs/feat/power-management-foundation.md。 */
typedef struct {
  int mv;
  int pct;
} bb_ocv_point_t;

static const bb_ocv_point_t k_ocv_curve[] = {
    {4200, 100}, {4150, 95}, {4110, 90}, {4080, 85}, {4020, 80},
    {3980, 75},  {3950, 70}, {3910, 65}, {3870, 60}, {3850, 55},
    {3840, 50},  {3820, 45}, {3800, 40}, {3790, 35}, {3770, 30},
    {3750, 25},  {3730, 20}, {3710, 15}, {3690, 10}, {3610, 5},
    {3400, 0},
};

static int battery_percent_from_mv(int mv) {
  const int n = (int)(sizeof(k_ocv_curve) / sizeof(k_ocv_curve[0]));
  /* 端点 clamp（也尊重板级 FULL/EMPTY 边界）。 */
  if (mv >= k_ocv_curve[0].mv) return 100;
  if (mv <= k_ocv_curve[n - 1].mv) return 0;
  /* 在相邻两点间线性插值。曲线按电压降序，找到 mv 所在区间。 */
  for (int i = 0; i < n - 1; ++i) {
    const int hi_mv = k_ocv_curve[i].mv;
    const int lo_mv = k_ocv_curve[i + 1].mv;
    if (mv <= hi_mv && mv >= lo_mv) {
      const int hi_pct = k_ocv_curve[i].pct;
      const int lo_pct = k_ocv_curve[i + 1].pct;
      const int span_mv = hi_mv - lo_mv;
      if (span_mv <= 0) return clamp_percent(lo_pct);
      return clamp_percent(lo_pct + (mv - lo_mv) * (hi_pct - lo_pct) / span_mv);
    }
  }
  return 0;
}

void bb_power_get_state(bb_power_state_t* out_state) {
  if (out_state == NULL) return;
  *out_state = s_state;
}

int bbclaw_power_is_charging(void) {
  return s_state.charging;
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

#if !defined(BBCLAW_SIMULATOR) && BBCLAW_POWER_SOURCE_AXP2101
/* ── AXP2101 硬件电量计后端（手表）──
 * 数据全部来自 PMU 寄存器：0xA4 电量百分比（内置库仑计+OCV 融合）、
 * 0x01 bit[6:5] 电池电流方向（01=充电）、0x00 bit3 电池在位。
 * I2C 设备懒加载：共享总线由 bb_audio_init 创建（先于首次 poll）。 */
#define AXP2101_ADDR 0x34

static i2c_master_dev_handle_t s_axp_dev;

static esp_err_t axp_read_reg(uint8_t reg, uint8_t* out) {
  return i2c_master_transmit_receive(s_axp_dev, &reg, 1, out, 1, 100);
}

static esp_err_t power_refresh_axp2101(void) {
  if (s_axp_dev == NULL) {
    i2c_master_bus_handle_t bus = bb_audio_shared_i2c_bus();
    if (bus == NULL) return ESP_ERR_INVALID_STATE; /* audio init 未到,下轮再试 */
    const i2c_device_config_t cfg = {
        .dev_addr_length = I2C_ADDR_BIT_LEN_7,
        .device_address = AXP2101_ADDR,
        .scl_speed_hz = 400000,
    };
    ESP_RETURN_ON_ERROR(i2c_master_bus_add_device(bus, &cfg, &s_axp_dev), TAG, "axp dev add");
    ESP_LOGI(TAG, "power source: axp2101 fuel gauge");
  }
  uint8_t status1 = 0, status2 = 0, pct = 0;
  ESP_RETURN_ON_ERROR(axp_read_reg(0x00, &status1), TAG, "axp status1");
  ESP_RETURN_ON_ERROR(axp_read_reg(0x01, &status2), TAG, "axp status2");
  ESP_RETURN_ON_ERROR(axp_read_reg(0xA4, &pct), TAG, "axp soc");

  const int present = (status1 >> 3) & 1;
  const int chg_dir = (status2 >> 5) & 0x3; /* 01=charging, 10=discharging */

  s_state.supported = 1;
  s_state.available = present;
  s_state.millivolts = 0; /* 电量计直接给百分比,电压展示暂不需要 */
  s_state.percent = present ? clamp_percent((int)pct) : -1;
  s_state.low = (present && s_state.percent <= BBCLAW_POWER_LOW_PERCENT) ? 1 : 0;
  s_state.charging = (chg_dir == 1) ? 1 : 0;
  ESP_LOGD(TAG, "axp2101 present=%d pct=%d charging=%d status=0x%02X/0x%02X", present, s_state.percent,
           s_state.charging, status1, status2);
  return ESP_OK;
}
#endif /* BBCLAW_POWER_SOURCE_AXP2101 */

#if !defined(BBCLAW_SIMULATOR) && BBCLAW_POWER_SOURCE_M5PM1
/* ── M5PM1 PMIC 后端（M5StickS3）──
 * 电量：VBAT reg0x22(L)/0x23(H) 16-bit 小端 = 毫伏，映射百分比（battery_percent_from_mv）。
 * 外部供电/充电：电源来源 reg0x04 [2:0]（0=USB，2=电池）→ charging = 在 USB 上。
 * 复用 bb_audio 的 M5PM1 句柄，避免在 0x6E 重复 add_device。 */
static esp_err_t power_refresh_m5pm1(void) {
  uint8_t vb[2] = {0, 0}, vbus[2] = {0, 0}, src = 0, gin = 0;
  /* 启动早期 bb_radio_app 会先于 bb_audio 建好共享 M5PM1 I2C 句柄就试读一次 → 句柄/总线
   * 未就绪。这是正常时序,不是故障:静默 DEBUG 返回,UI poll(每 5s)与后续交互会重试成功。 */
  esp_err_t rerr = bb_audio_m5pm1_read(0x22, vb, 2); /* VBAT mV LE */
  if (rerr != ESP_OK) {
    ESP_LOGD(TAG, "m5pm1 not ready for vbat (%s)", esp_err_to_name(rerr));
    return rerr;
  }
  rerr = bb_audio_m5pm1_read(0x24, vbus, 2); /* VBUS/USB 输入 mV LE */
  if (rerr != ESP_OK) {
    ESP_LOGD(TAG, "m5pm1 not ready for vbus (%s)", esp_err_to_name(rerr));
    return rerr;
  }
  (void)bb_audio_m5pm1_read(0x04, &src, 1);  /* PWR_SRC（调试参考） */
  (void)bb_audio_m5pm1_read(0x12, &gin, 1);  /* GPIO_IN：bit0=PYG0_CHG_STAT（调试参考） */
  int mv = (int)((uint16_t)vb[0] | ((uint16_t)vb[1] << 8));
  int vbus_mv = (int)((uint16_t)vbus[0] | ((uint16_t)vbus[1] << 8));
  int present = (mv > 2500) ? 1 : 0; /* 有合理电压读数即视为电池在位 */
  /* 充电/外部供电 = USB 5V 到位（VBUS>4.2V）。比 PWR_SRC 位编码稳。 */
  int on_usb = (vbus_mv > 4200) ? 1 : 0;
  s_state.supported = 1;
  s_state.available = present;
  s_state.millivolts = mv;
  s_state.percent = present ? battery_percent_from_mv(mv) : -1;
  s_state.low = (present && s_state.percent >= 0 && s_state.percent <= BBCLAW_POWER_LOW_PERCENT) ? 1 : 0;
  s_state.charging = on_usb;
  ESP_LOGD(TAG, "m5pm1 vbat=%dmv(%d%%) vbus=%dmv usb=%d src=0x%02X gin=0x%02X", mv, s_state.percent, vbus_mv,
           on_usb, src, gin);
  return ESP_OK;
}
#endif /* BBCLAW_POWER_SOURCE_M5PM1 */

esp_err_t bb_power_refresh(void) {
#if !defined(BBCLAW_SIMULATOR) && BBCLAW_POWER_SOURCE_M5PM1
  return power_refresh_m5pm1();
#elif !defined(BBCLAW_SIMULATOR) && BBCLAW_POWER_SOURCE_AXP2101
  return power_refresh_axp2101();
#elif !defined(BBCLAW_SIMULATOR) && BBCLAW_POWER_ENABLE && (BBCLAW_POWER_ADC_GPIO >= 0)
  if (s_adc_handle == NULL) {
    return ESP_ERR_INVALID_STATE;
  }

  /* Dummy read：丢弃首次转换，规避采样保持电容在高阻分压下充电不足导致的偏低。 */
  {
    int dummy = 0;
    (void)adc_oneshot_read(s_adc_handle, s_adc_channel, &dummy);
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

  /* 跨周期 EMA 低通滤波：吸收负载瞬态（PTT/功放/WiFi）与采样噪声。
   * 首次采样直接置入，避免冷启动从 0 缓慢爬升。 */
  if (!s_vbat_ema_init) {
    s_vbat_ema_mv = vbat_mv;
    s_vbat_ema_init = 1;
  } else {
    const int a = BBCLAW_POWER_EMA_ALPHA_PCT; /* 新值权重（%） */
    s_vbat_ema_mv = (vbat_mv * a + s_vbat_ema_mv * (100 - a)) / 100;
  }
  const int filtered_mv = s_vbat_ema_mv;

  int pct = battery_percent_from_mv(filtered_mv);
  /* 百分比迟滞：变化未达阈值则维持上次展示值，消除 ±1% 抖动。
   * 首次（s_last_shown_pct<0）或达到阈值时才更新。 */
  if (s_last_shown_pct < 0 ||
      abs(pct - s_last_shown_pct) >= BBCLAW_POWER_HYSTERESIS_PCT) {
    s_last_shown_pct = pct;
  }
  pct = s_last_shown_pct;

  s_state.supported = 1;
  s_state.available = 1;
  s_state.millivolts = filtered_mv;
  s_state.percent = pct;
  s_state.low = s_state.percent <= BBCLAW_POWER_LOW_PERCENT ? 1 : 0;
  s_state.charging = 0; /* TODO: set from VBUS GPIO detection when hardware supports it */
  ESP_LOGD(TAG, "battery raw=%d adc_mv=%d vbat_raw=%d vbat_ema=%d percent=%d low=%d cali=%d",
           raw_avg, adc_mv, vbat_mv, filtered_mv, s_state.percent, s_state.low, s_adc_cali_ready);
  return ESP_OK;
#else
  s_state.supported = 0;
  s_state.available = 0;
  s_state.millivolts = 0;
  s_state.percent = -1;
  s_state.low = 0;
  s_state.charging = 0; /* TODO: set from VBUS GPIO detection when hardware supports it */
  return ESP_OK;
#endif
}
