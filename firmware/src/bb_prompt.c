#include "bb_prompt.h"

#include <stdio.h>
#include <string.h>

#include "cJSON.h"

int bb_prompt_parse_open(const char* json, bb_prompt_t* out) {
  if (json == NULL || out == NULL) {
    return 0;
  }
  memset(out, 0, sizeof(*out));
  cJSON* root = cJSON_Parse(json);
  if (root == NULL) {
    return 0;
  }
  /* The LAN bbwire/2 frame is flat; the cloud WS envelope nests the fields under
   * "payload". Accept either. */
  cJSON* obj = root;
  cJSON* payload = cJSON_GetObjectItem(root, "payload");
  if (cJSON_IsObject(payload)) {
    obj = payload;
  }

  const cJSON* j;
  if ((j = cJSON_GetObjectItem(obj, "promptId")) != NULL && cJSON_IsString(j)) {
    snprintf(out->prompt_id, sizeof(out->prompt_id), "%s", j->valuestring);
  }
  if ((j = cJSON_GetObjectItem(obj, "kind")) != NULL && cJSON_IsString(j)) {
    snprintf(out->kind, sizeof(out->kind), "%s", j->valuestring);
  }
  if ((j = cJSON_GetObjectItem(obj, "question")) != NULL && cJSON_IsString(j)) {
    snprintf(out->question, sizeof(out->question), "%s", j->valuestring);
  }
  const cJSON* opts = cJSON_GetObjectItem(obj, "options");
  if (cJSON_IsArray(opts)) {
    const cJSON* o;
    cJSON_ArrayForEach(o, opts) {
      if (out->n_options >= BB_PROMPT_MAX_OPTIONS) {
        break;
      }
      bb_prompt_option_t* dst = &out->options[out->n_options];
      const cJSON* f;
      if ((f = cJSON_GetObjectItem(o, "key")) != NULL && cJSON_IsString(f)) {
        snprintf(dst->key, sizeof(dst->key), "%s", f->valuestring);
      }
      if ((f = cJSON_GetObjectItem(o, "label")) != NULL && cJSON_IsString(f)) {
        snprintf(dst->label, sizeof(dst->label), "%s", f->valuestring);
      }
      if ((f = cJSON_GetObjectItem(o, "default")) != NULL) {
        dst->is_default = cJSON_IsTrue(f) ? 1 : 0;
      }
      if (dst->key[0] != '\0') {
        out->n_options++;
      }
    }
  }
  cJSON_Delete(root);
  return (out->prompt_id[0] != '\0' && out->n_options > 0) ? 1 : 0;
}
