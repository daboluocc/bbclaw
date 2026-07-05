/* Device Monitor — see ADR-015 and bb_device_monitor.h.
 *
 * Transport: TinyUSB dual CDC over the chip's native USB pins (GPIO 19/20),
 * which the production BBClaw PCB exposes via the on-board CH334F USB hub.
 * Two CDC interfaces enumerate on the host:
 *
 *   CDC0  console (stdio redirect) — replaces the previously-secondary USJ
 *                                     log channel; host monitors this to
 *                                     see ESP_LOG output without UART cable
 *   CDC1  binary frame protocol    — REQ_ECHO / REQ_SCREENSHOT / REQ_INPUT
 *
 * Frame layout (all multi-byte fields little-endian):
 *
 *     +--------+------+------+--------+-----+----------+-----+
 *     | magic  | kind | flags| len    | seq | payload  | crc |
 *     | 2 B    | 1 B  | 1 B  | 4 B    | 2 B | len B    | 2 B |
 *     +--------+------+------+--------+-----+----------+-----+
 *     | 0xBBC1 |      |      | u32 LE | u16 |          | u16 |
 *     +--------+------+------+--------+-----+----------+-----+
 *
 * Kind codes:
 *
 *   0x01  REQ_ECHO         payload echoed back as RES_ECHO
 *   0x02  RES_ECHO
 *   0x03  REQ_SCREENSHOT   payload empty
 *   0x04  RES_SCREENSHOT   payload: u16 w, u16 h, RGB565 LE pixels (w*h*2)
 *   0x05  REQ_INPUT        payload: u8 event_id (Phase 5)
 *   0x06  RES_INPUT_ACK    payload: u8 status
 *   0xFF  ERR              payload: u8 code (device_monitor_err_t)
 *
 * History: an earlier revision used UART1 on GPIO 38/39 with an external
 * USB-UART module because the previous BBClaw breadboard didn't route the
 * chip's native USB pins. The CH334F-based PCB makes that workaround
 * unnecessary — see ADR-015 for the full transport history.
 */

#include "sdkconfig.h"

#ifdef CONFIG_BBCLAW_DEVICE_MONITOR

#include "bb_config.h"
#include "bb_device_monitor.h"

#include "bb_recorder.h"
#include "bb_nav_input.h"

#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>

#include "esp_err.h"
#include "esp_heap_caps.h"
#include "esp_log.h"
#include "esp_lvgl_port.h"
#include "esp_rom_sys.h"
#include "esp_system.h"
#include "soc/rtc_cntl_reg.h"
#include "soc/soc.h"
#if CONFIG_IDF_TARGET_ESP32S3
#include "esp32s3/rom/usb/chip_usb_dw_wrapper.h"
#include "esp32s3/rom/usb/usb_dc.h"
#include "esp32s3/rom/usb/usb_persist.h"
#endif
#include "freertos/FreeRTOS.h"
#include "freertos/queue.h"
#include "freertos/task.h"
#include "lvgl.h"
#include "src/draw/snapshot/lv_snapshot.h"
#include "tinyusb.h"
#include "tusb_cdc_acm.h"

static const char* TAG = "bb_devmon";

#define BB_DEVMON_MAGIC0 0xBB
#define BB_DEVMON_MAGIC1 0xC1

/* Inbound frames are always small (REQ_ECHO test bytes, REQ_INPUT ~1 byte).
 * 256 covers any plausible request; larger inbound frames are rejected. */
#define BB_DEVMON_MAX_RX_PAYLOAD 256

/* CDC ports — CDC0 is the console output, CDC1 is the binary protocol. */
#define BB_DEVMON_CDC_CONSOLE  TINYUSB_CDC_ACM_0
#define BB_DEVMON_CDC_PROTOCOL TINYUSB_CDC_ACM_1

/* Per-write chunk fed to TinyUSB. tinyusb_cdcacm_write_queue copies into the
 * driver TX buffer; if the buffer is full it returns less than requested,
 * so callers must loop with flush() until everything is drained. 256 keeps
 * latency low without thrashing the driver's internal locks. */
#define BB_DEVMON_TX_CHUNK 256

