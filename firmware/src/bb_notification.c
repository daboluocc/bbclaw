#include "bb_notification.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "bb_agent_theme.h"
#include "bb_config.h"
#include "bb_transport.h"
#include "bb_wifi.h"
#include "esp_log.h"
#include "esp_websocket_client.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "lvgl.h"

static const char* TAG = "bb_notify";

/* ── Notification store ── */

typedef struct {
    bb_notification_t items[BB_NOTIFY_MAX];
    int count;
    int unread_total;
    SemaphoreHandle_t lock;
} bb_notification_store_t;

static bb_notification_store_t s_store = {0};

/* ── local_home WebSocket client ── */

static esp_websocket_client_handle_t s_ws_client = NULL;
static int s_ws_connected = 0;
static int s_initialized = 0;

/* Simple JSON string extractor (no cJSON dependency — keeps stack small).
 * Looks for "key":"value" and copies value into out. Returns 1 on success. */
static int json_extract(const char* json, const char* key, char* out, size_t out_len) {
    if (json == NULL || key == NULL || out == NULL || out_len == 0) return 0;
    char pattern[64];
    snprintf(pattern, sizeof(pattern), "\"%s\"", key);
    const char* p = strstr(json, pattern);
    if (p == NULL) return 0;
    p += strlen(pattern);
    while (*p == ' ' || *p == ':') p++;
    if (*p != '"') return 0;
    p++;
    size_t i = 0;
    while (*p != '\0' && *p != '"' && i < out_len - 1) {
        if (*p == '\\' && *(p + 1) != '\0') {
            p++;
        }
        out[i++] = *p++;
    }
    out[i] = '\0';
    return i > 0 ? 1 : 0;
}

/* ── Theme helpers ── */

static void update_theme_badge(void) {
    const bb_agent_theme_t* theme = bb_agent_theme_get_active();
    if (theme != NULL && theme->set_unread_count != NULL) {
        theme->set_unread_count(s_store.unread_total);
    }
}

static void show_theme_toast(const char* preview) {
    const bb_agent_theme_t* theme = bb_agent_theme_get_active();
    if (theme != NULL && theme->show_toast != NULL) {
        theme->show_toast(preview);
    }
}

typedef struct {
    char preview[48];
    int unread;
} notify_async_t;

static void on_notify_async(void* arg) {
    notify_async_t* a = (notify_async_t*)arg;
    if (a == NULL) return;
    update_theme_badge();
    if (a->preview[0] != '\0') {
        show_theme_toast(a->preview);
    }
    free(a);
}

/* ── local_home WS message handler ── */

static void handle_ws_text(const char* msg, int len) {
    if (msg == NULL || len <= 0) return;

    char type[24] = {0};
    char kind[48] = {0};
    json_extract(msg, "type", type, sizeof(type));
    if (strcmp(type, "event") != 0) return;

    json_extract(msg, "kind", kind, sizeof(kind));
    if (strcmp(kind, "session.notification") != 0) return;

    /* Find the nested "payload" object. */
    const char* payload_start = strstr(msg, "\"payload\"");
    if (payload_start == NULL) return;
    const char* brace = strchr(payload_start, '{');
    if (brace == NULL) return;

    char sid[64] = {0};
    char drv[24] = {0};
    char ntype[16] = {0};
    char preview[48] = {0};
    json_extract(brace, "sessionId", sid, sizeof(sid));
    json_extract(brace, "driver", drv, sizeof(drv));
    json_extract(brace, "type", ntype, sizeof(ntype));
    json_extract(brace, "preview", preview, sizeof(preview));

    if (sid[0] != '\0') {
        bb_notification_on_ws_event(sid, drv, ntype, preview);
    }
}

