#include "bb_ota.h"

#include <string.h>
#include <sys/stat.h>

#include "esp_app_desc.h"
#include "esp_err.h"
#include "esp_ota_ops.h"
#include "nvs_flash.h"
#include "esp_crt_bundle.h"
#include "esp_http_client.h"
#include "esp_log.h"
#include "esp_partition.h"
#include "esp_system.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

#include "bb_config.h"

static const char* TAG = "bb_ota";

// NVS 命名空间与键（OTA 状态持久化）
#define OTA_NVS_NAMESPACE     "ota"
#define OTA_NVS_KEY_JUST_UPD  "just_upd"   // 升级完成庆祝标志
#define OTA_NVS_KEY_LAST_TRY  "last_try"   // 上次「已烧录并重启」尝试的目标版本

// OTA 状态
static ota_state_t s_state = OTA_STATE_IDLE;

// 当前待升级信息
static ota_update_info_t s_pending_update;

// HTTP 响应缓冲区
typedef struct {
    char* body;
    size_t offset;
    size_t max_size;
} ota_http_buf_t;

// OTA 写入状态
typedef struct {
    const esp_partition_t* update_partition;
    esp_ota_handle_t update_handle;
    size_t total_written;
    bool invalid;
} ota_write_ctx_t;

static esp_err_t ota_http_event_handler(esp_http_client_event_t* evt) {
    switch (evt->event_id) {
        case HTTP_EVENT_ON_DATA:
            if (evt->user_data && evt->data && evt->data_len > 0) {
                ota_http_buf_t* buf = (ota_http_buf_t*)evt->user_data;
                size_t remain = buf->max_size - buf->offset - 1;
                size_t copy_len = evt->data_len < remain ? evt->data_len : remain;
                memcpy(buf->body + buf->offset, evt->data, copy_len);
                buf->offset += copy_len;
                buf->body[buf->offset] = '\0';
            }
            break;
        default:
            break;
    }
    return ESP_OK;
}

// 读取上次尝试升级的目标版本（issue #179 死循环护栏）。
// 成功返回 ESP_OK 且 out 填入版本字符串；无记录或出错返回非 ESP_OK。
static esp_err_t ota_nvs_get_last_try(char* out, size_t out_size) {
    if (out == NULL || out_size == 0) {
        return ESP_ERR_INVALID_ARG;
    }
    out[0] = '\0';
    nvs_handle_t nvsh;
    if (nvs_open(OTA_NVS_NAMESPACE, NVS_READONLY, &nvsh) != ESP_OK) {
        return ESP_FAIL;
    }
    size_t len = out_size;
    esp_err_t err = nvs_get_str(nvsh, OTA_NVS_KEY_LAST_TRY, out, &len);
    nvs_close(nvsh);
    return err;
}

// 记录本次「已烧录并准备重启」尝试的目标版本（issue #179 死循环护栏）。
static void ota_nvs_set_last_try(const char* version) {
    if (version == NULL || version[0] == '\0') {
        return;
    }
    nvs_handle_t nvsh;
    if (nvs_open(OTA_NVS_NAMESPACE, NVS_READWRITE, &nvsh) == ESP_OK) {
        nvs_set_str(nvsh, OTA_NVS_KEY_LAST_TRY, version);
        nvs_commit(nvsh);
        nvs_close(nvsh);
        ESP_LOGI(TAG, "OTA NVS last_try set to %s", version);
    }
}

ota_state_t bb_ota_get_state(void) {
    return s_state;
}

const char* bb_ota_get_state_str(ota_state_t state) {
    switch (state) {
        case OTA_STATE_IDLE:         return "IDLE";
        case OTA_STATE_CHECKING:     return "CHECKING";
        case OTA_STATE_AVAILABLE:    return "AVAILABLE";
        case OTA_STATE_DOWNLOADING:  return "DOWNLOADING";
        case OTA_STATE_VERIFYING:    return "VERIFYING";
        case OTA_STATE_BOOTING:      return "BOOTING";
        default:                     return "UNKNOWN";
    }
}