typedef enum {
  KIND_REQ_ECHO                 = 0x01,
  KIND_RES_ECHO                 = 0x02,
  KIND_REQ_SCREENSHOT           = 0x03,
  KIND_RES_SCREENSHOT           = 0x04,
  KIND_REQ_INPUT                = 0x05,
  KIND_RES_INPUT_ACK            = 0x06,
  KIND_REQ_REBOOT_TO_BOOTLOADER = 0x07,
  KIND_RES_REBOOT_ACK           = 0x08,
  KIND_ERR                      = 0xFF,
} device_monitor_kind_t;

typedef enum {
  ERR_NONE             = 0,
  ERR_NOT_IMPL         = 1,
  ERR_UNKNOWN_KIND     = 2,
  ERR_BAD_CRC          = 3,
  ERR_PAYLOAD_LIMIT    = 4,
  ERR_LVGL_LOCK        = 5,
  ERR_SNAPSHOT_FAILED  = 6,
  ERR_TX_FAILED        = 7,
} device_monitor_err_t;

/* ---------------- CDC0 console redirect (tee from ESP_LOG) ---------------- */

/* The official esp_tusb_init_console hook didn't reliably produce output on
 * this board (host saw zero bytes), so we install a custom vprintf hook
 * that formats the log line and writes it directly to CDC0's TX buffer. The
 * default vprintf path (going to UART0) is preserved so the CH340 channel
 * still sees logs if it's enumerated. */

static vprintf_like_t s_prior_vprintf;

static int cdc0_tee_vprintf(const char* fmt, va_list args) {
  /* Format into a stack buffer. Log lines >256 chars get truncated — fine
   * for debug output. */
  char buf[256];
  va_list args_copy;
  va_copy(args_copy, args);
  int len = vsnprintf(buf, sizeof(buf), fmt, args_copy);
  va_end(args_copy);

  if (len > 0) {
    if (len >= (int)sizeof(buf)) len = (int)sizeof(buf) - 1;
    size_t off = 0;
    while (off < (size_t)len) {
      size_t queued = tinyusb_cdcacm_write_queue(BB_DEVMON_CDC_CONSOLE,
                                                 (const uint8_t*)buf + off,
                                                 (size_t)len - off);
      if (queued == 0) break; /* host not draining; drop the rest silently */
      off += queued;
    }
    if (off > 0) {
      tinyusb_cdcacm_write_flush(BB_DEVMON_CDC_CONSOLE, 0);
    }
  }

  /* Also forward to the original sink (UART0) so nothing is lost. */
  if (s_prior_vprintf) return s_prior_vprintf(fmt, args);
  return len;
}

/* ---------------- CRC16-CCITT (poly 0x1021, init 0xFFFF) ---------------- */

static uint16_t crc16_ccitt(uint16_t seed, const uint8_t* data, size_t len) {
  uint16_t crc = seed;
  for (size_t i = 0; i < len; ++i) {
    crc ^= (uint16_t)data[i] << 8;
    for (int b = 0; b < 8; ++b) {
      crc = (crc & 0x8000) ? (uint16_t)((crc << 1) ^ 0x1021) : (uint16_t)(crc << 1);
    }
  }
  return crc;
}

/* ---------------------------- TX over CDC1 ---------------------------- */

/* Push a buffer through tusb_cdc_acm with chunked flush so the host sees
 * data continuously even on large payloads (e.g. 110 KB screenshots). */
static esp_err_t devmon_cdc_write(const uint8_t* data, size_t len) {
  /* Queue as much as TinyUSB will accept in one call; only flush when the
   * buffer fills up (queue returned 0). Per-chunk flushes added too much
   * overhead and caused the 110 KB screenshot path to time out. */
  size_t offset = 0;
  int stalled_flushes = 0;
  while (offset < len) {
    const size_t queued =
        tinyusb_cdcacm_write_queue(BB_DEVMON_CDC_PROTOCOL,
                                   data + offset, len - offset);
    if (queued > 0) {
      offset += queued;
      stalled_flushes = 0;
    } else {
      /* Buffer full — flush to make room. 主机不拉数据（发完命令就关端口）时
       * flush 永远排不动，旧实现在这里无限自旋把 devmon worker 卡死。
       * 连续 6 次（约 3s）排不动就放弃本帧。 */
      tinyusb_cdcacm_write_flush(BB_DEVMON_CDC_PROTOCOL, pdMS_TO_TICKS(500));
      if (++stalled_flushes >= 6) {
        ESP_LOGW(TAG, "cdc write stalled (host not draining), dropping %u/%u bytes",
                 (unsigned)(len - offset), (unsigned)len);
        return ESP_ERR_TIMEOUT;
      }
    }
  }
  /* Final flush so the last partial buffer is pushed to host. */
  tinyusb_cdcacm_write_flush(BB_DEVMON_CDC_PROTOCOL, pdMS_TO_TICKS(1000));
  return ESP_OK;
}

