#pragma once

/** 填充云上 deviceId：BBClaw-<固件版本>-<MAC 后三字节>，与 SoftAP SSID 后缀同源。须在首处使用 BBCLAW_DEVICE_ID 之前调用。 */
void bbclaw_identity_init(void);

const char *bbclaw_device_id(void);