const char* bb_ota_get_current_version(void) {
    const esp_app_desc_t* app = esp_app_get_description();
    return (app && app->version[0]) ? app->version : "unknown";
}

esp_err_t bb_ota_init(void) {
    ESP_LOGI(TAG, "OTA init, current version: %s", bb_ota_get_current_version());
    s_state = OTA_STATE_IDLE;
    memset(&s_pending_update, 0, sizeof(s_pending_update));
    return ESP_OK;
}

esp_err_t bb_ota_check(ota_update_info_t* info) {
    return bb_ota_check_ex(info, /*manual_check=*/false);
}

esp_err_t bb_ota_check_ex(ota_update_info_t* info, bool manual_check) {
    // dev/dirty 护栏已取消（用户要求：dev/dirty 构建也要能 OTA 升级）。原来 make flash 的
    // dev 构建（版本带 "-dirty" / "-<N>-g<hash>"）会在开机自动检查里被短路跳过，导致测试
    // 设备永远拉不到 OTA；现在是否升级一律由云端 /v1/ota/check 决定——固件侧不再做 OTA
    // eligibility 判断（原则：OTA 判断只放后台，后台可随时改，固件改不动）。
    // 注意取舍：dev 版 M.m.p 常落后于 active，会每次开机被判「有更新」并弹确认页——但只是
    // 确认页（按 OK 才下载/烧录），可以拒，不会静默覆盖你正在调的固件。
    if (info == NULL) {
        return ESP_ERR_INVALID_ARG;
    }

    memset(info, 0, sizeof(*info));

    s_state = OTA_STATE_CHECKING;

    // #179 防砖死循环护栏已迁到 cloud:判断放后台,固件只「上报事实」。开机/自动路径
    // (manual_check=false,跑在内部 RAM 栈,读 NVS 安全)把 last_try——「上次真正烧录并
    // 重启进去的目标版本」(bb_ota_apply_update 里 set,只有 reboot 那一刻才置位)——作为
    // 事实带给 cloud,由 cloud 决定要不要退避。菜单路径(manual_check=true,PSRAM 栈不能读
    // NVS,否则 cache-disabled assert 重启)不带 last_try,cloud 因此对它不启用护栏——与
    // 旧固件「菜单路径跳过护栏」语义一致。
    char last_try[sizeof(info->version)] = {0};
    if (!manual_check) {
        (void)ota_nvs_get_last_try(last_try, sizeof(last_try));
    }

    char url[320];
    if (last_try[0] != '\0') {
        snprintf(url, sizeof(url),
                 "%s/v1/ota/check?device_id=%s&current_version=%s&platform=" BBCLAW_OTA_PLATFORM "&last_try=%s",
                 BBCLAW_CLOUD_BASE_URL, BBCLAW_DEVICE_ID, bb_ota_get_current_version(), last_try);
    } else {
        snprintf(url, sizeof(url), "%s/v1/ota/check?device_id=%s&current_version=%s&platform=" BBCLAW_OTA_PLATFORM,
                 BBCLAW_CLOUD_BASE_URL, BBCLAW_DEVICE_ID, bb_ota_get_current_version());
    }

    ESP_LOGI(TAG, "OTA check: %s", url);

    char response_body[2048] = {0};
    ota_http_buf_t buf = {
        .body = response_body,
        .offset = 0,
        .max_size = sizeof(response_body) - 1,
    };

    esp_http_client_config_t cfg = {
        .url = url,
        .timeout_ms = BBCLAW_HTTP_TIMEOUT_MS,
        .method = HTTP_METHOD_GET,
        .event_handler = ota_http_event_handler,
        .user_data = &buf,
    };

    if (strncasecmp(url, "https", 5) == 0) {
        cfg.crt_bundle_attach = esp_crt_bundle_attach;
    }

    esp_http_client_handle_t client = esp_http_client_init(&cfg);
    if (client == NULL) {
        ESP_LOGE(TAG, "HTTP client init failed");
        s_state = OTA_STATE_IDLE;
        return ESP_FAIL;
    }

    esp_err_t err = esp_http_client_perform(client);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "OTA check request failed: %s", esp_err_to_name(err));
        esp_http_client_cleanup(client);
        s_state = OTA_STATE_IDLE;
        return err;
    }

    int status_code = esp_http_client_get_status_code(client);
    ESP_LOGI(TAG, "OTA HTTP status: %d", status_code);
    esp_http_client_cleanup(client);

    if (status_code != 200) {
        ESP_LOGW(TAG, "OTA check response status: %d", status_code);
        s_state = OTA_STATE_IDLE;
        return ESP_FAIL;
    }

    // 解析 JSON 响应
    // {"ok":true,"data":{"hasUpdate":true,"version":"1.1.0",...}}
    char* body = response_body;
    ESP_LOGI(TAG, "OTA response body (first 300): %.300s", body);
    if (body[0] == '{') {
        // Simple JSON parsing without cJSON
        char* p = body;

        // Find "hasUpdate":true
        if ((p = strstr(p, "\"hasUpdate\":")) != NULL) {
            p += 12;  // skip "\"hasUpdate\":"
            while (*p == ' ') p++;
            ESP_LOGI(TAG, "OTA hasUpdate value: '%.8s'", p);
            if (strncmp(p, "true", 4) == 0) {
                info->has_update = true;
                ESP_LOGI(TAG, "OTA parsed has_update=1");
            }
        } else {
            ESP_LOGW(TAG, "OTA hasUpdate field not found in response");
        }

        if (info->has_update) {
            // Parse version
            if ((p = strstr(body, "\"version\":")) != NULL) {
                p += 10;
                while (*p == ' ' || *p == '"') p++;
                char* end = strchr(p, '"');
                if (end) {
                    size_t len = end - p < sizeof(info->version) - 1 ? end - p : sizeof(info->version) - 1;
                    strncpy(info->version, p, len);
                    info->version[len] = '\0';
                }
            }

            // Parse downloadURL
            if ((p = strstr(body, "\"downloadURL\":")) != NULL) {
                p += 14;
                while (*p == ' ' || *p == '"') p++;
                char* end = strchr(p, '"');
                if (end) {
                    size_t len = end - p < sizeof(info->download_url) - 1 ? end - p : sizeof(info->download_url) - 1;
                    strncpy(info->download_url, p, len);
                    info->download_url[len] = '\0';
                }
            }

            // Parse size
            if ((p = strstr(body, "\"size\":")) != NULL) {
                p += 7;
                info->size = atoi(p);
            }

            // Parse checksum
            if ((p = strstr(body, "\"checksum\":")) != NULL) {
                p += 11;
                while (*p == ' ' || *p == '"') p++;
                char* end = strchr(p, '"');
                if (end) {
                    size_t len = end - p < sizeof(info->checksum) - 1 ? end - p : sizeof(info->checksum) - 1;
                    strncpy(info->checksum, p, len);
                    info->checksum[len] = '\0';
                }
            }

            // Parse releaseNotes
            if ((p = strstr(body, "\"releaseNotes\":")) != NULL) {
                p += 15;
                while (*p == ' ' || *p == '"') p++;
                char* end = strchr(p, '"');
                if (end) {
                    size_t len = end - p < sizeof(info->release_notes) - 1 ? end - p : sizeof(info->release_notes) - 1;
                    strncpy(info->release_notes, p, len);
                    info->release_notes[len] = '\0';
                }
            }
        }
    }

    // #179 防砖死循环护栏已迁到 cloud:固件在 check 请求里带 last_try(见上方 URL 构建),
    // 由 cloud 判定要不要退避(cloud/internal/ota/handler.go HandleOTACheck +
    // loopGuardSuppress)。固件侧不再做这个判断——判断只放后台、后台可随时改;固件只
    // 「上报事实」(current_version + last_try)。

    ESP_LOGI(TAG, "OTA check result: has_update=%d version=%s",
             info->has_update, info->has_update ? info->version : "N/A");

    s_state = info->has_update ? OTA_STATE_AVAILABLE : OTA_STATE_IDLE;
    return ESP_OK;
}

