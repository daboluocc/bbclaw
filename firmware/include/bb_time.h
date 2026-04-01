#pragma once

#include <stddef.h>
#include <stdint.h>
#include <stdbool.h>
#include <time.h>

int64_t bb_now_ms(void);

/** 连接公网后调用一次；内部启动 SNTP（pool.ntp.org 等），设置 TZ=CST-8 */
void bb_sntp_start(void);

/** SNTP 已完成同步，或系统时间已被其它途径设为合理值 */
bool bb_wall_time_ready(void);

/** 格式化为本地 HH:MM；未就绪时写入 "--:--" */
void bb_wall_time_format_hm(char *buf, size_t buf_sz);

/** Adapter 或调试：直接写入 Unix 秒时间 */
void bb_wall_time_set_unix(time_t unix_sec);