static esp_err_t devmon_send_frame(uint8_t kind, uint16_t seq,
                                   const uint8_t* payload, uint32_t payload_len) {
  uint8_t header[10];
  header[0] = BB_DEVMON_MAGIC0;
  header[1] = BB_DEVMON_MAGIC1;
  header[2] = kind;
  header[3] = 0;
  header[4] = (uint8_t)(payload_len & 0xFF);
  header[5] = (uint8_t)((payload_len >> 8) & 0xFF);
  header[6] = (uint8_t)((payload_len >> 16) & 0xFF);
  header[7] = (uint8_t)((payload_len >> 24) & 0xFF);
  header[8] = (uint8_t)(seq & 0xFF);
  header[9] = (uint8_t)((seq >> 8) & 0xFF);

  uint16_t crc = crc16_ccitt(0xFFFF, header + 2, 8);
  if (devmon_cdc_write(header, sizeof(header)) != ESP_OK) return ESP_FAIL;
  if (payload_len > 0 && payload != NULL) {
    if (devmon_cdc_write(payload, payload_len) != ESP_OK) return ESP_FAIL;
    crc = crc16_ccitt(crc, payload, payload_len);
  }
  const uint8_t crc_bytes[2] = {
      (uint8_t)(crc & 0xFF),
      (uint8_t)((crc >> 8) & 0xFF),
  };
  if (devmon_cdc_write(crc_bytes, 2) != ESP_OK) return ESP_FAIL;
  return ESP_OK;
}

static inline esp_err_t devmon_send_err(uint16_t seq, device_monitor_err_t code) {
  const uint8_t body = (uint8_t)code;
  return devmon_send_frame(KIND_ERR, seq, &body, 1);
}

/* ---------------------------- Screenshot ---------------------------- */

static esp_err_t devmon_send_screenshot(uint16_t seq,
                                        uint16_t width, uint16_t height,
                                        const uint8_t* pixels, size_t pixels_len) {
  const uint32_t payload_len = 4 + (uint32_t)pixels_len;

  uint8_t header[10];
  header[0] = BB_DEVMON_MAGIC0;
  header[1] = BB_DEVMON_MAGIC1;
  header[2] = KIND_RES_SCREENSHOT;
  header[3] = 0;
  header[4] = (uint8_t)(payload_len & 0xFF);
  header[5] = (uint8_t)((payload_len >> 8) & 0xFF);
  header[6] = (uint8_t)((payload_len >> 16) & 0xFF);
  header[7] = (uint8_t)((payload_len >> 24) & 0xFF);
  header[8] = (uint8_t)(seq & 0xFF);
  header[9] = (uint8_t)((seq >> 8) & 0xFF);

  const uint8_t dim[4] = {
      (uint8_t)(width & 0xFF),  (uint8_t)((width >> 8) & 0xFF),
      (uint8_t)(height & 0xFF), (uint8_t)((height >> 8) & 0xFF),
  };

  uint16_t crc = crc16_ccitt(0xFFFF, header + 2, 8);
  crc = crc16_ccitt(crc, dim, sizeof(dim));
  crc = crc16_ccitt(crc, pixels, pixels_len);

  if (devmon_cdc_write(header, sizeof(header)) != ESP_OK) return ESP_FAIL;
  if (devmon_cdc_write(dim, sizeof(dim)) != ESP_OK) return ESP_FAIL;
  if (devmon_cdc_write(pixels, pixels_len) != ESP_OK) return ESP_FAIL;
  const uint8_t crc_bytes[2] = {
      (uint8_t)(crc & 0xFF),
      (uint8_t)((crc >> 8) & 0xFF),
  };
  if (devmon_cdc_write(crc_bytes, 2) != ESP_OK) return ESP_FAIL;
  return ESP_OK;
}