esp_err_t bb_ota_download_and_flash(ota_update_info_t* info, ota_progress_cb progress) {
    if (info == NULL || !info->has_update || info->download_url[0] == '\0') {
        return ESP_ERR_INVALID_ARG;
    }

    s_state = OTA_STATE_DOWNLOADING;

    char url[384];
    if (strncmp(info->download_url, "/", 1) == 0) {
        // Relative URL, prepend cloud base
        snprintf(url, sizeof(url), "%s%s", BBCLAW_CLOUD_BASE_URL, info->download_url);
    } else {
        strncpy(url, info->download_url, sizeof(url) - 1);
        url[sizeof(url) - 1] = '\0';
    }

    ESP_LOGI(TAG, "OTA download: %s (size=%u)", url, info->size);

    // 获取 OTA 分区
    const esp_partition_t* update_partition = esp_ota_get_next_update_partition(NULL);
    if (update_partition == NULL) {
        ESP_LOGE(TAG, "No OTA partition found");
        s_state = OTA_STATE_IDLE;
        return ESP_FAIL;
    }
    ESP_LOGI(TAG, "OTA partition: %s @ 0x%x (size=%d)",
             update_partition->label, update_partition->address, update_partition->size);

    // 开始 OTA
    esp_ota_handle_t update_handle;
    const esp_partition_t* boot_partition = esp_ota_get_boot_partition();
    ESP_LOGI(TAG, "Boot partition: %s @ 0x%x", boot_partition->label, boot_partition->address);

    esp_err_t err = esp_ota_begin(update_partition, info->size, &update_handle);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "esp_ota_begin failed: %s", esp_err_to_name(err));
        s_state = OTA_STATE_IDLE;
        return err;
    }

    // 准备 HTTP 请求
    char response_body[64] = {0};  // 下载不需要大缓冲区
    ota_http_buf_t buf = {
        .body = response_body,
        .offset = 0,
        .max_size = sizeof(response_body) - 1,
    };

    esp_http_client_config_t http_cfg = {
        .url = url,
        .timeout_ms = 120000,  // 2min timeout for large files
        .method = HTTP_METHOD_GET,
        .event_handler = ota_http_event_handler,
        .user_data = &buf,
    };

    if (strncasecmp(url, "https", 5) == 0) {
        http_cfg.crt_bundle_attach = esp_crt_bundle_attach;
    }

    esp_http_client_handle_t client = esp_http_client_init(&http_cfg);
    if (client == NULL) {
        ESP_LOGE(TAG, "HTTP client init failed");
        esp_ota_abort(update_handle);
        s_state = OTA_STATE_IDLE;
        return ESP_FAIL;
    }

    err = esp_http_client_open(client, 0);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "HTTP client open failed: %s", esp_err_to_name(err));
        esp_http_client_cleanup(client);
        esp_ota_abort(update_handle);
        s_state = OTA_STATE_IDLE;
        return err;
    }

    int content_length = esp_http_client_fetch_headers(client);
    if (content_length <= 0) {
        content_length = info->size;
    }

    ESP_LOGI(TAG, "OTA content length: %d", content_length);

    // 下载并写入
    char chunk[4096];
    int total_read = 0;
    int last_percent = -1;

    while (1) {
        int read_len = esp_http_client_read(client, chunk, sizeof(chunk));
        if (read_len < 0) {
            ESP_LOGE(TAG, "HTTP read error");
            esp_http_client_cleanup(client);
            esp_ota_abort(update_handle);
            s_state = OTA_STATE_IDLE;
            return ESP_FAIL;
        } else if (read_len == 0) {
            break;
        }

        err = esp_ota_write(update_handle, chunk, read_len);
        if (err != ESP_OK) {
            ESP_LOGE(TAG, "esp_ota_write failed: %s", esp_err_to_name(err));
            esp_http_client_cleanup(client);
            esp_ota_abort(update_handle);
            s_state = OTA_STATE_IDLE;
            return err;
        }

        total_read += read_len;
        int percent = (total_read * 100) / content_length;
        if (percent != last_percent && progress) {
            progress(percent);
            last_percent = percent;
        }

        if (total_read % (256 * 1024) == 0) {
            ESP_LOGI(TAG, "OTA progress: %d/%d bytes (%d%%)", total_read, content_length, percent);
        }
    }

    esp_http_client_cleanup(client);

    if (progress) {
        progress(100);
    }

    ESP_LOGI(TAG, "OTA download complete: %d bytes", total_read);

    // 完成 OTA
    err = esp_ota_end(update_handle);
    if (err != ESP_OK) {
        if (err == ESP_ERR_OTA_VALIDATE_FAILED) {
            ESP_LOGE(TAG, "OTA image validation failed!");
        } else {
            ESP_LOGE(TAG, "esp_ota_end failed: %s", esp_err_to_name(err));
        }
        s_state = OTA_STATE_IDLE;
        return err;
    }

    // 保存待更新的分区信息
    memcpy(&s_pending_update, info, sizeof(s_pending_update));
    s_state = OTA_STATE_VERIFYING;

    ESP_LOGI(TAG, "OTA verification passed");
    return ESP_OK;
}

