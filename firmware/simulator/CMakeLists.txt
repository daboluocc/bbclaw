cmake_minimum_required(VERSION 3.20)

project(bbclaw_lvgl_sim LANGUAGES C CXX)

set(CMAKE_C_STANDARD 11)
set(CMAKE_C_STANDARD_REQUIRED ON)
set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

find_package(PkgConfig REQUIRED)
pkg_check_modules(SDL2 REQUIRED IMPORTED_TARGET sdl2)

set(LV_BUILD_CONF_DIR "${CMAKE_CURRENT_SOURCE_DIR}" CACHE PATH "" FORCE)
set(CONFIG_LV_BUILD_DEMOS OFF CACHE BOOL "" FORCE)
set(CONFIG_LV_BUILD_EXAMPLES OFF CACHE BOOL "" FORCE)
set(CONFIG_LV_USE_THORVG_INTERNAL OFF CACHE BOOL "" FORCE)

add_subdirectory(
  "${CMAKE_CURRENT_SOURCE_DIR}/../managed_components/lvgl__lvgl"
  "${CMAKE_CURRENT_BINARY_DIR}/lvgl"
)

target_include_directories(lvgl PUBLIC ${SDL2_INCLUDE_DIRS})
target_link_libraries(lvgl PUBLIC PkgConfig::SDL2)

set(_sim_srcs
  "${CMAKE_CURRENT_SOURCE_DIR}/main.c"
  "${CMAKE_CURRENT_SOURCE_DIR}/bb_time_stub.c"
  "${CMAKE_CURRENT_SOURCE_DIR}/bb_wifi_stub.c"
  "${CMAKE_CURRENT_SOURCE_DIR}/../src/bb_lvgl_assets.c"
  "${CMAKE_CURRENT_SOURCE_DIR}/../src/bb_lvgl_display.c"
  "${CMAKE_CURRENT_SOURCE_DIR}/../generated/bb_lvgl_element_assets.c"
)

if(EXISTS "${CMAKE_CURRENT_SOURCE_DIR}/../generated/lv_font_bbclaw_cjk.c")
  list(APPEND _sim_srcs "${CMAKE_CURRENT_SOURCE_DIR}/../generated/lv_font_bbclaw_cjk.c")
endif()

add_executable(bbclaw_lvgl_sim ${_sim_srcs})

target_include_directories(bbclaw_lvgl_sim PRIVATE
  "${CMAKE_CURRENT_SOURCE_DIR}"
  "${CMAKE_CURRENT_SOURCE_DIR}/../include"
  "${CMAKE_CURRENT_SOURCE_DIR}/../generated"
  "${CMAKE_CURRENT_SOURCE_DIR}/../build/config"
  ${SDL2_INCLUDE_DIRS}
)

target_compile_definitions(bbclaw_lvgl_sim PRIVATE LV_LVGL_H_INCLUDE_SIMPLE=1 BBCLAW_SIMULATOR=1)

if(EXISTS "${CMAKE_CURRENT_SOURCE_DIR}/../generated/lv_font_bbclaw_cjk.c")
  target_compile_definitions(bbclaw_lvgl_sim PRIVATE BBCLAW_HAVE_CJK_FONT=1)
endif()

target_link_libraries(bbclaw_lvgl_sim PRIVATE lvgl PkgConfig::SDL2)
