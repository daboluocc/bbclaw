#pragma once

#include "esp_err.h"

/**
 * Agent Bus 设备端客户端（Phase 4.0）
 *
 * 配套 adapter `POST /v1/agent/message` NDJSON 端点 + `GET /v1/agent/drivers`。
 * 每行 NDJSON 解析成一帧 bb_agent_stream_event_t 丢给 user 回调。
 * 详细协议见 design/agent_bus.md §6 与 design/firmware_agent_integration.md §4。
 *
 * 注意：所有 const char* 字段只在回调返回前有效；回调若需要保留请自行 strdup。
 */

typedef enum {
  BB_AGENT_EVENT_SESSION = 0, /* 首帧：sessionId / isNew / driver */
  BB_AGENT_EVENT_TEXT,        /* assistant 文本片段（可多次） */
  BB_AGENT_EVENT_TOOL_CALL,   /* display-only（Phase 2 再做审批） */
  BB_AGENT_EVENT_TOKENS,      /* in/out token 用量 */
  BB_AGENT_EVENT_TURN_END,    /* 一轮结束 */
  BB_AGENT_EVENT_ERROR,       /* driver/adapter 层错误 */
} bb_agent_event_type_t;

typedef struct {
  bb_agent_event_type_t type;
  const char* text;       /* TEXT / ERROR 用 */
  const char* session_id; /* SESSION 用 */
  const char* driver;     /* SESSION / TOOL_CALL 用 */
  int is_new;             /* SESSION 用 */
  const char* tool;       /* TOOL_CALL 用，工具名（Bash/Edit/...） */
  const char* hint;       /* TOOL_CALL 用，简短预览 */
  int tokens_in;          /* TOKENS 用 */
  int tokens_out;         /* TOKENS 用 */
  const char* error_code; /* ERROR 用 */
} bb_agent_stream_event_t;

typedef void (*bb_agent_stream_cb_t)(const bb_agent_stream_event_t* event, void* user_ctx);

typedef struct {
  char name[24];
  int tool_approval;
  int resume;
  int streaming;
} bb_agent_driver_info_t;

/**
 * 列出 adapter 上可用 driver。
 *
 * @param out_list   调用方提供的数组；可为 NULL（仅查总数）。
 * @param cap        out_list 容量；out_list 为 NULL 时忽略。
 * @param out_count  实际可用 driver 总数（不受 cap 限制），即使 cap < 总数也按总数填。
 * @return ESP_OK / 错误码。
 */
esp_err_t bb_agent_list_drivers(bb_agent_driver_info_t* out_list, int cap, int* out_count);

/**
 * 发一条用户消息并流式读取 NDJSON 事件，阻塞直到 turn_end / error / HTTP 关闭。
 *
 * @param text         用户输入；不可为空。
 * @param session_id   续接已存在 session（来自上次 SESSION 事件）；NULL 或 "" = 新开会话。
 * @param driver_name  指定 driver；NULL 或 "" = 走 adapter 默认。
 *                     有 session_id 时除非显式 mismatch，driver 由 session 决定（详见 ADR-003）。
 * @param on_event     每行 NDJSON 触发一次；可为 NULL（仅丢弃）。
 * @param user_ctx     透传给 on_event。
 * @return ESP_OK 表示拿到 turn_end；其他码表示 HTTP/解析/error 失败。
 */
esp_err_t bb_agent_send_message(const char* text, const char* session_id, const char* driver_name,
                                bb_agent_stream_cb_t on_event, void* user_ctx);
