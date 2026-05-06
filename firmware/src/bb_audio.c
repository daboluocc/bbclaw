#include "bb_audio.h"

#include <limits.h>
#include <stddef.h>
#include <stdint.h>
#include <strings.h>
#include <string.h>

#include "bb_config.h"
#include "driver/gpio.h"
#include "driver/i2c_master.h"
#include "driver/i2s_std.h"
#include "esp_check.h"
#include "esp_log.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"

#if BBCLAW_AUDIO_I2S_SLOT_BITS == 32
#define BBCLAW_I2S_SLOT_WIDTH I2S_SLOT_BIT_WIDTH_32BIT
#elif BBCLAW_AUDIO_I2S_SLOT_BITS == 16
#define BBCLAW_I2S_SLOT_WIDTH I2S_SLOT_BIT_WIDTH_16BIT
#else
#error "Unsupported BBCLAW_AUDIO_I2S_SLOT_BITS (must be 16 or 32)"
#endif

static const char* TAG = "bb_audio";
static int s_tx_active;
static int s_audio_ready;
static int s_volume_pct = BBCLAW_TTS_VOLUME_PCT;
static int s_speaker_enabled = 1; /* 1=enabled, 0=disabled; default enabled */
static int s_speaker_sw_enabled = 1; /* hardware switch state: 1=enabled, 0=disabled */
static i2c_master_bus_handle_t s_i2c_bus;
static i2c_master_dev_handle_t s_codec_dev;
static i2s_chan_handle_t s_tx_chan;
static i2s_chan_handle_t s_rx_chan;
static uint8_t s_codec_addr = BBCLAW_ES8311_I2C_ADDR;
static uint8_t s_rx_raw_buf[4096];
static int s_rx_use_32bit;
static int s_tx_use_stereo32;
static int s_rx_enabled;
static int s_rx_pick_right_locked = -1;
static uint32_t s_rx_timeout_count;
static uint32_t s_rx_frame_count;
static size_t s_rx_total_bytes;
static size_t s_rx_warmup_remaining;
static int s_rx_raw_diag_pending;
static int s_pa_ready;
static int32_t s_inmp441_hpf_prev_x;
static int32_t s_inmp441_hpf_prev_y;
static int s_tx_use_stereo16;
static int s_tx_use_mono16;
static int s_tx_effective_sample_rate = BBCLAW_AUDIO_SAMPLE_RATE;
static int s_i2s_full_duplex;
static volatile int s_playback_interrupt_requested;

typedef enum {
  BB_I2S_PREP_NONE = 0,
  BB_I2S_PREP_PLAYBACK = 1,
  BB_I2S_PREP_CAPTURE = 2,
  BB_I2S_PREP_FULL_DUPLEX = 3,
} bb_i2s_prep_mode_t;

static bb_i2s_prep_mode_t s_i2s_prep_mode;

typedef struct {
  uint64_t samples;
  uint64_t zeros;
  uint64_t positives;
  uint64_t negatives;
  uint64_t clipped;
  int16_t min_sample;
  int16_t max_sample;
  uint64_t sum_squares;
  int64_t last_log_ms;
} pcm_diag_t;

static pcm_diag_t s_diag = {
    .min_sample = INT16_MAX,
    .max_sample = INT16_MIN,
    .last_log_ms = 0,
};

static int use_es8311_input_source(void) {
  return strcasecmp(BBCLAW_AUDIO_INPUT_SOURCE, "es8311") == 0;
}

static const char* tx_config_name(void) {
  if (s_tx_use_stereo32) {
    return "stereo32";
  }
  if (s_tx_use_stereo16) {
    return "stereo16";
  }
  if (s_tx_use_mono16) {
    return "mono16_left";
  }
  return "unknown";
}

static uint32_t effective_tx_sample_rate(uint32_t sample_rate) {
  if (!s_tx_use_stereo32) {
    return sample_rate;
  }
  if (BBCLAW_AUDIO_TX_RATE_COMP_NUM <= 0 || BBCLAW_AUDIO_TX_RATE_COMP_DEN <= 0) {
    return sample_rate;
  }
  uint64_t adjusted = ((uint64_t)sample_rate * (uint64_t)BBCLAW_AUDIO_TX_RATE_COMP_NUM) /
                      (uint64_t)BBCLAW_AUDIO_TX_RATE_COMP_DEN;
  if (adjusted == 0U) {
    adjusted = sample_rate;
  }
  if (adjusted > UINT32_MAX) {
    adjusted = UINT32_MAX;
  }
  return (uint32_t)adjusted;
}

typedef struct {
  uint8_t reg;
  uint8_t value;
} es8311_reg_pair_t;

static esp_err_t es8311_write_reg(uint8_t reg, uint8_t value) {
  uint8_t payload[2] = {reg, value};
  return i2c_master_transmit(s_codec_dev, payload, sizeof(payload), 100);
}

static esp_err_t es8311_read_reg(uint8_t reg, uint8_t* out_value) {
  if (out_value == NULL) {
    return ESP_ERR_INVALID_ARG;
  }
  return i2c_master_transmit_receive(s_codec_dev, &reg, sizeof(reg), out_value, 1, 100);
}

static void es8311_log_reg_summary(void) {
  static const uint8_t kRegs[] = {0x01, 0x04, 0x06, 0x08, 0x09, 0x0A, 0x31, 0x32};
  for (size_t i = 0; i < sizeof(kRegs) / sizeof(kRegs[0]); ++i) {
    uint8_t value = 0;
    if (es8311_read_reg(kRegs[i], &value) == ESP_OK) {
      ESP_LOGI(TAG, "es8311 reg[0x%02X]=0x%02X", kRegs[i], value);
    }
  }
}

/*
 * Minimal ES8311 bring-up sequence for early integration.
 * TODO(hardware): replace with final sequence after board-level validation.
 */
static esp_err_t es8311_init_sequence(void) {
  static const es8311_reg_pair_t kInitSeq[] = {
      {0x00, 0x1F}, /* reset */
      {0x00, 0x80}, /* release reset */
      {0x44, 0x08}, /* improve I2C noise immunity */
      {0x44, 0x08}, /* first write can be flaky on ES8311 */
      {0x01, 0x30}, /* slave mode, external MCLK */
      {0x02, 0x00}, /* 16 kHz @ 4.096 MHz MCLK */
      {0x03, 0x10}, /* ADC OSR */
      {0x04, 0x20}, /* DAC OSR */
      {0x05, 0x00}, /* ADC/DAC dividers */
      {0x06, 0x03}, /* BCLK divider for 16-bit stereo I2S */
      {0x07, 0x00}, /* LRCK high byte */
      {0x08, 0xFF}, /* LRCK low byte */
      {0x09, 0x0C}, /* DAC interface: I2S, 16-bit */
      {0x0A, 0x0C}, /* ADC interface: I2S, 16-bit */
      {0x0B, 0x00},
      {0x0C, 0x00},
      {0x10, 0x1F},
      {0x11, 0x7F},
      {0x13, 0x10},
      {0x14, 0x1A}, /* analog PGA / mic path */
      {0x15, 0x40}, /* ADC ramp */
      {0x16, 0x24}, /* ADC PGA gain */
      {0x17, 0xBF}, /* ADC digital volume unity */
      {0x1B, 0x0A}, /* ADC HPF */
      {0x1C, 0x6A}, /* ADC HPF */
      {0x31, 0x00}, /* DAC unmute */
      {0x32, 0xBF}, /* DAC volume unity */
      {0x37, 0x08}, /* DAC ramp */
      {0x44, 0x58}, /* enable internal reference path */
      {0x0E, 0x02}, /* power sequencing */
      {0x12, 0x00}, /* enable DAC */
      {0x0D, 0x01}, /* power up analog blocks */
      {0x45, 0x00},
  };

  for (size_t i = 0; i < sizeof(kInitSeq) / sizeof(kInitSeq[0]); ++i) {
    esp_err_t err = es8311_write_reg(kInitSeq[i].reg, kInitSeq[i].value);
    if (err != ESP_OK) {
      ESP_LOGE(TAG, "es8311 reg write failed reg=0x%02X err=%s", kInitSeq[i].reg, esp_err_to_name(err));
      return err;
    }
  }
  return ESP_OK;
}

