#pragma once

#include <stdint.h>

#include "esp_err.h"

esp_err_t bb_display_init(void);
/** 设置当前运行模式，用于状态栏 HOME/CLOUD 图标显示 */
void bb_display_set_cloud_mode(int is_cloud);
esp_err_t bb_display_show_status(const char* status_line);
/** 主界面：仅展示「自己说的」与「助手回复」，短信式上下两行（ME / AI） */
esp_err_t bb_display_show_chat_turn(const char* user_said, const char* assistant_reply);
esp_err_t bb_display_upsert_chat_turn(const char* user_said, const char* assistant_reply, int finalize);

/** 查看更早一轮 / 较新一轮（需 GPIO 等调用；勿在 ISR 内直接调用） */
esp_err_t bb_display_chat_prev_turn(void);
esp_err_t bb_display_chat_next_turn(void);
/** 当前轮内长文上下滚动（默认作用在 AI 栏，可用 focus 切换） */
esp_err_t bb_display_chat_scroll_down(void);
esp_err_t bb_display_chat_scroll_up(void);
void bb_display_chat_focus_me(void);
void bb_display_chat_focus_ai(void);
void bb_display_set_locked(int locked);
/** TTS 播放状态：播放期间抑制 scroll reset 和 auto-scroll 循环回顶部 */
void bb_display_set_tts_playing(int playing);
/** TTS 播放到新句子时，滚动到该句子在回复文本中的位置 */
void bb_display_set_tts_sentence(const char* sentence_text);
/** PTT 录音阶段的实时输入电平（0-100）与是否检测到有效声音 */
void bb_display_set_record_level(uint8_t level_pct, int voiced);
/** 状态栏电池信息；supported=0 时整个组件隐藏，available=0 时显示占位。
 *  charging=1 时顶栏图标显示绿色满格+闪电（数据层当前恒 0，等 VBUS GPIO 接好后启用）。 */
void bb_display_set_battery(int supported, int available, int percent, int low, int charging);
/** 底栏 session id；NULL/"" 清空。仅在 session alias 为空时被渲染（短截后）。 */
void bb_display_set_session_id(const char* session_id);
/** ADR-016: 底栏 session 别名（logical session title，如 "daboluocc-bbclaw"）。
 *  非空时优先显示，比 hex sid 更直观；NULL/"" 时回退到 sid 短截。
 *  Session picker 选择已知 session 时调用；其余路径(新建/cycle/启动恢复)传空字符串。 */
void bb_display_set_session_alias(const char* alias);
/** ADR-016: 底栏右半显示的 model id/label；NULL/"" 显示 em-dash 占位。
 *  由 Settings 切换 model 或者 chat 启动时从 active driver cache 读取触发。 */
void bb_display_set_active_model(const char* model_label);
/** Chat 激活状态：为 1 时强制显示 ACTIVE 视图（顶栏+底栏），
 *  并隐藏底层的简单对话文本区，让 chat overlay 的 transcript 占据中间区域。 */
void bb_display_set_chat_active(int active);
/** ADR-017: 阅读模式提示。on=1 时底栏 session 槽位被一行提示文字覆盖，
 *  告诉用户"按 DOWN 到底回到实时流"；on=0 恢复正常 alias/sid 显示。
 *  由 bb_chat_transcript 在 follow-tail 锁存变化时调用，不需要外部驱动。 */
void bb_display_set_reading_hint(int on);

/** ADR-021-firmware-ui §1.2: butler cwd 用于 bottom_bar 左侧 "[B] <cwd>" 显示。
 *  NULL/"" 时显示 "[B]"（无 cwd）。 */
void bb_display_set_butler_cwd(const char* cwd);

/** ADR-021-firmware-ui §1.2: dispatch_status 帧注入顶部 s_lbl_status。
 *  phase: "started" | "done" | "async" | "error"
 *  cwd: 派发目标项目（started 时有效，可为 NULL）
 *  task_id: MCP tool_use id（async 时显示，可为 NULL）
 *  elapsed_ms: worker 耗时（done/async 时有效） */
void bb_display_set_dispatch_status(const char* phase, const char* cwd,
                                    const char* task_id, int64_t elapsed_ms);

/** ADR-021-firmware-ui §1.3: 记忆条数更新 footer 右侧 "mem: N+M" 显示。
 *  inbox=-1 或 profile=-1 时降级为 "mem: ?" */
void bb_display_set_mem_stats(int inbox, int profile);
