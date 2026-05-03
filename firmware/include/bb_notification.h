#pragma once

#include "esp_err.h"

#define BB_NOTIFY_MAX 16

typedef struct {
    char session_id[64];
    char driver[24];
    char preview[48];
    int64_t timestamp;
    uint8_t type;   /* 0=turn_end, 1=error, 2=tool_approval */
    uint8_t read;   /* 0=unread, 1=read */
} bb_notification_t;

esp_err_t bb_notification_init(void);

void bb_notification_on_ws_event(const char* sid, const char* driver,
                                  const char* type, const char* preview);

int bb_notification_unread_count(void);

int bb_notification_unread_for_session(const char* session_id);

void bb_notification_mark_read(const char* session_id);

void bb_notification_ack(const char* session_id);
