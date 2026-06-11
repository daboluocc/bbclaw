cmake_minimum_required(VERSION 3.16)

set(EXTRA_COMPONENT_DIRS "${CMAKE_CURRENT_LIST_DIR}/src" "${CMAKE_CURRENT_LIST_DIR}/components")

include($ENV{IDF_PATH}/tools/cmake/project.cmake)
# Firmware version (esp_app_desc.version) comes from version.txt when present
# (CI writes the release tag there), else `git describe`. Do NOT hardcode a
# VERSION here — a fixed version makes every OTA'd device report the same value
# and re-download the "newer" release forever (OTA loop). See release.yml.
project(bbclaw_firmware)

# -O2 下 GCC 14 在该第三方组件的 std::vector::insert 内联展开里误报
# stringop-overflow（-Og 不触发），-Werror 会把它升级为编译失败。
# 组件级降回告警，不改 managed_components 内容。见 issue #149 Round 1。
idf_component_get_property(opus_encoder_lib 78__esp-opus-encoder COMPONENT_LIB)
target_compile_options(${opus_encoder_lib} PRIVATE -Wno-error=stringop-overflow)