static void handle_req_screenshot(uint16_t seq) {
  ESP_LOGI(TAG, "REQ_SCREENSHOT seq=%u: enter", seq);

  if (!lvgl_port_lock(500)) {
    ESP_LOGE(TAG, "lvgl_port_lock(500) timeout");
    devmon_send_err(seq, ERR_LVGL_LOCK);
    return;
  }
  /* 分步执行 snapshot（buf 分配 / 渲染分开），崩溃时日志能落在具体一步；
   * 顺带监控本任务栈水位（曾在 410x502 屏上 4KB 栈溢出秒崩）。 */
  ESP_LOGI(TAG, "lvgl lock acquired, stack_hwm=%u free_spiram=%u",
           (unsigned)uxTaskGetStackHighWaterMark(NULL),
           (unsigned)heap_caps_get_largest_free_block(MALLOC_CAP_SPIRAM));
  lv_obj_t* scr = lv_screen_active();
  lv_draw_buf_t* snap = lv_snapshot_create_draw_buf(scr, LV_COLOR_FORMAT_RGB565);
  ESP_LOGI(TAG, "snapshot draw_buf=%p", snap);
  if (snap != NULL) {
    lv_result_t res = lv_snapshot_take_to_draw_buf(scr, LV_COLOR_FORMAT_RGB565, snap);
    ESP_LOGI(TAG, "snapshot render res=%d stack_hwm=%u", (int)res,
             (unsigned)uxTaskGetStackHighWaterMark(NULL));
    if (res != LV_RESULT_OK) {
      lv_draw_buf_destroy(snap);
      snap = NULL;
    }
  }
  lvgl_port_unlock();

  if (snap == NULL || snap->data == NULL || snap->data_size == 0) {
    ESP_LOGE(TAG, "snap empty: ptr=%p data=%p size=%u",
             snap, snap ? snap->data : NULL,
             snap ? (unsigned)snap->data_size : 0u);
    if (snap) lv_draw_buf_destroy(snap);
    devmon_send_err(seq, ERR_SNAPSHOT_FAILED);
    return;
  }

  const uint16_t w = (uint16_t)snap->header.w;
  const uint16_t h = (uint16_t)snap->header.h;
  ESP_LOGI(TAG, "snap %ux%u (%u bytes) cf=%u → wire, sending now",
           w, h, (unsigned)snap->data_size, (unsigned)snap->header.cf);

  esp_err_t err = devmon_send_screenshot(seq, w, h, snap->data, snap->data_size);
  ESP_LOGI(TAG, "screenshot send done: %s", esp_err_to_name(err));
  lv_draw_buf_destroy(snap);
}

/* ---------------------------- Worker queue + task ----------------------
 *
 * Critical: the CDC RX callback runs on TinyUSB's internal task. Doing heavy
 * work (LVGL lock, big payload TX with flushes) directly from the callback
 * deadlocks USB — TinyUSB can't service its own TX completion interrupts
 * while we're blocked waiting for them. The RX callback now does the
 * minimum (enqueue + return); a dedicated worker task drains the queue and
 * dispatches handlers from a safe context.
 */

typedef struct {
  uint8_t  kind;
  uint16_t seq;
  uint16_t payload_len;
  uint8_t  payload[BB_DEVMON_MAX_RX_PAYLOAD];
} devmon_req_t;

static QueueHandle_t s_req_queue;

static void on_frame(uint8_t kind, uint16_t seq,
                     const uint8_t* payload, uint32_t payload_len) {
  /* CDC RX context — keep this fast. */
  devmon_req_t msg = {
      .kind = kind,
      .seq = seq,
      .payload_len = (uint16_t)payload_len,
  };
  if (payload_len > 0 && payload_len <= sizeof(msg.payload) && payload != NULL) {
    memcpy(msg.payload, payload, payload_len);
  }
  if (s_req_queue == NULL ||
      xQueueSend(s_req_queue, &msg, 0) != pdTRUE) {
    /* Queue full — reply directly with an error. devmon_send_err is small
     * enough (~13 bytes) to fit in the CDC TX buffer without flushing. */
    devmon_send_err(seq, ERR_PAYLOAD_LIMIT);
  }
}

