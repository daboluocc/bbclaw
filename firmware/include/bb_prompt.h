/**
 * Shared parser for ADR-033 blocking-prompt frames. Both transports — LAN
 * (bbwire/2 `prompt.open`) and cloud_saas (`voice.prompt.open` over WS /
 * `prompt.open` over the HTTP NDJSON stream) — carry the same structured menu
 * (promptId / question / options[{key,label,default}]). One parser keeps the
 * field handling in a single place.
 */
#ifndef BB_PROMPT_H
#define BB_PROMPT_H

#include "bb_adapter_client.h" /* bb_prompt_t */

#ifdef __cplusplus
extern "C" {
#endif

/** Parse a prompt.open JSON message into *out (cJSON). Handles both a flat object
 *  and one nested under "payload" (the cloud WS envelope). Caps options at
 *  BB_PROMPT_MAX_OPTIONS; an option with no key is skipped. Returns 1 when a
 *  usable menu (promptId + >=1 option) was parsed, else 0. *out is zeroed first. */
int bb_prompt_parse_open(const char* json, bb_prompt_t* out);

#ifdef __cplusplus
}
#endif

#endif /* BB_PROMPT_H */