static void ws_event_handler(void* arg, esp_event_base_t base, int32_t event_id, void* data) {
    esp_websocket_event_data_t* ev = (esp_websocket_event_data_t*)data;
    (void)arg;
    (void)base;

    switch (event_id) {
        case WEBSOCKET_EVENT_CONNECTED:
            s_ws_connected = 1;
            ESP_LOGI(TAG, "local_home WS connected");
            break;
        case WEBSOCKET_EVENT_DISCONNECTED:
            s_ws_connected = 0;
            ESP_LOGI(TAG, "local_home WS disconnected (auto-reconnect)");
            break;
        case WEBSOCKET_EVENT_DATA:
            if (ev != NULL && ev->op_code == 0x01 && ev->data_len > 0) {
                handle_ws_text(ev->data_ptr, ev->data_len);
            }
            break;
        case WEBSOCKET_EVENT_ERROR:
            ESP_LOGW(TAG, "local_home WS error");
            break;
        default:
            break;
    }
}

static void build_adapter_ws_url(char* out, size_t out_len) {
    const char* base = BBCLAW_ADAPTER_BASE_URL;
    const char* scheme = "ws://";
    const char* host_start = base;

    if (strncmp(base, "https://", 8) == 0) {
        scheme = "wss://";
        host_start = base + 8;
    } else if (strncmp(base, "http://", 7) == 0) {
        scheme = "ws://";
        host_start = base + 7;
    }

    snprintf(out, out_len, "%s%s/ws?deviceId=local", scheme, host_start);
}

static esp_err_t start_local_ws(void) {
    if (s_ws_client != NULL) return ESP_OK;

    char ws_url[256] = {0};
    build_adapter_ws_url(ws_url, sizeof(ws_url));

    esp_websocket_client_config_t cfg = {
        .uri = ws_url,
        .buffer_size = 1024,
        .network_timeout_ms = 10000,
        .reconnect_timeout_ms = 3000,
        .task_stack = 4096,
        .disable_auto_reconnect = false,
        .task_name = "notif_ws",
    };

    s_ws_client = esp_websocket_client_init(&cfg);
    if (s_ws_client == NULL) {
        ESP_LOGE(TAG, "local_home WS init failed");
        return ESP_ERR_NO_MEM;
    }

    esp_websocket_register_events(s_ws_client, WEBSOCKET_EVENT_ANY, ws_event_handler, NULL);

    esp_err_t err = esp_websocket_client_start(s_ws_client);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "local_home WS start failed: %s", esp_err_to_name(err));
        esp_websocket_client_destroy(s_ws_client);
        s_ws_client = NULL;
        return err;
    }

    ESP_LOGI(TAG, "local_home WS started: %s", ws_url);
    return ESP_OK;
}

static void send_ws_ack(const char* session_id) {
    if (session_id == NULL || session_id[0] == '\0') return;

    if (bb_transport_is_cloud_saas()) {
        /* cloud_saas: ACK goes through the existing cloud WS in bb_adapter_client.
         * TODO: wire bb_adapter_client_ws_send() for ACK envelopes. */
        ESP_LOGI(TAG, "ack via cloud WS session=%s (TODO: wire adapter_client)", session_id);
        return;
    }

    /* local_home: send ACK on our own WS connection. */
    if (s_ws_client == NULL || !s_ws_connected) {
        ESP_LOGW(TAG, "ack: local WS not connected, skipping");
        return;
    }

    char msg[192];
    int len = snprintf(msg, sizeof(msg),
        "{\"type\":\"request\",\"kind\":\"session.notification.ack\","
        "\"payload\":{\"sessionId\":\"%s\"}}", session_id);

    if (esp_websocket_client_send_text(s_ws_client, msg, len, pdMS_TO_TICKS(1000)) < 0) {
        ESP_LOGW(TAG, "ack: WS send failed session=%s", session_id);
    } else {
        ESP_LOGI(TAG, "ack: sent session=%s", session_id);
    }
}

/* ── Public API ── */

