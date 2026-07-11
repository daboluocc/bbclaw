# CPU/系统级低功耗 overlay（ADR-047）：自动 light sleep + DFS + tickless idle。
# 叠在板级/生产 config 之后，仅用于「想开 CPU light-sleep」的本地测试构建，例如：
#   idf.py -DSDKCONFIG_DEFAULTS="sdkconfig.bbclaw.latest;sdkconfig.pm" build          # bbclaw 生产板 + PM
#   idf.py -DSDKCONFIG_DEFAULTS="sdkconfig.defaults;boards/<b>/sdkconfig.board;sdkconfig.cloud;sdkconfig.pm" build
# ⚠️ 故意不进 sdkconfig.bbclaw.latest（OTA 生产配置）——CPU light-sleep 影响面大，
#    需真机功耗/音频/USB 验证充分后，才由人决定是否推 OTA。运行时由 bb_pm 显式
#    esp_pm_configure(240↔80MHz, light_sleep) + NO_LIGHT_SLEEP 交互锁驱动，见 ADR-047。
CONFIG_PM_ENABLE=y
CONFIG_FREERTOS_USE_TICKLESS_IDLE=y
CONFIG_PM_DFS_INIT_AUTO=y
# 保守：light sleep 里不断 CPU 电（与 Octal PSRAM 共存更稳），仅时钟门控。
# CONFIG_PM_POWER_DOWN_CPU_IN_LIGHT_SLEEP is not set
CONFIG_ESP_WIFI_STA_DISCONNECTED_PM_ENABLE=y