esp_err_t bb_ota_apply_update(void) {
    const esp_partition_t* update_partition = esp_ota_get_next_update_partition(NULL);
    if (update_partition == NULL) {
        ESP_LOGE(TAG, "No OTA partition found");
        return ESP_FAIL;
    }

    ESP_LOGI(TAG, "Setting boot partition to %s @ 0x%x", update_partition->label, update_partition->address);

    s_state = OTA_STATE_BOOTING;

    /* Mark "just updated" flag in NVS so first-boot celebration can be shown */
    nvs_handle_t nvsh;
    if (nvs_open(OTA_NVS_NAMESPACE, NVS_READWRITE, &nvsh) == ESP_OK) {
        nvs_set_u32(nvsh, OTA_NVS_KEY_JUST_UPD, 1);
        nvs_commit(nvsh);
        nvs_close(nvsh);
        ESP_LOGI(TAG, "OTA NVS just_upd flag set");
    }

    /* Record the version we are about to flash+reboot into. The next boot's OTA
     * check reports it to the cloud as last_try (see bb_ota_check_ex URL build); the
     * cloud uses it to detect a reflash loop when the running version doesn't change
     * (issue #179 — the guard DECISION now lives on the cloud, not here). Only
     * versions that actually reach reboot are recorded — transient download failures
     * never call this function, so decline/timeout never set last_try.
     * Intentionally never cleared: once you're actually running it the cloud's
     * current==offered check naturally stops mattering, and matching the exact
     * flashed version is the whole point of the guard. */
    ota_nvs_set_last_try(s_pending_update.version);

    // 标记待启动的分区
    esp_err_t err = esp_ota_set_boot_partition(update_partition);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "esp_ota_set_boot_partition failed: %s", esp_err_to_name(err));
        s_state = OTA_STATE_IDLE;
        return err;
    }

    ESP_LOGI(TAG, "OTA update scheduled. Restarting in 2 seconds...");
    vTaskDelay(pdMS_TO_TICKS(2000));
    esp_restart();

    // Never reaches here
    return ESP_OK;
}

int bb_ota_was_just_updated(void) {
    nvs_handle_t nvsh;
    if (nvs_open(OTA_NVS_NAMESPACE, NVS_READONLY, &nvsh) != ESP_OK) {
        return 0;
    }
    uint32_t val = 0;
    nvs_get_u32(nvsh, OTA_NVS_KEY_JUST_UPD, &val);
    if (val == 1) {
        nvs_erase_key(nvsh, OTA_NVS_KEY_JUST_UPD);
        nvs_commit(nvsh);
        ESP_LOGI(TAG, "OTA celebration flag cleared");
    }
    nvs_close(nvsh);
    return (int)val;
}

esp_err_t bb_ota_check_silent(void) {
    ota_update_info_t info;
    esp_err_t err = bb_ota_check(&info);
    if (err != ESP_OK) {
        return err;
    }

    if (info.has_update) {
        ESP_LOGI(TAG, "Silent OTA check: update available version=%s", info.version);
        // Store for later user confirmation
        memcpy(&s_pending_update, &info, sizeof(info));
        s_state = OTA_STATE_AVAILABLE;
    }

    return ESP_OK;
}
