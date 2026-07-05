/**
 * bb_panic_crumb.c — panic 现场 RTC 快照（本板 panic 输出不可达的终极替代）。
 *
 * linker --wrap=esp_panic_handler:进真正 panic 处理前把 PC/EXCCAUSE/任务名
 * 写进 RTC noinit,重启后 boot report 读出 → host 侧 addr2line 定位到行。
 */
#include <string.h>

#include "esp_attr.h"
#include "esp_private/panic_internal.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "xtensa_context.h"

RTC_NOINIT_ATTR uint32_t g_bb_panic_pc;
RTC_NOINIT_ATTR uint32_t g_bb_panic_cause;
RTC_NOINIT_ATTR char g_bb_panic_task[16];

extern void __real_esp_panic_handler(panic_info_t* info);

void __wrap_esp_panic_handler(panic_info_t* info) {
  if (info != NULL && info->frame != NULL) {
    const XtExcFrame* f = (const XtExcFrame*)info->frame;
    g_bb_panic_pc = (uint32_t)f->pc;
    g_bb_panic_cause = (uint32_t)f->exccause;
  }
  const char* name = pcTaskGetName(NULL);
  if (name != NULL) {
    strncpy(g_bb_panic_task, name, sizeof(g_bb_panic_task) - 1);
    g_bb_panic_task[sizeof(g_bb_panic_task) - 1] = '\0';
  }
  __real_esp_panic_handler(info);
}
