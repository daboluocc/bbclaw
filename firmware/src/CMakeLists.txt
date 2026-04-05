set(_bb_srcs
    "bb_identity.c"
    "bb_audio.c"
    "bb_adapter_client.c"
    "bb_button_test.c"
    "bb_cloud_client.c"
    "bb_ogg_opus.cpp"
    "bb_lvgl_assets.c"
    "bb_lvgl_display.c"
    "bb_led.c"
    "bb_panel.c"
    "bb_ui_assets.c"
    "bb_gateway_node.c"
    "bb_motor.c"
    "bb_ptt.c"
    "bb_radio_app.c"
    "bb_time.c"
    "bb_transport.c"
    "bb_wifi.c"
    "bb_xl9555.c"
)

if(EXISTS "${CMAKE_CURRENT_SOURCE_DIR}/../generated/lv_font_bbclaw_cjk.c")
  list(APPEND _bb_srcs "../generated/lv_font_bbclaw_cjk.c")
endif()

if(EXISTS "${CMAKE_CURRENT_SOURCE_DIR}/../generated/bb_lvgl_screen_assets.c")
  list(APPEND _bb_srcs "../generated/bb_lvgl_screen_assets.c")
endif()

if(EXISTS "${CMAKE_CURRENT_SOURCE_DIR}/../generated/bb_lvgl_element_assets.c")
  list(APPEND _bb_srcs "../generated/bb_lvgl_element_assets.c")
endif()

idf_component_register(
    SRCS ${_bb_srcs}
    INCLUDE_DIRS "../include" "../generated" "../boards"
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
        esp_lcd
        esp_netif
        esp_timer
        esp_wifi
        esp_websocket_client
        78__esp-opus
        78__esp-opus-encoder
        mbedtls
        nvs_flash
        lvgl
        esp_lvgl_port
)

if(EXISTS "${CMAKE_CURRENT_SOURCE_DIR}/../generated/lv_font_bbclaw_cjk.c")
  target_compile_definitions(${COMPONENT_LIB} PRIVATE BBCLAW_HAVE_CJK_FONT=1)
endif()

# lv_font_conv 生成的 .c 用 LV_LVGL_H_INCLUDE_SIMPLE 选择 "lvgl.h"（与 managed lvgl__lvgl 布局一致）
target_compile_definitions(${COMPONENT_LIB} PRIVATE LV_LVGL_H_INCLUDE_SIMPLE=1)