static void devmon_worker_task(void* arg) {
  (void)arg;
  devmon_req_t msg;
  while (true) {
    if (xQueueReceive(s_req_queue, &msg, portMAX_DELAY) != pdTRUE) continue;

    switch (msg.kind) {
      case KIND_REQ_ECHO:
        ESP_LOGI(TAG, "REQ_ECHO seq=%u len=%u", msg.seq, msg.payload_len);
        devmon_send_frame(KIND_RES_ECHO, msg.seq, msg.payload, msg.payload_len);
        break;

      case KIND_REQ_SCREENSHOT:
        handle_req_screenshot(msg.seq);
        break;

      case KIND_REQ_REBOOT_TO_BOOTLOADER: {
        /* 进 ROM 下载模式。单口 USB 板（手表：OTG/TinyUSB 与 USJ 复用同一个口）
         * 必须先把 USB 控制器交还并置 OTG 持久化标志，否则 ROM 起来后 USB 不
         * 枚举——芯片复位成功但外界看是"整机冻死"（AMOLED 自带显存还显示最后
         * 一帧，时钟停走）。配方来自 arduino-esp32 usb_persist（S3 单口板实证）：
         * prepare_persist + PERSIST_ENA + FORCE_DOWNLOAD_BOOT + esp_restart。
         * 旧实现 esp_rom_software_reset_system() 在 bbclaw PCB（USJ 走独立
         * hub 通道）上可用，在手表上间歇性挂死。 */
        ESP_LOGI(TAG, "REQ_REBOOT_TO_BOOTLOADER seq=%u", msg.seq);
        const uint8_t ack = 0;
        devmon_send_frame(KIND_RES_REBOOT_ACK, msg.seq, &ack, 1);
        /* Let the ACK clear the USB pipe before yanking the bus. */
        vTaskDelay(pdMS_TO_TICKS(150));
        ESP_LOGI(TAG, "reboot: usb disconnect, then ROM reset");
#if CONFIG_IDF_TARGET_ESP32S3
        /* 单口 USB 板（手表）的最终方案，前两版的教训：
         *  v1 esp_restart()+persist——WiFi/WS 活跃时 shutdown handler 阻塞，挂死；
         *  v2 persist 标志+ROM 复位——长 uptime 下 host 保留旧会话但 ROM 不应答，
         *     留下 esptool/pyusb 都救不回的僵尸端口（实测两次）。
         * v3：先 tud_disconnect() 拉掉 D+ 让 host 干净丢弃会话，再清 USB PHY
         * 选择寄存器（RTC 域，软复位不清），最后 FORCE_DOWNLOAD_BOOT + ROM 复位
         * ——ROM 以出厂语义全新枚举 USJ 下载口。全程寄存器/ROM 调用，无阻塞点。 */
        /* ADR-044:录音会话进行中先收尾(停编码+fsync+关文件),否则复位留脏 FAT
   * ——整卡写路径 EIO(真机踩过)。非录音态为 no-op。 */
  bb_recorder_stop();
  tud_disconnect();
        vTaskDelay(pdMS_TO_TICKS(300));
        REG_WRITE(RTC_CNTL_USB_CONF_REG, 0);
        REG_WRITE(RTC_CNTL_OPTION1_REG, RTC_CNTL_FORCE_DOWNLOAD_BOOT);
        esp_rom_software_reset_system();
#else
        SET_PERI_REG_MASK(RTC_CNTL_OPTION1_REG, RTC_CNTL_FORCE_DOWNLOAD_BOOT);
        esp_rom_software_reset_system();
#endif
        /* Unreachable. */
        break;
      }

      case KIND_REQ_INPUT: {
        if (msg.payload_len < 1) {
          ESP_LOGW(TAG, "REQ_INPUT seq=%u: empty payload", msg.seq);
          devmon_send_err(msg.seq, ERR_PAYLOAD_LIMIT);
          break;
        }
        const uint8_t event_id = msg.payload[0];
        if (event_id >= BB_NAV_EVENT_COUNT) {
          ESP_LOGW(TAG, "REQ_INPUT seq=%u: invalid event=%u",
                   msg.seq, event_id);
          devmon_send_err(msg.seq, ERR_UNKNOWN_KIND);
          break;
        }
        ESP_LOGI(TAG, "REQ_INPUT seq=%u event=%u", msg.seq, event_id);
        bb_nav_input_inject((bb_nav_event_t)event_id);
        const uint8_t ack = 0;
        devmon_send_frame(KIND_RES_INPUT_ACK, msg.seq, &ack, 1);
        break;
      }

      default:
        ESP_LOGW(TAG, "unknown kind 0x%02x seq=%u len=%u",
                 msg.kind, msg.seq, msg.payload_len);
        devmon_send_err(msg.seq, ERR_UNKNOWN_KIND);
        break;
    }
  }
}

