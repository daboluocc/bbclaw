/**
 * bb_camera.h — OV2640 DVP 摄像头（实战派 lichuang-szp，ADR-049 Phase 1）.
 *
 * 复用已初始化的 port0 I2C 作 SCCB；PWDN 走 PCA9557 bit2。帧缓冲在 PSRAM。
 * Phase 1 只做采集 + 自测，不含上传链路（上传见 ADR-049 §4/§5.3）。
 */
#pragma once

#include <esp_err.h>
#include <stddef.h>
#include <stdint.h>

#include "bb_config.h"

#if BBCLAW_CAMERA_ENABLE

/** 一帧 JPEG 的借出视图。数据由 esp32-camera 的帧缓冲池持有，用完必须
 *  bb_camera_fb_return() 归还，否则 fb_count 用尽后 capture 阻塞/失败。 */
typedef struct {
    const uint8_t *buf;  /* JPEG 字节流（PSRAM），起始为 FFD8 */
    size_t         len;  /* JPEG 字节数 */
    uint16_t       width;
    uint16_t       height;
    void          *_handle; /* 内部：原始 camera_fb_t*，归还时按指针身份匹配帧池，勿动 */
} bb_camera_frame_t;

/** 上电 + 初始化 OV2640（幂等）。内部：PCA9557 bit2 拉低上电 → esp_camera_init
 *  （SCCB 复用 port0 I2C，帧缓冲 PSRAM，输出 JPEG）。必须在 bb_pca9557_init 之后调。 */
esp_err_t bb_camera_init(void);

/** 抓一帧 JPEG。成功后 out 指向帧缓冲池内的数据，调用方用完必须
 *  bb_camera_fb_return(out)。未 init 或采集失败返回错误。 */
esp_err_t bb_camera_capture_jpeg(bb_camera_frame_t *out);

/** 归还一帧（配对 bb_camera_capture_jpeg）。 */
void bb_camera_fb_return(bb_camera_frame_t *frame);

#if BBCLAW_CAMERA_SELFTEST
/** Bring-up 自测：抓一帧，打印 size / JPEG magic / 分辨率到串口。失败打错误。
 *  证据优先（embedded-serial-first）：串口日志即验收依据，无需 SD/截图。 */
void bb_camera_selftest(void);
#endif

#endif /* BBCLAW_CAMERA_ENABLE */
