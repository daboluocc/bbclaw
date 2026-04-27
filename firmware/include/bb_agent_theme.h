#pragma once

#include "esp_err.h"
#include "lvgl.h"

/**
 * Agent Chat 主题抽象（Phase 4.1）
 *
 * 屏幕本体（bb_ui_agent_chat）只负责接事件、维护状态；视觉全部委托给主题。
 * 详细背景见 design/firmware_agent_integration.md §"主题系统（核心抽象）"。
 *
 * 七态语义沿用 claude-desktop-buddy 的命名，触发条件：
 *   sleep      → adapter 不可达 / 无活跃 session
 *   idle       → 上一次 turn_end 之后
 *   busy       → 流式生成中（EvText 帧到达期间）
 *   attention  → tool_call / 等审批
 *   celebrate  → token 累计达阈值
 *   dizzy      → 错误
 *   heart      → 快速 turn_end（响应 < 5s）
 */

typedef enum {
  BB_AGENT_STATE_SLEEP = 0,
  BB_AGENT_STATE_IDLE,
  BB_AGENT_STATE_BUSY,        /* agent 在算（ASR / 回复生成中） */
  BB_AGENT_STATE_ATTENTION,
  BB_AGENT_STATE_CELEBRATE,
  BB_AGENT_STATE_DIZZY,
  BB_AGENT_STATE_HEART,
  /* Phase 4.8.x — 区分"用户在说"和"agent 在思考"和"在播 TTS"
   * 加在 enum 末尾保留旧值（不破坏既有 switch / 主题表）。 */
  BB_AGENT_STATE_LISTENING,   /* PTT 按住时：mic 录音中 */
  BB_AGENT_STATE_SPEAKING,    /* TTS 播放中 */
} bb_agent_state_t;

typedef struct bb_agent_theme {
  const char* name;                                       /* "text-only", "buddy-ascii", … */
  void (*on_enter)(lv_obj_t* parent);                     /* 建初始 UI（一次） */
  void (*on_exit)(void);                                  /* 清理（一次） */
  void (*set_state)(bb_agent_state_t state);              /* 七态切换 */
  void (*append_user)(const char* text);                  /* 用户消息 */
  void (*append_assistant_chunk)(const char* delta);      /* 助手文本流式 append */
  void (*append_tool_call)(const char* tool, const char* hint);
  void (*append_error)(const char* msg);
  void (*set_driver)(const char* driver_name);            /* 顶部状态栏更新 */
  void (*set_session)(const char* sid_short);             /* 显示 session 前 8 位 */
  void (*scroll_transcript)(int lines);                   /* Phase 4.9: UP/DOWN 滚动对话; <0=up >0=down */
} bb_agent_theme_t;

/**
 * 注册主题；name 重复时覆盖前一个并打 warning。
 * theme 指针本身需在程序生命期内有效（典型：static const）。
 */
void bb_agent_theme_register(const bb_agent_theme_t* theme);

/**
 * 显式注册内置 "text-only" 主题。在 app 启动早期调一次（典型：bb_radio_app_start
 * 入口），保证 bb_agent_theme_get_active() 第一次被调时 registry 不为空。
 *
 * 之前用 GCC __attribute__((constructor)) 自注册，但 ESP-IDF 静态库链接 +
 * --gc-sections 会把没有外部引用的构造函数 DCE 掉，真机上 registry 空了。
 * 改成显式 init 后由调用点 force-link，绕开 DCE。幂等（重名注册带警告）。
 */
void bb_theme_text_only_init(void);

/**
 * Phase 4.6 — 显式注册 "buddy-ascii" 主题（七态 ASCII 角色）。
 * 与 text-only_init 同样的 DCE 规避模式：bb_radio_app_start 调一次。
 */
void bb_theme_buddy_ascii_init(void);

/**
 * Phase 4.6.x — 显式注册 "buddy-anim" 主题（九态 ASCII + LVGL 动效）。
 */
void bb_theme_buddy_anim_init(void);

/**
 * 当前激活主题；首次调用时从 NVS（namespace=bbclaw, key=agent/theme）加载，缺省 fallback 到 "text-only"。
 * 永不返回 NULL（只要至少注册过一个主题；text-only 通过 constructor 自动注册）。
 */
const bb_agent_theme_t* bb_agent_theme_get_active(void);

/**
 * 切换主题；name 不存在时返回 ESP_ERR_NOT_FOUND，不改动当前激活态。
 * 成功后写入 NVS。注意：本函数不调用 on_exit/on_enter；调用方需自行重建屏幕。
 */
esp_err_t bb_agent_theme_set_active(const char* name);

/**
 * 列出当前已注册主题名（供菜单使用）。返回的指针数组生命期与注册的主题对象绑定。
 * out_count 为 NULL 时安全。
 */
const char* const* bb_agent_theme_list(int* out_count);
