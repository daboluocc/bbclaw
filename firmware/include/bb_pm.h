/**
 * bb_pm — CPU/系统级低功耗（自动 light sleep + DFS），ADR-047。
 *
 * 与 bb_sleep_manager（ADR-046，只管屏幕）解耦但联动：息屏状态机是「用户是否空闲」
 * 的唯一真相源，本模块订阅其状态决定何时允许 SoC 进 light sleep。
 *
 * 门控：仅当 CONFIG_PM_ENABLE 且板级 BBCLAW_PM_LIGHT_SLEEP_ENABLE=1 时启用真实逻辑，
 * 其余板/配置下所有函数为 no-op（不引入任何运行时开销）。
 *
 * ⚠️ deep sleep 不在此模块——BBClaw 息屏待机仍需在线收云端 push（消息唤醒），只做
 * light sleep（保关联、保 RAM、微秒级恢复）。
 */
#ifndef BB_PM_H
#define BB_PM_H

#include "esp_err.h"

#ifdef __cplusplus
extern "C" {
#endif

/**
 * 初始化动态调频 + 自动 light sleep。
 * 配置 esp_pm（max/min 频率 + light_sleep_enable），并默认持有「交互锁」
 * （NO_LIGHT_SLEEP），使开机后处于全响应态，直到息屏状态机进入 SLEEPING 才释放。
 *
 * @return ESP_OK；未启用（no-op 构建）时也返回 ESP_OK。
 */
esp_err_t bb_pm_init(void);

/**
 * 由息屏状态联动：sleeping=1（SLEEPING）释放交互锁 → 允许 SoC 空闲即 light-sleep；
 * sleeping=0（ACTIVE/DIMMING/WAKING）持有交互锁 → 禁 light sleep，保持 UI/音频跟手。
 * 幂等，安全在 stream_task tick 每轮调用。
 */
void bb_pm_set_sleeping(int sleeping);

/** 打印 esp_pm 当前配置与本模块锁状态，用于排障。 */
void bb_pm_dump_status(void);

#ifdef __cplusplus
}
#endif

#endif /* BB_PM_H */
