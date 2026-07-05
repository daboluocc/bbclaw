set(_bb_srcs
    "bb_identity.c"
    "bb_audio.c"
    "bb_adapter_client.c"
    "bb_bbwire2.c"
    "bb_prompt.c"
    "bb_agent_client.c"
    "bb_agent_theme.c"
    "bb_button_test.c"
    "bb_cloud_client.c"
    "bb_device_config.c"
    "bb_device_monitor.c"
    "bb_ogg_opus.cpp"
    "bb_lvgl_assets.c"
    "bb_lvgl_display.c"
    "bb_lvgl_mem.c"
    "bb_page_standby.c"
    "bb_page_locked.c"
    "bb_page_boot.c"
    "bb_page_netconn.c"
    "bb_page_apconfig.c"
    "bb_page_ota.c"
    "bb_page_ota_confirm.c"
    "bb_page_prompt_select.c"
    "bb_chat_transcript.c"
    "bb_chat_cache.c"
    "bb_chat_recording.c"
    "bb_led.c"
    "bb_panel.c"
    "bb_power.c"
    "bb_ui_assets.c"
    "bb_gateway_node.c"
    "bb_motor.c"
    "bb_nav_input.c"
    "bb_notification.c"
    "bb_touch_input.c"
    "bb_ota.c"
    "bb_ptt.c"
    "bb_uart_cmd.c"
    "bb_radio_app.c"
    "bb_page_recorder.c"
    "bb_recorder.c"
    "bb_sdcard.c"
    "bb_session_store.c"
    "bb_state.c"
    "bb_theme_buddy_anim.c"
    "bb_time.c"
    "bb_ui_agent_chat.c"
    "bb_ui_task_list.c"
    "bb_ui_settings.c"
    "bb_transport.c"
    "bb_wifi.c"
    "bb_xl9555.c"
    "bb_display_control.c"
    "bb_display_control_co5300.c"
    "bb_sleep_manager.c"
    "bb_power_mgmt.c"
    "../drivers/qmi8658/qmi8658.c"
)

if(EXISTS "${CMAKE_CURRENT_SOURCE_DIR}/../generated/lv_font_bbclaw_cjk.c")
  list(APPEND _bb_srcs "../generated/lv_font_bbclaw_cjk.c")
endif()

if(EXISTS "${CMAKE_CURRENT_SOURCE_DIR}/../generated/bb_lvgl_screen_assets.c")
  list(APPEND _bb_srcs "../generated/bb_lvgl_screen_assets.c")
endif()

if(EXISTS "${CMAKE_CURRENT_SOURCE_DIR}/../generated/bb_lvgl_element_assets.c" AND
   NOT EXISTS "${CMAKE_CURRENT_SOURCE_DIR}/bb_lvgl_img_elements.c")
  list(APPEND _bb_srcs "../generated/bb_lvgl_element_assets.c")
endif()

if(EXISTS "${CMAKE_CURRENT_SOURCE_DIR}/bb_lvgl_img_elements.c")
  list(APPEND _bb_srcs "bb_lvgl_img_elements.c")
endif()

set(_bb_include_dirs "../include" "../boards" "../drivers")
if(EXISTS "${CMAKE_CURRENT_SOURCE_DIR}/../generated")
  list(APPEND _bb_include_dirs "../generated")
endif()

idf_component_register(
    SRCS ${_bb_srcs}
    INCLUDE_DIRS ${_bb_include_dirs}
    EMBED_FILES
        "../assets/svg/bbclaw.wav"
    EMBED_TXTFILES
        "../assets/isrg_root_x1.pem"
    REQUIRES
        esp_app_format
        esp_event
        esp_http_client
        esp_http_server
        esp_driver_i2c
        esp_driver_i2s
        esp_driver_gpio
        esp_driver_ledc
        esp_driver_spi
        esp_adc
        esp_lcd
        esp_netif
        esp_ringbuf
        app_update
        esp_partition
        esp_timer
        esp_wifi
        esp_websocket_client
        78__esp-opus
        78__esp-opus-encoder
        json
        mbedtls
        nvs_flash
        lvgl
        esp_lvgl_port
        esp_driver_rmt
        led_strip_encoder
        esp_tinyusb
        fatfs
        sdmmc
        esp_driver_sdmmc
)

# 新 ESP-IDF toolchain（GCC14）把 strncpy(dst, samesizebuf, sizeof-1) 以及有意截断
# 当 -Werror=stringop-truncation 拦下（本地旧 toolchain 不报，故本地绿/CI 红）。本
# 组件多处是「strncpy + 下一行手动补 \0」的安全截断（拷不下就截断），逐个改 snprintf
# 易漏。组件级把它降回告警（不改行为），与根 CMakeLists 对 opus 的 stringop-overflow
# 同策。见 v0.4.18 发布。
target_compile_options(${COMPONENT_LIB} PRIVATE -Wno-error=stringop-truncation)

if(EXISTS "${CMAKE_CURRENT_SOURCE_DIR}/../generated/lv_font_bbclaw_cjk.c")
  target_compile_definitions(${COMPONENT_LIB} PRIVATE BBCLAW_HAVE_CJK_FONT=1)
endif()

# lv_font_conv 生成的 .c 用 LV_LVGL_H_INCLUDE_SIMPLE 选择 "lvgl.h"（与 managed lvgl__lvgl 布局一致）
target_compile_definitions(${COMPONENT_LIB} PRIVATE LV_LVGL_H_INCLUDE_SIMPLE=1)

# LV_USE_CUSTOM_MALLOC：lvgl 组件库引用 bb_lvgl_mem.c 的 lv_*_core 实现，但
# 静态归档扫描顺序下 libsrc.a 的该 obj 没有被任何已解析符号牵出 → 链接失败。
# 用 -u 强制把这个 obj 拉进链接（一个符号即可整 obj 拉入）。
target_link_libraries(${COMPONENT_LIB} INTERFACE "-u lv_malloc_core")
