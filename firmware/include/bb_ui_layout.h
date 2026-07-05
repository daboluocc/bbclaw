/**
 * bb_ui_layout.h — 布局唯一真相源（UI_DESIGN_LANGUAGE.md §2.1 / ADR-040 §UI）。
 *
 * 圆角屏（手表 CO5300）四角被玻璃物理遮挡：板级给出 BBCLAW_DISPLAY_CORNER_RADIUS，
 * 此处推导两档安全内缩。方屏板 radius=0 时全部退化为原有小内缩，行为不变。
 */
#pragma once

#include "bb_config.h"

/* 面板中立别名（新代码用这组；ST7789 裸引用逐步替换） */
#define BB_DISP_W BBCLAW_ST7789_WIDTH
#define BB_DISP_H BBCLAW_ST7789_HEIGHT

/* 板级物理圆角半径（px）；未定义 = 方屏 */
#ifndef BBCLAW_DISPLAY_CORNER_RADIUS
#define BBCLAW_DISPLAY_CORNER_RADIUS 0
#endif

/* 角内切内缩：r×(1−1/√2) ≈ 0.293r（四舍五入）。
 * 语义：内容盒同时从两条邻边各缩进该值时，恰好避开半径 r 的圆角。 */
#define BB_UI_CORNER_INSET ((BBCLAW_DISPLAY_CORNER_RADIUS * 30 + 50) / 100)

#define BB_UI_MAX(a, b) ((a) > (b) ? (a) : (b))

/* 两档安全区：
 *  - SAFE_TOP/BOTTOM：贴顶/贴底内容（全宽条、页脚）的垂直内缩
 *  - SAFE_LR：贴左右内容的水平内缩（边中部内容可适当放宽，贴角必须用 CORNER_INSET）
 * 方屏板退化为 12/12/10（≈原 UI_SAFE_* 量级）。 */
#define BB_UI_SAFE_TOP    BB_UI_MAX(8, BB_UI_CORNER_INSET)
#define BB_UI_SAFE_BOTTOM BB_UI_MAX(10, BB_UI_CORNER_INSET)
#define BB_UI_SAFE_LR     BB_UI_MAX(10, (BB_UI_CORNER_INSET * 2) / 3)

/* 悬浮 PTT 钮区高度（滚动容器 pad_bottom 与钮定位共用） */
#define BB_UI_PTT_ZONE_H 96

/* 竖屏判定（手表 502>410；旧屏 172<320）——页面可据此选择构图分支 */
#define BB_UI_PORTRAIT (BB_DISP_H > BB_DISP_W)
