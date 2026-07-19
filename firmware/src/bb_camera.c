/**
 * bb_camera.c — OV2640 DVP 摄像头驱动（实战派 lichuang-szp，ADR-049 Phase 1）.
 *
 * 硬件：ESP32-S3 LCD_CAM(CAM) + DVP 8bit 并口。引脚见 board_config.h（真相源
 * xiaozhi lichuang-dev）。SCCB 复用已初始化的 port0 I2C 总线；PWDN=PCA9557 bit2。
 */
#include "bb_camera.h"

#if BBCLAW_CAMERA_ENABLE

#include <stdlib.h>
#include <esp_check.h>
#include <esp_heap_caps.h>
#include <esp_log.h>
#include <driver/i2c_master.h>
#include <driver/ledc.h>
#include <freertos/FreeRTOS.h>
#include <freertos/task.h>

#include "esp_camera.h"
#include "img_converters.h"
#include "bb_pca9557.h"
#include "bb_adapter_client.h"

static const char *TAG = "bb_camera";

static int s_ready;

esp_err_t bb_camera_init(void) {
    if (s_ready) return ESP_OK;

    /* SCCB 复用 port0 I2C：该总线由 bb_pca9557_init / bb_audio 先建。此处仅确认
     * 它已存在（拿不到句柄说明 init 顺序错了，直接失败而非另建避免双主机占脚）。 */
    i2c_master_bus_handle_t bus = NULL;
    ESP_RETURN_ON_ERROR(i2c_master_get_bus_handle(BBCLAW_CAMERA_SCCB_PORT, &bus), TAG,
                        "port%d I2C bus not ready (call after bb_pca9557_init)", BBCLAW_CAMERA_SCCB_PORT);

#if BBCLAW_CAMERA_SELFTEST
    /* bring-up 诊断：让 esp32-camera 打出检测到的 sensor PID（默认 DEBUG 级被吞）。 */
    esp_log_level_set("camera", ESP_LOG_DEBUG);
    esp_log_level_set("sccb", ESP_LOG_DEBUG);
    esp_log_level_set("cam_hal", ESP_LOG_DEBUG);
#endif
    /* PWDN 拉低上电（PCA9557 bit2）。OV2640 上电到稳定需要几个 ms + XCLK 就绪。 */
    ESP_RETURN_ON_ERROR(bb_pca9557_set_output(BBCLAW_PCA9557_CAM_PWDN_BIT, 0), TAG, "cam power on");
    vTaskDelay(pdMS_TO_TICKS(10));

    camera_config_t config = {
        .pin_pwdn = -1,   /* 由 PCA9557 管，不走 GPIO */
        .pin_reset = -1,  /* NC */
        .pin_xclk = BBCLAW_CAMERA_PIN_XCLK,
        .pin_sccb_sda = -1,                    /* -1 = 复用现有 I2C 总线 */
        .pin_sccb_scl = BBCLAW_CAMERA_PIN_SIOC,
        .sccb_i2c_port = BBCLAW_CAMERA_SCCB_PORT,
        .pin_d7 = BBCLAW_CAMERA_PIN_D7,
        .pin_d6 = BBCLAW_CAMERA_PIN_D6,
        .pin_d5 = BBCLAW_CAMERA_PIN_D5,
        .pin_d4 = BBCLAW_CAMERA_PIN_D4,
        .pin_d3 = BBCLAW_CAMERA_PIN_D3,
        .pin_d2 = BBCLAW_CAMERA_PIN_D2,
        .pin_d1 = BBCLAW_CAMERA_PIN_D1,
        .pin_d0 = BBCLAW_CAMERA_PIN_D0,
        .pin_vsync = BBCLAW_CAMERA_PIN_VSYNC,
        .pin_href = BBCLAW_CAMERA_PIN_HREF,
        .pin_pclk = BBCLAW_CAMERA_PIN_PCLK,
        .xclk_freq_hz = BBCLAW_CAMERA_XCLK_HZ,
        /* XCLK 用 LEDC 生成。避开已占用：bb_led=TIMER_0/CH0-2、背光=TIMER_1/CH3。
         * 取 TIMER_2/CH4（明确空闲，不管板子 LED 开没开都不撞）。 */
        .ledc_timer = LEDC_TIMER_2,
        .ledc_channel = LEDC_CHANNEL_4,
        /* 板上是 GC0308（PID=0x9b，非 OV2640），无硬件 JPEG，只能采 RGB565/YUV，
         * 由 frame2jpg 软编成 JPEG（ADR-049）。VGA 是 GC0308 上限，给 claude 足够细节。 */
        .pixel_format = PIXFORMAT_RGB565,
        .frame_size = FRAMESIZE_VGA,          /* 640x480 = GC0308 上限 */
        .jpeg_quality = 12,
        .fb_count = 1,
        .fb_location = CAMERA_FB_IN_PSRAM,
        .grab_mode = CAMERA_GRAB_WHEN_EMPTY,
    };

    esp_err_t err = esp_camera_init(&config);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "esp_camera_init failed: %s (0x%x)", esp_err_to_name(err), err);
        /* 失败则回到掉电态，省电且便于下次干净重试。 */
        (void)bb_pca9557_set_output(BBCLAW_PCA9557_CAM_PWDN_BIT, 1);
        return err;
    }

    sensor_t *s = esp_camera_sensor_get();
    if (s) {
        s->set_vflip(s, 1);
        s->set_hmirror(s, 0);
    }

    s_ready = 1;
    ESP_LOGI(TAG, "camera ready: GC0308 RGB565 VGA, sccb=port%d, fb=PSRAM", BBCLAW_CAMERA_SCCB_PORT);
    return ESP_OK;
}

