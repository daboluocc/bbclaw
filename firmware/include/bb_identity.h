#pragma once

/** 填充云上 deviceId：BBClaw-<固件版本>-<MAC 后三字节>，与 SoftAP SSID 后缀同源。须在首处使用 BBCLAW_DEVICE_ID 之前调用。 */
void bbclaw_identity_init(void);

const char *bbclaw_device_id(void);

/** 返回 per-device session key："agent:main:bbclaw-XXYYZZ"（MAC 后缀），多设备并行时各自隔离。 */
const char *bbclaw_session_key(void);