static esp_err_t init_i2c_and_codec(void) {
  const i2c_device_config_t dev_cfg = {
      .dev_addr_length = I2C_ADDR_BIT_LEN_7,
      .device_address = s_codec_addr,
      .scl_speed_hz = 400000,
  };
  ESP_RETURN_ON_ERROR(i2c_master_bus_add_device(s_i2c_bus, &dev_cfg, &s_codec_dev), TAG, "add codec i2c device");

  return es8311_init_sequence();
}

static esp_err_t detect_codec_address(void) {
  const uint8_t candidates[] = {BBCLAW_ES8311_I2C_ADDR, 0x19};
  for (size_t i = 0; i < sizeof(candidates); ++i) {
    esp_err_t err = i2c_master_probe(s_i2c_bus, candidates[i], 100);
    if (err == ESP_OK) {
      s_codec_addr = candidates[i];
      ESP_LOGI(TAG, "es8311 i2c detected addr=0x%02X", s_codec_addr);
      return ESP_OK;
    }
    ESP_LOGW(TAG, "es8311 probe miss addr=0x%02X err=%s", candidates[i], esp_err_to_name(err));
  }
  return ESP_ERR_NOT_FOUND;
}

static esp_err_t free_i2s_channels(void) {
  if (s_tx_chan != NULL) {
    ESP_RETURN_ON_ERROR(i2s_del_channel(s_tx_chan), TAG, "delete tx channel");
    s_tx_chan = NULL;
  }
  if (s_rx_chan != NULL) {
    ESP_RETURN_ON_ERROR(i2s_del_channel(s_rx_chan), TAG, "delete rx channel");
    s_rx_chan = NULL;
  }
  s_i2s_prep_mode = BB_I2S_PREP_NONE;
  s_i2s_full_duplex = 0;
  s_tx_use_stereo32 = 0;
  s_tx_use_stereo16 = 0;
  s_tx_use_mono16 = 0;
  s_rx_use_32bit = 0;
  s_tx_effective_sample_rate = BBCLAW_AUDIO_SAMPLE_RATE;
  return ESP_OK;
}