esp_err_t bb_camera_capture_jpeg(bb_camera_frame_t *out) {
    if (!s_ready || !out) return ESP_ERR_INVALID_STATE;
    camera_fb_t *fb = esp_camera_fb_get();
    if (!fb) {
        ESP_LOGE(TAG, "esp_camera_fb_get returned NULL");
        return ESP_FAIL;
    }
    /* GC0308 无硬件 JPEG：软编 RGB565 fb → JPEG（frame2jpg 在 PSRAM 分配 out）。
     * 编完立即归还原始 fb，只留 JPEG 缓冲给调用方。 */
    uint8_t *jpg = NULL;
    size_t jpg_len = 0;
    bool ok = frame2jpg(fb, BBCLAW_CAMERA_JPEG_QUALITY, &jpg, &jpg_len);
    uint16_t w = fb->width, h = fb->height;
    esp_camera_fb_return(fb);
    if (!ok || !jpg || jpg_len == 0) {
        if (jpg) free(jpg);
        ESP_LOGE(TAG, "frame2jpg failed");
        return ESP_FAIL;
    }
    out->buf = jpg;
    out->len = jpg_len;
    out->width = w;
    out->height = h;
    out->_handle = jpg; /* JPEG 堆缓冲，fb_return 时 free */
    return ESP_OK;
}

void bb_camera_fb_return(bb_camera_frame_t *frame) {
    if (!frame || !frame->_handle) return;
    free(frame->_handle); /* JPEG 缓冲由 frame2jpg malloc，free 释放（原始 fb 已在 capture 内归还） */
    frame->_handle = NULL;
    frame->buf = NULL;
    frame->len = 0;
}

esp_err_t bb_camera_shoot_and_send(const char *note) {
    /* 按需 init→拍→发→deinit：不常驻，拍完立刻释放内部 DRAM 与 WiFi 共存。 */
    esp_err_t err = bb_camera_init();
    if (err != ESP_OK) return err;
    bb_camera_frame_t f = {0};
    err = bb_camera_capture_jpeg(&f);
    if (err == ESP_OK) {
        err = bb_adapter_send_image_capture(f.buf, f.len, f.width, f.height, note);
        bb_camera_fb_return(&f);
    }
    bb_camera_deinit();
    ESP_LOGI(TAG, "shoot_and_send err=%s", esp_err_to_name(err));
    return err;
}

void bb_camera_deinit(void) {
    if (!s_ready) return;
    esp_camera_deinit();
    /* PWDN 拉高，摄像头掉电（XCLK 由 esp_camera_deinit 停）。 */
    (void)bb_pca9557_set_output(BBCLAW_PCA9557_CAM_PWDN_BIT, 1);
    s_ready = 0;
}

#if BBCLAW_CAMERA_SELFTEST
void bb_camera_selftest(void) {
    esp_err_t err = bb_camera_init();
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "selftest: init failed: %s", esp_err_to_name(err));
        return;
    }
    bb_camera_frame_t f = {0};
    err = bb_camera_capture_jpeg(&f);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "selftest: capture failed: %s", esp_err_to_name(err));
        return;
    }
    /* 现在是软编后的真 JPEG：验 FFD8 起 / FFD9 收尾。 */
    uint8_t h0 = f.len > 0 ? f.buf[0] : 0, h1 = f.len > 1 ? f.buf[1] : 0;
    uint8_t t0 = f.len > 1 ? f.buf[f.len - 2] : 0, t1 = f.len > 0 ? f.buf[f.len - 1] : 0;
    int magic_ok = (h0 == 0xFF && h1 == 0xD8 && t0 == 0xFF && t1 == 0xD9);
    ESP_LOGI(TAG, "selftest: %ux%u JPEG %u bytes head=%02X%02X tail=%02X%02X magic=%s (GC0308 frame2jpg)",
             f.width, f.height, (unsigned)f.len, h0, h1, t0, t1, magic_ok ? "OK" : "BAD");
    bb_camera_fb_return(&f);

    /* 一次性自测拍完即 deinit——释放 cam_hal 占的内部 DRAM + 停 XCLK，否则 WiFi
     * 起来抢不到内部堆（本板内部 largest 才 ~31KB，是反复崩因）。真正拍照功能
     * 走「按需 init→拍→deinit」而非常驻。打 deinit 前后内部 largest 做证。 */
    size_t before = heap_caps_get_largest_free_block(MALLOC_CAP_INTERNAL);
    bb_camera_deinit();
    size_t after = heap_caps_get_largest_free_block(MALLOC_CAP_INTERNAL);
    ESP_LOGI(TAG, "selftest: camera deinit, internal largest %u -> %u bytes",
             (unsigned)before, (unsigned)after);
}
#endif /* BBCLAW_CAMERA_SELFTEST */

#endif /* BBCLAW_CAMERA_ENABLE */