esp_err_t bb_notification_init(void) {
    if (s_initialized) return ESP_OK;
    s_store.lock = xSemaphoreCreateMutex();
    if (s_store.lock == NULL) return ESP_ERR_NO_MEM;
    s_store.count = 0;
    s_store.unread_total = 0;
    s_initialized = 1;
    ESP_LOGI(TAG, "notification store initialized (max=%d)", BB_NOTIFY_MAX);

    /* local_home: start dedicated WS connection to adapter /ws.
     * cloud_saas: notifications arrive via the existing cloud WS in
     * bb_adapter_client.c — no extra connection needed.
     * Defer WS start — WiFi may not be ready at init time. The connection
     * will be established on first use or when auto-reconnect kicks in. */
    if (!bb_transport_is_cloud_saas()) {
        if (bb_wifi_is_connected()) {
            esp_err_t err = start_local_ws();
            if (err != ESP_OK) {
                ESP_LOGW(TAG, "local_home WS start deferred (adapter may not be up yet)");
            }
        } else {
            ESP_LOGI(TAG, "local_home WS deferred (WiFi not ready)");
        }
    }

    return ESP_OK;
}

static uint8_t parse_type(const char* type_str) {
    if (type_str == NULL) return 0;
    if (strcmp(type_str, "error") == 0) return 1;
    if (strcmp(type_str, "tool_approval") == 0) return 2;
    return 0;
}

void bb_notification_on_ws_event(const char* sid, const char* driver,
                                  const char* type, const char* preview) {
    if (!s_initialized || sid == NULL) return;

    if (xSemaphoreTake(s_store.lock, pdMS_TO_TICKS(100)) != pdTRUE) {
        ESP_LOGW(TAG, "lock timeout, dropping notification");
        return;
    }

    if (s_store.count >= BB_NOTIFY_MAX) {
        if (!s_store.items[0].read) s_store.unread_total--;
        memmove(&s_store.items[0], &s_store.items[1],
                sizeof(bb_notification_t) * (BB_NOTIFY_MAX - 1));
        s_store.count = BB_NOTIFY_MAX - 1;
    }

    bb_notification_t* n = &s_store.items[s_store.count];
    memset(n, 0, sizeof(*n));
    strncpy(n->session_id, sid, sizeof(n->session_id) - 1);
    if (driver) strncpy(n->driver, driver, sizeof(n->driver) - 1);
    if (preview) strncpy(n->preview, preview, sizeof(n->preview) - 1);
    n->type = parse_type(type);
    n->read = 0;
    s_store.count++;
    s_store.unread_total++;

    xSemaphoreGive(s_store.lock);

    ESP_LOGI(TAG, "notification: session=%s driver=%s type=%s unread=%d",
             sid, driver ? driver : "?", type ? type : "turn_end", s_store.unread_total);

    notify_async_t* a = (notify_async_t*)calloc(1, sizeof(*a));
    if (a != NULL) {
        if (preview) strncpy(a->preview, preview, sizeof(a->preview) - 1);
        a->unread = s_store.unread_total;
        lv_async_call(on_notify_async, a);
    }
}

int bb_notification_unread_count(void) {
    return s_store.unread_total;
}

int bb_notification_unread_for_session(const char* session_id) {
    if (!s_initialized || session_id == NULL) return 0;
    int count = 0;
    if (xSemaphoreTake(s_store.lock, pdMS_TO_TICKS(50)) != pdTRUE) return 0;
    for (int i = 0; i < s_store.count; ++i) {
        if (!s_store.items[i].read &&
            strcmp(s_store.items[i].session_id, session_id) == 0) {
            count++;
        }
    }
    xSemaphoreGive(s_store.lock);
    return count;
}

void bb_notification_mark_read(const char* session_id) {
    if (!s_initialized || session_id == NULL) return;
    if (xSemaphoreTake(s_store.lock, pdMS_TO_TICKS(100)) != pdTRUE) return;
    for (int i = 0; i < s_store.count; ++i) {
        if (!s_store.items[i].read &&
            strcmp(s_store.items[i].session_id, session_id) == 0) {
            s_store.items[i].read = 1;
            s_store.unread_total--;
        }
    }
    xSemaphoreGive(s_store.lock);
    update_theme_badge();
}

void bb_notification_ack(const char* session_id) {
    bb_notification_mark_read(session_id);
    send_ws_ack(session_id);
}