static esp_err_t init_i2s_channels(bool prepare_capture) {
  i2s_chan_handle_t new_tx_chan = NULL;
  i2s_chan_handle_t new_rx_chan = NULL;
  int mclk_gpio = use_es8311_input_source() ? BBCLAW_AUDIO_I2S_MCK_GPIO : I2S_GPIO_UNUSED;
  i2s_std_slot_config_t tx_slot_cfg =
      I2S_STD_PHILIPS_SLOT_DEFAULT_CONFIG(I2S_DATA_BIT_WIDTH_16BIT, I2S_SLOT_MODE_MONO);
  tx_slot_cfg.slot_bit_width = I2S_SLOT_BIT_WIDTH_16BIT;
  int new_tx_use_stereo32 = 0;
  int new_tx_use_stereo16 = 0;
  int new_tx_use_mono16 = 0;
  int new_i2s_full_duplex = 0;
  bb_i2s_prep_mode_t new_prep_mode = BB_I2S_PREP_NONE;
  esp_err_t ret = ESP_OK;
  if (use_es8311_input_source()) {
    i2s_chan_config_t chan_cfg = I2S_CHANNEL_DEFAULT_CONFIG(I2S_NUM_0, I2S_ROLE_MASTER);
    chan_cfg.auto_clear = true;
    chan_cfg.dma_desc_num = BBCLAW_AUDIO_I2S_DMA_DESC_NUM;
    chan_cfg.dma_frame_num = BBCLAW_AUDIO_I2S_DMA_FRAME_NUM;
    ESP_GOTO_ON_ERROR(i2s_new_channel(&chan_cfg, &new_tx_chan, &new_rx_chan), fail, TAG, "new i2s channel");
    tx_slot_cfg =
        (i2s_std_slot_config_t)I2S_STD_PHILIPS_SLOT_DEFAULT_CONFIG(I2S_DATA_BIT_WIDTH_16BIT, I2S_SLOT_MODE_STEREO);
    tx_slot_cfg.slot_bit_width = I2S_SLOT_BIT_WIDTH_16BIT;
    new_tx_use_stereo16 = 1;
    new_i2s_full_duplex = 1;
    new_prep_mode = BB_I2S_PREP_FULL_DUPLEX;
  } else {
    if (prepare_capture) {
      i2s_chan_config_t rx_chan_cfg = I2S_CHANNEL_DEFAULT_CONFIG(I2S_NUM_0, I2S_ROLE_MASTER);
      rx_chan_cfg.auto_clear = true;
      rx_chan_cfg.dma_desc_num = BBCLAW_AUDIO_I2S_DMA_DESC_NUM;
      rx_chan_cfg.dma_frame_num = BBCLAW_AUDIO_I2S_DMA_FRAME_NUM;
      ESP_GOTO_ON_ERROR(i2s_new_channel(&rx_chan_cfg, NULL, &new_rx_chan), fail, TAG, "new simplex rx channel");
      new_prep_mode = BB_I2S_PREP_CAPTURE;
    } else {
      i2s_chan_config_t tx_chan_cfg = I2S_CHANNEL_DEFAULT_CONFIG(I2S_NUM_0, I2S_ROLE_MASTER);
      tx_chan_cfg.auto_clear = true;
      tx_chan_cfg.dma_desc_num = BBCLAW_AUDIO_I2S_DMA_DESC_NUM;
      tx_chan_cfg.dma_frame_num = BBCLAW_AUDIO_I2S_DMA_FRAME_NUM;
      ESP_GOTO_ON_ERROR(i2s_new_channel(&tx_chan_cfg, &new_tx_chan, NULL), fail, TAG, "new simplex tx channel");
#if BBCLAW_AUDIO_TX_EXPERIMENT == 1
      /* Experiment A: TX-only stereo32, duplicate mono to [L,R]. */
      tx_slot_cfg =
          (i2s_std_slot_config_t)I2S_STD_PHILIPS_SLOT_DEFAULT_CONFIG(I2S_DATA_BIT_WIDTH_32BIT, I2S_SLOT_MODE_STEREO);
      tx_slot_cfg.slot_bit_width = I2S_SLOT_BIT_WIDTH_32BIT;
      new_tx_use_stereo32 = 1;
#elif BBCLAW_AUDIO_TX_EXPERIMENT == 2
      /* Experiment B: TX-only stereo16, duplicate mono to [L,R]. */
      tx_slot_cfg =
          (i2s_std_slot_config_t)I2S_STD_PHILIPS_SLOT_DEFAULT_CONFIG(I2S_DATA_BIT_WIDTH_16BIT, I2S_SLOT_MODE_STEREO);
      tx_slot_cfg.slot_bit_width = I2S_SLOT_BIT_WIDTH_16BIT;
      new_tx_use_stereo16 = 1;
#elif BBCLAW_AUDIO_TX_EXPERIMENT == 3
      /* Experiment C: TX-only mono16, explicit left slot for MAX98357A. */
      tx_slot_cfg =
          (i2s_std_slot_config_t)I2S_STD_PHILIPS_SLOT_DEFAULT_CONFIG(I2S_DATA_BIT_WIDTH_16BIT, I2S_SLOT_MODE_MONO);
      tx_slot_cfg.slot_bit_width = I2S_SLOT_BIT_WIDTH_16BIT;
      tx_slot_cfg.slot_mask = I2S_STD_SLOT_LEFT;
      new_tx_use_mono16 = 1;
#else
#error "Unsupported BBCLAW_AUDIO_TX_EXPERIMENT"
#endif
      new_prep_mode = BB_I2S_PREP_PLAYBACK;
    }
  }

#if BBCLAW_AUDIO_RX_STEREO_CAPTURE
  i2s_std_slot_config_t rx_slot_cfg =
      I2S_STD_PHILIPS_SLOT_DEFAULT_CONFIG(I2S_DATA_BIT_WIDTH_16BIT, I2S_SLOT_MODE_STEREO);
#else
  i2s_std_slot_config_t rx_slot_cfg =
      I2S_STD_PHILIPS_SLOT_DEFAULT_CONFIG(I2S_DATA_BIT_WIDTH_16BIT, I2S_SLOT_MODE_MONO);
#endif
  s_rx_use_32bit = use_es8311_input_source() ? 0 : 1;
  if (s_rx_use_32bit) {
    rx_slot_cfg =
        (i2s_std_slot_config_t)I2S_STD_PHILIPS_SLOT_DEFAULT_CONFIG(I2S_DATA_BIT_WIDTH_32BIT, I2S_SLOT_MODE_STEREO);
    rx_slot_cfg.slot_bit_width = I2S_SLOT_BIT_WIDTH_32BIT;
  } else {
    rx_slot_cfg.slot_bit_width = BBCLAW_I2S_SLOT_WIDTH;
  }

  uint32_t tx_rate = effective_tx_sample_rate(BBCLAW_AUDIO_SAMPLE_RATE);
  if (new_tx_chan != NULL) {
    i2s_std_config_t tx_cfg = {
        .clk_cfg = I2S_STD_CLK_DEFAULT_CONFIG(tx_rate),
        .slot_cfg = tx_slot_cfg,
        .gpio_cfg =
            {
                .mclk = mclk_gpio,
                .bclk = BBCLAW_AUDIO_I2S_BCK_GPIO,
                .ws = BBCLAW_AUDIO_I2S_WS_GPIO,
                .dout = BBCLAW_AUDIO_I2S_DO_GPIO,
                .din = use_es8311_input_source() ? BBCLAW_AUDIO_I2S_DI_GPIO : I2S_GPIO_UNUSED,
                .invert_flags =
                    {
                        .mclk_inv = false,
                        .bclk_inv = false,
                        .ws_inv = false,
                    },
            },
    };
    tx_cfg.clk_cfg.mclk_multiple = I2S_MCLK_MULTIPLE_256;
    ESP_GOTO_ON_ERROR(i2s_channel_init_std_mode(new_tx_chan, &tx_cfg), fail, TAG, "i2s tx init");
  }
  if (new_rx_chan != NULL) {
    i2s_std_config_t rx_cfg = {
        .clk_cfg = I2S_STD_CLK_DEFAULT_CONFIG(BBCLAW_AUDIO_SAMPLE_RATE),
        .slot_cfg = rx_slot_cfg,
        .gpio_cfg =
            {
                .mclk = mclk_gpio,
                .bclk = BBCLAW_AUDIO_I2S_BCK_GPIO,
                .ws = BBCLAW_AUDIO_I2S_WS_GPIO,
                .dout = I2S_GPIO_UNUSED,
                .din = BBCLAW_AUDIO_I2S_DI_GPIO,
                .invert_flags =
                    {
                        .mclk_inv = false,
                        .bclk_inv = false,
                        .ws_inv = false,
                    },
            },
    };
    rx_cfg.clk_cfg.mclk_multiple = I2S_MCLK_MULTIPLE_256;
    ESP_GOTO_ON_ERROR(i2s_channel_init_std_mode(new_rx_chan, &rx_cfg), fail, TAG, "i2s rx init");
  }
  s_tx_chan = new_tx_chan;
  s_rx_chan = new_rx_chan;
  s_i2s_prep_mode = new_prep_mode;
  s_i2s_full_duplex = new_i2s_full_duplex;
  s_tx_use_stereo32 = new_tx_use_stereo32;
  s_tx_use_stereo16 = new_tx_use_stereo16;
  s_tx_use_mono16 = new_tx_use_mono16;
  s_tx_effective_sample_rate = (int)tx_rate;
  ESP_LOGI(TAG,
           "i2s prep=%d full_duplex=%d tx_cfg=%s tx_stereo32=%d tx_stereo16=%d tx_mono16=%d tx_rate=%d base_rate=%d rx_use_32bit=%d shift_bits=%d stereo_capture=%d comp=%d/%d exp=%d dma_desc=%d dma_frame=%d",
           (int)s_i2s_prep_mode, s_i2s_full_duplex, tx_config_name(), s_tx_use_stereo32, s_tx_use_stereo16,
           s_tx_use_mono16, s_tx_effective_sample_rate, BBCLAW_AUDIO_SAMPLE_RATE, s_rx_use_32bit,
           BBCLAW_AUDIO_RX_SHIFT_BITS, BBCLAW_AUDIO_RX_STEREO_CAPTURE, BBCLAW_AUDIO_TX_RATE_COMP_NUM,
           BBCLAW_AUDIO_TX_RATE_COMP_DEN, BBCLAW_AUDIO_TX_EXPERIMENT, BBCLAW_AUDIO_I2S_DMA_DESC_NUM,
           BBCLAW_AUDIO_I2S_DMA_FRAME_NUM);
  return ESP_OK;

fail:
  if (new_tx_chan != NULL) {
    (void)i2s_del_channel(new_tx_chan);
  }
  if (new_rx_chan != NULL) {
    (void)i2s_del_channel(new_rx_chan);
  }
  s_tx_chan = NULL;
  s_rx_chan = NULL;
  s_i2s_prep_mode = BB_I2S_PREP_NONE;
  s_i2s_full_duplex = 0;
  s_tx_use_stereo32 = 0;
  s_tx_use_stereo16 = 0;
  s_tx_use_mono16 = 0;
  s_rx_use_32bit = 0;
  s_tx_effective_sample_rate = BBCLAW_AUDIO_SAMPLE_RATE;
  return ret;
}

static esp_err_t ensure_i2s_prepared(bool for_capture) {
  if (use_es8311_input_source()) {
    return ESP_OK;
  }
  bb_i2s_prep_mode_t want = for_capture ? BB_I2S_PREP_CAPTURE : BB_I2S_PREP_PLAYBACK;
  if (s_i2s_prep_mode == want) {
    return ESP_OK;
  }
  ESP_RETURN_ON_FALSE(s_tx_active == 0, ESP_ERR_INVALID_STATE, TAG, "cannot rebuild active i2s channels");
  ESP_RETURN_ON_ERROR(free_i2s_channels(), TAG, "free i2s channels");
  return init_i2s_channels(for_capture);
}

