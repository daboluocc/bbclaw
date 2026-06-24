/**
 * Blocking-prompt confirm page (ADR-033) — on-device approval of claude's
 * tool/permission menus forwarded by the adapter (bbwire/2 prompt.open).
 *
 * Shows the question + the option rows; UP/DOWN move the selection, OK confirms
 * the highlighted row, BACK denies. The selection STARTS on the deny option (the
 * safe default — an accidental OK must not approve a destructive tool; ADR-033
 * §11). A countdown auto-denies if the user doesn't decide, so the device is
 * never stuck (the adapter's own PromptTimeout is the further backstop).
 *
 * Visual language matches bb_page_ota_confirm (dot palette, lv_layer_top,
 * synchronous show/dismiss). Input is routed by bb_radio_app.c via
 * bb_page_prompt_select_active() + the nav helpers below. All calls take the
 * LVGL port lock internally; callers hold no lock.
 */
#ifndef BB_PAGE_PROMPT_SELECT_H
#define BB_PAGE_PROMPT_SELECT_H

#include "bb_adapter_client.h" /* bb_prompt_t */

#ifdef __cplusplus
extern "C" {
#endif

/** Decision callback, fired exactly once. option_key is the chosen menu option's
 *  key (e.g. "1") for the adapter to inject; on deny/timeout it is the deny
 *  option's key (claude's last option, conventionally "No"). Never NULL while a
 *  menu has options. */
typedef void (*bb_page_prompt_select_cb_t)(const char* prompt_id, const char* option_key);

/** Create the page on lv_layer_top() from a COPY of *prompt (the caller's struct
 *  may be transient). No-op if already shown. cb fires once on decision/timeout. */
void bb_page_prompt_select_show(const bb_prompt_t* prompt, bb_page_prompt_select_cb_t cb);

/** Destroy the page synchronously WITHOUT firing the callback — used when the
 *  adapter closes the menu out-of-band (prompt.close). No-op if not shown. */
void bb_page_prompt_select_dismiss(void);

/** 1 while the page object exists (gate nav routing). */
int bb_page_prompt_select_active(void);

/** 1 if the live page is showing prompt_id (so a prompt.close matches the right
 *  menu and doesn't dismiss a newer one). */
int bb_page_prompt_select_active_id_is(const char* prompt_id);

/** 1 if the countdown expired; the main loop should then call
 *  bb_page_prompt_select_handle_nav(0) to deny + fire the callback. */
int bb_page_prompt_select_timed_out(void);

/** Move the selection: delta < 0 = UP, delta > 0 = DOWN. No-op if not active. */
void bb_page_prompt_select_nav_move(int delta);

/** Decide: nav_ok == 1 → confirm the highlighted option; nav_ok == 0 → deny
 *  (send the deny option's key). Destroys the page, fires the callback outside
 *  the LVGL lock. No-op if not active. */
void bb_page_prompt_select_handle_nav(int nav_ok);

#ifdef __cplusplus
}
#endif

#endif /* BB_PAGE_PROMPT_SELECT_H */