/* ---------------------------- RX state machine ---------------------------- */

typedef enum {
  RX_WAIT_MAGIC0 = 0,
  RX_WAIT_MAGIC1,
  RX_READ_HEADER,
  RX_READ_PAYLOAD,
  RX_READ_CRC,
} rx_state_t;

static struct {
  rx_state_t state;
  uint8_t    header[8];
  size_t     header_pos;
  uint8_t    payload[BB_DEVMON_MAX_RX_PAYLOAD];
  size_t     payload_pos;
  uint32_t   payload_len;
  uint16_t   seq;
  uint8_t    crc_bytes[2];
  size_t     crc_pos;
  uint16_t   running_crc;
} rx;

static void rx_reset(void) {
  rx.state = RX_WAIT_MAGIC0;
  rx.header_pos = 0;
  rx.payload_pos = 0;
  rx.payload_len = 0;
  rx.seq = 0;
  rx.crc_pos = 0;
  rx.running_crc = 0xFFFF;
}

static void rx_feed_byte(uint8_t b) {
  switch (rx.state) {
    case RX_WAIT_MAGIC0:
      if (b == BB_DEVMON_MAGIC0) rx.state = RX_WAIT_MAGIC1;
      break;

    case RX_WAIT_MAGIC1:
      if (b == BB_DEVMON_MAGIC1) {
        rx.header_pos = 0;
        rx.running_crc = 0xFFFF;
        rx.state = RX_READ_HEADER;
      } else if (b == BB_DEVMON_MAGIC0) {
        /* stay one step in */
      } else {
        rx.state = RX_WAIT_MAGIC0;
      }
      break;

    case RX_READ_HEADER:
      rx.header[rx.header_pos++] = b;
      rx.running_crc = crc16_ccitt(rx.running_crc, &b, 1);
      if (rx.header_pos == sizeof(rx.header)) {
        rx.payload_len = (uint32_t)rx.header[2]
                       | ((uint32_t)rx.header[3] << 8)
                       | ((uint32_t)rx.header[4] << 16)
                       | ((uint32_t)rx.header[5] << 24);
        rx.seq = (uint16_t)rx.header[6] | ((uint16_t)rx.header[7] << 8);

        if (rx.payload_len > BB_DEVMON_MAX_RX_PAYLOAD) {
          ESP_LOGW(TAG, "rx payload too big: %u > %d, drop",
                   (unsigned)rx.payload_len, BB_DEVMON_MAX_RX_PAYLOAD);
          devmon_send_err(rx.seq, ERR_PAYLOAD_LIMIT);
          rx_reset();
          break;
        }
        if (rx.payload_len == 0) {
          rx.state = RX_READ_CRC;
        } else {
          rx.payload_pos = 0;
          rx.state = RX_READ_PAYLOAD;
        }
      }
      break;

    case RX_READ_PAYLOAD:
      rx.payload[rx.payload_pos++] = b;
      rx.running_crc = crc16_ccitt(rx.running_crc, &b, 1);
      if (rx.payload_pos == rx.payload_len) {
        rx.state = RX_READ_CRC;
      }
      break;

    case RX_READ_CRC:
      rx.crc_bytes[rx.crc_pos++] = b;
      if (rx.crc_pos == 2) {
        const uint16_t got = (uint16_t)rx.crc_bytes[0]
                           | ((uint16_t)rx.crc_bytes[1] << 8);
        if (got == rx.running_crc) {
          on_frame(rx.header[0], rx.seq, rx.payload, rx.payload_len);
        } else {
          ESP_LOGW(TAG, "crc mismatch want=%04x got=%04x seq=%u kind=0x%02x",
                   rx.running_crc, got, rx.seq, rx.header[0]);
        }
        rx_reset();
      }
      break;
  }
}

/* ---------------------------- CDC RX callback ---------------------------- */

/* Drain whatever the host queued on CDC1, feed it through the state
 * machine. Called from the TinyUSB task context, so we keep the work cheap
 * and bounded. Anything heavy (screenshot capture) runs on the LVGL task
 * from inside on_frame -> handler. */