static esp_err_t init_pa_enable(void) {
#if BBCLAW_PA_EN_GPIO < 0
  if (!BBCLAW_PA_EN_PROBE_ON_BOOT) {
    return ESP_OK;
  }

  const int probe_pins[] = {
      BBCLAW_PA_EN_PROBE_GPIO1,
      BBCLAW_PA_EN_PROBE_GPIO2,
      BBCLAW_PA_EN_PROBE_GPIO3,
  };
  int enabled = 0;
  for (size_t i = 0; i < sizeof(probe_pins) / sizeof(probe_pins[0]); ++i) {
    int gpio_num = probe_pins[i];
    if (gpio_num < 0) {
      continue;
    }
    gpio_config_t cfg = {
        .pin_bit_mask = 1ULL << gpio_num,
        .mode = GPIO_MODE_OUTPUT,
        .pull_down_en = GPIO_PULLDOWN_DISABLE,
        .pull_up_en = GPIO_PULLUP_DISABLE,
        .intr_type = GPIO_INTR_DISABLE,
    };
    ESP_RETURN_ON_ERROR(gpio_config(&cfg), TAG, "pa probe gpio config failed gpio=%d", gpio_num);
    ESP_RETURN_ON_ERROR(gpio_set_level(gpio_num, BBCLAW_PA_EN_ACTIVE_LEVEL), TAG, "pa probe set failed gpio=%d",
                        gpio_num);
    enabled++;
    ESP_LOGW(TAG, "pa probe enabled gpio=%d level=%d", gpio_num, BBCLAW_PA_EN_ACTIVE_LEVEL);
  }
  if (enabled == 0) {
    ESP_LOGW(TAG, "pa probe enabled but no candidate gpio configured");
  } else {
    s_pa_ready = 1;
  }
  return ESP_OK;
#else
  gpio_config_t cfg = {
      .pin_bit_mask = 1ULL << BBCLAW_PA_EN_GPIO,
      .mode = GPIO_MODE_OUTPUT,
      .pull_down_en = GPIO_PULLDOWN_DISABLE,
      .pull_up_en = GPIO_PULLUP_DISABLE,
      .intr_type = GPIO_INTR_DISABLE,
  };
  ESP_RETURN_ON_ERROR(gpio_config(&cfg), TAG, "pa gpio config failed");
  ESP_RETURN_ON_ERROR(gpio_set_level(BBCLAW_PA_EN_GPIO, BBCLAW_PA_EN_ACTIVE_LEVEL), TAG, "pa gpio set failed");
  s_pa_ready = 1;
  ESP_LOGI(TAG, "pa en gpio=%d level=%d", BBCLAW_PA_EN_GPIO, BBCLAW_PA_EN_ACTIVE_LEVEL);
  return ESP_OK;
#endif
}

static esp_err_t init_speaker_sw(void) {
#if BBCLAW_SPEAKER_SW_GPIO >= 0
  gpio_config_t cfg = {
      .pin_bit_mask = 1ULL << BBCLAW_SPEAKER_SW_GPIO,
      .mode = GPIO_MODE_INPUT,
      .pull_up_en = GPIO_PULLUP_ENABLE,
      .pull_down_en = GPIO_PULLDOWN_DISABLE,
      .intr_type = GPIO_INTR_DISABLE,
  };
  ESP_RETURN_ON_ERROR(gpio_config(&cfg), TAG, "speaker sw gpio config failed");
  int level = gpio_get_level(BBCLAW_SPEAKER_SW_GPIO);
  s_speaker_sw_enabled = (level == BBCLAW_SPEAKER_SW_ACTIVE_LEVEL) ? 1 : 0;
  ESP_LOGI(TAG, "speaker sw gpio=%d level=%d enabled=%d", BBCLAW_SPEAKER_SW_GPIO, level, s_speaker_sw_enabled);
  return ESP_OK;
#else
  ESP_LOGI(TAG, "speaker sw disabled (gpio not configured)");
  s_speaker_sw_enabled = 1;
  return ESP_OK;
#endif
}

int bb_audio_get_speaker_sw_enabled(void) {
  return s_speaker_sw_enabled;
}

void bb_audio_poll_speaker_sw(void) {
#if BBCLAW_SPEAKER_SW_GPIO >= 0
  int level = gpio_get_level(BBCLAW_SPEAKER_SW_GPIO);
  int enabled = (level == BBCLAW_SPEAKER_SW_ACTIVE_LEVEL) ? 1 : 0;
  if (enabled != s_speaker_sw_enabled) {
    s_speaker_sw_enabled = enabled;
    ESP_LOGI(TAG, "speaker sw changed level=%d enabled=%d", level, enabled);
  }
#endif
}

static int16_t clamp_i16(int32_t v) {
  if (v > INT16_MAX) {
    return INT16_MAX;
  }
  if (v < INT16_MIN) {
    return INT16_MIN;
  }
  return (int16_t)v;
}

static void update_pcm_diag(const int16_t* samples, size_t count) {
  if (samples == NULL || count == 0U) {
    return;
  }
  for (size_t i = 0; i < count; ++i) {
    int16_t v = samples[i];
    s_diag.samples++;
    if (v == 0) {
      s_diag.zeros++;
    } else if (v > 0) {
      s_diag.positives++;
    } else {
      s_diag.negatives++;
    }
    if (v == INT16_MIN || v == INT16_MAX) {
      s_diag.clipped++;
    }
    if (v < s_diag.min_sample) {
      s_diag.min_sample = v;
    }
    if (v > s_diag.max_sample) {
      s_diag.max_sample = v;
    }
    s_diag.sum_squares += (uint64_t)((int32_t)v * (int32_t)v);
  }

  int64_t now_ms = esp_timer_get_time() / 1000;
  if (s_diag.last_log_ms == 0) {
    s_diag.last_log_ms = now_ms;
    return;
  }
  if (now_ms - s_diag.last_log_ms >= BBCLAW_AUDIO_DIAG_LOG_INTERVAL_MS) {
    uint64_t mean_square = s_diag.samples > 0 ? s_diag.sum_squares / s_diag.samples : 0;
    ESP_LOGI(TAG,
             "pcm diag samples=%llu zero=%llu pos=%llu neg=%llu clipped=%llu min=%d max=%d rms=%llu",
             (unsigned long long)s_diag.samples, (unsigned long long)s_diag.zeros,
             (unsigned long long)s_diag.positives, (unsigned long long)s_diag.negatives,
             (unsigned long long)s_diag.clipped, s_diag.min_sample, s_diag.max_sample,
             (unsigned long long)mean_square);
    s_diag.samples = 0;
    s_diag.zeros = 0;
    s_diag.positives = 0;
    s_diag.negatives = 0;
    s_diag.clipped = 0;
    s_diag.min_sample = INT16_MAX;
    s_diag.max_sample = INT16_MIN;
    s_diag.sum_squares = 0;
    s_diag.last_log_ms = now_ms;
  }
}

static void apply_soft_gain_inplace(int16_t* samples, size_t count, int gain_num, int gain_den) {
  if (samples == NULL || count == 0U || gain_num <= 0 || gain_den <= 0) {
    return;
  }
  if (gain_num == gain_den) {
    return;
  }
  for (size_t i = 0; i < count; ++i) {
    int32_t v = samples[i];
    v = (v * gain_num) / gain_den;
    if (v > INT16_MAX) {
      v = INT16_MAX;
    } else if (v < INT16_MIN) {
      v = INT16_MIN;
    }
    samples[i] = (int16_t)v;
  }
}

static void apply_inmp441_highpass_inplace(int16_t* samples, size_t count) {
#if !BBCLAW_AUDIO_INMP441_HPF_ENABLE
  (void)samples;
  (void)count;
#else
  if (samples == NULL || count == 0U) {
    return;
  }
  for (size_t i = 0; i < count; ++i) {
    int32_t x = samples[i];
    int32_t y = x - s_inmp441_hpf_prev_x +
                (int32_t)(((int64_t)BBCLAW_AUDIO_INMP441_HPF_ALPHA_Q15 * (int64_t)s_inmp441_hpf_prev_y) >> 15);
    s_inmp441_hpf_prev_x = x;
    s_inmp441_hpf_prev_y = y;
    samples[i] = clamp_i16(y);
  }
#endif
}

