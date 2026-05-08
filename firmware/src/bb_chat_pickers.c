/**
 * CHAT pickers (Phase 6 skeleton).
 * Real implementation currently in bb_ui_agent_chat.c. When Phase 7 moves
 * chat to a native full-screen view, the picker LVGL objects migrate here;
 * the async fetch + NVS logic stays in bb_ui_agent_chat.c.
 */
#include "bb_chat_pickers.h"

#include "bb_ui_agent_chat.h"

/* For now, forward directly to existing bb_ui_agent_chat_* APIs so calling
 * sites can switch to the new naming without changing behavior. */

void bb_chat_picker_session_show(void)        { bb_ui_agent_chat_session_picker_show(); }
void bb_chat_picker_session_hide(void)        { bb_ui_agent_chat_session_picker_hide(); }
void bb_chat_picker_session_move(int delta)   { bb_ui_agent_chat_session_picker_move(delta); }
int  bb_chat_picker_session_select(void)      { return bb_ui_agent_chat_session_picker_select(); }
int  bb_chat_picker_session_is_visible(void)  { return bb_ui_agent_chat_session_picker_is_visible(); }

void bb_chat_picker_cwd_hide(void)            { /* no-op: CWD picker is hide via confirm/cancel */ }
void bb_chat_picker_cwd_show(void)            { /* triggered internally by session picker → "+ 新建" */ }
void bb_chat_picker_cwd_move(int delta)       { bb_ui_agent_chat_cwd_picker_move(delta); }
void bb_chat_picker_cwd_confirm(void)         { bb_ui_agent_chat_cwd_picker_confirm(); }
void bb_chat_picker_cwd_cancel(void)          { bb_ui_agent_chat_cwd_picker_cancel(); }
int  bb_chat_picker_cwd_is_visible(void)      { return bb_ui_agent_chat_cwd_picker_is_visible(); }

esp_err_t bb_chat_picker_cycle_driver(int delta) {
  return bb_ui_agent_chat_cycle_driver(delta);
}