static void on_cdc_protocol_rx(int itf, cdcacm_event_t* event) {
  (void)event;
  uint8_t buf[64];
  size_t got = 0;
  while (tinyusb_cdcacm_read(itf, buf, sizeof(buf), &got) == ESP_OK && got > 0) {
    for (size_t i = 0; i < got; ++i) {
      rx_feed_byte(buf[i]);
    }
  }
}

/* ---------------------------- Init ---------------------------- */

esp_err_t bb_device_monitor_init(void) {
  ESP_LOGI(TAG, "init: TinyUSB dual CDC (CDC0=console, CDC1=protocol), frame proto v2");

  const tinyusb_config_t tusb_cfg = {
      .device_descriptor = NULL,
      .string_descriptor = NULL,
      .external_phy = false,
      .configuration_descriptor = NULL,
  };
  esp_err_t err = tinyusb_driver_install(&tusb_cfg);
  if (err != ESP_OK) {
    ESP_LOGE(TAG, "tinyusb_driver_install failed: %s", esp_err_to_name(err));
    return err;
  }

  const tinyusb_config_cdcacm_t cdc0_cfg = {
      .usb_dev = TINYUSB_USBDEV_0,
      .cdc_port = BB_DEVMON_CDC_CONSOLE,
      .rx_unread_buf_sz = 64,
      .callback_rx = NULL,
      .callback_rx_wanted_char = NULL,
      .callback_line_state_changed = NULL,
      .callback_line_coding_changed = NULL,
  };
  err = tusb_cdc_acm_init(&cdc0_cfg);
  if (err != ESP_OK) {
    ESP_LOGE(TAG, "cdc0 init failed: %s", esp_err_to_name(err));
    return err;
  }

  /* NOTE: esp_tusb_init_console(CDC0) was tried but the redirect appeared
   * to silently fail — CDC0 produced no bytes on host even when host was
   * actively reading. Leaving CDC0 enumerated as a placeholder so the host
   * tty layout matches the future plan; logs remain on UART0 (CH340)
   * until we figure out the redirect or move to a different log strategy. */

  const tinyusb_config_cdcacm_t cdc1_cfg = {
      .usb_dev = TINYUSB_USBDEV_0,
      .cdc_port = BB_DEVMON_CDC_PROTOCOL,
      .rx_unread_buf_sz = 512,
      .callback_rx = on_cdc_protocol_rx,
      .callback_rx_wanted_char = NULL,
      .callback_line_state_changed = NULL,
      .callback_line_coding_changed = NULL,
  };
  err = tusb_cdc_acm_init(&cdc1_cfg);
  if (err != ESP_OK) {
    ESP_LOGE(TAG, "cdc1 init failed: %s", esp_err_to_name(err));
    return err;
  }

  /* Spin up the worker queue + task before enabling the RX callback context
   * paths that depend on them. */
  s_req_queue = xQueueCreate(8, sizeof(devmon_req_t));
  if (s_req_queue == NULL) {
    ESP_LOGE(TAG, "queue alloc failed");
    return ESP_ERR_NO_MEM;
  }
  /* 12KB 栈：lv_snapshot_take 在本任务上下文里软渲染整棵 UI 树，4KB 在
   * 410x502 大屏（手表）上栈溢出秒崩重启。栈随其它音频任务的先例放 PSRAM，
   * 不挤内部 DRAM。 */
  BaseType_t task_ok = xTaskCreateWithCaps(devmon_worker_task, "bb_devmon_w",
                                           12288, NULL, 5, NULL,
                                           BBCLAW_MALLOC_CAP_PREFER_PSRAM);
  if (task_ok != pdPASS) {
    ESP_LOGE(TAG, "worker task create failed");
    return ESP_ERR_NO_MEM;
  }

  rx_reset();

  /* Install the CDC0 log tee. After this, all ESP_LOG output is mirrored
   * onto CDC0 in addition to whatever the previous sink was (UART0). */
  s_prior_vprintf = esp_log_set_vprintf(cdc0_tee_vprintf);
  ESP_LOGI(TAG, "CDC0 log tee installed");

  ESP_LOGI(TAG, "ready: echo + screenshot active over CDC1 (input pending phase 5)");
  return ESP_OK;
}

#endif /* CONFIG_BBCLAW_DEVICE_MONITOR */