static void log_raw_i2s_diag_pairs(const int32_t* raw_samples, size_t raw_count) {
  if (raw_samples == NULL || raw_count < 2U || s_rx_raw_diag_pending == 0) {
    return;
  }
  size_t pair_count = raw_count / 2U;
  if (pair_count == 0U) {
    return;
  }
  size_t to_log = pair_count;
  if (to_log > 8U) {
    to_log = 8U;
  }
  ESP_LOGI(TAG,
           "i2s raw diag shift_bits=%d stereo_capture=%d mono_pick_right=%d auto_channel_lock=%d pairs=%u",
           BBCLAW_AUDIO_RX_SHIFT_BITS, BBCLAW_AUDIO_RX_STEREO_CAPTURE, BBCLAW_AUDIO_RX_MONO_PICK_RIGHT,
           BBCLAW_AUDIO_RX_AUTO_CHANNEL_LOCK, (unsigned)pair_count);
  for (size_t i = 0; i < to_log; ++i) {
    int32_t left = raw_samples[i * 2];
    int32_t right = raw_samples[i * 2 + 1];
    ESP_LOGI(TAG, "i2s raw pair[%u] left=%ld right=%ld left>>shift=%ld right>>shift=%ld", (unsigned)i, (long)left,
             (long)right, (long)(left >> BBCLAW_AUDIO_RX_SHIFT_BITS), (long)(right >> BBCLAW_AUDIO_RX_SHIFT_BITS));
  }
  s_rx_raw_diag_pending = 0;
}

esp_err_t bb_audio_init(void) {
  s_tx_active = 0;
  s_audio_ready = 0;
  s_playback_interrupt_requested = 0;
  s_pa_ready = 0;
  s_rx_enabled = 0;
  if (use_es8311_input_source()) {
    ESP_LOGI(TAG,
             "audio init start: source=es8311 i2c(sda=%d scl=%d addr=0x%02X), i2s(mck=%d bck=%d ws=%d do=%d di=%d)",
             BBCLAW_ES8311_I2C_SDA_GPIO, BBCLAW_ES8311_I2C_SCL_GPIO, BBCLAW_ES8311_I2C_ADDR,
             BBCLAW_AUDIO_I2S_MCK_GPIO, BBCLAW_AUDIO_I2S_BCK_GPIO, BBCLAW_AUDIO_I2S_WS_GPIO,
             BBCLAW_AUDIO_I2S_DO_GPIO, BBCLAW_AUDIO_I2S_DI_GPIO);
    ESP_RETURN_ON_ERROR(i2c_new_master_bus(&(i2c_master_bus_config_t){
                                                 .i2c_port = BBCLAW_ES8311_I2C_PORT,
                                                 .sda_io_num = BBCLAW_ES8311_I2C_SDA_GPIO,
                                                 .scl_io_num = BBCLAW_ES8311_I2C_SCL_GPIO,
                                                 .clk_source = I2C_CLK_SRC_DEFAULT,
                                                 .glitch_ignore_cnt = 7,
                                                 .flags.enable_internal_pullup = true,
                                             },
                                             &s_i2c_bus),
                        TAG, "new i2c master bus");
    ESP_RETURN_ON_ERROR(detect_codec_address(), TAG, "codec i2c not found (check wiring + power + addr)");
    ESP_RETURN_ON_ERROR(init_i2c_and_codec(), TAG, "codec init failed");
    es8311_log_reg_summary();
  } else {
    ESP_LOGI(TAG, "audio init start: source=inmp441 i2s(bck=%d ws=%d do=%d di=%d)", BBCLAW_AUDIO_I2S_BCK_GPIO,
             BBCLAW_AUDIO_I2S_WS_GPIO, BBCLAW_AUDIO_I2S_DO_GPIO, BBCLAW_AUDIO_I2S_DI_GPIO);
  }

  ESP_RETURN_ON_ERROR(init_i2s_channels(false), TAG, "i2s init failed");
  ESP_RETURN_ON_ERROR(init_pa_enable(), TAG, "pa enable init failed");
  ESP_RETURN_ON_ERROR(init_speaker_sw(), TAG, "speaker sw init failed");

  s_audio_ready = 1;
  ESP_LOGI(TAG, "audio init done");
  return ESP_OK;
}

esp_err_t bb_audio_start_tx(void) {
  if (!s_audio_ready) {
    return ESP_ERR_INVALID_STATE;
  }
  if (s_tx_active == 0) {
    ESP_RETURN_ON_ERROR(ensure_i2s_prepared(true), TAG, "prepare capture i2s failed");
    if (s_pa_ready && BBCLAW_PA_EN_GPIO >= 0) {
      (void)gpio_set_level(BBCLAW_PA_EN_GPIO, BBCLAW_PA_EN_ACTIVE_LEVEL);
    }
    s_rx_timeout_count = 0;
    s_rx_frame_count = 0;
    s_rx_total_bytes = 0;
    s_rx_warmup_remaining = BBCLAW_AUDIO_RX_WARMUP_SAMPLES;
    s_rx_pick_right_locked = -1;
    s_rx_raw_diag_pending = !use_es8311_input_source();
    s_inmp441_hpf_prev_x = 0;
    s_inmp441_hpf_prev_y = 0;
    s_diag.samples = 0;
    s_diag.zeros = 0;
    s_diag.positives = 0;
    s_diag.negatives = 0;
    s_diag.clipped = 0;
    s_diag.min_sample = INT16_MAX;
    s_diag.max_sample = INT16_MIN;
    s_diag.sum_squares = 0;
    s_diag.last_log_ms = 0;
    ESP_LOGI(TAG, "rx warmup_samples=%u mono_pick_right=%d auto_channel_lock=%d input=%s",
             (unsigned)BBCLAW_AUDIO_RX_WARMUP_SAMPLES, BBCLAW_AUDIO_RX_MONO_PICK_RIGHT,
             BBCLAW_AUDIO_RX_AUTO_CHANNEL_LOCK, BBCLAW_AUDIO_INPUT_SOURCE);
    if (!use_es8311_input_source()) {
      ESP_LOGI(TAG, "inmp441 soft_gain=%d/%d", BBCLAW_AUDIO_INMP441_GAIN_NUM, BBCLAW_AUDIO_INMP441_GAIN_DEN);
    }
    if (s_i2s_full_duplex) {
      ESP_RETURN_ON_ERROR(i2s_channel_enable(s_tx_chan), TAG, "enable tx channel");
    }
    ESP_RETURN_ON_ERROR(i2s_channel_enable(s_rx_chan), TAG, "enable rx channel");
    s_tx_active = 1;
    s_rx_enabled = 1;
    ESP_LOGI(TAG, "capture start");
  } else if (!s_rx_enabled) {
    ESP_LOGW(TAG, "capture start requested while playback active");
    return ESP_ERR_INVALID_STATE;
  }
  return ESP_OK;
}

esp_err_t bb_audio_stop_tx(void) {
  if (!s_audio_ready) {
    return ESP_ERR_INVALID_STATE;
  }
  if (s_tx_active != 0) {
    if (s_rx_enabled) {
      if (s_i2s_full_duplex) {
        ESP_RETURN_ON_ERROR(i2s_channel_disable(s_tx_chan), TAG, "disable tx channel");
      }
      ESP_RETURN_ON_ERROR(i2s_channel_disable(s_rx_chan), TAG, "disable rx channel");
    } else {
      ESP_RETURN_ON_ERROR(i2s_channel_disable(s_tx_chan), TAG, "disable tx channel");
    }
    s_tx_active = 0;
    if (s_rx_enabled) {
      ESP_LOGI(TAG, "capture summary frames=%u bytes=%u timeouts=%u", s_rx_frame_count, (unsigned)s_rx_total_bytes,
               s_rx_timeout_count);
      ESP_LOGI(TAG, "capture stop");
    } else {
      ESP_LOGI(TAG, "playback stop");
    }
    s_rx_enabled = 0;
  }
  return ESP_OK;
}

