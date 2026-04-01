# BBClaw UI SVG Sources

该目录存放 BBClaw 固件公共 UI 元素的矢量源文件（source-of-truth）：

- `logo_claw.svg`
- `icon_ready.svg`
- `icon_tx.svg`
- `icon_rx.svg`
- `icon_err.svg`
- `icon_task.svg`
- `icon_speak.svg`
- `icon_rec_1.svg`
- `icon_rec_2.svg`
- `icon_rec_3.svg`

说明：

- **LVGL 屏**：`make gen-lvgl-assets`（`scripts/gen_lvgl_assets_from_svg.py`）用 `npx sharp-cli` 将 SVG 光栅化后转 **RGB565** `lv_image_dsc_t`，生成 `src/bb_lvgl_assets.c` / `include/bb_lvgl_assets.h`，由 `bb_lvgl_display.c` 在顶栏显示状态图标、底栏左侧显示 `logo_claw`。
- **旧自绘 / 位图常量**：`src/bb_ui_assets.c` 仍为 1-bit 参考，可与 SVG 视觉对齐。
- 修改图形时**优先改 SVG**，再运行 `make gen-lvgl-assets`（及按需更新 `bb_ui_assets`）。
- 风格约束参考 Flipper Zero：高对比、像素化、可在小尺寸快速识别。