esp_err_t bb_audio_start_playback(void) {
  if (!s_audio_ready) {
    return ESP_ERR_INVALID_STATE;
  }
  if (s_tx_active == 0) {
    ESP_RETURN_ON_ERROR(ensure_i2s_prepared(false), TAG, "prepare playback i2s failed");
    if (s_pa_ready && BBCLAW_PA_EN_GPIO >= 0) {
      (void)gpio_set_level(BBCLAW_PA_EN_GPIO, BBCLAW_PA_EN_ACTIVE_LEVEL);
    }
    ESP_RETURN_ON_FALSE(s_tx_chan != NULL, ESP_ERR_INVALID_STATE, TAG, "tx channel not prepared");
    ESP_RETURN_ON_ERROR(i2s_channel_enable(s_tx_chan), TAG, "enable tx channel");
    s_tx_active = 1;
    s_rx_enabled = 0;
    s_playback_interrupt_requested = 0;
    ESP_LOGI(TAG, "playback start tx_cfg=%s tx_stereo32=%d tx_stereo16=%d tx_mono16=%d volume=%d%%", tx_config_name(),
             s_tx_use_stereo32, s_tx_use_stereo16, s_tx_use_mono16, s_volume_pct);
  } else if (s_rx_enabled) {
    ESP_LOGW(TAG, "playback start requested while capture active");
    return ESP_ERR_INVALID_STATE;
  }
  return ESP_OK;
}

esp_err_t bb_audio_stop_playback(void) {
  if (s_rx_enabled) {
    return ESP_ERR_INVALID_STATE;
  }
  s_playback_interrupt_requested = 0;
  return bb_audio_stop_tx();
}

void bb_audio_request_playback_interrupt(void) {
  s_playback_interrupt_requested = 1;
}

void bb_audio_clear_playback_interrupt(void) {
  s_playback_interrupt_requested = 0;
}

int bb_audio_is_playback_interrupt_requested(void) {
  return s_playback_interrupt_requested ? 1 : 0;
}

esp_err_t bb_audio_set_playback_sample_rate(int sample_rate) {
  if (!s_audio_ready || sample_rate <= 0) {
    return ESP_ERR_INVALID_ARG;
  }
  ESP_RETURN_ON_FALSE(s_tx_chan != NULL, ESP_ERR_INVALID_STATE, TAG, "tx channel unavailable");
  uint32_t effective_rate = effective_tx_sample_rate((uint32_t)sample_rate);
  /* Must disable channel before reconfiguring clock. */
  bool was_active = s_tx_active != 0;
  if (was_active) {
    i2s_channel_disable(s_tx_chan);
  }
  i2s_std_clk_config_t clk = I2S_STD_CLK_DEFAULT_CONFIG(effective_rate);
  clk.mclk_multiple = I2S_MCLK_MULTIPLE_256;
  esp_err_t err = i2s_channel_reconfig_std_clock(s_tx_chan, &clk);
  if (was_active) {
    i2s_channel_enable(s_tx_chan);
  }
  if (err == ESP_OK) {
    s_tx_effective_sample_rate = (int)effective_rate;
    ESP_LOGI(TAG, "playback sample rate req=%d effective=%d tx_cfg=%s stereo32=%d stereo16=%d mono16=%d", sample_rate,
             s_tx_effective_sample_rate, tx_config_name(), s_tx_use_stereo32, s_tx_use_stereo16, s_tx_use_mono16);
  }
  return err;
}

esp_err_t bb_audio_read_pcm_frame(uint8_t* out_buf, size_t out_buf_len, size_t* out_read) {
  if (!s_audio_ready) {
    return ESP_ERR_INVALID_STATE;
  }
  if (out_buf == NULL || out_read == NULL || out_buf_len == 0U) {
    return ESP_ERR_INVALID_ARG;
  }
  size_t bytes_read = 0;
  esp_err_t err;
  if (s_rx_use_32bit) {
    size_t raw_cap = out_buf_len * 2;
    if (raw_cap > sizeof(s_rx_raw_buf)) {
      raw_cap = sizeof(s_rx_raw_buf);
    }
    err = i2s_channel_read(s_rx_chan, s_rx_raw_buf, raw_cap, &bytes_read, BBCLAW_AUDIO_IO_TIMEOUT_MS);
    if (err == ESP_ERR_TIMEOUT) {
      s_rx_timeout_count++;
      *out_read = 0;
      return ESP_OK;
    }
    if (err != ESP_OK) {
      return err;
    }
    const int32_t* raw_samples = (const int32_t*)s_rx_raw_buf;
    size_t raw_count = bytes_read / sizeof(int32_t);
    int16_t* mono_out = (int16_t*)out_buf;
    size_t mono_cap = out_buf_len / sizeof(int16_t);
    /* STEREO 32-bit RX: each frame = [L, R] int32 pair.
     * Pick the channel with more energy (auto-detect INMP441 L/R wiring). */
    size_t pair_count = raw_count / 2;
    if (pair_count > mono_cap) {
      pair_count = mono_cap;
    }
    int pick_right = BBCLAW_AUDIO_RX_MONO_PICK_RIGHT ? 1 : 0;
    if (BBCLAW_AUDIO_RX_AUTO_CHANNEL_LOCK && s_rx_pick_right_locked < 0) {
      int64_t energy_l = 0, energy_r = 0;
      for (size_t i = 0; i < pair_count; ++i) {
        int32_t l = raw_samples[i * 2] >> 8;
        int32_t r = raw_samples[i * 2 + 1] >> 8;
        energy_l += (int64_t)l * l;
        energy_r += (int64_t)r * r;
      }
      pick_right = energy_r > energy_l ? 1 : 0;
      s_rx_pick_right_locked = pick_right;
      ESP_LOGI(TAG, "inmp441 auto channel lock pick_right=%d energy_l=%lld energy_r=%lld",
               pick_right, (long long)energy_l, (long long)energy_r);
    }
    if (BBCLAW_AUDIO_RX_AUTO_CHANNEL_LOCK && s_rx_pick_right_locked >= 0) {
      pick_right = s_rx_pick_right_locked;
    }
    for (size_t i = 0; i < pair_count; ++i) {
      mono_out[i] = clamp_i16(raw_samples[i * 2 + pick_right] >> BBCLAW_AUDIO_RX_SHIFT_BITS);
    }
    if (s_rx_warmup_remaining > 0U) {
      size_t n = pair_count;
      if (n > s_rx_warmup_remaining) {
        n = s_rx_warmup_remaining;
      }
      memset(mono_out, 0, n * sizeof(int16_t));
      s_rx_warmup_remaining -= n;
    }
    if (!use_es8311_input_source()) {
      apply_inmp441_highpass_inplace(mono_out, pair_count);
      apply_soft_gain_inplace(mono_out, pair_count, BBCLAW_AUDIO_INMP441_GAIN_NUM, BBCLAW_AUDIO_INMP441_GAIN_DEN);
    }
    if (pair_count > 0U) {
      s_rx_frame_count++;
      s_rx_total_bytes += pair_count * sizeof(int16_t);
    }
    update_pcm_diag(mono_out, pair_count);
    *out_read = pair_count * sizeof(int16_t);
    return ESP_OK;
  }

  err = i2s_channel_read(s_rx_chan, out_buf, out_buf_len, &bytes_read, BBCLAW_AUDIO_IO_TIMEOUT_MS);
  if (err == ESP_ERR_TIMEOUT) {
    s_rx_timeout_count++;
    *out_read = 0;
    return ESP_OK;
  }
  if (err != ESP_OK) {
    return err;
  }
  if (bytes_read > 0U) {
    s_rx_frame_count++;
    s_rx_total_bytes += bytes_read;
  }
#if BBCLAW_AUDIO_RX_STEREO_CAPTURE
  const int16_t* in_samples = (const int16_t*)out_buf;
  size_t in_count = bytes_read / 2;
  size_t pair_count = in_count / 2;
  int pick_right = BBCLAW_AUDIO_RX_MONO_PICK_RIGHT ? 1 : 0;
  if (use_es8311_input_source() || (BBCLAW_AUDIO_RX_AUTO_CHANNEL_LOCK && s_rx_pick_right_locked < 0)) {
    int64_t energy_l = 0;
    int64_t energy_r = 0;
    for (size_t i = 0; i < pair_count; ++i) {
      int16_t l = in_samples[i * 2];
      int16_t r = in_samples[i * 2 + 1];
      energy_l += (int64_t)l * (int64_t)l;
      energy_r += (int64_t)r * (int64_t)r;
    }
    pick_right = energy_r > energy_l ? 1 : 0;
    if (!use_es8311_input_source() && BBCLAW_AUDIO_RX_AUTO_CHANNEL_LOCK && s_rx_pick_right_locked < 0) {
      s_rx_pick_right_locked = pick_right;
      ESP_LOGI(TAG, "inmp441 auto channel lock pick_right=%d energy_l=%lld energy_r=%lld", pick_right,
               (long long)energy_l, (long long)energy_r);
    } else if (use_es8311_input_source()) {
      ESP_LOGI(TAG, "es8311 stereo energy pick_right=%d energy_l=%lld energy_r=%lld", pick_right,
               (long long)energy_l, (long long)energy_r);
    }
  }
  if (!use_es8311_input_source() && BBCLAW_AUDIO_RX_AUTO_CHANNEL_LOCK && s_rx_pick_right_locked >= 0) {
    pick_right = s_rx_pick_right_locked;
  }
  int16_t* mono_out = (int16_t*)out_buf;
  for (size_t i = 0; i < pair_count; ++i) {
    mono_out[i] = in_samples[i * 2 + (pick_right ? 1 : 0)];
  }
  if (s_rx_warmup_remaining > 0U) {
    size_t n = pair_count;
    if (n > s_rx_warmup_remaining) {
      n = s_rx_warmup_remaining;
    }
    memset(mono_out, 0, n * sizeof(int16_t));
    s_rx_warmup_remaining -= n;
  }
  if (!use_es8311_input_source()) {
    apply_inmp441_highpass_inplace(mono_out, pair_count);
    apply_soft_gain_inplace(mono_out, pair_count, BBCLAW_AUDIO_INMP441_GAIN_NUM, BBCLAW_AUDIO_INMP441_GAIN_DEN);
  }
  update_pcm_diag(mono_out, pair_count);
  *out_read = pair_count * 2;
#else
  if (s_rx_warmup_remaining > 0U) {
    size_t sample_count = bytes_read / sizeof(int16_t);
    size_t n = sample_count;
    if (n > s_rx_warmup_remaining) {
      n = s_rx_warmup_remaining;
    }
    memset(out_buf, 0, n * sizeof(int16_t));
    s_rx_warmup_remaining -= n;
  }
  if (!use_es8311_input_source()) {
    apply_inmp441_highpass_inplace((int16_t*)out_buf, bytes_read / sizeof(int16_t));
    apply_soft_gain_inplace((int16_t*)out_buf, bytes_read / sizeof(int16_t), BBCLAW_AUDIO_INMP441_GAIN_NUM,
                            BBCLAW_AUDIO_INMP441_GAIN_DEN);
  }
  update_pcm_diag((const int16_t*)out_buf, bytes_read / 2);
  *out_read = bytes_read;
#endif
  return ESP_OK;
}

esp_err_t bb_audio_play_pcm_blocking(const uint8_t* pcm, size_t pcm_len) {
  if (!s_audio_ready) {
    return ESP_ERR_INVALID_STATE;
  }
  if (pcm == NULL || pcm_len == 0U) {
    return ESP_ERR_INVALID_ARG;
  }
  if (!s_speaker_enabled) {
    /* Speaker disabled; silently skip playback but return OK to avoid caller errors. */
    return ESP_OK;
  }
  /* STEREO 32-bit path for INMP441+MAX98357A.
   * TX is initialized as STEREO 32-bit (slot_mode=STEREO, slot_bit_width=32).
   * App-layer mono→stereo: convert mono PCM to [v,v] stereo pairs (same value L=R).
   * Each stereo frame = 2 × int32 = 64 bits → frame rate = BCK/64 = 16kHz ✓.
   * MAX98357A uses LEFT channel only (DOUT), so [v,v] is equivalent to [v].
   * NO runtime slot switching — original working approach. */
  if (s_tx_use_stereo32) {
    if ((pcm_len % sizeof(int16_t)) != 0U) {
      return ESP_ERR_INVALID_ARG;
    }
    const int16_t* src = (const int16_t*)pcm;
    size_t sample_count = pcm_len / sizeof(int16_t);
    size_t sample_idx = 0;
    int32_t frame_buf[256 * 2];
    int64_t t0 = esp_timer_get_time();
    size_t i2s_bytes_total = 0;
    int timeout_count = 0;
    while (sample_idx < sample_count) {
      if (s_playback_interrupt_requested) {
        ESP_LOGI(TAG, "play_pcm interrupted stereo32 sample_idx=%u/%u", (unsigned)sample_idx, (unsigned)sample_count);
        return ESP_ERR_INVALID_STATE;
      }
      size_t n = sample_count - sample_idx;
      if (n > 256U) {
        n = 256U;
      }
      for (size_t i = 0; i < n; ++i) {
        int32_t v = ((int32_t)src[sample_idx + i] * s_volume_pct / 100) << 16;
        frame_buf[i * 2] = v;
        frame_buf[i * 2 + 1] = v;
      }
      const uint8_t* out = (const uint8_t*)frame_buf;
      size_t out_len = n * 2U * sizeof(int32_t);
      size_t written_total = 0;
      while (written_total < out_len) {
        if (s_playback_interrupt_requested) {
          ESP_LOGI(TAG, "play_pcm interrupted stereo32 write=%u/%u", (unsigned)written_total, (unsigned)out_len);
          return ESP_ERR_INVALID_STATE;
        }
        size_t written = 0;
        esp_err_t err = i2s_channel_write(s_tx_chan, out + written_total, out_len - written_total, &written,
                                          BBCLAW_AUDIO_IO_TIMEOUT_MS);
        if (err == ESP_ERR_TIMEOUT) {
          timeout_count++;
          continue;
        }
        if (err != ESP_OK) {
          return err;
        }
        written_total += written;
      }
      i2s_bytes_total += out_len;
      sample_idx += n;
    }
    int64_t elapsed_us = esp_timer_get_time() - t0;
    ESP_LOGI(TAG, "play_pcm stereo32 samples=%u i2s_bytes=%u elapsed_ms=%lld timeouts=%d",
             (unsigned)sample_count, (unsigned)i2s_bytes_total, (long long)(elapsed_us / 1000), timeout_count);
    return ESP_OK;
  }
  if ((pcm_len % sizeof(int16_t)) != 0U) {
    return ESP_ERR_INVALID_ARG;
  }
  if (s_tx_use_stereo16) {
    const int16_t* src = (const int16_t*)pcm;
    size_t sample_count = pcm_len / sizeof(int16_t);
    size_t sample_idx = 0;
    int16_t frame_buf[256 * 2];
    int64_t t0 = esp_timer_get_time();
    size_t i2s_bytes_total = 0;
    int timeout_count = 0;
    while (sample_idx < sample_count) {
      if (s_playback_interrupt_requested) {
        ESP_LOGI(TAG, "play_pcm interrupted stereo16 sample_idx=%u/%u", (unsigned)sample_idx, (unsigned)sample_count);
        return ESP_ERR_INVALID_STATE;
      }
      size_t n = sample_count - sample_idx;
      if (n > 256U) {
        n = 256U;
      }
      for (size_t i = 0; i < n; ++i) {
        int32_t v = ((int32_t)src[sample_idx + i] * s_volume_pct) / 100;
        int16_t scaled = clamp_i16(v);
        frame_buf[i * 2] = scaled;
        frame_buf[i * 2 + 1] = scaled;
      }
      const uint8_t* out = (const uint8_t*)frame_buf;
      size_t out_len = n * 2U * sizeof(int16_t);
      size_t written_total = 0;
      while (written_total < out_len) {
        if (s_playback_interrupt_requested) {
          ESP_LOGI(TAG, "play_pcm interrupted stereo16 write=%u/%u", (unsigned)written_total, (unsigned)out_len);
          return ESP_ERR_INVALID_STATE;
        }
        size_t written = 0;
        esp_err_t err = i2s_channel_write(s_tx_chan, out + written_total, out_len - written_total, &written,
                                          BBCLAW_AUDIO_IO_TIMEOUT_MS);
        if (err == ESP_ERR_TIMEOUT) {
          timeout_count++;
          continue;
        }
        if (err != ESP_OK) {
          return err;
        }
        written_total += written;
      }
      i2s_bytes_total += out_len;
      sample_idx += n;
    }
    int64_t elapsed_us = esp_timer_get_time() - t0;
    ESP_LOGI(TAG, "play_pcm stereo16 samples=%u i2s_bytes=%u elapsed_ms=%lld timeouts=%d",
             (unsigned)sample_count, (unsigned)i2s_bytes_total, (long long)(elapsed_us / 1000), timeout_count);
    return ESP_OK;
  }
  if (s_tx_use_mono16) {
    const int16_t* src = (const int16_t*)pcm;
    size_t sample_count = pcm_len / sizeof(int16_t);
    size_t sample_idx = 0;
    int16_t frame_buf[256];
    int64_t t0 = esp_timer_get_time();
    size_t i2s_bytes_total = 0;
    int timeout_count = 0;
    while (sample_idx < sample_count) {
      if (s_playback_interrupt_requested) {
        ESP_LOGI(TAG, "play_pcm interrupted mono16 sample_idx=%u/%u", (unsigned)sample_idx, (unsigned)sample_count);
        return ESP_ERR_INVALID_STATE;
      }
      size_t n = sample_count - sample_idx;
      if (n > 256U) {
        n = 256U;
      }
      for (size_t i = 0; i < n; ++i) {
        int32_t v = ((int32_t)src[sample_idx + i] * s_volume_pct) / 100;
        frame_buf[i] = clamp_i16(v);
      }
      const uint8_t* out = (const uint8_t*)frame_buf;
      size_t out_len = n * sizeof(int16_t);
      size_t written_total = 0;
      while (written_total < out_len) {
        if (s_playback_interrupt_requested) {
          ESP_LOGI(TAG, "play_pcm interrupted mono16 write=%u/%u", (unsigned)written_total, (unsigned)out_len);
          return ESP_ERR_INVALID_STATE;
        }
        size_t written = 0;
        esp_err_t err = i2s_channel_write(s_tx_chan, out + written_total, out_len - written_total, &written,
                                          BBCLAW_AUDIO_IO_TIMEOUT_MS);
        if (err == ESP_ERR_TIMEOUT) {
          timeout_count++;
          continue;
        }
        if (err != ESP_OK) {
          return err;
        }
        written_total += written;
      }
      i2s_bytes_total += out_len;
      sample_idx += n;
    }
    int64_t elapsed_us = esp_timer_get_time() - t0;
    ESP_LOGI(TAG, "play_pcm mono16 samples=%u i2s_bytes=%u elapsed_ms=%lld timeouts=%d",
             (unsigned)sample_count, (unsigned)i2s_bytes_total, (long long)(elapsed_us / 1000), timeout_count);
    return ESP_OK;
  }
  if (s_volume_pct != 100) {
    const int16_t* src = (const int16_t*)pcm;
    size_t sample_count = pcm_len / sizeof(int16_t);
    size_t sample_idx = 0;
    int16_t frame_buf[256];
    while (sample_idx < sample_count) {
      if (s_playback_interrupt_requested) {
        ESP_LOGI(TAG, "play_pcm interrupted scaled sample_idx=%u/%u", (unsigned)sample_idx, (unsigned)sample_count);
        return ESP_ERR_INVALID_STATE;
      }
      size_t n = sample_count - sample_idx;
      if (n > 256U) {
        n = 256U;
      }
      for (size_t i = 0; i < n; ++i) {
        int32_t v = ((int32_t)src[sample_idx + i] * s_volume_pct) / 100;
        frame_buf[i] = clamp_i16(v);
      }
      const uint8_t* out = (const uint8_t*)frame_buf;
      size_t out_len = n * sizeof(int16_t);
      size_t written_total = 0;
      while (written_total < out_len) {
        if (s_playback_interrupt_requested) {
          ESP_LOGI(TAG, "play_pcm interrupted scaled write=%u/%u", (unsigned)written_total, (unsigned)out_len);
          return ESP_ERR_INVALID_STATE;
        }
        size_t written = 0;
        esp_err_t err = i2s_channel_write(s_tx_chan, out + written_total, out_len - written_total, &written,
                                          BBCLAW_AUDIO_IO_TIMEOUT_MS);
        if (err == ESP_ERR_TIMEOUT) {
          continue;
        }
        if (err != ESP_OK) {
          return err;
        }
        written_total += written;
      }
      sample_idx += n;
    }
    return ESP_OK;
  }
  size_t written_total = 0;
  while (written_total < pcm_len) {
    if (s_playback_interrupt_requested) {
      ESP_LOGI(TAG, "play_pcm interrupted raw write=%u/%u", (unsigned)written_total, (unsigned)pcm_len);
      return ESP_ERR_INVALID_STATE;
    }
    size_t written = 0;
    esp_err_t err =
        i2s_channel_write(s_tx_chan, pcm + written_total, pcm_len - written_total, &written, BBCLAW_AUDIO_IO_TIMEOUT_MS);
    if (err == ESP_ERR_TIMEOUT) {
      continue;
    }
    if (err != ESP_OK) {
      return err;
    }
    written_total += written;
  }
  return ESP_OK;
}

esp_err_t bb_audio_play_test_tone(uint32_t freq_hz, uint32_t duration_ms, int16_t amplitude) {
  if (!s_audio_ready) {
    return ESP_ERR_INVALID_STATE;
  }
  if (freq_hz == 0U || duration_ms == 0U) {
    return ESP_ERR_INVALID_ARG;
  }
  if (amplitude < 0) {
    amplitude = (int16_t)(-amplitude);
  }
  if (amplitude > 20000) {
    amplitude = 20000;
  }

  const uint32_t total_samples = (uint32_t)((BBCLAW_AUDIO_SAMPLE_RATE * duration_ms) / 1000U);
  uint32_t period_samples = BBCLAW_AUDIO_SAMPLE_RATE / freq_hz;
  if (period_samples < 2U) {
    period_samples = 2U;
  }
  int16_t frame[256];
  uint32_t generated = 0;
  while (generated < total_samples) {
    uint32_t n = total_samples - generated;
    if (n > (uint32_t)(sizeof(frame) / sizeof(frame[0]))) {
      n = (uint32_t)(sizeof(frame) / sizeof(frame[0]));
    }
    for (uint32_t i = 0; i < n; ++i) {
      uint32_t phase = (generated + i) % period_samples;
      frame[i] = (phase < (period_samples / 2U)) ? amplitude : (int16_t)(-amplitude);
    }
    ESP_RETURN_ON_ERROR(bb_audio_play_pcm_blocking((const uint8_t*)frame, n * sizeof(int16_t)), TAG, "play tone failed");
    generated += n;
  }
  return ESP_OK;
}


void bb_audio_set_volume_pct(int pct) {
  if (pct < 0) { pct = 0; }
  if (pct > 100) { pct = 100; }
  s_volume_pct = pct;
  ESP_LOGI(TAG, "volume set to %d%%", pct);
}

void bb_audio_set_speaker_enabled(int enabled) {
  s_speaker_enabled = enabled ? 1 : 0;
  ESP_LOGI(TAG, "speaker %s", s_speaker_enabled ? "enabled" : "disabled");
}
